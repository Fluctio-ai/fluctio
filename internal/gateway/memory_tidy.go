package gateway

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/llmjson"
	"github.com/fluctio-ai/fluctio/internal/privacy"
	"github.com/fluctio-ai/fluctio/internal/provider"
)

// runMemoryTidy runs the MEMORY.md consolidation sweep on a daily cadence
// (Hermes-style periodic tidy): MEMORY.md grows by appends (auto-persist
// never rewrites existing entries), ships in every turn's system prompt,
// and its extraction prompt only ever sees the first 500 chars of the
// current file — so beyond that line, duplicate and outdated facts keep
// accumulating. This sweep feeds the WHOLE file to an LLM that merges,
// prunes and tightens it. Probes at boot so a long-standby instance
// clears its backlog immediately. Disabled when
// FLUCTIO_MEMORY_TIDY_HOURS<=0 (default 24).
func (g *Gateway) runMemoryTidy(ctx context.Context) {
	hours := memoryTidyHours()
	if hours <= 0 {
		slog.Info("memory tidy disabled")
		return
	}
	slog.Info("memory tidy started", "interval_hours", hours)
	g.runEvery(ctx, time.Duration(hours)*time.Hour, g.tidyMemoryFilesOnce)
}

// tidyMemoryFilesOnce walks every agent in the store and tidies each
// MEMORY.md when it has grown past the size floor. Store-level walk
// (ListAllAgents, like the cards/diary/recall sweeps) rather than the
// in-memory userspace registry: the registry populates lazily on first
// auth, so a boot probe through it would usually find nothing, and agents
// evicted from memory would be skipped. One failure aborts the pass;
// per-agent errors are already logged inside tidyAgentMemory.
func (g *Gateway) tidyMemoryFilesOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("memory tidy cycle panic", "error", r)
		}
	}()
	minChars := memoryTidyMinChars()
	agents, err := g.store.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("memory tidy: list agents failed", "error", err)
		return
	}
	for _, ag := range agents {
		if ctx.Err() != nil {
			return
		}
		g.tidyAgentMemory(ctx, ag.ID, minChars)
	}
}

// memoryTidyResult is the tidy LLM's parsed output. Dropped entries are
// echoed back verbatim so each sweep's information loss is auditable in
// the log.
type memoryTidyResult struct {
	Tidied  string   `json:"tidied"`
	Dropped []string `json:"dropped"`
	Merged  int      `json:"merged"`
}

