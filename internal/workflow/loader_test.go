package workflow_test

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// Minimal Seam-B-adjacent coverage: Parse maps the YAML subset defined by the
// tracer bullet (version / input.schema / nodes / edges) into Definition.
// Full validation + round-trip is ticket 02's Seam B.
func TestLoader_Parse(t *testing.T) {
	def, err := workflow.Parse("hello", []byte(linearYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if def.ID != "hello" {
		t.Errorf("ID = %q, want hello", def.ID)
	}
	if def.Version != 1 {
		t.Errorf("Version = %d, want 1", def.Version)
	}
	if len(def.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(def.Nodes))
	}
	if def.Nodes[0].Name != "fetch" || def.Nodes[0].Kind != "tool" || def.Nodes[0].Tool != "get_data" {
		t.Errorf("node[0] = %+v, want fetch/tool/get_data", def.Nodes[0])
	}
	if def.Nodes[1].Name != "summarize" || def.Nodes[1].Kind != "llm" {
		t.Errorf("node[1] = %+v, want summarize/llm", def.Nodes[1])
	}
	if def.Nodes[1].Prompt == "" {
		t.Error("node[1].prompt empty")
	}
	if len(def.Edges) != 1 || def.Edges[0].From != "fetch" || def.Edges[0].To != "summarize" {
		t.Errorf("edges = %+v, want [fetch→summarize]", def.Edges)
	}
}
