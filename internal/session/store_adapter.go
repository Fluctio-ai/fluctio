package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// StoreAdapter adapts store.Store to the SessionStore interface for one
// owning user. Each UserSpace creates its own adapter so the user_id
// scoping is implicit at the call site instead of getting plumbed through
// every agent loop call.
type StoreAdapter struct {
	st     store.Store
	userID string
}

func NewStoreAdapter(st store.Store, userID string) *StoreAdapter {
	return &StoreAdapter{st: st, userID: userID}
}

func (a *StoreAdapter) GetSession(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error) {
	rec, err := a.st.GetSession(ctx, agentID, sessionKey)
	if err != nil || rec == nil {
		return nil, err
	}
	msgs := make([]provider.Message, len(rec.Messages))
	for i, m := range rec.Messages {
		msgs[i] = provider.Message{
			Role:         m.Role,
			Content:      m.Content,
			ToolCallID:   m.ToolCallID,
			Name:         m.Name,
			Metadata:     m.Metadata,
			Thinking:     m.Thinking,
			RawAssistant: m.RawAssistant,
			Origin:       m.Origin,
		}
		// ToolCalls / ContentParts are stored as interface{} so a
		// JSON round-trip leaves them as []interface{} / map nests.
		// Re-marshal + unmarshal to recover the typed slice — without
		// this, a refreshed history loses tool-group bubbles AND the
		// next provider call sends a multimodal user turn with no
		// content (ContentParts dropped → Content "" → API rejects).
		if m.ToolCalls != nil {
			if raw, err := json.Marshal(m.ToolCalls); err == nil {
				var tcs []provider.ToolCall
				if json.Unmarshal(raw, &tcs) == nil {
					msgs[i].ToolCalls = tcs
				}
			}
		}
		if m.ContentParts != nil {
			if raw, err := json.Marshal(m.ContentParts); err == nil {
				var parts []provider.ContentPart
				if json.Unmarshal(raw, &parts) == nil {
					msgs[i].ContentParts = parts
				}
			}
		}
	}
	return msgs, nil
}

func (a *StoreAdapter) SaveSession(ctx context.Context, agentID, sessionKey, channel, accountID, chatID, projectID string, messages []provider.Message) error {
	rec := &store.SessionRecord{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		ProjectID: projectID,
		Messages:  make([]store.SessionMessage, len(messages)),
		UpdatedAt: time.Now(),
	}
	for i, m := range messages {
		rec.Messages[i] = sessionMessageFromProvider(m)
	}
	return a.st.SaveSession(ctx, agentID, sessionKey, rec)
}

// ResolveActiveSessionKey forwards to the store. The session.Manager
// uses this to pick the active session_key for an inbound (channel,
// account, chat) triple before any messages get loaded.
func (a *StoreAdapter) ResolveActiveSessionKey(ctx context.Context, agentID, channel, accountID, chatID string) (string, error) {
	k, err := a.st.ResolveActiveSessionKey(ctx, agentID, channel, accountID, chatID)
	if err != nil {
		// Translate ErrNotFound to ("", nil) so the manager treats the
		// "no session yet" case as a normal mint trigger instead of
		// surfacing an error.
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return k, nil
}

// LookupSessionTriple inverts ResolveActiveSessionKey: session_key →
// (channel, accountID, chatID). Used when a URL hand-off carries only
// the session_key and the handler needs the conversation triple back.
func (a *StoreAdapter) LookupSessionTriple(ctx context.Context, agentID, sessionKey string) (string, string, string, error) {
	ch, acc, ci, err := a.st.LookupSessionTriple(ctx, agentID, sessionKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", "", nil
		}
		return "", "", "", err
	}
	return ch, acc, ci, nil
}

