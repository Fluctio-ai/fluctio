package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/workspace"
)

// RegisterShareFileTool registers share_file — the agent's way to hand a
// generated workspace artifact (audio, images, HTML, PDFs, …) to consumers
// that cannot authenticate: smart speakers playing generated audio, IM
// contacts, email recipients. The returned /f/{token} URL needs no login;
// expiry and use limits are enforced server-side. This replaces uploading
// artifacts to external file hosts whose links rot.
func RegisterShareFileTool(r *Registry, st store.Store, agentID string) {
	r.Register("share_file",
		"Create a public share link (no login required) for a file in this chat's workspace — generated audio, images, HTML, PDFs, any artifact. Use it whenever the file must be fetched by something that cannot log in: a smart speaker playing generated audio (小爱音箱播放生成的播客/新闻), an IM contact, an email recipient. The URL itself is the credential. Prefer this over uploading to external file hosts (临时网盘) — those links expire unpredictably. ttl_hours defaults to 168 (7 days); pass 0 for a permanent link, or a shorter window for one-off playback. max_uses (default 0 = unlimited) caps how often the link can be fetched — CAUTION: every fetch burns one use, including IM link previews and URL scanners, so for playback prefer an unlimited time-boxed link over a use-capped one. The link keeps working until it expires even if the chat continues; revoking is possible from the web console's share dialog.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "File to share, relative to this chat's workspace (e.g. \"morning_news.mp3\", \"../shared/report.html\" for the project-shared layer). The same path write_file/list_dir use.",
				},
				"ttl_hours": map[string]interface{}{
					"type":        "integer",
					"description": "How long the link stays valid, in hours. Default 168 (7 days). Pass 0 for a permanent link.",
				},
				"max_uses": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of fetches allowed (default 0 = unlimited). Remember IM link previews and scanners each consume one use.",
				},
			},
			"required": []string{"path"},
		},
		makeShareFile(r, st, agentID),
	)
}

func makeShareFile(r *Registry, st store.Store, agentID string) func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Path     string `json:"path"`
			TTLHours *int   `json:"ttl_hours,omitempty"` // nil → default 168; 0 → permanent
			MaxUses  int    `json:"max_uses,omitempty"`  // 0 → unlimited
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		// The tool has no request to infer a host from, so external
		// reachability requires the operator-configured base URL.
		base := strings.TrimRight(config.LoadEnv().Gateway.PublicBaseURL, "/")
		if base == "" {
			return "", fmt.Errorf("share links need FLUCTIO_PUBLIC_BASE_URL to be configured on the server (the externally reachable address), ask the operator to set it")
		}
		sp, err := r.wsScope(args.Path)
		if err != nil {
			return "", err
		}
		// Resolve the scope tuple into the agent-root store key the share
		// row (and the file server's Get(agentID, "", "", key)) speaks.
		key := sp.storePath
		if seg := workspace.ScopeKeyPrefix(sp.projectID, sp.sessionID); seg != "" {
			key = seg + "/" + sp.storePath
		}
		if strings.EqualFold(filepath.Base(filepath.Clean(key)), "SKILL.md") {
			return "", fmt.Errorf("refused: skill manifests are not shareable")
		}
		if r.workspaceStore == nil {
			return "", fmt.Errorf("no workspace store configured")
		}
		if _, err := r.workspaceStore.Stat(ctx, agentID, sp.projectID, sp.sessionID, sp.storePath); err != nil {
			return "", fmt.Errorf("file %q not found in workspace (write it first, then share)", args.Path)
		}

		ttlHours := 168
		if args.TTLHours != nil {
			ttlHours = *args.TTLHours
		}
		if ttlHours < 0 || args.MaxUses < 0 {
			return "", fmt.Errorf("ttl_hours and max_uses must be >= 0")
		}
		var expires *time.Time
		validNote := "permanent"
		if ttlHours > 0 {
			t := time.Now().Add(time.Duration(ttlHours) * time.Hour).UTC()
			expires = &t
			validNote = t.Format("2006-01-02 15:04 UTC")
		}
		token, err := newShareToken()
		if err != nil {
			return "", err
		}
		rec := &store.FileShareRecord{
			Token:     token,
			AgentID:   agentID,
			Path:      key,
			ExpiresAt: expires,
			MaxUses:   args.MaxUses,
		}
		if err := st.CreateFileShare(ctx, rec); err != nil {
			return "", err
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Public URL (no login needed): %s/f/%s\n", base, token)
		fmt.Fprintf(&sb, "File: %s\nValid until: %s\n", key, validNote)
		if args.MaxUses > 0 {
			fmt.Fprintf(&sb, "Use limit: %d fetch(es) — every fetch counts, including link previews.\n", args.MaxUses)
		}
		return sb.String(), nil
	}
}

// newShareToken mints the unguessable link credential (32 random bytes,
// hex — same shape as the HTTP handler's tokens; duplicated here to keep
// the tools package free of a setup import).
func newShareToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
