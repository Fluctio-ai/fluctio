// Package diag performs heuristic failure attribution over an agent turn's
// timeline. It localizes the root-cause step (not just the visible error) by
// applying the rules in specs/2026-07-22-heuristic-failure-attribution.md.
// Pure logic — no DB, no LLM — so it's fully unit-testable.
package diag

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// EntryKind classifies one timeline node.
type EntryKind string

const (
	KindLLMCall    EntryKind = "llm"
	KindToolCall   EntryKind = "tool_call"
	KindToolResult EntryKind = "tool_result"
	KindError      EntryKind = "error"
	KindOther      EntryKind = "event"
)

// TimelineEntry is one normalized node merged from llm_call_diag rows and
// session_events rows, sorted by Time.
type TimelineEntry struct {
	Time          time.Time
	Kind          EntryKind
	Status        string // llm status or event type
	Detail        string // tool name / error snippet
	Failed        bool
	FailKind      string // tool failure class (env_missing/permission/external/logic)
	HTTPStatus    int
	ToolCallCount int
	ResponseChars int
}

// RootCause is the attribution verdict for one timeline.
type RootCause struct {
	StepIdx    int      // index into the timeline
	Rule       string   // which rule fired
	Confidence string   // "high" | "medium"
	Summary    string
	Evidence   []string
}

// failKindRe parses the [失败类别: <kind>] marker loop.go writes into failed
// tool_result content (see classifyToolError). Kept local so this package
// doesn't depend on the agent package.
var failKindRe = regexp.MustCompile(`\[失败类别:\s*(\w+)\]`)

// BuildTimeline merges llm_call_diag rows + session_events rows into one
// time-sorted timeline. LLM calls and tool events interleave by created_at,
// which is what makes cascading-failure attribution possible.
func BuildTimeline(llm []store.LLMCallDiagRow, events []store.SessionEventRecord) []TimelineEntry {
	var tl []TimelineEntry
	for _, r := range llm {
		tl = append(tl, TimelineEntry{
			Time:          r.CreatedAt,
			Kind:          KindLLMCall,
			Status:        r.Status,
			Detail:        truncate(r.ErrorMsg, 80),
			Failed:        r.Status != "" && r.Status != "ok",
			HTTPStatus:    r.HTTPStatus,
			ToolCallCount: r.ToolCallCount,
			ResponseChars: r.ResponseChars,
		})
	}
	for _, e := range events {
		tl = append(tl, eventToEntry(e))
	}
	sort.SliceStable(tl, func(i, j int) bool { return tl[i].Time.Before(tl[j].Time) })
	return tl
}

func eventToEntry(e store.SessionEventRecord) TimelineEntry {
	entry := TimelineEntry{Time: e.CreatedAt, Status: e.Type}
	switch e.Type {
	case "tool_call":
		entry.Kind = KindToolCall
		entry.Detail = toolNameFromData(e.Data)
	case "tool_result":
		entry.Kind = KindToolResult
		body := string(e.Data)
		if m := failKindRe.FindStringSubmatch(body); len(m) == 2 {
			entry.Failed = true
			entry.FailKind = m[1]
		}
		entry.Detail = truncate(body, 80)
	case "error":
		entry.Kind = KindError
		entry.Failed = true
		entry.Detail = truncate(string(e.Data), 80)
	default:
		entry.Kind = KindOther
	}
	return entry
}

