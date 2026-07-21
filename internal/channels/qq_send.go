package channels

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
)

// QQ outbound send pipeline (contract §3.5 + §4.2 + §6.9 + §2.3).
//
// This file owns everything that pushes bytes toward QQ's REST API:
//
//   - SendMessage dispatch: text (msg_type 0 / 2) vs. media (msg_type 7).
//   - base64 direct media upload (contract §4.2): small files POSTed to
//     /v2/{groups|users}/{openid}/files with file_data; the resulting
//     file_info is cached per (content hash, scope, openid, file_type)
//     and replayed while the TTL window is open.
//   - SendTyping (msg_type 6, input_notify).
//   - HTML error-page detection (contract §6.9) so a 502/503/429 from
//     the QQ gateway doesn't trip a JSON parse error.
//   - SSRF-guarded image download for inbound attachments (contract §2.3).
//   - msg_seq generator (contract §3.5: (ts_low32 ^ rand16) % 65536).
//   - markdown downgrade for non-markdown accounts: FlattenMarkdownTables
//     + `**bold**` → `*bold*` (QQ plain-text mode uses single-asterisk
//     for bold; double-asterisk renders as literal punctuation).
//
// All REST calls go through qqSendDo (package-level swappable executor)
// which wraps qqWithTokenRetry so 401 / token-error responses replay
// once with a fresh token. Tests swap qqSendDo to capture the URL +
// body without touching the network.

// ---------------------------------------------------------------------------
// Protocol constants (contract §3.5 + §4)
// ---------------------------------------------------------------------------

// msg_type codes (contract §3.5).
const (
	qqMsgTypeText     = 0 // plain text (downgraded markdown)
	qqMsgTypeMarkdown = 2 // raw markdown content
	qqMsgTypeTyping   = 6 // input_notify indicator
	qqMsgTypeMedia    = 7 // rich media (uses media.file_info)
)

// file_type codes for /files upload (contract §4.1/§4.2).
const (
	qqFileTypeImage = 1
	qqFileTypeVideo = 2
	qqFileTypeVoice = 3
	qqFileTypeFile  = 4
)

// qqMsgSeqMod is the msg_seq modulus per contract §3.5 (range 0..65535).
const qqMsgSeqMod = 65536

// qqFileInfoTTL bounds how long a cached file_info is reused. Contract
// §4.3 default is 3600s; we shave a minute to avoid the boundary race
// where QQ expires the entry between our cache check and the send.
const qqFileInfoTTL = 55 * time.Minute

// qqMaxDownloadBytes caps inbound attachment size (10MB). QQ multimedia
// envelopes are typically well under 5MB; 10MB filters absurd outliers
// without rejecting real photos.
const qqMaxDownloadBytes = 10 * 1024 * 1024

// qqMaxRespBody caps how much of a REST response we read. QQ's JSON
// replies are small; HTML error pages are also small. 1MiB is plenty.
const qqMaxRespBody = 1 << 20

// ---------------------------------------------------------------------------
// Swappable HTTP executor (tests swap without touching network)
// ---------------------------------------------------------------------------

// qqSendDoFunc executes one REST call. Production wraps qqWithTokenRetry;
// tests swap to a stub that records the request.
type qqSendDoFunc func(
	ctx context.Context,
	appID, secret string,
	httpClient *http.Client,
	method, url string,
	body []byte,
) (status int, respHeader http.Header, respBody []byte, err error)

var (
	qqSendDo   qqSendDoFunc = qqDefaultSendDo
	qqSendDoMu sync.Mutex
)

// qqDefaultSendDo is the production executor. Routes through
// qqWithTokenRetry so 401 / token-error responses clear the cache and
// replay once with a fresh token (contract §3.2).
func qqDefaultSendDo(
	ctx context.Context,
	appID, secret string,
	httpClient *http.Client,
	method, reqURL string,
	body []byte,
) (int, http.Header, []byte, error) {
	do := func(token string) (*http.Response, []byte, error) {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", qqAuthScheme+" "+token)
		req.Header.Set("User-Agent", qqUserAgent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, qqMaxRespBody))
		if readErr != nil {
			return resp, b, readErr
		}
		return resp, b, nil
	}

	resp, respBody, err := qqWithTokenRetry(ctx, appID, secret, do)
	if resp == nil {
		return 0, nil, respBody, err
	}
	return resp.StatusCode, resp.Header, respBody, err
}

