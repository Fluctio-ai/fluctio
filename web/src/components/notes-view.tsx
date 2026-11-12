"use client";

// NotesView — 笔记: markdown 正文 + 白板块（```whiteboard fence，弹窗编辑
// Excalidraw、exportToSvg 内联渲染）+ 附件。布局沿用 ArticleView 的
// master-detail DNA：bg-muted/30 左列表、可拖宽分隔条（220–520px）、
// 轻量行式列表（最新在上，updated_at 排序）、mobile 返回切换。白板块
// 可在预览区拖拽换位。正文 1.2s 防抖自动保存。

import * as React from "react";
import dynamic from "next/dynamic";
import {
  ArrowLeftIcon,
  ChevronRightIcon,
  DownloadIcon,
  EyeIcon,
  FileIcon,
  GripVerticalIcon,
  ImagePlusIcon,
  PaperclipIcon,
  PenLineIcon,
  PlusIcon,
  Trash2Icon,
  XIcon,
} from "lucide-react";
import { useT } from "@/lib/i18n";

// sanitizeSvgHtml strips script-execution vectors from an exported
// Excalidraw SVG string before it goes through dangerouslySetInnerHTML:
// removes <script> and <foreignObject> subtrees, event-handler
// attributes (on*), and javascript:/data: hrefs. The SVG comes from the
// local exportToSvg of user-editable whiteboard JSON — normally benign,
// but hand-edited fences (the JSON is a plain markdown code block) make
// this the injection boundary.
export function sanitizeSvgHtml(html: string): string {
  try {
    const doc = new DOMParser().parseFromString(html, "image/svg+xml");
    if (doc.querySelector("parsererror")) return "";
    const root = doc.documentElement;
    for (const el of Array.from(root.querySelectorAll("script, foreignObject, iframe, object, embed"))) {
      el.remove();
    }
    for (const el of Array.from(root.querySelectorAll("*"))) {
      for (const attr of Array.from(el.attributes)) {
        const name = attr.name.toLowerCase();
        if (name.startsWith("on")) {
          el.attributes.removeNamedItem(attr.name);
          continue;
        }
        const v = attr.value.trim().toLowerCase();
        if ((name === "href" || name === "xlink:href") && /^(javascript|data|vbscript):/.test(v)) {
          el.attributes.removeNamedItem(attr.name);
        }
      }
    }
    return root.outerHTML;
  } catch {
    return "";
  }
}
import type { EditorView } from "@codemirror/view";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import {
  deleteNote,
  deleteNoteAttachment,
  fileUrl,
  listNoteAttachments,
  listNotes,
  saveNote,
  uploadNoteAttachments,
  type KBNote,
  type KBNoteAttachment,
} from "@/lib/api";
import { readCache, writeCache } from "@/lib/page-data-cache";
import { ConfirmDeleteDialog } from "@/components/confirm-delete-dialog";
import { cn } from "@/lib/utils";
import { ChatMarkdown } from "@/components/chat-markdown";
import { Whiteboard } from "@/components/whiteboard";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { SearchInput } from "@/components/ui/search-input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";

// The CodeMirror markdown editor chunk loads on demand (Whiteboard
// precedent): browsing the notes list never pulls it.
const MarkdownEditor = dynamic(
  () => import("@/components/markdown-codemirror").then((m) => m.MarkdownCodeMirror),
  { ssr: false },
);

type SaveState = "saved" | "saving" | "dirty" | "idle";

// EMPTY_BOARD is the canonical scene of a brand-new whiteboard. The fence
// inserted by "插入白板" starts as exactly this string, so the Whiteboard's
// mount-time emission compares equal and is skipped (no phantom save).
const EMPTY_BOARD = JSON.stringify({ type: "excalidraw", version: 2, elements: [], files: {} });

const BOARD_FENCE_RE = /(^|\n)(```whiteboard\n[\s\S]*?\n```)(?=\n|$)/g;

type BodyPart =
  | { kind: "md"; text: string; startLine: number }
  | { kind: "board"; json: string; line: number };

// splitBody partitions the markdown body into md runs and whiteboard
// fences so the preview can render boards as interactive cards between
// the surrounding text. Fence content is preserved verbatim, which keeps
// round-tripping (preview → edit → textarea) lossless.
//
// startLine/line are 1-based GLOBAL line numbers (within the whole note),
// fed to ChatMarkdown's lineAnchor / BoardCard's data-line so the preview
// blocks can be scroll-synced to the editor's caret line. Advancement rule:
// an md run always ends with the \n that precedes a fence (the regex's m[1]
// goes to the run), so the run spans lines(run)−1 line breaks and the next
// part starts at run.startLine + lines(run) − 1; a fence does NOT include
// its trailing \n, so the next part starts at fence.line + lines(fence).
function splitBody(md: string): BodyPart[] {
  const parts: BodyPart[] = [];
  let last = 0;
  let line = 1;
  for (const m of md.matchAll(BOARD_FENCE_RE)) {
    const fence = m[2];
    const start = (m.index ?? 0) + (m[1] ? m[1].length : 0);
    if (start > last) {
      const text = md.slice(last, start);
      parts.push({ kind: "md", text, startLine: line });
      line += text.split("\n").length - 1;
    }
    parts.push({ kind: "board", json: fence.slice("```whiteboard\n".length, -"```".length).trim(), line });
    line += fence.split("\n").length;
    last = start + fence.length;
  }
  if (last < md.length) parts.push({ kind: "md", text: md.slice(last), startLine: line });
  return parts;
}

