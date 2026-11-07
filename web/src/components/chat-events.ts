// Shared chat-event reducers extracted from chat-screen.tsx: the
// auth_prompt / error / compaction_notice / skill_pending cases used to
// be implemented twice verbatim (subscribe switch + POST switch) and had
// already drifted (claim timing, metadata handling). These helpers carry
// the RENDER semantics once; the two call sites keep only their own
// seq/claim bookkeeping and pass the event payload in. Each returns the
// message-appends as an array so callers feed them straight to
// setMessages (empty array = nothing to append).
//
// The second half of this module holds the turn-state transforms: pure
// (prev, ...) => next message-list functions for the streaming switch
// (delta flush, content seal, tool-group upsert, abort fill, failure
// bubble). They own the message-shape semantics; chat-screen keeps only
// refs, timers, and dispatch.

import type { ToolResultMetadata } from "@/lib/api";

export interface UserAttachment {
  name: string;
  isImage: boolean;
  // Local blob URL for instant in-bubble preview without a server round-trip.
  // Only set on the live-send turn; reloaded history won't carry it.
  previewUrl?: string;
}

export interface ToolCallRecord {
  id: string;
  name: string;
  arguments: string;
  result?: string;
  metadata?: ToolResultMetadata;
}

// ChatMessage — the canonical chat-bubble shape (moved here from
// chat-screen.tsx so the transforms below can be typed against it
// without a cycle).
export interface ChatMessage {
  id: string;
  role: "user" | "agent" | "tool-group" | "notice";
  content: string;
  // For role==="notice" — currently "compaction_notice" and
  // "skill_pending" are the kinds. Set so the renderer can distinguish
  // future notice variants.
  kind?: string;
  timestamp: number;
  // session_messages.seq (0-based) from WebChatHistory — daily-diary
  // #seq-N deep links scroll to the bubble whose id matches.
  seq?: number;
  toolCalls?: ToolCallRecord[];
  files?: { path: string; size?: number; modTime?: number }[];
  attachments?: UserAttachment[];
  // Optimistically-rendered steer bubble awaiting the server's persisted
  // "steer" echo. Used only to dedup against that echo (cleared on
  // match) — not rendered differently.
  pendingSteer?: boolean;
  // Assistant-side metadata (e.g. iteration-cap badge). Stamped from
  // either the live content event's `metadata` payload or the
  // ChatHistoryMessage.metadata on a refresh.
  metadata?: ToolResultMetadata;
  // Authorization prompt payload (from auth_prompt SSE event). When set,
  // the agent bubble renders tappable buttons for each option instead of
  // asking the user to type the slash. handleSend(opt.cmd) dispatches.
  authOptions?: {
    description: string;
    options: { cmd: string; label_zh: string; label_en: string }[];
  };
  // IM-bridge sender identity, surfaced from session_messages metadata
  // (set by the agent loop for Discord/Telegram/... routed turns).
  // Present means: render an avatar + nickname header instead of an
  // anonymous "you" bubble — the message came from a third party
  // talking to the bot, not from the agent owner themselves.
  sender?: {
    name: string;
    avatarUrl?: string;
    id?: string;
    channel?: string;
  };
}

// Shape mirror of the notice builders' output — a subset of ChatMessage.
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

// ── Turn-state transforms ─────────────────────────────────────────────
// Pure (prev, ...) => next list functions for the live-stream switch in
// chat-screen's handleSend. Each one owns a piece of message-shape
// semantics so the streaming closure keeps only refs, timers, dispatch.

// Resend of a trailing failed user turn: the server is rolling back the
// orphan user row, so drop the stale failed user bubble AND everything
// after it (trailing error bubbles) before the fresh user bubble lands.
export function trimForRollback(prev: ChatMessage[], rollbackId: string | null): ChatMessage[] {
  if (!rollbackId) return prev;
  const idx = prev.findIndex((m) => m.id === rollbackId);
  return idx >= 0 ? prev.slice(0, idx) : prev;
}

export function appendUserTurn(
  prev: ChatMessage[],
  rollbackId: string | null,
  text: string,
  attachments: UserAttachment[],
): ChatMessage[] {
  const base = trimForRollback(prev, rollbackId);
  return [
    ...base,
    {
      id: `u-${Date.now()}`,
      role: "user",
      content: text, // bubble shows text only; attachments rendered separately above
      timestamp: Date.now(),
      attachments: attachments.length > 0 ? attachments : undefined,
    },
  ];
}

// Delta-flush append: accrete the batched chunk onto the in-flight
// streaming bubble. Caller creates the bubble on first flush.
export function appendDelta(prev: ChatMessage[], id: string, chunk: string): ChatMessage[] {
  const idx = prev.findIndex((m) => m.id === id);
  if (idx < 0) return prev;
  const updated = [...prev];
  updated[idx] = { ...updated[idx], content: (updated[idx].content || "") + chunk };
  return updated;
}

