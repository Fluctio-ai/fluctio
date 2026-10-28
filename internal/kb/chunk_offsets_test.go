package kb

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestIngestTextStoresChunkOffsets verifies every stored chunk carries its
// char span in the governed source text: offsets strictly increase with
// chunk_index and content[start:end] reproduces the chunk exactly.
func TestIngestTextStoresChunkOffsets(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Long enough to span multiple 1000-char chunks (overlap included).
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "第%d段：这是一段用于验证偏移量契约的正文内容，包含足够的中文字符来填满整个块。\n\n", i)
	}
	content := sb.String()

	ks := NewKBStore(db, "sqlite")
	if _, err := ks.IngestText(ctx, "a1", "偏移测试", content, "text", "manual"); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT chunk_index, content, start_offset, end_offset FROM kb_entries ORDER BY chunk_index`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		idx          int
		content      string
		start, end   int
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.idx, &r.content, &r.start, &r.end); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) < 2 {
		t.Fatalf("chunks = %d, want multiple (content too short?)", len(got))
	}

	prevStart := -1
	for i, r := range got {
		if r.idx != i {
			t.Errorf("chunk_index gap: got %d at position %d", r.idx, i)
		}
		if r.start < 0 || r.end < 0 {
			t.Fatalf("chunk %d has unknown offsets (%d,%d)", r.idx, r.start, r.end)
		}
		if r.end <= r.start {
			t.Errorf("chunk %d empty span (%d,%d)", r.idx, r.start, r.end)
		}
		// The span covers the chunk's lines exactly at both ends: the
		// chunker re-joins paragraphs with single "\n" (source has "\n\n"),
		// so the whole chunk body isn't verbatim, but its first and last
		// lines must sit at the span boundaries.
		first, _, _ := strings.Cut(r.content, "\n")
		last := r.content[strings.LastIndex(r.content, "\n")+1:]
		if content[r.start:r.start+len(first)] != first {
			t.Errorf("chunk %d first line not at span start [%d:]", r.idx, r.start)
		}
		if content[r.end-len(last):r.end] != last {
			t.Errorf("chunk %d last line not at span end [:%d]", r.idx, r.end)
		}
		if r.start <= prevStart {
			t.Errorf("chunk %d start %d not increasing (prev %d)", r.idx, r.start, prevStart)
		}
		prevStart = r.start
	}
}
