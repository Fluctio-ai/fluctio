"use client";

import {
  useMemo,
  type ComponentProps,
  type MouseEvent as ReactMouseEvent,
  type WheelEvent as ReactWheelEvent,
} from "react";
import { Streamdown, defaultUrlTransform, type Components, type UrlTransform } from "streamdown";
import { createCodePlugin } from "@streamdown/code";
import { mermaid } from "@streamdown/mermaid";
import { math } from "@streamdown/math";
import { cjk } from "@streamdown/cjk";
import remarkBreaks from "remark-breaks";
import { fileUrl, type KnowledgeSource } from "@/lib/api";
import { ExternalAnchor } from "@/components/markdown-link";

// Streamdown 2.x splits rendering features into opt-in plugins. Without these,
// fenced code lands as an unstyled <pre> (no highlight), ```mermaid stays as
// text, $math$ doesn't resolve, and long CJK runs break awkwardly. Shiki theme
// stays on github-light / github-dark; the surrounding `dark:` context picks
// the side. Ported from fleet's MarkdownText.
const code = createCodePlugin({ themes: ["github-light", "github-dark"] });

// remark-breaks turns a single newline into <br>, which chat messages rely on
// (IM-style line breaks). It MUST run AFTER remarkGfm: run it before and it
// rewrites the newlines between table rows into <br>, so remarkGfm never sees a
// table and every table silently degrades to plain text. Streamdown runs the
// top-level `remarkPlugins` prop BEFORE gfm, so we inject it into the cjk
// plugin's `remarkPluginsAfter` slot, which runs post-gfm. (Verified end-to-end:
// via the prop → no <table>; via remarkPluginsAfter → <table> + <br> both render.)
const cjkWithBreaks = { ...cjk, remarkPluginsAfter: [...cjk.remarkPluginsAfter, remarkBreaks] };
const streamdownPlugins = { code, mermaid, math, cjk: cjkWithBreaks };

// knowledgeSourceLabel renders the tooltip for a [K#] citation badge: the
// origin ("Wiki"/"知识库"), the wiki page type when applicable (来源/概念/实体/总览),
// and the source title. The chunk index is omitted — it's a retrieval
// artifact, not meaningful to users.
function knowledgeSourceLabel(source: KnowledgeSource): string {
  const parts: string[] = [source.kind === "wiki" ? "Wiki" : "知识库"];
  if (source.kind === "wiki" && source.pageType) {
    const typeMap: Record<string, string> = {
      source: "来源",
      concept: "概念",
      entity: "实体",
      query: "总览",
    };
    if (typeMap[source.pageType]) parts.push(typeMap[source.pageType]);
  }
  parts.push(source.file);
  return parts.join(" · ");
}

// Prose typography tuned for chat density (heading sizes, tight spacing),
// mirroring the former CHAT_PROSE_CLASS. The bulky overrides that flatten
// Streamdown's card chrome live in globals.css under the `.chat-md` class.
const PROSE_CLASS =
  "chat-md text-[13.5px] leading-normal prose prose-sm max-w-none dark:prose-invert min-w-0 wrap-anywhere " +
  "prose-p:my-1.5 " +
  // Tighter, shallower lists: smaller indent (pl-5 ≈ 20px vs prose's ~26px),
  // less gap between the marker and text, and snug item spacing.
  "prose-ul:my-1.5 prose-ol:my-1.5 prose-ul:pl-4 prose-ol:pl-4 " +
  "prose-li:my-0.5 prose-li:pl-0 prose-li:marker:text-muted-foreground/60 " +
  "prose-headings:font-semibold prose-headings:mt-2.5 prose-headings:mb-1 " +
  "prose-h1:text-[15px] prose-h2:text-[14px] prose-h3:text-[13.5px] prose-h4:text-[13.5px] prose-h5:text-[13.5px] prose-h6:text-[13.5px] " +
  "prose-blockquote:border-l-primary/60 prose-blockquote:bg-muted/20 prose-blockquote:px-3 prose-blockquote:not-italic " +
  "prose-a:text-primary prose-a:underline-offset-2 hover:prose-a:opacity-80 " +
  "prose-table:my-2 prose-table:text-[13px] prose-th:bg-muted/40 prose-th:font-medium prose-th:border-border prose-td:border-border " +
  "prose-th:py-1 prose-th:px-2 prose-td:py-1 prose-td:px-2 prose-td:leading-snug " +
  "prose-hr:my-3";

