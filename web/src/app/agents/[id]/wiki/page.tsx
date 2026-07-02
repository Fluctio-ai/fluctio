"use client";
import { useT } from "@/lib/i18n";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  BookOpenIcon,
  BrainIcon,
  ChevronRightIcon,
  DatabaseIcon,
  EyeIcon,
  FlaskConicalIcon,
  LightbulbIcon,
  NetworkIcon,
  RefreshCwIcon,
  SearchIcon,
  SparklesIcon,
  TrashIcon,
} from "lucide-react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import { ExternalAnchor } from "@/components/markdown-link";
import { ChatMarkdown } from "@/components/chat-markdown";
import {
  type WikiPage,
  type WikiStats,
  getWikiStats,
  listWikiPages,
  getWikiPage,
  deleteWikiPage,
  generateWiki,
  getWikiProgress,
  getWikiGraph,
  listKBSources,
  type KBSource,
  type WikiAutoGenCfg,
  getAgentMemory,
  setAgentMemory,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";
import { cn } from "@/lib/utils";

const PAGE_TYPE_SECTIONS = (t: ReturnType<typeof useT>) => [
  { type: "overview", label: t("wiki.overview"), icon: EyeIcon },
  { type: "entity", label: t("wiki.entity"), icon: DatabaseIcon },
  { type: "concept", label: t("wiki.concept"), icon: LightbulbIcon },
  { type: "source", label: t("wiki.source"), icon: BookOpenIcon },
];

export default function WikiPage() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);

  const [stats, setStats] = useState<WikiStats | null>(null);
  const [pages, setPages] = useState<WikiPage[]>([]);
  const [selectedPageId, setSelectedPageId] = useState<string | null>(null);
  const [selectedPage, setSelectedPage] = useState<WikiPage | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<WikiPage | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [generating, setGenerating] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number; status: string } | null>(null);
  const [filterType, setFilterType] = useState<string>("all");
  const [showGraph, setShowGraph] = useState(false);
  const graphRef = useRef<HTMLDivElement>(null);

  // KB sources for generation
  const [kbSources, setKbSources] = useState<KBSource[]>([]);

  // Background auto-generation config (memory.wikiAutoGen). Loaded once,
  // saved via spread so we never clobber sibling memory fields.
  const [wikiCfg, setWikiCfg] = useState<WikiAutoGenCfg>({ enabled: false });
  const [wikiSaving, setWikiSaving] = useState(false);
  const wikiSavingRef = useRef(false);

  const loadData = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      const [s, p] = await Promise.all([
        getWikiStats(agentId),
        listWikiPages(agentId, filterType === "all" ? undefined : filterType),
      ]);
      setStats(s);
      setPages(p.pages ?? []);
    } catch {}
    setLoading(false);
  }, [agentId, filterType]);

  useEffect(() => {
    loadData();
  }, [loadData]);

  // Load KB sources for generation UI
  useEffect(() => {
    if (!agentId) return;
    listKBSources(agentId).then(setKbSources).catch(() => {});
  }, [agentId]);

  // Load auto-gen config
  useEffect(() => {
    if (!agentId) return;
    getAgentMemory(agentId)
      .then((m) => setWikiCfg(m.memory?.wikiAutoGen || { enabled: false }))
      .catch(() => {});
  }, [agentId]);

  const saveWikiCfg = async (next: WikiAutoGenCfg) => {
    if (wikiSavingRef.current) return;
    wikiSavingRef.current = true;
    setWikiCfg(next);
    setWikiSaving(true);
    try {
      const cur = await getAgentMemory(agentId).catch(() => null);
      const base = cur?.memory || {};
      await setAgentMemory(agentId, { ...base, wikiAutoGen: next });
    } finally {
      setWikiSaving(false);
      wikiSavingRef.current = false;
    }
  };

  const handleSelectPage = useCallback(
    async (pageId: string) => {
      if (!agentId) return;
      setSelectedPageId(pageId);
      try {
        const p = await getWikiPage(agentId, pageId);
        setSelectedPage(p);
      } catch {}
    },
    [agentId],
  );

  const handleGenerate = useCallback(async (force?: boolean) => {
    if (!agentId || kbSources.length === 0) return;
    setGenerating(true);
    setProgress({ done: 0, total: kbSources.length, status: "running" });
    try {
      await generateWiki(
        agentId,
        kbSources.map((s) => s.id),
        force,
      );
    } catch {
      setGenerating(false);
      setProgress(null);
    }
  }, [agentId, kbSources]);

  useEffect(() => {
    if (!agentId || !generating) return;
    let cancelled = false;
    const poll = async () => {
      try {
        const p = await getWikiProgress(agentId);
        if (cancelled) return;
        if (p.status === "idle") return;
        setProgress({ done: p.done ?? 0, total: p.total ?? 0, status: p.status });
        if (p.status === "done") {
          setGenerating(false);
          loadData();
        }
      } catch {}
    };
    poll();
    const id = setInterval(poll, 2000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [agentId, generating, loadData]);

  const handleForceGenerate = useCallback(() => {
    if (!window.confirm(t("wiki.forceRegenConfirm"))) return;
    handleGenerate(true);
  }, [handleGenerate, t]);

  const unprocessedCount = kbSources.filter((s) => !s.wiki_generated_at).length;

  const openDelete = useCallback((page: WikiPage) => {
    setDeleteError(null);
    setDeleteTarget(page);
  }, []);

  const confirmDelete = useCallback(async () => {
    if (!agentId || !deleteTarget) return;
    try {
      await deleteWikiPage(agentId, deleteTarget.id);
      if (selectedPageId === deleteTarget.id) {
        setSelectedPageId(null);
        setSelectedPage(null);
      }
      setDeleteTarget(null);
      loadData();
    } catch (e) {
      setDeleteError(e instanceof Error ? e.message : t("wiki.deleteFailed"));
    }
  }, [agentId, deleteTarget, selectedPageId, loadData, t]);

  const handleLoadGraph = useCallback(async () => {
    if (!agentId) return;
    setShowGraph(true);
  }, [agentId]);

  // Render vis-network graph
  useEffect(() => {
    if (!showGraph || !graphRef.current || !agentId) return;
    let network: import("vis-network").Network | null = null;
    let cancelled = false;

    const init = async () => {
      const [{ Network }, { DataSet }] = await Promise.all([
        import("vis-network/standalone"),
        import("vis-data/standalone"),
      ]);
      if (cancelled) return;

      const g = await getWikiGraph(agentId);
      if (cancelled) return;

      const typeColors: Record<string, string> = {
        overview: "#8b5cf6",
        entity: "#3b82f6",
        concept: "#10b981",
        source: "#f59e0b",
        query: "#ef4444",
      };

      const nodes = new DataSet(
        g.nodes.map((n) => ({
          id: n.id,
          label: n.title.length > 12 ? n.title.slice(0, 12) + "…" : n.title,
          title: n.title,
          color: { background: typeColors[n.page_type] || "#666", border: "#333" },
          font: { size: 11, color: "#e5e7eb" },
          shape: "dot",
          size: 20,
        })),
      );

      const edges = new DataSet(
        (g.edges ?? []).map((e, i) => ({
          id: i + 1,
          from: e.src_page_id,
          to: e.dst_page_id,
          title: e.relation,
          arrows: "to",
          color: { color: "#555", opacity: 0.4 },
          width: 1,
        })),
      );

      network = new Network(graphRef.current!, { nodes, edges }, {
        physics: { stabilization: { iterations: 100 }, solver: "forceAtlas2Based" },
        interaction: { hover: true, tooltipDelay: 200 },
        edges: { smooth: true },
      });
    };

    init();
    return () => { cancelled = true; if (network) network.destroy(); };
  }, [showGraph, agentId]);

  // Group pages by type
  const grouped = useMemo(() => {
    const m: Record<string, WikiPage[]> = {};
    for (const p of pages) {
      if (!m[p.page_type]) m[p.page_type] = [];
      m[p.page_type].push(p);
    }
    return m;
  }, [pages]);

  return (
    <div className="flex h-[calc(100vh-3.5rem)]">
      {/* Left: Tree navigation */}
      <div className="w-64 shrink-0 border-r bg-muted/30 flex flex-col">
        <div className="p-3 border-b">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <h3 className="text-sm font-semibold">{t("wiki.title")}</h3>
              {generating && progress && (
                <span className="text-xs text-muted-foreground flex items-center gap-1">
                  <RefreshCwIcon className="h-3 w-3 animate-spin" />
                  {t("wiki.generatingProgress", { done: progress.done, total: progress.total })}
                </span>
              )}
            </div>
            <div className="flex gap-1">
              <Button
                variant="ghost"
                size="icon"
                className="h-7 w-7"
                onClick={() => handleGenerate()}
                disabled={generating || unprocessedCount === 0}
                title={unprocessedCount === 0 ? t("wiki.allProcessed") : t("wiki.generateUnprocessed")}
              >
                <SparklesIcon className="h-4 w-4" />
              </Button>
              <Button
                variant="ghost"
                size="icon"
                className="h-7 w-7"
                onClick={handleForceGenerate}
                disabled={generating || kbSources.length === 0}
                title={t("wiki.forceRegenAll")}
              >
                <RefreshCwIcon className="h-3.5 w-3.5" />
              </Button>
              <Button
                variant="ghost"
                size="icon"
                className="h-7 w-7"
                onClick={handleLoadGraph}
                title={t("wiki.knowledgeGraph")}
              >
                <NetworkIcon className="h-4 w-4" />
              </Button>
            </div>
          </div>
          {stats && (
            <p className="text-xs text-muted-foreground mt-1">
              {t("wiki.pageStats", { pages: stats.total_pages, links: stats.total_edges })}
            </p>
          )}
        </div>

        {/* Auto-generation config */}
        <div className="p-3 border-b space-y-2">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium">{t("wiki.autoGen")}</span>
            <Switch
              checked={wikiCfg.enabled}
              onCheckedChange={(v) => saveWikiCfg({ ...wikiCfg, enabled: v })}
              disabled={wikiSaving}
            />
          </div>
          {wikiCfg.enabled && (
            <>
              <div className="flex items-center justify-between gap-2">
                <label className="text-xs text-muted-foreground">{t("wiki.autoGenInterval")}</label>
                <div className="flex items-center gap-1.5">
                  <Input
                    type="number"
                    min={1}
                    className="w-16 h-7 text-xs"
                    value={wikiCfg.interval ? Math.round(wikiCfg.interval / 3600000000000) : 6}
                    onChange={(e) => {
                      const hours = Math.max(1, Number(e.target.value) || 6);
                      saveWikiCfg({ ...wikiCfg, interval: hours * 3600000000000 });
                    }}
                    disabled={wikiSaving}
                  />
                  <span className="text-xs text-muted-foreground">{t("wiki.autoGenHours")}</span>
                </div>
              </div>
              <p className="text-[11px] leading-tight text-muted-foreground">{t("wiki.autoGenHint")}</p>
            </>
          )}
        </div>

        <ScrollArea className="flex-1">
          <div className="p-2">
            {PAGE_TYPE_SECTIONS(t).map((section) => {
              const sectionPages = grouped[section.type] || [];
              return (
                <div key={section.type} className="mb-2">
                  <div className="flex items-center gap-1.5 px-2 py-1 text-xs font-medium text-muted-foreground">
                    <section.icon className="h-3 w-3" />
                    {section.label}
                    {sectionPages.length > 0 && (
                      <span className="ml-auto">{sectionPages.length}</span>
                    )}
                  </div>
                  {sectionPages.map((page) => (
                    <div
                      key={page.id}
                      role="button"
                      tabIndex={0}
                      className={cn(
                        "group w-full text-left px-3 py-1.5 text-sm rounded-md hover:bg-accent flex items-center gap-1.5 cursor-pointer",
                        selectedPageId === page.id && "bg-accent font-medium",
                      )}
                      onClick={() => handleSelectPage(page.id)}
                    >
                      <ChevronRightIcon className="h-3 w-3 shrink-0 text-muted-foreground" />
                      <span className="truncate flex-1">{page.title}</span>
                      <button
                        type="button"
                        className="opacity-0 group-hover:opacity-100 text-muted-foreground hover:text-destructive shrink-0"
                        onClick={(e) => {
                          e.stopPropagation();
                          openDelete(page);
                        }}
                        aria-label={t("common.delete")}
                      >
                        <TrashIcon className="h-3.5 w-3.5" />
                      </button>
                    </div>
                  ))}
                </div>
              );
            })}
          </div>
        </ScrollArea>
      </div>

      {/* Center: Markdown preview */}
      <div className="flex-1 flex flex-col min-w-0">
        {showGraph ? (
          <div className="flex-1 flex flex-col">
            <div className="p-3 border-b flex items-center gap-2">
              <h3 className="text-sm font-semibold">{t("wiki.knowledgeGraph")}</h3>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setShowGraph(false)}
              >
                返回
              </Button>
            </div>
            <div ref={graphRef} className="flex-1" />
          </div>
        ) : selectedPage ? (
          <ScrollArea className="flex-1">
            <div className="p-6 max-w-4xl">
              <div className="flex items-center gap-2 mb-4">
                <Badge variant="outline">{selectedPage.page_type}</Badge>
                <h1 className="text-xl font-bold">{selectedPage.title}</h1>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-7 w-7 ml-auto"
                  onClick={() => openDelete(selectedPage)}
                >
                  <TrashIcon className="h-4 w-4" />
                </Button>
              </div>
              <Separator className="mb-4" />
              <div className="prose prose-sm dark:prose-invert max-w-none">
                <ReactMarkdown
                  remarkPlugins={[remarkGfm, remarkBreaks]}
                  components={{
                    a: ExternalAnchor,
                    // Handle [[type:slug]] wiki links inside plain-text
                    // paragraphs. Only runs when children is a string —
                    // ReactMarkdown passes an element array when a
                    // paragraph has inline markup (bold/links/...), and
                    // String()-ing that produced "[object Object]" in
                    // front of the link list. Rich paragraphs render as-is.
                    p: ({ children }) => {
                      if (typeof children !== "string") {
                        return <p>{children}</p>;
                      }
                      const parts = children.split(
                        /\[\[(\w+:[\w-]+)\]\]/g,
                      );
                      if (parts.length <= 1) return <p>{children}</p>;
                      return (
                        <p>
                          {parts.map((part, i) => {
                            if (i % 2 === 1) {
                              return (
                                <button
                                  key={i}
                                  className="text-primary underline hover:text-primary/80"
                                  onClick={() => handleSelectPage(part)}
                                >
                                  {part}
                                </button>
                              );
                            }
                            return <span key={i}>{part}</span>;
                          })}
                        </p>
                      );
                    },
                  }}
                >
                  {selectedPage.body || ""}
                </ReactMarkdown>
              </div>
            </div>
          </ScrollArea>
        ) : (
          <div className="flex-1 flex items-center justify-center text-muted-foreground">
            <div className="text-center">
              <BookOpenIcon className="h-12 w-12 mx-auto mb-4 opacity-50" />
              <h2 className="text-lg font-semibold mb-1">{t("wiki.wikiGraphTitle", { name: agentName })}</h2>
              <p className="text-sm mb-4">
                {agentName
                  ? `${agentName} 的结构化知识库`
                  : "从知识库源生成结构化 Wiki 页面"}
              </p>
              {pages.length === 0 && kbSources.length > 0 && (
                <div className="space-y-2">
                  <Button onClick={() => handleGenerate()} disabled={generating || unprocessedCount === 0}>
                    <SparklesIcon className="h-4 w-4 mr-2" />
                    {generating ? "生成中..." : `生成 Wiki (${unprocessedCount} 待处理)`}
                  </Button>
                  {unprocessedCount < kbSources.length && (
                    <p className="text-xs text-muted-foreground">
                      {t("wiki.sourcesProcessed", { done: kbSources.length - unprocessedCount, total: kbSources.length })}
                      <button
                        className="underline hover:text-foreground ml-1"
                        onClick={handleForceGenerate}
                      >
                        {t("wiki.forceRegenFull")}
                      </button>
                    </p>
                  )}
                </div>
              )}
              {kbSources.length === 0 && (
                <p className="text-xs">
                  请先在知识库管理中添加数据源
                </p>
              )}
            </div>
          </div>
        )}
      </div>
      <AlertDialog
        open={!!deleteTarget}
        onOpenChange={(o) => {
          if (!o) {
            setDeleteTarget(null);
            setDeleteError(null);
          }
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("wiki.deletePageTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteError ?? t("wiki.deletePageConfirm", { name: deleteTarget?.title ?? "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                confirmDelete();
              }}
            >
              {t("common.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
