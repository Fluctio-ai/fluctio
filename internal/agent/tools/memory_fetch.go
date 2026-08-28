package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

)

type memoryFetchArgs struct {
	IDs []int64 `json:"ids"`
}

// RegisterMemoryFetch registers the memory_fetch tool — the detail layer
// of layered memory injection. When memory_search surfaces more than
// memoryCompactThreshold hits it returns a compact id+topic index; this
// tool pulls the full summaries (plus the session_key/segments pointers
// for fetch_messages) for only the ids the LLM actually needs.
//
// A successful fetch also marks this turn's recall events consumed —
// following up on recalled ids is the recall.consumed adoption signal.
func RegisterMemoryFetch(r *Registry) {
	r.Register("memory_fetch",
		"Fetch full conversation-summary memories by id. memory_search returns a compact id+topic index when it finds many matches; call this with the ids you actually need to read the full summary, keywords, and the (session_key, segments) pointer for fetch_messages.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ids": map[string]interface{}{
					"type":        "array",
					"description": "Summary ids from the memory_search compact index. Only request the ones you will use.",
					"items":       map[string]interface{}{"type": "integer"},
				},
			},
			"required": []string{"ids"},
		}, makeMemoryFetch(r))
}

func makeMemoryFetch(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args memoryFetchArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if len(args.IDs) == 0 {
			return "", fmt.Errorf("ids is required")
		}
		if len(args.IDs) > memorySearchMaxLimit {
			return "", fmt.Errorf("too many ids (%d); fetch at most %d", len(args.IDs), memorySearchMaxLimit)
		}
		if r.vecDB == nil {
			return "", fmt.Errorf("memory_fetch not available: store not wired")
		}

		// Active-only fetch: the agent + superseded predicates live in
		// the store (GetActiveSummariesByIDs) — ids are LLM input
		// and must not leak other agents' memories or stale state.
		scoped, err := r.vecDB.GetActiveSummariesByIDs(ctx, r.agentID, args.IDs)
		if err != nil {
			return "", fmt.Errorf("fetch memories: %w", err)
		}
		if len(scoped) == 0 {
			return "No memories found for the requested ids (wrong agent scope or superseded).", nil
		}

		// recall.consumed: the LLM pulled detail on recalled ids — that
		// recall was adopted, not just surfaced. Best-effort.
		flushRecallConsumed(ctx, r)

		var sb strings.Builder
		fmt.Fprintf(&sb, "Fetched %d memories:\n\n", len(scoped))
		for i, h := range scoped {
			fmt.Fprintf(&sb, "--- Memory %d (session=%s, time=%s) ---\n",
				i+1, h.SessionKey, h.CreatedAt.Format("2006-01-02 15:04"))
			if h.Topic != "" {
				fmt.Fprintf(&sb, "Topic: %s\n", h.Topic)
			}
			sb.WriteString(h.Summary)
			if len(h.Keywords) > 0 {
				sb.WriteString("\n\nKeywords: ")
				sb.WriteString(strings.Join(h.Keywords, ", "))
			}
			fmt.Fprintf(&sb, "\n\n[session_key=%s segments=%s]\n\n",
				h.SessionKey, formatSegmentsPointer(h.Segments, h.SeqStart, h.SeqEnd))
		}
		sb.WriteString("To retrieve the verbatim original messages of any memory above, call:\n")
		sb.WriteString("  fetch_messages(session_key=<value>, segments=<value>)\n")
		return sb.String(), nil
	}
}
