package kb

import (
	"context"
	"strings"
	"testing"
)

// TestRewashDryRunThenApply: ingest under the current pipeline, bump the
// hash by simulating a pipeline change, verify dry-run counts and apply
// replaces chunks + stamps wiki_dirty_at.
func TestRewashDryRunThenApply(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()
	ks := NewKBStore(db, "sqlite")

	content := "正文段落一，内容足够长才能通过治理和切块。\n\n正文段落二。\n\n本文约 100 字，阅读需要 1 分钟\n\n正文段落三。"
	sid, err := ks.IngestText(ctx, "a1", "rewash me", content, "url", "https://example.com/x")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Fresh ingest under the current pipeline: nothing to do.
	rep, err := ks.RewashSources(ctx, "a1", false)
	if err != nil {
		t.Fatalf("dry run fresh: %v", err)
	}
	if rep.WouldChange != 0 || rep.SkippedFresh != 1 {
		t.Fatalf("fresh rewash = %+v, want 1 fresh 0 change", rep)
	}

	// Simulate a pipeline change (e.g. a governance rule bump in
	// production): a source whose stored hash no longer matches the
	// current pipeline is stale.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO kb_sources (id, agent_id, title, source_type, source_ref, orig_content, pipeline_hash)
		 VALUES ('stale1', 'a1', 'stale', 'url', 'x', ?, 'stalehash')`, content); err != nil {
		t.Fatalf("insert stale: %v", err)
	}

	rep, err = ks.RewashSources(ctx, "a1", false)
	if err != nil {
		t.Fatalf("dry run stale: %v", err)
	}
	if rep.WouldChange != 1 {
		t.Fatalf("stale dry run = %+v, want WouldChange 1", rep)
	}

	// Apply: chunks replaced (the read-time boilerplate line now stripped),
	// wiki_dirty_at stamped, pipeline_hash updated.
	rep, err = ks.RewashSources(ctx, "a1", true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Rewashed != 1 {
		t.Fatalf("apply report = %+v, want Rewashed 1", rep)
	}
	var joined, hash string
	if err := db.QueryRowContext(ctx,
		`SELECT GROUP_CONCAT(ke.content, ''), s.pipeline_hash FROM kb_entries ke JOIN kb_sources s ON s.id = ke.source_id WHERE s.id = 'stale1' GROUP BY s.pipeline_hash`).Scan(&joined, &hash); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(joined, "本文约") {
		t.Errorf("rewashed chunks still contain boilerplate: %q", joined)
	}
	if !strings.Contains(joined, "正文段落三") {
		t.Errorf("real content lost in rewash: %q", joined)
	}
	if hash != PipelineHash() {
		t.Errorf("pipeline_hash = %q, want current %q", hash, PipelineHash())
	}
	var dirty string
	if err := db.QueryRowContext(ctx, `SELECT wiki_dirty_at FROM kb_sources WHERE id = 'stale1'`).Scan(&dirty); err != nil || dirty == "" {
		t.Errorf("wiki_dirty_at not stamped (err=%v val=%q)", err, dirty)
	}
	var orig string
	if err := db.QueryRowContext(ctx, `SELECT orig_content FROM kb_sources WHERE id = 'stale1'`).Scan(&orig); err != nil {
		t.Fatalf("read orig: %v", err)
	}
	if orig != content {
		t.Errorf("orig_content lost: %q", orig)
	}
	// The originally-ingested source was fresh (current hash): untouched,
	// no dirty stamp.
	var dirty2 string
	_ = db.QueryRowContext(ctx, `SELECT wiki_dirty_at FROM kb_sources WHERE id = ?`, sid).Scan(&dirty2)
	if dirty2 != "" {
		t.Errorf("fresh source got dirty-stamped: %q", dirty2)
	}
}

// TestRewashSkipsLegacySources: rows without orig_content count as
// MissingOrigin, never WouldChange.
func TestRewashSkipsLegacySources(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO kb_sources (id, agent_id, title, source_type, source_ref) VALUES ('legacy1', 'a1', 'old', 'url', 'x')`); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	ks := NewKBStore(db, "sqlite")
	rep, err := ks.RewashSources(ctx, "a1", false)
	if err != nil {
		t.Fatalf("rewash: %v", err)
	}
	if rep.MissingOrigin != 1 || rep.WouldChange != 0 {
		t.Errorf("legacy rewash = %+v, want MissingOrigin 1", rep)
	}
}

func TestPreviewIngestStatsAndBoundaries(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("第一段完整句子。\n\n第二段也很完整。\n\n")
	// A long unterminated tail forces a mid-sentence chunk boundary.
	for i := 0; i < 200; i++ {
		sb.WriteString("无句号长句片段")
	}
	p := PreviewIngest(sb.String(), "text")
	st := p.Stats
	if st.Chunks == 0 {
		t.Fatal("no chunks")
	}
	if st.CoverageRatio < 0.9 {
		t.Errorf("coverage %.3f, want >0.9", st.CoverageRatio)
	}
	if len(p.Chunks) != st.Chunks {
		t.Errorf("chunk rows %d != stats %d", len(p.Chunks), st.Chunks)
	}
	// The paragraph chunks end cleanly; the long-tail chunk should trip
	// the boundary and probably the cliff gate.
	if st.BadBoundaries == 0 {
		t.Error("expected at least one bad boundary from the unterminated tail")
	}
	for _, c := range p.Chunks {
		if c.Start < 0 {
			t.Errorf("chunk %d missing offsets", c.Index)
		}
	}
}

func TestPreviewWebPackReflectsGovernance(t *testing.T) {
	content := "正文。\n\n点击上方蓝字关注公众号，获取最新资讯\n\n更多正文。"
	p := PreviewIngest(content, "url")
	if p.Stats.GovernHits["follow_cta"] != 1 {
		t.Errorf("web governance hits missing follow_cta: %+v", p.Stats.GovernHits)
	}
	if p.Stats.GovernedChars >= p.Stats.SourceChars {
		t.Errorf("governed %d should be < source %d after a line removal", p.Stats.GovernedChars, p.Stats.SourceChars)
	}
	p2 := PreviewIngest(content, "text")
	if len(p2.Stats.GovernHits) != 0 {
		t.Errorf("text sources must not get the web pack: %+v", p2.Stats.GovernHits)
	}
	if p2.Stats.GovernedChars != p2.Stats.SourceChars {
		t.Errorf("text preview should not shrink content")
	}
}
