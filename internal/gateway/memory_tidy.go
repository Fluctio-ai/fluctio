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

	"github.com/fluctio-ai/fluctio/internal/agent"
	"github.com/fluctio-ai/fluctio/internal/llmjson"
	"github.com/fluctio-ai/fluctio/internal/privacy"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// runMemoryTidy runs the MEMORY.md consolidation sweep on a daily cadence
// (Hermes-style periodic tidy): MEMORY.md grows by appends (auto-persist
// never rewrites existing entries), ships in every turn's system prompt,
// and its extraction prompt only ever sees the first 500 chars of the
// current file — so beyond that line, duplicate and outdated facts keep
// accumulating. This sweep feeds the WHOLE file to an LLM that proposes
// bounded surgical ops (merge / update / remove); whatever an op prunes
// lands verbatim in MEMORY.archive.md, so consolidation is reversible.
// Probes at boot so a long-standby instance clears its backlog
// immediately. Disabled when FLUCTIO_MEMORY_TIDY_HOURS<=0 (default 24).
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

// memoryTidyResult is the tidy LLM's parsed output: surgical edits
// (agent.EntryEdit, applied via agent.ApplyEntryEdits) — not a
// full-file rewrite — so a bad pass can only touch the entries it
// explicitly targets (the Hermes memory-tool add/replace/remove model).
type memoryTidyResult struct {
	Ops []agent.EntryEdit `json:"ops"`
}

const (
	// maxTidyOpsPerRun bounds the blast radius of one pass (hermes-dreaming
	// caps changes per run the same way, tighter).
	maxTidyOpsPerRun = 10
	// memoryArchiveFile receives everything a tidy pass pruned, verbatim.
	// It is NOT injected into the system prompt — demote-not-delete (Letta
	// archival memory / Zep invalid_at edges): pruning is reversible at
	// zero runtime cost, which is what licenses the relaxed PRUNE rules in
	// the prompt below.
	memoryArchiveFile = "MEMORY.archive.md"
)

// tidyAgentMemory consolidates one agent's MEMORY.md in place. Fail-closed
// throughout: LLM/parse failure, zero applicable ops, or a result that
// would empty the file all skip the write — a bad tidy can destroy
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

	tidyMemoryWithProvider(ctx, g.store, agentID, prov, model, original)
}

