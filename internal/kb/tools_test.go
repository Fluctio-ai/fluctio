package kb

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
)

// TestRegisterKBToolsIncludesFlashTodo verifies the stage-3 wiring: the four
// new content-type tools land in the registry alongside the existing KB tools.
// Guards against the dead-wiring trap (register func defined but never called
// from RegisterKBTools). sourceRatioFn/thresholdFn can be nil — the new tools
// never read them.
func TestRegisterKBToolsIncludesFlashTodo(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	r := tools.NewRegistry("", "")
	RegisterKBTools(r, store, "agt_test", nil, nil)

	for _, name := range []string{
		"knowledgebase_save_flash",
		"knowledgebase_save_todo",
		"knowledgebase_update_todo",
		"knowledgebase_list_todos",
		"knowledgebase_search", // existing — sanity check
	} {
		if !r.HasBuiltin(name) {
			t.Errorf("tool %q not registered", name)
		}
	}
}
