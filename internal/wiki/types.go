package wiki

import "time"

// Page types follow OmniKB's categorisation.
const (
	PageTypeOverview = "overview"
	PageTypeEntity   = "entity"
	PageTypeConcept  = "concept"
	PageTypeSource   = "source"
	PageTypeQuery    = "query"
)

var PageTypes = []string{PageTypeOverview, PageTypeEntity, PageTypeConcept, PageTypeSource, PageTypeQuery}

type WikiPage struct {
	ID         string    `json:"id"`          // "<type>:<slug>" e.g. "entity:fluctio"
	AgentID    string    `json:"agent_id"`
	PageType   string    `json:"page_type"`
	Slug       string    `json:"slug"`
	Title      string    `json:"title"`
	Body       string    `json:"body,omitempty"`
	Summary    string    `json:"summary"`
	SourceIDs  []string  `json:"source_ids"`
	Tags       []string  `json:"tags"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Revision   int       `json:"revision"`
}

type WikiLink struct {
	SrcPageID string  `json:"src_page_id"`
	DstPageID string  `json:"dst_page_id"`
	Relation  string  `json:"relation"`
	Weight    float64 `json:"weight"`
}

type WikiStats struct {
	PageCounts map[string]int `json:"page_counts"`
	TotalPages int            `json:"total_pages"`
	TotalEdges int            `json:"total_edges"`
}

type WikiGraph struct {
	Nodes []WikiPage `json:"nodes"`
	Edges []WikiLink `json:"edges"`
}
