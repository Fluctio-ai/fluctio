package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/privacy"
	"github.com/fluctio-ai/fluctio/internal/provider"
)

// MemoryStore is an optional interface for DB-backed memory persistence.
// userID is the chatter — chat-time MEMORY.md / USER.md updates land in
// that user's per-user override row so they don't pollute the shared
// template that the agent owner edits via the Customize page.
//
// GetWorkspaceFile vs GetWorkspaceFileExact:
//   - GetWorkspaceFile picks the caller's row first, falls back to the
//     agent owner's row when the caller has none. Used for shared
//     identity files (SOUL/IDENTITY/AGENTS/...): a chatter inherits
//     whatever the owner configured.
//   - GetWorkspaceFileExact returns ONLY the caller's row, or
//     ErrNotFound. Used for per-chatter files (USER.md, MEMORY.md):
//     a brand-new visitor must see an empty profile/memory, never
//     leak the owner's.
type MemoryStore interface {
	GetMemory(ctx context.Context, agentID, userID string) (string, error)
	SaveMemory(ctx context.Context, agentID, userID, content string) error
	GetWorkspaceFile(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	GetWorkspaceFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	SaveWorkspaceFile(ctx context.Context, agentID, userID, filename string, data []byte) error
}

type Memory struct {
	workspace string
	store     MemoryStore
	userID    string
	agentID   string
}

func NewMemory(workspace string) *Memory {
	return &Memory{workspace: workspace}
}

// NewMemoryWithStoreForUser is the user-scoped constructor. userID should be
// a real users.id resolved from auth. We keep the Memory alive on empty input
// so a bad request cannot crash the gateway; per-user store reads/writes then
// fail closed until a caller rebinds via WithUserID.
func NewMemoryWithStoreForUser(workspace string, st MemoryStore, userID, agentID string) *Memory {
	if userID == "" {
		slog.Error("agent.NewMemoryWithStoreForUser: empty userID", "agent", agentID)
	}
	return &Memory{workspace: workspace, store: st, userID: userID, agentID: agentID}
}

// UserID returns the userID this Memory is bound to (set via
// NewMemoryWithStoreForUser / WithUserID). Used by the agent loop's
// autoPersist gate to query the per-chatter user-message count
// without re-resolving chatterUID through the inbound message.
func (m *Memory) UserID() string { return m.userID }

// WithUserID returns a shallow copy bound to a different userID.
// Lets a per-turn caller rebind MEMORY.md / USER.md reads + writes to
// the chatter (rather than the agent owner) without mutating the
// shared agent-scoped Memory other concurrent turns may be reading.
// Returns nil when m is nil so callers don't have to nil-guard.
func (m *Memory) WithUserID(uid string) *Memory {
	if m == nil {
		return nil
	}
	out := *m
	out.userID = uid
	return &out
}

// ctx returns a context tagged with this Memory's user so SQL queries in
// the store layer scope correctly. Empty userID yields an unscoped ctx;
// store-backed methods guard that case separately and fail closed.
func (m *Memory) ctx() context.Context {
	if m.userID == "" {
		return context.Background()
	}
	return config.WithUserID(context.Background(), m.userID)
}

// memoryPath returns the path to MEMORY.md.
func (m *Memory) memoryPath() string {
	return filepath.Join(m.workspace, "MEMORY.md")
}

// historyPath returns the path to HISTORY.md.
func (m *Memory) historyPath() string {
	return filepath.Join(m.workspace, "HISTORY.md")
}

// LoadMemory reads the long-term memory for this Memory's user. When a
// store is configured we never fall back to the on-disk workspace
// MEMORY.md — that file is the agent owner's copy and would leak to
// any non-owner chatter whose row simply doesn't exist yet. FS read
// only fires on legacy single-user installs without a store.
func (m *Memory) LoadMemory() string {
	if m.store != nil {
		if m.userID == "" {
			return ""
		}
		content, err := m.store.GetMemory(m.ctx(), m.agentID, m.userID)
		if err == nil {
			return content
		}
		return ""
	}
	data, err := os.ReadFile(m.memoryPath())
	if err != nil {
		return ""
	}
	return string(data)
}

// SaveMemory overwrites the long-term memory.
func (m *Memory) SaveMemory(content string) error {
	if m.store != nil {
		if m.userID == "" {
			return fmt.Errorf("agent.Memory.SaveMemory: userID required")
		}
		return m.store.SaveMemory(m.ctx(), m.agentID, m.userID, content)
	}
	os.MkdirAll(m.workspace, 0o755)
	return os.WriteFile(m.memoryPath(), []byte(content), 0o644)
}

// AppendHistory adds an entry to the history log.
func (m *Memory) AppendHistory(entry string) error {
	os.MkdirAll(m.workspace, 0o755)
	f, err := os.OpenFile(m.historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	_, err = fmt.Fprintf(f, "- [%s] %s\n", timestamp, entry)
	return err
}

// LoadHistory reads the history log.
func (m *Memory) LoadHistory() string {
	data, err := os.ReadFile(m.historyPath())
	if err != nil {
		return ""
	}
	return string(data)
}

// ReviewAndUpdateMemory scans recent history entries and appends new key facts
// to MEMORY.md. This is called by the heartbeat to keep long-term memory fresh.
func (m *Memory) ReviewAndUpdateMemory(workspace string) {
	history := m.LoadHistory()
	if history == "" {
		return
	}

	// Get the last N lines of history to review
	lines := strings.Split(strings.TrimSpace(history), "\n")
	reviewCount := 50
	if len(lines) < reviewCount {
		reviewCount = len(lines)
	}
	recentLines := lines[len(lines)-reviewCount:]

	// Extract key facts from recent history (simple keyword-based extraction)
	currentMemory := m.LoadMemory()
	var newFacts []string

	for _, line := range recentLines {
		lower := strings.ToLower(line)
		// Look for lines that contain important keywords
		if containsAny(lower, []string{
			"learned", "discovered", "user prefers", "important",
			"remember", "note:", "key fact", "decision",
			"preference", "configured", "set up",
		}) {
			// Extract the content after the timestamp
			if idx := strings.Index(line, "] "); idx >= 0 {
				fact := strings.TrimSpace(line[idx+2:])
				if fact != "" && !strings.Contains(currentMemory, fact) {
					newFacts = append(newFacts, fact)
				}
			}
		}
	}

	if len(newFacts) == 0 {
		slog.Debug("memory review: no new facts to add")
		return
	}

	// Append new facts to MEMORY.md
	var sb strings.Builder
	sb.WriteString(currentMemory)
	if currentMemory != "" && !strings.HasSuffix(currentMemory, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("\n## Auto-updated: %s\n", time.Now().Format("2006-01-02 15:04")))
	for _, fact := range newFacts {
		sb.WriteString(fmt.Sprintf("- %s\n", fact))
	}

	if err := m.SaveMemory(sb.String()); err != nil {
		slog.Warn("failed to update memory", "error", err)
		return
	}

	slog.Info("memory updated", "new_facts", len(newFacts))
}

func containsAny(s string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// SaveMemoryWithScan scans content for threats before writing to MEMORY.md.
// Logs warnings for any detected threats but still writes (to avoid data loss).
func (m *Memory) SaveMemoryWithScan(content string) error {
	if threats := privacy.Scan(content); len(threats) > 0 {
		for _, t := range threats {
			slog.Warn("memory safety threat detected in MEMORY.md write",
				"type", t.Type,
				"pattern", t.Pattern,
				"context", t.Context,
			)
		}
	}
	return m.SaveMemory(content)
}

// SaveUserFile writes USER.md with threat scanning.
func (m *Memory) SaveUserFile(content string) error {
	if threats := privacy.Scan(content); len(threats) > 0 {
		for _, t := range threats {
			slog.Warn("memory safety threat detected in USER.md write",
				"type", t.Type,
				"pattern", t.Pattern,
				"context", t.Context,
			)
		}
	}
	if m.store != nil {
		if m.userID == "" {
			return fmt.Errorf("agent.Memory.SaveUserFile: userID required")
		}
		return m.store.SaveWorkspaceFile(m.ctx(), m.agentID, m.userID, "USER.md", []byte(content))
	}
	os.MkdirAll(m.workspace, 0o755)
	return os.WriteFile(filepath.Join(m.workspace, "USER.md"), []byte(content), 0o644)
}

// LoadUserFile reads the USER.md file for this Memory's user. Same
// rationale as LoadMemory: USER.md is per-chatter (the visitor's
// profile, not the agent owner's), so we read it via the Exact path
// that bypasses the SQL owner-fallback overlay, and skip the on-disk
// fallback when a store is configured to avoid leaking the owner's
// workspace copy to a chatter without their own row.
func (m *Memory) LoadUserFile() string {
	if m.store != nil {
		if m.userID == "" {
			return ""
		}
		data, err := m.store.GetWorkspaceFileExact(m.ctx(), m.agentID, m.userID, "USER.md")
		if err == nil {
			return string(data)
		}
		return ""
	}
	data, err := os.ReadFile(filepath.Join(m.workspace, "USER.md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// AutoPersistMemory uses an LLM to extract facts from recent messages and
// append them to MEMORY.md and USER.md. Called every N turns.
func AutoPersistMemory(ctx context.Context, mem *Memory, prov provider.Provider, model string, messages []provider.Message) {
	// Build a summary of recent messages for the LLM
	var sb strings.Builder
	// Only look at last 20 messages to keep prompt small
	start := 0
	if len(messages) > 20 {
		start = len(messages) - 20
	}
	for _, m := range messages[start:] {
		if m.Role == "system" {
			continue
		}
		content := m.Content
		if len(content) > 300 {
			content = content[:300] + "..."
		}
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, content))
	}

	currentMemory := mem.LoadMemory()
	currentUser := mem.LoadUserFile()

	extractPrompt := fmt.Sprintf(`Analyze this conversation and extract:
1. Key facts, decisions, or learnings worth remembering (for MEMORY.md)
2. User preferences, profile details, or work style notes (for USER.md)

Current MEMORY.md:
%s

Current USER.md:
%s

Recent conversation:
%s

Output JSON only (no markdown fences):
{"memory_facts": ["fact1", "fact2"], "user_notes": ["note1"]}
If nothing worth saving, output: {"memory_facts": [], "user_notes": []}`,
		truncateStr(currentMemory, 500),
		truncateStr(currentUser, 500),
		sb.String(),
	)

	resp, err := prov.Chat(provider.WithNoThinking(ctx), []provider.Message{
		{Role: "user", Content: extractPrompt},
	}, nil, model, 200, 0.3)
	if err != nil {
		// Warn (not Debug) — invisible failures here are exactly
		// the "I turned the switch on but nothing got persisted"
		// experience that's painful to debug after the fact.
		slog.Warn("auto-persist: LLM call failed", "error", err, "model", model)
		return
	}

	var result struct {
		MemoryFacts []string `json:"memory_facts"`
		UserNotes   []string `json:"user_notes"`
	}
	// Strip markdown code fences before parsing — many tuned models
	// (Sonnet 4.x, Opus, …) reflexively wrap structured output in
	// ```json … ``` even when the prompt asks for "no markdown fences".
	cleaned := stripJSONFence(resp.Content)
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		// Same Warn upgrade as above — silent skip here was hiding
		// "Sonnet returned wrapped JSON, parse failed" in the wild.
		preview := cleaned
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		slog.Warn("auto-persist: failed to parse LLM response",
			"error", err, "model", model, "preview", preview)
		return
	}
	slog.Info("auto-persist: extracted",
		"model", model,
		"memory_facts", len(result.MemoryFacts),
		"user_notes", len(result.UserNotes))

	// Append new memory facts
	if len(result.MemoryFacts) > 0 {
		var memSB strings.Builder
		memSB.WriteString(currentMemory)
		if currentMemory != "" && !strings.HasSuffix(currentMemory, "\n") {
			memSB.WriteString("\n")
		}
		memSB.WriteString(fmt.Sprintf("\n## Auto-persisted: %s\n", time.Now().Format("2006-01-02 15:04")))
		for _, fact := range result.MemoryFacts {
			memSB.WriteString(fmt.Sprintf("- %s\n", fact))
		}
		if err := mem.SaveMemoryWithScan(memSB.String()); err != nil {
			slog.Warn("auto-persist: failed to save MEMORY.md", "error", err)
		} else {
			slog.Info("auto-persist: updated MEMORY.md", "facts", len(result.MemoryFacts))
		}
	}

	// Append user notes
	if len(result.UserNotes) > 0 {
		var userSB strings.Builder
		userSB.WriteString(currentUser)
		if currentUser != "" && !strings.HasSuffix(currentUser, "\n") {
			userSB.WriteString("\n")
		}
		userSB.WriteString(fmt.Sprintf("\n## Auto-persisted: %s\n", time.Now().Format("2006-01-02 15:04")))
		for _, note := range result.UserNotes {
			userSB.WriteString(fmt.Sprintf("- %s\n", note))
		}
		if err := mem.SaveUserFile(userSB.String()); err != nil {
			slog.Warn("auto-persist: failed to save USER.md", "error", err)
		} else {
			slog.Info("auto-persist: updated USER.md", "notes", len(result.UserNotes))
		}
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// stripJSONFence removes a leading ```json (or ```) / trailing ```
// wrapper from an LLM response. Tuned chat models routinely wrap
// structured output even when the prompt asks for raw JSON. Returns
// the original (trimmed) string when no fence is present.
func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence (```json\n or ```\n) — anything up to the
	// first newline after the leading backticks.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// tryAutoTitle fires the auto-title summariser when the current session
// has enough user turns. Extracted so both the success path
// (runPostTurn) and the empty-response bail can call it — the title
// only needs the opening turns, so a failed turn (e.g. context too
// long → empty response) shouldn't block naming the session.
func (a *Agent) tryAutoTitle(ctx context.Context, messages []provider.Message) {
	if !a.memoryCfg.AutoTitle.Enabled || a.memoryCfg.AutoTitle.AfterRounds <= 0 {
		return
	}
	sessionUserTurns := 0
	for _, m := range messages {
		if m.Role == "user" {
			sessionUserTurns++
		}
	}
	afterRounds := a.memoryCfg.AutoTitle.AfterRounds
	willFire := sessionUserTurns >= afterRounds
	slog.Info("auto-title gate",
		"agent", a.name,
		"session_user_turns", sessionUserTurns,
		"after_rounds", afterRounds,
		"will_fire", willFire)
	if !willFire {
		return
	}
	sessionKey := a.registry.GoalSessionKey()
	if sessionKey == "" || a.provider == nil {
		return
	}
	var hub *EventHub
	if stream := streamFromContext(ctx); stream != nil {
		hub = stream.hub
	}
	go a.maybeAutoTitle(sessionKey, messages, hub, a.ownerUserID)
}

// maybeAutoTitle asks the LLM to summarise the opening turns of a
// session into a short title and writes it to sessions.title. Best-
// effort background work — every error path logs at debug/warn and
// returns, never breaking the chat flow. Skips any session whose title
// is already non-empty (user renamed OR a previous run landed).
func (a *Agent) maybeAutoTitle(sessionKey string, messages []provider.Message, hub *EventHub, ownerUserID string) {
	if a.dataStore == nil || ownerUserID == "" {
		return
	}
	ctx := context.Background()

	// Skip if already titled — user renamed or a previous run landed.
	if title, _ := a.dataStore.LookupSessionTitle(ctx, ownerUserID, a.name, sessionKey); strings.TrimSpace(title) != "" {
		return
	}

	// Build a compact transcript for the summariser. Only the first few
	// turns — a title never needs the whole conversation — and only the
	// text part of each message (images / tool I/O are noise for it).
	const maxMessages = 10 // ~5 user/assistant turns
	var sb strings.Builder
	count := 0
	for _, m := range messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		text := strings.TrimSpace(m.TextContent())
		if text == "" {
			continue
		}
		// Truncate long messages so the prompt stays cheap.
		if len(text) > 600 {
			text = text[:600] + "…"
		}
		who := "User"
		if m.Role == "assistant" {
			who = "Assistant"
		}
		sb.WriteString(who)
		sb.WriteString(": ")
		sb.WriteString(text)
		sb.WriteString("\n")
		count++
		if count >= maxMessages {
			break
		}
	}
	if count == 0 {
		return
	}

	model := a.memoryCfg.AutoTitle.Model
	if model == "" {
		model = a.model
	}
	maxChars := a.memoryCfg.AutoTitle.MaxChars
	if maxChars == 0 {
		maxChars = 30
	}

	prompt := fmt.Sprintf(
		"Summarize the following conversation in a single short title. "+
			"Constraints: at most %d characters, no quotes, no trailing period, "+
			"no emoji. Respond with the title and nothing else.\n\n%s",
		maxChars, sb.String())
	resp, err := a.provider.Chat(provider.WithNoThinking(ctx), []provider.Message{
		{Role: "user", Content: prompt},
	}, nil, model, 4096, 0.3)
	if err != nil {
		slog.Warn("auto-title: LLM call failed", "agent", a.name, "session", sessionKey, "model", model, "error", err)
		return
	}
	title := cleanAutoTitle(resp.Content, maxChars)
	if title == "" {
		return
	}
	if err := a.dataStore.RenameSession(ctx, ownerUserID, a.name, sessionKey, title); err != nil {
		slog.Warn("auto-title: rename failed", "agent", a.name, "session", sessionKey, "error", err)
		return
	}
	slog.Info("auto-title: wrote", "agent", a.name, "session", sessionKey, "title", title)

	// Push a live event so subscribed chat panels update the sidebar /
	// header title without a manual refresh. Persist first so the seq
	// lets reconnecting clients dedup.
	if hub != nil {
		evt := ChatEvent{
			Type: "session_title",
			Data: map[string]any{
				"sessionKey": sessionKey,
				"title":      title,
			},
		}
		var seq int64 = -1
		blob, _ := json.Marshal(evt.Data)
		if s, err := a.dataStore.AppendSessionEvent(ctx, ownerUserID, a.name, sessionKey, evt.Type, blob); err == nil {
			seq = s
		} else {
			slog.Debug("auto-title: persist event failed", "error", err)
		}
		hub.Publish(ownerUserID, a.name, sessionKey, EventEnvelope{Seq: seq, Event: evt})
	}
}

// cleanAutoTitle strips the quotes / backticks the model tends to wrap
// around the title, collapses newlines, and truncates to maxChars on a
// rune boundary.
func cleanAutoTitle(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'`")
	s = strings.TrimSpace(s)
	// Collapse newlines + tabs to spaces — the title is one line.
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	if maxChars > 0 {
		runes := []rune(s)
		if len(runes) > maxChars {
			s = string(runes[:maxChars])
		}
	}
	return strings.TrimSpace(s)
}
