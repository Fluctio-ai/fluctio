"use client";
import { useLocale, useT } from "@/lib/i18n";

import { createElement, isValidElement, useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { SearchInput } from "@/components/ui/search-input";
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
  ArrowLeftIcon,
  AlignLeftIcon,
  AlertTriangleIcon,
  CheckIcon,
  CheckSquareIcon,
  ChevronDownIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  CopyIcon,
  FileTextIcon,
  ListOrderedIcon,
  PencilIcon,
  PlusIcon,
  QuoteIcon,
  SparklesIcon,
  SproutIcon,
  TrashIcon,
  XIcon,
  type LucideIcon,
} from "lucide-react";
import {
  type KBSource,
  type KBStats,
  type KBEntry,
  type KBBookmark,
  listKBSources,
  listKBEntries,
  kbIngestText,
  kbIngestURL,
  deleteKBSource,
  getKBStats,
  generateWiki,
  kbSaveFlash,
  kbUpdateFlash,
  kbSaveTodo,
  kbUpdateTodo,
  kbListTodos,
  listBookmarks,
  saveBookmark,
  deleteBookmark,
  promoteBookmark,
  updateBookmark,
  kbGetInsights,
  kbGenerateInsights,
  listKBPending,
  resolveKBPending,
  type ArticleInsights,
  type KBPending,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { cn } from "@/lib/utils";
import { dayKey, endOfToday, pad2 } from "@/lib/time";
import { readCache, writeCache } from "@/lib/page-data-cache";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ConfirmDeleteDialog } from "@/components/confirm-delete-dialog";
import { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import { DocMarkdown } from "@/components/doc-markdown";

type Tab = "article" | "flash" | "todo" | "diary";

// DetailTab is the article-detail sub-tab: the original text plus the five
// deep-reading sections. Section tabs render only after insights exist, so an
// article with no reading yet shows just "原文" + the generate button.
type DetailTab = "text" | "summary" | "chapters" | "quotes" | "actions" | "sprouts";

const INSIGHT_SECTIONS: ReadonlyArray<[Exclude<DetailTab, "text">, string, LucideIcon]> = [
  ["summary", "knowledge.coreSummary", AlignLeftIcon],
  ["chapters", "knowledge.chapters", ListOrderedIcon],
  ["quotes", "knowledge.quotesTitle", QuoteIcon],
  ["actions", "knowledge.actionsTitle", CheckSquareIcon],
  ["sprouts", "knowledge.sproutsTitle", SproutIcon],
];
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
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

// KnowledgePage is the shared shell for the four KB sub-pages
// (/knowledge/ = articles, /knowledge/flashes/, /knowledge/todos/,
// /knowledge/diary/). It owns the page-level error toast (replaces
// blocking alert()) and fills the viewport; each sub-page renders the
// single view it wants via the children render-prop, which receives the
// notify callback. The old top tab bar (articles/flashes/todos/diary +
// pending/todo badges) is gone — those are now first-class sidebar
// entries, see nav-knowledge.
export function KnowledgePage({
  children,
}: {
  children: (notify: (msg: string) => void) => ReactNode;
}) {
  const t = useT();
  const [toast, setToast] = useState<string | null>(null);
  const notify = useCallback((msg: string) => setToast(msg), []);
  return (
    <div className="flex h-[calc(100vh-3.5rem)] flex-col">
      <div className="flex-1 min-h-0">{children(notify)}</div>
      {toast && (
        <div
          role="alert"
          className="fixed bottom-4 right-4 z-50 flex items-start gap-2 rounded-lg border border-destructive/30 bg-background p-3 pr-2 shadow-lg max-w-sm"
        >
          <p className="flex-1 text-sm text-destructive">{toast}</p>
          <button
            type="button"
            onClick={() => setToast(null)}
            aria-label={t("common.dismiss")}
            className="shrink-0 rounded-md p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            <XIcon className="h-3.5 w-3.5" />
          </button>
        </div>
      )}
    </div>
  );
}

// ── Articles: chunked sources (original two-pane browser) ──

// The first chunk of an ingested article usually opens with a markdown
// heading repeating the source title — the detail header above already
// shows it, so a matching heading renders the title twice back-to-back.
// Strip the leading heading only when its text matches the title after
// whitespace/emphasis normalization; anything else passes untouched.
function stripLeadingTitle(content: string, title: string): string {
  const m = content.match(/^#{1,6}\s+(.+)\r?\n?/);
  if (!m) return content;
  const norm = (s: string) => s.replace(/[\s*_~`#]/g, "");
  if (!norm(m[1]) || norm(m[1]) !== norm(title)) return content;
  return content.slice(m[0].length);
}

// Attribution like「## 来源信息」「## 来源：某公众号」is byline metadata,
// not document structure — but agent-authored ingest marks it as a
// heading, where it renders as large as a section title. isSourceHeadingText
// recognizes those forms (label-only headings like 来源信息/元信息, or the
// label with its value inline) so the renderer can demote them to a small
// annotation. Structural headings (来源分析方法论) don't match on purpose.
function isSourceHeadingText(text: string): boolean {
  const t = text.trim().replace(/\s+/g, " ");
  return (
    /^(?:来源信息|参考来源|元信息|元数据|出处|来源|作者|发布时间|发布日期|source|references?|author|published|meta)\s*[:：]?$/i.test(t) ||
    /^(?:来源|出处|参考来源|作者|发布|source|references?|author|published)\s*[:：]/i.test(t)
  );
}

// flattenNodeText pulls a heading's plain text out of React children: the
// heading may be a plain string, an array, or wrap the text in links/em.
function flattenNodeText(node: ReactNode): string {
  if (node == null || typeof node === "boolean") return "";
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(flattenNodeText).join("");
  if (isValidElement<{ children?: ReactNode }>(node)) return flattenNodeText(node.props.children);
  return "";
}

// ArticleHeading is the article body's h1–h6: attribution headings demote
// to a small muted byline; everything else renders as the heading itself.
// The byline is a bare <div> so the typography plugin's element styles
// (which outrank single utility classes on specificity) leave it alone.
function ArticleHeading({ level, children }: { level?: number; children?: ReactNode }) {
  if (isSourceHeadingText(flattenNodeText(children))) {
    return <div className="my-3 text-xs text-muted-foreground">{children}</div>;
  }
  return createElement(`h${level ?? 2}`, null, children);
}

const ARTICLE_MD_COMPONENTS: Components = {
  h1: ArticleHeading,
  h2: ArticleHeading,
  h3: ArticleHeading,
  h4: ArticleHeading,
  h5: ArticleHeading,
  h6: ArticleHeading,
};

export function ArticleView({ notify }: { notify: (msg: string) => void }) {
  const t = useT();
  const agentId = useAgentIdFromURL();

  const [initialArticle] = useState(() =>
    agentId
      ? readCache<{ sources: KBSource[]; stats: KBStats | null; pending: KBPending[] }>(`kb-article:${agentId}`)
      : undefined,
  );
  const [sources, setSources] = useState<KBSource[]>(initialArticle?.sources ?? []);
  const [stats, setStats] = useState<KBStats | null>(initialArticle?.stats ?? null);
  const [pending, setPending] = useState<KBPending[]>(initialArticle?.pending ?? []);
  const [pendingOpen, setPendingOpen] = useState(true);
  const [loading, setLoading] = useState(!initialArticle);

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

  // Deep-reading insights (深度解读) for the selected article.
  const [insights, setInsights] = useState<ArticleInsights | null>(null);
  const [insightsLoading, setInsightsLoading] = useState(false);
  const [generating, setGenerating] = useState(false);
  const [detailTab, setDetailTab] = useState<DetailTab>("text");

  // Resizable left pane (mirrors the wiki page's drag divider). Width in px,
  // clamped to 220–520 so the source list can't collapse off-screen.
  const [leftWidth, setLeftWidth] = useState(320);
  // Delete flows through ConfirmDeleteDialog: the trash button only stages
  // the target, confirmDeleteSource performs the API call.
  const [deleteTarget, setDeleteTarget] = useState<KBSource | null>(null);
  const [query, setQuery] = useState("");

  const loadData = useCallback(async () => {
    if (!agentId) return;
    const silent = !!readCache(`kb-article:${agentId}`);
    if (!silent) setLoading(true);
    try {
      const [srcs, st, pend] = await Promise.all([listKBSources(agentId), getKBStats(agentId), listKBPending(agentId)]);
      // Articles = everything that is NOT a flash/todo (incl. legacy untyped rows).
      const articles = srcs.filter((s) => s.type !== "flash" && s.type !== "todo");
      setSources(articles);
      setStats(st);
      setPending(pend);
      writeCache(`kb-article:${agentId}`, { sources: articles, stats: st, pending: pend });
    } catch {}
    if (!silent) setLoading(false);
  }, [agentId]);

  const handleResolvePending = useCallback(async (pendingId: string, action: "merge" | "create" | "skip") => {
    if (!agentId) return;
    await resolveKBPending(agentId, pendingId, action);
    await loadData();
  }, [agentId, loadData]);

  useEffect(() => { loadData(); }, [loadData]);

  // Local title filter for the source list (mirrors flash/bookmark search).
  const visibleSources = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? sources.filter((s) => s.title.toLowerCase().includes(q)) : sources;
  }, [sources, query]);

  const handleSelectSource = useCallback(async (src: KBSource) => {
    if (!agentId) return;
    setSelectedSource(src);
    setEntries([]);
    setInsights(null);
    setDetailTab("text");
    setEntriesLoading(true);
    setInsightsLoading(true);
    try {
      const [es, ins] = await Promise.all([
        listKBEntries(agentId, src.id),
        kbGetInsights(agentId, src.id),
      ]);
      setEntries(es);
      setInsights(ins);
    } catch {
      setEntries([]);
      setInsights(null);
    }
    setEntriesLoading(false);
    setInsightsLoading(false);
  }, [agentId]);

  // articleText stitches the stored chunks back into one article. The
  // chunker splits on paragraph boundaries with no overlap, so a blank-line
  // join rebuilds the original; rendering it as ONE markdown pass — instead
  // of per-chunk blocks joined by * * * dividers — keeps constructs that
  // straddle a chunk boundary (lists, tables, split sentences) intact.
  const articleText = useMemo(() => {
    if (!entries.length) return "";
    const first = stripLeadingTitle(entries[0].content, selectedSource?.title ?? "");
    return [first, ...entries.slice(1).map((e) => e.content)].join("\n\n");
  }, [entries, selectedSource]);

  // handleGenerate triggers the synchronous deep-reading LLM pass for the
  // selected article, swapping to the insights tab on success.
  const handleGenerate = useCallback(async () => {
    if (!agentId || !selectedSource) return;
    setGenerating(true);
    try {
      const res = await kbGenerateInsights(agentId, selectedSource.id);
      if ("error" in res) {
        notify(res.error!);
      } else {
        setInsights(res as ArticleInsights);
        setDetailTab("summary");
      }
    } catch {
      notify(t("knowledge.generateFailed"));
    }
    setGenerating(false);
  }, [agentId, selectedSource, t]);

  // Drag the vertical divider to resize the source list vs. detail pane.
  // Pointer events attach to the document so the drag keeps tracking when the
  // cursor leaves the thin handle. Drag right = grow the left pane.
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

  const handleIngestText = useCallback(async () => {
    if (!agentId || !textContent.trim()) return;
    setSubmitting(true);
    try {
      const res = await kbIngestText(agentId, textTitle || t("knowledge.untitled"), textContent);
      if ("error" in res) { notify(res.error!); } else {
        setTextDialogOpen(false); setTextTitle(""); setTextContent("");
        await loadData();
        if ("source_id" in res) generateWiki(agentId, [res.source_id!]).catch(() => {});
      }
    } catch { notify(t("knowledge.failedAddText")); }
    setSubmitting(false);
  }, [agentId, textTitle, textContent, loadData, t]);

  const handleIngestURL = useCallback(async () => {
    if (!agentId || !urlValue.trim()) return;
    setSubmitting(true);
    try {
      const res = await kbIngestURL(agentId, urlValue, urlTitle || undefined);
      if ("error" in res) { notify(res.error!); } else {
        setUrlDialogOpen(false); setUrlValue(""); setUrlTitle("");
        await loadData();
        if ("source_id" in res) generateWiki(agentId, [res.source_id!]).catch(() => {});
      }
    } catch { notify(t("knowledge.failedFetchURL")); }
    setSubmitting(false);
  }, [agentId, urlValue, urlTitle, loadData, t]);

  const handleDeleteSource = useCallback(async (sourceId: string) => {
    if (!agentId) return;
    try {
      const res = await deleteKBSource(agentId, sourceId);
      if ("error" in res) notify(res.error!); else {
        if (selectedSource?.id === sourceId) { setSelectedSource(null); setEntries([]); }
        loadData();
      }
    } catch {}
  }, [agentId, loadData, selectedSource]);

  const confirmDeleteSource = useCallback(async () => {
    if (!deleteTarget) return;
    await handleDeleteSource(deleteTarget.id);
    setDeleteTarget(null);
  }, [deleteTarget, handleDeleteSource]);

  return (
    <div className="flex h-full flex-col">
      {pending.length > 0 && (
        <div className="border-b border-warning/30 bg-warning/5">
          <button
            type="button"
            onClick={() => setPendingOpen((o) => !o)}
            aria-expanded={pendingOpen}
            className="flex w-full items-center gap-2 px-3 py-2 text-left"
          >
            <AlertTriangleIcon className="h-4 w-4 shrink-0 text-warning" />
            <span className="text-sm font-medium">{t("knowledge.pendingTitle")}</span>
            <Badge variant="outline" className="px-1.5 py-0 text-xs">{pending.length}</Badge>
            <ChevronDownIcon className={cn("ml-auto h-4 w-4 text-muted-foreground transition-transform", pendingOpen && "rotate-180")} />
          </button>
          {pendingOpen && (
            <div className="grid gap-2 px-3 pb-3 sm:grid-cols-2 lg:grid-cols-3">
              {pending.map((p) => (
                <div key={p.id} className="rounded-lg border border-warning/40 bg-background p-2">
                  <p className="truncate text-xs font-medium">{p.title || p.content.slice(0, 30)}</p>
                  <p className="mt-0.5 truncate text-xs text-muted-foreground">
                    ≈ {p.candidate_title}（{(p.similarity * 100).toFixed(0)}%）
                  </p>
                  <div className="mt-1.5 flex gap-1">
                    <Button size="sm" variant="outline" className="h-6 flex-1 text-xs" onClick={() => handleResolvePending(p.id, "merge")}>
                      {t("knowledge.pendingMerge")}
                    </Button>
                    <Button size="sm" variant="outline" className="h-6 flex-1 text-xs" onClick={() => handleResolvePending(p.id, "create")}>
                      {t("knowledge.pendingCreate")}
                    </Button>
                    <Button size="sm" variant="ghost" className="h-6 text-xs" onClick={() => handleResolvePending(p.id, "skip")}>
                      {t("knowledge.pendingSkip")}
                    </Button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
      <div className="flex min-h-0 flex-1">
      <div
        style={{ "--pane-lw": `${leftWidth}px` } as any}
        className={cn(
          "border-r bg-muted/30 flex-col w-full md:w-[var(--pane-lw)] md:shrink-0",
          selectedSource ? "hidden md:flex" : "flex",
        )}
      >
        <div className="border-b">
          <div className="flex items-center gap-2 p-3 pb-2">
            <SearchInput
              value={query}
              onChange={setQuery}
              placeholder={t("knowledge.searchArticles")}
            />
            <Button variant="outline" size="sm" className="h-7 shrink-0 text-xs" onClick={() => setTextDialogOpen(true)}>
              <PlusIcon className="h-3 w-3 mr-1" /> {t("knowledge.text")}
            </Button>
            <Button variant="outline" size="sm" className="h-7 shrink-0 text-xs" onClick={() => setUrlDialogOpen(true)}>
              <PlusIcon className="h-3 w-3 mr-1" /> {t("knowledge.url")}
            </Button>
          </div>
          {stats && (
            <p className="px-3 pb-2 text-xs tabular-nums text-muted-foreground">
              {stats.source_count} {t("knowledge.sources")} · {stats.entry_count} {t("knowledge.entries")} · {(stats.total_chars / 1024).toFixed(1)} KB
            </p>
          )}
        </div>
        <ScrollArea className="flex-1">
          <div className="p-2">
            {loading ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">{t("common.loading")}</p>
            ) : sources.length === 0 ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">{t("knowledge.noSources")}</p>
            ) : visibleSources.length === 0 ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">{t("knowledge.noSearchResult")}</p>
            ) : (
              visibleSources.map((src) => (
                <div
                  key={src.id}
                  role="button"
                  tabIndex={0}
                  className={cn(
                    "group w-full text-left px-3 py-1.5 text-sm rounded-md hover:bg-accent flex items-center gap-2 cursor-pointer",
                    selectedSource?.id === src.id && "bg-accent",
                  )}
                  onClick={() => handleSelectSource(src)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      handleSelectSource(src);
                    }
                  }}
                >
                  <ChevronRightIcon className="h-3 w-3 shrink-0 text-muted-foreground" />
                  <div className="flex-1 min-w-0">
                    <p className="truncate">{src.title}</p>
                    <p className="text-xs tabular-nums text-muted-foreground">
                      {src.entry_count} entries · {(src.total_chars / 1024).toFixed(1)} KB
                    </p>
                  </div>
                  <CopyIconButton
                    sizeClass="h-3.5 w-3.5"
                    value={async () => {
                      const es = await listKBEntries(agentId, src.id);
                      return `# ${src.title}\n\n${es.map((e) => e.content).join("\n")}`;
                    }}
                  />
                  <button
                    type="button"
                    className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 text-muted-foreground hover:text-destructive shrink-0 relative after:absolute after:-inset-2"
                    onClick={(e) => { e.stopPropagation(); setDeleteTarget(src); }}
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

      <div
        onPointerDown={startDrag}
        className="hidden md:block w-1 shrink-0 cursor-col-resize hover:bg-primary/40 transition-colors"
      />

      <div className={cn("flex-1 flex-col min-w-0", selectedSource ? "flex" : "hidden md:flex")}>
        {selectedSource ? (
          <>
            <div className="p-4 border-b">
              <div className="flex items-center gap-3">
                <button
                  type="button"
                  onClick={() => { setSelectedSource(null); setEntries([]); setInsights(null); setDetailTab("text"); }}
                  className="md:hidden -ml-1 shrink-0 text-muted-foreground hover:text-foreground"
                  aria-label={t("common.back")}
                >
                  <ArrowLeftIcon className="h-5 w-5" />
                </button>
                <h1 className="text-xl font-bold truncate flex-1 min-w-0">{selectedSource.title}</h1>
                {insights ? (
                  <Button variant="outline" size="sm" onClick={handleGenerate} disabled={generating} className="shrink-0">
                    <SparklesIcon className="h-3 w-3 mr-1" />
                    {generating ? t("knowledge.generating") : t("knowledge.regenerate")}
                  </Button>
                ) : (
                  <Button size="sm" onClick={handleGenerate} disabled={generating} className="shrink-0">
                    <SparklesIcon className="h-3.5 w-3.5 mr-1.5" />
                    {generating ? t("knowledge.generating") : t("knowledge.generateInsights")}
                  </Button>
                )}
              </div>
              <p className="text-xs tabular-nums text-muted-foreground mt-1">
                {selectedSource.entry_count} {t("knowledge.entries")} · {((selectedSource.total_chars ?? 0) / 1024).toFixed(1)} KB
              </p>
              <div className="mt-3 flex gap-1 overflow-x-auto">
                <button
                  type="button"
                  onClick={() => setDetailTab("text")}
                  className={cn(
                    "inline-flex items-center gap-1 whitespace-nowrap shrink-0 rounded-md px-2.5 py-1 text-xs font-medium transition-colors",
                    detailTab === "text" ? "bg-background text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  <FileTextIcon className="h-3 w-3" />
                  {t("knowledge.rawTextTab")}
                </button>
                {insights &&
                  INSIGHT_SECTIONS.map(([key, label, Icon]) => (
                    <button
                      key={key}
                      type="button"
                      onClick={() => setDetailTab(key)}
                      className={cn(
                        "inline-flex items-center gap-1 whitespace-nowrap shrink-0 rounded-md px-2.5 py-1 text-xs font-medium transition-colors",
                        detailTab === key ? "bg-background text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
                      )}
                    >
                      <Icon className="h-3 w-3" />
                      {t(label)}
                    </button>
                  ))}
              </div>
            </div>
            <ScrollArea className="flex-1">
              <div className="p-6">
                {detailTab === "text" ? (
                  entriesLoading ? (
                    <p className="text-sm text-muted-foreground">{t("common.loading")}</p>
                  ) : entries.length === 0 ? (
                    <p className="text-sm text-muted-foreground">{t("knowledge.noEntries")}</p>
                  ) : (
                    <DocMarkdown
                      className="break-words"
                      components={ARTICLE_MD_COMPONENTS}
                      text={articleText}
                    />
                  )
                ) : insights ? (
                  <InsightSection section={detailTab} insights={insights} />
                ) : null}
              </div>
            </ScrollArea>
          </>
        ) : (
          <div className="flex-1 flex items-center justify-center text-sm text-muted-foreground p-4 text-center">
            {t("knowledge.selectSourcePrompt")}
          </div>
        )}
      </div>
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

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        name={deleteTarget?.title ?? ""}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        onConfirm={confirmDeleteSource}
      />

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

// ── Insights: one section panel of the deep reading ──

// InsightSection renders a single deep-reading panel, selected by the
// article-detail tab bar above. An empty section shows a placeholder so "the
// LLM didn't produce this" is explicit, not a silent UI gap.
function InsightSection({
  section,
  insights,
}: {
  section: Exclude<DetailTab, "text">;
  insights: ArticleInsights;
}) {
  const t = useT();
  return (
    <div>
      {section === "summary" &&
            (insights.summary.core || insights.summary.topics?.length ? (
              <div className="space-y-4">
                {insights.summary.core && <p className="text-sm leading-relaxed">{insights.summary.core}</p>}
                {insights.summary.topics?.length > 0 && (
                  <div className="space-y-3">
                    {insights.summary.topics.map((tp, i) => (
                      <div key={i} className="rounded-md border bg-muted/20 p-3">
                        <p className="text-sm font-semibold mb-1.5">{tp.heading}</p>
                        <ul className="space-y-1">
                          {tp.points?.map((p, j) => (
                            <li key={j} className="text-sm flex gap-2">
                              <span className="text-primary shrink-0 font-medium">{p.label}</span>
                              <span className="text-muted-foreground">{p.text}</span>
                            </li>
                          ))}
                        </ul>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            ) : (
              <EmptyHint />
            ))}

          {section === "chapters" &&
            (insights.summary.chapters?.length ? (
              <div className="space-y-3">
                {insights.summary.chapters.map((ch, i) => (
                  <div key={i} className="border-l-2 border-primary/30 pl-4 py-1">
                    <p className="text-sm font-medium">{ch.title}</p>
                    {ch.body && <p className="text-sm text-muted-foreground mt-1">{ch.body}</p>}
                  </div>
                ))}
              </div>
            ) : (
              <EmptyHint />
            ))}

          {section === "quotes" &&
            (insights.quotes?.length ? (
              <div className="space-y-2">
                {insights.quotes.map((q, i) => (
                  <blockquote key={i} className="border-l-2 border-warning/60 pl-3 py-1">
                    <p className="text-sm italic">{q.text}</p>
                    <div className="mt-1 flex items-center gap-2">
                      {q.tag && <span className="text-xs text-muted-foreground">{q.tag}</span>}
                      {q.verified === true && (
                        <span
                          className="text-xs text-success"
                          title={t("knowledge.quoteVerifiedTip")}
                        >
                          ✓ {t("knowledge.quoteVerified")}
                        </span>
                      )}
                      {q.verified === false && (
                        <span
                          className="text-xs text-muted-foreground"
                          title={t("knowledge.quoteUnverifiedTip")}
                        >
                          ⚠ {t("knowledge.quoteUnverified")}
                        </span>
                      )}
                    </div>
                  </blockquote>
                ))}
              </div>
            ) : (
              <EmptyHint />
            ))}

          {section === "actions" &&
            (insights.actions?.length ? (
              <ul className="space-y-1.5">
                {insights.actions.map((a, i) => (
                  <li key={i} className="text-sm flex gap-2 items-start">
                    <span className="text-primary mt-0.5">▸</span>
                    <span>{a}</span>
                  </li>
                ))}
              </ul>
            ) : (
              <EmptyHint />
            ))}

          {section === "sprouts" &&
            (insights.sprouts.items?.length || insights.sprouts.intro || insights.sprouts.echo ? (
              <div className="space-y-4">
                {insights.sprouts.intro && <p className="text-sm text-muted-foreground">{insights.sprouts.intro}</p>}
                {insights.sprouts.items?.map((sp) => (
                  <div key={sp.index} className="rounded-md border bg-muted/20 p-3">
                    <p className="text-sm font-semibold flex items-center gap-1.5">
                      <span>{sp.emoji || "🌱"}</span>
                      {sp.title}
                    </p>
                    {sp.seed && (
                      <p className="text-xs text-muted-foreground mt-1">
                        <span className="font-medium">{t("knowledge.sproutSeed")}</span>：{sp.seed}
                      </p>
                    )}
                    {sp.body && <p className="text-sm mt-1.5 whitespace-pre-wrap">{sp.body}</p>}
                    {sp.aha && (
                      <p className="text-xs mt-1.5 text-warning">
                        ✨ {t("knowledge.sproutAha")}：{sp.aha}
                      </p>
                    )}
                  </div>
                ))}
                {insights.sprouts.echo && insights.sprouts.echo.items?.length > 0 && (
                  <div className="rounded-md border border-primary/20 bg-primary/5 p-3">
                    <p className="text-sm font-semibold mb-2">{t("knowledge.echoTitle")}</p>
                    {insights.sprouts.echo.seed_quote && (
                      <blockquote className="text-sm italic mb-2">{insights.sprouts.echo.seed_quote}</blockquote>
                    )}
                    {insights.sprouts.echo.seed_comment && (
                      <p className="text-xs text-muted-foreground mb-2">{insights.sprouts.echo.seed_comment}</p>
                    )}
                    <div className="space-y-1.5">
                      {insights.sprouts.echo.items.map((it, i) => (
                        <div key={i} className="text-sm">
                          <span className="text-xs uppercase text-muted-foreground mr-1.5">{it.perspective}</span>
                          <span className="italic">"{it.quote}"</span>
                          {it.source && <span className="text-xs text-muted-foreground"> — {it.source}</span>}
                        </div>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            ) : (
              <EmptyHint />
            ))}
    </div>
  );
}

// EmptyHint is the placeholder shown inside an insights section tab whose LLM
// output came back empty — so an absent section is explicit, not silently gone.
function EmptyHint() {
  const t = useT();
  return <p className="text-sm text-muted-foreground italic">{t("knowledge.emptySection")}</p>;
}

// ── Flashes: 灵感闪记 masonry + "记一笔" capture ──

type FlashItem = { src: KBSource; content: string };

// KB list toolbars share one "add" affordance — h-7 + text-xs, the reading-
// surface mini deliberately below the settings h-8 default. Encoded once so
// the three toolbars can't drift.
function ToolbarAddButton({
  onClick,
  children,
}: {
  onClick: () => void;
  children: string;
}) {
  return (
    <Button size="sm" className="h-7 shrink-0 text-xs" onClick={onClick}>
      <PlusIcon className="h-3 w-3 mr-1" /> {children}
    </Button>
  );
}

export function FlashView({ notify }: { notify: (msg: string) => void }) {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [initialFlash] = useState(() =>
    agentId ? readCache<FlashItem[]>(`kb-flash:${agentId}`) : undefined,
  );
  const [flashes, setFlashes] = useState<FlashItem[]>(initialFlash ?? []);
  const [draft, setDraft] = useState("");
  const [flashOpen, setFlashOpen] = useState(false);
  const [loading, setLoading] = useState(!initialFlash);
  const [saving, setSaving] = useState(false);
  const [query, setQuery] = useState("");
  const [sortNew, setSortNew] = useState(true);
  const [deleteTarget, setDeleteTarget] = useState<FlashItem | null>(null);
  // Manual content edit (the pencil on a card): editTarget is the flash
  // being edited, editDraft its textarea value.
  const [editTarget, setEditTarget] = useState<FlashItem | null>(null);
  const [editDraft, setEditDraft] = useState("");
  const [editSaving, setEditSaving] = useState(false);

  const load = useCallback(async () => {
    if (!agentId) return;
    const silent = !!readCache(`kb-flash:${agentId}`);
    if (!silent) setLoading(true);
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
      writeCache(`kb-flash:${agentId}`, withContent);
    } catch {}
    if (!silent) setLoading(false);
  }, [agentId]);

  useEffect(() => { load(); }, [load]);

  const handleSave = useCallback(async () => {
    if (!agentId || !draft.trim()) return;
    setSaving(true);
    try {
      const res = await kbSaveFlash(agentId, draft.trim());
      if ("error" in res) notify(res.error!); else { setDraft(""); setFlashOpen(false); await load(); }
    } catch { notify(t("knowledge.failedAddText")); }
    setSaving(false);
  }, [agentId, draft, load, t]);

  const handleDelete = useCallback(async (id: string) => {
    if (!agentId) return;
    await deleteKBSource(agentId, id);
    load();
  }, [agentId, load]);

  // handleUpdateFlash saves the edit dialog's content over the existing
  // flash (backend rewrites the chunk, re-derives the title, re-embeds).
  const handleUpdateFlash = useCallback(async () => {
    if (!agentId || !editTarget || !editDraft.trim()) return;
    setEditSaving(true);
    const res = await kbUpdateFlash(agentId, editTarget.src.id, editDraft.trim());
    setEditSaving(false);
    if ("error" in res) { notify(res.error!); return; }
    setEditTarget(null);
    load();
  }, [agentId, editTarget, editDraft, notify, load]);

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    const list = q ? flashes.filter((f) => f.content.toLowerCase().includes(q)) : flashes;
    return [...list].sort((a, b) => {
      const cmp = a.src.created_at < b.src.created_at ? 1 : a.src.created_at > b.src.created_at ? -1 : 0;
      return sortNew ? cmp : -cmp;
    });
  }, [flashes, query, sortNew]);

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2 border-b px-3 py-2">
        <SearchInput value={query} onChange={setQuery} placeholder={t("knowledge.searchFlashes")} />
        <Button size="sm" variant="outline" className="h-7 shrink-0 text-xs" onClick={() => setSortNew((v) => !v)}>
          {sortNew ? t("knowledge.sortNewest") : t("knowledge.sortOldest")}
        </Button>
        <ToolbarAddButton onClick={() => setFlashOpen(true)}>{t("knowledge.flashes")}</ToolbarAddButton>
      </div>
      <ScrollArea className="flex-1">
        <div className="p-4 columns-1 sm:columns-2 lg:columns-3 gap-3">
          {loading ? (
            <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
          ) : flashes.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("knowledge.noFlashes")}</p>
          ) : visible.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("knowledge.noSearchResult")}</p>
          ) : (
            visible.map(({ src, content }) => (
              <div
                key={src.id}
                className="group mb-3 break-inside-avoid rounded-lg border bg-background p-3"
              >
                <DocMarkdown text={content} />
                <div className="mt-2 flex items-center justify-between text-xs text-muted-foreground">
                  <span>{relativeTime(src.created_at)}</span>
                  <div className="flex gap-2">
                    <button
                      type="button"
                      className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 hover:text-primary relative after:absolute after:-inset-2"
                      onClick={() => { setEditDraft(content); setEditTarget({ src, content }); }}
                      aria-label={t("common.edit")}
                    >
                      <PencilIcon className="h-3 w-3" />
                    </button>
                    <CopyIconButton sizeClass="h-3 w-3" value={content} />
                    <button
                      type="button"
                      className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 hover:text-destructive relative after:absolute after:-inset-2"
                      onClick={() => setDeleteTarget({ src, content })}
                      aria-label={t("common.delete")}
                    >
                      <TrashIcon className="h-3 w-3" />
                    </button>
                  </div>
                </div>
              </div>
            ))
          )}
        </div>
      </ScrollArea>

      <Dialog open={flashOpen} onOpenChange={setFlashOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.saveFlash")}</DialogTitle></DialogHeader>
          <div className="space-y-1.5">
            <Label>{t("knowledge.contentLabel")}</Label>
            <textarea
              className="flex min-h-[120px] w-full rounded-md border bg-transparent px-3 py-2 text-sm"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder={t("knowledge.flashPlaceholder")}
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setFlashOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleSave} disabled={saving || !draft.trim()}>
              {saving ? t("common.saving") : t("knowledge.saveFlash")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Flash edit dialog — the pencil's target, same shape as the capture one. */}
      <Dialog open={editTarget !== null} onOpenChange={(o) => { if (!o) setEditTarget(null); }}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.editFlash")}</DialogTitle></DialogHeader>
          <div className="space-y-1.5">
            <Label>{t("knowledge.contentLabel")}</Label>
            <textarea
              className="flex min-h-[120px] w-full rounded-md border bg-transparent px-3 py-2 text-sm"
              value={editDraft}
              onChange={(e) => setEditDraft(e.target.value)}
              placeholder={t("knowledge.flashPlaceholder")}
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditTarget(null)}>{t("common.cancel")}</Button>
            <Button onClick={handleUpdateFlash} disabled={editSaving || !editDraft.trim()}>
              {editSaving ? t("common.saving") : t("common.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        name={deleteTarget?.content ?? ""}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        onConfirm={async () => {
          if (!deleteTarget) return;
          await handleDelete(deleteTarget.src.id);
          setDeleteTarget(null);
        }}
      />
    </div>
  );
}

// ── Bookmarks: saved web links (URL + title/summary + fetched body) ──

export function BookmarkView({ notify }: { notify: (msg: string) => void }) {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [initialBm] = useState(() =>
    agentId ? readCache<KBBookmark[]>(`kb-bookmark:${agentId}`) : undefined,
  );
  const [bookmarks, setBookmarks] = useState<KBBookmark[]>(initialBm ?? []);
  const [addOpen, setAddOpen] = useState(false);
  const [loading, setLoading] = useState(!initialBm);
  const [saving, setSaving] = useState(false);
  const [query, setQuery] = useState("");
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [url, setUrl] = useState("");
  const [title, setTitle] = useState("");
  const [summary, setSummary] = useState("");
  const [promoting, setPromoting] = useState<string | null>(null);
  const [editId, setEditId] = useState<string | null>(null);
  const [editTitle, setEditTitle] = useState("");
  const [editSummary, setEditSummary] = useState("");
  const [editSaving, setEditSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<KBBookmark | null>(null);

  const load = useCallback(async () => {
    if (!agentId) return;
    const silent = !!readCache(`kb-bookmark:${agentId}`);
    if (!silent) setLoading(true);
    try {
      const list = await listBookmarks(agentId);
      list.sort((a, b) => (a.created_at < b.created_at ? 1 : -1));
      setBookmarks(list);
      writeCache(`kb-bookmark:${agentId}`, list);
    } catch {}
    if (!silent) setLoading(false);
  }, [agentId]);

  useEffect(() => { load(); }, [load]);

  const handleSave = useCallback(async () => {
    if (!agentId || !url.trim()) return;
    setSaving(true);
    try {
      const res = await saveBookmark(agentId, url.trim(), title.trim() || undefined, summary.trim() || undefined);
      if (res.error) notify(res.error);
      else { setUrl(""); setTitle(""); setSummary(""); setAddOpen(false); await load(); }
    } catch { notify(t("knowledge.failedAddText")); }
    setSaving(false);
  }, [agentId, url, title, summary, load, t]);

  const handleDelete = useCallback(async (id: string) => {
    if (!agentId) return;
    await deleteBookmark(agentId, id);
    load();
  }, [agentId, load]);

  const handlePromote = useCallback(async (id: string) => {
    if (!agentId) return;
    setPromoting(id);
    try {
      const res = await promoteBookmark(agentId, id);
      if (res.error) notify(res.error);
      else await load();
    } catch { notify(t("knowledge.failedAddText")); }
    setPromoting(null);
  }, [agentId, load, notify, t]);

  const openEdit = useCallback((b: KBBookmark) => {
    setEditId(b.id);
    setEditTitle(b.title || "");
    setEditSummary(b.summary || "");
  }, []);

  const handleEditSave = useCallback(async () => {
    if (!agentId || !editId) return;
    setEditSaving(true);
    try {
      const res = await updateBookmark(agentId, editId, { title: editTitle, summary: editSummary });
      if (res.error) notify(res.error);
      else { setEditId(null); await load(); }
    } catch { notify(t("knowledge.failedAddText")); }
    setEditSaving(false);
  }, [agentId, editId, editTitle, editSummary, load, notify, t]);

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return bookmarks;
    return bookmarks.filter((b) =>
      `${b.title} ${b.summary} ${b.url}`.toLowerCase().includes(q),
    );
  }, [bookmarks, query]);

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2 border-b px-3 py-2">
        <SearchInput value={query} onChange={setQuery} placeholder={t("knowledge.searchBookmarks")} />
        <ToolbarAddButton onClick={() => setAddOpen(true)}>{t("knowledge.bookmarks")}</ToolbarAddButton>
      </div>
      <ScrollArea className="flex-1">
        <div className="columns-1 gap-3 p-4 sm:columns-2 lg:columns-3 xl:columns-4">
          {loading ? (
            <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
          ) : bookmarks.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("knowledge.noBookmarks")}</p>
          ) : visible.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("knowledge.noSearchResult")}</p>
          ) : (
            visible.map((b) => (
              <div key={b.id} className="group mb-3 break-inside-avoid rounded-lg border bg-background p-3">
                <div className="flex items-start justify-between gap-2">
                  <div className="min-w-0 flex-1">
                    <a href={b.url} target="_blank" rel="noopener noreferrer" className="block break-words text-sm font-medium hover:underline">
                      {b.title || b.url}
                    </a>
                    <a href={b.url} target="_blank" rel="noopener noreferrer" className="block break-all text-xs text-muted-foreground hover:underline">
                      {b.url}
                    </a>
                  </div>
                  <div className="flex shrink-0 items-center gap-0.5">
                    <button
                      type="button"
                      className="rounded-md p-1 text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 hover:text-foreground"
                      onClick={() => openEdit(b)}
                      aria-label={t("common.edit")}
                    >
                      <PencilIcon className="h-3.5 w-3.5" />
                    </button>
                    <button
                      type="button"
                      className="rounded-md p-1 text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 hover:text-destructive"
                      onClick={() => setDeleteTarget(b)}
                      aria-label={t("common.delete")}
                    >
                      <TrashIcon className="h-3.5 w-3.5" />
                    </button>
                  </div>
                </div>
                {b.summary && (
                  <p className="mt-1.5 whitespace-pre-wrap break-words text-xs text-muted-foreground">{b.summary}</p>
                )}
                <div className="mt-2 flex flex-wrap items-center justify-between gap-x-2 gap-y-1">
                  <span className="min-w-0 break-words text-xs text-muted-foreground">
                    {relativeTime(b.created_at)}{b.content ? ` · ${t("knowledge.bookmarkBody", { n: b.content.length })}` : ""}
                    {b.promoted_to_article_id && (
                      <span className="ml-2 text-success">{t("knowledge.bookmarkPromoted")}</span>
                    )}
                  </span>
                  <div className="flex shrink-0 items-center gap-1.5">
                    {b.content && (
                      <Button variant="outline" size="sm" className="h-6 px-2 text-xs" onClick={() => setExpanded((m) => ({ ...m, [b.id]: !m[b.id] }))}>
                        {expanded[b.id] ? t("knowledge.bookmarkHideBody") : t("knowledge.bookmarkShowBody")}
                      </Button>
                    )}
                    {!b.promoted_to_article_id && (
                      <Button variant="outline" size="sm" className="h-6 px-2 text-xs" disabled={promoting === b.id} onClick={() => handlePromote(b.id)}>
                        {promoting === b.id ? t("common.saving") : t("knowledge.bookmarkPromote")}
                      </Button>
                    )}
                  </div>
                </div>
                {b.content && expanded[b.id] && (
                  <pre className="mt-2 max-h-80 overflow-auto whitespace-pre-wrap break-all rounded bg-muted/40 p-2 text-xs">{b.content}</pre>
                )}
              </div>
            ))
          )}
        </div>
      </ScrollArea>

      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.addBookmark")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("knowledge.bookmarkUrlLabel")}</Label>
              <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder={t("knowledge.bookmarkUrlPlaceholder")} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("knowledge.bookmarkTitleLabel")}</Label>
              <Input value={title} onChange={(e) => setTitle(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("knowledge.bookmarkSummaryLabel")}</Label>
              <textarea
                className="flex min-h-[80px] w-full rounded-md border bg-transparent px-3 py-2 text-sm"
                value={summary}
                onChange={(e) => setSummary(e.target.value)}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleSave} disabled={saving || !url.trim()}>
              {saving ? t("common.saving") : t("knowledge.addBookmark")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={editId !== null} onOpenChange={(o) => { if (!o) setEditId(null); }}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.bookmarkEdit")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("knowledge.bookmarkTitleLabel")}</Label>
              <Input value={editTitle} onChange={(e) => setEditTitle(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("knowledge.bookmarkSummaryLabel")}</Label>
              <textarea
                className="flex min-h-[80px] w-full rounded-md border bg-transparent px-3 py-2 text-sm"
                value={editSummary}
                onChange={(e) => setEditSummary(e.target.value)}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditId(null)}>{t("common.cancel")}</Button>
            <Button onClick={handleEditSave} disabled={editSaving}>
              {editSaving ? t("common.saving") : t("common.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        name={deleteTarget ? (deleteTarget.title || deleteTarget.url) : ""}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        onConfirm={async () => {
          if (!deleteTarget) return;
          await handleDelete(deleteTarget.id);
          setDeleteTarget(null);
        }}
      />
    </div>
  );
}

// ── Todos: kanban by status ──

export function TodoView({ notify }: { notify: (msg: string) => void }) {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [initialTodo] = useState(() =>
    agentId ? readCache<FlashItem[]>(`kb-todo:${agentId}`) : undefined,
  );
  const [todos, setTodos] = useState<FlashItem[]>(initialTodo ?? []);
  const [loading, setLoading] = useState(!initialTodo);
  const [newOpen, setNewOpen] = useState(false);
  const [newContent, setNewContent] = useState("");
  const [newStart, setNewStart] = useState("");
  const [newEnd, setNewEnd] = useState("");
  const [creating, setCreating] = useState(false);
  const [view, setView] = useState<"board" | "calendar" | "list">("board");
  // calGran is the shared day-column granularity of the calendar + gantt
  // panes: month grid, single week, or a single day (gantt goes hourly).
  const [calGran, setCalGran] = useState<CalGran>("month");
  // calAnchor is the shared range anchor of the calendar + gantt panes:
  // paging either pane keeps both on the same month/week/day.
  const [calAnchor, setCalAnchor] = useState(() => new Date());
  const [deleteTarget, setDeleteTarget] = useState<FlashItem | null>(null);
  const [detailTarget, setDetailTarget] = useState<FlashItem | null>(null);
  const [archiveOpen, setArchiveOpen] = useState<Record<string, boolean>>({});
  const [query, setQuery] = useState("");

  const load = useCallback(async () => {
    if (!agentId) return;
    const silent = !!readCache(`kb-todo:${agentId}`);
    if (!silent) setLoading(true);
    try {
      const all = await kbListTodos(agentId);
      const withContent = await Promise.all(
        all.map(async (src) => {
          const es = await listKBEntries(agentId, src.id);
          return { src, content: es.map((e) => e.content).join("\n") } as FlashItem;
        }),
      );
      setTodos(withContent);
      writeCache(`kb-todo:${agentId}`, withContent);
    } catch {}
    if (!silent) setLoading(false);
  }, [agentId]);

  useEffect(() => { load(); }, [load]);

  // Keep the detail dialog fresh after load() swaps the FlashItem objects
  // (status/date edits re-fetch the whole list).
  useEffect(() => {
    setDetailTarget((prev) => (prev ? todos.find((it) => it.src.id === prev.src.id) ?? null : prev));
  }, [todos]);

  // openCreate opens the new-todo dialog with optional prefilled dates —
  // calendar empty-cell clicks pass the clicked day as the due date.
  const openCreate = useCallback((start?: string, end?: string) => {
    setNewStart(start ?? "");
    setNewEnd(end ?? "");
    setNewOpen(true);
  }, []);

  const handleCreate = useCallback(async () => {
    if (!agentId || !newContent.trim()) return;
    setCreating(true);
    try {
      const startAt = newStart ? new Date(newStart).toISOString() : undefined;
      const endAt = newEnd ? new Date(newEnd).toISOString() : undefined;
      const res = await kbSaveTodo(agentId, newContent.trim(), "pending", startAt, endAt);
      if ("error" in res) notify(res.error!); else {
        setNewOpen(false); setNewContent(""); setNewStart(""); setNewEnd(""); load();
      }
    } catch { notify(t("knowledge.failedAddText")); }
    setCreating(false);
  }, [agentId, newContent, newStart, newEnd, load, t]);

  const handleMove = useCallback(async (id: string, status: TodoStatus) => {
    if (!agentId) return;
    const res = await kbUpdateTodo(agentId, id, { status });
    if ("error" in res) notify(res.error!); else load();
  }, [agentId, load]);

  // handlePatch saves the content/start/due edits from the detail dialog.
  // Undefined keys are omitted from the PATCH, so a cleared input field
  // simply leaves the stored value unchanged.
  const handlePatch = useCallback(async (id: string, patch: { content?: string; start_at?: string; end_at?: string }) => {
    if (!agentId) return;
    const res = await kbUpdateTodo(agentId, id, patch);
    if ("error" in res) notify(res.error!); else load();
  }, [agentId, load]);

  const handleDelete = useCallback(async (id: string) => {
    if (!agentId) return;
    await deleteKBSource(agentId, id);
    load();
  }, [agentId, load]);

  // TodoCard reports only the id; look the item up so the confirm dialog can
  // quote its content, then delete on confirm.
  const askDelete = useCallback((id: string) => {
    setDeleteTarget(todos.find((it) => it.src.id === id) ?? null);
  }, [todos]);

  // visibleTodos applies the search-box content filter shared by the board,
  // calendar and list views.
  const visibleTodos = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? todos.filter((it) => it.content.toLowerCase().includes(q)) : todos;
  }, [todos, query]);

  const byStatus = useMemo(() => {
    const m: Record<TodoStatus, FlashItem[]> = { pending: [], in_progress: [], done: [], cancelled: [] };
    for (const it of visibleTodos) {
      const s = (it.src.status || "pending") as TodoStatus;
      if (m[s]) m[s].push(it);
    }
    return m;
  }, [visibleTodos]);

  // dueSoon: active todos with a due date, split into overdue vs. due-today
  // for the urgency summary above the board.
  const { overdue: overdueTodos, today: dueToday } = useMemo(() => {
    const now = Date.now();
    const overdue: FlashItem[] = [];
    const today: FlashItem[] = [];
    for (const it of visibleTodos) {
      if (it.src.status !== "pending" && it.src.status !== "in_progress") continue;
      if (!it.src.end_at) continue;
      const due = new Date(it.src.end_at).getTime();
      if (due < now) overdue.push(it);
      else if (due <= endOfToday(now)) today.push(it);
    }
    return { overdue, today };
  }, [visibleTodos]);

  // listGroups buckets the visible todos by due date for the list view:
  // overdue / today / tomorrow / this week / later / undated, plus the done
  // and cancelled archives (rendered collapsed at the bottom).
  const listGroups = useMemo(() => {
    const { now, eod, eodTomorrow, eodWeek } = dueBounds();
    const g: Record<TodoGroupKey, FlashItem[]> = {
      overdue: [], today: [], tomorrow: [], week: [], later: [], undated: [],
      done: [], cancelled: [],
    };
    for (const it of visibleTodos) {
      const st = it.src.status || "pending";
      if (st === "done" || st === "cancelled") { g[st].push(it); continue; }
      if (!it.src.end_at) { g.undated.push(it); continue; }
      const due = new Date(it.src.end_at).getTime();
      if (due < now) g.overdue.push(it);
      else if (due <= eod.getTime()) g.today.push(it);
      else if (due <= eodTomorrow.getTime()) g.tomorrow.push(it);
      else if (due <= eodWeek.getTime()) g.week.push(it);
      else g.later.push(it);
    }
    return g;
  }, [visibleTodos]);

  return (
    <div className="flex h-full flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2">
        <SearchInput value={query} onChange={setQuery} placeholder={t("knowledge.searchTodos")} />
        <div className="flex shrink-0 items-center rounded-md border p-0.5">
          <button type="button" onClick={() => setView("board")} className={cn("rounded px-2 py-1 text-xs", view === "board" ? "bg-background text-foreground shadow-sm" : "text-muted-foreground")}>{t("knowledge.viewBoard")}</button>
          <button type="button" onClick={() => setView("calendar")} className={cn("rounded px-2 py-1 text-xs", view === "calendar" ? "bg-background text-foreground shadow-sm" : "text-muted-foreground")}>{t("knowledge.viewCalendar")}</button>
          <button type="button" onClick={() => setView("list")} className={cn("rounded px-2 py-1 text-xs", view === "list" ? "bg-background text-foreground shadow-sm" : "text-muted-foreground")}>{t("knowledge.viewList")}</button>
        </div>
        {view === "calendar" && (
          <div className="flex shrink-0 items-center rounded-md border p-0.5">
            {CAL_GRANS.map((g) => (
              <button
                key={g}
                type="button"
                onClick={() => setCalGran(g)}
                className={cn("rounded px-2 py-1 text-xs", calGran === g ? "bg-background text-foreground shadow-sm" : "text-muted-foreground")}
              >
                {t("knowledge.gran_" + g)}
              </button>
            ))}
          </div>
        )}
        <ToolbarAddButton onClick={() => openCreate()}>{t("knowledge.todos")}</ToolbarAddButton>
      </div>
      {(overdueTodos.length > 0 || dueToday.length > 0) && (
        <div className="flex items-center gap-3 border-b px-4 py-1.5 text-xs">
          {overdueTodos.length > 0 && <span className="font-medium text-destructive">{t("knowledge.overdue")} {overdueTodos.length}</span>}
          {dueToday.length > 0 && <span className="font-medium text-warning">{t("knowledge.dueToday")} {dueToday.length}</span>}
        </div>
      )}
      <ScrollArea className="flex-1">
        {view === "list" ? (
          <div className="mx-auto max-w-3xl space-y-5 p-4">
            {loading ? (
              <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
            ) : visibleTodos.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t("knowledge.noTodos")}</p>
            ) : (
              <>
                {TODO_LIST_GROUPS.map(({ key, label, accent }) =>
                  listGroups[key].length === 0 ? null : (
                    <section key={key}>
                      <h3 className={cn("mb-1.5 px-0.5 text-xs font-semibold uppercase tracking-wide", accent)}>
                        {t(label)} ({listGroups[key].length})
                      </h3>
                      <div className="space-y-1.5">
                        {listGroups[key].map((it) => (
                          <TodoRow key={it.src.id} item={it} onOpen={setDetailTarget} onDelete={askDelete} />
                        ))}
                      </div>
                    </section>
                  ),
                )}
                {(["done", "cancelled"] as const).map((st) =>
                  listGroups[st].length === 0 ? null : (
                    <div key={st} className="overflow-hidden rounded-lg border">
                      <button
                        type="button"
                        onClick={() => setArchiveOpen((o) => ({ ...o, [st]: !(o[st] ?? false) }))}
                        className="rounded-md flex w-full items-center justify-between px-3 py-2 hover:bg-accent/50"
                      >
                        <span className={cn("text-xs font-semibold uppercase tracking-wide", statusAccent(st))}>
                          {t("knowledge.status_" + st)} ({listGroups[st].length})
                        </span>
                        <ChevronDownIcon className={cn("h-3.5 w-3.5 text-muted-foreground transition-transform", archiveOpen[st] && "rotate-180")} />
                      </button>
                      {archiveOpen[st] && (
                        <div className="space-y-1.5 border-t p-2">
                          {listGroups[st].map((it) => (
                            <TodoRow key={it.src.id} item={it} onOpen={setDetailTarget} onDelete={askDelete} />
                          ))}
                        </div>
                      )}
                    </div>
                  ),
                )}
              </>
            )}
          </div>
        ) : view === "calendar" ? (
          // Stacked, full-width panes: side-by-side squeezed both surfaces;
          // each keeps its own MonthNav and scrolls in the shared area.
          <div className="flex flex-col">
            <div className="border-b">
              <TodoCalendar
                items={visibleTodos}
                anchor={calAnchor}
                setAnchor={setCalAnchor}
                gran={calGran}
                onOpenItem={setDetailTarget}
                onCreateAt={(day) => openCreate(undefined, `${day}T09:00`)}
                onDelete={askDelete}
              />
            </div>
            <TodoGantt
              items={visibleTodos}
              loading={loading}
              anchor={calAnchor}
              setAnchor={setCalAnchor}
              gran={calGran}
              onOpenItem={setDetailTarget}
              onDelete={askDelete}
            />
          </div>
        ) : (
        // Kanban columns: single stack below sm — phone drag-and-drop
        // doesn't exist (HTML5 DnD is pointer-only), so the side-by-side
        // column model buys nothing there and 2-up squeezed cards into
        // 4-line wraps. sm keeps 2-up, lg restores the 4-column board.
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3 p-4">
          {TODO_STATUSES.map((st) => (
            <div
              key={st}
              onDragOver={(e) => e.preventDefault()}
              onDrop={(e) => {
                e.preventDefault();
                const id = e.dataTransfer.getData("text/plain");
                if (id) handleMove(id, st);
              }}
              className="flex flex-col rounded-lg bg-muted/30 min-h-[200px] transition-colors"
            >
              <div className="p-2 border-b flex items-center justify-between">
                <span className={cn("text-xs font-semibold uppercase tracking-wide", statusAccent(st))}>
                  {t("knowledge.status_" + st)}
                </span>
                <Badge variant="outline" className="text-xs px-1.5 py-0">{byStatus[st].length}</Badge>
              </div>
              <div className="flex-1 p-2 space-y-2">
                {loading ? (
                  <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
                ) : byStatus[st].map(({ src, content }) => (
                  <TodoCard
                    key={src.id}
                    src={src}
                    content={content}
                    onDelete={askDelete}
                    onMove={handleMove}
                  />
                ))}
              </div>
            </div>
          ))}
        </div>
        )}
      </ScrollArea>

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        name={deleteTarget?.content ?? ""}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        onConfirm={async () => {
          if (!deleteTarget) return;
          await handleDelete(deleteTarget.src.id);
          setDeleteTarget(null);
        }}
      />

      <TodoDetailDialog
        key={detailTarget?.src.id ?? "none"}
        item={detailTarget}
        onMove={handleMove}
        onPatch={handlePatch}
        onDelete={(id) => { setDetailTarget(null); askDelete(id); }}
        onOpenChange={(o) => { if (!o) setDetailTarget(null); }}
      />

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
              <Label>{t("knowledge.startLabel")}</Label>
              <Input type="datetime-local" value={newStart} onChange={(e) => setNewStart(e.target.value)} />
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
  onDelete,
  onMove,
}: {
  src: KBSource;
  content: string;
  onDelete: (id: string) => void;
  onMove: (id: string, status: TodoStatus) => void;
}) {
  const t = useT();
  const overdue = src.end_at && new Date(src.end_at).getTime() < Date.now() && (src.status === "pending" || src.status === "in_progress");
  return (
    <div
      draggable
      tabIndex={0}
      aria-label={`${content.slice(0, 40)} · ${t("knowledge.status_" + (src.status || "pending"))} · ${t("knowledge.move")}`}
      onKeyDown={(e) => {
        if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
        e.preventDefault();
        const idx = TODO_STATUSES.indexOf((src.status || "pending") as TodoStatus);
        if (idx < 0) return;
        const next =
          e.key === "ArrowRight" ? Math.min(TODO_STATUSES.length - 1, idx + 1) : Math.max(0, idx - 1);
        if (next !== idx) onMove(src.id, TODO_STATUSES[next]);
      }}
      onDragStart={(e) => {
        e.dataTransfer.setData("text/plain", src.id);
        e.dataTransfer.effectAllowed = "move";
      }}
      title={t("knowledge.move")}
      className="group rounded-lg border bg-background p-2.5 text-sm shadow-sm cursor-grab active:cursor-grabbing hover:border-primary/40 focus-visible:outline-2 focus-visible:outline-offset-2 transition-colors"
    >
      {src.status === "cancelled" && (
        <span className="mb-1.5 inline-flex items-center rounded-md border border-border px-1.5 py-0.5 text-2xs text-muted-foreground">
          {t("knowledge.status_cancelled")}
        </span>
      )}
      <DocMarkdown
        className={cn(src.status === "cancelled" && "opacity-75")}
        text={content}
      />
      {src.end_at && (
        <p className={cn("mt-1.5 text-xs", overdue ? "text-destructive font-medium" : "text-muted-foreground")}>
          {t("knowledge.dueLabel")}: {datetimeLocalValue(src.end_at).replace("T", " ")}
        </p>
      )}
      <div className="mt-2 flex justify-end gap-2">
        <CopyIconButton sizeClass="h-3 w-3" value={content} />
        <button
          type="button"
          className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 text-muted-foreground hover:text-destructive relative after:absolute after:-inset-2"
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
      return "text-info";
    case "done":
      return "text-success";
    case "cancelled":
      return "text-muted-foreground";
  }
  return "";
}

