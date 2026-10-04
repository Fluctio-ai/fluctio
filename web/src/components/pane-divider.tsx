"use client";

// PaneDivider + usePaneResize — the shared left-pane resize control for
// the knowledge master-detail views (articles, cards, diary). The hook
// drives the --pane-lw CSS variable on the pane element it is handed:
// during the drag it writes the variable directly (no per-move re-render
// of the whole view), and pointerup commits the final width into state so
// the next render agrees with the DOM. Pointer listeners attach to the
// document so the drag keeps tracking when the cursor leaves the thin
// handle.

import { useCallback, useRef, useState } from "react";
import type { PointerEvent as ReactPointerEvent, RefObject } from "react";
import { cn } from "@/lib/utils";

export function usePaneResize(
  paneRef: RefObject<HTMLElement | null>,
  initial: number,
  min: number,
  max: number,
) {
  const [width, setWidth] = useState(initial);
  const startWidth = useRef(initial);
  const startDrag = useCallback(
    (e: ReactPointerEvent) => {
      e.preventDefault();
      const startX = e.clientX;
      const clamp = (w: number) => Math.min(max, Math.max(min, w));
      startWidth.current = width;
      const move = (ev: PointerEvent) => {
        paneRef.current?.style.setProperty("--pane-lw", `${clamp(startWidth.current + ev.clientX - startX)}px`);
      };
      const up = (ev: PointerEvent) => {
        document.removeEventListener("pointermove", move);
        document.removeEventListener("pointerup", up);
        document.body.style.cursor = "";
        document.body.style.userSelect = "";
        setWidth(clamp(startWidth.current + ev.clientX - startX));
      };
      document.addEventListener("pointermove", move);
      document.addEventListener("pointerup", up);
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";
    },
    [paneRef, width, min, max],
  );
  return { width, startDrag };
}

// PaneDivider is the thin drag handle between the list pane and the detail
// pane; desktop only — mobile uses the stacked master-detail.
export function PaneDivider({
  onPointerDown,
  className,
}: {
  onPointerDown: (e: ReactPointerEvent) => void;
  className?: string;
}) {
  return (
    <div
      onPointerDown={onPointerDown}
      className={cn("hidden md:block w-1 shrink-0 cursor-col-resize hover:bg-primary/40 transition-colors", className)}
    />
  );
}
