package channels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/config"
)

// WeChat implements the Channel interface for the iLink (微信) bot
// platform. Pattern mirrors telegram.go: a single file owning the
// HTTP client, long-poll loop, and outbound send. We deliberately
// don't import a higher-level library — keeping the protocol surface
// in-tree makes it easy to evolve alongside fluctio's own message
// types and avoids a Go-module dependency on an out-of-tree project.

// iLink protocol constants. Matches the upstream WeChat bot API.
const (
	wechatDefaultBaseURL    = "https://ilinkai.weixin.qq.com"
	wechatLongPollTimeout   = 35 * time.Second
	wechatSendTimeout       = 15 * time.Second
	wechatErrSessionExpired = -14 // server-side sync token rotated; reset and re-poll

	wechatMsgTypeUser = 1
	wechatMsgTypeBot  = 2

	wechatMsgStateFinish = 2

	wechatItemTypeText  = 1
	wechatItemTypeImage = 2
	wechatItemTypeVoice = 3
	wechatItemTypeFile  = 4
	wechatItemTypeVideo = 5

	wechatBackoffInitial = 3 * time.Second
	wechatBackoffMax     = 60 * time.Second

	// /ilink/bot/sendtyping status values.
	wechatTypingStatusTyping = 1
	wechatTypingStatusCancel = 2

	wechatTypingTimeout = 8 * time.Second

	// CDN constants for image/file upload. Mirrors the upstream weclaw
	// daemon: iLink mints a per-upload URL via /ilink/bot/getuploadurl,
	// the bot AES-128-ECB-encrypts the bytes, POSTs ciphertext to the
	// CDN, and gets back an X-Encrypted-Param header that gets fed into
	// the ImageItem.media.encrypt_query_param.
	wechatCDNBaseURL        = "https://novac2c.cdn.weixin.qq.com/c2c"
	wechatCDNMediaTypeImage = 1
	wechatCDNMediaTypeVideo = 2
	wechatCDNMediaTypeFile  = 3
	wechatCDNEncryptType    = 1 // AES-128-ECB

	// Media-send timeout. Covers the getuploadurl round-trips + the
	// retried CDN POST + the second sendmessage. 120s so a full
	// uploadToCDN retry sequence (up to 3 × (getuploadurl + 30s POST))
	// fits even when the CDN is flaky.
	wechatMediaSendTimeout = 300 * time.Second

	// wechatCDNUploadRetries caps per-upload retries on 5xx CDN responses.
	// Mirrors openclaw-weixin's UPLOAD_MAX_RETRIES: the iLink CDN
	// intermittently 500s (notably a freshly-rescanned account's first
	// upload), and a couple of retries clears it. 4xx is not retried — a
	// client error won't be fixed by resending the same bytes.
	wechatCDNUploadRetries = 5

	// wechatCDNUploadAttemptTimeout caps a single CDN POST. The endpoint
	// occasionally goes silent mid-upload (no RST, just stops responding)
	// and one hung request would otherwise eat the whole
	// wechatMediaSendTimeout budget, leaving no room for the retry loop.
	// 60s is generous for a multi-MB upload over a slow (IPv6) uplink; on expiry we retry.
	wechatCDNUploadAttemptTimeout = 60 * time.Second

	// Threshold of consecutive empty-buf SessionExpired responses before
	// we declare the bot token dead and fire onExpired. iLink returns
	// SessionExpired when the supplied get_updates_buf is missing or
	// stale — including the legitimate "freshly rescanned account that
	// hasn't received its first message yet" case. Treating the first
	// occurrence as terminal would purge healthy accounts on every
	// restart (we used to do this). Combined with calcBackoff capping at
	// wechatBackoffMax (typically 60s), 20 consecutive failures gives
	// roughly 15–20 minutes of retries before we give up — long enough
	// for a freshly-rescanned bot to receive its first message and
	// graduate to a real buf, short enough that a truly revoked token
	// doesn't loop forever.
	wechatEmptyBufExpiredThreshold = 20

	// wechatPollingErrorThreshold is the consecutive-failure count for
	// GetUpdates errors (path ①) and generic server errors (path ③)
	// after which we mark the account failed and stop retrying. Same
	// order of magnitude as the empty-buf threshold so iLink gets ~15-
	// 20 min of grace before we surface a reconnect prompt.
	wechatPollingErrorThreshold = 20
)

// WeChat is the iLink long-poll adapter for one logged-in WeChat bot.
type WeChat struct {
	bus       *bus.MessageBus
	accountID string // ilink_bot_id, used by routing to look up the owner

	// HTTP credentials (one-time on QR confirm, persisted in configs):
	botToken    string
	baseURL     string
	ilinkUserID string

	httpClient *http.Client
	wechatUIN  string // randomized per process; iLink wants a stable-ish header

	// Long-poll cursor. iLink's `get_updates_buf` advances each turn
	// and is persisted to disk at `bufPath` so process restarts don't
	// poll with the empty buf (which iLink answers with SessionExpired,
	// which the old code misread as "bot token dead" — see
	// wechatEmptyBufExpiredThreshold for the matching softened heuristic).
	getUpdatesBuf string
	bufPath       string

	// Consecutive-failure tracking. pollFC covers GetUpdates errors
	// (path ①) and generic server errors (path ③); emptyBufFC covers
	// the empty-buf SessionExpired path (②) which has its own semantics
	// ("token may be dead, rescan needed"). Both trip onFailed at their
	// thresholds and exit Start.
	pollFC     *FailureCounter
	emptyBufFC *FailureCounter

	// Per-chat ContextToken cache. The /ilink/bot/getconfig call that
	// mints typing_ticket wants the latest context_token from the user's
	// inbound message; we don't get one in SendTyping(chatID) so we
	// remember the most recent token off each inbound and use it on the
	// way back. Empty string is allowed (getconfig has it as optional)
	// — the cache is best-effort, not a hard prerequisite.
	ctxTokensMu sync.Mutex
	ctxTokens   map[string]string

	// onFailed fires once when the adapter has decided the account is
	// unrecoverable without user action (path ①③ polling/server errors
	// past threshold, or ② empty-buf session expired past threshold).
	// Set by the gateway via the FailureReporter interface so it can
	// persist FailureType + unregister; without it the loop would log
	// the same warning forever.
	onFailed func(accountID, reason string)
}

// OnFailed registers the framework callback fired when the adapter has
// given up reconnecting. Implements FailureReporter. The callback runs
// once; Start exits afterwards.
func (w *WeChat) OnFailed(fn func(accountID, reason string)) {
	w.onFailed = fn
}

