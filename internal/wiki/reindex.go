package wiki

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/embedding"
)

// ReindexResult counts one wiki page-embedding reindex pass.
type ReindexResult struct {
	Processed int
	Failed    int
}

// ReindexEmbeddings re-embeds every wiki page for an agent. When force is
// true it first clears existing vectors (model switch / mass re-vectorize).
// Pages with empty summary+title are skipped. perCallDelay paces the
// embedding API; zero disables it. Best-effort: a failed embed is counted
// and skipped, never aborts the pass.
func ReindexEmbeddings(ctx context.Context, ws *WikiStore, emb embedding.Embedder, agentID string, force bool, perCallDelay time.Duration) (ReindexResult, error) {
	res := ReindexResult{}
	if ws == nil || emb == nil || !emb.Available() || agentID == "" {
		return res, errors.New("wiki.ReindexEmbeddings: store, embedder, and agentID required")
	}
	if force {
		if err := ws.ClearPageEmbeddingsForAgent(ctx, agentID); err != nil {
			return res, err
		}
	}
	pages, _, err := ws.ListPages(ctx, agentID, "", 5000, 0)
	if err != nil {
		return res, err
	}
	for _, p := range pages {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		text := p.Summary
		if strings.TrimSpace(text) == "" {
			text = p.Title
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		vecs, err := emb.Embed(ctx, []string{text})
		if err != nil || len(vecs) != 1 {
			slog.Warn("wiki reindex: embed failed", "page_id", p.ID, "error", err)
			res.Failed++
			continue
		}
		if err := ws.SavePageEmbedding(ctx, p.AgentID, p.ID, vecs[0], emb.Model()); err != nil {
			slog.Warn("wiki reindex: save failed", "page_id", p.ID, "error", err)
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
