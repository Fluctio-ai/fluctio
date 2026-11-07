package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// agentConfigMu serializes load→mutate→save cycles on the agents.config
// column (im-claim bind/unbind/rebind, and any other caller of
// loadAgentFileConfig + saveAgentFileConfig). The save rewrites the whole
// row from the caller's snapshot, so two concurrent claim edits would
// otherwise lose each other's changes (a freshly-claimed admin can be
// wiped by a concurrent unbind).
var agentConfigMu sync.Mutex

// loadAgentFileConfig returns an agent's per-row override JSON from the
// agents.config column.
func (s *Server) loadAgentFileConfig(r *http.Request, agentID string) (*config.AgentFileConfig, error) {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &config.AgentFileConfig{}, nil
		}
		return nil, err
	}
	cfg := &config.AgentFileConfig{}
	if len(rec.Config) > 0 {
		blob, _ := json.Marshal(rec.Config)
		_ = json.Unmarshal(blob, cfg)
	}
	return cfg, nil
}

// mutateAgentFileConfig runs load → mutate → save under agentConfigMu so
// concurrent editors of one agent's config row can't clobber each other
// with stale snapshots. The mutator returns an error to abort the save.
func (s *Server) mutateAgentFileConfig(r *http.Request, agentID string, mutate func(cfg *config.AgentFileConfig) error) error {
	agentConfigMu.Lock()
	defer agentConfigMu.Unlock()
	cfg, err := s.loadAgentFileConfig(r, agentID)
	if err != nil {
		return err
	}
	if err := mutate(cfg); err != nil {
		return err
	}
	return s.saveAgentFileConfig(r, agentID, cfg)
}

// saveAgentFileConfig persists per-agent overrides into agents.config.
// Callers that build on a loaded snapshot should prefer mutateAgentFileConfig.
func (s *Server) saveAgentFileConfig(r *http.Request, agentID string, cfg *config.AgentFileConfig) error {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			rec = &store.AgentRecord{ID: agentID, UserID: s.effectiveUserID(r), Name: agentID}
		} else {
			return err
		}
	}
	blob, _ := json.Marshal(cfg)
	var asMap map[string]interface{}
	if err := json.Unmarshal(blob, &asMap); err != nil {
		return err
	}
	rec.Config = asMap
	rec.UpdatedAt = time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = rec.UpdatedAt
	}
	return s.dataStore.SaveAgent(r.Context(), rec)
}

// isStoreNotFound recognises the "not found" signal across backends.
func isStoreNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no rows in result set") || strings.Contains(msg, "not found")
}

var _ = context.Background
