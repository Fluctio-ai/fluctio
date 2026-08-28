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

type SaveState = "saved" | "saving" | "dirty";

// EMPTY_BOARD is the canonical scene of a brand-new whiteboard. The fence
// inserted by "插入白板" starts as exactly this string, so the Whiteboard's
// mount-time emission compares equal and is skipped (no phantom save).
const EMPTY_BOARD = JSON.stringify({ type: "excalidraw", version: 2, elements: [], files: {} });

const BOARD_FENCE_RE = /(^|\n)(```whiteboard\n[\s\S]*?\n```)(?=\n|$)/g;

type BodyPart = { kind: "md"; text: string } | { kind: "board"; json: string };

// splitBody partitions the markdown body into md runs and whiteboard
// fences so the preview can render boards as interactive cards between
// the surrounding text. Fence content is preserved verbatim, which keeps
// round-tripping (preview → edit → textarea) lossless.
function splitBody(md: string): BodyPart[] {
  const parts: BodyPart[] = [];
  let last = 0;
  for (const m of md.matchAll(BOARD_FENCE_RE)) {
    const fence = m[2];
    const start = (m.index ?? 0) + (m[1] ? m[1].length : 0);
    if (start > last) parts.push({ kind: "md", text: md.slice(last, start) });
    parts.push({ kind: "board", json: fence.slice("```whiteboard\n".length, -"```".length).trim() });
    last = start + fence.length;
  }
  if (last < md.length) parts.push({ kind: "md", text: md.slice(last) });
  return parts;
}

function boardElementCount(json: string): number {
  try { return JSON.parse(json)?.elements?.length ?? 0; } catch { return 0; }
}

function boardSlots(md: string): number[] {
  return splitBody(md).reduce<number[]>((acc, p, i) => (p.kind === "board" ? [...acc, i] : acc), []);
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
      const jsons = splitBody(d.content_md).filter((p): p is { kind: "board"; json: string } => p.kind === "board").map((p) => p.json);
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

  // Rendered body shared by the desktop split pane and the mobile preview
  // sheet — one source of truth so both stay in sync with the draft.
  const previewBody = draft.content_md.trim() ? (
    parts.map((p, i) =>
      p.kind === "md" ? (
        p.text.trim() ? <ChatMarkdown key={i} text={p.text} /> : null
      ) : (
        <BoardCard
          key={i}
          slot={slots.indexOf(i)}
          totalSlots={slots.length}
          json={p.json}
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
                <div className="hidden min-w-0 flex-1 overflow-y-auto p-4 md:block md:h-full">
                  {previewBody}
                </div>
                <div className="h-full min-h-0 w-full shrink-0 border-t md:w-1/2 md:border-t-0 md:border-r">
                  <MarkdownEditor
                    value={draft.content_md}
                    onChange={(v) => setDraft((d) => ({ ...d, content_md: v }))}
                    placeholder={t("knowledge.notes.bodyPlaceholder")}
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
                        <span className="w-full truncate text-[10px] text-muted-foreground">{a.file_name}</span>
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
  dragging, dropAt,
  onDragStartSlot, onDragOverSlot, onDropSlot, onDragEndSlot,
  onOpen, onRemove,
}: {
  slot: number;
  totalSlots: number;
  json: string;
  dark: boolean;
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
        if (alive) setSvgHTML(svg.outerHTML);
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
        <span className="text-[11px] tabular-nums text-muted-foreground">
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
