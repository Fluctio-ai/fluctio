"use client";
import { useT } from "@/lib/i18n";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
import {
  FileTextIcon,
  GlobeIcon,
  TrashIcon,
  PlusIcon,
} from "lucide-react";
import {
  type KBSource,
  type KBStats,
  type KBEntry,
  listKBSources,
  listKBEntries,
  kbIngestText,
  kbIngestURL,
  deleteKBSource,
  getKBStats,
  generateWiki,
  kbSaveFlash,
  kbSaveTodo,
  kbUpdateTodo,
  kbListTodos,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { cn } from "@/lib/utils";

type Tab = "article" | "flash" | "todo";
type TodoStatus = "pending" | "in_progress" | "done" | "cancelled";
const TODO_STATUSES: TodoStatus[] = ["pending", "in_progress", "done", "cancelled"];

// relativeTime renders a short "x minutes ago" style label. Intentionally
// locale-light (the user base is zh-CN); falls back to a date for old items.
function relativeTime(iso: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const diff = Date.now() - d.getTime();
  const m = Math.floor(diff / 60000);
  if (m < 1) return "刚刚";
  if (m < 60) return `${m} 分钟前`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} 小时前`;
  const day = Math.floor(h / 24);
  if (day < 30) return `${day} 天前`;
  return d.toLocaleDateString();
}

// datetimeLocalValue converts an RFC3339 string into the value a
// <input type="datetime-local"> expects ("YYYY-MM-DDTHH:mm"), local time.
function datetimeLocalValue(iso: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// AgentKnowledgePage is the three-tab KB browser at /agents/<id>/knowledge/:
// 📄 Articles (chunked sources, two-pane), 💡 Flashes (灵感闪记 masonry +
// "记一笔" capture), ✅ Todos (kanban by status). KB *retrieval settings*
// stay in the Settings dialog's Knowledge tab (kb-settings-card).
export default function AgentKnowledgePage() {
  const t = useT();
  const [tab, setTab] = useState<Tab>("article");
  const tabs: ReadonlyArray<[Tab, string, string]> = [
    ["article", "📄", "knowledge.articles"],
    ["flash", "💡", "knowledge.flashes"],
    ["todo", "✅", "knowledge.todos"],
  ];
  return (
    <div className="flex h-[calc(100vh-3.5rem)] flex-col">
      <div className="flex gap-1 border-b bg-muted/30 px-3 py-1.5">
        {tabs.map(([key, icon, label]) => (
          <button
            key={key}
            type="button"
            onClick={() => setTab(key)}
            className={cn(
              "rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
              tab === key
                ? "bg-background text-foreground shadow-sm"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            {icon} {t(label)}
          </button>
        ))}
      </div>
      <div className="flex-1 min-h-0">
        {tab === "article" && <ArticleView />}
        {tab === "flash" && <FlashView />}
        {tab === "todo" && <TodoView />}
      </div>
    </div>
  );
}

// ── Articles: chunked sources (original two-pane browser) ──

function ArticleView() {
  const t = useT();
  const agentId = useAgentIdFromURL();

  const [sources, setSources] = useState<KBSource[]>([]);
  const [stats, setStats] = useState<KBStats | null>(null);
  const [loading, setLoading] = useState(true);

  const [selectedSource, setSelectedSource] = useState<KBSource | null>(null);
  const [entries, setEntries] = useState<KBEntry[]>([]);
  const [entriesLoading, setEntriesLoading] = useState(false);

  const [textDialogOpen, setTextDialogOpen] = useState(false);
  const [urlDialogOpen, setUrlDialogOpen] = useState(false);
  const [textTitle, setTextTitle] = useState("");
  const [textContent, setTextContent] = useState("");
  const [urlValue, setUrlValue] = useState("");
  const [urlTitle, setUrlTitle] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const loadData = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      const [srcs, st] = await Promise.all([listKBSources(agentId), getKBStats(agentId)]);
      // Articles = everything that is NOT a flash/todo (incl. legacy untyped rows).
      setSources(srcs.filter((s) => s.type !== "flash" && s.type !== "todo"));
      setStats(st);
    } catch {}
    setLoading(false);
  }, [agentId]);

  useEffect(() => { loadData(); }, [loadData]);

  const handleSelectSource = useCallback(async (src: KBSource) => {
    if (!agentId) return;
    setSelectedSource(src);
    setEntries([]);
    setEntriesLoading(true);
    try {
      setEntries(await listKBEntries(agentId, src.id));
    } catch { setEntries([]); }
    setEntriesLoading(false);
  }, [agentId]);

  const handleIngestText = useCallback(async () => {
    if (!agentId || !textContent.trim()) return;
    setSubmitting(true);
    try {
      const res = await kbIngestText(agentId, textTitle || t("knowledge.untitled"), textContent);
      if ("error" in res) { alert(res.error); } else {
        setTextDialogOpen(false); setTextTitle(""); setTextContent("");
        await loadData();
        if ("source_id" in res) generateWiki(agentId, [res.source_id!]).catch(() => {});
      }
    } catch { alert(t("knowledge.failedAddText")); }
    setSubmitting(false);
  }, [agentId, textTitle, textContent, loadData, t]);

  const handleIngestURL = useCallback(async () => {
    if (!agentId || !urlValue.trim()) return;
    setSubmitting(true);
    try {
      const res = await kbIngestURL(agentId, urlValue, urlTitle || undefined);
      if ("error" in res) { alert(res.error); } else {
        setUrlDialogOpen(false); setUrlValue(""); setUrlTitle("");
        await loadData();
        if ("source_id" in res) generateWiki(agentId, [res.source_id!]).catch(() => {});
      }
    } catch { alert(t("knowledge.failedFetchURL")); }
    setSubmitting(false);
  }, [agentId, urlValue, urlTitle, loadData, t]);

  const handleDeleteSource = useCallback(async (sourceId: string) => {
    if (!agentId) return;
    try {
      const res = await deleteKBSource(agentId, sourceId);
      if ("error" in res) alert(res.error); else {
        if (selectedSource?.id === sourceId) { setSelectedSource(null); setEntries([]); }
        loadData();
      }
    } catch {}
  }, [agentId, loadData, selectedSource]);

  return (
    <div className="flex h-full">
      <div className="w-80 shrink-0 border-r bg-muted/30 flex flex-col">
        <div className="p-3 border-b space-y-2">
          <div>
            <h3 className="text-sm font-semibold">{t("knowledge.dataSources")}</h3>
            {stats && (
              <p className="text-xs text-muted-foreground mt-0.5">
                {stats.source_count} {t("knowledge.sources")} · {stats.entry_count} {t("knowledge.entries")} · {(stats.total_chars / 1024).toFixed(1)} KB
              </p>
            )}
          </div>
          <div className="flex gap-1.5">
            <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => setTextDialogOpen(true)}>
              <FileTextIcon className="h-3 w-3 mr-1" /> {t("knowledge.text")}
            </Button>
            <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => setUrlDialogOpen(true)}>
              <GlobeIcon className="h-3 w-3 mr-1" /> {t("knowledge.url")}
            </Button>
          </div>
        </div>
        <ScrollArea className="flex-1">
          <div className="p-2">
            {loading ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">{t("common.loading")}</p>
            ) : sources.length === 0 ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">{t("knowledge.noSources")}</p>
            ) : (
              sources.map((src) => (
                <div
                  key={src.id}
                  role="button"
                  tabIndex={0}
                  className={cn(
                    "group w-full text-left px-3 py-1.5 text-sm rounded-md hover:bg-accent flex items-center gap-2 cursor-pointer",
                    selectedSource?.id === src.id && "bg-accent",
                  )}
                  onClick={() => handleSelectSource(src)}
                >
                  <div className="flex-1 min-w-0">
                    <p className="truncate">{src.title}</p>
                    <p className="text-xs text-muted-foreground">
                      {src.entry_count} entries · {(src.total_chars / 1024).toFixed(1)} KB
                    </p>
                  </div>
                  <button
                    type="button"
                    className="opacity-0 group-hover:opacity-100 text-muted-foreground hover:text-destructive shrink-0"
                    onClick={(e) => { e.stopPropagation(); handleDeleteSource(src.id); }}
                    aria-label={t("common.delete")}
                  >
                    <TrashIcon className="h-3.5 w-3.5" />
                  </button>
                </div>
              ))
            )}
          </div>
        </ScrollArea>
      </div>

      <div className="flex-1 flex flex-col min-w-0">
        {selectedSource ? (
          <>
            <div className="p-4 border-b">
              <h1 className="text-xl font-bold truncate">{selectedSource.title}</h1>
              <p className="text-xs text-muted-foreground mt-1">
                {selectedSource.entry_count} {t("knowledge.entries")} · {((selectedSource.total_chars ?? 0) / 1024).toFixed(1)} KB
              </p>
            </div>
            <ScrollArea className="flex-1">
              <div className="p-4 space-y-3 max-w-4xl">
                {entriesLoading ? (
                  <p className="text-sm text-muted-foreground">{t("common.loading")}</p>
                ) : entries.length === 0 ? (
                  <p className="text-sm text-muted-foreground">{t("knowledge.noEntries")}</p>
                ) : (
                  entries.map((entry) => (
                    <div key={entry.id} className="rounded-md border bg-muted/30 p-3">
                      <p className="text-[10px] text-muted-foreground mb-1">{t("knowledge.chunk")} {entry.chunk_index}</p>
                      <pre className="text-sm whitespace-pre-wrap font-sans">{entry.content}</pre>
                    </div>
                  ))
                )}
              </div>
            </ScrollArea>
          </>
        ) : (
          <div className="flex-1 flex items-center justify-center text-sm text-muted-foreground p-4 text-center">
            {t("knowledge.selectSourcePrompt")}
          </div>
        )}
      </div>

      <Dialog open={textDialogOpen} onOpenChange={setTextDialogOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.addText")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("knowledge.titleLabel")}</Label>
              <Input value={textTitle} onChange={(e) => setTextTitle(e.target.value)} placeholder={t("knowledge.sourceTitlePlaceholder")} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("knowledge.contentLabel")}</Label>
              <textarea
                className="flex min-h-[200px] w-full rounded-md border bg-transparent px-3 py-2 text-sm"
                value={textContent} onChange={(e) => setTextContent(e.target.value)}
                placeholder={t("knowledge.contentPlaceholder")}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setTextDialogOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleIngestText} disabled={submitting || !textContent.trim()}>
              {submitting ? t("knowledge.adding") : t("knowledge.add")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={urlDialogOpen} onOpenChange={setUrlDialogOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.addFromURL")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("knowledge.urlLabel")}</Label>
              <Input value={urlValue} onChange={(e) => setUrlValue(e.target.value)} placeholder={t("knowledge.urlPlaceholder")} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("knowledge.titleOptional")}</Label>
              <Input value={urlTitle} onChange={(e) => setUrlTitle(e.target.value)} placeholder={t("knowledge.customTitlePlaceholder")} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setUrlDialogOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleIngestURL} disabled={submitting || !urlValue.trim()}>
              {submitting ? t("knowledge.fetching") : t("knowledge.add")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// ── Flashes: 灵感闪记 masonry + "记一笔" capture ──

type FlashItem = { src: KBSource; content: string };

function FlashView() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [flashes, setFlashes] = useState<FlashItem[]>([]);
  const [draft, setDraft] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  const load = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      const all = await listKBSources(agentId);
      const flashSrcs = all.filter((s) => s.type === "flash");
      const withContent = await Promise.all(
        flashSrcs.map(async (src) => {
          const es = await listKBEntries(agentId, src.id);
          return { src, content: es.map((e) => e.content).join("\n") } as FlashItem;
        }),
      );
      // newest first
      withContent.sort((a, b) => (a.src.created_at < b.src.created_at ? 1 : -1));
      setFlashes(withContent);
    } catch {}
    setLoading(false);
  }, [agentId]);

  useEffect(() => { load(); }, [load]);

  const handleSave = useCallback(async () => {
    if (!agentId || !draft.trim()) return;
    setSaving(true);
    try {
      const res = await kbSaveFlash(agentId, draft.trim());
      if ("error" in res) alert(res.error); else { setDraft(""); await load(); }
    } catch { alert(t("knowledge.failedAddText")); }
    setSaving(false);
  }, [agentId, draft, load, t]);

  const handleDelete = useCallback(async (id: string) => {
    if (!agentId) return;
    await deleteKBSource(agentId, id);
    load();
  }, [agentId, load]);

  return (
    <div className="flex h-full flex-col">
      <div className="border-b p-3 flex gap-2">
        <textarea
          className="flex-1 min-h-[44px] max-h-28 resize-y rounded-md border bg-transparent px-3 py-2 text-sm"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder={t("knowledge.flashPlaceholder")}
        />
        <Button size="sm" className="self-end" onClick={handleSave} disabled={saving || !draft.trim()}>
          {saving ? t("common.saving") : t("knowledge.saveFlash")}
        </Button>
      </div>
      <ScrollArea className="flex-1">
        <div className="p-4 columns-1 sm:columns-2 lg:columns-3 gap-3">
          {loading ? (
            <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
          ) : flashes.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("knowledge.noFlashes")}</p>
          ) : (
            flashes.map(({ src, content }) => (
              <div
                key={src.id}
                className="group mb-3 break-inside-avoid rounded-lg border border-amber-200/60 bg-amber-50/70 dark:border-amber-900/40 dark:bg-amber-950/20 p-3"
              >
                <p className="text-sm whitespace-pre-wrap">{content}</p>
                <div className="mt-2 flex items-center justify-between text-[10px] text-muted-foreground">
                  <span>{relativeTime(src.created_at)}</span>
                  <button
                    type="button"
                    className="opacity-0 group-hover:opacity-100 hover:text-destructive"
                    onClick={() => handleDelete(src.id)}
                    aria-label={t("common.delete")}
                  >
                    <TrashIcon className="h-3 w-3" />
                  </button>
                </div>
              </div>
            ))
          )}
        </div>
      </ScrollArea>
    </div>
  );
}

// ── Todos: kanban by status ──

function TodoView() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [todos, setTodos] = useState<FlashItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [newOpen, setNewOpen] = useState(false);
  const [newContent, setNewContent] = useState("");
  const [newEnd, setNewEnd] = useState("");
  const [creating, setCreating] = useState(false);

  const load = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      const all = await kbListTodos(agentId);
      const withContent = await Promise.all(
        all.map(async (src) => {
          const es = await listKBEntries(agentId, src.id);
          return { src, content: es.map((e) => e.content).join("\n") } as FlashItem;
        }),
      );
      setTodos(withContent);
    } catch {}
    setLoading(false);
  }, [agentId]);

  useEffect(() => { load(); }, [load]);

  const handleCreate = useCallback(async () => {
    if (!agentId || !newContent.trim()) return;
    setCreating(true);
    try {
      const endAt = newEnd ? new Date(newEnd).toISOString() : undefined;
      const res = await kbSaveTodo(agentId, newContent.trim(), "pending", undefined, endAt);
      if ("error" in res) alert(res.error); else {
        setNewOpen(false); setNewContent(""); setNewEnd(""); load();
      }
    } catch { alert(t("knowledge.failedAddText")); }
    setCreating(false);
  }, [agentId, newContent, newEnd, load, t]);

  const handleMove = useCallback(async (id: string, status: TodoStatus) => {
    if (!agentId) return;
    const res = await kbUpdateTodo(agentId, id, { status });
    if ("error" in res) alert(res.error); else load();
  }, [agentId, load]);

  const handleDelete = useCallback(async (id: string) => {
    if (!agentId) return;
    await deleteKBSource(agentId, id);
    load();
  }, [agentId, load]);

  const byStatus = useMemo(() => {
    const m: Record<TodoStatus, FlashItem[]> = { pending: [], in_progress: [], done: [], cancelled: [] };
    for (const it of todos) {
      const s = (it.src.status || "pending") as TodoStatus;
      if (m[s]) m[s].push(it);
    }
    return m;
  }, [todos]);

  return (
    <div className="flex h-full flex-col">
      <div className="border-b p-3 flex items-center justify-between">
        <h3 className="text-sm font-semibold">{t("knowledge.todoBoard")}</h3>
        <Button size="sm" onClick={() => setNewOpen(true)}>
          <PlusIcon className="h-3 w-3 mr-1" /> {t("knowledge.newTodo")}
        </Button>
      </div>
      <ScrollArea className="flex-1">
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-3 p-4">
          {TODO_STATUSES.map((st) => (
            <div key={st} className="flex flex-col rounded-lg bg-muted/30 min-h-[200px]">
              <div className="p-2 border-b flex items-center justify-between">
                <span className={cn("text-xs font-semibold uppercase tracking-wide", statusAccent(st))}>
                  {t("knowledge.status_" + st)}
                </span>
                <Badge variant="outline" className="text-[10px] px-1.5 py-0">{byStatus[st].length}</Badge>
              </div>
              <div className="flex-1 p-2 space-y-2">
                {loading ? (
                  <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
                ) : byStatus[st].map(({ src, content }) => (
                  <TodoCard
                    key={src.id}
                    src={src}
                    content={content}
                    onMove={handleMove}
                    onDelete={handleDelete}
                  />
                ))}
              </div>
            </div>
          ))}
        </div>
      </ScrollArea>

      <Dialog open={newOpen} onOpenChange={setNewOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.newTodo")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("knowledge.contentLabel")}</Label>
              <textarea
                className="flex min-h-[120px] w-full rounded-md border bg-transparent px-3 py-2 text-sm"
                value={newContent}
                onChange={(e) => setNewContent(e.target.value)}
                placeholder={t("knowledge.todoContentPlaceholder")}
              />
            </div>
            <div className="space-y-1.5">
              <Label>{t("knowledge.dueLabel")}</Label>
              <Input type="datetime-local" value={newEnd} onChange={(e) => setNewEnd(e.target.value)} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setNewOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleCreate} disabled={creating || !newContent.trim()}>
              {creating ? t("common.saving") : t("knowledge.add")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function TodoCard({
  src,
  content,
  onMove,
  onDelete,
}: {
  src: KBSource;
  content: string;
  onMove: (id: string, status: TodoStatus) => void;
  onDelete: (id: string) => void;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const overdue = src.end_at && new Date(src.end_at).getTime() < Date.now() && (src.status === "pending" || src.status === "in_progress");
  const next: TodoStatus | null =
    src.status === "pending" ? "in_progress" :
    src.status === "in_progress" ? "done" : null;
  return (
    <div className="group rounded-md border bg-background p-2.5 text-sm shadow-sm">
      <p className={cn("whitespace-pre-wrap", src.status === "cancelled" && "line-through text-muted-foreground")}>
        {content}
      </p>
      {src.end_at && (
        <p className={cn("mt-1.5 text-[10px]", overdue ? "text-destructive font-medium" : "text-muted-foreground")}>
          {t("knowledge.dueLabel")}: {datetimeLocalValue(src.end_at).replace("T", " ")}
        </p>
      )}
      <div className="mt-2 flex items-center gap-1">
        {next && (
          <Button
            size="sm"
            variant="outline"
            className="h-6 px-2 text-[11px]"
            onClick={() => onMove(src.id, next)}
          >
            → {t("knowledge.status_" + next)}
          </Button>
        )}
        <button
          type="button"
          className="ml-auto text-[10px] text-muted-foreground hover:text-foreground"
          onClick={() => setOpen((v) => !v)}
        >
          {t("knowledge.move")}
        </button>
        {open && (
          <select
            className="absolute z-10 text-[11px] rounded border bg-background shadow-md"
            value={src.status}
            onChange={(e) => { onMove(src.id, e.target.value as TodoStatus); setOpen(false); }}
          >
            {TODO_STATUSES.map((s) => (
              <option key={s} value={s}>{t("knowledge.status_" + s)}</option>
            ))}
          </select>
        )}
        <button
          type="button"
          className="opacity-0 group-hover:opacity-100 text-muted-foreground hover:text-destructive"
          onClick={() => onDelete(src.id)}
          aria-label={t("common.delete")}
        >
          <TrashIcon className="h-3 w-3" />
        </button>
      </div>
    </div>
  );
}

// statusAccent returns a tailwind text-color class to tint each kanban column
// header by its lifecycle state.
function statusAccent(s: TodoStatus): string {
  switch (s) {
    case "pending":
      return "text-muted-foreground";
    case "in_progress":
      return "text-blue-600 dark:text-blue-400";
    case "done":
      return "text-green-600 dark:text-green-400";
    case "cancelled":
      return "text-muted-foreground line-through";
  }
  return "";
}
