package workflow

import (
	"context"
	"sync"
)

// ConcurrencyManager enforces a workflow's concurrency policy across overlapping
// triggers of the SAME workflow (spec decision 13). One manager per agent
// Service — coordination is keyed by workflow id, so different workflows never
// interfere (AC4). A run calls Acquire before Runner.Run and defers the returned
// release.
//
// Modes:
//   - allow (default): no-op — runs overlap freely (AC3).
//   - serial: a per-workflow lock serializes runs — a second Acquire blocks
//     until the first releases, so runs execute one at a time (AC1).
//   - cancel_previous: the prior inflight run's ctx is canceled, this run's
//     cancel becomes the new inflight, and a derived ctx is returned (AC2).
type ConcurrencyManager struct {
	mu       sync.Mutex
	locks    map[string]*sync.Mutex        // per-workflow serial lock (serial mode)
	inflight map[string]*cancelRef         // per-workflow latest run (cancel_previous)
}

// cancelRef pairs a run's cancel func with a unique identity — the pointer
// itself — so a release can tell whether the inflight slot still belongs to
// THIS run (func values aren't comparable, but pointers are).
type cancelRef struct {
	cancel context.CancelFunc
}

// NewConcurrencyManager builds an empty manager.
func NewConcurrencyManager() *ConcurrencyManager {
	return &ConcurrencyManager{
		locks:    map[string]*sync.Mutex{},
		inflight: map[string]*cancelRef{},
	}
}

// Acquire applies mode for workflowID and returns the ctx the run should use
// plus a release func the caller MUST call when the run ends (typically defer).
// For serial it may block; for cancel_previous it may cancel a prior run.
func (m *ConcurrencyManager) Acquire(ctx context.Context, workflowID string, mode Concurrency) (context.Context, func()) {
	switch mode {
	case ConcurrencySerial:
		lk := m.lockFor(workflowID)
		lk.Lock()
		return ctx, func() { lk.Unlock() }
	case ConcurrencyCancelPrevious:
		m.mu.Lock()
		if prev := m.inflight[workflowID]; prev != nil {
			prev.cancel()
		}
		derived, cancel := context.WithCancel(ctx)
		ref := &cancelRef{cancel: cancel}
		m.inflight[workflowID] = ref
		m.mu.Unlock()
		release := func() {
			m.mu.Lock()
			// Only clear the slot if it still points at THIS run — a newer
			// run may already have replaced it.
			if cur, ok := m.inflight[workflowID]; ok && cur == ref {
				delete(m.inflight, workflowID)
			}
			m.mu.Unlock()
			cancel() // idempotent; no-op if the run already ended
		}
		return derived, release
	default: // ConcurrencyAllow
		return ctx, func() {}
	}
}

// lockFor returns the per-workflow serial lock, creating it on first use.
func (m *ConcurrencyManager) lockFor(workflowID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lk, ok := m.locks[workflowID]
	if !ok {
		lk = &sync.Mutex{}
		m.locks[workflowID] = lk
	}
	return lk
}