// ---------------------------------------------------------------------------
// SendMessage / Send / SendTyping (replace Phase 1 stubs)
// ---------------------------------------------------------------------------

// SendMessage delivers an outbound message to QQ. Routes by media
// presence + useMarkdown:
//
//   - MediaItems present → upload each via base64 direct upload
//     (contract §4.2) and send as msg_type=7 with media.file_info.
//     Any text on the message is attached as `content` on the same
//     send (QQ allows text alongside media).
//   - useMarkdown=true → msg_type=2 with `markdown.content`.
//   - useMarkdown=false (default) → msg_type=0 with `content`. The
//     raw text is passed through FlattenMarkdownTables + **b**→*b*
//     so tables and bold survive the plain-text downgrade.
//
// Passive replies set msg_id from OutboundMessage.ReplyToMsgID
// (contract §3.5: 5min validity window — caller's responsibility).
//
// Group vs. C2C endpoint selection uses the peerKind recorded from the
// most recent inbound event for this ChatID (group_openid → groups,
// user_openid → users). ChatIDs we've never seen inbound from default
// to "users" — the agent rarely sends first, and "users" is the safer
// fallback (group sends with an unknown group_openid always fail).
func (q *QQChannel) SendMessage(msg bus.OutboundMessage) error {
	if msg.ChatID == "" {
		return errors.New("qq send: empty ChatID")
	}
	// Materialize MediaPaths (host file paths from MEDIA: protocol, e.g.
	// image_gen tool output) into MediaItems so the base64 upload path
	// handles them. QQ runs host-mounted so os.ReadFile works.
	for _, p := range msg.MediaPaths {
		b, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("qq media path read failed", "path", p, "error", err)
			continue
		}
		msg.MediaItems = append(msg.MediaItems, bus.MediaItem{
			Filename: filepath.Base(p),
			Bytes:    b,
		})
	}

	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scope := "users"
	if q.lookupPeerKind(msg.ChatID) == "group" {
		scope = "groups"
	}

	if len(msg.MediaItems) > 0 {
		return q.sendMedia(ctx, scope, msg.ChatID, msg)
	}
	return q.sendText(ctx, scope, msg.ChatID, msg)
}

