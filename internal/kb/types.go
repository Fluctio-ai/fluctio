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
	// Content-type fields. Type is "article" (default, regular chunked KB
	// source), "flash" (灵感闪记 — short inspiration note, one chunk), or
	// "todo" (a task with status + optional start/end times). Status is the
	// todo lifecycle (pending/in_progress/done/cancelled); the time fields are
	// set only for todos and reminded_at is the due-push dedup stamp.
	Type       string     `json:"type,omitempty"`
	Status     string     `json:"status,omitempty"`
	StartAt    *time.Time `json:"start_at,omitempty"`
	EndAt      *time.Time `json:"end_at,omitempty"`
	RemindedAt *time.Time `json:"reminded_at,omitempty"`
	// WikiDirtyAt, when set and later than WikiGeneratedAt, marks the source's
	// derived wiki pages as stale — the autogen sweep rebuilds them.
	WikiDirtyAt *time.Time `json:"wiki_dirty_at,omitempty"`
}

type KBEntry struct {
	ID         int    `json:"id"`
	SourceID   string `json:"source_id"`
	ChunkIndex int    `json:"chunk_index"`
	Content    string `json:"content"`
}

// KBBookmark is one row of kb_bookmarks — a saved web link the user wants to
// revisit later. Unlike a KBSource (chunked article content), a bookmark's
// payload is the URL itself: an external pointer that may later be promoted
// into a full article (PromotedTo tracks that link). Title and Summary are
// optional metadata; Content holds the page body fetched at save time so the
// bookmark survives link rot. Source records the entry point ("cli" / "slash"
// / "llm").
type KBBookmark struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	Content    string    `json:"content,omitempty"`
	FetchedAt  time.Time `json:"fetched_at,omitempty"`
	Source     string    `json:"source,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	PromotedTo string    `json:"promoted_to_article_id,omitempty"`
}

// KBNote is one personal note: markdown body + optional whiteboard
// (Excalidraw elements JSON) + attachments (tracked separately in
// kb_note_attachments).
type KBNote struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	Title      string    `json:"title"`
	ContentMD  string    `json:"content_md"`
	Whiteboard string    `json:"whiteboard,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// KBNoteAttachment tracks one uploaded file bound to a note. Bytes live
