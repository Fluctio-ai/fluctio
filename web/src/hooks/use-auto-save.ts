"use client";

import { useCallback, useEffect, useRef, useState } from "react";

// useAutoSave — the settings dialect's auto-save engine. Wrap each field's
// setter with `wrap(...)`: the edit applies locally and a debounced persist
// follows, so rapid edits (keystrokes, slider drags) coalesce into one save.
//
// Behavior contract:
//   - Nothing persists before `loaded` (the initial config fetch). Load
//     paths use the RAW setters, which never schedule a save — so initial
//     load, agent switches and failure rollbacks stay silent without any
//     skip-flag machinery.
//   - While `blocked` (invalid draft, e.g. keyword mode with an empty
//     keyword list) edits stay local — the same contract the old disabled
//     SaveButton enforced. A later valid edit persists everything, since
//     `save` always reads the latest render's state.
//   - On failure the draft rolls back via `reload` (refetch or a local
//     last-good snapshot — raw setters again, so no save loop) and `failed`
//     flags the red status label.
export function useAutoSave({
  loaded,
  blocked = false,
  save,
  reload,
  onSaved,
  debounceMs = 500,
}: {
  /** False until the initial config load completes — no saves before that. */
  loaded: boolean;
  /** True while the draft is invalid: edits stay local until it clears. */
  blocked?: boolean;
  /** Persists the full config; must throw on failure. */
  save: () => Promise<void>;
  /** Restores last-good values after a failed save. */
  reload: () => void | Promise<void>;
  /** Runs after each successful save (e.g. to refresh a last-good snapshot). */
  onSaved?: () => void;
  debounceMs?: number;
}): {
  failed: boolean;
  /** Transient (2s) "saved" flag for the status chip. */
  saved: boolean;
  /** Wrap a state setter so calling it also schedules a persist. */
  wrap: <A extends unknown[]>(fn: (...args: A) => void) => (...args: A) => void;
} {
  const [failed, setFailed] = useState(false);
  const [saved, setSaved] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const flashRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Latest-value refs: the debounced run must see the flags and state of
  // the render that scheduled it, not the mount render. Mirrored in an
  // effect — writing refs during render is unsafe under concurrent React.
  const loadedRef = useRef(loaded);
  const blockedRef = useRef(blocked);
  const saveRef = useRef(save);
  const reloadRef = useRef(reload);
  const onSavedRef = useRef(onSaved);
  useEffect(() => {
    loadedRef.current = loaded;
    blockedRef.current = blocked;
    saveRef.current = save;
    reloadRef.current = reload;
    onSavedRef.current = onSaved;
  });

  useEffect(() => {
    return () => {
      if (timerRef.current) clearTimeout(timerRef.current);
      if (flashRef.current) clearTimeout(flashRef.current);
    };
  }, []);

  const schedule = useCallback(() => {
    if (!loadedRef.current) return;
    setFailed(false);
    if (timerRef.current) clearTimeout(timerRef.current);
    timerRef.current = setTimeout(() => {
      if (blockedRef.current) return; // invalid draft — keep it local
      void (async () => {
        try {
          await saveRef.current();
          onSavedRef.current?.();
          setSaved(true);
          if (flashRef.current) clearTimeout(flashRef.current);
          flashRef.current = setTimeout(() => setSaved(false), 2000);
        } catch {
          setFailed(true);
          try {
            await reloadRef.current();
          } catch {
            // Rollback failed too — keep the error label up; the next
            // edit retries against whatever state is showing.
          }
        }
      })();
    }, debounceMs);
  }, [debounceMs]);

  const wrap = useCallback(
    <A extends unknown[]>(fn: (...args: A) => void) =>
      (...args: A) => {
        fn(...args);
        schedule();
      },
    [schedule],
  );

  return { failed, saved, wrap };
}