// Send is the plain-text shortcut. It routes through SendMessage so the
// useMarkdown switch + msg_seq generation stay in one place.
func (q *QQChannel) Send(chatID, text string) error {
	return q.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendTyping fires an input_notify indicator (contract §3.5 msg_type=6).
// QQ only honours this in C2C ("users") scope; we still attempt for
// groups because the platform may widen support later, and a no-op
// response is harmless. Errors are logged at debug — typing indicators
// are best-effort and should never block a follow-up send.
func (q *QQChannel) SendTyping(chatID string) error {
	if chatID == "" {
		return errors.New("qq typing: empty ChatID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	scope := "users"
	if q.lookupPeerKind(chatID) == "group" {
		scope = "groups"
	}

	body := qqBuildTypingBody(qqNextMsgSeq())
	endpoint := fmt.Sprintf("%s/v2/%s/%s/messages", qqAPIBase, scope, chatID)
	status, _, respBody, err := q.doREST(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		slog.Debug("qq typing failed", "account", q.accountID, "chat", chatID, "error", err)
		return err
	}
	if status >= 400 {
		slog.Debug("qq typing non-2xx",
			"account", q.accountID, "chat", chatID, "status", status,
			"body", qqSnippet(respBody))
	}
	return nil
}

// SetUseMarkdown configures whether SendMessage uses msg_type=2 (raw
// markdown) or msg_type=0 (plain-text downgrade). Default false per
// contract §6.3 — QQ markdown templates require separate approval and
// are usually not configured. Phase 4 wires this from AccountConfig.
func (q *QQChannel) SetUseMarkdown(v bool) { q.useMarkdown = v }

// UseMarkdown exposes the current setting (tests only).
func (q *QQChannel) UseMarkdown() bool { return q.useMarkdown }

// ---------------------------------------------------------------------------
// Text path
// ---------------------------------------------------------------------------

// sendText builds the JSON body per useMarkdown and POSTs it.
func (q *QQChannel) sendText(ctx context.Context, scope, openid string, msg bus.OutboundMessage) error {
	body, err := q.buildTextBody(msg)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v2/%s/%s/messages", qqAPIBase, scope, openid)
	status, _, respBody, err := q.doREST(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("qq send text: %w", err)
	}
	if status >= 400 {
		return qqBuildSendError(status, respBody)
	}
	return nil
}

// buildTextBody constructs the request JSON for the text path. Returns
// the marshalled body, choosing msg_type=2 when useMarkdown=true,
// otherwise msg_type=0 with downgraded markdown.
func (q *QQChannel) buildTextBody(msg bus.OutboundMessage) ([]byte, error) {
	seq := qqNextMsgSeq()
	if q.useMarkdown {
		return json.Marshal(qqTextRequestBody{
			MsgType: qqMsgTypeMarkdown,
			MsgSeq:  seq,
			MsgID:   msg.ReplyToMsgID,
			Markdown: &qqMarkdownField{
				Content: msg.Text,
			},
		})
	}
	return json.Marshal(qqTextRequestBody{
		MsgType: qqMsgTypeText,
		MsgSeq:  seq,
		MsgID:   msg.ReplyToMsgID,
		Content: qqDowngradeMarkdown(msg.Text),
	})
}

// ---------------------------------------------------------------------------
// Media path (contract §4.2: base64 direct upload)
// ---------------------------------------------------------------------------

// sendMedia uploads each MediaItem via base64 direct upload, then sends
// one msg_type=7 message per item. The text payload (if any) is attached
// as `content` to the FIRST media send so the chatter sees the caption
// alongside the image rather than as a separate bubble.
//
// Failures on individual items are logged but don't abort the chain —
// partial delivery beats dropping a whole turn for one bad upload
// (mirrors WeChat/Discord semantics).
func (q *QQChannel) sendMedia(ctx context.Context, scope, openid string, msg bus.OutboundMessage) error {
	textAttached := false
	for _, item := range msg.MediaItems {
		fileInfo, err := q.uploadMediaBase64(ctx, scope, openid, item)
		if err != nil {
			slog.Warn("qq media upload failed — skipping item",
				"account", q.accountID,
				"chat", msg.ChatID,
				"filename", item.Filename,
				"error", err)
			continue
		}

		body := qqMediaRequestBody{
			MsgType: qqMsgTypeMedia,
			MsgSeq:  qqNextMsgSeq(),
			MsgID:   msg.ReplyToMsgID,
			Media: &qqMediaField{
				FileInfo: fileInfo,
			},
		}
		// Attach caption to the first successful send.
		if !textAttached && msg.Text != "" {
			body.Content = msg.Text
			textAttached = true
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("qq media body: %w", err)
		}
		endpoint := fmt.Sprintf("%s/v2/%s/%s/messages", qqAPIBase, scope, openid)
		status, _, respBody, err := q.doREST(ctx, http.MethodPost, endpoint, raw)
		if err != nil {
			slog.Warn("qq media send http failed",
				"account", q.accountID, "chat", msg.ChatID, "error", err)
			continue
		}
		if status >= 400 {
			slog.Warn("qq media send non-2xx",
				"account", q.accountID,
				"chat", msg.ChatID,
				"status", status,
				"body", qqSnippet(respBody))
		}
	}

	// If we had text but every media upload failed, at least deliver the
	// text so the chatter isn't left hanging.
	if !textAttached && msg.Text != "" {
		slog.Warn("qq media all-failed — falling back to text",
			"account", q.accountID, "chat", msg.ChatID)
		return q.sendText(ctx, scope, openid, msg)
	}
	return nil
}

// uploadMediaBase64 POSTs one MediaItem to /v2/{scope}/{openid}/files
// (contract §4.2). Uses the file_info cache when the content hash +
// scope + openid + file_type have been uploaded within TTL.
func (q *QQChannel) uploadMediaBase64(ctx context.Context, scope, openid string, item bus.MediaItem) (string, error) {
	fileType := qqFileTypeFromContentType(item.ContentType, item.Filename)

	hash := qqMediaHash(openid, fileType, item.Bytes)
	if cached, ok := qqLookupFileInfo(hash); ok {
		slog.Debug("qq media cache hit",
			"account", q.accountID, "hash", hash, "fileType", fileType)
		return cached, nil
	}

	bodyReq := qqFileUploadRequest{
		FileType:     fileType,
		SrvSendMsg:   false,
		FileData:     base64.StdEncoding.EncodeToString(item.Bytes),
	}
	if fileType == qqFileTypeFile && item.Filename != "" {
		bodyReq.FileName = item.Filename
	}
	body, err := json.Marshal(bodyReq)
	if err != nil {
		return "", fmt.Errorf("qq upload marshal: %w", err)
	}

	endpoint := fmt.Sprintf("%s/v2/%s/%s/files", qqAPIBase, scope, openid)
	status, _, respBody, err := q.doREST(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", fmt.Errorf("qq upload: %w", err)
	}
	if status >= 400 {
		return "", qqBuildSendError(status, respBody)
	}

	// HTML guard before JSON parse (contract §6.9).
	if qqIsHTMLResponse(nil, respBody) {
		return "", fmt.Errorf("qq upload: gateway returned HTML (status %d): %s",
			status, qqSnippet(respBody))
	}

	var parsed qqFileUploadResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("qq upload parse: %w (body=%s)", err, qqSnippet(respBody))
	}
	if parsed.FileInfo == "" {
		return "", fmt.Errorf("qq upload: empty file_info in response (body=%s)",
			qqSnippet(respBody))
	}

	qqCacheFileInfo(hash, parsed.FileInfo, time.Now().Add(qqFileInfoTTL))
	slog.Debug("qq media uploaded + cached",
		"account", q.accountID, "hash", hash,
		"fileType", fileType, "ttl", qqFileInfoTTL)
	return parsed.FileInfo, nil
}

// ---------------------------------------------------------------------------
// file_info cache (content-hash scoped per (openid, fileType))
// ---------------------------------------------------------------------------

type qqFileInfoEntry struct {
	fileInfo  string
	expiresAt time.Time
}

var qqFileInfoCache sync.Map

// qqMediaHash returns a stable SHA-256 digest of the
// (openid, fileType, bytes) tuple. Used as the cache key so the same
// image sent to two different groups is uploaded once per group, but
// a re-send to the same group hits the cache.
func qqMediaHash(openid string, fileType int, b []byte) string {
	h := sha256.New()
	h.Write([]byte(openid))
	h.Write([]byte{byte(fileType)})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func qqLookupFileInfo(hash string) (string, bool) {
	v, ok := qqFileInfoCache.Load(hash)
	if !ok {
		return "", false
	}
	e, ok := v.(qqFileInfoEntry)
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		qqFileInfoCache.Delete(hash)
		return "", false
	}
	return e.fileInfo, true
}

func qqCacheFileInfo(hash, fileInfo string, expiresAt time.Time) {
	qqFileInfoCache.Store(hash, qqFileInfoEntry{
		fileInfo:  fileInfo,
		expiresAt: expiresAt,
	})
}

// qqResetFileInfoCacheForTest wipes the cache. Test-only.
func qqResetFileInfoCacheForTest() {
	qqFileInfoCache.Range(func(k, _ any) bool {
		qqFileInfoCache.Delete(k)
		return true
	})
}

// ---------------------------------------------------------------------------
// msg_seq (contract §3.5: (timestamp_low32 ^ random16) % 65536)
// ---------------------------------------------------------------------------

// qqNextMsgSeq generates a per-call value in [0, 65535]. Mixed with the
// timestamp so that rapid successive calls in the same msg_id window
// (multi-bubble replies, tool-call cascades) don't collide. Math/rand
// is auto-seeded in Go 1.20+; no explicit seed needed.
func qqNextMsgSeq() int {
	tsLow := int(time.Now().UnixMilli() % 1_0000_0000) // lower 32 bits of ms
	rnd := rand.Intn(qqMsgSeqMod)
	return (tsLow ^ rnd) % qqMsgSeqMod
}

// ---------------------------------------------------------------------------
// Markdown downgrade (msg_type=0 path)
// ---------------------------------------------------------------------------

// qqBoldPattern matches GFM `**bold**`. Non-greedy + at least one char
// inside + no nested asterisks. Doesn't touch code blocks — LLMs rarely
// emit literal `**` inside fenced code, and the downgrade path is a
// best-effort readability aid, not a full markdown parser.
var qqBoldPattern = regexp.MustCompile(`\*\*([^*]+?)\*\*`)

// qqDowngradeMarkdown converts raw markdown to a form that renders
// readably when QQ sends it as msg_type=0 (plain text). Steps:
//  1. FlattenMarkdownTables — most IM clients render `|cell|cell|` as
//     literal pipes; this turns tables into "label: value" / middle-dot
//     rows that scan cleanly as plain text.
//  2. `**bold**` → `*bold*` — QQ plain-text mode treats single-asterisk
//     as the bold marker; double-asterisk shows as literal punctuation.
func qqDowngradeMarkdown(text string) string {
	text = FlattenMarkdownTables(text)
	return qqBoldPattern.ReplaceAllString(text, "*$1*")
}

// ---------------------------------------------------------------------------
// file_type inference (contract §4.1 file_type enum)
// ---------------------------------------------------------------------------

// qqFileTypeFromContentType maps a MediaItem to QQ's file_type enum.
// Order: explicit content-type → filename extension sniffing → default
// to 4 (generic file).
func qqFileTypeFromContentType(contentType, filename string) int {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct == "" && filename != "" {
		ct = mime.TypeByExtension(qqEnsureDotExt(filename))
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		return qqFileTypeImage
	case strings.HasPrefix(ct, "video/"):
		return qqFileTypeVideo
	case strings.HasPrefix(ct, "audio/"):
		return qqFileTypeVoice
	}
	return qqFileTypeFile
}

// qqEnsureDotExt returns the extension of filename (including the dot),
// or empty if none. Used to drive mime.TypeByExtension.
func qqEnsureDotExt(filename string) string {
	idx := strings.LastIndexByte(filename, '.')
	if idx < 0 {
		return ""
	}
	return filename[idx:]
}

// ---------------------------------------------------------------------------
// HTML error-page detection (contract §6.9)
// ---------------------------------------------------------------------------

// qqIsHTMLResponse reports whether the REST response is an HTML error
// page rather than the expected JSON. QQ's gateway occasionally returns
// HTML on 502/503/504/429 — calling JSON.parse on those would throw.
//
// Signals:
//   - explicit Content-Type: text/html
//   - body begins with `<` after trimming whitespace (defensive — covers
//     misconfigured gateways that omit Content-Type)
func qqIsHTMLResponse(resp *http.Response, body []byte) bool {
	if resp != nil {
		ct := resp.Header.Get("Content-Type")
		if strings.HasPrefix(strings.ToLower(ct), "text/html") {
			return true
		}
	}
	if len(body) == 0 {
		return false
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '<'
}

// ---------------------------------------------------------------------------
// SSRF-guarded download for inbound attachments (contract §2.3)
// ---------------------------------------------------------------------------

// qqSafeDownload fetches an attachment URL with SSRF protection.
// Rejects non-http(s) schemes, hosts that are obvious loopback/private
// IPs, and responses exceeding qqMaxDownloadBytes. Used by the inbound
// handlers to materialize image attachments into MediaItems.
func qqSafeDownload(ctx context.Context, client *http.Client, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, "", fmt.Errorf("qq download: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("qq download: scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, "", errors.New("qq download: empty host")
	}
	if qqIsDisallowedHost(host) {
		return nil, "", fmt.Errorf("qq download: host %q blocked by SSRF guard", host)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", qqUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("qq download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("qq download HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, qqMaxDownloadBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("qq download read: %w", err)
	}
	if len(body) > qqMaxDownloadBytes {
		return nil, "", fmt.Errorf("qq download: body exceeds %d bytes", qqMaxDownloadBytes)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// qqIsDisallowedHost rejects localhost and private/loopback IPs by
// textual parse. Hostnames (non-IP) are allowed — a domain that later
// resolves to a private IP would slip past; for QQ's CDN URLs (which
// are public MultiValueDomain addresses) this is acceptable. A full
// DNS-resolution check is overkill for the QQ attachment path.
func qqIsDisallowedHost(host string) bool {
	lower := strings.ToLower(host)
	if lower == "localhost" || lower == "" {
		return true
	}
	// Strip zone ID from IPv6 (e.g. "fe80::1%eth0").
	if idx := strings.IndexByte(lower, '%'); idx > 0 {
		lower = lower[:idx]
	}
	ip := net.ParseIP(lower)
	if ip == nil {
		return false // hostname — allow
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// ---------------------------------------------------------------------------
// PeerKind tracking (group vs. dm per ChatID)
// ---------------------------------------------------------------------------

// qqAttachmentFetcher is the swappable attachment-download entry point.
// Production uses qqSafeDownload (SSRF guard + size cap); tests swap
// it to bypass the SSRF guard so httptest.Server (which binds
// 127.0.0.1) can drive the inbound path end-to-end.
var qqAttachmentFetcher = qqSafeDownload

// recordPeerKind stores the PeerKind observed on an inbound event so
// outbound SendMessage can pick the right REST endpoint. Populated by
// handleGroupAtMessage / handleC2CMessage.
func (q *QQChannel) recordPeerKind(chatID, peerKind string) {
	if chatID == "" {
		return
	}
	q.peerKinds.Store(chatID, peerKind)
}

// lookupPeerKind returns the last observed PeerKind for chatID, or
// empty string if unknown. Callers fall back to "users" (C2C) so an
// unsolicited proactive send doesn't accidentally broadcast to a group.
func (q *QQChannel) lookupPeerKind(chatID string) string {
	v, ok := q.peerKinds.Load(chatID)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// REST plumbing
// ---------------------------------------------------------------------------

// doREST invokes the swappable executor with this channel's credentials.
func (q *QQChannel) doREST(ctx context.Context, method, endpoint string, body []byte) (int, http.Header, []byte, error) {
	qqSendDoMu.Lock()
	do := qqSendDo
	qqSendDoMu.Unlock()
	return do(ctx, q.appID, q.appSecret, q.httpClient, method, endpoint, body)
}

// qqBuildSendError turns a non-2xx response into a descriptive error.
// Contract §6.9: HTML bodies must surface a friendly message rather
// than a raw HTML dump.
func qqBuildSendError(status int, body []byte) error {
	if qqIsHTMLResponse(nil, body) {
		return fmt.Errorf("qq send: gateway returned HTML (status %d) — likely 502/503/504/429", status)
	}
	return fmt.Errorf("qq send: HTTP %d: %s", status, qqSnippet(body))
}

// qqSnippet trims a response body for error/log messages. Caps at 256
// chars so a multi-KB HTML error page doesn't spam the log.
func qqSnippet(b []byte) string {
	const max = 256
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// ---------------------------------------------------------------------------
// Request / response wire types
// ---------------------------------------------------------------------------

// qqTextRequestBody is the JSON sent to /messages for the text path
// (contract §3.5). Only one of Markdown/Content is populated; msg_type
// signals which.
type qqTextRequestBody struct {
	MsgType  int              `json:"msg_type"`
	MsgSeq   int              `json:"msg_seq"`
	MsgID    string           `json:"msg_id,omitempty"`
	Content  string           `json:"content,omitempty"`
	Markdown *qqMarkdownField `json:"markdown,omitempty"`
}

type qqMarkdownField struct {
	Content string `json:"content"`
}

// qqMediaRequestBody is the JSON sent for msg_type=7 (rich media).
// FileInfo comes from a prior /files upload (§4.2). Content is the
// optional caption — QQ allows text alongside a media message.
type qqMediaRequestBody struct {
	MsgType int           `json:"msg_type"`
	MsgSeq  int           `json:"msg_seq"`
	MsgID   string        `json:"msg_id,omitempty"`
	Content string        `json:"content,omitempty"`
	Media   *qqMediaField `json:"media,omitempty"`
}

type qqMediaField struct {
	FileInfo string `json:"file_info"`
}

// qqTypingRequestBody is msg_type=6 input_notify (contract §3.5).
// msg_seq is required but its value is irrelevant — QQ uses it only
// for idempotency on actual sends.
type qqTypingRequestBody struct {
	MsgType     int               `json:"msg_type"`
	MsgSeq      int               `json:"msg_seq"`
	MsgID       string            `json:"msg_id,omitempty"`
	InputNotify *qqInputNotifyField `json:"input_notify,omitempty"`
}

type qqInputNotifyField struct {
	InputType  int `json:"input_type"`
	InputSecond int `json:"input_second"`
}

// qqBuildTypingBody constructs the msg_type=6 JSON.
func qqBuildTypingBody(seq int) []byte {
	b, _ := json.Marshal(qqTypingRequestBody{
		MsgType: qqMsgTypeTyping,
		MsgSeq:  seq,
		InputNotify: &qqInputNotifyField{
			InputType:   1,
			InputSecond: 60,
		},
	})
	return b
}

// qqFileUploadRequest is the /files body for base64 direct upload
// (contract §4.2). FileName is only required for file_type=4.
type qqFileUploadRequest struct {
	FileType   int    `json:"file_type"`
	SrvSendMsg bool   `json:"srv_send_msg"`
	FileData   string `json:"file_data"`
	FileName   string `json:"file_name,omitempty"`
}

// qqFileUploadResponse is the /files reply. FileInfo is the opaque
// string we pass back into qqMediaRequestBody.Media.FileInfo. TTL is
// seconds until platform-side expiry.
type qqFileUploadResponse struct {
	FileUUID string `json:"file_uuid,omitempty"`
	FileInfo string `json:"file_info"`
	TTL      int    `json:"ttl,omitempty"`
}