// tidyMemoryWithProvider runs one consolidation pass over a MEMORY.md
// snapshot: the LLM proposes bounded surgical ops, each op applies (or is
// skipped) fail-closed, and whatever leaves the file lands verbatim in
// MEMORY.archive.md. Split from tidyAgentMemory so tests can pass a stub
// provider instead of a store-resolved one.
func tidyMemoryWithProvider(ctx context.Context, st store.Store, agentID string, prov provider.Provider, model string, original []byte) {
	// Output is a bounded ops list, not the whole file; the budget still
	// scales with input so long entries can be quoted verbatim in targets.
	maxTokens := 2048 + len(original)/4
	prompt := fmt.Sprintf(`You are consolidating MEMORY.md — the long-term memory file of an AI agent, injected into every conversation's system prompt. It grew by appends. Return surgical edit operations; do NOT rewrite or re-output the whole file.

Current MEMORY.md:
%s

Propose operations:
- replace: merge duplicate or near-duplicate entries into one, update entries superseded by newer information, or tighten wordy entries. Target the OLD entry (an exact substring copied verbatim from the file); new is the rewritten entry, same language as the original, keeping its markdown bullet.
- remove: delete an entry that is dead weight: clearly superseded by a newer entry on the same subject (entries often carry dates — the newer one wins), or clearly transient one-off task state.

Rules:
- Everything you remove or replace away is archived automatically and recoverable — prune dead weight instead of hoarding it. When genuinely unsure an entry is dead, prefer replace (tighten/merge) over remove.
- Never invent facts: replacement text may only recombine information present in the file above.
- A target must appear exactly once in the file or the operation is skipped — quote enough of the entry to be unambiguous.
- At most %d operations. Proposing none is a valid, successful outcome.
- Preserve the markdown structure (## sections, bullet lists).

Output STRICT JSON only — no markdown fences, no commentary:
{"ops": [{"op": "replace", "target": "<exact unique substring of the old entry>", "new": "<rewritten entry>"}, {"op": "remove", "target": "<exact unique substring of the entry>"}]}`,
		string(original), maxTidyOpsPerRun)

	chatCtx := func(timeout time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(provider.WithJSONMode(provider.WithNoThinking(ctx)), timeout)
	}

	callCtx, cancel := chatCtx(3 * time.Minute)
	resp, err := prov.Chat(callCtx, []provider.Message{
		{Role: "user", Content: prompt},
	}, nil, model, maxTokens, 0.2)
	cancel()
	if err != nil {
		slog.Warn("memory tidy: LLM call failed", "agent", agentID, "model", model, "error", err)
		return
	}

	// Always-reasoning models (GLM-5.3 via z.ai's anthropic endpoint
	// thinks unboundedly by default) can burn the ENTIRE output budget on
	// reasoning and return 200 with ZERO content: the tidy budget
	// (2048 + len/4) measured smaller than the reasoning alone, a plain
	// larger budget is a coin flip, and budget_tokens is ignored by that
	// backend. The retry sends thinking:disabled — verified to suppress
	// reasoning to a stub (the same starving call then completes in ~1.3k
	// tokens) — with a modest content floor. Still empty fails closed to
	// the next sweep.
	if strings.TrimSpace(resp.Content) == "" {
		retryMax := max(maxTokens, 8192)
		slog.Info("memory tidy: empty response — reasoning likely ate the budget, retrying with thinking disabled",
			"agent", agentID, "model", model, "maxTokens", maxTokens, "retryMaxTokens", retryMax)
		callCtx, cancel := context.WithTimeout(provider.WithThinkingDisabled(provider.WithJSONMode(provider.WithNoThinking(ctx))), 3*time.Minute)
		resp, err = prov.Chat(callCtx, []provider.Message{
			{Role: "user", Content: prompt},
		}, nil, model, retryMax, 0.2)
		cancel()
		if err != nil {
			slog.Warn("memory tidy: LLM call failed (budget retry)", "agent", agentID, "model", model, "error", err)
			return
		}
		if strings.TrimSpace(resp.Content) == "" {
			slog.Warn("memory tidy: model returned empty content — reasoning consumed the output budget; consider a non-reasoning summary model",
				"agent", agentID, "model", model)
			return
		}
	}

	var res memoryTidyResult
	if err := llmjson.UnmarshalLLM(resp.Content, &res); err != nil {
		slog.Warn("memory tidy: parse failed", "agent", agentID, "model", model, "error", err)
		return
	}
	if len(res.Ops) == 0 {
		slog.Info("memory tidy: nothing to do", "agent", agentID, "model", model)
		return
	}

	tidied, archived := agent.ApplyEntryEdits(string(original), res.Ops, maxTidyOpsPerRun)
	if len(archived) == 0 {
		slog.Info("memory tidy: no applicable operations", "agent", agentID, "proposed", len(res.Ops))
		return
	}
	if strings.TrimSpace(tidied) == "" {
		slog.Warn("memory tidy: operations emptied the file, skipping write", "agent", agentID)
		return
	}

	// Re-read before writing: if a concurrent auto-persist appended to the
	// file during the LLM call, give up this round instead of dropping the
	// fresh facts (they'll ride the next sweep).
	current, err := st.GetAgentFile(ctx, agentID, "MEMORY.md")
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
	if err := st.SaveAgentFile(ctx, agentID, "MEMORY.md", []byte(tidied)); err != nil {
		slog.Warn("memory tidy: save failed", "agent", agentID, "error", err)
		return
	}
	archiveMemoryTidy(ctx, st, agentID, archived)
	slog.Info("memory tidy: consolidated",
		"agent", agentID,
		"applied", len(archived), "proposed", len(res.Ops),
		"before", len(original), "after", len(tidied))
	for _, e := range archived {
		slog.Info("memory tidy: archived entry", "agent", agentID, "entry", e)
	}
}

// archiveMemoryTidy appends the entries one pass pruned to
// MEMORY.archive.md under a timestamped header. Archive failures are
// logged, not fatal: the pruned entries are already echoed to the log as
// their own audit trail.
func archiveMemoryTidy(ctx context.Context, st store.Store, agentID string, entries []string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n## Archived %s\n", time.Now().UTC().Format("2006-01-02 15:04:05"))
	for _, e := range entries {
		fmt.Fprintf(&sb, "- %s\n", strings.TrimSpace(e))
	}
	prev, _ := st.GetAgentFile(ctx, agentID, memoryArchiveFile)
	if err := st.SaveAgentFile(ctx, agentID, memoryArchiveFile, append(prev, []byte(sb.String())...)); err != nil {
		slog.Warn("memory tidy: archive save failed", "agent", agentID, "error", err)
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
