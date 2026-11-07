"use client";

// File-tree family extracted from chat-screen.tsx: the produced-file list
// under a chat bubble (FilesPanel), the workspace directory tree
// (FileTreeView / FileTreeRow / buildFileTree / sortFileTree), and the
// file-type helpers (fileKind / formatBytes / langForPath) shared with the
// inline FileViewer. Pure rendering + pure transforms — no chat state.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ChevronDown,
  ChevronRight,
  Download,
  File,
  FileCode,
  FileText,
  Film,
  Folder,
  FolderOpen,
  Image as ImageIcon,
  Music,
  Trash2,
} from "lucide-react";

import { fileUrl, type WorkspaceFile } from "@/lib/api";
import { useT } from "@/lib/i18n";

// Shape of a produced-file entry. Structurally compatible with chat-screen's
// richer ProducedFile (which adds modTime for message attribution), so the
// caller can pass its instances without a cast.
export type ProducedFile = {
  path: string;
  size?: number;
};

export function fileKind(path: string): { icon: typeof File; preview: "image" | "pdf" | "markdown" | "html" | "text" | "none" } {
  const ext = path.toLowerCase().split(".").pop() || "";
  if (["png", "jpg", "jpeg", "jfif", "gif", "svg", "webp", "bmp", "ico", "avif", "apng"].includes(ext)) return { icon: ImageIcon, preview: "image" };
  if (ext === "pdf") return { icon: FileText, preview: "pdf" };
  if (ext === "md" || ext === "markdown") return { icon: FileText, preview: "markdown" };
  if (ext === "html" || ext === "htm") return { icon: FileCode, preview: "html" };
  if (["mp4", "webm", "mov", "mkv", "avi", "m4v"].includes(ext)) return { icon: Film, preview: "none" };
  if (["mp3", "wav", "ogg", "flac", "m4a", "aac"].includes(ext)) return { icon: Music, preview: "none" };
  // Genuinely binary formats → download only. Everything else is treated as
  // plain text below (so .env / Dockerfile / .sql / SKILL / extension-less /
  // unknown-but-textual files render their content instead of a download
  // prompt). `.text()` on a binary that slips through just shows garbage —
  // an acceptable trade for never hiding a readable file.
  if (
    [
      "zip", "tar", "gz", "tgz", "bz2", "7z", "rar", "xz", "zst",
      "woff", "woff2", "ttf", "otf", "eot",
      "exe", "dll", "so", "dylib", "bin", "dat", "wasm", "class", "node",
      "doc", "docx", "xls", "xlsx", "ppt", "pptx",
      "db", "sqlite", "sqlite3", "mdb",
    ].includes(ext)
  ) {
    return { icon: File, preview: "none" };
  }
  // Known code extensions get the code icon; all other textual files fall
  // through to a plain-text view with a generic file icon.
  if (
    ["js", "ts", "tsx", "jsx", "mjs", "cjs", "py", "go", "rs", "c", "cpp", "h", "java",
     "rb", "sh", "bash", "zsh", "json", "jsonc", "yaml", "yml", "toml", "xml", "css",
     "scss", "sql", "dockerfile"].includes(ext)
  ) {
    return { icon: FileCode, preview: "text" };
  }
  return { icon: FileText, preview: "text" };
}