// NewWeChat creates a new WeChat channel adapter from a connected
// account's stored credentials.
func NewWeChat(botToken, baseURL, ilinkUserID, accountID string, mb *bus.MessageBus) (*WeChat, error) {
	if botToken == "" || accountID == "" {
		return nil, fmt.Errorf("wechat: botToken and accountID required")
	}
	if baseURL == "" {
		baseURL = wechatDefaultBaseURL
	}
	slog.Info("wechat bot authorized", "account", accountID)
	return &WeChat{
		bus:         mb,
		accountID:   accountID,
		botToken:    botToken,
		baseURL:     baseURL,
		ilinkUserID: ilinkUserID,
		httpClient:  &http.Client{},
		wechatUIN:   wechatGenerateUIN(),
		ctxTokens:   make(map[string]string),
		bufPath:     wechatBufPath(accountID),
		pollFC:      NewFailureCounter(wechatPollingErrorThreshold, wechatBackoffInitial, wechatBackoffMax),
		emptyBufFC:  NewFailureCounter(wechatEmptyBufExpiredThreshold, wechatBackoffInitial, wechatBackoffMax),
	}, nil
}

// wechatBufPath returns the on-disk location for this account's
// persisted get_updates_buf. AccountIDs contain `@` (e.g.
// `4090de018d12@im.bot`) which is filesystem-safe on every OS we ship
// to, but we replace path separators defensively in case iLink ever
// hands one back. Returns "" when HomeDir() fails — caller treats that
// as "persistence disabled, fall back to in-process state."
func wechatBufPath(accountID string) string {
	home, err := config.HomeDir()
	if err != nil || home == "" {
		return ""
	}
	safe := strings.ReplaceAll(accountID, "/", "_")
	safe = strings.ReplaceAll(safe, string(os.PathSeparator), "_")
	return filepath.Join(home, "state", "wechat", safe+".json")
}

// loadBuf populates getUpdatesBuf from disk. Missing file → no-op
// (first run for this account, or state dir was wiped). Corrupt file
// → log + ignore (we'll just sync from "" once, which is fine — the
// softened expiry threshold prevents a single empty-buf reply from
// purging the account).
func (w *WeChat) loadBuf() {
	if w.bufPath == "" {
		return
	}
	data, err := os.ReadFile(w.bufPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("wechat loadBuf failed",
				"account", w.accountID, "path", w.bufPath, "error", err)
		}
		return
	}
	var s struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		slog.Warn("wechat loadBuf parse failed — discarding",
			"account", w.accountID, "path", w.bufPath, "error", err)
		return
	}
	w.getUpdatesBuf = s.GetUpdatesBuf
	if s.GetUpdatesBuf != "" {
		slog.Info("wechat loaded persisted sync buf",
			"account", w.accountID, "path", w.bufPath)
	}
}

// saveBuf writes the current getUpdatesBuf to disk. Best-effort: errors
// are logged but don't abort the poll loop — losing the buf only costs
// us one fresh-sync round next start, and the softened expiry threshold
// keeps that from triggering a purge.
func (w *WeChat) saveBuf() {
	if w.bufPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(w.bufPath), 0o700); err != nil {
		slog.Warn("wechat saveBuf mkdir failed",
			"account", w.accountID, "path", w.bufPath, "error", err)
		return
	}
	data, _ := json.Marshal(struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}{GetUpdatesBuf: w.getUpdatesBuf})
	if err := os.WriteFile(w.bufPath, data, 0o600); err != nil {
		slog.Warn("wechat saveBuf write failed",
			"account", w.accountID, "path", w.bufPath, "error", err)
	}
}

// clearBuf removes the on-disk buf file. Called after we declare the
// token dead so a manually-rescanned-and-relabeled account doesn't
// inherit the dead session's stale cursor on the next process start.
func (w *WeChat) clearBuf() {
	if w.bufPath == "" {
		return
	}
	if err := os.Remove(w.bufPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("wechat clearBuf failed",
			"account", w.accountID, "path", w.bufPath, "error", err)
	}
}

func (w *WeChat) Name() string        { return "wechat" }
func (w *WeChat) AccountID() string   { return w.accountID }
func (w *WeChat) BotUsername() string { return w.accountID }

