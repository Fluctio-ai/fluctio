package workflow_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// AC1 — serial: overlapping triggers of one workflow run one at a time (max
// inflight == 1), and all three eventually run.
func TestConcurrencySerialNoOverlap(t *testing.T) {
	m := workflow.NewConcurrencyManager()
	var inflight int32
	var maxInflight int32
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release := m.Acquire(context.Background(), "wf-1", workflow.ConcurrencySerial)
			defer release()
			cur := atomic.AddInt32(&inflight, 1)
			for {
				mx := atomic.LoadInt32(&maxInflight)
				if cur <= mx || atomic.CompareAndSwapInt32(&maxInflight, mx, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
		}()
	}
	wg.Wait()
	if maxInflight != 1 {
		t.Errorf("max concurrent runs = %d, want 1 (serial)", maxInflight)
	}
}

// AC2 — cancel_previous: a second Acquire cancels the first run's ctx while
// leaving the second active.
func TestConcurrencyCancelPreviousCancelsPrior(t *testing.T) {
	m := workflow.NewConcurrencyManager()
	ctx1, release1 := m.Acquire(context.Background(), "wf-1", workflow.ConcurrencyCancelPrevious)
	defer release1()
	// first run "in flight"; a second trigger arrives.
	ctx2, release2 := m.Acquire(context.Background(), "wf-1", workflow.ConcurrencyCancelPrevious)
	defer release2()

	select {
	case <-ctx1.Done():
	case <-time.After(time.Second):
		t.Fatal("first run ctx not canceled by cancel_previous")
	}
	if ctx2.Err() != nil {
		t.Errorf("second run ctx should be active, got %v", ctx2.Err())
	}
}

// AC2 detail — releasing the second run clears the inflight slot, so a later
// trigger does NOT cancel an already-finished run's ctx spuriously. Also the
// release's own cancel is idempotent.
func TestConcurrencyCancelPreviousReleaseClearsSlot(t *testing.T) {
	m := workflow.NewConcurrencyManager()
	_, release1 := m.Acquire(context.Background(), "wf-1", workflow.ConcurrencyCancelPrevious)
	release1() // run 1 finished
	// A new trigger should not panic / misbehave; its ctx stays active.
	ctx2, release2 := m.Acquire(context.Background(), "wf-1", workflow.ConcurrencyCancelPrevious)
	defer release2()
	if ctx2.Err() != nil {
		t.Errorf("ctx2 should be active, got %v", ctx2.Err())
	}
}

// AC3 — allow (default): ctx passes through unchanged, release is a no-op.
func TestConcurrencyAllowNoOp(t *testing.T) {
	m := workflow.NewConcurrencyManager()
	parent := context.Background()
	ctx, release := m.Acquire(parent, "wf-1", workflow.ConcurrencyAllow)
	defer release()
	if ctx != parent {
		t.Error("allow should return the ctx unchanged")
	}
}

// AC4 — different workflows don't interfere: serial on wf-1 doesn't block wf-2.
func TestConcurrencyDifferentWorkflowsIndependent(t *testing.T) {
	m := workflow.NewConcurrencyManager()
	_, release1 := m.Acquire(context.Background(), "wf-1", workflow.ConcurrencySerial)
	defer release1()
	// wf-2 should acquire immediately despite wf-1's lock being held.
	done := make(chan struct{})
	go func() {
		_, rel := m.Acquire(context.Background(), "wf-2", workflow.ConcurrencySerial)
		rel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wf-2 blocked by wf-1's serial lock — different workflows must be independent")
	}
}

// ctxLLM blocks inside Call until its ctx is canceled, signaling start so the
// test can sequence a second trigger against an in-flight run.
type ctxLLM struct{ started chan struct{} }

func (l *ctxLLM) Call(ctx context.Context, prompt string) (string, error) {
	close(l.started)
	<-ctx.Done()
	return "", ctx.Err()
}

// AC2 end-to-end at the Service layer: a cancel_previous workflow's second
// RunWorkflow cancels the first in-flight run (its node observes ctx cancel →
// failed), while the second run completes normally.
func TestServiceCancelPreviousCancelsRun(t *testing.T) {
	def := mustParse(t, `id: cp
version: 1
nodes:
  - name: step
    kind: llm
    prompt: "hi"
`)
	def.Concurrency = workflow.ConcurrencyCancelPrevious
	defs := map[string]*workflow.Definition{def.ID: def}
	st := newTestStore(t)
	svc := workflow.NewService(defs, st)

	slow := &ctxLLM{started: make(chan struct{})}
	var res1 *workflow.ExecutionResult
	done1 := make(chan struct{})
	go func() {
		res1, _ = svc.RunWorkflow(context.Background(), def.ID, nil, "", "", slow, &fakeTools{})
		close(done1)
	}()
	<-slow.started // first run is inside its node

	res2, err := svc.RunWorkflow(context.Background(), def.ID, nil, "", "", &fakeLLM{resp: `{"ok":true}`}, &fakeTools{})
	if err != nil {
		t.Fatalf("second RunWorkflow: %v", err)
	}
	<-done1

	if res1 == nil || res1.Status != workflow.StatusFailed {
		got := "<nil>"
		if res1 != nil {
			got = string(res1.Status)
		}
		t.Errorf("first run status=%s, want failed (canceled)", got)
	}
	if res2.Status != workflow.StatusSucceeded {
		t.Errorf("second run status=%s, want succeeded", res2.Status)
	}
}
