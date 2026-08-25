"use client";

// MarkdownCodeMirror — the notes body editor: the same IDE-grade CodeMirror
// surface the workflow YAML tab uses, tuned for prose instead of config:
// markdown syntax highlighting, line wrapping on, line numbers off. All
// CodeMirror imports live here so the wrapper (notes-view) can lazy-load
// the whole chunk via next/dynamic — the notes list never pays for it
// (Whiteboard precedent).
//
// Theming deliberately avoids @uiw's built-in "light"/"dark" strings: the
// light default ships CodeMirror's own white-gutter look (#f5f5f5 gutters,
// pale-blue active line, grey selection) which glared in night mode. Every
// surface below rides the app's CSS variables instead, so the editor blends
// into both themes — and foldGutter/lineNumbers stay off so no gutter
// column ever appears on the left.

import * as React from "react";
import CodeMirror from "@uiw/react-codemirror";
import { EditorView } from "@codemirror/view";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { tags as t } from "@lezer/highlight";
import { markdown } from "@codemirror/lang-markdown";
import { useTheme } from "@/components/theme-provider";

// Token-tinted markdown highlighting: heading/strong carry the foreground
// weight, links/inline-code borrow the accent, fences and quotes mute —
// same visual language as the rendered preview, no matter the theme.
const mdHighlight = HighlightStyle.define([
  { tag: t.heading, color: "var(--foreground)", fontWeight: "600" },
  { tag: t.strong, color: "var(--foreground)", fontWeight: "600" },
  { tag: t.emphasis, color: "var(--muted-foreground)", fontStyle: "italic" },
  { tag: [t.link, t.url], color: "var(--primary)", textDecoration: "underline" },
  { tag: t.monospace, color: "var(--primary)", backgroundColor: "var(--muted)", borderRadius: "3px" },
  { tag: [t.processingInstruction, t.meta, t.quote], color: "var(--muted-foreground)" },
]);

export function MarkdownCodeMirror({
  value,
  onChange,
  placeholder,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
}) {
  const { resolvedTheme } = useTheme();
  const dark = resolvedTheme === "dark";
  // Rebuilt per theme flip so CodeMirror's hasDarkTheme flag stays true in
  // night mode (drawSelection/caret defaults key off it).
  const themeExt = React.useMemo(
    () => [
      EditorView.theme(
        {
          "&": { height: "100%", fontSize: "13.5px", color: "var(--foreground)" },
          ".cm-scroller": { fontFamily: "var(--font-mono)" },
          ".cm-content": { padding: "16px", caretColor: "var(--foreground)" },
          ".cm-line": { lineHeight: "1.625" },
          ".cm-activeLine": { backgroundColor: "color-mix(in oklab, var(--muted-foreground) 10%, transparent)" },
          "&.cm-focused .cm-selectionBackground, .cm-selectionBackground": {
            backgroundColor: "color-mix(in oklab, var(--primary) 22%, transparent)",
          },
          ".cm-cursor": { borderLeftColor: "var(--foreground)", borderLeftWidth: "2px" },
        },
        { dark },
      ),
      syntaxHighlighting(mdHighlight),
    ],
    [dark],
  );
  return (
    <CodeMirror
      value={value}
      onChange={onChange}
      placeholder={placeholder}
      // The wrapper div needs an explicit height — @uiw's height prop only
      // themes the inner .cm-editor, so without this the wrapper stretches
      // to document height and blows past the flex-constrained parent.
      style={{ height: "100%" }}
      height="100%"
      theme="none"
      extensions={[markdown(), EditorView.lineWrapping, ...themeExt]}
      basicSetup={{ lineNumbers: false, foldGutter: false, highlightActiveLineGutter: false }}
    />
  );
}