export function formatBytes(n?: number): string {
  if (n === undefined) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

export function FilesPanel({ agentId, files, onOpenFile }: { agentId: string; files: ProducedFile[]; onOpenFile: (f: ProducedFile) => void }) {
  const t = useT();
  return (
    <div className="mt-2 space-y-1.5 max-w-[85%]">
      <p className="text-xs font-medium uppercase tracking-wider text-muted-foreground/70">
        {t("chat.files")}
      </p>
      <div className="flex flex-col gap-1.5">
        {files.map((f) => {
          const { icon: Icon } = fileKind(f.path);
          const basename = f.path.split("/").pop() || f.path;
          const downloadUrl = fileUrl(agentId, f.path, true);
          return (
            <div
              key={f.path}
              className="group flex items-center gap-2.5 rounded-lg border border-border bg-card/50 px-3 py-2 hover:bg-card/80 transition-colors"
            >
              <Icon className="h-4 w-4 text-muted-foreground shrink-0" />
              <button
                onClick={() => onOpenFile(f)}
                className="flex-1 min-w-0 text-left"
              >
                <div className="text-sm font-medium text-foreground truncate">{basename}</div>
                {f.size !== undefined && (
                  <div className="text-2xs text-muted-foreground/70">{formatBytes(f.size)}</div>
                )}
              </button>
              <a
                href={downloadUrl}
                className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors"
                title={t("preview.download")}
              >
                <Download className="h-4 w-4" />
              </a>
            </div>
          );
        })}
      </div>
    </div>
  );
}

export type FileTreeNode = {
  name: string;
  path: string; // full workspace-relative path (files) or the folder path (dirs)
  isDir: boolean;
  size?: number;
  children: FileTreeNode[];
};

// buildFileTree turns the flat file list into a nested tree. stripPrefixes
// (e.g. ["projects/<pid>/<sid>/", "projects/<pid>/"]) are tried in order and
// the first one the path starts with is removed for the tree STRUCTURE —
// for a project chat that renders its own files bare while shared entries
// keep their subdirectory structure (math-course/…) instead of nesting under
// a redundant projects/<pid>/ folder. File leaves keep their FULL path, which
// the preview/download URLs need. Folders are synthesized from the remaining
// segments; their `path` is the relative path (a stable, unique toggle key).
export function buildFileTree(files: WorkspaceFile[], stripPrefixes: string[]): FileTreeNode[] {
  const root: FileTreeNode = { name: "", path: "", isDir: true, children: [] };
  for (const f of files) {
    let rel = f.path;
    for (const p of stripPrefixes) {
      if (p && rel.startsWith(p)) {
        rel = rel.slice(p.length);
        break;
      }
    }
    const parts = rel.split("/").filter(Boolean);
    if (parts.length === 0) continue;
    let node = root;
    for (let i = 0; i < parts.length; i++) {
      const isLeaf = i === parts.length - 1;
      const name = parts[i];
      let child = node.children.find((c) => c.name === name && c.isDir === !isLeaf);
      if (!child) {
        child = isLeaf
          ? { name, path: f.path, isDir: false, size: f.size, children: [] }
          : { name, path: parts.slice(0, i + 1).join("/"), isDir: true, children: [] };
        node.children.push(child);
      }
      node = child;
    }
  }
  sortFileTree(root.children);
  return root.children;
}

function sortFileTree(nodes: FileTreeNode[]) {
  nodes.sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1; // folders before files
    return a.name.localeCompare(b.name);
  });
  for (const n of nodes) if (n.isDir) sortFileTree(n.children);
}

export function FileTreeView({
  files,
  rootPrefixes,
  selectedPath,
  onSelect,
  onDelete,
  defaultExpandDepth = 1,
}: {
  files: WorkspaceFile[];
  rootPrefixes: string[];
  selectedPath?: string;
  onSelect: (f: ProducedFile) => void;
  // Optional per-row delete affordance (owner-only server-side). Called
  // with the row's node — folders included; the handler expands them.
  onDelete?: (node: FileTreeNode) => void;
  // Folders shallower than this are open on first load (1 = open the root
  // folders only) so the user sees the top entries without a deep dump.
  defaultExpandDepth?: number;
}) {
  const tree = useMemo(() => buildFileTree(files, rootPrefixes), [files, rootPrefixes]);
  // Expansion state keys on stable relative paths, so it survives refreshes.
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  // Auto-expand the first `defaultExpandDepth` folder levels once, when the
  // tree first arrives (files load async). User toggles persist after that.
  const initedRef = useRef(false);
  useEffect(() => {
    if (initedRef.current || tree.length === 0) return;
    initedRef.current = true;
    const next = new Set<string>();
    const walk = (nodes: FileTreeNode[], depth: number) => {
      for (const n of nodes) {
        if (n.isDir && depth < defaultExpandDepth) {
          next.add(n.path);
          walk(n.children, depth + 1);
        }
      }
    };
    walk(tree, 0);
    setExpanded(next);
  }, [tree, defaultExpandDepth]);
  const toggle = useCallback((path: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  }, []);
  return (
    <div className="text-sm">
      {tree.map((n) => (
        <FileTreeRow
          key={n.path}
          node={n}
          depth={0}
          expanded={expanded}
          toggle={toggle}
          selectedPath={selectedPath}
          onSelect={onSelect}
          onDelete={onDelete}
        />
      ))}
    </div>
  );
}