// Start runs the long-poll loop until ctx is cancelled. Mirrors the
// retry / session-recovery semantics of the upstream weclaw monitor:
//   - any GetUpdates error → exponential backoff up to 60s
//   - errcode -14 (session expired) → reset sync buf and retry; if
//     the sync buf was already empty the bot token itself is dead
//     (operator needs to re-scan).
func (w *WeChat) Start(ctx context.Context) error {
	w.loadBuf()
	slog.Info("wechat long-poll loop starting",
		"account", w.accountID, "buf_present", w.getUpdatesBuf != "")
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		resp, err := w.getUpdates(ctx, w.getUpdatesBuf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if w.pollFC.Hit() {
				slog.Warn("wechat polling failed — marking account failed",
					"account", w.accountID, "failures", w.pollFC.Count(), "error", err)
				w.clearBuf()
				if w.onFailed != nil {
					w.onFailed(w.accountID, "polling_failed")
				}
				return nil
			}
			backoff := w.pollFC.Backoff()
			slog.Warn("wechat getUpdates error",
				"account", w.accountID, "failures", w.pollFC.Count(), "backoff", backoff, "error", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		w.pollFC.Reset()

		if resp.ErrCode == wechatErrSessionExpired {
			if w.getUpdatesBuf != "" {
				slog.Info("wechat session expired, resetting sync buf", "account", w.accountID)
				w.getUpdatesBuf = ""
				w.saveBuf()
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
					return nil
				}
				continue
			}
			// Sync buf already empty. This is ambiguous — it could mean
			// "first poll after restart, server doesn't know us yet" or
			// "bot token has been revoked." Treating the first occurrence
			// as terminal (the old behavior) caused every restart of a
			// healthy account to purge itself before iLink had a chance
			// to mint a fresh buf. Mirror upstream weclaw: keep retrying
			// with exponential backoff, and only declare the token dead
			// after wechatEmptyBufExpiredThreshold consecutive failures.
			if w.emptyBufFC.Hit() {
				// Threshold tripped: token is dead for real. Wipe the
				// on-disk buf so a freshly-rescanned account doesn't
				// inherit the stale cursor on the next process start,
				// then fire onFailed (the gateway persists FailureType
				// + unregisters us) and exit.
				slog.Warn("wechat bot token expired — user must rescan QR",
					"account", w.accountID, "attempts", w.emptyBufFC.Count())
				w.clearBuf()
				if w.onFailed != nil {
					w.onFailed(w.accountID, "session_expired")
				}
				return nil
			}
			// Only the first attempt warns — subsequent retries log
			// at Debug so a slow-to-warm-up account doesn't fill the
			// log with N copies of the same message before either
			// recovering (counter resets, see below) or hitting
			// threshold (which logs its own terminal Warn).
			if w.emptyBufFC.Count() == 1 {
				slog.Warn("wechat session expired with empty buf — will retry up to threshold",
					"account", w.accountID,
					"threshold", wechatEmptyBufExpiredThreshold)
			} else {
				slog.Debug("wechat session expired with empty buf — retrying",
					"account", w.accountID,
					"attempt", w.emptyBufFC.Count(),
					"threshold", wechatEmptyBufExpiredThreshold)
			}
			backoff := w.emptyBufFC.Backoff()
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		if resp.Ret != 0 && resp.ErrCode != 0 {
			slog.Warn("wechat server error",
				"account", w.accountID, "ret", resp.Ret, "errcode", resp.ErrCode, "errmsg", resp.ErrMsg)
			if w.pollFC.Hit() {
				slog.Warn("wechat server errors persisted — marking account failed",
					"account", w.accountID, "failures", w.pollFC.Count())
				w.clearBuf()
				if w.onFailed != nil {
					w.onFailed(w.accountID, "server_error")
				}
				return nil
			}
			backoff := w.pollFC.Backoff()
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		// Any non-SessionExpired success resets the empty-buf counter —
		// we got a real response from the server, the token is alive.
		if w.emptyBufFC.Count() > 0 {
			slog.Info("wechat session recovered after empty-buf retries",
				"account", w.accountID, "attempts", w.emptyBufFC.Count())
			w.emptyBufFC.Reset()
		}
		if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != w.getUpdatesBuf {
			w.getUpdatesBuf = resp.GetUpdatesBuf
			w.saveBuf()
		}
		for _, m := range resp.Msgs {
			w.dispatchInbound(m)
		}
	}
}

// dispatchInbound flattens a iLink message into a bus.InboundMessage.
// Filter rules:
//   - drop messages from the bot itself (MessageType=2). They're echoes
//     of our own sends.
//   - drop in-progress streaming messages (MessageState != finish);
//     iLink sends partial deltas during voice transcription, we only
//     want the final.
//   - text + image are surfaced; voice is surfaced as the
//     speech-to-text transcription iLink already provides; files are
//     downloaded + decrypted from the CDN into MediaItems (video is
//     still dropped — transcription-less media we can't yet act on).
func (w *WeChat) dispatchInbound(m wechatMessage) {
	if m.MessageType != wechatMsgTypeUser {
		return
	}
	if m.MessageState != wechatMsgStateFinish {
		return
	}

	var text string
	var imageURLs []string
	var mediaItems []bus.MediaItem
	for _, item := range m.ItemList {
		switch item.Type {
		case wechatItemTypeText:
			if item.TextItem != nil && item.TextItem.Text != "" {
				text = item.TextItem.Text
			}
		case wechatItemTypeImage:
			if item.ImageItem == nil {
				continue
			}
			// Log the raw image item for debugging — need to understand
			// what iLink actually sends before deciding how to process.
			slog.Info("wechat inbound image item",
				"account", w.accountID, "from", m.FromUserID,
				"has_url", item.ImageItem.URL != "",
				"url_preview", truncate(item.ImageItem.URL, 80),
				"has_media", item.ImageItem.Media != nil,
				"encrypt_query_param", truncateMediaParam(item.ImageItem.Media),
				"mid_size", item.ImageItem.MidSize)
			// Download + decrypt from CDN, encode as data URL.
			// iLink CDN URLs are encrypted/auth-only — the model cannot
			// fetch them directly. Must download and decrypt first.
			if item.ImageItem.Media != nil && item.ImageItem.Media.EncryptQueryParam != "" {
				if dataURL, err := w.downloadAndDecryptImage(item.ImageItem); err != nil {
					slog.Warn("wechat image download failed",
						"account", w.accountID, "from", m.FromUserID, "error", err)
				} else {
					imageURLs = append(imageURLs, dataURL)
				}
			} else if item.ImageItem.URL != "" {
				// Fallback: try direct URL if no encrypted media present.
				imageURLs = append(imageURLs, item.ImageItem.URL)
			}
		case wechatItemTypeVoice:
			// iLink ships speech-to-text transcription alongside the
			// audio bytes — use it directly so the agent sees the
			// user's spoken request as text without us having to
			// download + transcribe ourselves.
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				text = item.VoiceItem.Text
			}
		case wechatItemTypeFile:
			// File attachments (zip, tar.gz, pdf, docs, …) ride the same
			// AES-128-ECB CDN envelope as images, so downloadAndDecryptBytes
			// handles them. Plaintext lands in MediaItems only (NOT
			// imageURLs — these aren't images, and persistInboundImages
			// would mis-handle a zip); the gateway then materializes it
			// into /workspace with a `[Attached: …]` breadcrumb.
			if item.FileItem == nil || item.FileItem.Media == nil {
				continue
			}
			plaintext, err := w.downloadAndDecryptBytes(item.FileItem.Media)
			if err != nil {
				slog.Warn("wechat file download failed",
					"account", w.accountID, "from", m.FromUserID,
					"filename", item.FileItem.FileName, "error", err)
				continue
			}
			mediaItems = append(mediaItems, bus.MediaItem{
				Filename:    item.FileItem.FileName,
				ContentType: http.DetectContentType(plaintext),
				Bytes:       plaintext,
			})
		}
	}
	if text == "" && len(imageURLs) == 0 && len(mediaItems) == 0 {
		slog.Debug("wechat skipping unsupported message",
			"account", w.accountID, "from", m.FromUserID, "items", len(m.ItemList))
		return
	}
	// Image-only messages: give the model a text cue so it knows to
	// describe or act on the image rather than seeing an empty turn.
	if text == "" && len(imageURLs) > 0 {
		text = "[image]"
	}

	// iLink doesn't distinguish DM vs group at the protocol level the
	// way Telegram does — every message has a from_user_id and a
	// to_user_id (the bot). Treat all as DM for now; group support
	// would require parsing room_id which the current iLink response
	// shape doesn't expose.
	slog.Info("wechat message received",
		"account", w.accountID, "from", m.FromUserID, "len", len(text))

	// Remember this user's most recent ContextToken so a subsequent
	// SendTyping(chatID) can mint a typing_ticket without round-trip-
	// owning the original message. Cache is per-chat; we just overwrite
	// — the freshest token is the most likely to validate.
	if m.FromUserID != "" {
		w.ctxTokensMu.Lock()
		w.ctxTokens[m.FromUserID] = m.ContextToken
		w.ctxTokensMu.Unlock()
	}

	w.bus.Inbound <- bus.InboundMessage{
		Channel:    "wechat",
		AccountID:  w.accountID,
		ChatID:     m.FromUserID, // 1:1 — sender is also the chat key
		UserID:     m.FromUserID,
		SenderName: m.FromUserID, // iLink carries no nickname; use the stable user id so chatters get a readable display name instead of a blank
		MessageID:  strconv.FormatInt(m.MessageID, 10),
		Text:       text,
		PhotoURLs:  imageURLs,
		MediaItems: mediaItems,
		PeerKind:   "dm",
	}
}

// Send sends a plain text message — the simple form. Used by tools
// that don't need rich formatting.
func (w *WeChat) Send(chatID, text string) error {
	return w.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage posts a reply to a iLink user. iLink doesn't have native
// markdown, inline keyboards, or message edits, so most of the
// OutboundMessage fields are intentionally ignored — we honor Text,
// ChatID, MediaItems, and the per-chat ContextToken cached from the
// last inbound (used both by SendTyping and by the image-send path).
//
// Text and images are sent as separate iLink messages: a text-only
// sendmessage first (if there's text), then one sendmessage per image
// after each has been uploaded to the iLink CDN. Failures on individual
// images are logged but don't abort the rest of the reply — partial
// delivery is better than dropping the whole turn for one bad upload.
//
// Multi-bubble replies: when the agent emits SplitMessageMarker, the
// text is split into N bubbles, each sent as its own sendmessage.
// Failure on one chunk stops the chain — partial delivery is preferable
// to silently dropping later bubbles, but if iLink itself errored we
// don't want to keep hammering the API.
func (w *WeChat) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	// iLink renders markdown natively in modern WeChat clients, so send
	// the agent's text as-is — headers, bold, lists, tables etc. display
	// formatted instead of being collapsed to plain text. Two light
	// passes first (mirroring hermes-agent's weixin adapter):
	// wechatNormalizeMarkdownBlocks collapses extra blank lines outside
	// code blocks so the bubble stays tight; wechatWrapCopyFriendlyLines
	// soft-wraps very long display lines so they stay easy to copy in
	// WeChat (which has no horizontal scroll).
	// Splitting on SplitMessageMarker happens at the dispatcher layer
	// (internal/channels/manager.go: routeOutbound) so all IM adapters
	// honor it uniformly — by the time SendMessage gets called here,
	// msg.Text is a single bubble's worth of content. The dispatcher
	// also collapses stray markers to newlines when AllowSplit is off,
	// so we never see them here.
	text := wechatWrapCopyFriendlyLines(wechatNormalizeMarkdownBlocks(msg.Text))
	// Skip the text leg when the body is empty/whitespace — e.g. a
	// multi-bubble split chunk whose visible content is all in its media.
	if strings.TrimSpace(text) != "" {
		if err := w.sendTextOnly(msg.ChatID, text); err != nil {
			return err
		}
	}
	for _, item := range msg.MediaItems {
		if len(item.Bytes) == 0 {
			continue
		}
		if err := w.sendMedia(msg.ChatID, item); err != nil {
			slog.Warn("wechat send media failed",
				"account", w.accountID, "chat", msg.ChatID,
				"filename", item.Filename, "error", err)
		}
	}
	return nil
}

// sendTextOnly is the simple text-message path used by SendMessage when
// there is any plain text to send. Kept distinct from sendImage so each
// path can carry its own timeout + payload shape.
func (w *WeChat) sendTextOnly(chatID, plain string) error {
	w.ctxTokensMu.Lock()
	contextToken := w.ctxTokens[chatID]
	w.ctxTokensMu.Unlock()

	body := wechatSendRequest{
		Msg: wechatSendMsg{
			FromUserID:   w.accountID,
			ToUserID:     chatID,
			ClientID:     uuid.NewString(),
			MessageType:  wechatMsgTypeBot,
			MessageState: wechatMsgStateFinish,
			ItemList: []wechatItem{
				{
					Type:     wechatItemTypeText,
					TextItem: &wechatTextItem{Text: plain},
				},
			},
			ContextToken: contextToken,
		},
		BaseInfo: wechatBaseInfo{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), wechatSendTimeout)
	defer cancel()
	var resp wechatSendResponse
	if err := w.doPost(ctx, "/ilink/bot/sendmessage", body, &resp); err != nil {
		return fmt.Errorf("wechat send: %w", err)
	}
	if resp.Ret != 0 {
		return fmt.Errorf("wechat send: ret=%d errmsg=%s", resp.Ret, resp.ErrMsg)
	}
	return nil
}

// SendTyping shows a "对方正在输入..." indicator on the user's WeChat
// while the agent is processing the turn. iLink wants two calls:
//
//  1. /ilink/bot/getconfig with the recipient's ilink_user_id (and
//     optionally their last context_token) to mint a typing_ticket;
//  2. /ilink/bot/sendtyping with that ticket and status=1.
//
// The gateway pings this every 5s for the duration of a turn — same
// cadence as Telegram's sendChatAction. Errors are logged at Debug and
// returned, but the gateway treats them as best-effort, so a hiccup
// doesn't fail the user-visible reply.
func (w *WeChat) SendTyping(chatID string) error {
	if chatID == "" {
		return nil
	}
	w.ctxTokensMu.Lock()
	contextToken := w.ctxTokens[chatID]
	w.ctxTokensMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), wechatTypingTimeout)
	defer cancel()

	cfgBody := wechatGetConfigRequest{
		ILinkUserID:  chatID,
		ContextToken: contextToken,
	}
	var cfgResp wechatGetConfigResponse
	if err := w.doPost(ctx, "/ilink/bot/getconfig", cfgBody, &cfgResp); err != nil {
		slog.Warn("wechat getconfig failed", "account", w.accountID, "chat", chatID, "error", err)
		return fmt.Errorf("wechat getconfig: %w", err)
	}
	if cfgResp.Ret != 0 {
		slog.Warn("wechat getconfig non-zero ret",
			"account", w.accountID, "chat", chatID, "ret", cfgResp.Ret, "errmsg", cfgResp.ErrMsg)
		return fmt.Errorf("wechat getconfig: ret=%d errmsg=%s", cfgResp.Ret, cfgResp.ErrMsg)
	}
	if cfgResp.TypingTicket == "" {
		slog.Info("wechat getconfig returned empty typing_ticket — typing disabled for this account",
			"account", w.accountID, "chat", chatID)
		return nil
	}

	typingBody := wechatSendTypingRequest{
		ILinkUserID:  chatID,
		TypingTicket: cfgResp.TypingTicket,
		Status:       wechatTypingStatusTyping,
	}
	var typingResp wechatSendTypingResponse
	if err := w.doPost(ctx, "/ilink/bot/sendtyping", typingBody, &typingResp); err != nil {
		slog.Warn("wechat sendtyping failed", "account", w.accountID, "chat", chatID, "error", err)
		return fmt.Errorf("wechat sendtyping: %w", err)
	}
	if typingResp.Ret != 0 {
		slog.Warn("wechat sendtyping non-zero ret",
			"account", w.accountID, "chat", chatID, "ret", typingResp.Ret, "errmsg", typingResp.ErrMsg)
		return fmt.Errorf("wechat sendtyping: ret=%d errmsg=%s", typingResp.Ret, typingResp.ErrMsg)
	}
	slog.Debug("wechat typing sent", "account", w.accountID, "chat", chatID)
	return nil
}

