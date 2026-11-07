"use client";

// Workspace side sheet extracted from chat-screen.tsx: the resizable
// WorkspacePanel (file tree + inline viewer + live dev-server preview tab,
// desktop aside + mobile sheet), the FileViewer it hosts, the build-log pane
// behind the preview tab, and the zip-download helper. Pure presentation over
// the files API — no chat state.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ChevronLeft,
  Code2,
  Download,
  ExternalLink,
  Eye,
  File,
  FileText,
  FolderOpen,
  FolderSearch,
  MoreHorizontal,
  PanelLeftClose,
  PanelLeftOpen,
  RefreshCw,
  X,
} from "lucide-react";

import {
  deleteAgentFile,
  fileUrl,
  getChangedFiles,
  getMe,
  getScopePreview,
  getScopePreviewLogs,
  listAgentFiles,
  revealAgentWorkspace,
  type ScopePreview,
  type WorkspaceFile,
} from "@/lib/api";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { ConfirmDeleteDialog } from "@/components/confirm-delete-dialog";
import { usePersistedDragWidth } from "@/hooks/use-drag-width";
import { ChatMarkdown } from "@/components/chat-markdown";
import {
  FileTreeView,
  fileKind,
  formatBytes,
  isSystemFile,
  langForPath,
  type FileTreeNode,
  type ProducedFile,
} from "@/components/chat-file-tree";
import { useT } from "@/lib/i18n";

// zipUrl carries NO bearer token — same cookie-auth rationale as fileUrl
// (see lib/api): a token in the URL leaks a full API credential via Referer,
// history, and proxy logs.
function zipUrl(agentId: string, sessionId: string, projectId?: string): string {
  const params = new URLSearchParams();
  // projectId wins when both are present — same precedence as the
  // backend's fileScopeForRequest, which treats projectId-without-
  // session as "whole project zip".
  if (projectId) params.set("projectId", projectId);
  else if (sessionId) params.set("sessionId", sessionId);
  const qs = params.toString();
  return `/api/agents/${agentId}/files.zip${qs ? "?" + qs : ""}`;
}

// BuildLogView renders the live scaffold/dev log as a scrolling terminal,
// auto-pinned to the bottom so the latest pnpm-install lines stay visible.
function BuildLogView({ text }: { text: string }) {
  const ref = useRef<HTMLPreElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [text]);
  return (
    <pre
      ref={ref}
      className="h-full w-full overflow-auto whitespace-pre-wrap break-words bg-card px-4 py-3 text-left font-mono text-2xs leading-relaxed text-card-foreground"
    >
      {text || "Starting build…"}
    </pre>
  );
}

const FILES_PANEL_MIN = 280;
const FILES_PANEL_MAX = 1000; // wide enough to view a desktop preview iframe
const FILES_PANEL_DEFAULT = 280;
const FILES_PANEL_KEY = "chat:filesPanelWidth";
// Inner file-tree column (left of the viewer). 224 = the old w-56 fixed
// width; the floor keeps long filenames readable, the ceiling leaves the
// viewer at least ~300px inside the default panel.
const FILES_TREE_DEFAULT = 224;
const FILES_TREE_MIN = 150;
const FILES_TREE_MAX = 520;
const FILES_TREE_KEY = "chat:filesTreeWidth";
// When the user switches to the Preview tab and the panel is still narrow,
// auto-grow to this so the embedded site isn't cramped. Transient (not
// persisted), so the Code tab keeps its own saved width.
const PREVIEW_AUTO_WIDTH = 760;