function FileTreeRow({
  node,
  depth,
  expanded,
  toggle,
  selectedPath,
  onSelect,
  onDelete,
}: {
  node: FileTreeNode;
  depth: number;
  expanded: Set<string>;
  toggle: (p: string) => void;
  selectedPath?: string;
  onSelect: (f: ProducedFile) => void;
  onDelete?: (node: FileTreeNode) => void;
}) {
  const t = useT();
  const pad = { paddingLeft: 8 + depth * 14 };
  const delBtn = onDelete && (
    <button
      onClick={(e) => { e.stopPropagation(); onDelete(node); }}
      className="shrink-0 rounded p-1 text-muted-foreground/60 opacity-0 transition-opacity hover:bg-muted hover:text-destructive group-hover/row:opacity-100"
      title={t("chat.files.delete")}
    >
      <Trash2 className="h-3.5 w-3.5" />
    </button>
  );
  if (node.isDir) {
    const open = expanded.has(node.path);
    return (
      <>
        <div className="group/row flex items-center" style={pad}>
          <button
            onClick={() => toggle(node.path)}
            className="flex min-w-0 flex-1 items-center gap-1.5 rounded-md py-1 pr-1 text-left hover:bg-muted/40"
          >
            {open ? (
              <ChevronDown className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
            ) : (
              <ChevronRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
            )}
            <Folder className="h-4 w-4 shrink-0 text-muted-foreground" />
            <span className="truncate text-foreground">{node.name}</span>
          </button>
          {delBtn}
        </div>
        {open &&
          node.children.map((c) => (
            <FileTreeRow
              key={c.path}
              node={c}
              depth={depth + 1}
              expanded={expanded}
              toggle={toggle}
              selectedPath={selectedPath}
              onSelect={onSelect}
              onDelete={onDelete}
            />
          ))}
      </>
    );
  }
  const { icon: Icon } = fileKind(node.path);
  const active = selectedPath === node.path;
  return (
    <div
      className={`group/row flex items-center rounded-md ${active ? "bg-muted" : "hover:bg-muted/40"}`}
      style={pad}
      title={node.path}
    >
      <button
        onClick={() => onSelect({ path: node.path, size: node.size })}
        className="flex min-w-0 flex-1 items-center gap-1.5 py-1 pr-1 text-left"
      >
        <span className="w-3.5 shrink-0" />
        <Icon className="h-4 w-4 shrink-0 text-muted-foreground" />
        <span className="truncate text-foreground">{node.name}</span>
      </button>
      {delBtn}
    </div>
  );
}

// langForPath maps a file extension to a Shiki language id so the code
// preview can syntax-highlight via the markdown code-fence renderer.
export function langForPath(path: string): string {
  const ext = path.toLowerCase().split(".").pop() || "";
  const map: Record<string, string> = {
    ts: "ts", tsx: "tsx", js: "js", jsx: "jsx", mjs: "js", cjs: "js",
    json: "json", css: "css", scss: "scss", html: "html", htm: "html",
    py: "python", go: "go", rs: "rust", rb: "ruby", java: "java",
    c: "c", cpp: "cpp", h: "c", sh: "bash", bash: "bash", zsh: "bash",
    yaml: "yaml", yml: "yaml", toml: "toml", xml: "xml", sql: "sql",
    md: "markdown", markdown: "markdown",
  };
  return map[ext] || "text";
}