// --- HTTP plumbing ---

// getUpdates is the long-poll. Server holds the request open up to
// `longpolling_timeout_ms` (typically 30s) and returns either pending
// messages or an empty Msgs slice. We give the request 5s of slack
// over the server-side timeout so client-side cancellation is
// distinguishable from server-side empty-batch.
func (w *WeChat) getUpdates(ctx context.Context, buf string) (*wechatGetUpdatesResponse, error) {
	body := wechatGetUpdatesRequest{
		GetUpdatesBuf: buf,
		BaseInfo:      wechatBaseInfo{ChannelVersion: "1.0.0"},
	}
	ctx, cancel := context.WithTimeout(ctx, wechatLongPollTimeout+5*time.Second)
	defer cancel()
	var resp wechatGetUpdatesResponse
	if err := w.doPost(ctx, "/ilink/bot/getupdates", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (w *WeChat) doPost(ctx context.Context, path string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("Authorization", "Bearer "+w.botToken)
	req.Header.Set("X-WECHAT-UIN", w.wechatUIN)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return json.Unmarshal(respBody, result)
}

// wechatGenerateUIN produces the randomized base64 string iLink wants
// in the X-WECHAT-UIN header. The upstream protocol documents it as
// "anything stable-ish per process"; we generate once at adapter
// construction.
func wechatGenerateUIN() string {
	var n uint32
	_ = binary.Read(rand.Reader, binary.LittleEndian, &n)
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", n)))
}

// wechatCopyLineWidth is the soft wrap width for outbound WeChat lines.
// WeChat clients don't offer horizontal scroll, so paragraphs wider than
// this get copy-unfriendly; we word-wrap to stay within it. Lines inside
// code blocks and table rows are exempt.
const wechatCopyLineWidth = 120

// wechatTableRuleRe matches a GFM table separator row like |---|:--:|-|
// — such lines must be preserved verbatim (not wrapped) so the table
// still renders. Matched against the whitespace-trimmed line.
var wechatTableRuleRe = regexp.MustCompile(`^\|?(?:\s*:?-{3,}:?\s*\|)+\s*:?-{3,}:?\s*\|?$`)

// wechatIsFenceLine reports whether s (whitespace-trimmed) is a markdown
// fenced-code-block delimiter — ``` optionally followed by a language tag.
func wechatIsFenceLine(stripped string) bool {
	return strings.HasPrefix(stripped, "```")
}

// wechatNormalizeMarkdownBlocks collapses runs of consecutive blank lines
// outside fenced code blocks to a single blank line. LLM output frequently
// has 3-4 blank lines between sections; WeChat renders each as a gap, so
// compacting keeps the bubble tight. Code-block content is left untouched.
// Mirrors hermes-agent's _normalize_markdown_blocks.
func wechatNormalizeMarkdownBlocks(content string) string {
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))
	inCodeBlock := false
	blankRun := 0
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		stripped := strings.TrimSpace(line)
		if wechatIsFenceLine(stripped) {
			inCodeBlock = !inCodeBlock
			result = append(result, line)
			blankRun = 0
			continue
		}
		if inCodeBlock {
			result = append(result, line)
			continue
		}
		if stripped == "" {
			blankRun++
			if blankRun <= 1 {
				result = append(result, "")
			}
			continue
		}
		blankRun = 0
		result = append(result, line)
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

// wechatWrapCopyFriendlyLines soft-wraps display lines wider than
// wechatCopyLineWidth so long paragraphs stay easy to copy inside WeChat
// clients. Lines inside fenced code blocks, table rows and the GFM table
// separator are left intact. Mirrors hermes-agent's
// _wrap_copy_friendly_lines_for_weixin.
func wechatWrapCopyFriendlyLines(content string) string {
	if content == "" {
		return content
	}
	lines := strings.Split(content, "\n")
	wrapped := make([]string, 0, len(lines))
	inCodeBlock := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		stripped := strings.TrimSpace(line)
		if wechatIsFenceLine(stripped) {
			inCodeBlock = !inCodeBlock
			wrapped = append(wrapped, line)
			continue
		}
		if inCodeBlock ||
			utf8.RuneCountInString(line) <= wechatCopyLineWidth ||
			stripped == "" ||
			strings.HasPrefix(stripped, "|") ||
			wechatTableRuleRe.MatchString(stripped) {
			wrapped = append(wrapped, line)
			continue
		}
		wrapped = append(wrapped, wechatWordWrap(line, wechatCopyLineWidth)...)
	}
	return strings.TrimSpace(strings.Join(wrapped, "\n"))
}

