package diag

import (
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

func mustAttribute(t *testing.T, tl []TimelineEntry, wantRule string) RootCause {
	t.Helper()
	rc, ok := Attribute(tl)
	if !ok {
		t.Fatalf("Attribute returned ok=false; want rule %q", wantRule)
	}
	if rc.Rule != wantRule {
		t.Fatalf("rule=%q want %q (rc=%+v)", rc.Rule, wantRule, rc)
	}
	return rc
}

// Rule 1: an LLM call that failed is the root cause, high confidence.
func TestAttributeLLMCallFailed(t *testing.T) {
	base := time.Now()
	tl := []TimelineEntry{
		{Time: base, Kind: KindLLMCall, Status: "ok", ToolCallCount: 1},
		{Time: base.Add(time.Second), Kind: KindToolCall, Detail: "search"},
		{Time: base.Add(2 * time.Second), Kind: KindLLMCall, Status: "error", Failed: true, HTTPStatus: 500, Detail: "boom"},
	}
	rc := mustAttribute(t, tl, "llm-call-failed")
	if rc.Confidence != "high" || rc.StepIdx != 2 {
		t.Errorf("confidence=%s step=%d want high/2", rc.Confidence, rc.StepIdx)
	}
}

// Rule 2: a failed tool_result with downstream progress (cascading failure).
func TestAttributeToolFailureCascades(t *testing.T) {
	base := time.Now()
	tl := []TimelineEntry{
		{Time: base, Kind: KindLLMCall, Status: "ok", ToolCallCount: 1},
		{Time: base.Add(time.Second), Kind: KindToolResult, Failed: true, FailKind: "external"},
		{Time: base.Add(2 * time.Second), Kind: KindLLMCall, Status: "ok"}, // downstream proceeded
		{Time: base.Add(3 * time.Second), Kind: KindError, Failed: true},
	}
	rc := mustAttribute(t, tl, "tool-failure-before-visible-error")
	if rc.StepIdx != 1 {
		t.Errorf("StepIdx=%d want 1 (the failed tool_result)", rc.StepIdx)
	}
}

// Rule 4: empty response / early stop.
func TestAttributeEmptyResponse(t *testing.T) {
	base := time.Now()
	tl := []TimelineEntry{
		{Time: base, Kind: KindLLMCall, Status: "ok", ToolCallCount: 1, ResponseChars: 100},
		{Time: base.Add(time.Second), Kind: KindLLMCall, Status: "ok", ResponseChars: 0, ToolCallCount: 0},
	}
	rc := mustAttribute(t, tl, "empty-response")
	if rc.StepIdx != 1 {
		t.Errorf("StepIdx=%d want 1", rc.StepIdx)
	}
}

// Rule 3: same tool called 3+ times (loop / max-iterations).
func TestAttributeToolLoop(t *testing.T) {
	base := time.Now()
	tl := []TimelineEntry{
		{Time: base, Kind: KindToolCall, Detail: "search"},
		{Time: base.Add(time.Second), Kind: KindToolCall, Detail: "search"},
		{Time: base.Add(2 * time.Second), Kind: KindToolCall, Detail: "search"},
	}
	rc := mustAttribute(t, tl, "tool-loop")
	if rc.StepIdx != 0 {
		t.Errorf("StepIdx=%d want 0 (first occurrence)", rc.StepIdx)
	}
}

// A healthy timeline yields no root cause.
func TestAttributeNoFailure(t *testing.T) {
	base := time.Now()
	tl := []TimelineEntry{
		{Time: base, Kind: KindLLMCall, Status: "ok", ResponseChars: 50},
		{Time: base.Add(time.Second), Kind: KindToolCall, Detail: "ok-tool"},
	}
	if _, ok := Attribute(tl); ok {
		t.Fatal("Attribute returned ok=true on an all-healthy timeline")
	}
}

// Rule 1 beats rule 2: when an LLM call fails AND a tool failed upstream, the
// LLM failure is the direct cause and wins.
func TestAttributeLLMFailureBeatsToolFailure(t *testing.T) {
	base := time.Now()
	tl := []TimelineEntry{
		{Time: base, Kind: KindToolResult, Failed: true, FailKind: "external"},
		{Time: base.Add(time.Second), Kind: KindLLMCall, Status: "error", Failed: true, HTTPStatus: 500},
	}
	rc := mustAttribute(t, tl, "llm-call-failed")
	if rc.StepIdx != 1 {
		t.Errorf("StepIdx=%d want 1 (the LLM call, not the tool)", rc.StepIdx)
	}
}

// BuildTimeline merges llm_call_diag + session_events, sorts ascending, and
// parses the [失败类别: x] marker on tool_results.
func TestBuildTimelineMergesAndSorts(t *testing.T) {
	base := time.Now()
	llm := []store.LLMCallDiagRow{
		{CreatedAt: base.Add(2 * time.Second), Status: "error", HTTPStatus: 500, ErrorMsg: "boom"},
		{CreatedAt: base, Status: "ok", ToolCallCount: 1, ResponseChars: 10},
	}
	events := []store.SessionEventRecord{
		{CreatedAt: base.Add(time.Second), Type: "tool_result", Data: []byte(`{"x":1} [失败类别: external]`)},
		{CreatedAt: base.Add(3 * time.Second), Type: "done"},
	}
	tl := BuildTimeline(llm, events)
	if len(tl) != 4 {
		t.Fatalf("len=%d want 4", len(tl))
	}
	for i := 1; i < len(tl); i++ {
		if tl[i].Time.Before(tl[i-1].Time) {
			t.Errorf("not sorted ascending at %d", i)
		}
	}
	// tool_result with the failure marker parsed as failed.
	foundFailedTool := false
	foundFailedLLM := false
	for i := range tl {
		if tl[i].Kind == KindToolResult && tl[i].Failed && tl[i].FailKind == "external" {
			foundFailedTool = true
		}
		if tl[i].Kind == KindLLMCall && tl[i].Status == "error" && tl[i].Failed {
			foundFailedLLM = true
		}
	}
	if !foundFailedTool {
		t.Errorf("tool_result failure marker not parsed; timeline=%+v", tl)
	}
	if !foundFailedLLM {
		t.Errorf("error llm row not flagged failed; timeline=%+v", tl)
	}
}