// LookupSessionProject returns the project_id stamped on the session
// row (or "" for loose chats). Treats not-found as "no project" rather
// than an error so callers can use the empty string to mean "fall back
// to the per-chat workspace dir".
func (a *StoreAdapter) LookupSessionProject(ctx context.Context, agentID, sessionKey string) (string, error) {
	pid, err := a.st.LookupSessionProject(ctx, agentID, sessionKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return pid, nil
}

// AppendMessage persists one turn into session_messages — the append-only
// archive parallel to the sessions blob. Called from Session.Append on
// every Append, in addition to SaveSession.
func (a *StoreAdapter) AppendMessage(ctx context.Context, agentID, sessionKey string, m provider.Message) error {
	return a.st.AppendSessionMessage(ctx, agentID, sessionKey, sessionMessageFromProvider(m))
}

// AppendMessageHidden writes m to the archive marked llm_visible=false so
// it stays out of the LLM working set / summary / recall while remaining
// visible in web history. The in-memory session slice is untouched.
func (a *StoreAdapter) AppendMessageHidden(ctx context.Context, agentID, sessionKey string, m provider.Message) error {
	sm := sessionMessageFromProvider(m)
	sm.LLMVisible = false
	return a.st.AppendSessionMessage(ctx, agentID, sessionKey, sm)
}

// ListMessages reads the full archive for one session, in turn order.
// Used by the chat history UI so users see the original conversation
// even after compaction has shrunk the LLM-facing working set.
func (a *StoreAdapter) ListMessages(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error) {
	sms, err := a.st.ListSessionMessages(ctx, agentID, sessionKey)
	if err != nil {
		return nil, err
	}
	msgs := make([]provider.Message, len(sms))
	for i, m := range sms {
		msgs[i] = providerMessageFromStored(m)
	}
	return msgs, nil
}

// sessionMessageFromProvider converts a provider.Message into the wire
// shape stored in both sessions.messages (as a JSON array element) and
// session_messages (as a row). Single conversion site so the two paths
// can't drift.
func sessionMessageFromProvider(m provider.Message) store.SessionMessage {
	out := store.SessionMessage{
		Role:         m.Role,
		Content:      m.Content,
		ToolCallID:   m.ToolCallID,
		Name:         m.Name,
		Metadata:     m.Metadata,
		Timestamp:    time.Now(),
		Thinking:     m.Thinking,
		RawAssistant: m.RawAssistant,
		Origin:       m.Origin,
		Provider:     m.Provider,
		Model:        m.Model,
		LLMVisible:   true,
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = m.ToolCalls
	}
	if len(m.ContentParts) > 0 {
		out.ContentParts = m.ContentParts
	}
	return out
}

// providerMessageFromStored is the inverse of sessionMessageFromProvider.
// JSON-tunnel ToolCalls / ContentParts back into typed provider slices,
// otherwise the generic interface{} shape leaves them as map nests and
// downstream callers see "no tool calls / no parts" on a populated row.
func providerMessageFromStored(m store.SessionMessage) provider.Message {
	out := provider.Message{
		Role:         m.Role,
		Content:      m.Content,
		ToolCallID:   m.ToolCallID,
		Name:         m.Name,
		Metadata:     m.Metadata,
		Thinking:     m.Thinking,
		RawAssistant: m.RawAssistant,
		Origin:       m.Origin,
		Provider:     m.Provider,
		Model:        m.Model,
		Seq:          m.Seq,
	}
	// Restore created_at as the provider.Message timestamp (unix ms) so
	// WebChatHistory can place produced files under the message whose
	// turn created them. Zero-time (no row / pre-archive session) leaves
	// Timestamp at 0 and the UI falls back to the last agent message.
	if !m.Timestamp.IsZero() {
		out.Timestamp = m.Timestamp.UnixMilli()
	}
	if m.ToolCalls != nil {
		if raw, err := json.Marshal(m.ToolCalls); err == nil {
			var tcs []provider.ToolCall
			if json.Unmarshal(raw, &tcs) == nil {
				out.ToolCalls = tcs
			}
		}
	}
	if m.ContentParts != nil {
		if raw, err := json.Marshal(m.ContentParts); err == nil {
			var parts []provider.ContentPart
			if json.Unmarshal(raw, &parts) == nil {
				out.ContentParts = parts
			}
		}
	}
	return out
}

// ListWebSessions returns every chat session for this agent regardless
// of channel — the historical name is kept to avoid a sweep of every
// caller, but the result spans web + IM channels. Each row's Channel
// is set so the dashboard can render the source-channel icon prefix.
//
// ID is the session_key (the canonical, channel-independent row id).
// The agent-side history/delete/rename handlers accept either a
// session_key or a legacy `<chat_id>` URL token via ResolveSessionKey.
func (a *StoreAdapter) ListWebSessions(ctx context.Context, agentID string) ([]WebSession, error) {
	metas, err := a.st.ListSessions(ctx, agentID)
	if err != nil {
		return nil, err
	}
	var sessions []WebSession
	for _, m := range metas {
		if m.AgentID == "" {
			m.AgentID = agentID
		}
		ws := a.BuildWebSession(ctx, m)
		if ws != nil {
			sessions = append(sessions, *ws)
		}
	}
	return sessions, nil
}

// BuildWebSession converts a single SessionMeta into a WebSession by
// resolving the preview text and thumbnail from the message archive.
// Returns nil when the session has no displayable user turn (empty
// sessions are omitted from listings).
func (a *StoreAdapter) BuildWebSession(ctx context.Context, m store.SessionMeta) *WebSession {
	agentID := m.AgentID
	channel := m.Channel
	if channel == "" {
		if i := strings.Index(m.Key, "_"); i > 0 {
			channel = m.Key[:i]
		}
	}
	preview := ""
	thumb := ""
	archive, _ := a.st.ListSessionMessages(ctx, agentID, m.Key)
	var source []store.SessionMessage
	if len(archive) > 0 {
		source = archive
	} else if rec, err := a.st.GetSession(ctx, agentID, m.Key); err == nil && rec != nil {
		source = rec.Messages
	}
	for _, msg := range source {
		if msg.Role != "user" {
			continue
		}
		text := userText(msg)
		img := userImage(msg)
		if text == "" && img == "" {
			continue
		}
		if msg.Origin != "" {
			if obj := extractObjective(text); obj != "" {
				text = obj
			}
		}
		preview = text
		if preview == "" {
			preview = "[image]"
		}
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		thumb = img
		break
	}
	if preview == "" {
		return nil
	}
	title := displaySessionTitle(m.Title, m.Key, preview)
	return &WebSession{
		ID:            m.Key,
		Channel:       channel,
		AccountID:     m.AccountID,
		ChatID:        m.ChatID,
		ProjectID:     m.ProjectID,
		Title:         title,
		Preview:       preview,
		ThumbnailURL:  thumb,
		CreatedAt:     m.UpdatedAt.UnixMilli(),
		UpdatedAt:     m.UpdatedAt.UnixMilli(),
	}
}

// displaySessionTitle normalizes legacy rows that persisted the opaque
// session_key as their title.  Treating that value as a real custom title
// prevents the UI's otherwise-correct title -> preview -> id fallback from
// ever reaching the first user message.
func displaySessionTitle(storedTitle, sessionKey, preview string) string {
	title := strings.TrimSpace(storedTitle)
	if title == "" || title == sessionKey {
		return preview
	}
	return title
}

// extractObjective pulls the `<objective>…</objective>` payload out of a
// goal-continuation prompt. Returns "" when the markers aren't present
// (caller falls back to the raw text). Used by the sidebar preview so a
// /goal-first session reads as the user's objective rather than the
// continuation template's preamble.
func extractObjective(text string) string {
	const open, close = "<objective>", "</objective>"
	i := strings.Index(text, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(text[i+len(open):], close)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(text[i+len(open) : i+len(open)+j])
}

// userText pulls the user-visible text from a stored user turn. Falls
// back to ContentParts' "text" parts when Content is empty (the shape
// produced by HandleMessageStream when the turn carried image
// attachments). Without this, callers gating on Content silently treat
// multimodal turns as empty.
func userText(m store.SessionMessage) string {
	if m.Content != "" {
		return provider.StripAttachedPrefix(m.Content)
	}
	if m.ContentParts == nil {
		return ""
	}
	raw, err := json.Marshal(m.ContentParts)
	if err != nil {
		return ""
	}
	var parts []provider.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return provider.StripAttachedPrefix(strings.Join(out, "\n"))
}

// userImage returns the first image_url URL from a stored user turn's
// ContentParts, or "" if none. Powers the sidebar thumbnail next to
// the chat title.
func userImage(m store.SessionMessage) string {
	if m.ContentParts == nil {
		return ""
	}
	raw, err := json.Marshal(m.ContentParts)
	if err != nil {
		return ""
	}
	var parts []provider.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	for _, p := range parts {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
			return p.ImageURL.URL
		}
	}
	return ""
}

func (a *StoreAdapter) DeleteSession(ctx context.Context, agentID, sessionKey string) error {
	return a.st.DeleteSession(ctx, agentID, sessionKey)
}

func (a *StoreAdapter) RenameSession(ctx context.Context, agentID, sessionKey, title string) error {
	return a.st.RenameSession(ctx, agentID, sessionKey, title)
}

func (a *StoreAdapter) MoveSession(ctx context.Context, agentID, sessionKey, projectID string) error {
	return a.st.MoveSession(ctx, agentID, sessionKey, projectID)
}