// wechatWordWrap greedily packs space-separated words into segments no
// wider than width (counted in runes). A single token longer than width —
// a bare URL, a run of CJK with no spaces — is kept whole, mirroring
// Python textwrap with break_long_words=False.
func wechatWordWrap(line string, width int) []string {
	if utf8.RuneCountInString(line) <= width {
		return []string{line}
	}
	words := strings.Fields(line)
	if len(words) <= 1 {
		return []string{line}
	}
	var result []string
	cur := ""
	curLen := 0
	for _, w := range words {
		wl := utf8.RuneCountInString(w)
		if cur == "" {
			cur = w
			curLen = wl
		} else if curLen+1+wl <= width {
			cur += " " + w
			curLen += 1 + wl
		} else {
			result = append(result, cur)
			cur = w
			curLen = wl
		}
	}
	if cur != "" {
		result = append(result, cur)
	}
	return result
}

// --- Wire types (iLink protocol shape, kept private to this package) ---

type wechatBaseInfo struct {
	ChannelVersion string `json:"channel_version,omitempty"`
}

type wechatGetUpdatesRequest struct {
	GetUpdatesBuf string         `json:"get_updates_buf"`
	BaseInfo      wechatBaseInfo `json:"base_info"`
}

type wechatGetUpdatesResponse struct {
	Ret           int             `json:"ret"`
	ErrCode       int             `json:"errcode,omitempty"`
	ErrMsg        string          `json:"errmsg,omitempty"`
	Msgs          []wechatMessage `json:"msgs"`
	GetUpdatesBuf string          `json:"get_updates_buf"`
}

type wechatMessage struct {
	Seq          int          `json:"seq,omitempty"`
	MessageID    int64        `json:"message_id,omitempty"`
	FromUserID   string       `json:"from_user_id"`
	ToUserID     string       `json:"to_user_id"`
	MessageType  int          `json:"message_type"`
	MessageState int          `json:"message_state"`
	ItemList     []wechatItem `json:"item_list"`
	ContextToken string       `json:"context_token"`
}

type wechatItem struct {
	Type      int              `json:"type"`
	TextItem  *wechatTextItem  `json:"text_item,omitempty"`
	ImageItem *wechatImageItem `json:"image_item,omitempty"`
	VoiceItem *wechatVoiceItem `json:"voice_item,omitempty"`
	VideoItem *wechatVideoItem `json:"video_item,omitempty"`
	FileItem  *wechatFileItem  `json:"file_item,omitempty"`
}

type wechatVideoItem struct {
	Media     *wechatMediaInfo `json:"media,omitempty"`
	VideoSize int              `json:"video_size,omitempty"` // ciphertext size
}

