"use client";
import { useT } from "@/lib/i18n";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
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
  BookOpenIcon,
  ChevronRightIcon,
  DatabaseIcon,
  LightbulbIcon,
  NetworkIcon,
  PanelRightCloseIcon,
  PanelRightOpenIcon,
  TrashIcon,
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
import { cn } from "@/lib/utils";

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
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);

  const [stats, setStats] = useState<WikiStats | null>(null);
  const [pages, setPages] = useState<WikiPage[]>([]);
  const [selectedPageId, setSelectedPageId] = useState<string | null>(null);
  const [selectedPage, setSelectedPage] = useState<WikiPage | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<WikiPage | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  // Dynamic loading: the left pane renders the first `visibleCount`
  // pages and loads more as the user scrolls to the bottom. A continuous
  // list (not pagination) keeps the three-pane interlink intact — the
  // selected page never disappears onto another page.
  const [visibleCount, setVisibleCount] = useState(30);
  const sentinelRef = useRef<HTMLDivElement>(null);

  // Resizable panes + right-pane collapse (Step B). Widths in px; the
  // drag handlers clamp to min/max so a pane can't be sized off-screen.
  const [leftWidth, setLeftWidth] = useState(256);
  const [rightWidth, setRightWidth] = useState(480);
  const [rightCollapsed, setRightCollapsed] = useState(false);

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
    setLoading(true);
    try {
      const [s, p] = await Promise.all([
        getWikiStats(agentId),
        listWikiPages(agentId),
      ]);
      setStats(s);
      setPages(p.pages ?? []);
    } catch {}
    setLoading(false);
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
  // that page is outside the visible window.
  const visiblePages = useMemo(
    () => pages.slice(0, visibleCount),
    [pages, visibleCount],
  );
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
    // has to be rebuilt against the freshly mounted DOM node.
  }, [agentId, rightCollapsed]);

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
        style={{ width: leftWidth }}
        className="shrink-0 border-r bg-muted/30 flex flex-col"
      >
        <div className="p-3 border-b">
          <h3 className="text-sm font-semibold">{t("wiki.title")}</h3>
          {stats && (
            <p className="text-xs text-muted-foreground mt-1">
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
            {visibleCount < pages.length && (
              <div
                ref={sentinelRef}
                className="py-2 text-center text-xs text-muted-foreground"
              >
                {t("common.loading")}
              </div>
            )}
          </div>
        </ScrollArea>
      </div>

      {/* Left/center resize handle */}
      <div
        onPointerDown={(e) => startDrag(e, "left")}
        className="w-1 shrink-0 cursor-col-resize hover:bg-primary/40 transition-colors"
      />

      {/* Center: markdown preview */}
      <div className="flex-1 flex flex-col min-w-0">
        {selectedPage ? (
          <ScrollArea className="flex-1">
            <div className="p-6 max-w-5xl">
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
          className="w-1 shrink-0 cursor-col-resize hover:bg-primary/40 transition-colors"
        />
      )}
      {!rightCollapsed ? (
        <div
          style={{ width: rightWidth }}
          className="shrink-0 border-l flex flex-col"
        >
          <div className="p-3 border-b flex items-center gap-2">
            <NetworkIcon className="h-4 w-4" />
            <h3 className="text-sm font-semibold">{t("wiki.knowledgeGraph")}</h3>
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 ml-auto"
              onClick={() => setRightCollapsed(true)}
              title={t("wiki.collapseGraph")}
            >
              <PanelRightCloseIcon className="h-4 w-4" />
            </Button>
          </div>
          <div ref={graphRef} className="flex-1" />
        </div>
      ) : (
        <button
          onClick={() => setRightCollapsed(false)}
          className="shrink-0 border-l px-1 py-3 flex flex-col items-center gap-1 text-xs text-muted-foreground hover:bg-accent"
          title={t("wiki.expandGraph")}
        >
          <PanelRightOpenIcon className="h-4 w-4" />
          <span className="[writing-mode:vertical-rl] rotate-180">
            {t("wiki.knowledgeGraph")}
          </span>
        </button>
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
