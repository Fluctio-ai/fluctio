package kb

import (
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// Chunk preview + quality scoring — pure functions over the same
// GovernContent + ChunkText + chunkOffsets pipeline IngestText runs, so
// what the preview shows is byte-identical to what ingest would store
// (the MimirQ "preview = production pipeline in a temp scope" contract,
// minus the temp scope since nothing here writes).
//
// The scores are heuristic gates, not judgments: they flag chunks a human
// would eyebrow (absurdly short/long, mid-sentence boundaries) so a
// chunker-parameter or governance-rule change can be screened before a
// RewashSources -apply commits it to the KB.

// ChunkStat is one chunk's preview row. All *Chars fields count RUNES
// (true character counts — the old byte counts inflated CJK ~3x and
// clashed with the rune-truncated head snippet). Start/End stay BYTE
// offsets into the governed text, matching chunkOffsets' units.
type ChunkStat struct {
	Index        int    `json:"index"`
	Chars        int    `json:"chars"` // rune count of the chunk
	Start        int    `json:"start"` // byte offset
	End          int    `json:"end"`   // byte offset
	BoundaryOK   bool   `json:"boundary_ok"`
	First40Chars string `json:"head,omitempty"` // preview snippet (rune-safe)
}

// PreviewStats summarizes a would-be ingest. SourceChars/GovernedChars
// are rune counts; coverage/overlap ratios compare byte spans and are
// unit-independent.
type PreviewStats struct {
	SourceChars   int            `json:"source_chars"`   // rune count
	GovernedChars int            `json:"governed_chars"` // rune count
	GovernHits    map[string]int `json:"govern_hits,omitempty"`
	Chunks        int            `json:"chunks"`
	CoverageRatio float64        `json:"coverage_ratio"` // union of spans / governed bytes
	OverlapWaste  float64        `json:"overlap_waste"`  // duplicated span / total span
	TinyChunks    int            `json:"tiny_chunks"`    // < 100 chars: embedding noise
	CliffChunks   int            `json:"cliff_chunks"`   // > 4000 chars: context cliff
	BadBoundaries int            `json:"bad_boundaries"` // ends mid-sentence
}

// IngestPreview is the full preview result.
type IngestPreview struct {
	Chunks []ChunkStat  `json:"chunks"`
	Stats  PreviewStats `json:"stats"`
}

// sentenceEnders are runes a chunk may end on without a boundary warning.
var sentenceEnders = map[rune]bool{
	'。': true, '！': true, '？': true, '.': true, '!': true, '?': true,
	'」': true, '』': true, '）': true, ')': true, '"': true, '”': true,
	'`': true, // code fence or inline code end
}

// boundaryOK reports whether the chunk ends at a sentence/paragraph
// boundary. Trailing whitespace and closing quotes are skipped first, so
// '..."' and '…）' still count when the underlying sentence ended.
func boundaryOK(chunk string) bool {
	t := strings.TrimRight(chunk, " \t\n\r\"'”’）)]}")
	if t == "" {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(t)
	return sentenceEnders[r]
}

// PreviewIngest runs the exact ingest pipeline without writing anything.
func PreviewIngest(content, sourceType string) IngestPreview {
	governed, hits := GovernContent(content, sourceType)
	chunks := ChunkText(governed, 0, 0)
	offsets := chunkOffsets(governed, chunks)

	p := IngestPreview{}
	st := &p.Stats
	// Char counts in runes so the "chars" fields mean characters for CJK
	// content too (byte counts overstated them ~3x).
	st.SourceChars = utf8.RuneCountInString(content)
	st.GovernedChars = utf8.RuneCountInString(governed)
	if len(hits) > 0 {
		st.GovernHits = hits
	}
	st.Chunks = len(chunks)

	// Span accounting: coverage = how much of the source the chunks touch;
	// overlap waste = duplicated coverage from the overlap windows.
	spans := make([][2]int, 0, len(offsets))
	totalSpan := 0
	for _, o := range offsets {
		if o[0] < 0 {
			continue
		}
		spans = append(spans, o)
		totalSpan += o[1] - o[0]
	}
	merged := mergeSpans(spans)
	union := 0
	for _, s := range merged {
		union += s[1] - s[0]
	}
	if len(governed) > 0 {
		st.CoverageRatio = math.Round(float64(union)/float64(len(governed))*1000) / 1000
	}
	if totalSpan > 0 {
		st.OverlapWaste = math.Round(float64(totalSpan-union)/float64(totalSpan)*1000) / 1000
	}

	for i, c := range chunks {
		ok := boundaryOK(c.Content)
		if !ok {
			st.BadBoundaries++
		}
		// Tiny/cliff gates in runes, same unit as the Chars fields and the
		// comments ("< 100 chars" / "> 4000 chars") — not chunker bytes.
		switch n := utf8.RuneCountInString(c.Content); {
		case n < 100:
			st.TinyChunks++
		case n > 4000:
			st.CliffChunks++
		}
		start, end := -1, -1
		if i < len(offsets) {
			start, end = offsets[i][0], offsets[i][1]
		}
		p.Chunks = append(p.Chunks, ChunkStat{
			Index: i, Chars: utf8.RuneCountInString(c.Content), Start: start, End: end,
			BoundaryOK: ok, First40Chars: truncateHeadRunes(c.Content, 40),
		})
	}
	return p
}

// truncateHeadRunes returns the first n runes of s without allocating a
// full rune slice for the whole string (chunk content can be kilobytes
// while only the head is kept).
func truncateHeadRunes(s string, n int) string {
	count := 0
	for idx := range s {
		if count == n {
			return s[:idx]
		}
		count++
	}
	return s
}

// mergeSpans unions spans (sorted by start).
func mergeSpans(spans [][2]int) [][2]int {
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a][0] < spans[b][0] })
	out := [][2]int{spans[0]}
	for _, s := range spans[1:] {
		last := &out[len(out)-1]
		if s[0] <= last[1] {
			if s[1] > last[1] {
				last[1] = s[1]
			}
			continue
		}
		out = append(out, s)
	}
	return out
}
