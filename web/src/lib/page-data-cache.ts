// Module-level snapshot cache, keyed by string. Survives route
// unmount/remount so flipping between /knowledge/ and /wiki/ paints the
// stale snapshot instantly instead of showing the loading spinner — the
// page revalidates in the background and silently swaps in fresh data.
//
// No TTL and no invalidation API: these are cheap list reads that are
// always re-fetched on mount. The cache only buys instant paint on
// repeat visits, never correctness (stale data shows for at most one
// render before the background fetch lands).
const store = new Map<string, unknown>();

export function readCache<T>(key: string): T | undefined {
  return store.get(key) as T | undefined;
}

export function writeCache<T>(key: string, data: T): void {
  store.set(key, data);
}
