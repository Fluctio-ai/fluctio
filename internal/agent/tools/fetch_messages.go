package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// MessageFetcher is the subset of *store.DBStore fetch_messages needs.
type MessageFetcher interface {
	// ListSessionMessagesBySeq returns messages whose seq falls in any of
	// the supplied [start,end] ranges for one (owner, agent, session),
	// ascending by seq. Fluctio is single-user: an agent's memory is
	// shared across its chatters, so chatter_user_id is not a filter here.
	// Empty slice when no rows match.
	ListSessionMessagesBySeq(ctx context.Context, userID, agentID, sessionKey string, ranges [][2]int) ([]store.SessionMessage, error)
}

// fetchMessagesArgs is the JSON schema for fetch_messages.
type fetchMessagesArgs struct {
	SessionKey string `json:"session_key"`
	// Segments is a list of [seq_start, seq_end] pairs from a
	// memory_search result pointer. A topic in an interleaved
	// conversation often covers several disjoint ranges; pass them all
	// and fetch_messages returns the union, in seq order.
	Segments [][2]int `json:"segments"`
	// Legacy single-range fields — still accepted so older pointers
	// (and hand-written calls) work. Ignored when Segments is non-empty.
	SeqStart int `json:"seq_start"`
	SeqEnd   int `json:"seq_end"`
}

// RegisterFetchMessages registers the fetch_messages tool. The LLM gets
// (session_key, segments) pointers from memory_search results and calls
// this tool to retrieve the verbatim original messages of a topic.
//
// The fetcher is wired via SetMessageFetcher on the Registry after Agent
// construction (same pattern as SetSummarySearcher).
func RegisterFetchMessages(r *Registry) {
	r.Register("fetch_messages",
		"Retrieve verbatim original messages of one topic from a past conversation session, "+
			"given a session_key and the segments pointer returned by memory_search. "+
			"Call this after memory_search to read the exact conversation a summary refers to.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"session_key": map[string]interface{}{
					"type":        "string",
					"description": "Session key from a memory_search result pointer",
				},
				"segments": map[string]interface{}{
					"type":        "array",
					"description": "List of [seq_start, seq_end] pairs from a memory_search result. Pass every pair the pointer shows — a topic often spans several disjoint ranges.",
					"items": map[string]interface{}{
						"type":    "array",
						"items":   map[string]interface{}{"type": "integer"},
						"minItems": 2,
						"maxItems": 2,
					},
				},
				"seq_start": map[string]interface{}{
					"type":        "integer",
					"description": "Legacy single-range pointer start. Ignored when segments is set.",
				},
				"seq_end": map[string]interface{}{
					"type":        "integer",
					"description": "Legacy single-range pointer end. Ignored when segments is set.",
				},
			},
			"required": []string{"session_key"},
		}, makeFetchMessages(r))
}

func makeFetchMessages(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args fetchMessagesArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		if args.SessionKey == "" {
			return "", fmt.Errorf("session_key is required")
		}

		ranges := normalizeSegments(args.Segments, args.SeqStart, args.SeqEnd)
		if len(ranges) == 0 {
			return "", fmt.Errorf("segments (or seq_start/seq_end) are required")
		}

		// Scope by owner (user_id = agent owner) + agent + session.
		// Fluctio is single-user: an agent's memory is shared across all
		// its chatters, so fetch_messages and memory_search both scope by
		// agent only — a summary surfaced for one chatter must be fetchable
		// regardless of which chatter originally produced it.
		ownerID := r.userID
		agentID := r.agentID
		if ownerID == "" || agentID == "" {
			return "", fmt.Errorf("fetch_messages requires a chat context")
		}

		if r.msgFetcher == nil {
			return "", fmt.Errorf("fetch_messages not available: store not wired")
		}

		msgs, err := r.msgFetcher.ListSessionMessagesBySeq(ctx, ownerID, agentID, args.SessionKey, ranges)
		if err != nil {
			return "", fmt.Errorf("fetch messages: %w", err)
		}
		if len(msgs) == 0 {
			return fmt.Sprintf("No messages found for session %q across %d segment range(s). "+
				"The session may have been archived or the segments pointer may be wrong.",
				args.SessionKey, len(ranges)), nil
		}

		return formatFetchedMessages(msgs, args.SessionKey, ranges), nil
	}
}

// normalizeSegments merges the new `segments` field with the legacy
// `seq_start/seq_end` pair, fixes s>e order, drops invalid (negative)
// entries, de-overlaps, and sorts ascending. Returns the effective
// ranges to query. Empty when neither source supplied anything usable.
func normalizeSegments(segs [][2]int, legacyStart, legacyEnd int) [][2]int {
	var out [][2]int
	for _, seg := range segs {
		if seg[0] > seg[1] {
			seg[0], seg[1] = seg[1], seg[0]
		}
		if seg[0] < 0 {
			continue
		}
		out = append(out, seg)
	}
	if len(out) == 0 && legacyStart >= 0 && legacyEnd >= 0 {
		s, e := legacyStart, legacyEnd
		if s > e {
			s, e = e, s
		}
		out = append(out, [2]int{s, e})
	}
	if len(out) <= 1 {
		return out
	}
	// Sort + merge overlapping so the SQL stays tight.
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	merged := []([2]int){out[0]}
	for _, cur := range out[1:] {
		last := &merged[len(merged)-1]
		if cur[0] <= last[1]+1 {
			if cur[1] > last[1] {
				last[1] = cur[1]
			}
			continue
		}
		merged = append(merged, cur)
	}
	return merged
}

// maxFetchMessageBytes caps a single tool response. Past this, the tail
// is truncated with a marker — protects the context window when a
// segment catches a large web_fetch result or similar.
const maxFetchMessageBytes = 12000

func formatFetchedMessages(msgs []store.SessionMessage, sessionKey string, ranges [][2]int) string {
	var sb strings.Builder
	segParts := make([]string, len(ranges))
	for i, rg := range ranges {
		segParts[i] = strconv.Itoa(rg[0]) + "-" + strconv.Itoa(rg[1])
	}
	fmt.Fprintf(&sb, "Session %q, segments %s (%d messages):\n\n",
		sessionKey, strings.Join(segParts, ", "), len(msgs))

	for i, m := range msgs {
		line := fmt.Sprintf("[%d] [%s] %s\n", i+1, m.Role, m.Content)
		// Truncate mid-message if the running total would blow the cap.
		// Keeps fetch_messages useful as a pointer-following tool even
		// when one segment happens to catch a huge blob.
		if sb.Len()+len(line) > maxFetchMessageBytes {
			remaining := maxFetchMessageBytes - sb.Len()
			if remaining > 0 {
				sb.WriteString(line[:remaining])
			}
			fmt.Fprintf(&sb, "\n…[truncated — %d more messages omitted to fit the context window]", len(msgs)-i)
			break
		}
		sb.WriteString(line)
	}
	return sb.String()
}