type wechatFileItem struct {
	Media    *wechatMediaInfo `json:"media,omitempty"`
	FileName string           `json:"file_name,omitempty"`
	Len      string           `json:"len,omitempty"` // plaintext size, as a string (iLink quirk)
}

type wechatTextItem struct {
	Text string `json:"text"`
}

type wechatVoiceItem struct {
	Text     string `json:"text,omitempty"`     // STT transcription
	Playtime int    `json:"playtime,omitempty"` // ms
}

type wechatSendRequest struct {
	Msg      wechatSendMsg  `json:"msg"`
	BaseInfo wechatBaseInfo `json:"base_info"`
}

type wechatSendMsg struct {
	FromUserID   string       `json:"from_user_id"`
	ToUserID     string       `json:"to_user_id"`
	ClientID     string       `json:"client_id"`
	MessageType  int          `json:"message_type"`
	MessageState int          `json:"message_state"`
	ItemList     []wechatItem `json:"item_list"`
	ContextToken string       `json:"context_token,omitempty"`
}

type wechatSendResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg,omitempty"`
}

type wechatGetConfigRequest struct {
	ILinkUserID  string         `json:"ilink_user_id"`
	ContextToken string         `json:"context_token,omitempty"`
	BaseInfo     wechatBaseInfo `json:"base_info"`
}

type wechatGetConfigResponse struct {
	Ret          int    `json:"ret"`
	ErrMsg       string `json:"errmsg,omitempty"`
	TypingTicket string `json:"typing_ticket,omitempty"`
}

type wechatSendTypingRequest struct {
	ILinkUserID  string         `json:"ilink_user_id"`
	TypingTicket string         `json:"typing_ticket"`
	Status       int            `json:"status"`
	BaseInfo     wechatBaseInfo `json:"base_info"`
}

type wechatSendTypingResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg,omitempty"`
}

// --- CDN image upload + send (mirrors weclaw/messaging/cdn.go + media.go) ---
//
// iLink's image flow is two-leg:
//   1. POST /ilink/bot/getuploadurl to mint a one-shot CDN upload URL
//      (the bot supplies a random filekey + AES-128 key + plaintext
//      md5; server returns either a full URL or just a query param to
//      tack onto the well-known CDN endpoint).
//   2. POST the AES-128-ECB-encrypted bytes to that URL; server replies
//      with an X-Encrypted-Param header that becomes the
//      ImageItem.media.encrypt_query_param for the eventual sendmessage.
//
// AES key wire format is a base64-encoded *hex string* (not the raw
// 16 bytes) — quirk of the iLink protocol, preserved here for
// compatibility with the upstream daemon.

type wechatImageItem struct {
	URL     string           `json:"url,omitempty"`
	Media   *wechatMediaInfo `json:"media,omitempty"`
	MidSize int              `json:"mid_size,omitempty"` // ciphertext size
}

