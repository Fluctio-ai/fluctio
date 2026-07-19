package agent

import "github.com/fluctio-ai/fluctio/internal/skills"

// pendingSkillNames returns the sorted names of skills currently staged in
// the PENDING approval dir under agentHome (i.e. written by skill_manage or
// the skills learner but not yet activated via `fluctio skill approve`).
//
// Returns an empty (non-nil) slice when there are no pending skills —
// including when the dir does not exist yet (a fresh agent). This makes the
// result safe to length-check without a separate existence probe.
//
// Used by runPostTurn to surface a "N skills pending approval" notice to
// the chatter right after a turn finishes, without forcing them to poll the
// CLI. The names slice rides on a ChatEvent payload so the frontend can
// render the count and surface quick approve/reject actions.
func pendingSkillNames(agentHome string) ([]string, error) {
	entries, err := skills.ListPending(agentHome)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names, nil
}
