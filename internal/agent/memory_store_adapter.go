package agent

import (
	"context"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// MemoryStoreAdapter exposes the agent's identity + memory files via the
// underlying store.
//
// NOTE: agent_files was flattened to one row per (agent, filename), so
// the per-user overlay (caller's row vs owner's row) is gone and every
// method here resolves to a single agent-scoped row. The userID parameter
// is retained on each method only for interface compatibility —
// MemoryStore (memory.go) and SystemFileStore (tools/registry.go) still
// thread a chatter userID through to support the not-yet-removed
// per-chatter Memory rebind. It's received as `_` to signal "unused
// here"; Phase 2 task 2.2 deletes it from both interfaces and then the
// parameter disappears for real.
type MemoryStoreAdapter struct {
	st store.Store
}

func NewMemoryStoreAdapter(st store.Store) *MemoryStoreAdapter {
	return &MemoryStoreAdapter{st: st}
}

const memoryFilename = "MEMORY.md"

func (a *MemoryStoreAdapter) GetMemory(ctx context.Context, agentID, _ string) (string, error) {
	data, err := a.st.GetAgentFile(ctx, agentID, memoryFilename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (a *MemoryStoreAdapter) SaveMemory(ctx context.Context, agentID, _ string, content string) error {
	return a.st.SaveAgentFile(ctx, agentID, memoryFilename, []byte(content))
}

func (a *MemoryStoreAdapter) GetWorkspaceFile(ctx context.Context, agentID, _ string, filename string) ([]byte, error) {
	return a.st.GetAgentFile(ctx, agentID, filename)
}

// GetWorkspaceFileExact previously bypassed the owner-fallback overlay to
// read only the caller's row. Post-flatten there's a single row, so it's
// equivalent to GetWorkspaceFile; the method stays for interface compat.
func (a *MemoryStoreAdapter) GetWorkspaceFileExact(ctx context.Context, agentID, _ string, filename string) ([]byte, error) {
	return a.st.GetAgentFile(ctx, agentID, filename)
}

func (a *MemoryStoreAdapter) SaveWorkspaceFile(ctx context.Context, agentID, _ string, filename string, data []byte) error {
	return a.st.SaveAgentFile(ctx, agentID, filename, data)
}
