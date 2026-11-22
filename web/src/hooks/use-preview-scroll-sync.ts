"use client";

// usePreviewScrollSync — editor → preview scroll sync (one-way) for the
// markdown split editors (notes-view, MarkdownSplitEditor). The editor
// reports its caret line (click/typing) or topmost visible line (scroll);
// the preview aligns to the last stamped block at or above that line.
// Anchors are the data-line attrs stamped after render (stampRunAnchors).
// Preview→editor sync is deliberately absent: it needs re-entrancy locks
// and isn't the workflow ("write here, read there").

import * as React from "react";
import type { EditorView } from "@codemirror/view";

export function usePreviewScrollSync() {
  // The scrollable preview pane; also the query root for [data-line] blocks.
  const previewRef = React.useRef<HTMLDivElement | null>(null);
  // Handed out once by the editor (onEditorReady) so the scroll-sync
  // interpolation can map anchor lines to pixel heights. Type-only import —
  // the EditorView class itself stays inside the lazy editor chunk.
  const editorViewRef = React.useRef<EditorView | null>(null);
  // A caret jump makes CodeMirror scroll the caret line into view, and that
  // induced editor scrolling fires onViewLine for ~0.5s — long enough to
  // override the caret's own alignment with a top-line alignment (observed
  // live). Grace-window those induced events away so a click on a distant
  // line lands the preview on the CLICKED block, not the block the editor's
  // top line happened to show.
  const caretSyncAtRef = React.useRef(0);

  // caret=true (click/typing): only scroll when the target block is OUTSIDE
  // the preview viewport, so writing inside the visible block never jerks
  // the preview. caret=false (editor scroll): always align the topmost
  // block. Both instant — CSS smooth scrolling stalls in headless browsers
  // and an interrupted mid-flight smooth lands the preview between blocks.
  const syncPreviewLine = React.useCallback((line: number, caret: boolean) => {
    const container = previewRef.current;
    // offsetParent is null when the desktop pane is display:none (mobile
    // shows the preview as a full-screen sheet instead — nothing to sync).
    if (!container || container.offsetParent === null) return;
    const blocks = container.querySelectorAll<HTMLElement>("[data-line]");
    if (!blocks.length) return;
    let target = blocks[0];
    for (const el of blocks) {
      if (Number(el.dataset.line) <= line) target = el;
      else break;
    }
    // Measured once per event — loop-invariant for every block below.
    const cRect = container.getBoundingClientRect();
    if (caret) {
      const r = target.getBoundingClientRect();
      if (r.top >= cRect.top - 4 && r.bottom <= cRect.top + container.clientHeight + 4) return;
      // Caret jumps land exactly on the clicked block's top.
      container.scrollTo({ top: Math.max(0, r.top - cRect.top + container.scrollTop - 12), behavior: "auto" });
      return;
    }
    // Editor scroll: ByteMD-style block-interval interpolation so the
    // preview follows CONTINUOUSLY — the top visible line at 60% inside
    // source block i scrolls the preview to 60% inside block i's rendered
    // span — instead of snapping to block tops only at block boundaries.
    // Height tables are rebuilt per event; anchor counts are small (tens)
    // and lineBlockAt is a binary search, so one pass per frame is fine.
    const view = editorViewRef.current;
    if (!view) {
      const top = target.getBoundingClientRect().top - cRect.top + container.scrollTop - 12;
      container.scrollTo({ top: Math.max(0, top), behavior: "auto" });
      return;
    }
    const doc = view.state.doc;
    const editTops: number[] = [];
    const previewTops: number[] = [];
    const cTop = cRect.top;
    const scrollTop = container.scrollTop;
    for (const el of blocks) {
      const ln = Math.min(Number(el.dataset.line), doc.lines);
      editTops.push(view.lineBlockAt(doc.line(ln).from).top);
      previewTops.push(el.getBoundingClientRect().top - cTop + scrollTop);
    }
    const previewRange = container.scrollHeight - container.clientHeight;
    const editTop = view.scrollDOM.scrollTop;
    let i = 0;
    while (i < editTops.length - 1 && editTops[i + 1] <= editTop) i++;
    let previewTop: number;
    if (editTop <= editTops[0] || editTops.length === 1) {
      previewTop = 0;
    } else if (i >= editTops.length - 1) {
      // Past the last anchor (e.g. trailing blank lines): ride to the bottom.
      previewTop = previewRange;
    } else {
      const span = editTops[i + 1] - editTops[i];
      const frac = span > 0 ? (editTop - editTops[i]) / span : 0;
      previewTop = previewTops[i] + frac * (previewTops[i + 1] - previewTops[i]);
    }
    container.scrollTo({ top: Math.max(0, Math.min(previewRange, previewTop - 12)), behavior: "auto" });
  }, []);

  // Stable identities (refs only) — safe to hand straight to the editor.
  const onCursorLine = React.useCallback((l: number) => {
    caretSyncAtRef.current = Date.now();
    syncPreviewLine(l, true);
  }, [syncPreviewLine]);
  const onViewLine = React.useCallback((l: number) => {
    if (Date.now() - caretSyncAtRef.current < 450) return;
    syncPreviewLine(l, false);
  }, [syncPreviewLine]);
  const onEditorReady = React.useCallback((v: EditorView) => {
    editorViewRef.current = v;
  }, []);

  return { previewRef, onCursorLine, onViewLine, onEditorReady };
}
