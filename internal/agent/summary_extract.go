package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/llmjson"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/session"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// seqSegment is one [start,end] seq range a topic covers. Mirrors the
// JSON shape the LLM emits so it unmarshals directly.
type seqSegment struct {
	S int `json:"s"`
	E int `json:"e"`
}

// ExtractedTopic is one topic the LLM distilled from a conversation
// window. A window may yield several topics (interleaved conversations),
// each with its own summary and the disjoint seq ranges it actually
// covers — so a future fetch_messages retrieves only the messages
// belonging to that topic, not the whole interleaved window.
type ExtractedTopic struct {
	Topic      string       `json:"topic"`
	Summary    string       `json:"summary"`
	Keywords   []string     `json:"keywords"`
	Importance int          `json:"importance"`
	Kind       string       `json:"kind"`       // "durable" (stable fact/preference/decision) | "episodic" (one-off session event)
	Supersedes []int64      `json:"supersedes"` // ids from RECENT MEMORIES this topic replaces (state flips: installed→removed)
	Segments   []seqSegment `json:"segments"`
}

// ExtractedShelved is one open loop the extractor noticed in the window:
// a deferred topic, a promise, or an unanswered question. It is stored in
// shelved_candidates (NOT conversation_summaries) so it never pollutes
// recall. Seq anchors the user's line in session_messages.
type ExtractedShelved struct {
	Kind         string `json:"kind"`
	Topic        string `json:"topic"`
	Context      string `json:"context"`
	Reason       string `json:"reason"`
	Seq          int    `json:"seq"`
	ActionPrompt string `json:"action_prompt"`
}

// shelvedExtract is the Pass-3-style settlement section of a summary
// extraction: new open loops from this window plus the ids of inherited
// candidates this window actually advanced.
type shelvedExtract struct {
	New      []ExtractedShelved `json:"new"`
	Resolved []int64            `json:"resolved"`
}

// extractOutcome is the full LLM answer: topics for recall plus the
// independent shelved settlement. ShelvedInvalid marks a section that was
// present but malformed — the caller queues a shelved-only retry because
// the topic summary itself succeeded and the window will not be re-read.
type extractOutcome struct {
	Topics         []ExtractedTopic
	Shelved        shelvedExtract
	ShelvedInvalid bool
}

// extractConversationTopics calls the LLM to split a message window into
// one or more topics, each annotated with the seq ranges it covers.
// Returns nil when the model says nothing is worth saving. Errors only
// on network/parse failure. Used for the FULL extraction path (session
// never summarized before).
func extractConversationTopics(
	ctx context.Context,
	prov provider.Provider,
	model string,
	recent []store.ConversationSummary,
	open []store.ShelvedCandidate,
	shelvedOn bool,
	messages []store.SessionMessage,
) (extractOutcome, error) {
	out, err := callExtractTopics(ctx, prov, model, messages, false, nil, recent, open, shelvedOn)
	if err != nil {
		return extractOutcome{}, err
	}
	out.Topics = validateTopics(out.Topics, messages, nil)
	return out, nil
}

// mergeConversationTopics is the INCREMENTAL extraction path. Given the
// session's existing topic rows + only the NEW messages since the last
// summary, the LLM returns the full updated topic list — continuing
// existing topics (appending new seq segments) and adding new ones.
func mergeConversationTopics(
	ctx context.Context,
	prov provider.Provider,
	model string,
	existing []store.ConversationSummary,
	recent []store.ConversationSummary,
	open []store.ShelvedCandidate,
	shelvedOn bool,
	messages []store.SessionMessage,
) (extractOutcome, error) {
	out, err := callExtractTopics(ctx, prov, model, messages, true, existing, recent, open, shelvedOn)
	if err != nil {
		return extractOutcome{}, err
	}
	out.Topics = validateTopics(out.Topics, messages, existing)
	return out, nil
}

func callExtractTopics(
	ctx context.Context,
	prov provider.Provider,
	model string,
	messages []store.SessionMessage,
	incremental bool,
	existing []store.ConversationSummary,
	recent []store.ConversationSummary,
	open []store.ShelvedCandidate,
	shelvedOn bool,
) (extractOutcome, error) {
	if len(messages) == 0 {
		return extractOutcome{}, nil
	}

	transcript := renderTranscript(messages)
	if transcript == "" {
		return extractOutcome{}, nil
	}

	var prompt string
	if incremental {
		prompt = buildIncrementalPrompt(existing, recent, renderOpenShelved(open), transcript, shelvedOn)
	} else {
		prompt = buildFullPrompt(recent, renderOpenShelved(open), transcript, shelvedOn)
	}

	// 8192 fits the multi-topic JSON a large window distills, including
	// Chinese summaries — a 96-message session produced 6+ topics that
	// overflowed 4096 mid-character and failed to parse.
	content, err := callJSON(ctx, prov, model, prompt, 8192)
	if err != nil {
		return extractOutcome{}, fmt.Errorf("extract topics LLM call: %w", err)
	}
	if content == "" {
		// Some models (e.g. deepseek-v4-flash) accept JSON-mode
		// response_format without error but emit an empty body — the
		// upstream call succeeds (no error) yet there is nothing to
		// parse. Treat it as "nothing extractable this batch" rather
		// than a parse error: returning nil advances last_summarized_seq
		// so the idle sweep won't keep re-trying the same window, and
		// avoids a warning storm. The summary is simply stored without
		// fresh topics for this run.
		slog.Debug("conversation summary: empty LLM body, skipping topics",
			"model", model)
		return extractOutcome{}, nil
	}
	return parseTopicsBody(content)
}

