package setup

// handlers_shares.go serves capability-limited public links to workspace
// files. POST /api/agents/{id}/share/{path...} mints a link (auth required),
// GET /f/{token} serves the file with NO login — the token is the only
// credential and carries no path/agent information. GET /api/agents/{id}/shares
// lists a agent's links, DELETE /api/share/{token} revokes one. Expiry and
// use limits are enforced atomically in store.ConsumeFileShare.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// handleAgentFileShare mints a public /f/{token} link for one workspace
// file. Body: {"ttlHours": 168} (0 = permanent) and {"maxUses": 0}
// (0 = unlimited). The path is the agent-root store key exactly as the
// file list / tree hands the client.
func (s *Server) handleAgentFileShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel := r.PathValue("path")
	if rel == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	// Same name-level guard as handleAgentFile: skill manifests are the
	// agent's IP and never legitimately shareable.
	if isSkillManifestPath(rel) {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "refused: skill manifests are not downloadable"})
		return
	}
	var req struct {
		TTLHours int `json:"ttlHours"` // 0 = permanent
		MaxUses  int `json:"maxUses"`  // 0 = unlimited
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if req.TTLHours < 0 || req.MaxUses < 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "ttlHours and maxUses must be >= 0"})
		return
	}
	if s.dataStore == nil || s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace store not configured"})
		return
	}
	// Refuse to mint links to files that don't exist (typos, deleted
	// artifacts) — a dead link minted now can't be diagnosed later.
	if _, err := s.workspaceStore.Stat(r.Context(), id, "", "", rel); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "file not found"})
		return
	}

	var expires *time.Time
	if req.TTLHours > 0 {
		t := time.Now().Add(time.Duration(req.TTLHours) * time.Hour).UTC()
		expires = &t
	}
	createdBy := ""
	if ident, ok := authIdentity(r); ok {
		createdBy = ident.UserID
	}
	token, err := newShareToken()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rec := &store.FileShareRecord{
		Token:     token,
		AgentID:   id,
		Path:      rel,
		CreatedBy: createdBy,
		ExpiresAt: expires,
		MaxUses:   req.MaxUses,
	}
	if err := s.dataStore.CreateFileShare(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":        true,
		"token":     token,
		"url":       shareURL(r, token),
		"path":      rel,
		"expiresAt": expires,
		"maxUses":   req.MaxUses,
	})
}

// handleSharedFile serves the file behind a share token with no auth. Each
// successful hit consumes one use; expired / exhausted links 410, unknown
// tokens 404 (same answer for every miss, so tokens can't be probed).
func (s *Server) handleSharedFile(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil || s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace store not configured"})
		return
	}
	token := r.PathValue("token")
	rec, err := s.dataStore.ConsumeFileShare(r.Context(), token, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		case errors.Is(err, store.ErrFileShareExhausted):
			jsonResponse(w, http.StatusGone, map[string]any{"error": "share link expired or exhausted"})
		default:
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	// Defense in depth: the mint-side guard already refuses SKILL.md, but a
	// row written by a future/buggy path must not become an exfil channel.
	if isSkillManifestPath(rec.Path) {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "refused: skill manifests are not downloadable"})
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(filepath.Clean(rec.Path))}))
	}
	s.serveFileFromWorkspaceStore(w, r, rec.AgentID, rec.Path)
}

// handleAgentShareList lists every share (including expired ones, so the
// UI can show and revoke them) for one agent.
func (s *Server) handleAgentShareList(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "store not configured"})
		return
	}
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	recs, err := s.dataStore.ListFileSharesByAgent(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if recs == nil {
		recs = []store.FileShareRecord{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "shares": recs})
}

// handleShareDelete revokes a link. Any caller who can read the owning
// agent may revoke — the token alone is never enough to discover or kill
// a share.
func (s *Server) handleShareDelete(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "store not configured"})
		return
	}
	token := r.PathValue("token")
	rec, err := s.dataStore.GetFileShare(r.Context(), token)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.requireAgentReadable(w, r, rec.AgentID) {
		return
	}
	if err := s.dataStore.DeleteFileShare(r.Context(), token); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// newShareToken mints the unguessable link credential: 32 random bytes,
// hex-encoded (same shape as session SIDs).
func newShareToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// shareURL builds the public URL for a token. FLUCTIO_PUBLIC_BASE_URL wins
// (the canonical external address on reverse-proxied deployments), else the
// request's own host so LAN links still work; falls back to a relative path
// when neither is usable (the web UI joins it against its own origin).
func shareURL(r *http.Request, token string) string {
	p := "/f/" + token
	if base := strings.TrimRight(config.LoadEnv().Gateway.PublicBaseURL, "/"); base != "" {
		return base + p
	}
	if r.Host == "" {
		return p
	}
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host + p
}

// isSkillManifestPath is the name-level guard shared by the mint and serve
// paths (see handleAgentFile for the rationale).
func isSkillManifestPath(rel string) bool {
	return strings.EqualFold(filepath.Base(filepath.Clean(rel)), "SKILL.md")
}
