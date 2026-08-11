// last-agent.ts — persists the most recently visited agent id in localStorage
// so a returning logged-in user can land on that agent directly instead of
// the overview, skipping the manual agent switch every visit.

const KEY = "fluctio-last-agent";

export function rememberAgent(id: string): void {
  try {
    localStorage.setItem(KEY, id);
  } catch {
    /* ignore quota / private-mode errors */
  }
}

export function lastAgentId(): string | null {
  try {
    const v = localStorage.getItem(KEY);
    return v && v.length > 0 ? v : null;
  } catch {
    return null;
  }
}

// lastAgentPath returns the /agents/<id>/chat/ path of the most recently used
// agent, or null when none is remembered. Callers fall back to /overview/.
export function lastAgentPath(): string | null {
  const id = lastAgentId();
  return id ? `/agents/${id}/chat/` : null;
}