function boardElementCount(json: string): number {
  try { return JSON.parse(json)?.elements?.length ?? 0; } catch { return 0; }
}

function boardSlots(md: string): number[] {
  return splitBody(md).reduce<number[]>((acc, p, i) => (p.kind === "board" ? [...acc, i] : acc), []);
}

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

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function noteTitle(n: KBNote, untitled: string): string {
  return n.title || n.content_md.split("\n").map((l) => l.trim()).find(Boolean) || untitled;
}

function noteTime(iso: string): string {
  const d = new Date(iso);
  return `${d.toLocaleDateString()} ${d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
}

export function NotesView({ notify }: { notify: (msg: string) => void }) {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [notes, setNotes] = React.useState<KBNote[]>(() =>
    agentId ? (readCache<KBNote[]>(`kb-notes:${agentId}`) ?? []) : [],
  );
  const [loading, setLoading] = React.useState(() => !readCache<KBNote[]>(`kb-notes:${agentId ?? ""}`));
  const [selectedId, setSelectedId] = React.useState<string | null>(null);
  // Mirror of selectedId for async callbacks: autosave/attachment responses
  // must not write another note's data into the currently-viewed note when
  // the user switched mid-flight.
  const selectedIdRef = React.useRef<string | null>(selectedId);
  React.useEffect(() => {
    selectedIdRef.current = selectedId;
  }, [selectedId]);
  // editorActive tracks "an editor is open" separately from the draft's
  // emptiness: a brand-new note starts with an empty draft, and gating the
  // editor on content would bounce the user straight back to the empty
  // state (the first-click "新建笔记 does nothing" bug). On mobile it also
  // drives the ArticleView master-detail swap.
  const [editorActive, setEditorActive] = React.useState(false);
  const [draft, setDraft] = React.useState({ title: "", content_md: "" });
  const [saveState, setSaveState] = React.useState<SaveState>("saved");
  const [attachments, setAttachments] = React.useState<KBNoteAttachment[]>([]);
  const [uploading, setUploading] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [confirmDelete, setConfirmDelete] = React.useState(false);
  const [deleteTarget, setDeleteTarget] = React.useState<KBNote | null>(null);
  // boardDialog indexes into splitBody(draft.content_md)'s board slots;
  // -1 = closed.
  const [boardDialog, setBoardDialog] = React.useState(-1);
  // Mobile only: the rendered preview opens on demand behind a floating eye
  // button (chat-file-preview sheet shape) instead of a stacked pane, so the
  // md source owns the screen. Desktop keeps the side-by-side split.
  const [previewOpen, setPreviewOpen] = React.useState(false);
  // Resizable left pane — mirrors ArticleView's divider (220–520px).
  const [leftWidth, setLeftWidth] = React.useState(320);
  // Note-list ordering is server-side newest-first (updated_at DESC) —
  // the old drag-to-reorder was removed; sort_order is legacy.
  // Board-card drag: source slot + live drop indicator. The drag LOGIC
  // (which slot moved where) travels through a ref + event params — the
  // React state (dragBoardSlot/boardDropAt) is presentation-only for the
  // ghost opacity and the drop line. Reading state inside the drop
  // handler lost races: dragover's setState hadn't committed when the
  // drop closure was invoked, so the drop silently no-op'd.
  const [dragBoardSlot, setDragBoardSlot] = React.useState<number | null>(null);
  const [boardDropAt, setBoardDropAt] = React.useState<{ slot: number; before: boolean } | null>(null);
  const dragBoardSlotRef = React.useRef<number | null>(null);
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const [dragOver, setDragOver] = React.useState(false);
  // Autosave suppression flag: set true whenever the draft is swapped in
  // wholesale (select/new/delete), so the swap itself doesn't count as an
  // edit and trigger a save.
  const skipSaveRef = React.useRef(true);

  const load = React.useCallback(async () => {
    if (!agentId) return;
    const cached = readCache<KBNote[]>(`kb-notes:${agentId}`);
    if (!cached) setLoading(true);
    try {
      const list = await listNotes(agentId);
      setNotes(list);
      writeCache(`kb-notes:${agentId}`, list);
    } catch { /* offline: keep cache */ }
    if (!cached) setLoading(false);
  }, [agentId]);

  React.useEffect(() => { load(); }, [load]);

  const select = React.useCallback((n: KBNote) => {
    setSelectedId(n.id);
    setEditorActive(true);
    setDraft({ title: n.title, content_md: n.content_md });
    setSaveState("saved");
    setConfirmDelete(false);
    skipSaveRef.current = true; // loading a note into the draft isn't an edit
  }, []);

  const backToList = React.useCallback(() => {
    setEditorActive(false);
    setSelectedId(null);
    skipSaveRef.current = true;
    setDraft({ title: "", content_md: "" });
    setPreviewOpen(false);
  }, []);

  React.useEffect(() => {
    if (!selectedId || !agentId) { setAttachments([]); return; }
    listNoteAttachments(agentId, selectedId).then(setAttachments).catch(() => {});
  }, [selectedId, agentId]);

  // Autosave: draft edits settle for 1.2s, then save. skipSaveRef
  // suppresses the first fire after select()/newNote() swapped the draft.
  React.useEffect(() => {
    if (skipSaveRef.current) { skipSaveRef.current = false; return; }
    if (!agentId) return;
    if (!draft.title.trim() && !draft.content_md.trim()) return;
    setSaveState("dirty");
    const id = selectedId;
    const timer = setTimeout(async () => {
      setSaveState("saving");
      try {
        const res = await saveNote(agentId, { id: id ?? undefined, ...draft });
        // Stale-response guard: if the user switched notes while this save
        // was in flight, neither adopt the fresh id (would yank the view
        // back to the old note) nor stamp the new note's saved state.
        if (selectedIdRef.current !== id) { setSaveState("idle"); return; }
        if (res.error || !res.id) { notify(res.error ?? t("knowledge.notes.saveFailed")); setSaveState("dirty"); return; }
        setSaveState("saved");
        if (res.id !== id) {
          // Fresh note got its server id — adopt it and refresh the list.
          skipSaveRef.current = true; // selectedId change must not re-save
          setSelectedId(res.id);
          await load();
        } else {
          setNotes((prev) => prev.map((n) =>
            n.id === res.id ? { ...n, ...draft, id: res.id, updated_at: new Date().toISOString() } : n,
          ));
        }
      } catch { notify(t("knowledge.notes.saveFailed")); setSaveState("dirty"); }
    }, 1200);
    return () => clearTimeout(timer);
    // draft is one object; depending on it directly is enough.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft, selectedId, agentId]);

  const newNote = React.useCallback(() => {
    setSelectedId(null);
    setEditorActive(true);
    setDraft({ title: "", content_md: "" });
    setSaveState("saved");
    setConfirmDelete(false);
    skipSaveRef.current = true;
  }, []);

  const handleDelete = React.useCallback(async () => {
    if (!agentId || !selectedId) return;
    await deleteNote(agentId, selectedId);
    backToList();
    await load();
  }, [agentId, selectedId, load, backToList]);

  const upload = React.useCallback(async (files: File[]) => {
    if (!agentId || !selectedId || files.length === 0) return;
    setUploading(true);
    try {
      const saved = await uploadNoteAttachments(agentId, selectedId, files);
      if (saved.length === 0) notify(t("knowledge.notes.uploadFailed"));
      setAttachments((prev) => [...prev, ...saved]);
    } catch { notify(t("knowledge.notes.uploadFailed")); }
    setUploading(false);
  }, [agentId, selectedId, notify, t]);

  const insertImageRef = React.useCallback((a: KBNoteAttachment) => {
    setDraft((d) => ({ ...d, content_md: `${d.content_md}\n\n![${a.file_name}](${fileUrl(agentId ?? "", a.file_path)})\n` }));
  }, [agentId]);

  const parts = React.useMemo(() => splitBody(draft.content_md), [draft.content_md]);
  const slots = React.useMemo(() => boardSlots(draft.content_md), [draft.content_md]);

  // insertBoard appends a fresh whiteboard fence at the end of the body.
  // Appending (not cursor-position — the preview pane owns rendering, the
  // textarea has no shared cursor state) keeps the first version simple;
  // the user can drag the fence anywhere afterwards — it stays one block.
  const insertBoard = React.useCallback(() => {
    setDraft((d) => ({ ...d, content_md: `${d.content_md}\n\n\`\`\`whiteboard\n${EMPTY_BOARD}\n\`\`\`\n` }));
  }, []);

  // updateBoard rewrites the n-th whiteboard fence. A no-op when the new
  // scene equals the current one — this is what drops Excalidraw's
  // mount-time emission so merely opening a board never marks the note
  // dirty.
  const updateBoard = React.useCallback((slot: number, sceneJSON: string) => {
    setDraft((d) => {
      let seen = -1;
      const next = d.content_md.replace(BOARD_FENCE_RE, (full, lead, fence) => {
        seen++;
        if (seen !== slot) return full;
        return `${lead}\`\`\`whiteboard\n${sceneJSON}\n\`\`\``;
      });
      return next === d.content_md ? d : { ...d, content_md: next };
    });
  }, []);

  const removeBoard = React.useCallback((slot: number) => {
    setDraft((d) => {
      let seen = -1;
      const next = d.content_md.replace(BOARD_FENCE_RE, (full, _lead, _fence) => {
        seen++;
        return seen === slot ? "" : full;
      });
      // Collapse any double blank lines the removal left behind.
      return next === d.content_md ? d : { ...d, content_md: next.replace(/\n{3,}/g, "\n\n") };
    });
  }, []);

  // moveBoard reorders fences: the dragged fence's JSON moves to the
  // target slot (insert before/after by pointer half). The md text
  // between fences never moves — only which scene occupies which slot.
  const moveBoard = React.useCallback((from: number, to: number, before: boolean) => {
    if (from === to) return;
    setDraft((d) => {
      const ss = boardSlots(d.content_md);
      const jsons = splitBody(d.content_md).filter((p): p is Extract<BodyPart, { kind: "board" }> => p.kind === "board").map((p) => p.json);
      const [moved] = jsons.splice(from, 1);
      if (!moved) return d;
      const at = to > from ? (before ? to - 1 : to) : (before ? to : to + 1);
      jsons.splice(at, 0, moved);
      let k = 0;
      const next = d.content_md.replace(BOARD_FENCE_RE, (full, lead) => `${lead}\`\`\`whiteboard\n${jsons[k++] ?? "{}"}\n\`\`\``);
      return next === d.content_md ? d : { ...d, content_md: next };
    });
  }, []);

  const filtered = React.useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return notes;
    return notes.filter((n) =>
      (n.title + "\n" + n.content_md).toLowerCase().includes(q),
    );
  }, [notes, query]);

  // Delete flows through ConfirmDeleteDialog: the trash button stages the
  // note, the confirm closes the editor if it held that note.
  const confirmDeleteNote = React.useCallback(() => {
    if (!agentId || !deleteTarget) return;
    const id = deleteTarget.id;
    setDeleteTarget(null);
    deleteNote(agentId, id).then(() => {
      if (id === selectedId) backToList();
      load();
    });
  }, [agentId, deleteTarget, selectedId, backToList, load]);

  // Drag the vertical divider — mirrors ArticleView.startDrag verbatim.
  const startDrag = (e: React.PointerEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startLeft = leftWidth;
    const move = (ev: PointerEvent) => {
      const dx = ev.clientX - startX;
      setLeftWidth(Math.min(520, Math.max(220, startLeft + dx)));
    };
    const up = () => {
      document.removeEventListener("pointermove", move);
      document.removeEventListener("pointerup", up);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
    };
    document.addEventListener("pointermove", move);
    document.addEventListener("pointerup", up);
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
  };

  const dark = typeof document !== "undefined" && document.documentElement.classList.contains("dark");
  const charCount = draft.content_md.replace(/```whiteboard[\s\S]*?\n```/g, "").replace(/\s/g, "").length;

  // ── editor → preview scroll sync (one-way) ──
  // The editor reports its caret line (click/typing → smooth) or topmost
  // visible line (scroll → instant); we align the preview to the last
  // stamped block at or above that line. Anchors are the data-line attrs
  // stamped after render (stampRunAnchors) + BoardCard's fence line — all
  // 1-based global note lines. Preview→editor sync is deliberately absent:
  // it needs re-entrancy locks and isn't the workflow ("write here, read
  // there").
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
    // Height tables are rebuilt per event; anchor counts are small (tens)
    // and lineBlockAt is a binary search, so one pass per frame is fine.
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
    const editRange = view.scrollDOM.scrollHeight - view.scrollDOM.clientHeight;
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

  // Stamp the anchors after the preview DOM commits. Runs per content
  // change, and idempotent — re-stamping also clears stale data-line attrs
  // when blocks were deleted.
  //
  // A MutationObserver re-stamps on ANY subtree change, not just [parts]
  // flips: Streamdown commits new blocks through startTransition (its DOM
  // lands AFTER this effect ran — the "second note shows no anchors" bug),
  // and Shiki highlighting / board SVGs / math resolve replace content
  // asynchronously. data-line attribute writes are not observed (no
  // attributes filter), so stamping never re-triggers itself. rAF-coalesced
  // because highlight tokenization can fire many mutations in one frame.
  React.useEffect(() => {
    const container = previewRef.current;
    if (!container) return;
    const stamp = () => {
      const runs = container.querySelectorAll<HTMLElement>(".chat-md");
      let run = 0;
      for (const p of parts) {
        if (p.kind !== "md") continue;
        const el = runs[run++];
        if (el && p.text.trim()) stampRunAnchors(el, p.text, p.startLine);
      }
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
  }, [parts]);

  // Rendered body shared by the desktop split pane and the mobile preview
  // sheet — one source of truth so both stay in sync with the draft.
  const previewBody = draft.content_md.trim() ? (
    parts.map((p, i) =>
      p.kind === "md" ? (
        // Keyed by note id: switching notes must REMOUNT ChatMarkdown.
        // Reusing the instance (key={i}) lets Streamdown's block-level memo
        // keep serving the previous note's first block (observed: the old
        // H1 stayed while the new note's paragraphs rendered — anchor
        // matching then failed completely and scroll-sync died). Typing
        // keeps selectedId stable, so streaming edits are unaffected.
        p.text.trim() ? <ChatMarkdown key={`${selectedId}-${i}`} text={p.text} /> : null
      ) : (
        <BoardCard
          key={i}
          slot={slots.indexOf(i)}
          totalSlots={slots.length}
          json={p.json}
          dataLine={p.line}
          dark={dark}
          dragging={dragBoardSlot}
          dropAt={boardDropAt}
          onDragStartSlot={(s) => { dragBoardSlotRef.current = s; setDragBoardSlot(s); }}
          onDragOverSlot={(slot, before) => setBoardDropAt({ slot, before })}
          onDropSlot={(slot, before) => {
            const from = dragBoardSlotRef.current;
            if (from !== null) moveBoard(from, slot, before);
            dragBoardSlotRef.current = null;
            setDragBoardSlot(null);
            setBoardDropAt(null);
          }}
          onDragEndSlot={() => { dragBoardSlotRef.current = null; setDragBoardSlot(null); setBoardDropAt(null); }}
          onOpen={() => setBoardDialog(slots.indexOf(i))}
          onRemove={() => removeBoard(slots.indexOf(i))}
        />
      ),
    )
  ) : (
    <p className="text-xs text-muted-foreground">{t("knowledge.notes.previewEmpty")}</p>
  );

  return (
    <div className="flex h-full min-h-0">
      {/* ── list pane (ArticleView DNA: muted surface, resizable, light rows) ── */}
      <div
        style={{ "--pane-lw": `${leftWidth}px` } as React.CSSProperties}
        className={cn(
          "w-full flex-col border-r bg-muted/30 md:w-[var(--pane-lw)] md:shrink-0",
          editorActive ? "hidden md:flex" : "flex",
        )}
      >
        <div className="border-b">
          <div className="flex items-center gap-2 p-3 pb-2">
            <SearchInput
              value={query}
              onChange={setQuery}
              placeholder={t("knowledge.notes.search")}
            />
            <Button variant="outline" size="sm" className="h-7 shrink-0 text-xs" onClick={newNote}>
              <PlusIcon className="mr-1 size-3" /> {t("knowledge.notes.new")}
            </Button>
          </div>
          <p className="px-3 pb-2 text-xs tabular-nums text-muted-foreground">
            {t("knowledge.notes.count", { n: notes.length })}
          </p>
        </div>
        <ScrollArea className="min-h-0 flex-1">
          <div className="p-2">
            {loading ? (
              <p className="px-2 py-1.5 text-xs text-muted-foreground">{t("common.loading")}</p>
            ) : filtered.length === 0 ? (
              <p className="px-2 py-1.5 text-xs text-muted-foreground">{t("knowledge.notes.empty")}</p>
            ) : (
              filtered.map((n) => {
                return (
                  <div
                    key={n.id}
                    role="button"
                    tabIndex={0}
                    onClick={() => select(n)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(n); }
                    }}
                    className={cn(
                      "group relative flex w-full cursor-pointer items-center gap-1.5 rounded-md px-3 py-1.5 text-left text-sm hover:bg-accent",
                      n.id === selectedId && "bg-accent",
                    )}
                  >
                    <ChevronRightIcon className="h-3 w-3 shrink-0 text-muted-foreground" />
                    <div className="min-w-0 flex-1">
                      <p className="truncate">{noteTitle(n, t("knowledge.notes.untitled"))}</p>
                      <p className="text-xs tabular-nums text-muted-foreground">
                        {noteTime(n.updated_at)}
                      </p>
                    </div>
                    <button
                      type="button"
                      className="relative shrink-0 text-muted-foreground opacity-0 after:absolute after:-inset-2 hover:text-destructive focus-visible:opacity-100 group-hover:opacity-100"
                      onClick={(e) => { e.stopPropagation(); setDeleteTarget(n); }}
                      aria-label={t("knowledge.notes.delete")}
                    >
                      <Trash2Icon className="size-3.5" />
                    </button>
                  </div>
                );
              })
            )}
          </div>
        </ScrollArea>
      </div>

      {/* drag divider (ArticleView pattern) */}
      <div
        onPointerDown={startDrag}
        className="hidden w-1 shrink-0 cursor-col-resize transition-colors hover:bg-primary/40 md:block"
      />

      {/* ── editor pane ── */}
      <div
        className={cn("min-w-0 flex-1 flex-col", editorActive ? "flex" : "hidden md:flex")}
        onDragOver={(e) => {
          if (!selectedId) return;
          if (e.dataTransfer.types.includes("Files")) {
            e.preventDefault();
            setDragOver(true);
          }
        }}
        onDragLeave={(e) => {
          if (e.currentTarget.contains(e.relatedTarget as Node)) return;
          setDragOver(false);
        }}
        onDrop={(e) => {
          const files = Array.from(e.dataTransfer.files ?? []);
          setDragOver(false);
          if (!files.length) return;
          e.preventDefault();
          upload(files);
        }}
      >
        {editorActive ? (
          <>
            <div className="border-b p-4">
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={backToList}
                  className="-ml-1 shrink-0 text-muted-foreground hover:text-foreground md:hidden"
                  aria-label={t("common.back")}
                >
                  <ArrowLeftIcon className="size-5" />
                </button>
                <Input
                  value={draft.title}
                  onChange={(e) => setDraft((d) => ({ ...d, title: e.target.value }))}
                  placeholder={t("knowledge.notes.title")}
                  className="h-7 min-w-0 flex-1 rounded-md px-2.5 text-sm font-medium"
                />
                <Button
                  size="sm" variant="outline" className="h-7 shrink-0 text-xs"
                  onClick={insertBoard}
                  title={t("knowledge.notes.insertBoard")}
                >
                  <PenLineIcon className="mr-1 size-3" />
                  {t("knowledge.notes.tabBoard")}
                </Button>
                <Button
                  size="sm" variant="outline" className="h-7 shrink-0 text-xs"
                  disabled={!selectedId || uploading}
                  onClick={() => fileInputRef.current?.click()}
                  title={t("knowledge.notes.upload")}
                >
                  <PaperclipIcon className="mr-1 size-3" />
                  {uploading ? t("knowledge.notes.uploading") : t("knowledge.notes.upload")}
                  {attachments.length > 0 && ` (${attachments.length})`}
                </Button>
                <input
                  ref={fileInputRef}
                  type="file"
                  multiple
                  className="hidden"
                  onChange={(e) => {
                    const files = Array.from(e.target.files ?? []);
                    if (files.length) upload(files);
                    e.target.value = "";
                  }}
                />
                {selectedId && (
                  <Button
                    size="sm"
                    variant={confirmDelete ? "destructive" : "ghost"}
                    className="h-7 shrink-0 px-2 text-xs"
                    onClick={() => (confirmDelete ? handleDelete() : setConfirmDelete(true))}
                    onBlur={() => setConfirmDelete(false)}
                    title={t("knowledge.notes.delete")}
                  >
                    <Trash2Icon className="size-3.5" />
                    {confirmDelete ? t("knowledge.notes.deleteConfirm") : ""}
                  </Button>
                )}
              </div>
              <p className="mt-1 flex items-center gap-2 text-xs tabular-nums text-muted-foreground">
                <span>{t("knowledge.notes.words", { n: charCount })}</span>
                <span>·</span>
                <span>
                  {saveState === "saving" ? t("knowledge.notes.saving")
                    : saveState === "dirty" ? t("knowledge.notes.dirty")
                    : t("knowledge.notes.saved")}
                </span>
              </p>
            </div>

            <div className="relative min-h-0 flex-1 overflow-y-auto">
              {/* 正文：desktop textarea + live preview 分栏；mobile 只显 md 源码
                  （渲染预览收进右上角浮动按钮的全屏 sheet） */}
              <div className="flex h-full flex-col md:flex-row-reverse">
                <div ref={previewRef} className="hidden min-w-0 flex-1 overflow-y-auto p-4 md:block md:h-full">
                  {previewBody}
                </div>
                <div className="h-full min-h-0 w-full shrink-0 border-t md:w-1/2 md:border-t-0 md:border-r">
                  <MarkdownEditor
                    value={draft.content_md}
                    onChange={(v) => setDraft((d) => ({ ...d, content_md: v }))}
                    placeholder={t("knowledge.notes.bodyPlaceholder")}
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
            </div>

            {/* attachments strip */}
            {attachments.length > 0 && (
              <div className="flex shrink-0 gap-2 overflow-x-auto border-t px-3 py-2">
                {attachments.map((a) => (
                  <div
                    key={a.id}
                    className="group relative shrink-0 overflow-hidden rounded-lg border bg-muted/20"
                    title={`${a.file_name} · ${formatBytes(a.size)}`}
                  >
                    {a.mime.startsWith("image/") ? (
                      // eslint-disable-next-line @next/next/no-img-element
                      <img
                        src={fileUrl(agentId ?? "", a.file_path)}
                        alt={a.file_name}
                        className="h-20 w-28 object-cover"
                      />
                    ) : (
                      <div className="flex h-20 w-28 flex-col items-center justify-center gap-1 p-2 text-center">
                        <FileIcon className="size-5 text-muted-foreground" />
                        <span className="w-full truncate text-2xs text-muted-foreground">{a.file_name}</span>
                      </div>
                    )}
                    <div className="absolute inset-x-0 bottom-0 flex justify-end gap-0.5 bg-background/80 p-1 opacity-0 transition-opacity group-hover:opacity-100">
                      {a.mime.startsWith("image/") && (
                        <button type="button" className="rounded-md p-1 hover:bg-accent" title={t("knowledge.notes.insertImage")} onClick={() => insertImageRef(a)}>
                          <ImagePlusIcon className="size-3" />
                        </button>
                      )}
                      <a href={fileUrl(agentId ?? "", a.file_path, true)} download={a.file_name} className="rounded p-1 hover:bg-accent" title={t("knowledge.notes.download")}>
                        <DownloadIcon className="size-3" />
                      </a>
                      <button type="button" className="rounded-md p-1 text-destructive hover:bg-accent" title={t("knowledge.notes.deleteFile")} onClick={() => {
                        if (!agentId || !selectedId) return;
                        deleteNoteAttachment(agentId, selectedId, a.id)
                          .then(() => setAttachments((prev) => prev.filter((x) => x.id !== a.id)))
                          .catch(() => notify(t("knowledge.notes.uploadFailed")));
                      }}>
                        <Trash2Icon className="size-3" />
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            )}

            {/* drop zone hint */}
            {dragOver && (
              <div className="shrink-0 border-t-2 border-dashed border-primary/60 bg-primary/5 px-3 py-2 text-center text-xs text-primary">
                {t("knowledge.notes.dropHere")}
              </div>
            )}

            {/* mobile full-screen rendered preview (same sheet shape as the
                chat file viewer); renders after the whiteboard dialog's JSX
                position so the board editor still stacks above it */}
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
                    {draft.title || t("knowledge.notes.untitled")}
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

            {/* whiteboard editor dialog — Excalidraw chunk loads only
                while a board is open */}
            <Dialog open={boardDialog >= 0} onOpenChange={(o) => { if (!o) setBoardDialog(-1); }}>
              <DialogContent className="flex h-[88vh] max-w-[92vw] flex-col gap-2 md:max-w-5xl">
                <DialogHeader className="shrink-0">
                  <DialogTitle className="flex items-center gap-2 text-sm">
                    <PenLineIcon className="size-4" />
                    {t("knowledge.notes.tabBoard")}
                  </DialogTitle>
                </DialogHeader>
                <div className="min-h-0 flex-1">
                  {(() => {
                    const part = boardDialog >= 0 ? parts[slots[boardDialog]] : undefined;
                    if (part?.kind !== "board") return null;
                    return (
                      <Whiteboard
                        data={part.json}
                        dark={dark}
                        onChange={(sceneJSON) => updateBoard(boardDialog, sceneJSON)}
                      />
                    );
                  })()}
                </div>
              </DialogContent>
            </Dialog>
          </>
        ) : (
          <div className="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center">
            <p className="text-sm text-muted-foreground">{t("knowledge.notes.emptyHint")}</p>
            <Button size="sm" onClick={newNote}>
              <PlusIcon className="size-4" />
              {t("knowledge.notes.new")}
            </Button>
          </div>
        )}
      </div>

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        name={deleteTarget ? noteTitle(deleteTarget, t("knowledge.notes.untitled")) : ""}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        onConfirm={confirmDeleteNote}
      />
    </div>
  );
}

// BoardCard renders a ```whiteboard fence in the preview: the actual
// board as a static SVG (exportToSvg — no editor mount), falling back to
// a dashed placeholder for an empty board. Double-click (or the edit
// button) opens the Excalidraw dialog; the whole card is draggable to
// reorder fences (a live primary line shows the drop position); the SVG
// path dynamically imports the excalidraw chunk, so pages without boards
// never pay for it.
function BoardCard({
  slot, totalSlots, json, dark,
  dataLine,
  dragging, dropAt,
  onDragStartSlot, onDragOverSlot, onDropSlot, onDragEndSlot,
  onOpen, onRemove,
}: {
  slot: number;
  totalSlots: number;
  json: string;
  dark: boolean;
  // 1-based global line of the fence — a scroll-sync anchor for the split
  // preview (a caret inside a fence maps to this card).
  dataLine?: number;
  dragging: number | null;
  dropAt: { slot: number; before: boolean } | null;
  onDragStartSlot: (slot: number) => void;
  onDragOverSlot: (slot: number, before: boolean) => void;
  onDropSlot: (slot: number, before: boolean) => void;
  onDragEndSlot: () => void;
  onOpen: () => void;
  onRemove: () => void;
}) {
  const t = useT();
  const [svgHTML, setSvgHTML] = React.useState<string | null>(null);
  const count = boardElementCount(json);

  React.useEffect(() => {
    if (count === 0) { setSvgHTML(null); return; }
    let alive = true;
    (async () => {
      try {
        const { exportToSvg } = await import("@excalidraw/excalidraw");
        const scene = JSON.parse(json);
        const svg = await exportToSvg({
          elements: scene.elements ?? [],
          files: scene.files,
          appState: { exportWithDarkMode: dark } as never,
        } as never);
        // Sanitize before it reaches dangerouslySetInnerHTML: strip
        // <script>/<foreignObject>/event-handler attributes/JS-URL hrefs.
        // exportToSvg output is normally benign, but the whiteboard JSON
        // is user-editable — defense in depth at the injection boundary.
        const cleaned = sanitizeSvgHtml(svg.outerHTML);
        if (alive) setSvgHTML(cleaned);
      } catch { setSvgHTML(null); }
    })();
    return () => { alive = false; };
  }, [json, count, dark]);

  const isDragging = dragging === slot;
  const dropOnMe = dropAt?.slot === slot;

  // The whole card (header + body) is the drag surface — drop handlers
  // live on the outer div so a release over the header row still lands.
  return (
    <div
      draggable
      data-line={dataLine}
      onDragStart={(e) => {
        if (totalSlots < 2) { e.preventDefault(); return; } // nowhere to move
        onDragStartSlot(slot);
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("text/plain", String(slot));
      }}
      onDragOver={(e) => {
        // No "is a board being dragged" guard: dragover only fires during
        // a drag anyway, and a files-drag dropping here falls through to
        // the editor's upload handler (this card doesn't stopPropagation).
        e.preventDefault();
        const r = e.currentTarget.getBoundingClientRect();
        onDragOverSlot(slot, e.clientY < r.top + r.height / 2);
      }}
      onDrop={(e) => {
        e.preventDefault();
        const r = e.currentTarget.getBoundingClientRect();
        onDropSlot(slot, e.clientY < r.top + r.height / 2);
      }}
      onDragEnd={onDragEndSlot}
      className={cn(
        "group relative my-2 overflow-hidden rounded-lg border",
        isDragging && "opacity-40",
        // drop indicator above/below the whole card
        dropOnMe && dropAt!.before && "before:absolute before:inset-x-0 before:top-0 before:z-10 before:h-0.5 before:bg-primary",
        dropOnMe && !dropAt!.before && "after:absolute after:inset-x-0 after:bottom-0 after:z-10 after:h-0.5 after:bg-primary",
      )}
    >
      <div className="flex items-center gap-2 border-b bg-muted/30 px-3 py-1.5">
        <GripVerticalIcon className="size-3.5 shrink-0 cursor-grab text-muted-foreground/50 group-hover:text-muted-foreground" aria-hidden />
        <span className="text-xs font-medium">{t("knowledge.notes.tabBoard")}</span>
        <span className="text-2xs tabular-nums text-muted-foreground">
          {t("knowledge.notes.boardElements", { n: count })}
        </span>
        <span className="flex-1" />
        <Button
          size="sm" variant="outline" className="h-6 px-2 text-xs"
          onClick={(e) => { e.stopPropagation(); onOpen(); }}
        >
          {t("knowledge.notes.boardEdit")}
        </Button>
        <Button
          size="sm" variant="ghost" className="h-6 px-1.5 text-xs text-destructive"
          onClick={(e) => { e.stopPropagation(); onRemove(); }}
          title={t("knowledge.notes.deleteFile")}
        >
          <Trash2Icon className="size-3" />
        </Button>
      </div>
      <div
        role="button"
        tabIndex={0}
        onDoubleClick={onOpen}
        onKeyDown={(e) => { if (e.key === "Enter") onOpen(); }}
        className="cursor-pointer select-none"
      >
        {svgHTML ? (
          <div className="p-2 [&_svg]:mx-auto [&_svg]:block [&_svg]:h-auto [&_svg]:max-h-64 [&_svg]:w-full" dangerouslySetInnerHTML={{ __html: svgHTML }} />
        ) : (
          <div className="flex h-20 items-center justify-center gap-2 text-xs text-muted-foreground">
            <PenLineIcon className="size-4" />
            {t("knowledge.notes.boardEmpty")}
          </div>
        )}
      </div>
    </div>
  );
}
