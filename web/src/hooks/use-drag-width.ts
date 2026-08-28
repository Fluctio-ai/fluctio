"use client";

import { useEffect, useRef, useState } from "react";

// usePersistedDragWidth drives a column splitter: width state clamped to
// [min, max], initialized from (and persisted to) localStorage, with the
// shared mousemove/mouseup plumbing — col-resize cursor + text-selection
// lockout while dragging, save on drag end. `compute` maps a pointer's
// clientX to the width (each splitter anchors its own edge: the files
// panel measures from the viewport's right, the inner tree from the
// panel's left). setWidth is exposed for callers that also adjust the
// width programmatically (e.g. preview auto-width).
export function usePersistedDragWidth({
  storageKey,
  initial,
  min,
  max,
  compute,
}: {
  storageKey: string;
  initial: number;
  min: number;
  max: number;
  compute: (clientX: number) => number;
}) {
  const [width, setWidth] = useState<number>(() => {
    if (typeof window === "undefined") return initial;
    const stored = Number(window.localStorage.getItem(storageKey));
    return Number.isFinite(stored) && stored >= min && stored <= max ? stored : initial;
  });
  const [resizing, setResizing] = useState(false);
  const widthRef = useRef(width);
  useEffect(() => {
    widthRef.current = width;
  }, [width]);
  const computeRef = useRef(compute);
  useEffect(() => {
    computeRef.current = compute;
  }, [compute]);

  useEffect(() => {
    if (!resizing) return;
    const handleMove = (e: MouseEvent) => {
      setWidth(Math.min(max, Math.max(min, computeRef.current(e.clientX))));
    };
    const handleUp = () => {
      setResizing(false);
      try {
        window.localStorage.setItem(storageKey, String(widthRef.current));
      } catch {
        /* ignore quota errors */
      }
    };
    window.addEventListener("mousemove", handleMove);
    window.addEventListener("mouseup", handleUp);
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    return () => {
      window.removeEventListener("mousemove", handleMove);
      window.removeEventListener("mouseup", handleUp);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
    };
  }, [resizing, storageKey, min, max]);

  const startResize = () => setResizing(true);
  return { width, setWidth, resizing, startResize };
}