// Attribute applies the heuristic rules (specs §4) and returns the best
// root-cause guess. ok=false when no rule fires (no detectable failure).
//
// Order matters: rule 1 (LLM call failed) is highest confidence and most
// unambiguous, so it wins even if a tool also failed upstream — the LLM
// failure is the direct cause. Then empty-response, then cascading tool
// failure, then tool loop.
func Attribute(tl []TimelineEntry) (RootCause, bool) {
	if len(tl) == 0 {
		return RootCause{}, false
	}
	// Rule 1: an LLM call itself failed.
	for i := len(tl) - 1; i >= 0; i-- {
		e := tl[i]
		if e.Kind == KindLLMCall && e.Failed {
			return RootCause{
				StepIdx:    i,
				Rule:       "llm-call-failed",
				Confidence: "high",
				Summary:    fmt.Sprintf("LLM call failed (status=%s, http=%d)", e.Status, e.HTTPStatus),
				Evidence:   []string{fmt.Sprintf("step %d: %s", i, orDash(e.Detail))},
			}, true
		}
	}
	// Rule 2: a failed tool_result followed by further progress downstream
	// (the cascading-failure signature — bad tool data propagated). Checked
	// before empty-response: an upstream tool failure is the deeper root
	// cause of a downstream empty/early-stop LLM call.
	lastErrorIdx := -1
	for i := len(tl) - 1; i >= 0; i-- {
		if tl[i].Kind == KindError {
			lastErrorIdx = i
			break
		}
	}
	for i, e := range tl {
		if e.Kind != KindToolResult || !e.Failed {
			continue
		}
		hasDownstream := false
		for j := i + 1; j < len(tl); j++ {
			if tl[j].Kind == KindLLMCall || (lastErrorIdx >= 0 && j == lastErrorIdx) {
				hasDownstream = true
				break
			}
		}
		if hasDownstream {
			return RootCause{
				StepIdx:    i,
				Rule:       "tool-failure-before-visible-error",
				Confidence: "medium",
				Summary:    fmt.Sprintf("tool returned failed/partial data (%s) before the visible error", orDash(e.FailKind)),
				Evidence: []string{
					fmt.Sprintf("step %d: tool_result failed [%s]", i, orDash(e.FailKind)),
					"downstream steps proceeded on unvalidated data",
				},
			}, true
		}
	}
	// Rule 4: empty response / early stop (LLM returned nothing, no tool call).
	for i := len(tl) - 1; i >= 0; i-- {
		e := tl[i]
		if e.Kind == KindLLMCall && e.ResponseChars == 0 && e.ToolCallCount == 0 {
			return RootCause{
				StepIdx:    i,
				Rule:       "empty-response",
				Confidence: "medium",
				Summary:    "LLM returned empty content with no tool call (early stop)",
				Evidence:   []string{fmt.Sprintf("step %d: response_chars=0 tool_calls=0", i)},
			}, true
		}
	}
	// Rule 3: same tool called repeatedly (agent stuck in a loop, hit max iters).
	if name, idx, n := repeatedTool(tl); n >= 3 {
		return RootCause{
			StepIdx:    idx,
			Rule:       "tool-loop",
			Confidence: "medium",
			Summary:    fmt.Sprintf("tool %q called %d times (possible loop / max-iterations)", name, n),
			Evidence:   []string{fmt.Sprintf("first occurrence at step %d", idx)},
		}, true
	}
	return RootCause{}, false
}

func repeatedTool(tl []TimelineEntry) (name string, firstIdx int, count int) {
	counts := map[string]int{}
	first := map[string]int{}
	for i, e := range tl {
		if e.Kind != KindToolCall || e.Detail == "" {
			continue
		}
		counts[e.Detail]++
		if _, ok := first[e.Detail]; !ok {
			first[e.Detail] = i
		}
	}
	for n, c := range counts {
		if c > count {
			name, count, firstIdx = n, c, first[n]
		}
	}
	return
}

// RenderReport formats the timeline + root cause as the human/code-readable
// report from spec §6.
func RenderReport(agentID, sessionKey string, tl []TimelineEntry, rc RootCause, ok bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session: %s  agent: %s\n", sessionKey, agentID)
	fmt.Fprintf(&b, "timeline: %d steps\n", len(tl))
	for i, e := range tl {
		mark := ""
		if ok && i == rc.StepIdx {
			mark = "  <- suspect"
		}
		fmt.Fprintf(&b, "  %s  %-11s %-12s %s%s\n",
			e.Time.Format("15:04:05"), e.Kind, e.Status, truncate(orDash(e.Detail), 50), mark)
	}
	if !ok {
		fmt.Fprintln(&b, "ROOT CAUSE: no detectable failure (all steps ok)")
		return b.String()
	}
	fmt.Fprintf(&b, "ROOT CAUSE (confidence: %s):\n", rc.Confidence)
	fmt.Fprintf(&b, "  step %d - %s\n", rc.StepIdx, rc.Summary)
	fmt.Fprintf(&b, "  rule: %s\n", rc.Rule)
	for _, ev := range rc.Evidence {
		fmt.Fprintf(&b, "  evidence: %s\n", ev)
	}
	return b.String()
}

// toolNameFromData pulls a best-effort tool name out of a tool_call event's
// JSON data. Falls back to a raw snippet — it's only a display hint.
func toolNameFromData(data []byte) string {
	s := string(data)
	for _, key := range []string{`"name"`, `"tool"`, `"function"`} {
		i := strings.Index(s, key)
		if i < 0 {
			continue
		}
		rest := s[i+len(key):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		rest = rest[j+1:]
		k := strings.Index(rest, `"`)
		if k >= 0 {
			return rest[:k]
		}
	}
	return truncate(s, 30)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