// tidyAgentMemory consolidates one agent's MEMORY.md in place. Fail-closed
// throughout: LLM/parse failure, an empty result, or a result that didn't
// actually shrink the file all skip the write — a bad tidy can destroy
// long-term memory, while a skipped tidy only costs one more day of
// growth. The write is also abandoned when the file changed on disk during
// the LLM call (a concurrent auto-persist append) so this sweep can't
// clobber fresh facts; that append just waits for the next sweep.
func (g *Gateway) tidyAgentMemory(ctx context.Context, agentID string, minChars int) {
	original, err := g.store.GetAgentFile(ctx, agentID, "MEMORY.md")
	if err != nil || len(original) < minChars {
		return // no file yet, read error, or still small enough to not bother
	}

	prov, model := resolveWikiProvider(g.store, agentID, "")
	if prov == nil || model == "" {
		slog.Debug("memory tidy: no provider resolved", "agent", agentID)
		return
	}

	callCtx, cancel := context.WithTimeout(provider.WithJSONMode(provider.WithNoThinking(ctx)), 3*time.Minute)
	defer cancel()

	// Output budget scales with the input: the tidied file is roughly the
	// original's size, and Chinese text runs ~1.5 chars/token, so half the
	// byte count (plus a floor) keeps the JSON from truncating mid-string.
	maxTokens := 2048 + len(original)/2
	prompt := fmt.Sprintf(`You are tidying MEMORY.md — the long-term memory file of an AI agent, injected into every conversation's system prompt. It grew by appends and now needs consolidation: dedupe, merge, prune, tighten — WITHOUT losing live information.

Current MEMORY.md:
%s

Rules:
- MERGE: entries stating the same fact/preference/decision (possibly worded differently or dated differently) become ONE entry, keeping the most recent/correct details.
- PRUNE: drop an entry ONLY when it is clearly superseded by a newer entry in this file, or clearly transient (one-off task state). When unsure, KEEP it.
- TIGHTEN: rewrite wordy entries compactly, preserving their language (Chinese stays Chinese, English stays English).
- NEVER invent new facts: the tidied file may only contain information present above.
- Preserve the markdown structure (## sections, bullet lists).

Output STRICT JSON only — no markdown fences, no commentary:
{"tidied": "<the full tidied MEMORY.md content>", "dropped": ["<each pruned entry, verbatim, for the audit log>"], "merged": <number of entries merged away>}`,
		string(original))

	resp, err := prov.Chat(callCtx, []provider.Message{
		{Role: "user", Content: prompt},
	}, nil, model, maxTokens, 0.2)
	if err != nil {
		slog.Warn("memory tidy: LLM call failed", "agent", agentID, "model", model, "error", err)
		return
	}

	var res memoryTidyResult
	if err := llmjson.UnmarshalLLM(resp.Content, &res); err != nil {
		slog.Warn("memory tidy: parse failed", "agent", agentID, "model", model, "error", err)
		return
	}
	tidied := strings.TrimSpace(res.Tidied)
	if tidied == "" {
		slog.Warn("memory tidy: empty result, skipping write", "agent", agentID)
		return
	}
	if len(tidied) >= len(original) {
		slog.Info("memory tidy: result did not shrink the file, skipping write",
			"agent", agentID, "before", len(original), "after", len(tidied))
		return
	}

	// Re-read before writing: if a concurrent auto-persist appended to the
	// file during the LLM call, give up this round instead of dropping the
	// fresh facts (they'll ride the next sweep).
	current, err := g.store.GetAgentFile(ctx, agentID, "MEMORY.md")
	if err != nil || !bytes.Equal(current, original) {
		slog.Info("memory tidy: file changed during tidy, deferring to next sweep", "agent", agentID)
		return
	}

	if threats := privacy.Scan(tidied); len(threats) > 0 {
		for _, t := range threats {
			slog.Warn("memory tidy: safety threat in tidied output",
				"agent", agentID, "type", t.Type, "pattern", t.Pattern)
		}
	}
	if err := g.store.SaveAgentFile(ctx, agentID, "MEMORY.md", []byte(tidied)); err != nil {
		slog.Warn("memory tidy: save failed", "agent", agentID, "error", err)
		return
	}
	slog.Info("memory tidy: consolidated",
		"agent", agentID,
		"before", len(original), "after", len(tidied),
		"dropped", len(res.Dropped), "merged", res.Merged)
	for _, d := range res.Dropped {
		slog.Info("memory tidy: dropped entry", "agent", agentID, "entry", d)
	}
}

// memoryTidyHours reads FLUCTIO_MEMORY_TIDY_HOURS (default 24; 0
// disables). Delegates to readRetentionHours, shared with the workflow
// retention sweep.
func memoryTidyHours() int {
	return readRetentionHours("FLUCTIO_MEMORY_TIDY_HOURS", 24)
}

// memoryTidyMinChars reads FLUCTIO_MEMORY_TIDY_MIN_CHARS (default 4096) —
// files below this size are left alone so the sweep never spends an LLM
// call on a MEMORY.md that is still tight. Empty/invalid/non-positive
// values all fall back to the default; disable the sweep entirely via
// FLUCTIO_MEMORY_TIDY_HOURS=0.
func memoryTidyMinChars() int {
	v := strings.TrimSpace(os.Getenv("FLUCTIO_MEMORY_TIDY_MIN_CHARS"))
	if v == "" {
		return 4096
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 4096
	}
	return n
}
