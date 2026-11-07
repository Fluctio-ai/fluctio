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
// agent, or null when none is remembered. The stored id is untrusted
// localStorage data (user-editable via devtools) — validate the shape
// before embedding it in a navigation path so a tampered value can't
// produce an unexpected route.
export function lastAgentPath(): string | null {
  const id = lastAgentId();
  if (!id) return null;
  if (!/^[A-Za-z0-9._-]{1,128}$/.test(id)) {
    // Unrecognized shape: drop it so the next rememberAgent overwrites.
    try {
      localStorage.removeItem(KEY);
    } catch {
      /* ignore */
    }
    return null;
  }
  return `/agents/${id}/chat/`;
}