// WorkspacePanel renders the files in the active scope:
//   - chat scope (sessionId set): files produced in this conversation.
//     Project chats also see root-level project files so shared notes
//     are visible alongside the chat's own outputs.
//   - project scope (projectId set, no session): every file under the
//     project — root-level + every chat's subtree. Used on the
//     /agents/<aid>/project/<pid> landing where no specific chat is
//     selected, so the user can still see what's accumulated in the
//     project.
// The agent's shared files (SKILL.md / main.py / templates) are
// excluded by the backend's scope filter so they can't leak into
// either view and confuse "what did this conversation produce".
export function WorkspacePanel({
  agentId,
  sessionId,
  projectId,
  onClose,
  initialFile,
}: {
  agentId: string;
  sessionId: string;
  projectId?: string;
  onClose: () => void;
  initialFile?: ProducedFile | null;
}) {
  const t = useT();
  const [files, setFiles] = useState<WorkspaceFile[]>([]);
  // Scope directory prefixes the backend says to strip before building
  // the tree (e.g. ["projects/<pid>/<sid>/", "projects/<pid>/"] for a
  // project chat). Falls back to a client-side guess below until the
  // listing arrives / when the backend omits it (agent-wide view).
  const [scopePrefixes, setScopePrefixes] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [previewing, setPreviewing] = useState<ProducedFile | null>(null);
  // Live dev-server preview for this chat scope (from start_app_preview).
  const [appPreview, setAppPreview] = useState<ScopePreview>({ status: "none" });
  // Live build/dev log tail, shown in the preview pane while the app is
  // scaffolding so "Building…" isn't an opaque spinner.
  const [buildLogs, setBuildLogs] = useState("");
  // Code (file tree) vs Preview (embedded iframe of the running dev server).
  const [tab, setTab] = useState<"code" | "preview">("code");
  // Collapse the left file tree to give the viewer the full width.
  const [treeCollapsed, setTreeCollapsed] = useState(false);
  // Inner splitter: draggable width of the file-tree column (left of the
  // viewer). Persisted across sessions like the outer panel width; capped
  // so the viewer keeps a usable share even when the panel is narrow.
  // Inner file-tree splitter: clamp against the panel's own left edge so
  // the tree can never eat the whole panel (viewer keeps the remainder).
  const {
    width: treeW,
    startResize: startTreeResize,
  } = usePersistedDragWidth({
    storageKey: FILES_TREE_KEY,
    initial: FILES_TREE_DEFAULT,
    min: FILES_TREE_MIN,
    max: FILES_TREE_MAX,
    compute: (clientX) => clientX - (asideRef.current?.getBoundingClientRect().left ?? 0),
  });
  // Files the agent changed vs the template baseline (so the tree can show
  // just this task's output), and whether to show all files instead.
  const [changed, setChanged] = useState<{ files: WorkspaceFile[]; available: boolean }>({ files: [], available: false });
  const [showAll, setShowAll] = useState(false);
  // Self-hosted-only "open in Finder" affordance. We learn the deploy
  // mode from /api/me on mount; it doesn't change at runtime, so one
  // fetch per panel instance is enough. Hosted deployments leave this
  // null and the button never renders.
  const [deployMode, setDeployMode] = useState<"self-hosted" | "hosted" | null>(null);
  const [revealing, setRevealing] = useState(false);
  useEffect(() => {
    let cancelled = false;
    getMe()
      .then((m) => {
        if (cancelled) return;
        if (m.deployMode === "self-hosted" || m.deployMode === "hosted") {
          setDeployMode(m.deployMode);
        }
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);
  // Outer panel splitter (right-anchored: width = viewport right − pointer).
  const {
    width,
    setWidth,
    resizing,
    startResize,
  } = usePersistedDragWidth({
    storageKey: FILES_PANEL_KEY,
    initial: FILES_PANEL_DEFAULT,
    min: FILES_PANEL_MIN,
    max: FILES_PANEL_MAX,
    compute: (clientX) => window.innerWidth - clientX,
  });
  // Measure the panel's ACTUAL rendered width (not the `width` state, which
  // the CSS maxWidth cap can shrink below on small viewports) so the header
  // can collapse its toolbar before it overflows and pushes a page scroll.
  const asideRef = useRef<HTMLElement>(null);
  const [panelW, setPanelW] = useState<number>(FILES_PANEL_DEFAULT);
  useEffect(() => {
    const el = asideRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver((entries) => {
      for (const e of entries) setPanelW(e.contentRect.width);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  // Below this the secondary action icons fold into a "⋯" menu and the
  // "Files" label drops, so the header always fits the narrow panel.
  const compactHeader = panelW < 480;

  // Inner splitter drag now lives in the usePersistedDragWidth hook above
  // (storageKey FILES_TREE_KEY); this effect is gone.

  const handleReveal = useCallback(async () => {
    if (!agentId || (!sessionId && !projectId)) return;
    setRevealing(true);
    try {
      const res = await revealAgentWorkspace(agentId, sessionId || undefined, projectId);
      if (!res.ok) {
        // Best-effort UX — surface the error inline rather than a
        // toast lib we don't have. The message comes from the
        // backend (e.g. "S3-backed store, no host path").
        // eslint-disable-next-line no-alert
        alert(res.error || "Could not open workspace folder");
      }
    } finally {
      setRevealing(false);
    }
  }, [agentId, sessionId, projectId]);

  const refresh = useCallback(async () => {
    // Project scope (no session) is handled via projectId; chat scope
    // requires sessionId. With neither, there's nothing to fetch.
    if (!agentId || (!sessionId && !projectId)) return;
    setLoading(true);
    try {
      // When projectId is set we skip sessionId — backend scope filter
      // expects exactly one of them to drive the prefix match. Mixing
      // them would fall into the chat-scope branch and miss other
      // chats' files.
      const list = projectId
        ? await listAgentFiles(agentId, undefined, projectId)
        : await listAgentFiles(agentId, sessionId);
      const cleaned = list.files
        .filter((f) => !isSystemFile(f.path))
        .sort((a, b) => (b.modTime || 0) - (a.modTime || 0));
      setFiles(cleaned);
      setScopePrefixes(list.scopePrefixes);
      // Best-effort: is there a live app preview for this scope?
      getScopePreview(agentId, projectId ? undefined : sessionId, projectId)
        .then(setAppPreview)
        .catch(() => setAppPreview({ status: "none" }));
      // Best-effort: which files did the agent change vs the template?
      getChangedFiles(agentId, projectId ? undefined : sessionId, projectId)
        .then(setChanged)
        .catch(() => setChanged({ files: [], available: false }));
    } finally {
      setLoading(false);
    }
  }, [agentId, sessionId, projectId]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // Delete a tree row (file or folder). Folders expand to their leaf
  // files client-side — the store has no directory concept and this
  // keeps LocalFS and S3 backends behaving identically. The shared
  // ConfirmDeleteDialog asks first (count-aware copy for folders); the
  // 403 path (public-agent viewer) surfaces the server's error via alert,
  // same best-effort UX as the reveal button.
  const [deleting, setDeleting] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<FileTreeNode | null>(null);
  const handleDeleteNode = useCallback(
    (node: FileTreeNode) => {
      if (deleting) return;
      setDeleteTarget(node);
    },
    [deleting],
  );
  const confirmDeleteNode = useCallback(async () => {
    const node = deleteTarget;
    if (!node || deleting) return;
    setDeleteTarget(null);
    const paths: string[] = [];
    const collect = (n: FileTreeNode) => {
      if (n.isDir) n.children.forEach(collect);
      else paths.push(n.path);
    };
    collect(node);
    if (paths.length === 0) return;
    setDeleting(true);
    try {
      for (const p of paths) await deleteAgentFile(agentId, p);
      if (previewing && paths.includes(previewing.path)) setPreviewing(null);
      await refresh();
    } catch (err) {
      alert(t("chat.files.deleteFailed", { error: err instanceof Error ? err.message : "unknown" }));
    } finally {
      setDeleting(false);
    }
  }, [agentId, deleting, deleteTarget, previewing, refresh, t]);
  const deleteTargetCount = useMemo(() => {
    if (!deleteTarget) return 0;
    let n = 0;
    const collect = (x: FileTreeNode) => {
      if (x.isDir) x.children.forEach(collect);
      else n++;
    };
    collect(deleteTarget);
    return n;
  }, [deleteTarget]);

  // Strip prefixes for the tree: backend-provided when it resolved the
  // scope (it knows which project a chat belongs to — the URL doesn't);
  // client-side guess otherwise (agent-wide views, listing failure).
  const treePrefixes = scopePrefixes.length
    ? scopePrefixes
    : projectId
      ? [`projects/${projectId}/`]
      : [`sessions/${sessionId}/`];

  // Switching conversations swaps the file tree to the new scope — clear the
  // selected file too, so the viewer never shows a file from the previous
  // conversation (the tree refetches but `previewing` would otherwise linger).
  useEffect(() => {
    setPreviewing(null);
  }, [agentId, sessionId, projectId]);

  // Open the file the caller handed us via initialFile (a FilesPanel row
  // click in the conversation). Fires on pendingFile change — including
  // first mount when the panel was opened by that click — so the tree
  // lands pre-selected instead of showing an empty viewer.
  useEffect(() => {
    if (initialFile) setPreviewing(initialFile);
  }, [initialFile]);

  // While the Preview tab is open, poll the runtime so a "building" preview
  // flips to the live iframe on its own (and reflects sleep/crash). Cheap
  // local call; stops when the tab closes.
  useEffect(() => {
    if (tab !== "preview") return;
    let active = true;
    const sid = projectId ? undefined : sessionId;
    const poll = async () => {
      const p = await getScopePreview(agentId, sid, projectId).catch(() => null);
      if (!active || !p) return;
      setAppPreview(p);
      // While building, tail the live install/dev output for the log pane.
      if (p.status === "scaffolding" || p.status === "starting") {
        const logs = await getScopePreviewLogs(agentId, sid, projectId).catch(() => "");
        if (active) setBuildLogs(logs);
      }
    };
    poll();
    const t = setInterval(poll, 4000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, [tab, agentId, sessionId, projectId]);

  // Open at a comfortable width: the 280px drag-floor is far too cramped to
  // read a file tree + viewer. Grow to PREVIEW_AUTO_WIDTH on mount (panel
  // open) — capped by the 70% container maxWidth, so it never overflows. The
  // user can still drag narrower within the session; reopening re-widens.
  useEffect(() => {
    setWidth((w) => (w < PREVIEW_AUTO_WIDTH ? Math.min(PREVIEW_AUTO_WIDTH, FILES_PANEL_MAX) : w));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Also grow when entering Preview / opening a file (in case the user dragged
  // narrow earlier) so the iframe / viewer column isn't cramped.
  useEffect(() => {
    if (tab === "preview" || previewing) {
      setWidth((w) => (w < PREVIEW_AUTO_WIDTH ? Math.min(PREVIEW_AUTO_WIDTH, FILES_PANEL_MAX) : w));
    }
  }, [tab, previewing]);

  // The Preview tab only exists for coding projects with a live dev server.
  // When there's no app preview, hide the tab and snap back to Files.
  const hasPreview = appPreview.status !== "none";
  useEffect(() => {
    if (!hasPreview && tab === "preview") setTab("code");
  }, [hasPreview, tab]);

  return (
    <>
      <aside
        ref={asideRef}
        // width is the dragged/auto px width, but cap it to 70% of THIS panel's
        // own flex container (the chat-area minus the platform sidebar) — NOT
        // the viewport. A container-relative cap auto-shrinks the panel when the
        // sidebar expands (the container narrows, 70% narrows with it), so the
        // chat always keeps ≥30% and the page never scrolls horizontally. min()
        // still bounds it to FILES_PANEL_MAX on very wide screens. overflow-
        // hidden is the belt-and-suspenders against inner content overflow.
        style={{ width, maxWidth: `min(${FILES_PANEL_MAX}px, 70%)` }}
        className="relative z-30 hidden md:flex shrink-0 flex-col overflow-hidden border-l border-border bg-background -mt-12 h-dvh"
      >
        <div
          onMouseDown={(e) => { e.preventDefault(); startResize(); }}
          className={`absolute left-0 top-0 bottom-0 w-2 cursor-col-resize z-10 group ${resizing ? "" : ""}`}
          title={t("preview.dragResize")}
        >
          <div
            className={`absolute inset-y-0 left-1/2 w-px -translate-x-1/2 transition-colors ${
              resizing ? "bg-primary" : "bg-transparent group-hover:bg-primary/40"
            }`}
          />
        </div>
        <div className="flex h-12 items-center justify-between gap-2 border-b border-border px-4">
          <div className="flex min-w-0 items-center gap-2 text-sm font-medium">
            <FolderOpen className="h-4 w-4 shrink-0" />
            {!compactHeader && <span className="truncate">Workspace</span>}
          </div>
          <div className="flex shrink-0 items-center gap-1">
            {/* Secondary actions: inline on a wide panel, folded into a "⋯"
                menu when the panel is narrow so the toolbar never overflows
                and pushes a horizontal page scroll. */}
            {compactHeader ? (
              <DropdownMenu>
                <DropdownMenuTrigger
                  render={
                    <button
                      className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
                      title={t("preview.moreActions")}
                    >
                      <MoreHorizontal className="h-4 w-4" />
                    </button>
                  }
                />
                <DropdownMenuContent align="end" className="w-44 rounded-lg">
                  {appPreview.status === "running" && appPreview.previewUrl && (
                    <DropdownMenuItem
                      onClick={() =>
                        window.open(appPreview.previewUrl!, "_blank", "noopener,noreferrer")
                      }
                    >
                      <ExternalLink className="h-4 w-4 text-muted-foreground" />
                      <span>{t("preview.openInNewTab")}</span>
                    </DropdownMenuItem>
                  )}
                  <DropdownMenuItem
                    disabled={files.length === 0}
                    onClick={() => {
                      if (files.length === 0) return;
                      const a = document.createElement("a");
                      a.href = zipUrl(agentId, sessionId, projectId);
                      a.rel = "noopener";
                      a.click();
                    }}
                  >
                    <Download className="h-4 w-4 text-muted-foreground" />
                    <span>{t("preview.downloadZip")}</span>
                  </DropdownMenuItem>
                  {deployMode === "self-hosted" && (
                    <DropdownMenuItem
                      disabled={revealing || (!sessionId && !projectId)}
                      onClick={handleReveal}
                    >
                      <FolderSearch className="h-4 w-4 text-muted-foreground" />
                      <span>{t("preview.openInFinder")}</span>
                    </DropdownMenuItem>
                  )}
                  <DropdownMenuItem disabled={loading} onClick={refresh}>
                    <RefreshCw className="h-4 w-4 text-muted-foreground" />
                    <span>{t("preview.refresh")}</span>
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            ) : (
              <>
                {appPreview.status === "running" && appPreview.previewUrl && (
                  <a
                    href={appPreview.previewUrl}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
                    title={`Open preview in new tab: ${appPreview.previewUrl}`}
                  >
                    <ExternalLink className="h-4 w-4" />
                  </a>
                )}
                <a
                  href={files.length > 0 ? zipUrl(agentId, sessionId, projectId) : undefined}
                  aria-disabled={files.length === 0}
                  className={`rounded-md p-1.5 transition-colors ${
                    files.length === 0
                      ? "pointer-events-none text-muted-foreground/40"
                      : "text-muted-foreground hover:bg-muted/50 hover:text-foreground"
                  }`}
                  title={t("preview.downloadAllZip")}
                >
                  <Download className="h-4 w-4" />
                </a>
                {deployMode === "self-hosted" && (
                  <button
                    onClick={handleReveal}
                    disabled={revealing || (!sessionId && !projectId)}
                    className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground disabled:opacity-50"
                    title={t("preview.openFolderFinder")}
                  >
                    <FolderSearch className="h-4 w-4" />
                  </button>
                )}
                <button
                  onClick={refresh}
                  disabled={loading}
                  className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground disabled:opacity-50"
                  title={t("preview.refresh")}
                >
                  <RefreshCw className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} />
                </button>
              </>
            )}
            <button
              onClick={onClose}
              className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
              title={t("preview.close")}
            >
              <X className="h-4 w-4" />
            </button>
          </div>
        </div>
        {/* Row 2: Files (tree) / Preview (dev server) toggle + tree collapse.
            The Preview tab is shown only for coding projects with a live dev
            server; a plain file session (a PDF, some docs) just shows Files. */}
        <div className="flex h-10 items-center justify-between gap-2 border-b border-border px-3">
          {hasPreview ? (
            <div className="flex items-center rounded-md bg-muted p-0.5 text-xs">
              <button
                onClick={() => setTab("code")}
                className={`rounded-md px-2.5 py-1 transition-colors ${
                  tab === "code"
                    ? "bg-background font-medium text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                Files
              </button>
              <button
                onClick={() => setTab("preview")}
                className={`flex items-center gap-1 rounded-md px-2.5 py-1 transition-colors ${
                  tab === "preview"
                    ? "bg-background font-medium text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                Preview
                {(appPreview.status === "starting" || appPreview.status === "scaffolding") && (
                  <RefreshCw className="h-3 w-3 animate-spin" />
                )}
              </button>
            </div>
          ) : (
            <span className="px-1 text-xs font-medium text-muted-foreground">Files</span>
          )}
          {tab === "code" && (
            <button
              onClick={() => setTreeCollapsed((c) => !c)}
              className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
              title={treeCollapsed ? "Show file tree" : "Hide file tree"}
            >
              {treeCollapsed ? <PanelLeftOpen className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}
            </button>
          )}
        </div>
        {tab === "code" ? (
          <div className="flex min-h-0 flex-1">
            {/* Left: file tree (collapsible). */}
            {!treeCollapsed && (
            <div
              className="relative flex shrink-0 flex-col border-r border-border"
              style={{ width: treeW }}
            >
              {/* Inner splitter: drag to resize the tree column against
                  the viewer. Mirrors the outer panel edge handle. */}
              <div
                onMouseDown={(e) => { e.preventDefault(); startTreeResize(); }}
                className="absolute right-0 top-0 bottom-0 z-10 w-1.5 cursor-col-resize hover:bg-border/60"
                title={t("preview.dragResize")}
              />
              {/* When there's a template baseline, default to showing only the
                  files THIS task changed; let the user flip to the full tree. */}
              {changed.available && (
                <div className="flex items-center gap-1 border-b border-border px-3 py-1.5 text-xs">
                  <button
                    onClick={() => setShowAll(false)}
                    className={`rounded-md px-2 py-0.5 transition-colors ${
                      !showAll ? "bg-muted font-medium text-foreground" : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    Changed{changed.files.length ? ` (${changed.files.length})` : ""}
                  </button>
                  <button
                    onClick={() => setShowAll(true)}
                    className={`rounded-md px-2 py-0.5 transition-colors ${
                      showAll ? "bg-muted font-medium text-foreground" : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    All files
                  </button>
                </div>
              )}
              <div className="flex-1 overflow-y-auto p-2">
                {(() => {
                  const showChanged = changed.available && !showAll;
                  const list = showChanged ? changed.files : files;
                  if (!loading && list.length === 0) {
                    return (
                      <p className="px-3 py-8 text-center text-sm text-muted-foreground">
                        {showChanged
                          ? t("chatScreen.filesEmptyChanged")
                          : projectId
                            ? t("chatScreen.filesEmptyProject")
                            : t("chatScreen.filesEmptySession")}
                      </p>
                    );
                  }
                  return (
                    <FileTreeView
                      files={list}
                      rootPrefixes={treePrefixes}
                      onDelete={handleDeleteNode}
                      selectedPath={previewing?.path}
                      onSelect={(f) => setPreviewing(f)}
                    />
                  );
                })()}
              </div>
            </div>
            )}
            {/* Right: viewer for the selected file — overflow-hidden so wide
                content (a PDF, long code lines) never scrolls the panel. */}
            <div className="min-w-0 flex-1 overflow-hidden">
              {previewing ? (
                <FileViewer
                  // Remount on file change so text/error/view state resets and
                  // the new file's content is fetched (not the stale previous).
                  key={previewing.path}
                  agentId={agentId}
                  file={previewing}
                  onClose={() => setPreviewing(null)}
                />
              ) : (
                <div className="flex h-full flex-col items-center justify-center gap-2 px-6 text-center text-muted-foreground">
                  <FileText className="h-6 w-6" />
                  <p className="text-sm">{t("preview.selectFile")}</p>
                </div>
              )}
            </div>
          </div>
        ) : (
          <div className="flex-1 min-h-0">
            {appPreview.status === "running" && appPreview.previewUrl ? (
              <iframe
                src={appPreview.previewUrl}
                className="h-full w-full border-0 bg-white"
                title={t("preview.appPreview")}
              />
            ) : appPreview.status === "starting" || appPreview.status === "scaffolding" ? (
              <div className="flex h-full flex-col">
                <div className="flex items-center gap-2 border-b border-border px-4 py-2 text-xs text-muted-foreground">
                  <RefreshCw className="h-4 w-4 shrink-0 animate-spin" />
                  <span>
                    {appPreview.status === "scaffolding"
                      ? t("preview.installingDeps")
                      : t("preview.startingDev")}
                  </span>
                </div>
                <div className="min-h-0 flex-1">
                  <BuildLogView text={buildLogs} />
                </div>
              </div>
            ) : appPreview.status === "crashed" ? (
              <div className="flex h-full flex-col items-center justify-center gap-2 px-6 text-center">
                <p className="text-sm text-destructive">{t("preview.previewFailed")}</p>
                <p className="text-xs text-muted-foreground">
                  {t("preview.checkLogs")}
                </p>
              </div>
            ) : (
              <div className="flex h-full flex-col items-center justify-center gap-2 px-6 text-center text-muted-foreground">
                <Eye className="h-6 w-6" />
                <p className="text-sm">{t("preview.noPreview")}</p>
                <p className="text-xs">
                  {t("preview.askBuild")}
                </p>
              </div>
            )}
          </div>
        )}
      </aside>

      {/* Shared delete confirmation for workspace tree rows — count-aware
          copy for folders (deleteConfirmDir) replaces the old native
          window.confirm. */}
      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        name={deleteTarget?.name ?? ""}
        description={
          deleteTarget
            ? deleteTarget.isDir
              ? t("chat.files.deleteConfirmDir", { name: deleteTarget.name, count: deleteTargetCount })
              : t("chat.files.deleteConfirmFile", { name: deleteTarget.name })
            : undefined
        }
        onOpenChange={(o) => {
          if (!o) setDeleteTarget(null);
        }}
        onConfirm={confirmDeleteNode}
      />

      {/* Mobile full-screen workspace sheet (md:hidden). The desktop
          aside above is `hidden md:flex`, so without this branch the
          header workspace toggle is a no-op on phones — the panel
          mounts (filesSheetOpen flips) but nothing paints. Master-detail
          shape: the file tree owns the screen; tapping a file swaps in
          the viewer behind a back bar. Shares previewing/files state
          with the desktop aside so the selection survives a breakpoint
          switch mid-session. */}
      <div
        className="fixed inset-0 z-50 flex flex-col bg-background md:hidden"
        style={{
          paddingTop: "env(safe-area-inset-top)",
          paddingBottom: "env(safe-area-inset-bottom)",
        }}
      >
        <div className="flex h-12 shrink-0 items-center justify-between gap-2 border-b border-border px-4">
          <div className="flex min-w-0 items-center gap-2 text-sm font-medium">
            <FolderOpen className="h-4 w-4 shrink-0" />
            <span className="truncate">Workspace</span>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
            aria-label={t("preview.close")}
          >
            <X className="h-4 w-4" />
          </button>
        </div>
        {previewing ? (
          <div className="flex min-h-0 flex-1 flex-col">
            <button
              type="button"
              onClick={() => setPreviewing(null)}
              className="rounded-md flex shrink-0 items-center gap-1 border-b border-border px-4 py-2 text-sm text-muted-foreground transition-colors hover:text-foreground"
            >
              <ChevronLeft className="h-4 w-4 shrink-0" />
              <span className="truncate">{previewing.path.split("/").pop() || previewing.path}</span>
            </button>
            <div className="min-h-0 flex-1 overflow-hidden">
              <FileViewer
                key={previewing.path}
                agentId={agentId}
                file={previewing}
                onClose={() => setPreviewing(null)}
              />
            </div>
          </div>
        ) : (
          <div className="flex-1 overflow-y-auto p-2">
            {(() => {
              const showChanged = changed.available && !showAll;
              const list = showChanged ? changed.files : files;
              if (!loading && list.length === 0) {
                return (
                  <p className="px-3 py-8 text-center text-sm text-muted-foreground">
                    {showChanged
                      ? t("chatScreen.filesEmptyChanged")
                      : projectId
                        ? t("chatScreen.filesEmptyProject")
                        : t("chatScreen.filesEmptySession")}
                  </p>
                );
              }
              return (
                <FileTreeView
                  files={list}
                  rootPrefixes={treePrefixes}
                  onDelete={handleDeleteNode}
                  onSelect={(f) => setPreviewing(f)}
                />
              );
            })()}
          </div>
        )}
      </div>
    </>
  );
}

// FileViewer renders a selected workspace file inline (right column of the
// Files tab): image / pdf / markdown / highlighted text / rendered-or-source
// HTML. onClose, when given, deselects the file.
export function FileViewer({ agentId, file, onClose }: { agentId: string; file: ProducedFile; onClose?: () => void }) {
  const t = useT();
  const { preview } = fileKind(file.path);
  const src = fileUrl(agentId, file.path, false);
  const downloadUrl = fileUrl(agentId, file.path, true);
  const basename = file.path.split("/").pop() || file.path;
  // Directory portion of the file's workspace path (e.g. "sessions/<sid>")
  // so relative image/link URLs in a previewed .md resolve against the
  // file's own location rather than the page URL.
  const baseDir = file.path.includes("/") ? file.path.slice(0, file.path.lastIndexOf("/")) : "";
  const [text, setText] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Default to SOURCE for markdown/html — clicking a file shows its code; the
  // toggle flips to rendered when wanted.
  const [view, setView] = useState<"rendered" | "source">("source");

  useEffect(() => {
    // Fetch the raw text for anything we show as source: markdown, code/text,
    // and html (html starts in source view too).
    if (preview !== "markdown" && preview !== "text" && preview !== "html") return;
    if (text !== null) return;
    let cancelled = false;
    fetch(src)
      .then((r) => { if (!r.ok) throw new Error(`HTTP ${r.status}`); return r.text(); })
      .then((t) => { if (!cancelled) setText(t); })
      .catch((e) => { if (!cancelled) setError(String(e)); });
    return () => { cancelled = true; };
  }, [src, preview, text]);

  return (
    <div className="flex h-full flex-col">
        <div className="flex items-center justify-between border-b border-border px-4 py-2 shrink-0">
          <div className="flex items-center gap-2 min-w-0">
            <FileText className="h-4 w-4 text-muted-foreground shrink-0" />
            <span className="font-medium text-sm truncate">{basename}</span>
            {file.size !== undefined && (
              <span className="text-2xs text-muted-foreground shrink-0">{formatBytes(file.size)}</span>
            )}
          </div>
          <div className="flex items-center gap-1 shrink-0">
            {(preview === "html" || preview === "markdown") && (
              <button
                onClick={() => setView(view === "rendered" ? "source" : "rendered")}
                className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50"
                title={view === "rendered" ? t("preview.viewSource") : t("preview.viewRendered")}
              >
                {view === "rendered" ? <Code2 className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
              </button>
            )}
            <a
              href={src}
              target="_blank"
              rel="noopener noreferrer"
              className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50"
              title={t("preview.openInNewTab")}
            >
              <ExternalLink className="h-4 w-4" />
            </a>
            {onClose && (
              <button
                onClick={onClose}
                className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50"
                title={t("preview.closeFile")}
              >
                <X className="h-4 w-4" />
              </button>
            )}
          </div>
        </div>
        <div className="min-h-0 flex-1">
          {preview === "image" && (
            <div className="flex h-full items-center justify-center overflow-auto p-4">
              <img src={src} alt={basename} className="max-h-full max-w-full object-contain" />
            </div>
          )}
          {preview === "pdf" && (
            <iframe src={src} className="h-full w-full border-0" title={basename} />
          )}
          {(preview === "markdown" || preview === "text" || preview === "html") && (
            // Rendered view (markdown / html) only when toggled; otherwise the
            // default is full-bleed highlighted SOURCE.
            preview !== "text" && view === "rendered" ? (
              preview === "html" ? (
                // sandbox="allow-scripts": CSS/animations work; scripts can't
                // reach parent cookies/storage — safe for untrusted output.
                <iframe
                  src={src}
                  sandbox="allow-scripts"
                  className="h-full w-full border-0 bg-white"
                  title={basename}
                />
              ) : (
                <div className="h-full overflow-auto p-4">
                  <ChatMarkdown text={text ?? ""} agentId={agentId} baseDir={baseDir} singleDollarMath />
                </div>
              )
            ) : error ? (
              <p className="p-4 text-sm text-destructive">{t("preview.loadFailed", { error })}</p>
            ) : text === null ? (
              <p className="p-4 text-sm text-muted-foreground">{t("preview.loading")}</p>
            ) : text.includes("```") ? (
              // Content with its own fences would break the fenced wrapper —
              // fall back to a plain (unhighlighted) full-bleed block.
              <pre className="h-full overflow-auto whitespace-pre-wrap break-all p-3 font-mono text-xs">{text}</pre>
            ) : (
              // Shiki-highlighted source, no card / copy pill / padding —
              // fills the pane edge-to-edge.
              <div className="h-full overflow-auto">
                <ChatMarkdown bareCode text={"```" + langForPath(file.path) + "\n" + text + "\n```"} />
              </div>
            )
          )}
          {preview === "none" && (
            <div className="flex h-full flex-col items-center justify-center gap-3 p-4 text-center">
              <File className="h-12 w-12 text-muted-foreground/50" />
              <p className="text-sm text-muted-foreground">{t("preview.notAvailable")}</p>
              <a href={downloadUrl} className="inline-flex items-center gap-2 rounded-md bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground hover:bg-primary/90">
                <Download className="h-3.5 w-3.5" /> {t("preview.download")}
              </a>
            </div>
          )}
        </div>
    </div>
  );
}
