package agent

import (
	"strings"
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
	for _, name := range []string{"workflow_resume", "workflow_list", "workflow_get", "workflow_save", "workflow_schedule_list", "workflow_schedule_create", "workflow_schedule_delete"} {
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

// TestWorkflowScheduleToolsNilStore pins the fail-closed path: with no DBStore
// wired the schedule tools return a clear error instead of panicking — the
// legacy filesystem-only install shape.
func TestWorkflowScheduleToolsNilStore(t *testing.T) {
	a := newAgentForWireTest(t)
	a.SetWorkflowService(workflow.NewService(nil, nil))
	_, err := a.registry.GetFunc("workflow_schedule_create")(t.Context(), []byte(`{"workflow":"x","cron":"30 7 * * *"}`))
	if err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Errorf("create without store: got %v, want store-unavailable error", err)
	}
	_, err = a.registry.GetFunc("workflow_schedule_list")(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Errorf("list without store: got %v, want store-unavailable error", err)
	}
}

// TestWorkflowScheduleCreateBadCron pins arg validation ahead of any store
// access: a non-5-field cron is rejected with the field-count reason.
func TestWorkflowScheduleCreateBadCron(t *testing.T) {
	a := newAgentForWireTest(t)
	a.SetWorkflowService(workflow.NewService(nil, nil))
	_, err := a.registry.GetFunc("workflow_schedule_create")(t.Context(), []byte(`{"workflow":"x","cron":"daily"}`))
	if err == nil || !strings.Contains(err.Error(), "5 space-separated fields") {
		t.Errorf("bad cron: got %v, want field-count error", err)
	}
}
