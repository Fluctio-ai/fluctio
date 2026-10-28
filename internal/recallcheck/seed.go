package recallcheck

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// Seed generates golden cases from the agent's own KB — the mechanical
// bootstrap before hand-written hard cases exist. Title-as-query is an easy
// recall test; its job is not difficulty but tripwiring: a lane killed by
// a threshold or embedding regression fails every seeded case at once.
// Hard cases (paraphrases, cross-lingual, is_unanswerable traps that look
// answerable) are manual additions on top.
//
// Read-only: the seed SELECTs never write. Unanswerable traps use random
// UUID strings — guaranteed absent from any KB.
func Seed(ctx context.Context, db *store.DBStore, agentID string, perLane, unanswerable int) ([]GoldenCase, error) {
	raw := db.DB()
	dialect := db.Dialect()
	ph := func(n int) string {
		if dialect == "postgres" {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	var out []GoldenCase
	n := 0
	add := func(lane, query, expect, source string) {
		n++
		out = append(out, GoldenCase{
			Schema: GoldenCaseSchema, ID: fmt.Sprintf("%s-%04d", lane, n),
			Query: query, Lane: lane, ExpectIDs: []string{expect}, Source: source,
		})
	}

	// Wiki pages: title is the query, page id the expectation.
	if rows, err := raw.QueryContext(ctx,
		`SELECT id, title FROM wiki_pages WHERE agent_id=`+ph(1)+` AND title != '' ORDER BY updated_at DESC LIMIT `+ph(2), agentID, perLane); err == nil {
		for rows.Next() {
			var id, title string
			if err := rows.Scan(&id, &title); err == nil {
				add("wiki", title, id, "seed:wiki_title")
			}
		}
		rows.Close()
	}

	// Flash/todo sources: title as query, source uuid as expectation.
	if rows, err := raw.QueryContext(ctx,
		`SELECT id, title FROM kb_sources WHERE agent_id=`+ph(1)+` AND type IN ('flash','todo') AND title != '' ORDER BY updated_at DESC LIMIT `+ph(2), agentID, perLane); err == nil {
		for rows.Next() {
			var id, title string
			if err := rows.Scan(&id, &title); err == nil {
				add("flash_todo", title, id, "seed:ft_title")
			}
		}
		rows.Close()
	}

	// Bookmarks: title as query, bookmark id as expectation.
	if rows, err := raw.QueryContext(ctx,
		`SELECT id, title FROM kb_bookmarks WHERE agent_id=`+ph(1)+` AND title != '' ORDER BY created_at DESC LIMIT `+ph(2), agentID, perLane); err == nil {
		for rows.Next() {
			var id, title string
			if err := rows.Scan(&id, &title); err == nil {
				add("bookmark", title, id, "seed:bm_title")
			}
		}
		rows.Close()
	}

	// Conversation summaries: topic (or summary head) as query, summary id
	// as expectation. The [MEM] lane and the memory_search tool share this
	// corpus.
	if rows, err := raw.QueryContext(ctx,
		`SELECT id, topic, summary FROM conversation_summaries WHERE agent_id=`+ph(1)+` AND superseded_by = 0 ORDER BY id DESC LIMIT `+ph(2), agentID, perLane); err == nil {
		for rows.Next() {
			var id, topic, summary string
			if err := rows.Scan(&id, &topic, &summary); err == nil {
				q := topic
				if q == "" {
					r := []rune(summary)
					if len(r) > 12 {
						r = r[:12]
					}
					q = string(r)
				}
				add("mem_inject", q, id, "seed:mem_topic")
			}
		}
		rows.Close()
	}

	// Unanswerable traps: random UUID queries must stay silent on every lane.
	for i := 0; i < unanswerable; i++ {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		q := fmt.Sprintf("zz-%x-%x", buf[:4], buf[4:8])
		n++
		out = append(out, GoldenCase{
			Schema: GoldenCaseSchema, ID: fmt.Sprintf("unanswerable-%04d", n),
			Query: q, Lane: "any", Unanswerable: true, Source: "seed:uuid_trap",
		})
	}
	return out, nil
}
