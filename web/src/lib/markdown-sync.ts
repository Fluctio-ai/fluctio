// Markdown preview anchor stamping (editor→preview scroll sync).
// Streamdown parses each markdown block as an independent parse run, so no
// plugin can recover global source lines (every block's AST position starts
// at 1). Instead we stamp AFTER render: match each rendered top-level
// element to the first source line whose marker-stripped text it renders.
// A two-pointer scan in source order; elements that match nothing (hr,
// image-only paragraphs, math/mermaid chrome) simply carry no anchor and
// sync falls through to the previous stamped block.

// Block prefixes that vanish in rendered text: ATX heading, bullet/ordered
// list marker (incl. task checkbox), quote marker, table row pipes.
export const BLOCK_PREFIX_RE = /^(?:#{1,6}\s+|[-*+]\s+(?:\[[ xX]\]\s*)?|\d+[.)]\s+|>\s*|\|)/;

// Inline markup that survives to the DOM only as rendered text: links/
// images keep their label, emphasis/code-span marks drop out.
export function stripMdMarkers(line: string): string {
  let s = line
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/[*_~`]+/g, "")
    .trim();
  let prev = "";
  while (s !== prev) {
    prev = s;
    s = s.replace(BLOCK_PREFIX_RE, "");
  }
  return s;
}

// Alphanumeric runs only (CJK counts): whitespace/punctuation differences
// between source and render can't break the prefix compare.
const wordChars = (s: string) => s.match(/[\p{L}\p{N}]+/gu)?.join("") ?? "";

export function stampRunAnchors(run: HTMLElement, text: string, startLine: number) {
  // Streamdown wraps all blocks in one container div (.space-y-4); the
  // block elements are that wrapper's children. Unwrap one level when we
  // see it (sole child with multiple children); otherwise stamp run's own
  // children — a lone block then lands on its wrapper, still positionally
  // correct.
  let host: HTMLElement = run;
  if (run.children.length === 1) {
    const only = run.children[0];
    if (only instanceof HTMLElement && only.children.length > 1) host = only;
  }
  for (const el of host.children) if (el instanceof HTMLElement) delete el.dataset.line;
  const lines = text.split("\n");
  let cursor = 0;
  for (const el of host.children) {
    if (!(el instanceof HTMLElement)) continue;
    // Code blocks: the <code> body is the verbatim source (the fence line
    // has no word chars, so the first code line matches). Everything else:
    // leading text is enough — lists/quotes render their items in order.
    const src = el.querySelector("code")?.textContent ?? el.textContent ?? "";
    const want = wordChars(src);
    if (!want) continue;
    for (; cursor < lines.length; cursor++) {
      const have = wordChars(stripMdMarkers(lines[cursor]));
      if (have && want.startsWith(have)) {
        el.dataset.line = String(startLine + cursor);
        cursor++;
        break;
      }
    }
  }
}
