package kb

import (
	"context"
	"time"
)

type KBSource struct {
	ID              string     `json:"id"`
	AgentID         string     `json:"agent_id"`
	Title           string     `json:"title"`
	SourceType      string     `json:"source_type"` // "text", "url", "file"
	SourceRef       string     `json:"source_ref"`  // URL or filename
	EntryCount      int        `json:"entry_count"`
	TotalChars      int        `json:"total_chars"`
	WikiGeneratedAt *time.Time `json:"wiki_generated_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type KBEntry struct {
	ID         int    `json:"id"`
	SourceID   string `json:"source_id"`
	ChunkIndex int    `json:"chunk_index"`
	Content    string `json:"content"`
}

type KBResult struct {
	SourceID    string  `json:"source_id"`
	SourceTitle string  `json:"source_title"`
	SourceKind  string  `json:"source_kind"`          // "wiki" or "kb"
	PageType    string  `json:"page_type,omitempty"`  // wiki page type: source/concept/entity/query
	ChunkIndex  int     `json:"chunk_index"`
	Content     string  `json:"content"`
	Snippet     string  `json:"snippet"`
	Rank        float64 `json:"rank"`
}

// KnowledgeSource is one [K#]-citable KB source attached to an assistant
// message's metadata so the web UI can render citations as clickable
// badges that open the source. ID is the bracket marker ("K1"), File is
// the source title, Chunk is the 0-based chunk index.
type KnowledgeSource struct {
	ID       string `json:"id"`
	File     string `json:"file"`               // source title (display name)
	Kind     string `json:"kind,omitempty"`     // "wiki" or "kb"
	PageType string `json:"pageType,omitempty"` // wiki page type: source/concept/entity/query
	Chunk    int    `json:"chunk,omitempty"`
}

type KBStats struct {
	SourceCount int `json:"source_count"`
	EntryCount  int `json:"entry_count"`
	TotalChars  int `json:"total_chars"`
}

type KBCfg struct {
	Enabled           bool     `json:"enabled"`
	AutoMode          string   `json:"autoMode,omitempty"`    // "always", "keyword", "disabled"
	Keywords          []string `json:"keywords,omitempty"`    // trigger words for keyword mode
	MaxResults        int      `json:"maxResults,omitempty"`  // default 5
	SearchMode        string   `json:"searchMode,omitempty"`  // "augment" (default), "strict"
	EmptyAction       string   `json:"emptyAction,omitempty"` // "llm" (default), "stop"
	ShowIndicator     *bool    `json:"showIndicator,omitempty"`
	IndicatorFound    string   `json:"indicatorFound,omitempty"`
	IndicatorNotFound string   `json:"indicatorNotFound,omitempty"`
}

// sourcesAccumulatorKey is the context key for a *[]KnowledgeSource that the
// agent threads through tool calls so every KB entry point numbers [K#]
// citations continuing from one counter — no duplicate [K1]/[K2] across
// multiple tool calls in one turn.
type sourcesAccumulatorKey struct{}

// WithSourcesAccumulator returns ctx carrying acc so KB tool calls can append
// citation sources to the shared per-turn accumulator.
func WithSourcesAccumulator(ctx context.Context, acc *[]KnowledgeSource) context.Context {
	return context.WithValue(ctx, sourcesAccumulatorKey{}, acc)
}

// SourcesFromCtx returns the shared accumulator, or nil if none is set.
func SourcesFromCtx(ctx context.Context) *[]KnowledgeSource {
	acc, _ := ctx.Value(sourcesAccumulatorKey{}).(*[]KnowledgeSource)
	return acc
}
