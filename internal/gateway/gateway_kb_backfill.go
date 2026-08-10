package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/wiki"
)

// kbEmbedBackfillTicker is the safety-net vectorizer for kb_entries chunks,
// mirroring wikiEmbedBackfillTicker's role for wiki pages. A kb entry is
// normally embedded at save time (embedSourceEntries, best-effort, silent on
// failure); if the embedder was down or not yet configured then, that chunk
// never gets a vector and searchFlashTodoByVector / SearchRawKB can't reach
// it. This ticker mops up that backlog: every hour (plus a boot pass) it
// walks every agent and, for those with vectorization.kbEmbedding enabled,
// runs BackfillEntryEmbeddings which embeds only the chunks still missing a
// vector. Failure-driven like memoryindex.runOnce — no scanning or API
// probing when nothing's pending.
func (g *Gateway) kbEmbedBackfillTicker(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	g.runKbEmbedBackfillCycle(ctx) // boot pass: a newly-enabled agent doesn't wait an hour
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.runKbEmbedBackfillCycle(ctx)
		}
	}
}

// runKbEmbedBackfillCycle walks every agent and kicks off an incremental
// kb-entry embedding backfill for those with vectorization.kbEmbedding on and
// a resolvable embedder. Each agent runs in its own goroutine (same pattern
// as runWikiEmbedBackfillCycle) so one slow agent can't stall the others.
// One agent's panic is recovered so the rest still run.
func (g *Gateway) runKbEmbedBackfillCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("kb embed backfill cycle panic", "error", r)
		}
	}()
	dbs, ok := g.store.(*store.DBStore)
	if !ok {
		return
	}
	agents, err := g.store.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("kb embed backfill: list agents failed", "error", err)
		return
	}
	for _, ar := range agents {
		if ctx.Err() != nil {
			return
		}
		// Same vectorization-scope read as runWikiEmbedBackfillCycle — owner
		// left empty so the agent-level + system-level merge resolves.
		var vec config.VectorCfg
		if err := scope.SettingInto(ctx, g.store, "vectorization", "", ar.ID, &vec); err != nil {
			continue
		}
		if !vec.KBEmbedding {
			continue
		}
		emb := wiki.EmbedderFromVectorCfg(vec)
		if emb == nil || !emb.Available() {
			continue
		}
		kbs := kb.NewKBStore(dbs.DB(), dbs.Dialect())
		kbs.SetRetriever(emb, nil) // backfill needs the embedder only; no reranker
		agentID := ar.ID
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("kb embed backfill panic", "agent", agentID, "error", r)
				}
			}()
			processed, failed, err := kbs.BackfillEntryEmbeddings(ctx, agentID, 200*time.Millisecond)
			if err != nil {
				slog.Warn("kb embed backfill failed", "agent", agentID, "error", err)
				return
			}
			if processed > 0 || failed > 0 {
				slog.Info("kb embed backfill done",
					"agent", agentID, "processed", processed, "failed", failed)
			}
			// Bookmarks have their own table + embeddings; backfill any saved
			// via CLI/slash (no embedder wired) or whose save-time embed missed.
			// Same kbEmbedding gate — both are KB-family recall.
			bmProcessed, bmFailed, bmErr := kbs.BackfillBookmarkEmbeddings(ctx, agentID, 200*time.Millisecond)
			if bmErr != nil {
				slog.Warn("kb bookmark embed backfill failed", "agent", agentID, "error", bmErr)
				return
			}
			if bmProcessed > 0 || bmFailed > 0 {
				slog.Info("kb bookmark embed backfill done",
					"agent", agentID, "processed", bmProcessed, "failed", bmFailed)
			}
		}()
	}
}
