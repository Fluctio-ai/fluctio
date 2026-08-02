package kb

import (
	"context"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/provider"
)

// TestRegisterKBToolsIncludesFlashTodo verifies the content-type tools land in
// the registry alongside the existing KB tools. Guards against the dead-wiring
// trap. sourceRatioFn/thresholdFn/insightInvoker can be nil — the new tools
// never read them, and a nil invoker simply leaves generate_insights off.
func TestRegisterKBToolsIncludesFlashTodo(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	r := tools.NewRegistry("", "")
	RegisterKBTools(r, store, "agt_test", nil, nil, nil, "", 0)

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
	if r.HasBuiltin("knowledgebase_generate_insights") {
		t.Errorf("generate_insights must NOT register when invoker is nil")
	}
}

// TestRegisterKBToolsInsightGenerator verifies the deep-reading tool registers
// when an invoker is supplied.
func TestRegisterKBToolsInsightGenerator(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	r := tools.NewRegistry("", "")
	invoker := InsightInvoker(func(ctx context.Context, messages []provider.Message) (string, error) {
		return "{}", nil
	})
	RegisterKBTools(r, store, "agt_test", nil, nil, invoker, "test-model", 1024)
	if !r.HasBuiltin("knowledgebase_generate_insights") {
		t.Errorf("generate_insights tool not registered when invoker provided")
	}
}
