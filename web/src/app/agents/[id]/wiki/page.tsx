"use client";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Separator } from "@/components/ui/separator";
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
  ArrowLeftIcon,
  BookOpenIcon,
  ChevronRightIcon,
  DatabaseIcon,
  LightbulbIcon,
  NetworkIcon,
  PanelRightCloseIcon,
  PanelRightOpenIcon,
  SearchIcon,
  TrashIcon,
  XIcon,
} from "lucide-react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import { ExternalAnchor } from "@/components/markdown-link";
import {
  type WikiPage,
  type WikiStats,
  getWikiStats,
  listWikiPages,
  getWikiPage,
  getWikiGraph,
  deleteWikiPage,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";
import { useTheme } from "@/components/theme-provider";
import { cn } from "@/lib/utils";
import { readCache, writeCache } from "@/lib/page-data-cache";

// WikiPage is the three-pane wiki browser at /agents/<id>/wiki/.
//   Left   — page list grouped by type (overview/entity/concept/source)
//   Center — markdown preview of the selected page
//   Right  — knowledge graph (WebGL engine, see components/wiki-graph/engine)
// All three panes are interlinked: selecting a page (left list or graph
// node click) updates the center preview AND focuses/highlights the
// node in the graph. Panes are resizable via draggable dividers; the
// right pane can collapse to a thin rail. Auto-gen settings + generate
// actions live in the Settings dialog (WikiAutoGenSettingsCard).
const PAGE_TYPE_SECTIONS = (t: ReturnType<typeof useT>) => [
  { type: "entity", label: t("wiki.entity"), icon: DatabaseIcon },
  { type: "concept", label: t("wiki.concept"), icon: LightbulbIcon },
  { type: "source", label: t("wiki.source"), icon: BookOpenIcon },
];

// Graph palette per resolved theme. The WebGL engine can't read CSS
// variables, so colors are resolved here; node fills mirror the page-type
// colors the left pane groups by.
const graphTheme = (isDark: boolean): import("@/components/wiki-graph/engine").GraphTheme => ({
  bg: isDark ? "#0a0a0a" : "#ffffff",
  node: "#6b7280",
  label: isDark ? "#e5e7eb" : "#1f2937",
  edge: isDark ? "#555" : "#9ca3af",
  edgeHover: isDark ? "#9ca3af" : "#4b5563",
  selectedBorder: "#8b5cf6",
  typeColors: {
    overview: "#8b5cf6",
    entity: "#3b82f6",
    concept: "#10b981",
    source: "#f59e0b",
    query: "#ef4444",
  },
});

export default function WikiPage() {
  const t = useT();
  usePageHeader(<h1 className="text-sm font-semibold">{t("wiki.title")}</h1>, []);
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  // The graph is a WebGL engine whose colors can't follow CSS variables, so
  // it reads the resolved theme and hot-swaps its palette on change.
  const { resolvedTheme } = useTheme();

  // Stale-while-revalidate: seed stats/pages from the module cache so a
  // return visit (e.g. flipping to /knowledge/ and back) paints at once
  // instead of flashing the loading spinner. loadData() revalidates.
  const [initialWiki] = useState(() =>
    agentId ? readCache<{ stats: WikiStats | null; pages: WikiPage[] }>(`wiki:${agentId}`) : undefined,
  );
  const [stats, setStats] = useState<WikiStats | null>(initialWiki?.stats ?? null);
  const [pages, setPages] = useState<WikiPage[]>(initialWiki?.pages ?? []);
  const [query, setQuery] = useState("");
  const [selectedPageId, setSelectedPageId] = useState<string | null>(null);
  const [selectedPage, setSelectedPage] = useState<WikiPage | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<WikiPage | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [loading, setLoading] = useState(!initialWiki);

  // Dynamic loading: the left pane renders the first `visibleCount`
  // pages and loads more as the user scrolls to the bottom. A continuous
  // list (not pagination) keeps the three-pane interlink intact — the
  // selected page never disappears onto another page.
  const [visibleCount, setVisibleCount] = useState(20);
  const sentinelRef = useRef<HTMLDivElement>(null);

  // Resizable panes + right-pane collapse (Step B). Widths in px; the
  // drag handlers clamp to min/max so a pane can't be sized off-screen.
  const [leftWidth, setLeftWidth] = useState(256);
  const [rightWidth, setRightWidth] = useState(480);
  const [rightCollapsed, setRightCollapsed] = useState(false);
  // Mobile-only: knowledge graph renders as a full-screen overlay below lg,
  // toggled by buttons in the list/preview headers. PC keeps the inline pane.
  const [showGraphOverlay, setShowGraphOverlay] = useState(false);

  // The right-pane graph is always mounted. engineRef persists the
  // WikiGraphEngine instance across renders so we can focus nodes when the
  // selected page changes. handleSelectPageRef lets the engine's click
  // handler call the latest selector without forcing the engine-build
  // effect to depend on (and rebuild on) every change.
  const graphRef = useRef<HTMLDivElement>(null);
  const engineRef = useRef<import("@/components/wiki-graph/engine").WikiGraphEngine | null>(null);
  const handleSelectPageRef = useRef<(id: string) => void>(() => {});

  const loadData = useCallback(async () => {
    if (!agentId) return;
    // Revalidate silently when a cached snapshot is already on screen —
    // avoids the loading flash on every route-switch return.
    const silent = !!readCache(`wiki:${agentId}`);
    if (!silent) setLoading(true);
    try {
      const [s, p] = await Promise.all([
        getWikiStats(agentId),
        listWikiPages(agentId),
      ]);
      setStats(s);
      setPages(p.pages ?? []);
      writeCache(`wiki:${agentId}`, { stats: s, pages: p.pages ?? [] });
    } catch {}
    if (!silent) setLoading(false);
  }, [agentId]);

  useEffect(() => {
    loadData();
  }, [loadData]);

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
  handleSelectPageRef.current = handleSelectPage;
  // Mirrors selectedPageId for the async engine-build effect: lets the
  // freshly built engine replay a focus for a page selected while it was
  // still loading, without the effect depending on the state itself.
  const selectedPageIdRef = useRef<string | null>(null);
  selectedPageIdRef.current = selectedPageId;

  // Deep link: /wiki/?page=<pageId> selects that page once on mount —
  // the cards source link lands here (page ids are "type:slug" pairs,
  // URL-encoded by the sender). One-shot via ref; window.location.search
  // (not useSearchParams) keeps the static-export page out of suspense.
  const deeplinkedRef = useRef(false);
  useEffect(() => {
    if (!agentId || deeplinkedRef.current) return;
    const p = new URLSearchParams(window.location.search).get("page");
    if (p) {
      deeplinkedRef.current = true;
      handleSelectPage(p);
    }
  }, [agentId, handleSelectPage]);

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

  // Drag a vertical divider to resize the left/right panes. Pointer
  // events attach to the document so the drag keeps tracking even when
  // the cursor leaves the thin handle. Left handle: drag right = grow;
  // right handle: drag left = grow (delta inverted).
  const startDrag = (e: React.PointerEvent, which: "left" | "right") => {
    e.preventDefault();
    const startX = e.clientX;
    const startLeft = leftWidth;
    const startRight = rightWidth;
    const move = (ev: PointerEvent) => {
      const dx = ev.clientX - startX;
      if (which === "left") {
        setLeftWidth(Math.min(520, Math.max(200, startLeft + dx)));
      } else {
        setRightWidth(Math.min(760, Math.max(320, startRight - dx)));
      }
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

  // Group the first `visibleCount` pages by type for the left-pane
  // sections. Slicing BEFORE grouping keeps per-type counts consistent
  // with what's actually shown. titleMap still uses the full list so
  // in-body [[wiki links]] resolve to the target page's title even when
  // that page is outside the visible window. While searching, the title
  // filter replaces the window (no slice) so every match is reachable.
  const visiblePages = useMemo(() => {
    const q = query.trim().toLowerCase();
    const matched = q ? pages.filter((p) => p.title.toLowerCase().includes(q)) : pages;
    return q ? matched : matched.slice(0, visibleCount);
  }, [pages, visibleCount, query]);
  const grouped = useMemo(() => {
    const m: Record<string, WikiPage[]> = {};
    for (const p of visiblePages) {
      if (!m[p.page_type]) m[p.page_type] = [];
      m[p.page_type].push(p);
    }
    return m;
  }, [visiblePages]);

  // titleMap resolves a wiki-link target ("page_type:slug") to its
  // display title so rendered links read as the target page's name.
  const titleMap = useMemo(() => {
    const m: Record<string, string> = {};
    for (const p of pages) {
      m[`${p.page_type}:${p.slug}`] = p.title;
    }
    return m;
  }, [pages]);

  // Rewrite [[type:slug]] / [[type:slug|alias]] wiki links into markdown
  // links on a relative /wiki-link/ path (survives ReactMarkdown's URL
  // sanitizer; the a-renderer below intercepts it).
  const renderedBody = useMemo(() => {
    if (!selectedPage) return "";
    return (selectedPage.body || "").replace(
      /\[\[(\w+:[\w-]+)(?:\|([^\]]+))?\]\]/g,
      (_, link, display) =>
        `[${display || titleMap[link] || link}](/wiki-link/${link})`,
    );
  }, [selectedPage, titleMap]);

  // Build the graph once per agent. Right pane is always mounted.
  useEffect(() => {
    if (!graphRef.current || !agentId) return;
    let cancelled = false;
    const init = async () => {
      // Dynamic import keeps pixi.js + d3 out of the initial page bundle.
      const { WikiGraphEngine } = await import("@/components/wiki-graph/engine");
      if (cancelled || !graphRef.current) return;
      const g = await getWikiGraph(agentId);
      if (cancelled || !graphRef.current) return;
      const engine = await WikiGraphEngine.create(graphRef.current, {
        theme: graphTheme(resolvedTheme !== "light"),
        onNodeClick: (id: string) => handleSelectPageRef.current?.(id),
      });
      if (cancelled) {
        engine.destroy();
        return;
      }
      engine.setData(
        g.nodes.map((n) => ({
          id: n.id,
          label: n.title.length > 12 ? n.title.slice(0, 12) + "…" : n.title,
          pageType: n.page_type,
        })),
        (g.edges ?? []).map((e) => ({ source: e.src_page_id, target: e.dst_page_id })),
      );
      // If a page was already selected before the engine finished loading
      // (e.g. deep link), replay the focus now that nodes exist.
      if (selectedPageIdRef.current) engine.focusNode(selectedPageIdRef.current);
      engineRef.current = engine;
    };
    init();
    return () => {
      cancelled = true;
      if (engineRef.current) {
        engineRef.current.destroy();
        engineRef.current = null;
      }
    };
    // Re-run when the right pane re-mounts after a collapse/expand:
    // collapsing unmounts the graphRef div, so on expand the engine has to
    // be rebuilt against the freshly mounted DOM node. Theme changes are
    // handled hot by the setTheme effect below (no rebuild).
  }, [agentId, rightCollapsed]);

  // Theme swap is hot: the WebGL engine recolors without rebuilding.
  useEffect(() => {
    engineRef.current?.setTheme(graphTheme(resolvedTheme !== "light"));
  }, [resolvedTheme]);

  // Selection sync: focus + highlight the matching node whenever the
  // selected page changes (left-list click OR graph-node click).
  useEffect(() => {
    if (!selectedPageId) return;
    // Node may not exist in the graph (e.g. a page with no edges);
    // focusing it is best-effort, not fatal.
    engineRef.current?.focusNode(selectedPageId);
  }, [selectedPageId]);

  // Mobile graph overlay: opening flips the pane display:none → fixed
  // full-screen. The engine's ResizeObserver adapts the canvas; explicitly
  // re-fit shortly after open so the graph fills the overlay at once.
  useEffect(() => {
    if (!showGraphOverlay) return;
    const id = setTimeout(() => engineRef.current?.fitView(), 60);
    return () => clearTimeout(id);
  }, [showGraphOverlay]);

  // Re-selecting the active "Wiki" sidebar item clears the center preview
  // (the URL is unchanged, so navigateOnce would otherwise no-op).
  useEffect(() => {
    const onReselect = (e: Event) => {
      const url = (e as CustomEvent<{ url?: string }>).detail?.url ?? "";
      if (url.includes("/wiki/")) {
        setSelectedPageId(null);
        setSelectedPage(null);
      }
    };
    window.addEventListener("fluctio:nav-reselect", onReselect);
    return () => window.removeEventListener("fluctio:nav-reselect", onReselect);
  }, []);

  // Infinite-scroll: when the sentinel at the bottom of the left pane
  // enters the viewport, reveal the next batch. No-op once everything is
  // visible (the sentinel unmounts in that case).
  useEffect(() => {
    const el = sentinelRef.current;
    if (!el) return;
    const io = new IntersectionObserver((entries) => {
      if (entries[0]?.isIntersecting) {
        setVisibleCount((c) => Math.min(c + 30, pages.length));
      }
    });
    io.observe(el);
    return () => io.disconnect();
  }, [pages.length]);

  return (
    <div className="flex h-[calc(100vh-3.5rem)]">
      {/* Left: page list grouped by type */}
      <div
        style={{ "--pane-lw": `${leftWidth}px` } as any}
        className={cn(
          "border-r bg-muted/30 flex-col w-full md:w-[var(--pane-lw)] md:shrink-0",
          selectedPageId ? "hidden md:flex" : "flex",
        )}
      >
        <div className="space-y-2 border-b p-3">
          <div className="flex items-center gap-2">
            <div className="relative min-w-0 flex-1">
              <SearchIcon className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder={t("wiki.search")}
                className="h-7 w-full rounded-md pl-8 text-xs"
              />
            </div>
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 shrink-0 lg:hidden"
              onClick={() => setShowGraphOverlay(true)}
              title={t("wiki.knowledgeGraph")}
            >
              <NetworkIcon className="h-4 w-4" />
            </Button>
          </div>
          {stats && (
            <p className="text-xs text-muted-foreground">
              {t("wiki.pageStats", {
                pages: stats.total_pages,
                links: stats.total_edges,
              })}
            </p>
          )}
        </div>
        <ScrollArea className="flex-1">
          <div className="p-2">
            {loading ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">
                {t("common.loading")}
              </p>
            ) : pages.length === 0 ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">
                {t("wiki.noPages")}
              </p>
            ) : (
              PAGE_TYPE_SECTIONS(t).map((section) => {
                const sectionPages = grouped[section.type] || [];
                if (sectionPages.length === 0) return null;
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
              })
            )}
            {!loading && pages.length > 0 && query.trim() !== "" && Object.values(grouped).every((a) => !a || a.length === 0) && (
              <p className="px-2 py-1.5 text-xs text-muted-foreground">
                {t("knowledge.noSearchResult")}
              </p>
            )}
            {!query.trim() && visibleCount < pages.length && (
              <div ref={sentinelRef} className="py-2 flex justify-center">
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-xs text-muted-foreground"
                  onClick={() => setVisibleCount((c) => Math.min(c + 20, pages.length))}
                >
                  {t("wiki.loadMore")}
                </Button>
              </div>
            )}
          </div>
        </ScrollArea>
      </div>

      {/* Left/center resize handle */}
      <div
        onPointerDown={(e) => startDrag(e, "left")}
        className="hidden md:block w-1 shrink-0 cursor-col-resize hover:bg-primary/40 transition-colors"
      />

      {/* Center: markdown preview */}
      <div className={cn("flex-1 flex-col min-w-0", selectedPageId ? "flex" : "hidden md:flex")}>
        {selectedPage ? (
          <ScrollArea className="flex-1">
            <div className="p-6 max-w-5xl">
              <div className="flex items-center gap-2 mb-4">
                <button
                  type="button"
                  onClick={() => { setSelectedPageId(null); setSelectedPage(null); }}
                  className="md:hidden -ml-1 shrink-0 text-muted-foreground hover:text-foreground"
                  aria-label={t("common.back")}
                >
                  <ArrowLeftIcon className="h-5 w-5" />
                </button>
                <Badge variant="outline">{selectedPage.page_type}</Badge>
                <h1 className="text-xl font-bold flex-1 min-w-0 truncate">{selectedPage.title}</h1>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-7 w-7 shrink-0 lg:hidden"
                  onClick={() => setShowGraphOverlay(true)}
                  title={t("wiki.knowledgeGraph")}
                >
                  <NetworkIcon className="h-4 w-4" />
                </Button>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-7 w-7 shrink-0"
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
                    a: (props) => {
                      const { href, children } = props;
                      if (href && href.startsWith("/wiki-link/")) {
                        return (
                          <button
                            type="button"
                            className="text-primary underline hover:text-primary/80 cursor-pointer inline bg-transparent border-0 p-0 font-inherit"
                            onClick={() =>
                              handleSelectPage(href.slice("/wiki-link/".length))
                            }
                          >
                            {children}
                          </button>
                        );
                      }
                      return ExternalAnchor(props);
                    },
                  }}
                >
                  {renderedBody}
                </ReactMarkdown>
              </div>
            </div>
          </ScrollArea>
        ) : (
          <div className="flex-1 flex items-center justify-center text-muted-foreground text-center px-4">
            <div>
              <BookOpenIcon className="h-12 w-12 mx-auto mb-4 opacity-50" />
              <h2 className="text-lg font-semibold mb-1">
                {t("wiki.wikiGraphTitle", { name: agentName })}
              </h2>
              <p className="text-sm">{t("wiki.selectPagePrompt")}</p>
            </div>
          </div>
        )}
      </div>

      {/* Center/right resize handle + right-pane collapse */}
      {!rightCollapsed && (
        <div
          onPointerDown={(e) => startDrag(e, "right")}
          className="hidden lg:block w-1 shrink-0 cursor-col-resize hover:bg-primary/40 transition-colors"
        />
      )}
      {!rightCollapsed ? (
        <div
          style={{ ["--pane-rw" as any]: `${rightWidth}px` }}
          className={cn(
            "flex-col",
            showGraphOverlay ? "fixed inset-0 z-50 bg-background flex" : "hidden",
            "lg:static lg:z-auto lg:inset-auto lg:bg-transparent lg:border-l lg:shrink-0 lg:w-[var(--pane-rw)] lg:flex",
          )}
        >
          <div className="p-3 border-b flex items-center gap-2">
            <NetworkIcon className="h-4 w-4" />
            <h3 className="text-sm font-semibold">{t("wiki.knowledgeGraph")}</h3>
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 ml-auto lg:hidden"
              onClick={() => setShowGraphOverlay(false)}
              title={t("common.close")}
            >
              <XIcon className="h-4 w-4" />
            </Button>
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 ml-auto hidden lg:inline-flex"
              onClick={() => setRightCollapsed(true)}
              title={t("wiki.collapseGraph")}
            >
              <PanelRightCloseIcon className="h-4 w-4" />
            </Button>
          </div>
          <div ref={graphRef} className="flex-1" />
        </div>
      ) : (
        <Button
          variant="ghost"
          size="icon"
          className="m-2 self-start hidden lg:inline-flex"
          onClick={() => setRightCollapsed(false)}
          title={t("wiki.expandGraph")}
          aria-expanded={false}
        >
          <PanelRightOpenIcon className="h-4 w-4" />
          <span className="sr-only">{t("wiki.knowledgeGraph")}</span>
        </Button>
      )}

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
              {deleteError ??
                t("wiki.deletePageConfirm", { name: deleteTarget?.title ?? "" })}
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
