// Pure content-transform helpers extracted from chat-screen.tsx: the
// regex-heavy markdown scans (data:/http image splitting, quick-reply
// block splitting) and the wire token for multi-bubble replies. Pure
// functions + bounded caches, no React imports — so chat-screen, the
// RegularBubble memo component, and any future consumer share one
// implementation instead of three drifted copies.

// Wire token the agent emits to request a multi-bubble reply — must
// match channels.SplitMessageMarker in internal/channels/base.go. On
// IM channels the dispatcher (manager.dispatchOutbound) splits the
// outbound text on this marker into separate platform messages; the
// web UI renders one bubble per split chunk so the experience matches.
export const SPLIT_MARKER = "<|split|>";

export function splitOnMarker(s: string): string[] {
  if (!s.includes(SPLIT_MARKER)) return [s];
  const parts = s.split(SPLIT_MARKER).map((p) => p.trim()).filter((p) => p.length > 0);
  return parts.length > 0 ? parts : [s];
}

export type DataImagePart =
  | { type: "text"; text: string }
  | { type: "image"; alt: string; src: string };

export function splitDataImages(s: string): DataImagePart[] {
  const out: DataImagePart[] = [];
  // Match markdown image tags whose destination is either an inline
  // data:base64 payload or a remote http(s) URL. data: is emitted by
  // providers like gpt-image-1; http(s) URLs come from fal/replicate and
  // from image_gen tool output (`![image 1](https://...)`). Both need to
  // surface as native <img> — a remote URL is exactly what image_gen
  // returns, so without this the generated image never renders inline.
  const headerRe = /!\[([^\]]*?)\]\s*\(\s*<?\s*(data:image\/[a-z0-9.+-]+;base64,|https?:\/\/)/gi;
  let lastIdx = 0;
  let m: RegExpExecArray | null;
  while ((m = headerRe.exec(s)) !== null) {
    const alt = m[1];
    const prefix = m[2];
    const bodyStart = m.index + m[0].length;
    let src: string;
    let after: number;
    if (prefix.startsWith("data:")) {
      // Consume base64 body: A–Z a–z 0–9 + / = and any whitespace.
      let cursor = bodyStart;
      while (cursor < s.length && /[A-Za-z0-9+/=\s]/.test(s[cursor])) cursor++;
      const body = s.slice(bodyStart, cursor).replace(/\s+/g, "");
      if (!body) continue;
      src = prefix + body;
      // Step past optional `>` and `)` closers (or accept truncation).
      after = cursor;
      while (after < s.length && /[\s>]/.test(s[after])) after++;
      if (s[after] === ")") after++;
    } else {
      // Remote URL — consume until the closing `)` (URLs may carry query
      // chars ?&#=, so scan by delimiter rather than an allowlist).
      let cursor = bodyStart;
      while (cursor < s.length && s[cursor] !== ")") cursor++;
      src = prefix + s.slice(bodyStart, cursor);
      after = cursor < s.length ? cursor + 1 : cursor;
    }

    if (m.index > lastIdx) out.push({ type: "text", text: s.slice(lastIdx, m.index) });
    out.push({ type: "image", alt, src });
    lastIdx = after;
    headerRe.lastIndex = after;
  }
  if (lastIdx < s.length) out.push({ type: "text", text: s.slice(lastIdx) });
  return out;
}

// splitDataImagesCache memoizes the regex-heavy image scan across renders.
// The render IIFE re-runs on every state change (each streamed flush, each
// keystroke) but message content only changes for the message actually
// being streamed — caching by input string makes every unchanged bubble's
// rescan free (contents with data: payloads can run to hundreds of KB).
// Bounded with a wholesale clear to cap memory: entries are small arrays
// of string slices referencing the cached input.
const splitDataImagesCache = new Map<string, DataImagePart[]>();
const splitDataImagesCacheLimit = 500;

export function splitDataImagesCached(s: string): DataImagePart[] {
  let out = splitDataImagesCache.get(s);
  if (out === undefined) {
    out = splitDataImages(s);
    if (splitDataImagesCache.size >= splitDataImagesCacheLimit) splitDataImagesCache.clear();
    splitDataImagesCache.set(s, out);
  }
  return out;
}

// splitReplyOptionsCache mirrors splitDataImagesCache for the imported
// quick-reply splitter: the render path calls it per agent bubble per
// render, and the regex scan over unchanged content is pure waste.
const splitReplyOptionsCache = new Map<string, ReturnType<typeof import("@/lib/reply-options").splitReplyOptions>>();

export function splitReplyOptionsCached(
  s: string,
  splitReplyOptions: (s: string) => ReturnType<typeof import("@/lib/reply-options").splitReplyOptions>,
): ReturnType<typeof import("@/lib/reply-options").splitReplyOptions> {
  let out = splitReplyOptionsCache.get(s);
  if (out === undefined) {
    out = splitReplyOptions(s);
    if (splitReplyOptionsCache.size >= 500) splitReplyOptionsCache.clear();
    splitReplyOptionsCache.set(s, out);
  }
  return out;
}
