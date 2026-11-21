"use client";

// MarkdownSplitEditor — the notes-grade markdown editing experience for
// plain markdown (no whiteboards/attachments): the CodeMirror source pane
// (lazy chunk) + a live ChatMarkdown preview with editor→preview scroll
// sync. Desktop splits side-by-side; on mobile the preview hides behind a
// floating eye button that opens a full-screen sheet. Extracted from
// notes-view.tsx for the flash two-pane rebuild; notes keeps its own copy
// because its preview interleaves whiteboard cards.

import * as React from "react";
import dynamic from "next/dynamic";
import { EyeIcon, XIcon } from "lucide-react";
import type { EditorView } from "@codemirror/view";
import { useT } from "@/lib/i18n";
import { ChatMarkdown } from "@/components/chat-markdown";

// The CodeMirror chunk loads on demand (notes-view precedent): browsing
// without an open editor never pays for it.
const MarkdownEditor = dynamic(
  () => import("@/components/markdown-codemirror").then((m) => m.MarkdownCodeMirror),
  { ssr: false },
);

// ── preview anchor stamping (editor→preview scroll sync) ──
// Streamdown parses each markdown block as an independent parse run, so no
// plugin can recover global source lines (every block's AST position starts
// at 1). Instead we stamp AFTER render: match each rendered top-level
// element to the first source line whose marker-stripped text it renders.
// A two-pointer scan in source order; elements that match nothing (hr,
// image-only paragraphs, math/mermaid chrome) simply carry no anchor and
// sync falls through to the previous stamped block.

// Block prefixes that vanish in rendered text: ATX heading, bullet/ordered
// list marker (incl. task checkbox), quote marker, table row pipes.
const BLOCK_PREFIX_RE = /^(?:#{1,6}\s+|[-*+]\s+(?:\[[ xX]\]\s*)?|\d+[.)]\s+|>\s*|\|)/;
// Inline markup that survives to the DOM only as rendered text: links/
// images keep their label, emphasis/code-span marks drop out.
function stripMdMarkers(line: string): string {
  let s = line
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/[*_~`]+/g, "")
    .trim();
  let prev = "";
  while (s !== prev) {
    prev = s;
    s = s.replace(BLOCK_PREFIX_RE, "");
  }
  return s;
}
// Alphanumeric runs only (CJK counts): whitespace/punctuation differences
// between source and render can't break the prefix compare.
const wordChars = (s: string) => s.match(/[\p{L}\p{N}]+/gu)?.join("") ?? "";

function stampRunAnchors(run: HTMLElement, text: string, startLine: number) {
  // Streamdown wraps all blocks in one container div (.space-y-4); the
  // block elements are that wrapper's children. Unwrap one level when we
  // see it (sole child with multiple children); otherwise stamp run's own
  // children — a lone block then lands on its wrapper, still positionally
  // correct.
  let host: HTMLElement = run;
  if (run.children.length === 1) {
    const only = run.children[0];
    if (only instanceof HTMLElement && only.children.length > 1) host = only;
  }
  for (const el of host.children) if (el instanceof HTMLElement) delete el.dataset.line;
  const lines = text.split("\n");
  let cursor = 0;
  for (const el of host.children) {
    if (!(el instanceof HTMLElement)) continue;
    // Code blocks: the <code> body is the verbatim source (the fence line
    // has no word chars, so the first code line matches). Everything else:
    // leading text is enough — lists/quotes render their items in order.
    const src = el.querySelector("code")?.textContent ?? el.textContent ?? "";
    const want = wordChars(src);
    if (!want) continue;
    for (; cursor < lines.length; cursor++) {
      const have = wordChars(stripMdMarkers(lines[cursor]));
      if (have && want.startsWith(have)) {
        el.dataset.line = String(startLine + cursor);
        cursor++;
        break;
      }
    }
  }
}

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
  const [previewOpen, setPreviewOpen] = React.useState(false);

  // ── editor → preview scroll sync (one-way) ──
  // The editor reports its caret line (click/typing → smooth) or topmost
  // visible line (scroll → instant); we align the preview to the last
  // stamped block at or above that line. Anchors are the data-line attrs
  // stamped after render (stampRunAnchors) — 1-based global lines.
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
  const syncPreviewLine = (line: number, caret: boolean) => {
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
    if (caret) {
      const r = target.getBoundingClientRect();
      const c = container.getBoundingClientRect();
      if (r.top >= c.top - 4 && r.bottom <= c.top + container.clientHeight + 4) return;
      // Caret jumps land exactly on the clicked block's top.
      const top =
        target.getBoundingClientRect().top - container.getBoundingClientRect().top + container.scrollTop - 12;
      container.scrollTo({ top: Math.max(0, top), behavior: "auto" });
      return;
    }
    // Editor scroll: ByteMD-style block-interval interpolation so the
    // preview follows CONTINUOUSLY — the top visible line at 60% inside
    // source block i scrolls the preview to 60% inside block i's rendered
    // span — instead of snapping to block tops only at block boundaries.
    const view = editorViewRef.current;
    if (!view) {
      const top =
        target.getBoundingClientRect().top - container.getBoundingClientRect().top + container.scrollTop - 12;
      container.scrollTo({ top: Math.max(0, top), behavior: "auto" });
      return;
    }
    const doc = view.state.doc;
    const editTops: number[] = [];
    const previewTops: number[] = [];
    for (const el of blocks) {
      const ln = Math.min(Number(el.dataset.line), doc.lines);
      editTops.push(view.lineBlockAt(doc.line(ln).from).top);
      previewTops.push(
        el.getBoundingClientRect().top - container.getBoundingClientRect().top + container.scrollTop
      );
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
  };

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
  }, [value, resetKey]);

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
            onCursorLine={(l) => {
              caretSyncAtRef.current = Date.now();
              syncPreviewLine(l, true);
            }}
            onViewLine={(l) => {
              if (Date.now() - caretSyncAtRef.current < 450) return;
              syncPreviewLine(l, false);
            }}
            onEditorReady={(v) => {
              editorViewRef.current = v;
            }}
          />
        </div>
      </div>
      {/* mobile: floating preview entry, chat-file-preview shaped */}
      <button
        type="button"
        onClick={() => setPreviewOpen(true)}
        className="absolute right-3 top-3 z-10 rounded-full border bg-background/80 p-2 text-muted-foreground shadow-sm backdrop-blur transition-colors hover:bg-muted hover:text-foreground md:hidden"
        aria-label={t("knowledge.notes.preview")}
      >
        <EyeIcon className="size-4" />
      </button>
      {/* mobile full-screen rendered preview (same sheet shape as the chat
          file viewer) */}
      {previewOpen && (
        <div
          className="fixed inset-0 z-50 flex flex-col bg-background md:hidden"
          style={{
            paddingTop: "env(safe-area-inset-top)",
            paddingBottom: "env(safe-area-inset-bottom)",
          }}
        >
          <div className="flex h-12 shrink-0 items-center justify-between gap-2 border-b border-border px-4">
            <span className="min-w-0 truncate text-sm font-medium">
              {t("knowledge.notes.preview")}
            </span>
            <button
              type="button"
              onClick={() => setPreviewOpen(false)}
              className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
              aria-label={t("preview.close")}
            >
              <XIcon className="h-4 w-4" />
            </button>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto p-4">{previewBody}</div>
        </div>
      )}
    </div>
  );
}
