"use client";

// Whiteboard lazily mounts react-excalidraw — the ~700KB chunk (plus font
// subsets) only loads the first time a note's 白板 tab opens, so the notes
// list, chat route, and the rest of the app never pay for it. Data is the
// Excalidraw scene JSON stored on kb_notes.whiteboard; saves are debounced
// upstream by the note editor.

import * as React from "react";
import dynamic from "next/dynamic";
import { PenLine } from "lucide-react";
import { useLocale, useT } from "@/lib/i18n";

// Point Excalidraw at the self-hosted font assets (public/excalidraw/)
// BEFORE its chunk evaluates, so whiteboards render offline instead of
// reaching for the jsdelivr CDN. Must be set at module scope — this
// module loads with the notes page, the canvas loads on demand. (Note:
// the directory deliberately isn't public/vendor/ — the root .gitignore
// ignores any vendor/ dir, which would silently exclude the assets.)
declare global {
  interface Window {
    EXCALIDRAW_ASSET_PATH?: string;
  }
}
if (typeof window !== "undefined" && window.EXCALIDRAW_ASSET_PATH === undefined) {
  window.EXCALIDRAW_ASSET_PATH = "/excalidraw/";
}

// The excalidraw prebuilt ESM derives its font-subsetting worker URL from
// import.meta.url, which Turbopack compiles into a broken
// `file:///ROOT/node_modules/...` placeholder — the Worker constructor
// then throws a SecurityError and the lib falls back to main-thread
// subsetting, printing a scary console.error. There's no supported
// override, so rewrite the URL at the Worker constructor: when a worker
// script URL points at the (nonexistent) bundled subset-worker, serve it
// from our self-hosted copy in public/excalidraw/ instead. Only that one
// URL is rewritten — every other Worker passes through untouched.
if (typeof window !== "undefined" && !("fluctioExcalidrawWorkerPatched" in window)) {
  (window as unknown as Record<string, unknown>).fluctioExcalidrawWorkerPatched = true;
  const OrigWorker = window.Worker;
  class PatchedWorker extends OrigWorker {
    constructor(scriptUrl: string | URL, options?: WorkerOptions) {
      if (String(scriptUrl).includes("subset-worker")) {
        super(new URL("/excalidraw/subset-worker.chunk.js", window.location.origin), options);
        return;
      }
      super(scriptUrl, options);
    }
  }
  window.Worker = PatchedWorker as typeof Worker;
}

// The Excalidraw chunk loads on demand. Its stylesheet can't ride the
// bundler (the package's exports map gives ./index.css no "default"
// condition, so Turbopack fails to resolve it) — it's self-hosted at
// /vendor/excalidraw/index.css next to the fonts and injected as a <link>
// by injectStylesheet() right before the canvas mounts, so the CSS stays
// on the same lazy path as the JS.
const Excalidraw = dynamic(
  () => import("@excalidraw/excalidraw").then((m) => m.Excalidraw),
  {
    ssr: false,
    loading: () => <WhiteboardLoading />,
  },
);

let stylesheetInjected: Promise<void> | null = null;
function injectStylesheet(): Promise<void> {
  if (!stylesheetInjected) {
    stylesheetInjected = new Promise((resolve) => {
      if (document.getElementById("excalidraw-css")) return resolve();
      const link = document.createElement("link");
      link.id = "excalidraw-css";
      link.rel = "stylesheet";
      link.href = "/excalidraw/index.css";
      link.onload = () => resolve();
      link.onerror = () => resolve(); // unstyled board beats a blank one
      document.head.appendChild(link);
      // Browsers sometimes skip onload for cached stylesheets — resolve
      // anyway after a grace tick so the canvas never hangs on CSS.
      setTimeout(resolve, 1200);
    });
  }
  return stylesheetInjected;
}

function WhiteboardLoading() {
  const t = useT();
  return (
    <div className="flex h-full min-h-[420px] items-center justify-center rounded-lg border bg-muted/30 text-xs text-muted-foreground">
      <PenLine className="mr-2 size-4 animate-pulse" />
      <span>{t("knowledge.notes.wbLoading")}</span>
    </div>
  );
}

// parseScene tolerates garbage / partial JSON (a save interrupted
// mid-write, a hand-edited cell) by starting from an empty board.
function parseScene(raw: string): { elements: unknown[]; files?: Record<string, { dataURL: string; mimeType: string }> } | null {
  if (!raw) return null;
  try {
    const v = JSON.parse(raw);
    if (Array.isArray(v?.elements)) {
      return { elements: v.elements, files: v.files ?? undefined };
    }
  } catch {
    /* fall through to null */
  }
  return null;
}

export function Whiteboard({
  data,
  dark,
  onChange,
}: {
  data: string;
  dark: boolean;
  onChange: (sceneJSON: string) => void;
}) {
  const { locale } = useLocale();
  const [cssReady, setCssReady] = React.useState(false);
  React.useEffect(() => {
    let alive = true;
    injectStylesheet().then(() => { if (alive) setCssReady(true); });
    return () => { alive = false; };
  }, []);
  // Parse ONCE on mount — re-parsing on every render would reset the
  // canvas. The component stays uncontrolled after that; saves flow out
  // through onChange only.
  const [initial] = React.useState(() => parseScene(data));
  // Excalidraw fires onChange on every scene mutation (each drag frame),
  // so serialize on a trailing 500ms debounce — the parent editor only
  // needs the settled scene, and a stringify per frame would re-render
  // the whole note editor at mouse-move frequency.
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  React.useEffect(() => () => { if (timer.current) clearTimeout(timer.current); }, []);
  const onChangeRef = React.useRef(onChange);
  onChangeRef.current = onChange;

  return (
    // h-full fills the dialog's flex-1 area; the min-height keeps a
    // collapsed container from rendering a zero-height canvas.
    <div className="h-full min-h-[400px] overflow-hidden rounded-lg border">
      {cssReady ? (
        <Excalidraw
          initialData={{ elements: initial?.elements as never, files: initial?.files as never }}
          langCode={locale === "zh-CN" ? "zh-CN" : "en"}
          theme={dark ? "dark" : "light"}
          onChange={(elements, _appState, files) => {
            if (timer.current) clearTimeout(timer.current);
            timer.current = setTimeout(() => {
              // Keep only uploaded/pasted binary files (dataURL) so the
              // scene survives a reload; skip the in-flight placeholder
              // entries Excalidraw tracks while an upload resolves.
              const outFiles: Record<string, { dataURL: string; mimeType: string }> = {};
              for (const [id, f] of Object.entries(files ?? {})) {
                const dataURL = (f as { dataURL?: string })?.dataURL;
                const mimeType = (f as { mimeType?: string })?.mimeType;
                if (dataURL && mimeType) outFiles[id] = { dataURL, mimeType };
              }
              onChangeRef.current(
                JSON.stringify({ type: "excalidraw", version: 2, elements, files: outFiles }),
              );
            }, 500);
          }}
        />
      ) : (
        <WhiteboardLoading />
      )}
    </div>
  );
}
