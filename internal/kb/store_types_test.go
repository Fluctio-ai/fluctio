package kb

import (
	"context"
	"testing"
	"time"
)

// TestSaveFlashTodoCRUD walks the full content-type lifecycle: flash/todo
// ingest with type stamped, status defaulting/validation, todo status flow via
// UpdateTodo, ErrTodoNotFound on a miss, and that flashes never leak into
// ListTodos (type='todo' scoped).
func TestSaveFlashTodoCRUD(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	ctx := context.Background()
	const agent = "agt_crud"

	flashID, err := store.SaveFlash(ctx, agent, "第一个灵感\n后面还有细节")
	if err != nil {
		t.Fatalf("SaveFlash: %v", err)
	}

	end := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	todoID, err := store.SaveTodo(ctx, agent, "写周报", "", "", end)
	if err != nil {
		t.Fatalf("SaveTodo: %v", err)
	}
	if _, err := store.SaveTodo(ctx, agent, "x", "bogus", "", ""); err == nil {
		t.Fatal("expected error on invalid status")
	}

	// ListSources surfaces the new type column.
	srcs, err := store.ListSources(ctx, agent, 50, 0)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	got := map[string]string{}
	for _, s := range srcs {
		got[s.ID] = s.Type
	}
	if got[flashID] != "flash" {
		t.Errorf("flash type = %q, want flash", got[flashID])
	}
	if got[todoID] != "todo" {
		t.Errorf("todo type = %q, want todo", got[todoID])
	}

	// ListTodos is todo-only and defaults to pending.
	todos, err := store.ListTodos(ctx, agent, "", 0)
	if err != nil {
		t.Fatalf("ListTodos: %v", err)
	}
	if len(todos) != 1 || todos[0].ID != todoID {
		t.Fatalf("ListTodos = %+v, want 1 todo %s", todos, todoID)
	}
	if todos[0].Status != "pending" {
		t.Errorf("status = %q, want pending", todos[0].Status)
	}
	if todos[0].EndAt == nil {
		t.Error("EndAt nil, want set")
	}
	if todos[0].Title != "写周报" {
		t.Errorf("title = %q, want 写周报", todos[0].Title)
	}

	// pending → in_progress shows up under "active".
	if err := store.UpdateTodo(ctx, agent, todoID, "in_progress", "", ""); err != nil {
		t.Fatalf("UpdateTodo in_progress: %v", err)
	}
	active, _ := store.ListTodos(ctx, agent, "active", 0)
	if len(active) != 1 || active[0].Status != "in_progress" {
		t.Fatalf("active after in_progress = %+v", active)
	}

	// done drops out of the active working set.
	if err := store.UpdateTodo(ctx, agent, todoID, "done", "", ""); err != nil {
		t.Fatalf("UpdateTodo done: %v", err)
	}
	if active2, _ := store.ListTodos(ctx, agent, "active", 0); len(active2) != 0 {
		t.Errorf("active after done = %d, want 0", len(active2))
	}

	// wrong id / foreign type → ErrTodoNotFound.
	if err := store.UpdateTodo(ctx, agent, "nope", "done", "", ""); err != ErrTodoNotFound {
		t.Errorf("wrong id err = %v, want ErrTodoNotFound", err)
	}
	if err := store.UpdateTodo(ctx, agent, flashID, "done", "", ""); err != ErrTodoNotFound {
		t.Errorf("flash-as-todo err = %v, want ErrTodoNotFound", err)
	}
}

// TestListTodosDueWithin checks the reminder-window filter: a todo due inside
// the horizon is returned, one due beyond it is not; window=0 returns both.
func TestListTodosDueWithin(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	ctx := context.Background()
	const agent = "agt_due"

	soonID, err := store.SaveTodo(ctx, agent, "soon", "pending", "",
		time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("SaveTodo soon: %v", err)
	}
	if _, err := store.SaveTodo(ctx, agent, "far", "pending", "",
		time.Now().UTC().Add(72*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("SaveTodo far: %v", err)
	}

	due, _ := store.ListTodos(ctx, agent, "pending", 48)
	if len(due) != 1 || due[0].ID != soonID {
		t.Errorf("dueWithin 48h = %d (%+v), want 1 [%s]", len(due), due, soonID)
	}
	all, _ := store.ListTodos(ctx, agent, "pending", 0)
	if len(all) != 2 {
		t.Errorf("no-window pending = %d, want 2", len(all))
	}
}
