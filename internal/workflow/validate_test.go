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

// JSON decodes every number to float64; an integer-typed input field must
// accept a float64 with an integral value (7.0 == 7). Found via the HTTP e2e
// path, where every integer input was rejected before the fix.
func TestValidate_InputIntegerJSON(t *testing.T) {
	def := mustParse(t, `
version: 1
input:
  schema:
    type: object
    required: [n]
    properties:
      n: {type: integer}
nodes:
  - name: r
    kind: reply
    prompt: "n=${input.n}"
`)
	if err := workflow.Validate(def, map[string]any{"n": float64(7)}); err != nil {
		t.Errorf("float64(7) rejected for integer field: %v", err)
	}
	if err := workflow.Validate(def, map[string]any{"n": 7}); err != nil {
		t.Errorf("int 7 rejected for integer field: %v", err)
	}
	if err := workflow.Validate(def, map[string]any{"n": float64(7.5)}); err == nil {
		t.Error("7.5 accepted for integer field, want rejected")
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

// AC4 corollary — explicit empty maps (`output: {}`, `schema: {}`) must
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

// A code node must carry a code body in a supported language (spec decision 3).
func TestValidate_CodeNode(t *testing.T) {
	// missing code body
	def := mustParse(t, `
version: 1
nodes:
  - name: c
    kind: code
    lang: python
`)
	errContains(t, workflow.Validate(def, nil), "c", "code")

	// unsupported language
	def2 := mustParse(t, `
version: 1
nodes:
  - name: c
    kind: code
    lang: ruby
    code: "puts 1"
`)
	errContains(t, workflow.Validate(def2, nil), "ruby")

	// valid code node (default language python)
	def3 := mustParse(t, `
version: 1
nodes:
  - name: c
    kind: code
    code: "print(1)"
`)
	if err := workflow.Validate(def3, nil); err != nil {
		t.Errorf("valid code node rejected: %v", err)
	}
}

// An unknown node kind is rejected at design time.
func TestValidate_UnknownKind(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - name: c
    kind: codd
    code: "print(1)"
`)
	errContains(t, workflow.Validate(def, nil), "codd")
}

// The workflow-level output map's ${...} references resolve against the same
// graph + input schema as node-level refs (spec decision 4).
func TestValidate_OutputMapRefs(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - name: src
    kind: tool
    tool: t
    output: {x: {type: string}}
output:
  leaked: ${ghost.field}
`)
	errContains(t, workflow.Validate(def, nil), "ghost")
}

// A reply node (M3 domain node) must carry a reply template in Prompt.
func TestValidate_ReplyNode(t *testing.T) {
	// missing prompt → rejected
	def := mustParse(t, `
version: 1
nodes:
  - name: r
    kind: reply
`)
	errContains(t, workflow.Validate(def, nil), "r", "reply template")

	// valid reply node with a prompt resolves clean.
	def2 := mustParse(t, `
version: 1
nodes:
  - name: r
    kind: reply
    prompt: "Hello world"
`)
	if err := workflow.Validate(def2, nil); err != nil {
		t.Errorf("valid reply node rejected: %v", err)
	}
}

// A question_rewrite node (M3) must carry a prompt.
func TestValidate_QuestionRewriteNode(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - name: rw
    kind: question_rewrite
`)
	errContains(t, workflow.Validate(def, nil), "rw", "question_rewrite")
}

// An http node (M3) must declare input.url.
func TestValidate_HTTPNode(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - name: h
    kind: http
    input:
      method: POST
`)
	errContains(t, workflow.Validate(def, nil), "h", "url")
}
