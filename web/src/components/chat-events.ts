// Shared chat-event reducers extracted from chat-screen.tsx: the
// auth_prompt / error / compaction_notice / skill_pending cases used to
// be implemented twice verbatim (subscribe switch + POST switch) and had
// already drifted (claim timing, metadata handling). These helpers carry
// the RENDER semantics once; the two call sites keep only their own
// seq/claim bookkeeping and pass the event payload in. Each returns the
// message-appends as an array so callers feed them straight to
// setMessages (empty array = nothing to append).

// Shape mirror of ChatScreen's ChatMessage — only the fields these
// reducers set. Structurally checked at the call site.
export interface ChatEventMessage {
  id: string;
  role: "agent" | "notice";
  content: string;
  timestamp: number;
  kind?: string;
  authOptions?: {
    description: string;
    options: { cmd: string; label_zh: string; label_en: string }[];
  };
}

type TFn = (key: string, vars?: Record<string, string | number>) => string;

export function authPromptMessages(
  desc: string,
  options: { cmd: string; label_zh: string; label_en: string }[],
): ChatEventMessage[] {
  return [
    {
      id: `auth-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
      role: "agent",
      content: desc,
      timestamp: Date.now(),
      authOptions: { description: desc, options },
    },
  ];
}

export function errorMessage(message: string): ChatEventMessage[] {
  // context-canceled means the user aborted — not an error worth a pill.
  if (/\bcontext canceled\b/i.test(message)) return [];
  return [
    {
      id: `e-${Date.now()}`,
      role: "notice",
      kind: "turn_error_notice",
      content: message || "Unknown error",
      timestamp: Date.now(),
    },
  ];
}

export function compactionMessage(content: unknown): ChatEventMessage[] {
  if (!content || typeof content !== "string") return [];
  return [
    {
      id: `cn-${Date.now()}`,
      role: "notice",
      kind: "compaction_notice",
      content,
      timestamp: Date.now(),
    },
  ];
}

export function skillPendingMessages(count: number, names: string[], t: TFn): ChatEventMessage[] {
  if (!(count > 0)) return [];
  const content = t("chat.skillPending", { count, names: names.join(", ") });
  // Replace any prior skill_pending pill so consecutive turns don't
  // stack duplicates — callers filter before appending.
  return [
    {
      id: `sp-${Date.now()}`,
      role: "notice",
      kind: "skill_pending",
      content,
      timestamp: Date.now(),
    },
  ];
}
