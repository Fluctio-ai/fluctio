package taskqueue

import (
	"context"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
)

// A panicking handler must not take down the queue's goroutine tree:
// executeTask recovers, marks the task failed with the panic value, and
// later tasks on the same chat still process.
func TestQueueRecoversFromHandlerPanic(t *testing.T) {
	var calls int
	q := NewQueue(1, 10*time.Second, func(ctx context.Context, task *Task) (string, error) {
		calls++
		if calls == 1 {
			panic("boom")
		}
		return "ok", nil
	})
	defer q.Stop()

	msg := bus.InboundMessage{ChatID: "chat1"}
	id1 := q.Submit("agt_x", "test:chat1", msg, "")
	id2 := q.Submit("agt_x", "test:chat1", msg, "")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tasks := q.RecentTasks(10)
		byID := map[string]*Task{}
		for _, tk := range tasks {
			byID[tk.ID] = tk
		}
		if t1, t2 := byID[id1], byID[id2]; t1 != nil && t2 != nil &&
			t1.Status == TaskFailed && t2.Status == TaskDone {
			if t1.Error == nil || t1.Error.Error() != "panic: boom" {
				t.Fatalf("failed task error = %v, want panic: boom", t1.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tasks did not settle in time")
}
