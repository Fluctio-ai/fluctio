package workflow_test

import (
	"context"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// AC 1 + 5 — deterministic `when` expressions: the true branch is taken, the
// false one is skipped, and multiple deterministic edges resolve unambiguously
// (first match in declaration order wins).
func TestBranch_Deterministic(t *testing.T) {
	const def = `
version: 1
nodes:
  - {name: src, kind: tool, tool: t, output: {score: {type: number}}}
  - {name: hi, kind: tool, tool: th}
  - {name: lo, kind: tool, tool: tl}
edges:
  - {from: src, to: hi, when: "${src.score} > 0.8"}
  - {from: src, to: lo, when: "${src.score} <= 0.8"}
`
	// score 0.9 → hi taken, lo skipped.
	tools := &fakeTools{out: map[string]string{"t": `{"score":0.9}`, "th": `{"r":"hi"}`, "tl": `{"r":"lo"}`}}
	res, err := workflow.NewRunner(&fakeLLM{}, tools, newTestStore(t)).Run(context.Background(), mustParse(t, def), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status %s, want succeeded", res.Status)
	}
	if _, ok := res.Snapshot["hi"]; !ok {
		t.Error("hi not reached (score 0.9 > 0.8 should take hi)")
	}
	if _, ok := res.Snapshot["lo"]; ok {
		t.Error("lo reached but should be skipped when hi's when is true")
	}

	// score 0.5 → lo taken, hi skipped (first-true-wins among multiple edges).
	tools2 := &fakeTools{out: map[string]string{"t": `{"score":0.5}`, "th": `{"r":"hi"}`, "tl": `{"r":"lo"}`}}
	res2, err := workflow.NewRunner(&fakeLLM{}, tools2, newTestStore(t)).Run(context.Background(), mustParse(t, def), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res2.Snapshot["lo"]; !ok {
		t.Error("lo not reached (score 0.5 <= 0.8 should take lo)")
	}
	if _, ok := res2.Snapshot["hi"]; ok {
		t.Error("hi reached but its when is false")
	}
}

// AC 2 — llm_route: the LLM picks a candidate edge by name; that branch runs.
func TestBranch_LLMRoute(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: router, kind: tool, tool: t}
  - {name: a, kind: tool, tool: ta}
  - {name: b, kind: tool, tool: tb}
edges:
  - {from: router, to: a, when: llm_route, desc: "go A"}
  - {from: router, to: b, when: llm_route, desc: "go B"}
  - {from: router, to: b, when: default}
`)
	tools := &fakeTools{out: map[string]string{"t": `{}`, "ta": `{}`, "tb": `{}`}}
	llm := &fakeLLM{resp: "a"}
	res, err := workflow.NewRunner(llm, tools, newTestStore(t)).Run(context.Background(), def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status %s, want succeeded", res.Status)
	}
	if _, ok := res.Snapshot["a"]; !ok {
		t.Error("a not reached (LLM chose a)")
	}
	if _, ok := res.Snapshot["b"]; ok {
		t.Error("b reached but should be skipped")
	}
}

// AC 3 — llm_route returns a non-candidate; with a default edge, default is taken.
func TestBranch_LLMRoute_Default(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: router, kind: tool, tool: t}
  - {name: a, kind: tool, tool: ta}
  - {name: b, kind: tool, tool: tb}
edges:
  - {from: router, to: a, when: llm_route, desc: "go A"}
  - {from: router, to: b, when: llm_route, desc: "go B"}
  - {from: router, to: b, when: default}
`)
	tools := &fakeTools{out: map[string]string{"t": `{}`, "ta": `{}`, "tb": `{}`}}
	llm := &fakeLLM{resp: "nonsense"} // non-candidate
	res, err := workflow.NewRunner(llm, tools, newTestStore(t)).Run(context.Background(), def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status %s, want succeeded", res.Status)
	}
	if _, ok := res.Snapshot["b"]; !ok {
		t.Error("default b not reached after non-candidate LLM response")
	}
	if _, ok := res.Snapshot["a"]; ok {
		t.Error("a reached but LLM response was a non-candidate")
	}
}