// --- Calendar + compact list (shared by the calendar and list views) ---

type TodoGroupKey = "overdue" | "today" | "tomorrow" | "week" | "later" | "undated" | "done" | "cancelled";

// TODO_LIST_GROUPS drives the list view's urgency sections; done/cancelled
// render separately as collapsed archives at the bottom.
const TODO_LIST_GROUPS: ReadonlyArray<{ key: Exclude<TodoGroupKey, "done" | "cancelled">; label: string; accent: string }> = [
  { key: "overdue", label: "knowledge.tgOverdue", accent: "text-destructive" },
  { key: "today", label: "knowledge.today", accent: "text-warning" },
  { key: "tomorrow", label: "knowledge.tgTomorrow", accent: "text-muted-foreground" },
  { key: "week", label: "knowledge.tgWeek", accent: "text-muted-foreground" },
  { key: "later", label: "knowledge.tgLater", accent: "text-muted-foreground" },
  { key: "undated", label: "knowledge.noDate", accent: "text-muted-foreground" },
];

// todoOverdue: active todo whose due timestamp is in the past.
function todoOverdue(src: KBSource): boolean {
  return !!src.end_at && new Date(src.end_at).getTime() < Date.now() &&
    (src.status === "pending" || src.status === "in_progress");
}

// dueBounds returns the current-time bucket edges for the list view.
function dueBounds() {
  const now = Date.now();
  const eod = new Date(endOfToday(now));
  const eodTomorrow = new Date(eod); eodTomorrow.setDate(eod.getDate() + 1);
  const eodWeek = new Date(eod); eodWeek.setDate(eod.getDate() + 7);
  return { now, eod, eodTomorrow, eodWeek };
}

