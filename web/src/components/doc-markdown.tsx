import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import { cn } from "@/lib/utils";

// KB reading surfaces (articles / notes / flash / wiki) share one heading
// ladder: h1 22px → h6 13px, all weight 600, h1 capped at ~1.6× the 14px
// body — hierarchy reads through weight, not size screams (DESIGN.md §3).
// Kept separate from chat-markdown's PROSE_CLASS on purpose: chat is the
// compact 13.5px column, this is the document-density column.
export const DOC_PROSE_CLASS =
  "prose prose-sm dark:prose-invert max-w-none " +
  "prose-headings:font-semibold prose-headings:mt-4 prose-headings:mb-1.5 " +
  "prose-h1:text-[1.375rem] prose-h2:text-lg prose-h3:text-base " +
  "prose-h4:text-[0.9375rem] prose-h5:text-sm prose-h6:text-[0.8125rem]";

// Plain document render — markdown in, styled prose out. Callers can pass
// react-markdown components overrides (the article view demotes 来源
// headings); heavier customization (the wiki's wiki-link anchors) still
// renders its own ReactMarkdown on DOC_PROSE_CLASS.
// muted retints body/headings to muted-foreground (secondary content like
// source excerpts) — the named variant keeps the typography plugin's
// CSS-var mechanism out of call sites.
export function DocMarkdown({
  text,
  className,
  components,
  muted,
}: {
  text: string;
  className?: string;
  components?: Components;
  muted?: boolean;
}) {
  return (
    <div
      className={cn(
        DOC_PROSE_CLASS,
        muted && "[--tw-prose-body:var(--color-muted-foreground)] [--tw-prose-headings:var(--color-muted-foreground)]",
        className,
      )}
    >
      <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]} components={components}>
        {text}
      </ReactMarkdown>
    </div>
  );
}
