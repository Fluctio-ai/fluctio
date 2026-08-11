"use client";
import { useT } from "@/lib/i18n";

import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
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
  ArrowLeftIcon,
  AlignLeftIcon,
  AlertTriangleIcon,
  CheckIcon,
  CheckSquareIcon,
  ChevronDownIcon,
  CopyIcon,
  FileTextIcon,
  GlobeIcon,
  ListOrderedIcon,
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
  kbSaveTodo,
  kbUpdateTodo,
  kbListTodos,
  listBookmarks,
  saveBookmark,
  deleteBookmark,
  promoteBookmark,
  kbGetInsights,
  kbGenerateInsights,
  listKBPending,
  resolveKBPending,
  type ArticleInsights,
  type KBPending,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { cn } from "@/lib/utils";
import { readCache, writeCache } from "@/lib/page-data-cache";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";

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
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
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
            className="shrink-0 rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            <XIcon className="h-3.5 w-3.5" />
          </button>
        </div>
      )}
    </div>
  );
}

// ── Articles: chunked sources (original two-pane browser) ──

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
        <div className="p-3 border-b space-y-2">
          <div>
            {stats && (
              <p className="text-xs tabular-nums text-muted-foreground mt-0.5">
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
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      handleSelectSource(src);
                    }
                  }}
                >
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
                    <div className="space-y-1">
                      {entries.map((entry, i) => (
                        <div key={entry.id} className="relative">
                          <span className="absolute right-0 top-0 text-xs text-muted-foreground/40 select-none pointer-events-none">
                            #{entry.chunk_index}
                          </span>
                          <div className="prose prose-sm dark:prose-invert max-w-none break-words">
                            <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]}>
                              {entry.content}
                            </ReactMarkdown>
                          </div>
                          {i < entries.length - 1 && (
                            <p className="text-center text-muted-foreground/40 text-xs my-2 select-none pointer-events-none">
                              * * *
                            </p>
                          )}
                        </div>
                      ))}
                    </div>
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
                          className="text-xs text-emerald-600 dark:text-emerald-400"
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
        <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("knowledge.searchFlashes")} className="h-7 text-xs flex-1 rounded-md" />
        <Button size="sm" variant="outline" className="h-7 shrink-0 text-xs" onClick={() => setSortNew((v) => !v)}>
          {sortNew ? t("knowledge.sortNewest") : t("knowledge.sortOldest")}
        </Button>
        <Button size="sm" className="h-7 shrink-0" onClick={() => setFlashOpen(true)}>
          <PlusIcon className="h-3 w-3 mr-1" /> {t("knowledge.saveFlash")}
        </Button>
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
                <div className="prose prose-sm dark:prose-invert max-w-none">
                  <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]}>
                    {content}
                  </ReactMarkdown>
                </div>
                <div className="mt-2 flex items-center justify-between text-xs text-muted-foreground">
                  <span>{relativeTime(src.created_at)}</span>
                  <div className="flex gap-2">
                    <CopyIconButton sizeClass="h-3 w-3" value={content} />
                    <button
                      type="button"
                      className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 hover:text-destructive relative after:absolute after:-inset-2"
                      onClick={() => handleDelete(src.id)}
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
        <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("knowledge.searchBookmarks")} className="h-7 text-xs flex-1 rounded-md" />
        <Button size="sm" className="h-7 shrink-0" onClick={() => setAddOpen(true)}>
          <PlusIcon className="h-3 w-3 mr-1" /> {t("knowledge.addBookmark")}
        </Button>
      </div>
      <ScrollArea className="flex-1">
        <div className="mx-auto max-w-screen-2xl columns-1 gap-3 p-4 sm:columns-2 lg:columns-3 xl:columns-4">
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
                  <button
                    type="button"
                    className="shrink-0 rounded p-1 text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 hover:text-destructive"
                    onClick={() => handleDelete(b.id)}
                    aria-label={t("common.delete")}
                  >
                    <TrashIcon className="h-3.5 w-3.5" />
                  </button>
                </div>
                {b.summary && (
                  <p className="mt-1.5 whitespace-pre-wrap break-words text-xs text-muted-foreground">{b.summary}</p>
                )}
                <div className="mt-2 flex flex-wrap items-center justify-between gap-x-2 gap-y-1">
                  <span className="min-w-0 break-words text-xs text-muted-foreground">
                    {relativeTime(b.created_at)}{b.content ? ` · ${t("knowledge.bookmarkBody", { n: b.content.length })}` : ""}
                    {b.promoted_to_article_id && (
                      <span className="ml-2 text-emerald-600 dark:text-emerald-400">{t("knowledge.bookmarkPromoted")}</span>
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
  const [newEnd, setNewEnd] = useState("");
  const [creating, setCreating] = useState(false);
  const [view, setView] = useState<"board" | "list">("board");

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

  const handleCreate = useCallback(async () => {
    if (!agentId || !newContent.trim()) return;
    setCreating(true);
    try {
      const endAt = newEnd ? new Date(newEnd).toISOString() : undefined;
      const res = await kbSaveTodo(agentId, newContent.trim(), "pending", undefined, endAt);
      if ("error" in res) notify(res.error!); else {
        setNewOpen(false); setNewContent(""); setNewEnd(""); load();
      }
    } catch { notify(t("knowledge.failedAddText")); }
    setCreating(false);
  }, [agentId, newContent, newEnd, load, t]);

  const handleMove = useCallback(async (id: string, status: TodoStatus) => {
    if (!agentId) return;
    const res = await kbUpdateTodo(agentId, id, { status });
    if ("error" in res) notify(res.error!); else load();
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

  // dueSoon: active todos with a due date, split into overdue vs. due-today
  // for the urgency summary above the board.
  const { overdue: overdueTodos, today: dueToday } = useMemo(() => {
    const now = Date.now();
    const eod = new Date(); eod.setHours(23, 59, 59, 999);
    const overdue: FlashItem[] = [];
    const today: FlashItem[] = [];
    for (const it of todos) {
      if (it.src.status !== "pending" && it.src.status !== "in_progress") continue;
      if (!it.src.end_at) continue;
      const due = new Date(it.src.end_at).getTime();
      if (due < now) overdue.push(it);
      else if (due <= eod.getTime()) today.push(it);
    }
    return { overdue, today };
  }, [todos]);

  // listSorted: flat urgency-ordered list for the list view — active first,
  // then ascending due date (undated last).
  const listSorted = useMemo(() => {
    return [...todos].sort((a, b) => {
      const aa = a.src.status === "pending" || a.src.status === "in_progress";
      const bb = b.src.status === "pending" || b.src.status === "in_progress";
      if (aa !== bb) return aa ? -1 : 1;
      const ea = a.src.end_at ? new Date(a.src.end_at).getTime() : Infinity;
      const eb = b.src.end_at ? new Date(b.src.end_at).getTime() : Infinity;
      return ea - eb;
    });
  }, [todos]);

  return (
    <div className="flex h-full flex-col">
      <div className="border-b p-3 flex items-center justify-between">
        <h3 className="text-sm font-semibold">{t("knowledge.todoBoard")}</h3>
        <div className="flex items-center gap-2">
          <div className="flex items-center rounded-md border p-0.5">
            <button type="button" onClick={() => setView("board")} className={cn("rounded px-2 py-1 text-xs", view === "board" ? "bg-background text-foreground shadow-sm" : "text-muted-foreground")}>{t("knowledge.viewBoard")}</button>
            <button type="button" onClick={() => setView("list")} className={cn("rounded px-2 py-1 text-xs", view === "list" ? "bg-background text-foreground shadow-sm" : "text-muted-foreground")}>{t("knowledge.viewList")}</button>
          </div>
          <Button size="sm" onClick={() => setNewOpen(true)}>
            <PlusIcon className="h-3 w-3 mr-1" /> {t("knowledge.newTodo")}
          </Button>
        </div>
      </div>
      {(overdueTodos.length > 0 || dueToday.length > 0) && (
        <div className="flex items-center gap-3 border-b px-4 py-1.5 text-xs">
          {overdueTodos.length > 0 && <span className="font-medium text-destructive">{t("knowledge.overdue")} {overdueTodos.length}</span>}
          {dueToday.length > 0 && <span className="font-medium text-warning">{t("knowledge.dueToday")} {dueToday.length}</span>}
        </div>
      )}
      <ScrollArea className="flex-1">
        {view === "list" ? (
          <div className="mx-auto max-w-2xl space-y-2 p-4">
            {loading ? (
              <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
            ) : listSorted.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t("knowledge.noTodos")}</p>
            ) : (
              listSorted.map(({ src, content }) => (
                <TodoCard key={src.id} src={src} content={content} onDelete={handleDelete} onMove={handleMove} />
              ))
            )}
          </div>
        ) : (
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-3 p-4">
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
                    onDelete={handleDelete}
                    onMove={handleMove}
                  />
                ))}
              </div>
            </div>
          ))}
        </div>
        )}
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
      <div className={cn("prose prose-sm dark:prose-invert max-w-none", src.status === "cancelled" && "opacity-60 [&_*]:line-through")}>
        <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]}>
          {content}
        </ReactMarkdown>
      </div>
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
      return "text-muted-foreground line-through";
  }
  return "";
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
