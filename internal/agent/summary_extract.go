package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/embedding"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
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
	Segments   []seqSegment `json:"segments"`
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
	messages []store.SessionMessage,
) ([]ExtractedTopic, error) {
	topics, err := callExtractTopics(ctx, prov, model, messages, false, nil)
	if err != nil {
		return nil, err
	}
	return validateTopics(topics, messages, nil), nil
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
	messages []store.SessionMessage,
) ([]ExtractedTopic, error) {
	topics, err := callExtractTopics(ctx, prov, model, messages, true, existing)
	if err != nil {
		return nil, err
	}
	return validateTopics(topics, messages, existing), nil
}

func callExtractTopics(
	ctx context.Context,
	prov provider.Provider,
	model string,
	messages []store.SessionMessage,
	incremental bool,
	existing []store.ConversationSummary,
) ([]ExtractedTopic, error) {
	if len(messages) == 0 {
		return nil, nil
	}

	var transcript strings.Builder
	for _, m := range messages {
		if m.Role == "system" || m.Origin != "" {
			continue
		}
		content := m.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		fmt.Fprintf(&transcript, "[seq=%d role=%s] %s\n", m.Seq, m.Role, content)
	}
	if transcript.Len() == 0 {
		return nil, nil
	}

	var prompt string
	if incremental {
		prompt = buildIncrementalPrompt(existing, transcript.String())
	} else {
		prompt = buildFullPrompt(transcript.String())
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	maxTokens := 1200
	if incremental {
		maxTokens = 1500
	}
	resp, err := prov.Chat(ctx, []provider.Message{
		{Role: "user", Content: prompt},
	}, nil, model, maxTokens, 0.3)
	if err != nil {
		return nil, fmt.Errorf("extract topics LLM call: %w", err)
	}

	content := stripJSONFence(resp.Content)
	var parsed struct {
		Topics []ExtractedTopic `json:"topics"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("parse topics JSON: %w (raw=%q)", err, content)
	}
	return parsed.Topics, nil
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

func buildFullPrompt(transcript string) string {
	return fmt.Sprintf(`Analyze this conversation excerpt. Each line is tagged with its seq number and role.

CRITICAL — OUTPUT LANGUAGE:
The summary and keywords MUST be in the same language as the conversation.
- Chinese conversation → Chinese summary + Chinese keywords
- English conversation → English summary + English keywords
- Mixed → use the language the user typed in
This is load-bearing: a Chinese-speaking user can only search in Chinese. A summary in the wrong language will NEVER be found by later queries.

Group the excerpt into TOPICS. Real conversations interleave several topics (e.g. weather small-talk wedged between health-advice threads). Each topic becomes its own recallable summary, scoped to ONLY the seq ranges where it was actually discussed — so future retrieval fetches the verbatim messages of that topic without unrelated turns.

For each topic emit:
- topic: short label (<=8 words) in the conversation's language
- summary: 1-2 sentences in the conversation's language
- keywords: 3-7 keywords in the conversation's language
- importance: 1-5 (usefulness to a FUTURE conversation; 1=trivial, 5=key fact/decision/preference)
- segments: list of {"s":N,"e":N} pairs. s and e are seq numbers SHOWN IN THE TRANSCRIPT that belong to this topic. A topic spanning disjoint ranges gets several pairs. Use the exact seq numbers from the transcript; do not invent numbers not present.

Skip greetings, small talk, chit-chat, unresolved errors — emit NO topic for them.

Output STRICT JSON only — no markdown fences, no commentary:
{"topics":[{"topic":"...","summary":"...","keywords":[...],"importance":N,"segments":[{"s":N,"e":N}]}]}

If nothing is worth remembering: {"topics":[]}

Conversation:
%s`, transcript)
}

func buildIncrementalPrompt(existing []store.ConversationSummary, transcript string) string {
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
- Drop greetings/chit-chat/unresolved errors — emit no topic for them.

EXISTING TOPICS (JSON; segments are [seq_start, seq_end] pairs already covered):
%s

NEW MESSAGES (each tagged with seq and role; use the exact seq numbers shown):
%s

Rules:
- Every segment from EXISTING topics that you carry over MUST reappear unchanged in the output.
- New segments must use seq numbers FROM THE NEW MESSAGES transcript only.
- Do not invent seq numbers not present in either source.

Output STRICT JSON only — no markdown fences:
{"topics":[{"topic":"...","summary":"...","keywords":[...],"importance":N,"segments":[{"s":N,"e":N}]}]}

If the new messages add nothing worth remembering, return the existing topics unchanged.`,
		string(existingJSON), transcript)
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
	sessions, err := db.ListIdleSessions(ctx, a.ownerUserID, a.agentID, cutoff, minMessages)
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
		persistConversationSummary(ctx, db, a.provider, a.summaryModelFor(), a.embedder,
			a.ownerUserID, a.agentID, s.SessionKey, s.ChatterUserID)
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
	userID, agentID, sessionKey, chatterUserID string,
) {
	if db == nil || prov == nil {
		return
	}
	allMsgs, err := db.ListSessionMessages(ctx, userID, agentID, sessionKey)
	if err != nil {
		slog.Warn("conversation summary: load messages failed",
			"agent", agentID, "session", sessionKey, "error", err)
		return
	}
	if len(allMsgs) < 2 {
		return
	}
	maxSeq := allMsgs[len(allMsgs)-1].Seq

	lastSeq := 0
	if rec, rerr := db.GetSession(ctx, userID, agentID, sessionKey); rerr == nil && rec != nil {
		lastSeq = rec.LastSummarizedSeq
	}

	var (
		topics      []ExtractedTopic
		extractErr  error
		incremental bool
	)
	if lastSeq == 0 {
		topics, extractErr = extractConversationTopics(ctx, prov, model, allMsgs)
	} else {
		incremental = true
		newMsgs := messagesAfterSeq(allMsgs, lastSeq)
		if len(newMsgs) == 0 {
			slog.Debug("conversation summary: no new messages since last summary",
				"agent", agentID, "session", sessionKey, "last_seq", lastSeq)
			return
		}
		existing, lerr := db.ListConversationSummariesBySession(ctx, userID, agentID, sessionKey)
		if lerr != nil {
			slog.Warn("conversation summary: list existing failed, falling back to full",
				"agent", agentID, "session", sessionKey, "error", lerr)
			topics, extractErr = extractConversationTopics(ctx, prov, model, allMsgs)
		} else {
			topics, extractErr = mergeConversationTopics(ctx, prov, model, existing, newMsgs)
		}
	}
	if extractErr != nil {
		slog.Warn("conversation summary extract failed",
			"agent", agentID, "session", sessionKey, "error", extractErr)
		return
	}
	if len(topics) == 0 {
		slog.Debug("conversation summary: nothing to save",
			"agent", agentID, "session", sessionKey, "incremental", incremental)
		return
	}

	if incremental {
		if derr := db.DeleteConversationSummariesBySession(ctx, userID, agentID, sessionKey); derr != nil {
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
			ChatterUserID:  chatterUserID,
			Topic:          t.Topic,
			Summary:        t.Summary,
			Keywords:       t.Keywords,
			Segments:       segs,
			SeqStart:       minSeq,
			SeqEnd:         tMaxSeq,
			EmbeddingModel: embModel,
			Importance:     t.Importance,
		})
		if err != nil {
			slog.Warn("conversation summary persist failed",
				"agent", agentID, "session", sessionKey, "topic", t.Topic, "error", err)
			continue
		}
		saved++

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

	if serr := db.SetSessionLastSummarizedSeq(ctx, userID, agentID, sessionKey, maxSeq); serr != nil {
		slog.Warn("conversation summary: advance last_summarized_seq failed",
			"agent", agentID, "session", sessionKey, "error", serr)
	}

	slog.Info("conversation summary saved",
		"agent", agentID, "session", sessionKey,
		"max_seq", maxSeq, "topics", saved, "incremental", incremental)
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
	chatterUID := sess.ChatterUserID()
	prov := a.provider
	model := a.summaryModelFor()
	emb := a.embedder

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		slog.Debug("summary extraction: background goroutine started",
			"agent", agentID, "session", sessionKey, "trigger", trigger)
		persistConversationSummary(ctx, db, prov, model, emb,
			owner, agentID, sessionKey, chatterUID)
	}()
}