// AC 4 — llm_route non-candidate AND no default edge → node fails with an error
// pointing at the routing node (spec decision 3: "LLM 路由无 default 则该节点失败"
// is a *runtime* failure, not a design-time validation one).
func TestBranch_LLMRoute_NoMatch_NoDefault(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: router, kind: tool, tool: t}
  - {name: a, kind: tool, tool: ta}
  - {name: b, kind: tool, tool: tb}
edges:
  - {from: router, to: a, when: llm_route, desc: "go A"}
  - {from: router, to: b, when: llm_route, desc: "go B"}
`)
	tools := &fakeTools{out: map[string]string{"t": `{}`}}
	llm := &fakeLLM{resp: "nonsense"}
	res, err := workflow.NewRunner(llm, tools, newTestStore(t)).Run(context.Background(), def, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusFailed {
		t.Errorf("status %s, want failed", res.Status)
	}
	if res.Error == nil || res.Error.Node != "router" {
		t.Errorf("error should point at router: %+v", res.Error)
	}
}

// AC 5 — when multiple deterministic edges are true, the first one in
// declaration order wins (no ambiguous match).
func TestBranch_Deterministic_FirstTrueWins(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: src, kind: tool, tool: t, output: {score: {type: number}}}
  - {name: first, kind: tool, tool: tf}
  - {name: second, kind: tool, tool: ts}
edges:
  - {from: src, to: first, when: "${src.score} > 0.3"}
  - {from: src, to: second, when: "${src.score} > 0.8"}
`)
	// score 0.9 makes BOTH whens true; the declared-first edge (first) must win.
	tools := &fakeTools{out: map[string]string{"t": `{"score":0.9}`, "tf": `{}`, "ts": `{}`}}
	res, err := workflow.NewRunner(&fakeLLM{}, tools, newTestStore(t)).Run(context.Background(), def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status %s, want succeeded", res.Status)
	}
	if _, ok := res.Snapshot["first"]; !ok {
		t.Error("first (declared first) should win when both whens are true")
	}
	if _, ok := res.Snapshot["second"]; ok {
		t.Error("second should not run when first already matched")
	}
}

// AC 6 — `when` combines conditions with && / || (single-level; && binds
// tighter than ||). Backs the editor's structured multi-condition branches so
// a user picks field/operator/value rows instead of hand-writing expressions.
func TestBranch_CondinedExpr(t *testing.T) {
	const def = `
version: 1
nodes:
  - {name: src, kind: tool, tool: t, output: {score: {type: number}, lang: {type: string}}}
  - {name: both, kind: tool, tool: tb}
  - {name: either, kind: tool, tool: te}
edges:
  - {from: src, to: both, when: "${src.score} > 0.8 && ${src.lang} == en"}
  - {from: src, to: either, when: "${src.score} > 0.8 || ${src.lang} == en"}
`
	// score 0.9 + lang en → both conditions true → "both" matches (first edge).
	tools := &fakeTools{out: map[string]string{"t": `{"score":0.9,"lang":"en"}`, "tb": `{}`, "te": `{}`}}
	res, err := workflow.NewRunner(&fakeLLM{}, tools, newTestStore(t)).Run(context.Background(), mustParse(t, def), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status %s, want succeeded", res.Status)
	}
	if _, ok := res.Snapshot["both"]; !ok {
		t.Error("both (&& with both sides true) should match first")
	}

	// score 0.5 + lang en → && false (score) but || true (lang) → either matches.
	tools2 := &fakeTools{out: map[string]string{"t": `{"score":0.5,"lang":"en"}`, "tb": `{}`, "te": `{}`}}
	res2, err := workflow.NewRunner(&fakeLLM{}, tools2, newTestStore(t)).Run(context.Background(), mustParse(t, def), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res2.Snapshot["both"]; ok {
		t.Error("both should NOT match when the && fails on score")
	}
	if _, ok := res2.Snapshot["either"]; !ok {
		t.Error("either (|| with lang==en true) should match")
	}

	// score 0.5 + lang fr → both && false, either || false → no matching edge → run fails.
	tools3 := &fakeTools{out: map[string]string{"t": `{"score":0.5,"lang":"fr"}`, "tb": `{}`, "te": `{}`}}
	res3, err := workflow.NewRunner(&fakeLLM{}, tools3, newTestStore(t)).Run(context.Background(), mustParse(t, def), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Status != workflow.StatusFailed {
		t.Errorf("status %s, want failed (no matching edge)", res3.Status)
	}
}
