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
//   Right  — knowledge graph (vis-network), always mounted
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

export default function WikiPage() {
  const t = useT();
  usePageHeader(<h1 className="text-sm font-semibold">{t("wiki.title")}</h1>, []);
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  // vis-network colors can't follow CSS variables, so the graph reads the
  // resolved theme and rebuilds on change.
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

  // The right-pane graph is always mounted. networkRef persists the
  // vis-network instance across renders so we can focus/select nodes
  // when the selected page changes. handleSelectPageRef lets the
  // graph's click handler call the latest selector without forcing the
  // network-build effect to depend on (and rebuild on) every change.
  const graphRef = useRef<HTMLDivElement>(null);
  const networkRef = useRef<import("vis-network").Network | null>(null);
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
      const [{ Network }, { DataSet }] = await Promise.all([
        import("vis-network/standalone"),
        import("vis-data/standalone"),
      ]);
      if (cancelled || !graphRef.current) return;
      const g = await getWikiGraph(agentId);
      if (cancelled) return;

      // vis-network colors can't follow CSS variables, so pick from the
      // resolved theme. Dot labels render on the pane background (not on
      // the node), so the old fixed #e5e7eb was near-invisible on the
      // light pane background. Edges get the matching contrast tone too.
      const isDark = resolvedTheme !== "light";
      const labelColor = isDark ? "#e5e7eb" : "#1f2937";
      const edgeColor = isDark ? "#555" : "#9ca3af";
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
          font: { size: 11, color: labelColor },
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
          color: { color: edgeColor, opacity: 0.5 },
          width: 1,
        })),
      );
      const network = new Network(graphRef.current, { nodes, edges }, {
        physics: { stabilization: { iterations: 100 }, solver: "forceAtlas2Based" },
        interaction: { hover: true, tooltipDelay: 200 },
        edges: { smooth: true },
      });
      networkRef.current = network;
      // Three-pane interlink: clicking a graph node selects that page.
      network.on("click", (params: { nodes?: string[] }) => {
        if (params.nodes?.length) {
          handleSelectPageRef.current?.(params.nodes[0]);
        }
      });
    };
    init();
    return () => {
      cancelled = true;
      if (networkRef.current) {
        networkRef.current.destroy();
        networkRef.current = null;
      }
    };
    // Re-run when the right pane re-mounts after a collapse/expand:
    // collapsing unmounts the graphRef div, so on expand the network
    // has to be rebuilt against the freshly mounted DOM node. Also
    // re-run on theme switch so node labels/edges pick up the new tone.
  }, [agentId, rightCollapsed, resolvedTheme]);

  // Selection sync: focus + highlight the matching node whenever the
  // selected page changes (left-list click OR graph-node click).
  useEffect(() => {
    const net = networkRef.current;
    if (!net || !selectedPageId) return;
    try {
      net.selectNodes([selectedPageId]);
      net.focus(selectedPageId, {
        scale: 1.2,
        animation: { duration: 400, easingFunction: "easeInOutQuad" },
      });
    } catch {
      // Node may not exist in the graph (e.g. a page with no edges);
      // selecting it is best-effort, not fatal.
    }
  }, [selectedPageId]);

  // Mobile graph overlay: opening flips the pane display:none → fixed
  // full-screen. vis-network's autoResize usually adapts, but explicitly
  // fit + redraw shortly after open so the graph fills the overlay at once.
  useEffect(() => {
    if (!showGraphOverlay || !networkRef.current) return;
    const id = setTimeout(() => {
      networkRef.current?.fit();
      networkRef.current?.redraw();
    }, 60);
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