// renderTranscript formats messages for an extraction prompt: one
// [seq=N role=...] line each, skipping system rows, injected origins and
// truncating long content. Shared by the summary and retry paths so both
// see the same window.
func renderTranscript(messages []store.SessionMessage) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Role == "system" || m.Origin != "" {
			continue
		}
		content := m.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		fmt.Fprintf(&b, "[seq=%d role=%s] %s\n", m.Seq, m.Role, content)
	}
	return b.String()
}

// callJSON runs one JSON-mode extraction call with thinking disabled and
// a bounded timeout. Returns "" for an empty body (some models accept
// response_format but answer nothing). Fence stripping is left to
// llmjson.UnmarshalLLM in the parser.
func callJSON(ctx context.Context, prov provider.Provider, model, prompt string, maxTokens int) (string, error) {
	c, cancel := context.WithTimeout(provider.WithJSONMode(provider.WithNoThinking(ctx)), 150*time.Second)
	defer cancel()
	resp, err := prov.Chat(c, []provider.Message{{Role: "user", Content: prompt}}, nil, model, maxTokens, 0.3)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// parseTopicsBody decodes the LLM's topics output via the shared llmjson
// entry point — the prompt (and JSON mode) asks for
// {"topics":[...],"shelved":{...}}, but models have been observed
// dropping the wrapper (bare array) and/or emitting bare quotes inside
// summaries; llmjson.UnmarshalLLM tolerates both plus fences, so a drift
// doesn't fail the same window on every idle sweep.
//
// The shelved section is decoded separately from raw message bytes: a
// malformed shelved block degrades to no settlement instead of taking the
// topics down with it. The top-level struct keeps exactly one slice field
// (Topics) so the bare-array fallback still recognizes it.
func parseTopicsBody(content string) (extractOutcome, error) {
	var parsed struct {
		Topics  []ExtractedTopic `json:"topics"`
		Shelved any              `json:"shelved"`
	}
	if err := llmjson.UnmarshalLLM(content, &parsed); err != nil {
		return extractOutcome{}, fmt.Errorf("parse topics JSON: %w (raw=%q)", err, content)
	}
	out := extractOutcome{Topics: parsed.Topics}
	if parsed.Shelved != nil {
		m, ok := parsed.Shelved.(map[string]any)
		if !ok {
			out.ShelvedInvalid = true
			slog.Warn("conversation summary: shelved section is not an object")
		} else if b, merr := json.Marshal(m); merr != nil {
			out.ShelvedInvalid = true
		} else if uerr := json.Unmarshal(b, &out.Shelved); uerr != nil {
			out.ShelvedInvalid = true
			slog.Warn("conversation summary: shelved section unparseable", "error", uerr)
		}
	}
	return out, nil
}

// renderOpenShelved formats the inherited open candidates for the prompt:
// id + topic + since only, capped at 20 (the list the model may resolve).
func renderOpenShelved(open []store.ShelvedCandidate) string {
	type row struct {
		ID    int64  `json:"id"`
		Topic string `json:"topic"`
		Since string `json:"since"`
	}
	rows := make([]row, 0, len(open))
	for _, c := range open {
		rows = append(rows, row{ID: c.ID, Topic: c.Topic, Since: c.Since})
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func seqRangeOf(messages []store.SessionMessage) (int, int) {
	if len(messages) == 0 {
		return 0, 0
	}
	mn, mx := messages[0].Seq, messages[0].Seq
	for _, m := range messages[1:] {
		if m.Seq < mn {
			mn = m.Seq
		}
		if m.Seq > mx {
			mx = m.Seq
		}
	}
	return mn, mx
}

func validateTopics(parsed []ExtractedTopic, messages []store.SessionMessage, existing []store.ConversationSummary) []ExtractedTopic {
	mn, mx := seqRangeOf(messages)
	existingSegSet := map[[2]int]bool{}
	for _, e := range existing {
		for _, s := range e.Segments {
			existingSegSet[[2]int{s[0], s[1]}] = true
		}
	}
	var cleaned []ExtractedTopic
	for _, t := range parsed {
		if strings.TrimSpace(t.Topic) == "" || strings.TrimSpace(t.Summary) == "" {
			continue
		}
		var valid []seqSegment
		for _, seg := range t.Segments {
			if seg.S > seg.E {
				seg.S, seg.E = seg.E, seg.S
			}
			inWindow := seg.S >= mn && seg.E <= mx
			if inWindow || existingSegSet[[2]int{seg.S, seg.E}] {
				valid = append(valid, seg)
			}
		}
		if len(valid) == 0 {
			continue
		}
		t.Segments = valid
		if t.Importance < 1 {
			t.Importance = 3
		}
		if t.Importance > 5 {
			t.Importance = 5
		}
		if t.Keywords == nil {
			t.Keywords = []string{}
		}
		cleaned = append(cleaned, t)
	}
	return cleaned
}

// renderRecentMemories formats the agent's recent cross-session memories
// for the extraction prompt: id + topic + truncated summary, enough for
// the LLM to (a) skip re-storing what's already remembered and (b) mark a
// new topic as superseding a now-outdated memory.
func renderRecentMemories(recent []store.ConversationSummary) string {
	if len(recent) == 0 {
		return "[]"
	}
	type row struct {
		ID      int64  `json:"id"`
		Topic   string `json:"topic"`
		Summary string `json:"summary"`
	}
	rows := make([]row, 0, len(recent))
	for _, r := range recent {
		rows = append(rows, row{ID: r.ID, Topic: r.Topic, Summary: truncateStr(r.Summary, 160)})
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func buildFullPrompt(recent []store.ConversationSummary, openJSON, transcript string, shelvedOn bool) string {
	shelvedBlock, shelvedSchema, emptyHint := shelvedPromptParts(shelvedOn, openJSON)
	return fmt.Sprintf(`Analyze this conversation excerpt. Each line is tagged with its seq number and role.

CRITICAL — OUTPUT LANGUAGE:
The summary and keywords MUST be in the same language as the conversation.
- Chinese conversation → Chinese summary + Chinese keywords
- English conversation → English summary + English keywords
- Mixed → use the language the user typed in
This is load-bearing: a Chinese-speaking user can only search in Chinese. A summary in the wrong language will NEVER be found by later queries.

WORTH-REMEMBERING GATE (skip before summarizing):
Emit NO topic for one-off task completions and ephemeral exchanges — generating an image on request, testing a feature/channel, asking who you are or what tools exist, installing/listing things that were immediately undone, transient status checks. A topic must be something that would change how a FUTURE conversation behaves. When in doubt, skip it — the corpus must stay small to stay useful.

Group the excerpt into TOPICS. Real conversations interleave several topics (e.g. weather small-talk wedged between health-advice threads). Each topic becomes its own recallable summary, scoped to ONLY the seq ranges where it was actually discussed — so future retrieval fetches the verbatim messages of that topic without unrelated turns.

For each topic emit:
- topic: short label (<=8 words) in the conversation's language
- summary: 1-2 sentences in the conversation's language
- keywords: 3-7 keywords in the conversation's language
- importance: 1-5 (usefulness to a FUTURE conversation; 1=trivial, 5=key fact/decision/preference)
- kind: "durable" for stable facts, preferences, decisions, user/family profile, long-term projects and setups; "episodic" for session-specific events and state that may go stale
- supersedes: array of ids from RECENT MEMORIES below that this topic directly REPLACES (a state flipped: something installed then removed, a plan cancelled or changed, a preference overridden). Use ONLY ids from that list. Empty array otherwise. When in doubt, empty.
- segments: list of {"s":N,"e":N} pairs. s and e are seq numbers SHOWN IN THE TRANSCRIPT that belong to this topic. A topic spanning disjoint ranges gets several pairs. Use the exact seq numbers from the transcript; do not invent numbers not present.

Skip greetings, small talk, chit-chat, unresolved errors — emit NO topic for them.

%s

RECENT MEMORIES from other sessions (JSON; for dedup + supersedes):
%s

Output STRICT JSON only — no markdown fences, no commentary:
{"topics":[{"topic":"...","summary":"...","keywords":[...],"importance":N,"kind":"durable|episodic","supersedes":[],"segments":[{"s":N,"e":N}]}]%s}

If nothing is worth remembering: %s

Conversation:
%s`, shelvedBlock, renderRecentMemories(recent), shelvedSchema, emptyHint, transcript)
}

// shelvedJSONContract is the settlement schema shared by the full,
// incremental and retry prompts — a field change edits one string.
const shelvedJSONContract = `"shelved":{"new":[{"kind":"deferred|commitment|awaiting_user","topic":"...","context":"...","reason":"...","seq":N,"action_prompt":"..."}],"resolved":[N]}`

// shelvedPromptParts returns the three prompt fragments that gate shelved
// extraction: the instruction block, the schema suffix and the empty-result
// example. Disabled agents get none of them, so their extraction prompt is
// byte-identical to the pre-feature one.
func shelvedPromptParts(enabled bool, openJSON string) (block, schema, emptyHint string) {
	if !enabled {
		return "", "", `{"topics":[]}`
	}
	return shelvedPromptBlock(openJSON), "," + shelvedJSONContract,
		`{"topics":[],"shelved":{"new":[],"resolved":[]}}`
}

// shelvedPromptBlock is the shared open-loops instruction + the inherited
// candidate list, appended to both extraction prompts. Topics and shelved
// are explicitly separate: the topic gate skips one-off/unresolved chatter,
// while shelved exists precisely to catch what is unfinished.
func shelvedPromptBlock(openJSON string) string {
	return fmt.Sprintf(`SHELVED LEDGER (separate from topics — never merge them):
A shelved item is an open loop, not knowledge: the user said they would come back to something (deferred), made a promise to do something (commitment), or you asked a question and they moved on (awaiting_user). Topics describe what was discussed; shelved tracks what is still unfinished.
- new[]: open loops created in THIS window only. 0-5 items, be strict; empty array when nothing qualifies.
  kind: "deferred" | "commitment" | "awaiting_user"
  topic: <=30 characters, in the conversation's language
  context: one sentence of the situation
  reason: why it is unfinished
  seq: the user's line's seq as shown in the transcript
  action_prompt: one imperative sentence to run if the user accepts
- resolved[]: ids from OPEN SHELVED below that this window actually advanced or completed. Mentioning is not resolving.
- An item already in OPEN SHELVED must NOT be repeated in new[].

OPEN SHELVED (JSON; ids are the only valid resolved values):
%s`, openJSON)
}

func buildIncrementalPrompt(existing []store.ConversationSummary, recent []store.ConversationSummary, openJSON, transcript string, shelvedOn bool) string {
	shelvedBlock, shelvedSchema, _ := shelvedPromptParts(shelvedOn, openJSON)
	tail := ""
	if shelvedOn {
		tail = " with empty shelved arrays"
	}
	type seg struct {
		S int `json:"s"`
		E int `json:"e"`
	}
	type existingTopic struct {
		Topic      string   `json:"topic"`
		Summary    string   `json:"summary"`
		Keywords   []string `json:"keywords"`
		Importance int      `json:"importance"`
		Segments   []seg    `json:"segments"`
	}
	old := make([]existingTopic, 0, len(existing))
	for _, e := range existing {
		segs := make([]seg, 0, len(e.Segments))
		for _, s := range e.Segments {
			segs = append(segs, seg{S: s[0], E: s[1]})
		}
		old = append(old, existingTopic{
			Topic: e.Topic, Summary: e.Summary, Keywords: e.Keywords,
			Importance: e.Importance, Segments: segs,
		})
	}
	existingJSON, _ := json.Marshal(old)

	return fmt.Sprintf(`You are maintaining a conversation's topic index. Below are the EXISTING topics already summarized for this session, plus NEW messages that arrived since the last summary.

CRITICAL — OUTPUT LANGUAGE: summary and keywords MUST match the conversation's language (Chinese→Chinese, English→English, mixed→the user's language). A summary in the wrong language will NEVER be found by later queries.

Your job: output the FULL updated topic list.
- Topics that CONTINUE in the new messages: keep them, refresh the summary to cover both old and new content, APPEND the new seq segments to the existing segments list (do NOT drop the old ones).
- Brand-new topics in the new messages: add them with their own segments.
- Topics NOT touched by the new messages: carry them over UNCHANGED (same summary, same segments, same importance).
- Skip greetings/chit-chat/unresolved errors — emit no topic for them. Also skip one-off task completions (image generation on request, feature tests, identity questions): a topic must change how a FUTURE conversation behaves.
- kind: "durable" for stable facts/preferences/decisions/profile; "episodic" for session-specific events and state that may go stale.
- supersedes: if a new topic directly REPLACES one of the RECENT MEMORIES below (state flipped: installed then removed, plan changed), put that memory's id in its supersedes array. Only ids from that list; empty otherwise.

EXISTING TOPICS (JSON; segments are [seq_start, seq_end] pairs already covered):
%s

RECENT MEMORIES from other sessions (JSON; for supersedes):
%s

%s

NEW MESSAGES (each tagged with seq and role; use the exact seq numbers shown):
%s

Rules:
- Every segment from EXISTING topics that you carry over MUST reappear unchanged in the output.
- New segments must use seq numbers FROM THE NEW MESSAGES transcript only.
- Do not invent seq numbers not present in either source.

Output STRICT JSON only — no markdown fences:
{"topics":[{"topic":"...","summary":"...","keywords":[...],"importance":N,"kind":"durable|episodic","supersedes":[],"segments":[{"s":N,"e":N}]}]%s}

If the new messages add nothing worth remembering, return the existing topics unchanged%s.`,
		string(existingJSON), renderRecentMemories(recent), shelvedBlock, transcript, shelvedSchema, tail)
}

// summarizeIdleSessions scans this agent's sessions that have been
// quiet for at least idleAfter and have at least minMessages messages,
// and runs persistConversationSummary on each (incremental when the
// session was summarized before). It's the background safety net for
// conversations the user ended by walking away.
//
// Best-effort: per-session errors are logged and the sweep moves on.
func (a *Agent) summarizeIdleSessions(ctx context.Context, idleAfter time.Duration, minMessages int) {
	db, ok := a.dataStore.(*store.DBStore)
	if !ok || a.provider == nil {
		return
	}
	cutoff := time.Now().Add(-idleAfter)
	sessions, err := db.ListIdleSessions(ctx, a.agentID, cutoff, minMessages)
	if err != nil {
		slog.Warn("idle summary: list sessions failed",
			"agent", a.agentID, "error", err)
		return
	}
	for _, s := range sessions {
		if ctx.Err() != nil {
			return
		}
		if !s.UpdatedAt.Before(cutoff) {
			continue
		}
		slog.Info("idle summary: summarizing quiet session",
			"agent", a.agentID, "session", s.SessionKey,
			"messages", s.MessageCount, "idle_for", time.Since(s.UpdatedAt).Round(time.Minute))
		persistConversationSummary(ctx, db, a.bgProvider(), a.summaryModelFor(), a.embedder,
			a.ownerUserID, a.agentID, s.SessionKey)
	}
}

func (a *Agent) summaryModelFor() string {
	if a.summaryModel != "" {
		return a.summaryModel
	}
	return a.model
}

// persistConversationSummary loads a session's messages from the store
// (each carrying its true DB seq), runs full or incremental extraction,
// and writes the topic rows. Triggered by /compact, new-session, and
// the idle-session sweep.
//
// Incremental: when last_summarized_seq > 0, only messages with seq >
// that value are fed to the LLM, alongside the existing topic list for
// merge. Old messages are never re-fed. On success the session's rows
// are replaced with the merged set and last_summarized_seq is advanced.
// Best-effort: logs errors but never propagates them — summary failures
// must not crash the main conversation flow.
func persistConversationSummary(
	ctx context.Context,
	db *store.DBStore,
	prov provider.Provider,
	model string,
	emb embedding.Embedder,
	userID, agentID, sessionKey string,
) {
	if db == nil || prov == nil {
		return
	}
	allMsgs, err := db.ListSessionMessages(ctx, agentID, sessionKey)
	if err != nil {
		slog.Warn("conversation summary: load messages failed",
			"agent", agentID, "session", sessionKey, "error", err)
		return
	}
	// Drop LLM-hidden rows (regex-hook turns with FeedToLLM=false) so the
	// summarizer never sees exchanges the LLM itself never saw. ListSessionMessages
	// is shared with the web history UI (which must show them), so the filter
	// lives here at the LLM-side caller, not in the query.
	visible := allMsgs[:0]
	for i := range allMsgs {
		if allMsgs[i].LLMVisible {
			visible = append(visible, allMsgs[i])
		}
	}
	allMsgs = visible
	if len(allMsgs) < 2 {
		return
	}
	// Shelved extraction is opt-in: agents that never enabled the diary
	// ledger get the pre-feature prompt (no section, no schema, no writes).
	shelvedOn := false
	if fc, ok := config.AgentFileConfigLoader(agentID, ""); ok && fc.Diary != nil {
		shelvedOn = fc.Diary.ShelvedEnabled
	}
	maxSeq := allMsgs[len(allMsgs)-1].Seq

	lastSeq := 0
	if rec, rerr := db.GetSession(ctx, agentID, sessionKey); rerr == nil && rec != nil {
		lastSeq = rec.LastSummarizedSeq
	}

	var (
		outcome     extractOutcome
		extractErr  error
		incremental bool
		fedMsgs     []store.SessionMessage
	)
	// Recent cross-session memories: shown to the LLM for dedup + the
	// supersedes chain (state flips like install→remove). Best-effort —
	// extraction proceeds without it when the lookup fails. Capped at 15:
	// each entry costs ~160B of summary in BOTH the full and incremental
	// prompt, and an out-of-date memory the LLM should supersede is nearly
	// always among the newest ones — rows 16-40 almost never earn their
	// per-extraction token tax.
	recent, rerr := db.ListRecentSummariesForSupersede(ctx, agentID, sessionKey, 15)
	if rerr != nil {
		slog.Debug("conversation summary: recent memories lookup failed",
			"agent", agentID, "error", rerr)
		recent = nil
	}
	recentIDs := make(map[int64]bool, len(recent))
	for _, r := range recent {
		recentIDs[r.ID] = true
	}
	// Open shelved candidates across every session: handed to the extractor
	// so this window can settle one that was picked back up. Skipped
	// entirely for agents that did not opt in.
	var open []store.ShelvedCandidate
	if shelvedOn {
		var oerr error
		open, oerr = db.ListOpenShelvedCandidates(ctx, agentID, 20)
		if oerr != nil {
			slog.Debug("conversation summary: open shelved lookup failed",
				"agent", agentID, "error", oerr)
			open = nil
		}
	}
	if lastSeq == 0 {
		fedMsgs = allMsgs
		outcome, extractErr = extractConversationTopics(ctx, prov, model, recent, open, shelvedOn, allMsgs)
	} else {
		incremental = true
		newMsgs := messagesAfterSeq(allMsgs, lastSeq)
		if len(newMsgs) == 0 {
			slog.Debug("conversation summary: no new messages since last summary",
				"agent", agentID, "session", sessionKey, "last_seq", lastSeq)
			return
		}
		fedMsgs = newMsgs
		existing, lerr := db.ListConversationSummariesBySession(ctx, agentID, sessionKey)
		if lerr != nil {
			slog.Warn("conversation summary: list existing failed, falling back to full",
				"agent", agentID, "session", sessionKey, "error", lerr)
			fedMsgs = allMsgs
			outcome, extractErr = extractConversationTopics(ctx, prov, model, recent, open, shelvedOn, allMsgs)
		} else {
			outcome, extractErr = mergeConversationTopics(ctx, prov, model, existing, recent, open, shelvedOn, newMsgs)
		}
	}
	if extractErr != nil {
		slog.Warn("conversation summary extract failed",
			"agent", agentID, "session", sessionKey, "error", extractErr)
		return
	}

	// Shelved settlement is independent of the topic gate: write it even
	// when the window produced no topics (the "nothing worth remembering"
	// early return below must not drop a fresh open loop).
	if shelvedOn {
		applyShelvedExtract(ctx, db, agentID, sessionKey, fedMsgs, open, outcome.Shelved, model)
	}
	if shelvedOn && outcome.ShelvedInvalid && len(fedMsgs) > 0 {
		seqStart, seqEnd := seqRangeOf(fedMsgs)
		if qerr := db.EnqueueShelvedExtractRetry(ctx, agentID, sessionKey, seqStart, seqEnd, "malformed shelved section"); qerr != nil {
			slog.Warn("shelved retry: enqueue failed",
				"agent", agentID, "session", sessionKey, "error", qerr)
		} else {
			slog.Warn("shelved retry: queued window",
				"agent", agentID, "session", sessionKey, "seq_start", seqStart, "seq_end", seqEnd)
		}
	}
	// Queued retries are drained by the gateway sweep (drainShelvedRetries),
	// not here — a session that never summarizes again must still recover.

	topics := outcome.Topics
	if len(topics) == 0 {
		// Nothing worth saving, but advance last_summarized_seq anyway so
		// the idle sweep stops re-scanning this session every tick — the
		// LLM already decided everything up to maxSeq is forgettable.
		if serr := db.SetSessionLastSummarizedSeq(ctx, agentID, sessionKey, maxSeq); serr != nil {
			slog.Warn("conversation summary: advance last_summarized_seq (empty) failed",
				"agent", agentID, "session", sessionKey, "error", serr)
		}
		slog.Debug("conversation summary: nothing to save",
			"agent", agentID, "session", sessionKey, "incremental", incremental)
		return
	}

	if incremental {
		if derr := db.DeleteConversationSummariesBySession(ctx, agentID, sessionKey); derr != nil {
			slog.Warn("conversation summary: delete old failed, aborting incremental",
				"agent", agentID, "session", sessionKey, "error", derr)
			return
		}
	}

	embModel := ""
	if emb != nil && emb.Available() {
		embModel = emb.Model()
	}

	saved := 0
	for _, t := range topics {
		minSeq, tMaxSeq := t.Segments[0].S, t.Segments[0].E
		segs := make([][2]int, 0, len(t.Segments))
		for _, seg := range t.Segments {
			if seg.S < minSeq {
				minSeq = seg.S
			}
			if seg.E > tMaxSeq {
				tMaxSeq = seg.E
			}
			segs = append(segs, [2]int{seg.S, seg.E})
		}
		id, err := db.InsertConversationSummary(ctx, store.ConversationSummary{
			UserID:         userID,
			AgentID:        agentID,
			SessionKey:     sessionKey,
			Topic:          t.Topic,
			Summary:        t.Summary,
			Keywords:       t.Keywords,
			Segments:       segs,
			SeqStart:       minSeq,
			SeqEnd:         tMaxSeq,
			EmbeddingModel: embModel,
			Importance:     t.Importance,
			Kind:           t.Kind,
		})
		if err != nil {
			slog.Warn("conversation summary persist failed",
				"agent", agentID, "session", sessionKey, "topic", t.Topic, "error", err)
			continue
		}
		saved++

		// Supersedes chain: flag each replaced memory (validated against
		// the ids actually shown to the LLM) so contradictory state stops
		// surfacing in recall. Best-effort per id.
		for _, oldID := range t.Supersedes {
			if oldID <= 0 || !recentIDs[oldID] {
				continue
			}
			if serr := db.MarkSummarySuperseded(ctx, agentID, oldID, id); serr != nil {
				slog.Warn("conversation summary: supersede failed",
					"agent", agentID, "old", oldID, "new", id, "error", serr)
			} else {
				slog.Info("conversation summary: memory superseded",
					"agent", agentID, "old", oldID, "new", id, "topic", t.Topic)
			}
		}

		if emb != nil && emb.Available() && id > 0 {
			text := t.Summary
			if len(t.Keywords) > 0 {
				text += " " + strings.Join(t.Keywords, " ")
			}
			vecs, embErr := emb.Embed(ctx, []string{text})
			if embErr != nil {
				slog.Warn("conversation summary embedding failed",
					"agent", agentID, "session", sessionKey, "topic", t.Topic, "error", embErr)
				continue
			}
			if len(vecs) == 1 {
				if err := db.InsertConversationSummaryVector(ctx, id, vecs[0]); err != nil {
					slog.Warn("conversation summary vector insert failed",
						"agent", agentID, "session", sessionKey, "topic", t.Topic, "error", err)
				}
			}
		}
	}

	if serr := db.SetSessionLastSummarizedSeq(ctx, agentID, sessionKey, maxSeq); serr != nil {
		slog.Warn("conversation summary: advance last_summarized_seq failed",
			"agent", agentID, "session", sessionKey, "error", serr)
	}

	slog.Info("conversation summary saved",
		"agent", agentID, "session", sessionKey,
		"max_seq", maxSeq, "topics", saved, "incremental", incremental)
}

// applyShelvedExtract persists one window's shelved settlement: upsert the
// new open loops (anchored to a real user line from this window) and
// resolve inherited ids the window advanced. Best-effort — settlement
// failures never sink the topic summary.
func applyShelvedExtract(ctx context.Context, db *store.DBStore, agentID, sessionKey string,
	fedMsgs []store.SessionMessage, open []store.ShelvedCandidate, parsed shelvedExtract, model string) {
	upserts, resolveIDs := assembleShelvedExtract(agentID, sessionKey, fedMsgs, open, parsed, time.Now(), model)
	if err := db.UpsertShelvedCandidates(ctx, upserts); err != nil {
		slog.Warn("shelved candidate upsert failed",
			"agent", agentID, "session", sessionKey, "count", len(upserts), "error", err)
	}
	if len(resolveIDs) > 0 {
		n, err := db.MarkShelvedCandidatesResolved(ctx, agentID, resolveIDs, "summary")
		if err != nil {
			slog.Warn("shelved candidate resolve failed",
				"agent", agentID, "ids", resolveIDs, "error", err)
		} else if n > 0 {
			slog.Info("shelved candidates resolved", "agent", agentID, "count", n)
		}
	}
}

// assembleShelvedExtract validates the LLM's settlement against the
// window it actually saw — pure so the rules stay unit-testable.
//   - kind must be one of the three open-loop kinds
//   - topic must be non-empty; seq must anchor a user line in fedMsgs
//     (an assistant line is not something the user said they'd return to)
//   - at most 5 new items per window, one per (seq, kind)
//   - resolved ids must exist in the inherited open set
func assembleShelvedExtract(agentID, sessionKey string, fedMsgs []store.SessionMessage,
	open []store.ShelvedCandidate, parsed shelvedExtract, now time.Time, model string) ([]store.ShelvedCandidate, []int64) {
	if len(parsed.New) == 0 && len(parsed.Resolved) == 0 {
		return nil, nil
	}
	openIDs := make(map[int64]bool, len(open))
	for _, c := range open {
		openIDs[c.ID] = true
	}
	userSeqs := make(map[int]bool, len(fedMsgs))
	for _, m := range fedMsgs {
		if m.Role == "user" {
			userSeqs[m.Seq] = true
		}
	}
	date := now.Format("2006-01-02")
	extractor := model + "@v1"
	seen := map[string]bool{}
	var out []store.ShelvedCandidate
	for _, n := range parsed.New {
		if len(out) >= 5 {
			break
		}
		kind := strings.TrimSpace(n.Kind)
		switch kind {
		case "deferred", "commitment", "awaiting_user":
		default:
			continue
		}
		topic := strings.TrimSpace(n.Topic)
		if topic == "" || !userSeqs[n.Seq] {
			continue
		}
		key := fmt.Sprintf("%d|%s", n.Seq, kind)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, store.ShelvedCandidate{
			AgentID:      agentID,
			Kind:         kind,
			Topic:        TruncateRunes(topic, 200),
			Context:      TruncateRunes(strings.TrimSpace(n.Context), 500),
			Reason:       TruncateRunes(strings.TrimSpace(n.Reason), 500),
			SessionKey:   sessionKey,
			Seq:          n.Seq,
			Since:        date,
			LastSeenAt:   now,
			Source:       "summary",
			Extractor:    extractor,
			ActionPrompt: TruncateRunes(strings.TrimSpace(n.ActionPrompt), 500),
		})
	}
	var resolve []int64
	seenID := map[int64]bool{}
	for _, id := range parsed.Resolved {
		if id <= 0 || !openIDs[id] || seenID[id] {
			continue
		}
		seenID[id] = true
		resolve = append(resolve, id)
	}
	return out, resolve
}

// shelvedRetryMaxAttempts caps retries per queued window. After that the
// row is dropped rather than retried forever (a model that keeps emitting
// a malformed section will not fix itself).
const shelvedRetryMaxAttempts = 3

// retryShelvedExtractions re-runs shelved-only extraction for windows
// queued after a malformed section. It is independent of the normal pass:
// the queue row exists because that window's last_summarized_seq already
// advanced, so the normal incremental path will never see it again.
// Best-effort — a failed retry stays queued for the next summary.
func retryShelvedExtractions(ctx context.Context, db *store.DBStore, prov provider.Provider,
	model, agentID, sessionKey string, allMsgs []store.SessionMessage,
	open []store.ShelvedCandidate) {
	retries, err := db.ListShelvedExtractRetries(ctx, agentID, sessionKey, 3)
	if err != nil {
		slog.Warn("shelved retry: list failed",
			"agent", agentID, "session", sessionKey, "error", err)
		return
	}
	openJSON := renderOpenShelved(open)
	for _, r := range retries {
		if r.Attempts >= shelvedRetryMaxAttempts {
			slog.Warn("shelved retry: giving up",
				"agent", agentID, "session", sessionKey,
				"seq_start", r.SeqStart, "seq_end", r.SeqEnd, "attempts", r.Attempts)
			if derr := db.DeleteShelvedExtractRetry(ctx, r.ID); derr != nil {
				slog.Warn("shelved retry: drop failed", "id", r.ID, "error", derr)
			}
			continue
		}
		msgs := messagesBetweenSeq(allMsgs, r.SeqStart, r.SeqEnd)
		if len(msgs) == 0 {
			// The window is gone (messages deleted or rewound).
			if derr := db.DeleteShelvedExtractRetry(ctx, r.ID); derr != nil {
				slog.Warn("shelved retry: drop stale failed", "id", r.ID, "error", derr)
			}
			continue
		}
		parsed, xerr := extractShelvedOnly(ctx, prov, model, msgs, openJSON)
		if xerr != nil {
			slog.Warn("shelved retry failed",
				"agent", agentID, "session", sessionKey,
				"seq_start", r.SeqStart, "seq_end", r.SeqEnd,
				"attempt", r.Attempts+1, "error", xerr)
			if berr := db.BumpShelvedExtractRetry(ctx, r.ID, xerr.Error()); berr != nil {
				slog.Warn("shelved retry: bump failed", "id", r.ID, "error", berr)
			}
			continue
		}
		applyShelvedExtract(ctx, db, agentID, sessionKey, msgs, open, parsed, model)
		if derr := db.DeleteShelvedExtractRetry(ctx, r.ID); derr != nil {
			slog.Warn("shelved retry: delete failed", "id", r.ID, "error", derr)
			continue
		}
		slog.Info("shelved retry recovered",
			"agent", agentID, "session", sessionKey,
			"seq_start", r.SeqStart, "seq_end", r.SeqEnd,
			"new", len(parsed.New), "resolved", len(parsed.Resolved))
	}
}

// drainShelvedRetries runs queued shelved-only retries for this agent,
// grouped by session. The gateway sweep calls it so a session that never
// summarizes again cannot strand its queue.
func (a *Agent) drainShelvedRetries(ctx context.Context) {
	db, ok := a.dataStore.(*store.DBStore)
	if !ok {
		return
	}
	retries, err := db.ListShelvedExtractRetriesByAgent(ctx, a.agentID, 20)
	if err != nil {
		slog.Warn("shelved retry: list failed", "agent", a.agentID, "error", err)
		return
	}
	if len(retries) == 0 {
		return
	}
	// Config is read only when there is work: the loader re-scans the
	// agents table, and the queue is empty on the common sweep.
	if fc, ok := config.AgentFileConfigLoader(a.agentID, ""); !ok || fc.Diary == nil || !fc.Diary.ShelvedEnabled {
		return
	}
	open, oerr := db.ListOpenShelvedCandidates(ctx, a.agentID, 20)
	if oerr != nil {
		open = nil
	}
	seen := map[string]bool{}
	for _, r := range retries {
		if ctx.Err() != nil {
			return
		}
		if seen[r.SessionKey] {
			continue
		}
		seen[r.SessionKey] = true
		msgs, merr := db.ListSessionMessages(ctx, a.agentID, r.SessionKey)
		if merr != nil {
			slog.Warn("shelved retry: load messages failed",
				"agent", a.agentID, "session", r.SessionKey, "error", merr)
			continue
		}
		visible := msgs[:0]
		for i := range msgs {
			if msgs[i].LLMVisible {
				visible = append(visible, msgs[i])
			}
		}
		retryShelvedExtractions(ctx, db, a.bgProvider(), a.summaryModelFor(),
			a.agentID, r.SessionKey, visible, open)
	}
}

// extractShelvedOnly is the retry's reduced call: no topic aggregation,
// just the settlement section over one queued window. A malformed
// section comes back as an error so the caller can bump the retry count.
func extractShelvedOnly(ctx context.Context, prov provider.Provider, model string,
	msgs []store.SessionMessage, openJSON string) (shelvedExtract, error) {
	transcript := renderTranscript(msgs)
	if transcript == "" {
		return shelvedExtract{}, nil
	}
	prompt := fmt.Sprintf(`Only do shelved settlement for the conversation below — no topics.

%s

Conversation:
%s

Output STRICT JSON only — no markdown fences:
{%s}`,
		shelvedPromptBlock(openJSON), transcript, shelvedJSONContract)

	body, err := callJSON(ctx, prov, model, prompt, 1024)
	if err != nil {
		return shelvedExtract{}, err
	}
	if body == "" {
		return shelvedExtract{}, fmt.Errorf("empty retry response")
	}
	out, perr := parseTopicsBody(body)
	if perr != nil {
		return shelvedExtract{}, perr
	}
	if out.ShelvedInvalid {
		return shelvedExtract{}, fmt.Errorf("malformed shelved section on retry")
	}
	return out.Shelved, nil
}

func messagesBetweenSeq(messages []store.SessionMessage, start, end int) []store.SessionMessage {
	var out []store.SessionMessage
	for _, m := range messages {
		if m.Seq >= start && m.Seq <= end {
			out = append(out, m)
		}
	}
	return out
}

func messagesAfterSeq(messages []store.SessionMessage, lastSeq int) []store.SessionMessage {
	var out []store.SessionMessage
	for _, m := range messages {
		if m.Seq > lastSeq {
			out = append(out, m)
		}
	}
	return out
}

// maybeExtractSummary fires a background persistConversationSummary for
// the session. Called by /compact (after compaction) and /new (before
// minting a fresh session) so a topic snapshot of the just-closed range
// enters cross-session recall. Best-effort: logs + returns on any error.
//
// `trigger` is a label for log context ("compaction", "new_session").
func (a *Agent) maybeExtractSummary(sess *session.Session, trigger string) {
	if a.dataStore == nil {
		return
	}
	db, ok := a.dataStore.(*store.DBStore)
	if !ok {
		slog.Debug("summary extraction: store is not DBStore, skipping",
			"agent", a.agentID, "trigger", trigger)
		return
	}

	// Capture variables — the goroutine outlives the calling turn.
	// Messages are loaded inside persistConversationSummary (straight
	// from the store, each carrying its true DB seq) so we don't thread
	// a possibly-stale in-memory snapshot through.
	owner := a.ownerUserID
	agentID := a.agentID
	sessionKey := sess.SessionKey()
	prov := a.bgProvider()
	model := a.summaryModelFor()
	emb := a.embedder

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		slog.Debug("summary extraction: background goroutine started",
			"agent", agentID, "session", sessionKey, "trigger", trigger)
		persistConversationSummary(ctx, db, prov, model, emb,
			owner, agentID, sessionKey)
	}()
}