/**
 * ChatMarkdown is the single markdown rendering primitive for chat bubbles and
 * file previews. It wraps Streamdown (a streaming-aware superset of
 * react-markdown) so chat content gains Shiki code highlighting, KaTeX math,
 * Mermaid diagrams, and CJK-aware line breaking.
 *
 * Pass `agentId` (+ `sessionId`) for agent chat bubbles so the sandbox
 * `/workspace/<name>` paths the model emits resolve to the authenticated file
 * API; omit them for file previews / the standalone chat page where there's no
 * workspace to map.
 */
export function ChatMarkdown({
  text,
  agentId,
  sessionId,
  baseDir,
  bareCode = false,
  knowledgeSources,
  onKnowledgeCitationClick,
}: {
  text: string;
  agentId?: string;
  sessionId?: string;
  // Workspace directory (relative to the agent root, e.g.
  // "sessions/<sid>" or "sessions/<sid>/sub") that relative URLs in the
  // markdown should resolve against. Used by the file previewer where an
  // .md references sibling images/links by bare relative path — without
  // this the browser resolves them against the page URL and they 404.
  // Omitted in chat-bubble context (model emits /workspace/... paths).
  baseDir?: string;
  // File-viewer mode: hide the floating copy pill on code blocks (the .chat-md
  // strip already removes the card) so a source file reads as plain code.
  bareCode?: boolean;
  // [K#] citation sources attached to the assistant message; the renderer
  // turns [K1]/[K2]… markers in the text into clickable badges.
  knowledgeSources?: KnowledgeSource[];
  onKnowledgeCitationClick?: (source: KnowledgeSource) => void;
}) {
  // Build the URL transform once per agent/session. A stable identity keeps
  // Streamdown (a memo component) from re-rendering on every streamed keystroke,
  // which a fresh inline function each render would defeat.
  const urlTransform = useMemo<UrlTransform>(() => {
    return (url, key, node) => {
      // Inline base64 images pass through (the default transform strips data:).
      if (key === "src" && url.startsWith("data:image/")) return url;
      // Remap sandbox `/workspace/<name>` (image src + link href) to the
      // authenticated file API. The docker bind-mount is session-scoped, so
      // prepend sessions/<sid>/ or the file API resolves against the agent root
      // and 404s.
      if (agentId && (key === "src" || key === "href") && url.startsWith("/workspace/")) {
        const rel = url.slice("/workspace/".length);
        return fileUrl(agentId, sessionId ? `sessions/${sessionId}/${rel}` : rel);
      }
      // NOTE: bare relative URLs (e.g. "cover.png") CANNOT be handled here.
      // Streamdown runs rehype-harden BEFORE urlTransform, and harden's
      // parseUrl returns null for a scheme-less, non-root path (no origin
      // to resolve against), so the image is already replaced by the
      // "[Image blocked]" indicator before this transform runs. Such URLs
      // are pre-resolved in processedText below instead.
      return defaultUrlTransform(url, key, node);
    };
  }, [agentId, sessionId]);

  // Pre-resolve bare relative image/link URLs in the markdown SOURCE so
  // they become root-absolute /api/... paths before Streamdown parses and
  // harden scrubs them. harden accepts root-relative URLs (its dummy-base
  // parse path yields http: → wildcard), but blocks bare relative ones
  // ("cover.png" → parseUrl null → "[Image blocked]"). Only in file-preview
  // context (baseDir supplied).
  const processedText = useMemo(() => {
    if (!agentId || baseDir === undefined) return text;
    return text.replace(/(!?\[[^\]]*\]\()([^)\s]+)(\))/g, (m, prefix, url, suffix) => {
      if (
        /^[a-z][a-z0-9+.-]*:/i.test(url) ||
        url.startsWith("/") ||
        url.startsWith("#") ||
        url.startsWith("data:")
      ) {
        return m;
      }
      let rel = url;
      if (rel.startsWith("./")) rel = rel.slice(2);
      const fullPath = baseDir ? `${baseDir}/${rel}` : rel;
      return prefix + fileUrl(agentId, fullPath) + suffix;
    });
  }, [text, agentId, baseDir]);

  const knowledgeByID = useMemo(() => {
    const map = new Map<string, KnowledgeSource>();
    for (const s of knowledgeSources ?? []) {
      if (s.id) map.set(s.id, s);
    }
    return map;
  }, [knowledgeSources]);

  // Turn [K1]/[K2]… markers into markdown links so the custom `a` renderer
  // below can surface them as clickable citation badges. Unknown ids stay as
  // literal text so a stale [K#] (e.g. from an older turn) degrades cleanly.
  const renderedText = useMemo(() => {
    if (knowledgeByID.size === 0) return processedText;
    return processedText.replace(/\[(K\d+)\]/g, (match, id: string) => {
      if (!knowledgeByID.has(id)) return match;
      return `[${id}](#knowledge-${id})`;
    });
  }, [processedText, knowledgeByID]);

  // components depends on knowledgeSources (badge rendering), so build it per
  // render instead of as a module constant. The `a` renderer intercepts
  // #knowledge- links → citation badge; everything else defers to
  // ExternalAnchor (cross-origin target="_blank").
  const components = useMemo<Components>(() => ({
    a: ({ node: _node, ...props }: ComponentProps<"a"> & { node?: unknown }) => {
      void _node;
      const href = typeof props.href === "string" ? props.href : "";
      if (href.startsWith("#knowledge-")) {
        const id = href.slice("#knowledge-".length);
        const source = knowledgeByID.get(id);
        return (
          <button
            type="button"
            className="rounded bg-primary/10 px-1 font-medium text-primary hover:bg-primary/15"
            title={source ? knowledgeSourceLabel(source) : id}
            onClick={(e) => {
              e.preventDefault();
              if (source) onKnowledgeCitationClick?.(source);
            }}
          >
            {props.children}
          </button>
        );
      }
      return <ExternalAnchor {...props} />;
    },
  }), [knowledgeByID, onKnowledgeCitationClick]);

  // Click anywhere on a mermaid diagram → fullscreen. Streamdown renders a
  // hidden fullscreen toggle inside the block; we delegate the click to it.
  function onMermaidClick(e: ReactMouseEvent<HTMLDivElement>) {
    const target = e.target as HTMLElement;
    if (target.closest("button, a")) return;
    target
      .closest<HTMLElement>("[data-streamdown=mermaid-block]")
      ?.querySelector<HTMLButtonElement>('button[title*="ull" i], button[aria-label*="ull" i]')
      ?.click();
  }

  // mermaid.js attaches a {passive:false} wheel listener that preventDefaults
  // and would swallow chat scrolling. Catch the wheel in CAPTURE phase and
  // stopPropagation so mermaid's handler never sees it.
  function onWheelCapture(e: ReactWheelEvent<HTMLDivElement>) {
    if ((e.target as HTMLElement).closest("[data-streamdown=mermaid-block]")) {
      e.stopPropagation();
    }
  }

  return (
    <div className={bareCode ? PROSE_CLASS + " chat-md-bare" : PROSE_CLASS} onClick={onMermaidClick} onWheelCapture={onWheelCapture}>
      <Streamdown
        parseIncompleteMarkdown
        plugins={streamdownPlugins}
        urlTransform={urlTransform}
        components={components}
        controls={{
          table: true,
          code: true,
          // Minimal inline mermaid: no pan/zoom (intercepts wheel, blocks chat
          // scroll), no copy/download clutter. Keep fullscreen — clicking the
          // block triggers it (onMermaidClick); the modal re-enables pan/zoom.
          mermaid: { panZoom: false, copy: false, download: false, fullscreen: true },
        }}
      >
        {renderedText}
      </Streamdown>
    </div>
  );
}