// todoFirstLine pulls the first non-empty line out of a markdown todo body
// for compact one-line chips/rows; heading hashes are stripped.
function todoFirstLine(content: string): string {
  const line = content.split("\n").find((l) => l.trim());
  return (line ?? "").replace(/^#{1,6}\s*/, "").trim();
}

// dayKey lives in lib/time.ts — the join key between todo timestamps and
// calendar cells (calendar days, not instants).

// monthGrid returns the 42 days (6 weeks, Monday-first) covering anchor's
// month so a todo spanning a week boundary still renders as aligned bars.
function monthGrid(anchor: Date): Date[] {
  const first = new Date(anchor.getFullYear(), anchor.getMonth(), 1);
  const start = new Date(first);
  start.setDate(first.getDate() - ((first.getDay() + 6) % 7));
  return Array.from({ length: 42 }, (_, i) => {
    const d = new Date(start);
    d.setDate(start.getDate() + i);
    return d;
  });
}

// CalGran is the day-column granularity shared by the calendar + gantt
// panes; CAL_GRANS drives the toolbar segmented control in that order.
type CalGran = "month" | "week" | "day";
const CAL_GRANS: readonly CalGran[] = ["month", "week", "day"];

// startOfWeek returns the Monday (00:00) of anchor's week — the Monday-first
// convention monthGrid uses, so week cells align with month-grid weeks.
function startOfWeek(anchor: Date): Date {
  const d = new Date(anchor.getFullYear(), anchor.getMonth(), anchor.getDate());
  d.setDate(d.getDate() - ((d.getDay() + 6) % 7));
  return d;
}

// startOfDay returns anchor's local midnight (the day-column join key).
function startOfDay(anchor: Date): Date {
  return new Date(anchor.getFullYear(), anchor.getMonth(), anchor.getDate());
}

// weekLabels returns the seven weekday abbreviations Monday-first (2024-01-01
// was a Monday) — shared by the calendar grid header and the gantt week view.
function weekLabels(locale: string): string[] {
  return Array.from({ length: 7 }, (_, i) =>
    new Intl.DateTimeFormat(locale, { weekday: "short" }).format(new Date(2024, 0, 1 + i)));
}

// fmtShort renders a timestamp as "M/D" (plus "HH:mm" when it carries a
// time-of-day) for compact list rows.
function fmtShort(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const md = `${d.getMonth() + 1}/${d.getDate()}`;
  if (!d.getHours() && !d.getMinutes()) return md;
  const p = (n: number) => String(n).padStart(2, "0");
  return `${md} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// todoSpanText renders a todo's date coverage: "M/D – M/D" when start and
// due fall on different days, else the single timestamp.
function todoSpanText(src: KBSource): string {
  const s = src.start_at, e = src.end_at;
  if (s && e) {
    const sameDay = dayKey(new Date(s)) === dayKey(new Date(e));
    return sameDay ? fmtShort(e) : `${fmtShort(s)} – ${fmtShort(e)}`;
  }
  return fmtShort(e || s || "");
}

// statusBar / statusChipBg carry the same four lifecycle colors as the
// kanban column headers (statusAccent) into list rows and calendar chips.
function statusBar(s: TodoStatus): string {
  switch (s) {
    case "pending":
      return "bg-muted-foreground/40";
    case "in_progress":
      return "bg-info";
    case "done":
      return "bg-success";
    case "cancelled":
      return "bg-muted-foreground/25";
  }
  return "";
}
function statusChipBg(s: TodoStatus): string {
  switch (s) {
    case "pending":
      return "bg-muted-foreground/30 text-foreground/90 ring-1 ring-inset ring-muted-foreground/25";
    case "in_progress":
      return "bg-info/25 text-foreground/90 ring-1 ring-inset ring-info/45";
    case "done":
      return "bg-success/25 text-foreground/90 ring-1 ring-inset ring-success/45";
    case "cancelled":
      return "bg-muted/60 text-muted-foreground/80 line-through";
  }
  return "";
}

// ganttBarStyle colors the bar by lifecycle status — the background carries
// the status color (same four accents as the chips above) so a status reads
// at a glance across every todo surface.
function ganttBarStyle(s: TodoStatus): string {
  switch (s) {
    case "pending":
      return "bg-muted-foreground/30 text-foreground/90 ring-muted-foreground/30";
    case "in_progress":
      return "bg-info/25 text-foreground/90 ring-info/50";
    case "done":
      return "bg-success/25 text-foreground/90 ring-success/50";
    case "cancelled":
      return "bg-muted/70 text-muted-foreground/80 ring-border";
  }
  return "";
}

// TodoRow is the compact row shared by the list view, the calendar's
// undated strip and the day sheet: status color bar + wrapping title +
// date span. Click opens the detail dialog.
function TodoRow({
  item,
  onOpen,
  onDelete,
}: {
  item: FlashItem;
  onOpen: (it: FlashItem) => void;
  onDelete: (id: string) => void;
}) {
  const t = useT();
  const st = (item.src.status || "pending") as TodoStatus;
  const overdue = todoOverdue(item.src);
  const span = todoSpanText(item.src);
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={() => onOpen(item)}
      onKeyDown={(e) => {
        if (e.key !== "Enter" && e.key !== " ") return;
        e.preventDefault();
        onOpen(item);
      }}
      className="group flex cursor-pointer items-center gap-2.5 rounded-md border bg-background px-2.5 py-1.5 hover:border-primary/40 focus-visible:outline-2 focus-visible:outline-offset-2"
    >
      <span className={cn("h-7 w-1 shrink-0 rounded-full", statusBar(st))} />
      <span className={cn("min-w-0 flex-1 break-words text-sm", st === "cancelled" && "line-through opacity-70")}>
        {todoFirstLine(item.content)}
      </span>
      {span && (
        <span className={cn("shrink-0 text-xs", overdue ? "font-medium text-destructive" : "text-muted-foreground")}>
          {span}
        </span>
      )}
      <CopyIconButton sizeClass="h-3 w-3" value={item.content} />
      <button
        type="button"
        onClick={(e) => { e.stopPropagation(); onDelete(item.src.id); }}
        aria-label={t("common.delete")}
        className="shrink-0 text-muted-foreground opacity-0 hover:text-destructive focus-visible:opacity-100 group-hover:opacity-100"
      >
        <TrashIcon className="h-3 w-3" />
      </button>
    </div>
  );
}

// TodoDetailDialog is the shared editor opened from calendar chips and list
// rows: the markdown body (pencil toggles it into a raw textarea for content
// edits), the four status pills and start/due datetime editing. Patched
// fields left blank are simply omitted, so they leave the stored values
// unchanged. The caller passes a key of the todo id so a different todo
// remounts (and reseeds) the datetime inputs; edits that reload the same
// todo keep the field values.
function TodoDetailDialog({
  item,
  onMove,
  onPatch,
  onDelete,
  onOpenChange,
}: {
  item: FlashItem | null;
  onMove: (id: string, status: TodoStatus) => void;
  onPatch: (id: string, patch: { content?: string; start_at?: string; end_at?: string }) => void | Promise<void>;
  onDelete: (id: string) => void;
  onOpenChange: (o: boolean) => void;
}) {
  const t = useT();
  const [start, setStart] = useState(() => (item?.src.start_at ? datetimeLocalValue(item.src.start_at) : ""));
  const [end, setEnd] = useState(() => (item?.src.end_at ? datetimeLocalValue(item.src.end_at) : ""));
  // Content editing: draft starts null (not editing); the pencil seeds it
  // with the current body and flips the preview into a textarea.
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<string | null>(null);
  if (!item) return null;
  const cur = (item.src.status || "pending") as TodoStatus;
  const origStart = item.src.start_at ? datetimeLocalValue(item.src.start_at) : "";
  const origEnd = item.src.end_at ? datetimeLocalValue(item.src.end_at) : "";
  const contentDirty = draft !== null && draft.trim() !== item.content.trim();
  const dirty = start !== origStart || end !== origEnd || contentDirty;
  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader><DialogTitle>{t("knowledge.todoDetail")}</DialogTitle></DialogHeader>
        {editing ? (
          <textarea
            className="flex max-h-64 min-h-[120px] w-full resize-y rounded-md border bg-transparent px-3 py-2 text-sm"
            value={draft ?? ""}
            onChange={(e) => setDraft(e.target.value)}
            placeholder={t("knowledge.todoContentPlaceholder")}
            autoFocus
          />
        ) : (
          <div className="relative">
            <DocMarkdown className="max-h-64 overflow-y-auto" text={item.content} />
            <button
              type="button"
              onClick={() => { setDraft(item.content); setEditing(true); }}
              className="absolute right-0 top-0 rounded-md p-1 text-muted-foreground transition-colors hover:text-primary"
              aria-label={t("common.edit")}
              title={t("common.edit")}
            >
              <PencilIcon className="h-3.5 w-3.5" />
            </button>
          </div>
        )}
        <div className="flex flex-wrap gap-1.5">
          {TODO_STATUSES.map((st) => (
            <button
              key={st}
              type="button"
              onClick={() => onMove(item.src.id, st)}
              className={cn(
                "rounded-md border px-2.5 py-1 text-xs transition-colors",
                cur === st
                  ? cn("border-current font-medium", statusAccent(st))
                  : "text-muted-foreground hover:border-foreground/30",
              )}
            >
              {t("knowledge.status_" + st)}
            </button>
          ))}
        </div>
        <div className="grid grid-cols-2 gap-2">
          <div className="space-y-1">
            <Label className="text-xs">{t("knowledge.startLabel")}</Label>
            <Input type="datetime-local" value={start} onChange={(e) => setStart(e.target.value)} className="h-8 text-xs" />
          </div>
          <div className="space-y-1">
            <Label className="text-xs">{t("knowledge.dueLabel")}</Label>
            <Input type="datetime-local" value={end} onChange={(e) => setEnd(e.target.value)} className="h-8 text-xs" />
          </div>
        </div>
        <DialogFooter className="sm:justify-between">
          <Button
            variant="ghost"
            size="sm"
            className="h-7 text-destructive hover:text-destructive"
            onClick={() => onDelete(item.src.id)}
          >
            <TrashIcon className="mr-1 h-3 w-3" /> {t("common.delete")}
          </Button>
          <Button
            size="sm"
            className="h-7"
            disabled={!dirty}
            onClick={async () => {
              await onPatch(item.src.id, {
                content: contentDirty && draft!.trim() ? draft!.trim() : undefined,
                start_at: start ? new Date(start).toISOString() : undefined,
                end_at: end ? new Date(end).toISOString() : undefined,
              });
              setEditing(false);
              setDraft(null);
            }}
          >
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// One segment of a calendar event on a given day.
interface CalSeg {
  item: FlashItem;
  track: number;
  // Segment opens here (event start or week boundary) → rounded left.
  isStart: boolean;
  // Segment closes here (event end or week boundary) → rounded right.
  isEnd: boolean;
  // Week-first segment renders the title text.
  showLabel: boolean;
}

// TodoCalendar renders the month-grid calendar view: dated todos become day
// chips, multi-day spans render as aligned color bars (rounded only at
// segment ends, one track per event so a span reads as one continuous
// block), and undated todos collect in a collapsed strip below the grid.
// MonthNav is the prev / range-title / next / today header shared by the
// calendar and gantt todo surfaces. The step and title follow gran: a month,
// a Monday-first week, or a single day. children render at the right end (the
// gantt shows its range count there).
function MonthNav({
  anchor,
  setAnchor,
  gran,
  children,
}: {
  anchor: Date;
  setAnchor: (d: Date) => void;
  gran: CalGran;
  children?: ReactNode;
}) {
  const t = useT();
  const { locale } = useLocale();
  const step = (dir: number) => {
    if (gran === "month") setAnchor(new Date(anchor.getFullYear(), anchor.getMonth() + dir, 1));
    else {
      const d = new Date(anchor);
      d.setDate(d.getDate() + dir * (gran === "week" ? 7 : 1));
      setAnchor(d);
    }
  };
  const title = useMemo(() => {
    if (gran === "month")
      return new Intl.DateTimeFormat(locale, { year: "numeric", month: "long" }).format(anchor);
    if (gran === "week") {
      const s = startOfWeek(anchor);
      const e = new Date(s);
      e.setDate(s.getDate() + 6);
      const crossYear = s.getFullYear() !== e.getFullYear();
      const f = new Intl.DateTimeFormat(locale, { year: crossYear ? "numeric" : undefined, month: "short", day: "numeric" });
      return `${f.format(s)} – ${f.format(e)}`;
    }
    return new Intl.DateTimeFormat(locale, { year: "numeric", month: "long", day: "numeric", weekday: "short" }).format(anchor);
  }, [anchor, gran, locale]);
  return (
    <div className="mb-2 flex items-center gap-2">
      <Button variant="outline" size="sm" className="h-7 px-2" aria-label={t("knowledge.prevPeriod")}
        onClick={() => step(-1)}>
        <ChevronLeftIcon className="h-3.5 w-3.5" />
      </Button>
      <span className="min-w-40 text-center text-sm font-semibold whitespace-nowrap">{title}</span>
      <Button variant="outline" size="sm" className="h-7 px-2" aria-label={t("knowledge.nextPeriod")}
        onClick={() => step(1)}>
        <ChevronRightIcon className="h-3.5 w-3.5" />
      </Button>
      <Button variant="outline" size="sm" className="h-7 px-2 text-xs" onClick={() => setAnchor(new Date())}>
        {t("knowledge.today")}
      </Button>
      {children}
    </div>
  );
}

// UndatedStrip is the collapsed no-date list shared by the calendar and
// gantt; callers keep the length check so the strip stays out of empty views.
function UndatedStrip({
  items,
  open,
  onToggle,
  onOpen,
  onDelete,
}: {
  items: FlashItem[];
  open: boolean;
  onToggle: () => void;
  onOpen: (it: FlashItem) => void;
  onDelete: (id: string) => void;
}) {
  const t = useT();
  return (
    <div className="mt-3 overflow-hidden rounded-lg border">
      <button
        type="button"
        onClick={onToggle}
        className="flex w-full items-center justify-between px-3 py-2 text-xs text-muted-foreground hover:text-foreground"
      >
        <span>{t("knowledge.noDate")} ({items.length})</span>
        <ChevronDownIcon className={cn("h-3.5 w-3.5 transition-transform", open && "rotate-180")} />
      </button>
      {open && (
        <div className="space-y-1.5 border-t p-2">
          {items.map((it) => (
            <TodoRow key={it.src.id} item={it} onOpen={onOpen} onDelete={onDelete} />
          ))}
        </div>
      )}
    </div>
  );
}
function TodoCalendar({
  items,
  anchor,
  setAnchor,
  gran,
  onOpenItem,
  onCreateAt,
  onDelete,
}: {
  items: FlashItem[];
  anchor: Date;
  setAnchor: (d: Date) => void;
  gran: CalGran;
  onOpenItem: (it: FlashItem) => void;
  onCreateAt: (day: string) => void;
  onDelete: (id: string) => void;
}) {
  const t = useT();
  const { locale } = useLocale();
  const [undatedOpen, setUndatedOpen] = useState(false);
  const [dayDialog, setDayDialog] = useState<{ day: string; items: FlashItem[] } | null>(null);

  // The visible grid follows gran: the 42-day month, the anchor's single
  // week (taller cells, more tracks), or just the anchor day (rendered as a
  // full list below, not the 7-column grid).
  const grid = useMemo(() => {
    if (gran === "month") return monthGrid(anchor);
    if (gran === "week") {
      const s = startOfWeek(anchor);
      return Array.from({ length: 7 }, (_, i) => {
        const d = new Date(s);
        d.setDate(s.getDate() + i);
        return d;
      });
    }
    return [startOfDay(anchor)];
  }, [anchor, gran]);
  const todayK = dayKey(new Date());

  // Layout: per-day segments with track assignment. The lowest track free
  // across an event's whole in-grid span keeps a multi-day bar on one row,
  // so it reads as one continuous block across the week.
  const { byDay, undated } = useMemo(() => {
    const gridKeys = grid.map(dayKey);
    const evs: { item: FlashItem; s: string; e: string }[] = [];
    const undated: FlashItem[] = [];
    for (const it of items) {
      const s = it.src.start_at ? dayKey(new Date(it.src.start_at)) : "";
      const e = it.src.end_at ? dayKey(new Date(it.src.end_at)) : "";
      if (s && e && e >= s) evs.push({ item: it, s, e });
      else if (e) evs.push({ item: it, s: e, e }); // due only → single day
      else if (s) evs.push({ item: it, s, e: s }); // start only → single day
      else undated.push(it);
    }
    evs.sort((a, b) => (a.s < b.s ? -1 : 1));
    const taken = new Map<number, Set<string>>();
    const byDay = new Map<string, CalSeg[]>();
    for (const ev of evs) {
      const days = gridKeys.filter((k) => k >= ev.s && k <= ev.e);
      let track = 0;
      outer: while (true) {
        const set = taken.get(track);
        if (!set) break;
        for (const k of days) {
          if (set.has(k)) { track++; continue outer; }
        }
        break;
      }
      let set = taken.get(track);
      if (!set) { set = new Set(); taken.set(track, set); }
      for (const k of days) {
        set.add(k);
        const idx = gridKeys.indexOf(k);
        const arr = byDay.get(k) ?? [];
        arr.push({
          item: ev.item,
          track,
          isStart: k === ev.s || idx % 7 === 0,
          isEnd: k === ev.e || idx % 7 === 6,
          showLabel: k === ev.s || idx % 7 === 0,
        });
        byDay.set(k, arr);
      }
    }
    return { byDay, undated };
  }, [items, grid]);

  // 2024-01-01 is a Monday — enumerate the weekday labels Monday-first.
  const dowLabelList = weekLabels(locale);
  const dayTitle = (k: string) =>
    new Intl.DateTimeFormat(locale, { month: "short", day: "numeric", weekday: "short" }).format(new Date(`${k}T00:00`));

  // Empty-cell click: desktop starts a new todo dated that day; mobile opens
  // a day sheet instead (the chips are too cramped at 7 columns).
  const cellClick = (k: string) => {
    if (window.matchMedia("(min-width: 640px)").matches) onCreateAt(k);
    else setDayDialog({ day: k, items: (byDay.get(k) ?? []).map((s) => s.item) });
  };
  const openDay = (k: string) =>
    setDayDialog({ day: k, items: (byDay.get(k) ?? []).map((s) => s.item) });

  // Day granularity skips the 7-column grid: the day's todos render as full
  // rows (no track squeezing, whole titles visible) plus an add button.
  if (gran === "day") {
    const k = dayKey(anchor);
    const list = (byDay.get(k) ?? []).map((s) => s.item);
    return (
      <div className="p-3 sm:p-4">
        <MonthNav anchor={anchor} setAnchor={setAnchor} gran={gran} />
        <div className="rounded-lg border p-2">
          {list.length === 0 ? (
            <p className="px-1 py-2 text-sm text-muted-foreground">{t("knowledge.noTodos")}</p>
          ) : (
            <div className="space-y-1.5">
              {list.map((it) => (
                <TodoRow key={it.src.id} item={it} onOpen={onOpenItem} onDelete={onDelete} />
              ))}
            </div>
          )}
          <Button
            variant="outline"
            size="sm"
            className="mt-2 h-7 px-2 text-xs"
            onClick={() => onCreateAt(k)}
          >
            <PlusIcon className="mr-1 h-3 w-3" /> {t("knowledge.newTodo")}
          </Button>
        </div>
        {undated.length > 0 && <UndatedStrip items={undated} open={undatedOpen} onToggle={() => setUndatedOpen((v) => !v)} onOpen={onOpenItem} onDelete={onDelete} />}
        <Dialog open={dayDialog !== null} onOpenChange={(o) => { if (!o) setDayDialog(null); }}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{dayDialog ? dayTitle(dayDialog.day) : ""}</DialogTitle>
            </DialogHeader>
            <div className="space-y-1.5">
              {(dayDialog?.items ?? []).length === 0 && (
                <p className="text-sm text-muted-foreground">{t("knowledge.noTodos")}</p>
              )}
              {(dayDialog?.items ?? []).map((it) => (
                <TodoRow key={it.src.id} item={it} onOpen={(x) => { setDayDialog(null); onOpenItem(x); }} onDelete={onDelete} />
              ))}
            </div>
            <DialogFooter>
              <Button
                variant="outline"
                onClick={() => { const dk = dayDialog?.day; setDayDialog(null); if (dk) onCreateAt(dk); }}
              >
                <PlusIcon className="mr-1 h-3 w-3" /> {t("knowledge.newTodo")}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      </div>
    );
  }

  // Week cells are taller and allow twice the bar tracks of the month grid.
  const maxTracks = gran === "week" ? 6 : 3;
  const cellMinH = gran === "week" ? "min-h-[110px] sm:min-h-[170px]" : "min-h-[76px] sm:min-h-[96px]";

  return (
    <div className="p-3 sm:p-4">
      <MonthNav anchor={anchor} setAnchor={setAnchor} gran={gran} />
      <div className="overflow-hidden rounded-lg border">
        <div className="grid grid-cols-7 border-b bg-muted/30">
          {dowLabelList.map((d, i) => (
            <div key={i} className="py-1 text-center text-2xs font-medium text-muted-foreground">{d}</div>
          ))}
        </div>
        <div className="grid grid-cols-7 gap-px bg-border">
          {grid.map((d) => {
            const k = dayKey(d);
            const segs = (byDay.get(k) ?? []).slice().sort((a, b) => a.track - b.track);
            const inMonth = d.getMonth() === anchor.getMonth();
            // Track-padded row count: a bar on track 2 keeps two blank rows
            // above it so its position stays stable across days.
            const rows = Math.min(maxTracks, segs.length ? segs[segs.length - 1].track + 1 : 0);
            const hidden = segs.filter((s) => s.track >= rows).length;
            return (
              <div
                key={k}
                onClick={() => cellClick(k)}
                className={cn(
                  "flex cursor-pointer flex-col bg-background p-1 hover:bg-accent/40",
                  cellMinH,
                  !inMonth && "bg-muted/30",
                  k === todayK && "bg-primary/5",
                )}
              >
                <span className={cn(
                  "mb-0.5 inline-flex size-5 items-center justify-center self-start rounded-full text-2xs",
                  k === todayK
                    ? "bg-primary font-semibold text-primary-foreground"
                    : inMonth ? "text-foreground/70" : "text-muted-foreground/50",
                )}>
                  {d.getDate()}
                </span>
                <div className="flex flex-col gap-[2px] overflow-hidden">
                  {Array.from({ length: rows }, (_, tr) => {
                    const seg = segs.find((s) => s.track === tr);
                    if (!seg) return <div key={tr} className="h-[32px]" />;
                    const st = (seg.item.src.status || "pending") as TodoStatus;
                    const overdue = todoOverdue(seg.item.src);
                    // Chips are two lines tall (line-clamp) so titles read in
                    // place; the hover card still carries the full text.
                    return (
                      <Tooltip key={tr}>
                        <TooltipTrigger
                          render={(props) => (
                            <button
                              {...props}
                              type="button"
                              onClick={(e) => { e.stopPropagation(); onOpenItem(seg.item); }}
                              className={cn(
                                "flex min-h-[32px] items-center gap-1 overflow-hidden px-1 text-left text-2xs leading-tight sm:text-2xs",
                                seg.isStart && "rounded-l-sm pl-1.5",
                                seg.isEnd && "rounded-r-sm pr-1.5",
                                statusChipBg(st),
                              )}
                            >
                              {seg.showLabel ? (
                                <span className="line-clamp-2 break-words">{todoFirstLine(seg.item.content)}</span>
                              ) : (
                                <span className="line-clamp-2 break-words opacity-0">·</span>
                              )}
                              {overdue && <span className="ml-auto size-1.5 shrink-0 rounded-full bg-destructive" />}
                            </button>
                          )}
                        />
                        <TooltipContent side="bottom">
                          <GanttTip item={seg.item} span={todoSpanText(seg.item.src)} />
                        </TooltipContent>
                      </Tooltip>
                    );
                  })}
                  {hidden > 0 && (
                    <button
                      type="button"
                      onClick={(e) => { e.stopPropagation(); openDay(k); }}
                      className="text-left text-2xs text-muted-foreground hover:text-foreground"
                    >
                      +{hidden}
                    </button>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      </div>
      {undated.length > 0 && <UndatedStrip items={undated} open={undatedOpen} onToggle={() => setUndatedOpen((v) => !v)} onOpen={onOpenItem} onDelete={onDelete} />}
      <Dialog open={dayDialog !== null} onOpenChange={(o) => { if (!o) setDayDialog(null); }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{dayDialog ? dayTitle(dayDialog.day) : ""}</DialogTitle>
          </DialogHeader>
          <div className="space-y-1.5">
            {(dayDialog?.items ?? []).length === 0 && (
              <p className="text-sm text-muted-foreground">{t("knowledge.noTodos")}</p>
            )}
            {(dayDialog?.items ?? []).map((it) => (
              <TodoRow key={it.src.id} item={it} onOpen={(x) => { setDayDialog(null); onOpenItem(x); }} onDelete={onDelete} />
            ))}
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => { const k = dayDialog?.day; setDayDialog(null); if (k) onCreateAt(k); }}
            >
              <PlusIcon className="mr-1 h-3 w-3" /> {t("knowledge.newTodo")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// GanttTip is the hover card shared by the gantt name cell, the gantt bar
// and the calendar chips: the untruncated first-line title, the full span
// and the status — fixed-width surfaces truncate long titles, this is where
// they read whole.
function GanttTip({ item, span }: { item: FlashItem; span: string }) {
  const t = useT();
  const st = (item.src.status || "pending") as TodoStatus;
  return (
    <div className="space-y-1 py-0.5 text-left">
      <p className="font-medium">{todoFirstLine(item.content)}</p>
      {span && <p className="opacity-80">{span}</p>}
      <p className="opacity-80">{t("knowledge.status_" + st)}</p>
    </div>
  );
}

// GanttRow is one gantt lane: the task-name cell plus the column area with
// its positioned bar. ticks (the day or hour background grid, including the
// now-line on today) render behind the bar; left/width are 0..1 fractions of
// the column area. nameW is the caller-owned name-column width (drag the
// header resizer to change it). Both the name cell and the bar open the
// GanttTip hover card with the untruncated title.
function GanttRow({
  item,
  left,
  width,
  label,
  clippedStart,
  clippedEnd,
  ticks,
  nameW,
  onOpenItem,
}: {
  item: FlashItem;
  left: number;
  width: number;
  label: string;
  clippedStart: boolean;
  clippedEnd: boolean;
  ticks: ReactNode;
  nameW: number;
  onOpenItem: (it: FlashItem) => void;
}) {
  const st = (item.src.status || "pending") as TodoStatus;
  const overdue = todoOverdue(item.src);
  const name = todoFirstLine(item.content);
  const tip = <GanttTip item={item} span={todoSpanText(item.src)} />;
  return (
    <div className="flex border-b last:border-b-0 hover:bg-accent/30">
      <Tooltip>
        <TooltipTrigger
          render={(props) => (
            <button
              {...props}
              type="button"
              onClick={() => onOpenItem(item)}
              // The name cell wraps to two lines (line-clamp) and every row
              // keeps the same two-line height so lanes read as an even
              // grid — long titles show in place, the hover card still
              // carries the fully untruncated text.
              className="flex min-h-[46px] shrink-0 items-center gap-1.5 border-r px-2 py-1.5 text-left text-xs leading-snug hover:text-primary"
              style={{ width: nameW }}
            >
              <span className={cn("size-1.5 shrink-0 rounded-full", statusBar(st))} />
              <span className={cn("line-clamp-2 break-words", st === "cancelled" && "line-through opacity-70")}>
                {name}
              </span>
            </button>
          )}
        />
        <TooltipContent side="right">{tip}</TooltipContent>
      </Tooltip>
      <div className="relative flex-1 py-1">
        {ticks}
        <Tooltip>
          <TooltipTrigger
            render={(props) => (
              <button
                {...props}
                type="button"
                onClick={() => onOpenItem(item)}
                // The bar fill and ring carry the status color (see
                // ganttBarStyle); the name-column dot and the overdue marker
                // stay as secondary cues, cancelled keeps its strike-through.
                className={cn(
                  "absolute inset-y-2 flex items-center gap-1 overflow-hidden rounded-sm px-1.5 text-left text-2xs font-medium ring-1 ring-inset",
                  ganttBarStyle(st),
                  st === "cancelled" && "opacity-60 line-through",
                )}
                style={{ left: left * 100 + "%", width: width * 100 + "%" }}
              >
                {clippedStart && <span className="shrink-0 opacity-60">&#8249;</span>}
                <span className="truncate">{label}</span>
                {overdue && <span className="size-1.5 shrink-0 rounded-full bg-destructive" />}
                {clippedEnd && <span className="ml-auto shrink-0 opacity-60">&#8250;</span>}
              </button>
            )}
          />
          <TooltipContent side="top">{tip}</TooltipContent>
        </Tooltip>
      </div>
    </div>
  );
}

// TodoGantt is the fourth todo surface: one row per task across a day-column
// grid spanning the anchor month or week (bars clip at the range edges with
// an arrow when the span continues beyond), or a 24-hour column grid for the
// day granularity (bars position by their overlap with the day, keeping a
// 1-hour minimum so point-in-time dues stay visible). Rows are only tasks
// overlapping the range, so the row count itself reads as the workload;
// undated tasks collapse into the same strip the calendar uses. Due-only /
// start-only todos become single-day bars, matching the calendar convention.
function TodoGantt({
  items,
  loading,
  anchor,
  setAnchor,
  gran,
  onOpenItem,
  onDelete,
}: {
  items: FlashItem[];
  loading: boolean;
  anchor: Date;
  setAnchor: (d: Date) => void;
  gran: CalGran;
  onOpenItem: (it: FlashItem) => void;
  onDelete: (id: string) => void;
}) {
  const t = useT();
  const { locale } = useLocale();
  const todayK = dayKey(new Date());
  // The name column width is user-adjustable (drag the header resizer) so
  // long titles stop truncating; it persists in localStorage across visits.
  const [nameW, setNameW] = useState(() => {
    if (typeof window === "undefined") return 176;
    const v = Number(window.localStorage.getItem("kb-gantt-name-w"));
    return Number.isFinite(v) && v >= 128 && v <= 480 ? v : 176;
  });
  const saveNameW = (w: number) => {
    try { window.localStorage.setItem("kb-gantt-name-w", String(w)); } catch {}
  };
  const startResize = (e: React.PointerEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startW = nameW;
    const onMove = (ev: PointerEvent) => {
      setNameW(Math.round(Math.min(480, Math.max(128, startW + ev.clientX - startX))));
    };
    const onUp = () => {
      document.removeEventListener("pointermove", onMove);
      document.removeEventListener("pointerup", onUp);
      document.body.style.cursor = "";
      setNameW((w) => { saveNameW(w); return w; });
    };
    document.body.style.cursor = "col-resize";
    document.addEventListener("pointermove", onMove);
    document.addEventListener("pointerup", onUp);
  };
  // The day view draws a now-line: keep a minute-fresh timestamp in state —
  // calling Date.now() during render would be impure.
  const [nowMs, setNowMs] = useState(() => Date.now());
  useEffect(() => {
    if (gran !== "day") return;
    const id = setInterval(() => setNowMs(Date.now()), 60000);
    return () => clearInterval(id);
  }, [gran]);

  // The range follows gran: the anchor month, its Monday-first week, or the
  // anchor day alone (rendered as the 24-hour column grid below).
  const { start, days } = useMemo(() => {
    if (gran === "month") {
      const s = new Date(anchor.getFullYear(), anchor.getMonth(), 1);
      return { start: s, days: new Date(s.getFullYear(), s.getMonth() + 1, 0).getDate() };
    }
    if (gran === "week") return { start: startOfWeek(anchor), days: 7 };
    return { start: startOfDay(anchor), days: 1 };
  }, [anchor, gran]);

  const cols = gran === "day" ? 24 : days;
  const gridCols = { gridTemplateColumns: "repeat(" + cols + ", minmax(0, 1fr))" };
  const minW = gran === "month" ? "min-w-[680px]" : gran === "week" ? "min-w-[560px]" : "min-w-[980px]";

  // dayCells is computed once per range, not per row: the day-number header
  // and every row's tick grid map over the same descriptors.
  const dayCells = useMemo(() =>
    Array.from({ length: days }, (_, i) => {
      const d = new Date(start);
      d.setDate(start.getDate() + i);
      return { k: dayKey(d), n: d.getDate(), weekend: d.getDay() === 0 || d.getDay() === 6 };
    }),
  [start, days]);

  const dows = useMemo(() => weekLabels(locale), [locale]);

  // Month/week rows: s/e are column indexes into the day range, clipped at
  // the edges when the span continues beyond it.
  const rows = useMemo(() => {
    if (gran === "day") return [];
    const rows: { item: FlashItem; s: number; e: number; clippedStart: boolean; clippedEnd: boolean }[] = [];
    const firstK = dayKey(start);
    const lastDate = new Date(start);
    lastDate.setDate(start.getDate() + days - 1);
    const lastK = dayKey(lastDate);
    const startMs = start.getTime();
    const idxOf = (k: string) => Math.round((new Date(`${k}T00:00`).getTime() - startMs) / 86400000);
    for (const it of items) {
      const s0 = it.src.start_at ? dayKey(new Date(it.src.start_at)) : "";
      const e0 = it.src.end_at ? dayKey(new Date(it.src.end_at)) : "";
      if (!s0 && !e0) continue; // undated live in the calendar pane strip
      const s = s0 || e0;
      const e = e0 || s0;
      if (e < firstK || s > lastK) continue; // span entirely outside this range
      rows.push({
        item: it,
        s: s < firstK ? 0 : idxOf(s),
        e: e > lastK ? days - 1 : idxOf(e),
        clippedStart: s < firstK,
        clippedEnd: e > lastK,
      });
    }
    rows.sort((a, b) => a.s - b.s || a.e - b.e);
    return rows;
  }, [items, gran, start, days]);

  // Day rows: one lane per task overlapping the day, positioned by its
  // clamped overlap ([max(start, midnight), min(due, next-midnight)]).
  const hourRows = useMemo(() => {
    if (gran !== "day") return [];
    const dayStart = start.getTime();
    const dayEnd = dayStart + 86400000;
    const p2 = (n: number) => String(n).padStart(2, "0");
    const hm = (ms: number) => { const d = new Date(ms); return `${d.getHours()}:${p2(d.getMinutes())}`; };
    const out: { item: FlashItem; l: number; w: number; label: string; clippedStart: boolean; clippedEnd: boolean }[] = [];
    for (const it of items) {
      const a = it.src.start_at ? new Date(it.src.start_at).getTime()
        : it.src.end_at ? new Date(it.src.end_at).getTime() : NaN;
      if (isNaN(a)) continue; // undated live in the calendar pane strip
      const b = it.src.end_at ? new Date(it.src.end_at).getTime() : a;
      if (b < dayStart || a > dayEnd) continue; // entirely outside this day
      const clippedStart = a < dayStart;
      const clippedEnd = b > dayEnd;
      let lo = Math.max(a, dayStart);
      let hi = Math.min(b, dayEnd);
      if (hi - lo < 3600000) {
        hi = lo + 3600000;
        if (hi > dayEnd) { hi = dayEnd; lo = hi - 3600000; }
      }
      out.push({
        item: it,
        l: (lo - dayStart) / 86400000,
        w: (hi - lo) / 86400000,
        label: b > a ? `${hm(lo)} – ${hm(hi)}` : hm(lo),
        clippedStart,
        clippedEnd,
      });
    }
    out.sort((x, y) => x.l - y.l);
    return out;
  }, [items, gran, start]);

  const laneCount = gran === "day" ? hourRows.length : rows.length;
  const countKey = gran === "month" ? "knowledge.ganttThisMonth"
    : gran === "week" ? "knowledge.ganttThisWeek" : "knowledge.ganttThisDay";

  // dayTicks shade weekends and highlight today; hourTicks draw the
  // now-line when the range is today. Both render behind every lane.
  const dayTicks = (
    <div className="absolute inset-0 grid" style={gridCols} aria-hidden>
      {dayCells.map(({ k, weekend }, i) => (
        <div key={i} className={cn(
          "border-l border-border/60",
          weekend && "bg-muted/25",
          k === todayK && "bg-primary/5",
        )} />
      ))}
    </div>
  );
  const nowFrac = todayK === dayKey(start) ? (nowMs - start.getTime()) / 86400000 : null;
  const hourTicks = (
    <div className="absolute inset-0 grid" style={gridCols} aria-hidden>
      {Array.from({ length: 24 }, (_, i) => (
        <div key={i} className="border-l border-border/60" />
      ))}
      {nowFrac !== null && nowFrac >= 0 && nowFrac <= 1 && (
        <div className="absolute inset-y-0 w-px bg-destructive/70" style={{ left: nowFrac * 100 + "%" }} />
      )}
    </div>
  );

  return (
    <div className="p-3 sm:p-4">
      <MonthNav anchor={anchor} setAnchor={setAnchor} gran={gran}>
        <span className="ml-auto text-xs text-muted-foreground">{t(countKey, { n: laneCount })}</span>
      </MonthNav>
      {loading ? (
        <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
      ) : laneCount === 0 ? (
        <p className="text-sm text-muted-foreground">{t("knowledge.noTodos")}</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border">
          <div className={minW}>
            <div className="flex border-b bg-muted/30">
              {/* Name column: drag the right-edge handle to widen (double-click resets). */}
              <div className="relative shrink-0 border-r" style={{ width: nameW }}>
                <div
                  role="separator"
                  aria-orientation="vertical"
                  aria-label={t("knowledge.ganttNameCol")}
                  onPointerDown={startResize}
                  onDoubleClick={() => { setNameW(176); saveNameW(176); }}
                  className="group absolute inset-y-0 right-0 z-10 flex w-1.5 cursor-col-resize touch-none"
                >
                  <div className="mx-auto h-full w-px bg-transparent transition-colors group-hover:bg-primary/60 group-active:bg-primary" />
                </div>
              </div>
              <div className="grid flex-1" style={gridCols}>
                {gran === "day" ? (
                  Array.from({ length: 24 }, (_, i) => (
                    <div key={i} className="py-1 text-center text-2xs text-foreground/70">{i}</div>
                  ))
                ) : gran === "week" ? (
                  dayCells.map(({ k, n }, i) => (
                    <div key={i} className={cn(
                      "flex flex-col items-center py-1 text-2xs",
                      k === todayK ? "font-semibold text-primary" : "text-foreground/70",
                    )}>
                      <span className="text-muted-foreground">{dows[i]}</span>
                      <span className="text-2xs font-medium">{n}</span>
                    </div>
                  ))
                ) : (
                  dayCells.map(({ k, n, weekend }, i) => (
                    <div key={i} className={cn(
                      "py-1 text-center text-2xs",
                      k === todayK ? "font-semibold text-primary" : weekend ? "text-muted-foreground" : "text-foreground/70",
                    )}>
                      {n}
                    </div>
                  ))
                )}
              </div>
            </div>
            {gran === "day" ? (
              hourRows.map((r) => (
                <GanttRow key={r.item.src.id} item={r.item} left={r.l} width={r.w} label={r.label}
                  clippedStart={r.clippedStart} clippedEnd={r.clippedEnd} ticks={hourTicks} nameW={nameW} onOpenItem={onOpenItem} />
              ))
            ) : (
              rows.map((r) => (
                <GanttRow key={r.item.src.id} item={r.item} left={r.s / days} width={(r.e - r.s + 1) / days}
                  label={todoSpanText(r.item.src)}
                  clippedStart={r.clippedStart} clippedEnd={r.clippedEnd} ticks={dayTicks} nameW={nameW} onOpenItem={onOpenItem} />
              ))
            )}
          </div>
        </div>
      )}
    </div>
  );
}
// CopyIconButton copies markdown text to the clipboard and briefly flips to a
// green check mark on success. value may be async (articles fetch entries on
// click). Mirrors the delete button's reveal-on-hover, but tinted primary on
// hover instead of destructive so the two stay visually distinct.
function CopyIconButton({
  value,
  sizeClass,
}: {
  value: string | (() => Promise<string>);
  sizeClass: string;
}) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 text-muted-foreground hover:text-primary shrink-0 relative after:absolute after:-inset-2 transition-colors"
      onClick={async (e) => {
        e.stopPropagation();
        try {
          const text = typeof value === "function" ? await value() : value;
          await navigator.clipboard.writeText(text);
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        } catch {}
      }}
      aria-label={t("common.copy")}
      title={t("common.copy")}
    >
      {copied ? (
        <CheckIcon className={cn(sizeClass, "text-success")} />
      ) : (
        <CopyIcon className={sizeClass} />
      )}
    </button>
  );
}
