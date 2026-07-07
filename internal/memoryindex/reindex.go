// Package memoryindex is the SAFETY-NET vectorizer for conversation
// summaries.
//
// The primary vectorization path is save-time: persistConversationSummary
// embeds a summary the moment it's written. This package only mops up the
// summaries that path MISSED — e.g. the embedding API was down when the
// summary was saved, or the embedder was configured after the fact. It is
// deliberately failure-driven: each tick it asks "which summaries lack a
// vector?" and processes only those, never scanning agents that have
// nothing pending and never probing the embedding API just to check.
package memoryindex

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// Result counts a single reindex pass.
type Result struct {
	Agent     string
	Processed int
	Failed    int
}

// Reindex re-embeds summaries for one agent. When force is true it first
// clears that agent's existing vectors and re-embeds every summary (model
// switch / mass re-vectorize) — this is the manual "Force re-vectorize"
// button, NOT the periodic loop. The periodic loop uses runOnce instead,
// which is failure-driven across all agents.
//
// perCallDelay sleeps between individual Embed calls to be gentle on the
// embedding API. Zero disables the delay.
func Reindex(ctx context.Context, db *store.DBStore, emb embedding.Embedder, agentID string, force bool, perCallDelay time.Duration) (Result, error) {
	res := Result{Agent: agentID}
	if db == nil || emb == nil || !emb.Available() || agentID == "" {
		return res, errors.New("memoryindex: db, embedder, and agentID required")
	}

	if force {
		if err := db.ClearConversationSummaryVectorsForAgent(ctx, agentID); err != nil {
			return res, err
		}
	}

	summaries, err := db.ListConversationSummariesByAgent(ctx, agentID, 5000)
	if err != nil {
		return res, err
	}

	for _, s := range summaries {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if vecErr := vectorizeSummary(ctx, db, emb, s); vecErr != nil {
			res.Failed++
			continue
		}
		res.Processed++
		if perCallDelay > 0 {
			select {
			case <-time.After(perCallDelay):
			case <-ctx.Done():
				return res, ctx.Err()
			}
		}
	}
	return res, nil
}

// vectorizeSummary embeds one summary's text (+keywords), writes the vec0
// row, and stamps the embedding_model. Shared by the force rebuild and the
// periodic loop. Returns an error (already logged) so callers count it as
// failed and move on — best-effort, never aborts the whole pass.
func vectorizeSummary(ctx context.Context, db *store.DBStore, emb embedding.Embedder, s store.ConversationSummary) error {
	text := s.Summary
	if len(s.Keywords) > 0 {
		text += " " + strings.Join(s.Keywords, " ")
	}
	vecs, err := emb.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		slog.Warn("memoryindex: embed failed", "summary_id", s.ID, "error", err)
		return errors.New("embed failed")
	}
	if err := db.InsertConversationSummaryVector(ctx, s.ID, vecs[0]); err != nil {
		slog.Warn("memoryindex: vector insert failed", "summary_id", s.ID, "error", err)
		return err
	}
	// Stamp the model so this row stops re-queueing. Best-effort — the
	// vector write already succeeded.
	_ = db.SetConversationSummaryEmbeddingModel(ctx, s.ID, emb.Model())
	return nil
}

// RunLoop is the gateway's periodic SAFETY NET. Every interval it asks
// "which summaries lack a vector?" and processes only those — never
// scanning agents with nothing pending, never probing the embedding API
// to check. Empty backlog = a single cheap query, no further work.
//
// perCallDelay paces individual embedding calls. The loop exits on ctx
// cancellation.
func RunLoop(ctx context.Context, db *store.DBStore, interval, perCallDelay time.Duration) {
	if db == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	slog.Info("memoryindex: periodic backfill loop started",
		"interval", interval, "per_call_delay", perCallDelay)

	t := time.NewTicker(interval)
	defer t.Stop()
	runOnce(ctx, db, perCallDelay) // clear any boot-time backlog fast
	for {
		select {
		case <-ctx.Done():
			slog.Info("memoryindex: periodic backfill loop stopped")
			return
		case <-t.C:
			runOnce(ctx, db, perCallDelay)
		}
	}
}

// runOnce is one failure-driven backfill pass. It pulls every summary
// lacking a vector (one global query), groups them by agent so each
// agent's embedder is resolved at most once, and embeds the backlog.
// No-op when nothing is pending — no agent scan, no embedder probing.
func runOnce(ctx context.Context, db *store.DBStore, perCallDelay time.Duration) {
	pending, err := db.ListConversationSummariesNeedingVector(ctx, "", 5000)
	if err != nil {
		slog.Warn("memoryindex: list pending failed", "error", err)
		return
	}
	if len(pending) == 0 {
		return // nothing failed vectorization this tick — done
	}

	// Group by (owner, agent) so each agent's embedder is resolved once.
	type ownerAgent struct{ owner, agent string }
	groups := map[ownerAgent][]store.ConversationSummary{}
	order := []ownerAgent{}
	for _, s := range pending {
		k := ownerAgent{owner: s.UserID, agent: s.AgentID}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s)
	}

	processed, failed := 0, 0
	for _, k := range order {
		if ctx.Err() != nil {
			return
		}
		// Embedder resolved ONLY for agents with pending work, and without
		// a probe — availability is proven by the per-summary Embed calls
		// below. Saves an API call per agent per tick.
		emb := embedderForAgent(ctx, db, k.owner, k.agent)
		if emb == nil || !emb.Available() {
			continue // embedding not configured for this agent — skip
		}
		for _, s := range groups[k] {
			if ctx.Err() != nil {
				return
			}
			if vecErr := vectorizeSummary(ctx, db, emb, s); vecErr != nil {
				failed++
			} else {
				processed++
			}
			if perCallDelay > 0 {
				select {
				case <-time.After(perCallDelay):
				case <-ctx.Done():
					return
				}
			}
		}
	}
	if processed > 0 || failed > 0 {
		slog.Info("memoryindex: backfill pass complete",
			"agents", len(order), "processed", processed, "failed", failed)
	}
}

// embedderForAgent resolves the agent's merged memory config
// (system→owner-user→agent) and builds an embedder. Returns nil when
// embedding is disabled. Does NOT probe — the caller (periodic loop)
// only invokes this when there's real work, and the per-summary Embed
// calls validate reachability naturally.
func embedderForAgent(ctx context.Context, db *store.DBStore, ownerUserID, agentID string) embedding.Embedder {
	var mem config.MemoryCfg
	if err := scope.SettingInto(ctx, db, "memory", ownerUserID, agentID, &mem); err != nil {
		return nil
	}
	if !mem.Embedding.Enabled {
		return nil
	}
	ec := mem.Embedding
	return embedding.NewOpenAICompatEmbedder(ec.APIBase, ec.APIKey, ec.Model, ec.Dim, ec.DimEnabled)
}
