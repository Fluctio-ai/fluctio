// Quick-reply options: the ```options fenced-block protocol.
//
// Mirrors internal/agent/replyoptions.go — same caps, same swallow of
// unterminated blocks — so the web bubble, the Go-side IM delivery, and
// history replay all agree on what the block means. Rendering-time split:
// msg.content keeps the raw text (copy/export/replay stay lossless), the
// bubble renders the stripped text plus a tappable button row.
//
// Clicking a button just sends that text through the normal composer
// path (steer when a turn is running, send otherwise) — no new reply
// channel.

export interface ReplyOptionsSplit {
  text: string;
  options: string[];
}

const MAX_COUNT = 4;
const MAX_RUNES = 40;

const CLOSED_BLOCK_RE = /```options[ \t]*\r?\n([\s\S]*?)\r?\n```/g;
const OPEN_BLOCK_RE = /```options[ \t]*\r?\n[\s\S]*/;

export function splitReplyOptions(raw: string): ReplyOptionsSplit {
  if (!raw.includes("```options")) return { text: raw, options: [] };

  const options: string[] = [];
  const seen = new Set<string>();

  let text = raw.replace(CLOSED_BLOCK_RE, (_m, body: string) => {
    try {
      const parsed = JSON.parse(body);
      if (Array.isArray(parsed)) {
        for (const o of parsed) {
          if (typeof o !== "string") continue;
          const v = o.trim();
          if (!v || seen.has(v) || [...v].length > MAX_RUNES) continue;
          seen.add(v);
          options.push(v);
        }
      }
    } catch {
      // Unparseable body: strip, contribute nothing.
    }
    return "";
  });

  // Unterminated opener (mid-stream): swallow so raw JSON never flashes;
  // the closed-block replace above wins once the fence completes.
  text = text.replace(OPEN_BLOCK_RE, "");
  // Collapse the 3+ blank runs a stripped block leaves behind.
  while (text.includes("\n\n\n")) text = text.replaceAll("\n\n\n", "\n\n");
  text = text.trim();

  return { text, options: options.slice(0, MAX_COUNT) };
}