type wechatMediaInfo struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`      // base64(hex(raw_key))
	EncryptType       int    `json:"encrypt_type"` // 1 = AES-128-ECB
}

type wechatGetUploadURLRequest struct {
	FileKey     string         `json:"filekey"`
	MediaType   int            `json:"media_type"`
	ToUserID    string         `json:"to_user_id"`
	RawSize     int            `json:"rawsize"`
	RawFileMD5  string         `json:"rawfilemd5"`
	FileSize    int            `json:"filesize"`
	NoNeedThumb bool           `json:"no_need_thumb"`
	AESKey      string         `json:"aeskey"`
	BaseInfo    wechatBaseInfo `json:"base_info"`
}

type wechatGetUploadURLResponse struct {
	Ret           int    `json:"ret"`
	ErrMsg        string `json:"errmsg,omitempty"`
	UploadParam   string `json:"upload_param"`
	UploadFullURL string `json:"upload_full_url,omitempty"`
}

// wechatUploadedFile is the post-upload handle: enough to mint a
// MediaInfo reference (image/video/file) for the follow-up sendmessage.
type wechatUploadedFile struct {
	DownloadParam string
	AESKeyHex     string
	FileSize      int // plaintext size — needed by FileItem.Len
	CipherSize    int
}

// sendMedia uploads one MediaItem's bytes to the iLink CDN and posts a
// sendmessage referencing the result. The MediaItem's ContentType /
// Filename pick the wire shape: image (type=2), video (type=5), or file
// (type=4) for everything else (including audio — outbound voice items
// need codec/sample-rate metadata we don't reliably have, and sending
// audio as a file still plays back inline in WeChat). Mirrors the
// dispatcher in upstream weclaw/messaging/media.go.
func (w *WeChat) sendMedia(chatID string, item bus.MediaItem) error {
	cdnMediaType, itemType := classifyWeChatMedia(item)

	ctx, cancel := context.WithTimeout(context.Background(), wechatMediaSendTimeout)
	defer cancel()

	uploaded, err := w.uploadToCDN(ctx, chatID, item.Bytes, cdnMediaType)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	w.ctxTokensMu.Lock()
	contextToken := w.ctxTokens[chatID]
	w.ctxTokensMu.Unlock()

	media := &wechatMediaInfo{
		EncryptQueryParam: uploaded.DownloadParam,
		AESKey:            base64.StdEncoding.EncodeToString([]byte(uploaded.AESKeyHex)),
		EncryptType:       wechatCDNEncryptType,
	}

	var sendItem wechatItem
	switch itemType {
	case wechatItemTypeImage:
		sendItem = wechatItem{
			Type: wechatItemTypeImage,
			ImageItem: &wechatImageItem{
				Media:   media,
				MidSize: uploaded.CipherSize,
			},
		}
	case wechatItemTypeVideo:
		sendItem = wechatItem{
			Type: wechatItemTypeVideo,
			VideoItem: &wechatVideoItem{
				Media:     media,
				VideoSize: uploaded.CipherSize,
			},
		}
	default: // wechatItemTypeFile
		fileName := item.Filename
		if fileName == "" {
			fileName = "file"
		}
		sendItem = wechatItem{
			Type: wechatItemTypeFile,
			FileItem: &wechatFileItem{
				Media:    media,
				FileName: fileName,
				Len:      strconv.Itoa(uploaded.FileSize),
			},
		}
	}

	body := wechatSendRequest{
		Msg: wechatSendMsg{
			FromUserID:   w.accountID,
			ToUserID:     chatID,
			ClientID:     uuid.NewString(),
			MessageType:  wechatMsgTypeBot,
			MessageState: wechatMsgStateFinish,
			ItemList:     []wechatItem{sendItem},
			ContextToken: contextToken,
		},
		BaseInfo: wechatBaseInfo{},
	}
	var resp wechatSendResponse
	if err := w.doPost(ctx, "/ilink/bot/sendmessage", body, &resp); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	if resp.Ret != 0 {
		return fmt.Errorf("send: ret=%d errmsg=%s", resp.Ret, resp.ErrMsg)
	}
	slog.Debug("wechat media sent",
		"account", w.accountID, "chat", chatID,
		"filename", item.Filename, "kind", itemType, "bytes", len(item.Bytes))
	return nil
}

// classifyWeChatMedia decides how to send a MediaItem on iLink: image,
// video, or file (default). Prefers MediaItem.ContentType when set;
// otherwise infers from the filename extension. Audio falls through to
// file — matches upstream weclaw's classifyMedia behavior.
func classifyWeChatMedia(item bus.MediaItem) (cdnMediaType int, itemType int) {
	ct := strings.ToLower(item.ContentType)
	if ct == "" {
		ct = strings.ToLower(mime.TypeByExtension(filepath.Ext(item.Filename)))
	}
	if strings.HasPrefix(ct, "image/") || isWeChatImageExt(item.Filename) {
		return wechatCDNMediaTypeImage, wechatItemTypeImage
	}
	if strings.HasPrefix(ct, "video/") || isWeChatVideoExt(item.Filename) {
		return wechatCDNMediaTypeVideo, wechatItemTypeVideo
	}
	return wechatCDNMediaTypeFile, wechatItemTypeFile
}

func isWeChatImageExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

func isWeChatVideoExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mov", ".webm", ".mkv", ".avi":
		return true
	}
	return false
}

// uploadToCDN handles the AES-encrypted CDN upload leg for any media
// type. `mediaType` is one of wechatCDNMediaType{Image,Video,File} and
// determines how iLink's CDN classifies + serves the bytes later.
func (w *WeChat) uploadToCDN(ctx context.Context, toUserID string, data []byte, mediaType int) (*wechatUploadedFile, error) {
	filekey := make([]byte, 16)
	aeskey := make([]byte, 16)
	if _, err := rand.Read(filekey); err != nil {
		return nil, fmt.Errorf("filekey: %w", err)
	}
	if _, err := rand.Read(aeskey); err != nil {
		return nil, fmt.Errorf("aeskey: %w", err)
	}
	filekeyHex := hex.EncodeToString(filekey)
	aeskeyHex := hex.EncodeToString(aeskey)

	hash := md5.Sum(data)
	rawMD5 := hex.EncodeToString(hash[:])
	cipherSize := wechatAESECBPaddedSize(len(data))

	encrypted, err := wechatAESECBEncrypt(data, aeskey)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	// Retry the whole getuploadurl + POST. Each attempt mints a FRESH
	// pre-signed upload URL (new taskid → a different CDN node): retrying
	// the same URL is correlated — one bad node/URL 500s every time —
	// whereas fresh URLs give independent chances. The first POST right
	// after a getuploadurl also frequently 500s while the CDN propagates
	// the filekey registration, so the backoff before the next fresh
	// attempt lets that settle too.
	upReq := wechatGetUploadURLRequest{
		FileKey:     filekeyHex,
		MediaType:   mediaType,
		ToUserID:    toUserID,
		RawSize:     len(data),
		RawFileMD5:  rawMD5,
		FileSize:    cipherSize,
		NoNeedThumb: true,
		AESKey:      aeskeyHex,
		BaseInfo:    wechatBaseInfo{ChannelVersion: "1.0.0"},
	}
	// 每次重试都重新 getuploadurl 拿全新预签名 URL。-5104001 是 CDN 节点未传播该
	// URL，同 URL 重试还是撞同一个没传播的节点；每次换新 URL = 换 CDN 节点，撞到
	// 已传播节点的概率更高（IPv6 上行慢 + URL 短有效期下尤其需要多给几次机会）。
	var lastErr error
	for attempt := 1; attempt <= wechatCDNUploadRetries; attempt++ {
		cdnURL, err := w.resolveCdnUploadURL(ctx, upReq, encrypted, filekeyHex)
		if err != nil {
			lastErr = err
			if attempt < wechatCDNUploadRetries {
				wechatCDNBackoff(ctx, attempt)
			}
			continue
		}
		downloadParam, err := wechatUploadCDNPost(ctx, encrypted, cdnURL)
		if err == nil {
			if attempt > 1 {
				slog.Info("wechat cdn upload succeeded after retry", "attempt", attempt)
			}
			return &wechatUploadedFile{
				DownloadParam: downloadParam,
				AESKeyHex:     aeskeyHex,
				FileSize:      len(data),
				CipherSize:    cipherSize,
			}, nil
		}
		lastErr = err
		if attempt < wechatCDNUploadRetries {
			slog.Debug("wechat cdn upload attempt failed — retrying with fresh URL",
				"account", w.accountID, "attempt", attempt, "error", err)
			wechatCDNBackoff(ctx, attempt)
		}
	}
	return nil, lastErr
}

// cdnUploadAttempt runs one full upload leg — getuploadurl → construct the
// CDN URL → a single AES-encrypted POST — and returns the download
// encrypted_query_param on success. uploadToCDN retries, re-minting the
// pre-signed URL each attempt (independent CDN node/taskid).
// resolveCdnUploadURL 调一次 getuploadurl 并构造上传 URL（不 POST）。uploadToCDN
// 对同一个 URL retry——对齐 Tencent openclaw-weixin 和 corespeed-io/wechatbot。
func (w *WeChat) resolveCdnUploadURL(ctx context.Context, upReq wechatGetUploadURLRequest, encrypted []byte, filekeyHex string) (string, error) {
	var upResp wechatGetUploadURLResponse
	if err := w.doPost(ctx, "/ilink/bot/getuploadurl", upReq, &upResp); err != nil {
		return "", fmt.Errorf("getuploadurl: %w", err)
	}
	if upResp.Ret != 0 {
		return "", fmt.Errorf("getuploadurl ret=%d errmsg=%s", upResp.Ret, upResp.ErrMsg)
	}
	cdnURL := ""
	if upResp.UploadParam != "" {
		// Build the upload URL from upload_param ourselves instead of
		// using the server's upload_full_url. The corespeed-io/wechatbot
		// Go reference deliberately ignores upload_full_url (it carries an
		// extra taskid and 500s far more reliably than the upload_param
		// URL) and always constructs from upload_param + filekey. We saw
		// the same: every POST to upload_full_url 500'd.
		cdnURL = fmt.Sprintf("%s/upload?encrypted_query_param=%s&filekey=%s",
			wechatCDNBaseURL, encodeURIComponent(upResp.UploadParam), encodeURIComponent(filekeyHex))
	} else {
		cdnURL = strings.TrimSpace(upResp.UploadFullURL)
		if cdnURL == "" {
			return "", fmt.Errorf("getuploadurl returned no URL")
		}
	}
	// DIAG: log upload URL source + taskid presence to tell -5104001 CDN
	// propagation race apart from upload_full_url taskid failure. INFO for
	// the investigation; demote to Debug once resolved.
	urlSource := "upload_full_url"
	if upResp.UploadParam != "" {
		urlSource = "upload_param"
	}
	slog.Info("wechat cdn upload url",
		"account", w.accountID,
		"source", urlSource,
		"has_taskid", strings.Contains(cdnURL, "taskid="),
		"url_len", len(cdnURL),
		"cipher_size", len(encrypted))
	return cdnURL, nil
}

// wechatCDNClient pins TLS for CDN uploads. curl -6 shows the iLink CDN
// negotiates TLS 1.2 with TLS_RSA_WITH_AES_256_GCM_SHA384 (RSA key exchange);
// Go 1.17+ defaults to ECDHE-only cipher suites, so its ClientHello carries
// no cipher the CDN accepts and the handshake fails with "remote error: tls:
// handshake failure". Explicitly enable the RSA suites (plus ECDHE fallback)
// and cap at TLS 1.2 to mirror what curl negotiates. (IPv4 to the CDN is
// unreachable from this host — TCP timeouts on every address — so this relies
// on Go Happy Eyeballs picking the working IPv6 path.)
var wechatCDNClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MaxVersion: tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			},
		},
	},
}

// wechatUploadCDNPost POSTs the AES-encrypted payload to the CDN once and
// returns the X-Encrypted-Param header from the response — the opaque token
// the bot later embeds as encrypt_query_param so the recipient's WeChat
// client can fetch + decrypt. Single shot; uploadToCDN handles retries.
//
// Runs under wechatCDNUploadAttemptTimeout so a hung CDN POST (the endpoint
// sometimes goes silent mid-upload) can't consume the whole media-send
// budget. On failure the CDN often returns a bare status with empty body
// AND empty X-Error-Message, so we log the full response header set — that's
// the only place a clue (proxy/limit/retry hint) can hide.
func wechatUploadCDNPost(ctx context.Context, encrypted []byte, cdnURL string) (string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, wechatCDNUploadAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, cdnURL, bytes.NewReader(encrypted))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := wechatCDNClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		downloadParam := resp.Header.Get("X-Encrypted-Param")
		if downloadParam == "" {
			return "", fmt.Errorf("HTTP 200 but missing X-Encrypted-Param header")
		}
		return downloadParam, nil
	}
	errMsg := strings.TrimSpace(resp.Header.Get("X-Error-Message"))
	if errMsg == "" {
		errMsg = strings.TrimSpace(string(body))
	}
	if errMsg == "" {
		// The CDN's 5xx usually carries no X-Error-Message and an empty
		// body; fall back to the numeric X-Error-Code so a total failure
		// (all retries exhausted) is still legible at default log level.
		if code := strings.TrimSpace(resp.Header.Get("X-Error-Code")); code != "" {
			errMsg = "x-error-code " + code
		}
	}
	// Per-attempt detail at Debug: a first-attempt 5xx that the retry
	// covers is the normal CDN propagation race, so it shouldn't spam
	// WARN. A total failure surfaces via the returned error → sendMedia's
	// "wechat send media failed" WARN.
	slog.Debug("wechat cdn upload non-200",
		"status", resp.StatusCode, "errMsg", errMsg,
		"headers", resp.Header, "body_len", len(body))
	return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, errMsg)
}

// wechatCDNBackoff sleeps briefly between CDN upload retries. Cancellable
// via ctx (the caller's media-send timeout) so a wedged CDN doesn't hold
// the turn beyond wechatMediaSendTimeout.
func wechatCDNBackoff(ctx context.Context, attempt int) {
	select {
	case <-time.After(time.Duration(attempt) * time.Second):
	case <-ctx.Done():
	}
}

func wechatAESECBEncrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	encrypted := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(encrypted[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}
	return encrypted, nil
}

// downloadAndDecryptBytes fetches an AES-128-ECB encrypted blob from the
// iLink CDN and returns the decrypted plaintext. Shared by the image
// path (returns a data URL) and the file path (returns raw bytes for
// MediaItems → /workspace). media carries the per-message
// encrypt_query_param + AES key (base64(hex(raw_key))).
func (w *WeChat) downloadAndDecryptBytes(media *wechatMediaInfo) ([]byte, error) {
	if media == nil || media.EncryptQueryParam == "" {
		return nil, fmt.Errorf("no media info")
	}
	// Reconstruct the CDN download URL from the encrypt_query_param.
	cdnURL := fmt.Sprintf("%s/download?encrypted_query_param=%s",
		wechatCDNBaseURL, url.QueryEscape(media.EncryptQueryParam))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDN HTTP %d", resp.StatusCode)
	}
	ciphertext, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Decode the AES key. Wire format: base64(hex_string).
	aesKeyHex, err := base64.StdEncoding.DecodeString(media.AESKey)
	if err != nil {
		return nil, fmt.Errorf("decode aes key base64: %w", err)
	}
	aesKey, err := hex.DecodeString(string(aesKeyHex))
	if err != nil {
		return nil, fmt.Errorf("decode aes key hex: %w", err)
	}

	plaintext, err := wechatAESECBDecrypt(ciphertext, aesKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

// downloadAndDecryptImage fetches an AES-128-ECB encrypted image from
// the iLink CDN and returns a base64 data URL the vision model can read.
func (w *WeChat) downloadAndDecryptImage(img *wechatImageItem) (string, error) {
	plaintext, err := w.downloadAndDecryptBytes(img.Media)
	if err != nil {
		return "", err
	}
	ct := http.DetectContentType(plaintext)
	b64 := base64.StdEncoding.EncodeToString(plaintext)
	return fmt.Sprintf("data:%s;base64,%s", ct, b64), nil
}

// wechatAESECBDecrypt is the inverse of wechatAESECBEncrypt — PKCS7 unpad.
func wechatAESECBDecrypt(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext not aligned to block size")
	}
	plaintext := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(plaintext[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}
	// PKCS7 unpad.
	if len(plaintext) == 0 {
		return plaintext, nil
	}
	padLen := int(plaintext[len(plaintext)-1])
	if padLen > aes.BlockSize || padLen == 0 {
		return plaintext, nil // not padded or invalid — return as-is
	}
	return plaintext[:len(plaintext)-padLen], nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func truncateMediaParam(m *wechatMediaInfo) string {
	if m == nil {
		return ""
	}
	return truncate(m.EncryptQueryParam, 40)
}

func wechatAESECBPaddedSize(plaintextSize int) int {
	return (plaintextSize/aes.BlockSize + 1) * aes.BlockSize
}

// encodeURIComponent mirrors JS encodeURIComponent, which the upstream
// openclaw-weixin uses to build the CDN upload URL query string. Go's
// url.QueryEscape differs on points that matter for the CDN's signed
// upload_param: it encodes ' ' as '+' (not '%20') and also encodes
// !*'(). Matching the upstream's encoding avoids corrupting the param.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z', '0' <= r && r <= '9':
			b.WriteRune(r)
		case strings.ContainsRune("-_.!~*'()", r):
			b.WriteRune(r)
		default:
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
	}
	return b.String()
}