// in the agent workspace at FilePath (notes/<noteID>/<file>), served via
// the standard agent-files channel.
type KBNoteAttachment struct {
	ID        string    `json:"id"`
	NoteID    string    `json:"note_id"`
	AgentID   string    `json:"agent_id"`
	FileName  string    `json:"file_name"`
	FilePath  string    `json:"file_path"`
	Mime      string    `json:"mime"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// KBCard is one row of kb_cards — a spaced-repetition Q&A flashcard. The
// front is Question; the back is Answer (释义/知识点/用法). Cards are
// generated nightly from the day's diary + wiki delta (SourceType
// "diary"/"wiki", SourceRef points back at the origin so the UI can deep
// link) or written by hand ("manual"). Scheduling is Ebbinghaus-lite over
// CardIntervals: DueAt nil means not yet scheduled. Status walks
// active → mastered (all intervals done) / archived.
type KBCard struct {
	ID              string     `json:"id"`
	AgentID         string     `json:"agent_id"`
	Question        string     `json:"question"`
	Answer          string     `json:"answer"`
	SourceType      string     `json:"source_type"`
	SourceRef       string     `json:"source_ref"`
	SourceExcerpt   string     `json:"source_excerpt"`
	Status          string     `json:"status"`
	IntervalIndex   int        `json:"interval_index"`
	DueAt           *time.Time `json:"due_at,omitempty"`
	LastReviewedAt  *time.Time `json:"last_reviewed_at,omitempty"`
	ReviewCount     int        `json:"review_count"`
	LapseCount      int        `json:"lapse_count"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// KBCardReview is one graded review of one card (kb_card_reviews) — the
// card detail's timeline and the streak computation source. Grade is
// "forgot" / "fuzzy" / "remembered".
type KBCardReview struct {
	ID                int        `json:"id"`
	CardID            string     `json:"card_id"`
	AgentID           string     `json:"agent_id"`
	Grade             string     `json:"grade"`
	PrevIntervalIndex int        `json:"prev_interval_index"`
	NewIntervalIndex  int        `json:"new_interval_index"`
	NewDueAt          *time.Time `json:"new_due_at,omitempty"`
	ReviewedAt        time.Time  `json:"reviewed_at"`
}

// KBCardStats is the cards-page dashboard header payload.
type KBCardStats struct {
	DueToday   int `json:"due_today"`
	Active     int `json:"active"`
	Mastered   int `json:"mastered"`
	Archived   int `json:"archived"`
	StreakDays int `json:"streak_days"`
}

type KBResult struct {
	SourceID    string  `json:"source_id"`
	SourceTitle string  `json:"source_title"`
	SourceKind  string  `json:"source_kind"`           // "wiki" or "kb"
	PageType    string  `json:"page_type,omitempty"`   // wiki page type: source/concept/entity/query
	ContentType string  `json:"content_type,omitempty"` // kb source type: flash/todo (set only via flash/todo vector recall)
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

// ArticleInsights is the LLM-generated deep reading of one KB article source,
// mirroring the sheng-gen-fa-ya skill's six-section layout. Original text and
// todos already live in kb_entries / the todo board, so insights keeps the
// other four: summary, chapter outline, curated quotes, and "sprouts"
// (knowledge extensions plus an optional cross-domain echo). 1:1 with a
// kb_sources row of type 'article'; stored as four independent JSON blobs so
// each section can be rendered or rebuilt on its own.
type ArticleInsights struct {
	SourceID    string         `json:"source_id"`
	Summary     InsightSummary `json:"summary"`
	Quotes      []InsightQuote `json:"quotes"`
	Actions     []string       `json:"actions"`
	Sprouts     InsightSprouts `json:"sprouts"`
	GeneratedAt time.Time      `json:"generated_at"`
}

// InsightSummary is the structured recap: a 2-3 sentence core, thematic topic
// blocks (each a heading + label/text points), and a chapter-by-chapter outline.
type InsightSummary struct {
	Core     string           `json:"core"`
	Topics   []InsightTopic   `json:"topics"`
	Chapters []InsightChapter `json:"chapters"`
}

type InsightTopic struct {
	Heading string         `json:"heading"`
	Points  []InsightPoint `json:"points"`
}

type InsightPoint struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

type InsightChapter struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// InsightQuote is one curated verbatim line from the source with a category
// tag. Verified is set by GenerateInsights via a deterministic substring
// check against the source text — true means the quote appears literally
// (modulo whitespace); false means the LLM likely paraphrased and the user
// should double-check it against the original.
type InsightQuote struct {
	Text     string `json:"text"`
	Tag      string `json:"tag"`
	Verified bool   `json:"verified,omitempty"`
}

// InsightSprouts holds the knowledge-extension section: an intro, 3-5 sprouts
// (each a seed tied to the source + body + an Aha moment), and an optional
// "echo" that cross-validates one seed quote across philosophy / psychology /
// literature lenses.
type InsightSprouts struct {
	Intro string          `json:"intro"`
	Items []InsightSprout `json:"items"`
	Echo  *InsightEcho    `json:"echo,omitempty"`
}

type InsightSprout struct {
	Index int    `json:"index"`
	Emoji string `json:"emoji"`
	Title string `json:"title"`
	Seed  string `json:"seed"`
	Body  string `json:"body"`
	Aha   string `json:"aha"`
}

type InsightEcho struct {
	SeedQuote   string            `json:"seed_quote"`
	SeedComment string            `json:"seed_comment"`
	Items       []InsightEchoItem `json:"items"`
}

type InsightEchoItem struct {
	Perspective string `json:"perspective"`
	Label       string `json:"label"`
	Quote       string `json:"quote"`
	Source      string `json:"source"`
}

type KBCfg struct {
	Enabled     bool     `json:"enabled"`
	AutoMode    string   `json:"autoMode,omitempty"`    // "always", "keyword", "disabled"
	Keywords    []string `json:"keywords,omitempty"`    // trigger words for keyword mode
	MaxResults  int      `json:"maxResults,omitempty"`  // default 5
	SearchMode  string   `json:"searchMode,omitempty"`  // "augment" (default), "strict"
	EmptyAction string   `json:"emptyAction,omitempty"` // "llm" (default), "stop"
	// Dedup thresholds for inbound KB writes (0 = use built-in default).
	// At or above these an existing same/similar source blocks the write:
	// flash/todo are skipped silently; high-similarity articles (≥High) also
	// skip (near-duplicate, nothing worth a merge); mid-tier pends for user.
	ArticleDupHigh    float64 `json:"articleDupHigh,omitempty"`    // ≥ → skip (near-duplicate)
	ArticleDupMid     float64 `json:"articleDupMid,omitempty"`     // ≥ → pend for user
	FlashDupThreshold float64 `json:"flashDupThreshold,omitempty"` // ≥ → skip flash
	TodoDupThreshold  float64 `json:"todoDupThreshold,omitempty"`  // ≥ → skip todo
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

// SourceOrigin records where a KB write originated (session + message seq),
// threaded through ctx so save paths can stamp kb_sources for L1 dedup:
// same session + overlapping seq range = a rewrite of already-captured content.
type SourceOrigin struct {
	SessionID string
	Seq       int
}

type sourceOriginKey struct{}

// WithSourceOrigin returns ctx carrying o so KB save tools can read the
// session+seq of the turn that triggered the write.
func WithSourceOrigin(ctx context.Context, o SourceOrigin) context.Context {
	return context.WithValue(ctx, sourceOriginKey{}, o)
}

// SourceOriginFromCtx returns the origin on ctx, or zero value if none.
func SourceOriginFromCtx(ctx context.Context) SourceOrigin {
	o, _ := ctx.Value(sourceOriginKey{}).(SourceOrigin)
	return o
}