export function newAgentBubbleId(): string {
  return `a-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
}

// Final `content` seal for a bubble that was streamed in via deltas:
// drop it entirely if it only collected whitespace (phantom blank),
// otherwise merge any metadata. Nothing to do when metadata is absent.
export function sealStreamedBubble(prev: ChatMessage[], id: string, meta?: ToolResultMetadata): ChatMessage[] {
  const idx = prev.findIndex((m) => m.id === id);
  if (idx < 0) return prev;
  if (!((prev[idx].content || "").trim())) {
    return prev.filter((m) => m.id !== id);
  }
  if (!meta) return prev;
  const updated = [...prev];
  updated[idx] = { ...updated[idx], metadata: { ...updated[idx].metadata, ...meta } };
  return updated;
}

// Some providers (longcat) emit a whitespace-only delta right before a
// tool_call, leaving a streaming bubble whose content is only
// whitespace; the tool-group is appended separately, so that empty
// bubble would otherwise linger as a phantom blank.
export function dropBubbleIfWhitespace(prev: ChatMessage[], id: string): ChatMessage[] {
  const idx = prev.findIndex((m) => m.id === id);
  if (idx < 0) return prev;
  if ((prev[idx].content || "").trim()) return prev;
  return prev.filter((m) => m.id !== id);
}

// Metadata-only `content` event (empty content, meta set): retro-stamp
// the LAST agent message — the backend uses this for the streaming
// forced-final-delivery path where chunks flow through a separate
// channel and the metadata follows after.
export function stampLastAgentMeta(prev: ChatMessage[], meta: ToolResultMetadata): ChatMessage[] {
  for (let i = prev.length - 1; i >= 0; i--) {
    if (prev[i].role === "agent") {
      const updated = [...prev];
      updated[i] = { ...updated[i], metadata: { ...updated[i].metadata, ...meta } };
      return updated;
    }
  }
  return prev;
}

// tool_call: update the group for this round when it exists (an
// additional call within the same assistant turn), otherwise append the
// group as its own message.
export function upsertToolGroup(prev: ChatMessage[], groupId: string, calls: ToolCallRecord[]): ChatMessage[] {
  const idx = prev.findIndex((m) => m.id === groupId);
  if (idx >= 0) {
    const updated = [...prev];
    updated[idx] = { ...updated[idx], toolCalls: calls };
    return updated;
  }
  return [
    ...prev,
    { id: groupId, role: "tool-group" as const, content: "", timestamp: Date.now(), toolCalls: calls },
  ];
}

// tool_result: refresh the group's call snapshots. Group may already be
// gone (dropped on session switch) — no-op then.
export function patchToolGroup(prev: ChatMessage[], groupId: string, calls: ToolCallRecord[]): ChatMessage[] {
  const idx = prev.findIndex((m) => m.id === groupId);
  if (idx < 0) return prev;
  const updated = [...prev];
  updated[idx] = { ...updated[idx], toolCalls: calls };
  return updated;
}

// skill_pending pill replaces any prior one so consecutive turns don't
// stack duplicates.
export function replaceSkillPills(prev: ChatMessage[], pills: ChatEventMessage[]): ChatMessage[] {
  if (pills.length === 0) return prev;
  return [...prev.filter((m) => m.kind !== "skill_pending"), ...pills];
}

// User pressed Stop: resolve every still-pending call in every visible
// tool-group so they stop spinning. Server-side padOrphanToolResults
// writes a matching record on its end; this keeps the UI consistent
// until the next history fetch overwrites it.
export function markStoppedTools(prev: ChatMessage[]): ChatMessage[] {
  return prev.map((m) =>
    m.role === "tool-group" && m.toolCalls
      ? {
          ...m,
          toolCalls: m.toolCalls.map((tc) =>
            tc.result === undefined ? { ...tc, result: "(stopped)" } : tc,
          ),
        }
      : m,
  );
}

export function stoppedNotice(): ChatMessage {
  return { id: `e-${Date.now()}`, role: "agent", content: "(Stopped)", timestamp: Date.now() };
}

// Whether the failed turn already produced visible output (an agent or
// tool-group bubble after the last user message). If so, a late throw
// after a successful turn must NOT tack a confusing failure bubble on.
export function turnRepliedSinceLastUser(prev: ChatMessage[]): boolean {
  const lastUser = [...prev].reverse().findIndex((m) => m.role === "user");
  if (lastUser < 0) return false;
  return prev
    .slice(prev.length - lastUser)
    .some((m) => m.role === "agent" || m.role === "tool-group");
}

// Non-abort send failure: append the failure bubble unless the turn
// already produced output (see turnRepliedSinceLastUser).
export function appendTurnFailure(prev: ChatMessage[], err: unknown): ChatMessage[] {
  if (turnRepliedSinceLastUser(prev)) return prev;
  const errMsg = err instanceof Error && err.message
    ? err.message
    : "Failed to get a response. Is the gateway running?";
  return [
    ...prev,
    {
      id: `e-${Date.now()}`,
      role: "agent",
      content: errMsg,
      timestamp: Date.now(),
    },
  ];
}

// Post-turn workspace diff: attach the produced-file chips to the last
// bubble (final reply). No-op on an empty list.
export function attachFilesToLast(prev: ChatMessage[], files: ChatMessage["files"]): ChatMessage[] {
  if (prev.length === 0 || !files || files.length === 0) return prev;
  const updated = [...prev];
  const last = updated[updated.length - 1];
  updated[updated.length - 1] = { ...last, files };
  return updated;
}
