"use client";

// MarkdownSplitEditor — the notes-grade markdown editing experience for
// plain markdown (no whiteboards/attachments): the CodeMirror source pane
// (lazy chunk) + a live ChatMarkdown preview with editor→preview scroll
// sync. Desktop splits side-by-side; on mobile the preview hides behind a
// floating eye button that opens a full-screen sheet. Extracted from
// notes-view.tsx for the flash two-pane rebuild; notes keeps its own
// preview composition because it interleaves whiteboard cards — the
// shared machinery (anchor stamping, scroll sync, mobile sheet) lives in
// lib/markdown-sync.ts, hooks/use-preview-scroll-sync.ts and
// components/mobile-preview-sheet.tsx.

import * as React from "react";
import dynamic from "next/dynamic";
import { useT } from "@/lib/i18n";
import { stampRunAnchors } from "@/lib/markdown-sync";
import { usePreviewScrollSync } from "@/hooks/use-preview-scroll-sync";
import { ChatMarkdown } from "@/components/chat-markdown";
import { MobilePreviewSheet } from "@/components/mobile-preview-sheet";

// The CodeMirror chunk loads on demand (notes-view precedent): browsing
// without an open editor never pays for it.
const MarkdownEditor = dynamic(
  () => import("@/components/markdown-codemirror").then((m) => m.MarkdownCodeMirror),
  { ssr: false },
);

export function MarkdownSplitEditor({
  value,
  onChange,
  placeholder,
  resetKey,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  // Remount the preview when the subject switches (notes' key trick:
  // reusing the ChatMarkdown instance lets Streamdown's block memo serve
  // the previous document's first block).
  resetKey?: string | null;
}) {
  const t = useT();
  const { previewRef, onCursorLine, onViewLine, onEditorReady } = usePreviewScrollSync();

  // Stamp the anchors after the preview DOM commits. A MutationObserver
  // re-stamps on ANY subtree change, not just [value] flips: Streamdown
  // commits new blocks through startTransition (its DOM lands AFTER this
  // effect ran), and Shiki highlighting / math resolve replace content
  // asynchronously. data-line attribute writes are not observed (no
  // attributes filter), so stamping never re-triggers itself.
  // rAF-coalesced because highlight tokenization can fire many mutations
  // in one frame.
  React.useEffect(() => {
    const container = previewRef.current;
    if (!container) return;
    const stamp = () => {
      const run = container.querySelector<HTMLElement>(".chat-md");
      if (run && value.trim()) stampRunAnchors(run, value, 1);
    };
    stamp();
    let raf = 0;
    const mo = new MutationObserver(() => {
      if (raf) return;
      raf = requestAnimationFrame(() => {
        raf = 0;
        stamp();
      });
    });
    mo.observe(container, { childList: true, subtree: true, characterData: true });
    return () => {
      mo.disconnect();
      if (raf) cancelAnimationFrame(raf);
    };
  }, [value, resetKey, previewRef]);

  // Rendered body shared by the desktop split pane and the mobile preview
  // sheet — one source of truth so both stay in sync with the draft.
  const previewBody = value.trim() ? (
    <ChatMarkdown key={resetKey ?? "md"} text={value} />
  ) : (
    <p className="text-xs text-muted-foreground">{t("knowledge.notes.previewEmpty")}</p>
  );

  return (
    <div className="relative h-full min-h-0">
      {/* desktop: source + live preview side by side; mobile: source only,
          preview behind the floating eye */}
      <div className="flex h-full flex-col md:flex-row-reverse">
        <div ref={previewRef} className="hidden min-w-0 flex-1 overflow-y-auto p-4 md:block md:h-full">
          {previewBody}
        </div>
        <div className="h-full min-h-0 w-full shrink-0 border-t md:w-1/2 md:border-r md:border-t-0">
          <MarkdownEditor
            value={value}
            onChange={onChange}
            placeholder={placeholder}
            onCursorLine={onCursorLine}
            onViewLine={onViewLine}
            onEditorReady={onEditorReady}
          />
        </div>
      </div>
      <MobilePreviewSheet title={t("knowledge.notes.preview")}>{previewBody}</MobilePreviewSheet>
    </div>
  );
}
