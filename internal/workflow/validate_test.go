package workflow_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

func mustParse(t *testing.T, yamlStr string) *workflow.Definition {
	t.Helper()
	def, err := workflow.Parse("test", []byte(yamlStr))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return def
}

// errContains fails when want is not a substring of err's message. A nil err
// never contains anything.
func errContains(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a validation error, got nil")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q missing %q", err.Error(), w)
		}
	}
}

// AC1 — a reference to a node that doesn't exist must fail, naming the bad
// reference and its target node.
func TestValidate_UnknownNodeRef(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - name: a
    kind: llm
    prompt: "use ${ghost.field} here"
`)
	err := workflow.Validate(def, nil)
	errContains(t, err, "ghost")
}

// AC2 — node exists but the referenced field is not in its declared output
// schema. Must fail, naming the node and the field.
func TestValidate_UnknownFieldRef(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - name: src
    kind: tool
    tool: t
    output:
      x: {type: string}
  - name: dst
    kind: llm
    prompt: "${src.missing}"
edges:
  - {from: src, to: dst}
`)
	err := workflow.Validate(def, nil)
	errContains(t, err, "src", "missing")
}

// AC2 corollary — when the upstream node does NOT declare an output schema,
// field references are trusted (the leaf's raw return is parsed at runtime).
// This is what keeps ticket 01's `fetch` node (no output schema) valid.
func TestValidate_FieldRefSkippedWhenNoOutputSchema(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - name: src
    kind: tool
    tool: t
  - name: dst
    kind: llm
    prompt: "${src.anything}"
edges:
  - {from: src, to: dst}
`)
	if err := workflow.Validate(def, nil); err != nil {
		t.Errorf("expected OK (no output schema → field check skipped), got: %v", err)
	}
}

// AC3 — input must satisfy the declared input schema (required + property types).
func TestValidate_InputSchema(t *testing.T) {
	def := mustParse(t, `
version: 1
input:
  schema:
    type: object
    required: [topic]
    properties:
      topic: {type: string}
nodes:
  - name: n
    kind: llm
`)
	// missing required field
	if err := workflow.Validate(def, map[string]any{}); err == nil {
		t.Error("expected failure for missing required field topic")
	}
	// wrong type
	if err := workflow.Validate(def, map[string]any{"topic": 123}); err == nil {
		t.Error("expected failure for wrong type (number vs string)")
	}
	// valid
	if err := workflow.Validate(def, map[string]any{"topic": "cats"}); err != nil {
		t.Errorf("valid input rejected: %v", err)
	}
}

// AC4 — parse → marshal → parse is semantically equal (DeepEqual on the
// re-parsed definition). Guards the 09 UI editor's round-trip invariant.
func TestRoundTrip(t *testing.T) {
	def := mustParse(t, linearYAML)
	out, err := workflow.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	def2, err := workflow.Parse(def.ID, out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !reflect.DeepEqual(def, def2) {
		t.Errorf("round-trip not equal:\ndef:  %#v\ndef2: %#v", def, def2)
	}
}

// AC5 — ticket 01's happy path still validates clean (the validation layer
// doesn't break the minimal flow).
func TestValidate_01HappyPath(t *testing.T) {
	def := mustParse(t, linearYAML)
	if err := workflow.Validate(def, map[string]any{"topic": "cats"}); err != nil {
		t.Errorf("01 happy path rejected by Validate: %v", err)
	}
}

// Scope: a cyclic graph is rejected.
func TestValidate_Cycle(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: a, kind: llm}
  - {name: b, kind: llm}
edges:
  - {from: a, to: b}
  - {from: b, to: a}
`)
	err := workflow.Validate(def, nil)
	errContains(t, err, "cycle")
}

// Scope: exactly one entry node (in-degree 0).
func TestValidate_MultipleEntries(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: a, kind: llm}
  - {name: b, kind: llm}
  - {name: c, kind: llm}
edges:
  - {from: a, to: c}
  - {from: b, to: c}
`)
	err := workflow.Validate(def, nil)
	errContains(t, err, "entry")
}

// Scope: an llm_route edge needs a sibling default edge on the same source.
// (Branch *execution* is ticket 04; only the structural rule lives here.)
func TestValidate_LLMRouteNeedsDefault(t *testing.T) {
	noDefault := mustParse(t, `
version: 1
nodes:
  - name: router
    kind: llm
    output: {choice: {type: string}}
  - {name: a, kind: tool, tool: ta}
  - {name: b, kind: tool, tool: tb}
edges:
  - {from: router, to: a, route: llm_route}
  - {from: router, to: b, route: llm_route}
`)
	errContains(t, workflow.Validate(noDefault, nil), "default", "router")

	withDefault := mustParse(t, `
version: 1
nodes:
  - name: router
    kind: llm
    output: {choice: {type: string}}
  - {name: a, kind: tool, tool: ta}
  - {name: b, kind: tool, tool: tb}
edges:
  - {from: router, to: a, route: llm_route}
  - {from: router, to: b, route: default}
`)
	if err := workflow.Validate(withDefault, nil); err != nil {
		t.Errorf("llm_route with default should validate, got: %v", err)
	}
}

// AC4 corner case — explicit empty maps (`output: {}`, `schema: {}`) must
// round-trip equal. Without nil/empty-map normalization this trips
// DeepEqual on the re-parsed definition.
func TestRoundTrip_EmptyMaps(t *testing.T) {
	def := mustParse(t, `
version: 1
input:
  schema: {}
nodes:
  - name: n
    kind: llm
    output: {}
`)
	out, err := workflow.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	def2, err := workflow.Parse(def.ID, out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !reflect.DeepEqual(def, def2) {
		t.Errorf("round-trip with empty maps not equal:\ndef:  %#v\ndef2: %#v", def, def2)
	}
}

// Decision 4 — references buried in nested tool input (maps/lists) are still
// statically validated, not just top-level string values.
func TestValidate_NestedInputRef(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - name: a
    kind: tool
    tool: t
    output: {x: {type: string}}
  - name: b
    kind: tool
    tool: t2
    input:
      opts:
        nested: "${ghost.field}"
edges:
  - {from: a, to: b}
`)
	errContains(t, workflow.Validate(def, nil), "ghost")
}
