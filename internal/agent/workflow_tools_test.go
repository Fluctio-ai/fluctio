package agent

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// TestSetWorkflowServiceEmptyRegistersAuthoringTools pins the fresh-agent
// contract: a service with zero definitions must still register the workflow
// authoring tools. loadAgentWorkflows builds exactly such a service when the
// agent has no workflow YAMLs yet — gating these tools on "has at least one
// workflow" made the first workflow uncreatable from chat, because no tool
// existed to create it.
func TestSetWorkflowServiceEmptyRegistersAuthoringTools(t *testing.T) {
	a := newAgentForWireTest(t)
	a.SetWorkflowService(workflow.NewService(nil, nil))
	for _, name := range []string{"workflow_resume", "workflow_list", "workflow_get", "workflow_save"} {
		if a.registry.GetFunc(name) == nil {
			t.Errorf("%s must be registered even with zero workflow definitions", name)
		}
	}
}

// TestSetWorkflowServiceNilIsNoOp pins the legacy path: nil leaves the agent
// with no workflow tooling at all.
func TestSetWorkflowServiceNilIsNoOp(t *testing.T) {
	a := newAgentForWireTest(t)
	a.SetWorkflowService(nil)
	if a.registry.GetFunc("workflow_save") != nil {
		t.Error("nil service must not register workflow tools")
	}
}
