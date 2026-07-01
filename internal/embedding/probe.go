package embedding

import (
	"context"
	"log/slog"
)

// ProbeEmbedder pings the embedding API with a lightweight "ping" text.
// Returns the original embedder on success, nilEmbedder on failure.
// Call at startup so the rest of the system can gate on Available().
func ProbeEmbedder(ctx context.Context, emb Embedder) Embedder {
	if !emb.Available() {
		slog.Debug("embedding: not configured, skipping probe")
		return nilEmbedder{}
	}
	vecs, err := emb.Embed(ctx, []string{"ping"})
	if err != nil {
		slog.Warn("embedding: probe failed, vector recall disabled", "err", err)
		return nilEmbedder{}
	}
	if len(vecs) != 1 || len(vecs[0]) != emb.Dim() {
		slog.Warn("embedding: probe returned unexpected shape, vector recall disabled",
			"got_vecs", len(vecs), "dim", len(vecs[0]))
		return nilEmbedder{}
	}
	slog.Info("embedding: probe ok", "model", emb.Model(), "dim", emb.Dim())
	return emb
}
