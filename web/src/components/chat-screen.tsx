"use client";

import { useEffect, useMemo, useState, useRef, useCallback, memo } from "react";
import { useRouter, usePathname, useSearchParams } from "next/navigation";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { Button } from "@/components/ui/button";
import { fileUrl, getAgent, getAgentSkills, getChatHistoryWithCursor, getChatSessions, getChatTodo, listAgentFiles, listProjects, renameChatSession, setChatSessionChatOnly, forkChatSession, sendChatStream, steerChat, stopChat, uploadAgentFiles, getSkills, type ChatHistoryMessage, type ChatStreamEvent, type SkillInfo, type TodoItem, type KnowledgeSource, type ToolResultMetadata } from "@/lib/api";
import { Bot, Send, Copy, Check, Pencil, Brain, BookOpen, Clock, CreditCard, Globe, Target, Wrench, Zap, ChevronDown, ChevronRight, X, File, FileText, Image as ImageIcon, FileCode, Music, Puzzle, SlidersHorizontal, ShieldCheck, Paperclip, Square, FolderOpen, GitBranch, RefreshCw, Eye, RotateCcw, ListChecks, Terminal, MessageCircle } from "lucide-react";
import Link from "next/link";
import { ChatMarkdown, knowledgeSourceLabel } from "@/components/chat-markdown";
import {
  authPromptMessages,
  errorMessage,
  compactionMessage,
  skillPendingMessages,
  appendTurnFailure,
  appendUserTurn,
  attachFilesToLast,
  markStoppedTools,
  sealStreamedBubble,
  stoppedNotice,
  type ChatMessage,
  type UserAttachment,
} from "@/components/chat-events";
import {
  createTurnEventHandler,
  extractProducedFiles,
  reconcileTurnFiles,
  type ProducedFile,
  type TurnState,
} from "@/components/chat-turn";
import { splitDataImagesCached, splitReplyOptionsCached, splitOnMarker, SPLIT_MARKER } from "@/components/chat-content";
import { FilesPanel } from "@/components/chat-file-tree";
import { FileViewer, WorkspacePanel } from "@/components/chat-workspace";
import { BUILTIN_COMMANDS, READ_ONLY_SLASH_COMMANDS, SlashMenu, TodoPanel, type SlashCommand, type SlashItem } from "@/components/chat-widgets";

// ---------------------------------------------------------------------------
// Memoized regular-message bubble. The original renderRegularBubble was an
// inline closure in ChatScreen's render IIFE: every keystroke/delta flush
// re-executed the whole bubble (markdown render, attachment mapping,
// action-row wiring) for EVERY message. Hoisted here as a memo component
// with an explicit prop contract — a delta flush touching one message now
// re-renders only that message's subtree. The JSX and behavior are
// byte-equivalent to the extracted closure.
// ---------------------------------------------------------------------------

// Memoized regular-message bubble. The original renderRegularBubble was an
// inline closure in ChatScreen's render IIFE: every keystroke/delta flush
// re-executed the whole bubble (markdown render, attachment mapping,
// action-row wiring) for EVERY message. Hoisted here as a memo component
// with an explicit prop contract — a delta flush touching one message now
// re-renders only that message's subtree. The JSX and behavior are
// byte-equivalent to the extracted closure.

// Pure time formatters hoisted from ChatScreen so the memo component can
// import-free reference them (they use no closure state).
const formatTime = (ts: number) =>
  new Date(ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

const formatDateTime = (ts: number) => {
  const d = new Date(ts);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
};

const RegularBubble = memo(function RegularBubble({
  msg,
  selectedAgent,
  sessionId,
  workspaceRoot,
  sending,
  copiedId,
  liveAfter,
  attachedImages,
  surfacedSrcs,
  suppressAllInlineImages,
  pendingPlanId,
  t,
  onQuickReply,
  onCopy,
  onRetry,
  onFork,
  onOpenFile,
  onOpenFilesSheet,
  onPlanEdit,
}: RegularBubbleCallbacks & { msg: ChatMessage }) {
  // Quick-reply options: one split per render feeds both the bubble text
  // (block stripped) and the button row below. Raw content stays
  // untouched in msg.content so copy / export / replay keep the protocol
  // text. Cached by input: unchanged agent bubbles skip the regex scan.
  const replySplit = msg.role === "agent" && msg.content ? splitReplyOptionsCached(msg.content, splitReplyOptions) : null;
  const metadata = (msg.metadata ?? {}) as {
    knowledgeSources?: KnowledgeSource[];
    iterationCapReached?: boolean;
    iterationCapValue?: string;
    planMode?: boolean;
  };
  return (
    <div
      id={msg.seq != null ? `seq-${msg.seq}` : undefined}
      className={`flex ${msg.role === "user" ? "justify-end" : "justify-start"} ${
        // Rise-in only for messages that arrived after this render tree
        // mounted (timestamp gate) — history and page-load never animate,
        // live arrivals acknowledge themselves. See .msg-in in globals.css.
        msg.timestamp > liveAfter ? "msg-in" : ""
      }`}
    >
      <div
        className={`group relative ${
          msg.role === "user" ? "max-w-[80%] order-1" : "max-w-full"
        }`}
      >
        <div
          className={`rounded-2xl px-4 py-2.5 break-words ${
            msg.role === "user"
              ? "bg-primary/10 dark:bg-primary/15 text-foreground rounded-br-md"
              : "bg-muted rounded-bl-md"
          }`}
        >
          {(() => {
            const attached = attachedImages.get(msg.id);
            return attached && attached.length > 0 ? (
              <div className="space-y-2 mb-2">
                {attached.map((img, i) => (
                  // eslint-disable-next-line @next/next/no-img-element
                  <img
                    key={i}
                    src={img.src}
                    alt={img.alt}
                    className="rounded-lg max-w-full h-auto"
                  />
                ))}
              </div>
            ) : null;
          })()}
          {msg.role === "user" && msg.attachments && msg.attachments.length > 0 && (
            <div className="flex flex-wrap gap-2 mb-2 justify-end">
              {msg.attachments.map((att, i) =>
                att.isImage && att.previewUrl ? (
                  <button
                    key={i}
                    type="button"
                    onClick={() => onOpenFilesSheet()}
                    className="block cursor-zoom-in"
                    aria-label={`Preview ${att.name}`}
                  >
                    {/* eslint-disable-next-line @next/next/no-img-element */}
                    <img
                      src={att.previewUrl}
                      alt={att.name}
                      className="rounded-lg max-h-48 max-w-[12rem] w-auto h-auto object-cover"
                    />
                  </button>
                ) : (
                  <div
                    key={i}
                    className="flex items-center gap-2 rounded-md bg-sidebar-foreground/10 px-2 py-1.5 text-xs"
                  >
                    <Paperclip className="h-3 w-3 opacity-70" />
                    <span className="truncate">{att.name}</span>
                  </div>
                ),
              )}
            </div>
          )}
          {msg.content &&
            (renderContentWithDataImages(
              replySplit?.text ?? msg.content,
              surfacedSrcs,
              (attachedImages.get(msg.id)?.length ?? 0) > 0,
              selectedAgent,
              sessionId,
              metadata.knowledgeSources,
              workspaceRoot,
            ) ?? (
              <ChatMarkdown text={replySplit?.text ?? msg.content} agentId={selectedAgent} sessionId={sessionId} workspaceRoot={workspaceRoot} knowledgeSources={metadata.knowledgeSources} />
            ))}
          {msg.role === "agent" && metadata.knowledgeSources && metadata.knowledgeSources.length > 0 && (
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              <span className="text-2xs text-muted-foreground">{t("knowledge.sourcesLabel")}</span>
              {metadata.knowledgeSources.map((src) => (
                <span
                  key={src.id}
                  className="rounded bg-primary/10 px-1.5 py-0.5 text-2xs font-medium text-primary"
                  title={knowledgeSourceLabel(src)}
                >
                  {src.id}
                </span>
              ))}
            </div>
          )}
          {msg.role === "agent" && metadata.iterationCapReached && (
            <div className="mt-2 flex items-start gap-1.5 rounded-md border border-warning/40 bg-warning/10 px-2.5 py-1.5 text-xs text-warning dark:text-warning">
              <span className="font-medium">{t("preview.iterationLimit")}</span>
              <span className="opacity-80">
                {t("preview.iterationLimitDesc", { value: metadata.iterationCapValue ?? "" })}
              </span>
            </div>
          )}
          {msg.role === "agent" && metadata.planMode && (
            <div className="mt-2 flex items-start gap-1.5 rounded-md border border-warning/40 bg-warning/10 px-2.5 py-1.5 text-xs text-warning dark:text-warning">
              <ListChecks className="mt-0.5 h-3.5 w-3.5 shrink-0" />
              <span className="font-medium">{t("preview.planOnly")}</span>
              <span className="opacity-80">
                {t("preview.planOnlyDesc")}
              </span>
            </div>
          )}
          {replySplit && replySplit.options.length > 0 && (
            <div className="mt-3 flex flex-wrap gap-2">
              {replySplit.options.map((opt) => (
                <Button
                  key={opt}
                  size="sm"
                  variant="outline"
                  onClick={() => onQuickReply(opt)}
                  className="h-8"
                >
                  {opt}
                </Button>
              ))}
            </div>
          )}
          {msg.role === "agent" && msg.authOptions && msg.authOptions.options.length > 0 && (
            <div className="mt-3 flex flex-wrap gap-2">
              {msg.authOptions.options.map((opt) => (
                <Button
                  key={opt.cmd}
                  size="sm"
                  variant={opt.cmd === "/yes" ? "default" : "outline"}
                  onClick={() => onQuickReply(opt.cmd)}
                  className="h-8"
                >
                  <span>{opt.label_zh}</span>
                  <span className="opacity-60 ml-1 text-2xs">{opt.label_en}</span>
                </Button>
              ))}
            </div>
          )}
          {msg.role === "agent" && msg.id === pendingPlanId && (
            <div className="mt-3 flex flex-wrap items-center gap-2">
              <Button
                size="sm"
                onClick={() => onQuickReply("go")}
                disabled={sending}
                className="h-8 gap-1.5"
              >
                <Check className="h-3.5 w-3.5" />
                {t("chatScreen.runPlan")}
              </Button>
              <Button
                size="sm"
                variant="outline"
                onClick={onPlanEdit}
                disabled={sending}
                className="h-8 gap-1.5"
              >
                <X className="h-3.5 w-3.5" />
                {t("chatScreen.editPlan")}
              </Button>
              <span className="text-xs text-muted-foreground">
                {t("chatScreen.planHint")}
              </span>
            </div>
          )}
        </div>
        {msg.files && msg.files.length > 0 && (
          <FilesPanel agentId={selectedAgent} files={msg.files} onOpenFile={onOpenFile} />
        )}
        <div
          className={`flex items-center gap-1.5 mt-1 ${
            msg.role === "user" ? "justify-end" : "justify-start"
          }`}
        >
          {msg.role === "user" ? (
            <>
              {msg.timestamp > 0 && (
                <span
                  className="opacity-0 group-hover:opacity-100 text-2xs text-muted-foreground/60 transition-all"
                  title={formatDateTime(msg.timestamp)}
                >
                  {formatTime(msg.timestamp)}
                </span>
              )}
              <button
                onClick={() => onCopy(msg)}
                className="opacity-0 group-hover:opacity-100 p-0.5 rounded-md hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                title={t("chat.copy")}
              >
                {copiedId === msg.id ? (
                  <Check className="h-3 w-3 text-success" />
                ) : (
                  <Copy className="h-3 w-3" />
                )}
              </button>
              <button
                onClick={() => onRetry(msg)}
                className="opacity-0 group-hover:opacity-100 p-0.5 rounded-md hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                title={t("chat.resend")}
              >
                <RotateCcw className="h-3 w-3" />
              </button>
            </>
          ) : (
            <>
              {msg.timestamp > 0 && (
                <span
                  className="text-2xs text-muted-foreground/60"
                  title={formatDateTime(msg.timestamp)}
                >
                  {formatTime(msg.timestamp)}
                </span>
              )}
              <button
                onClick={() => onCopy(msg)}
                className="opacity-0 group-hover:opacity-100 p-0.5 rounded-md hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                title={t("chat.copy")}
              >
                {copiedId === msg.id ? (
                  <Check className="h-3 w-3 text-success" />
                ) : (
                  <Copy className="h-3 w-3" />
                )}
              </button>
              <button
                onClick={onOpenFilesSheet}
                className="opacity-0 group-hover:opacity-100 p-0.5 rounded-md hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                title={t("chat.viewTaskFiles")}
              >
                <FolderOpen className="h-3 w-3" />
              </button>
              {msg.seq != null && msg.seq >= 0 && (
                <button
                  onClick={() => onFork(msg)}
                  className="opacity-0 group-hover:opacity-100 p-0.5 rounded-md hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                  title={t("chat.fork")}
                >
                  <GitBranch className="h-3 w-3" />
                </button>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  );
});

// ---------------------------------------------------------------------------
// Shared event reducer — the auth_prompt / subagent_progress / steer /
// error / compaction_notice / skill_pending cases used to be implemented
// twice verbatim (subscribe switch + POST switch) and had already drifted
// (claim timing, metadata handling). These helpers carry the RENDER
// semantics once; the two call sites keep only their own seq/claim
// bookkeeping and pass the event payload in. Returns the message-appends
// as updater lists so callers feed them straight to setMessages.
// ---------------------------------------------------------------------------


// splitDataImagesCached (imported) memoizes the regex-heavy image scan;
// the caching implementation now lives in components/chat-content.ts.

// Models sometimes emit markdown images with a line break between `]` and
// `(`, or with very long base64 destinations that some commonmark parsers
// give up on. Rather than fight the markdown parser, extract
// `![alt](data:image/...)` AND `![alt](https://...)` (tolerating
// whitespace/newlines between `]` and `(`) and render those as native
// <img>. The http(s) branch matters for image_gen, whose tool output is a
// remote-URL markdown tag — without extracting it the generated image
// never renders inline. Returns null if no markdown images are present
// so the caller can fall through to a plain ChatMarkdown render.
// When `suppressAllInlineImages` is true, every markdown image inside
// the content is dropped (used when the bubble has tool-output images
// already attached at the top — the model often re-embeds an image in
// its reply, and because LLMs can't reproduce base64 verbatim the
// re-embedded bytes are either a duplicate we already showed or a
// hallucination that renders as a different picture; for remote URLs the
// re-embed is simply a duplicate of the one already surfaced). `surfacedSrcs`
// provides a narrower exact-src match for cases where nothing is
// attached to this bubble but a prior bubble showed the same bytes.
function renderContentWithDataImages(
  content: string,
  surfacedSrcs?: ReadonlySet<string>,
  suppressAllInlineImages?: boolean,
  agentId?: string,
  sessionId?: string,
  knowledgeSources?: KnowledgeSource[],
  workspaceRoot?: string,
): React.ReactNode | null {
  const parts = splitDataImagesCached(content);
  if (!parts.some((p) => p.type === "image")) return null;
  return (
    <>
      {parts.map((p, i) => {
        if (p.type === "image") {
          if (suppressAllInlineImages || surfacedSrcs?.has(p.src)) return null;
          return (
            // eslint-disable-next-line @next/next/no-img-element
            <img key={i} src={p.src} alt={p.alt} className="rounded-lg max-w-full h-auto my-2" />
          );
        }
        return <ChatMarkdown key={i} text={p.text} agentId={agentId} sessionId={sessionId} workspaceRoot={workspaceRoot} knowledgeSources={knowledgeSources} />;
      })}
    </>
  );
}

import { usePageHeader } from "@/components/sidebar";
import { channelLabel } from "@/components/channel-icon";
import { ChatDashboard } from "@/components/chat-dashboard";
import { useT } from "@/lib/i18n";
import { splitReplyOptions } from "@/lib/reply-options";


// SPLIT_MARKER and splitOnMarker now live in components/chat-content.ts
// (shared with the RegularBubble memo component).

// Props for the memoized regular-message bubble. Callbacks arrive as a
// flat signature set; ChatScreen passes them via a shallow-stable object
// so memo's shallow compare skips re-renders when only an unrelated
// bubble's content changed (the heavy path used to re-run the whole
// render IIFE per keystroke/delta flush).
interface RegularBubbleCallbacks {
  selectedAgent: string;
  sessionId: string;
  workspaceRoot?: string;
  sending: boolean;
  copiedId: string | null;
  liveAfter: number; // timestamps ≤ this are pre-mount history (no msg-in animation)
  attachedImages: ReadonlyMap<string, Array<{ alt: string; src: string }>>;
  surfacedSrcs?: ReadonlySet<string>;
  suppressAllInlineImages?: boolean;
  pendingPlanId: string | null;
  t: (key: string, vars?: Record<string, string | number>) => string;
  onQuickReply: (text: string) => void; // routes send vs steer like the inline buttons did
  onCopy: (msg: ChatMessage) => void;
  onRetry: (msg: ChatMessage) => void;
  onFork: (msg: ChatMessage) => void;
  onOpenFile: (file: ProducedFile) => void;
  onOpenFilesSheet: () => void;
  onPlanEdit: () => void;
}

// splitOnMarker breaks `s` on SPLIT_MARKER, trims each chunk, and
// drops the empty ones. Used at render time so a streamed assistant
// reply containing the marker becomes multiple bubbles without any
// upstream content-event rewriting.
// splitOnMarker now lives in components/chat-content.ts (shared).

interface ChatSession {
  id: string;
  title?: string;
  preview: string;
  // channel/accountId/chatId travel with the listing so the chat
  // page can decide whether composing into this session is allowed
  // (only `web` is — IM channels have no reverse-send path).
  channel?: string;
  accountId?: string;
  chatId?: string;
  // chatOnly mirrors sessions.chat_only — pure-conversation mode flag
  // driving the composer toggle.
  chatOnly?: boolean;
}

function generateSessionId() {
  return `s-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

/** Convert raw history messages into UI ChatMessages, grouping tool calls with results. */
function buildChatMessages(history: ChatHistoryMessage[]): ChatMessage[] {
  const msgs: ChatMessage[] = [];
  let i = 0;
  while (i < history.length) {
    const h = history[i];
    if (h.role === "user") {
      // System notices rendered as centered pills instead of user bubbles:
      // turn_aborted_notice (turn-abort boundary from crash-heal/stop) and
      // turn_error_notice (persisted LLM-failure with its cause — the
      // durable copy a reloaded chat reads, since the live SSE error event
      // vanishes on refresh).
      if (h.kind === "turn_aborted_notice" || h.kind === "turn_error_notice") {
        msgs.push({ id: `h-${i}`, role: "notice", kind: h.kind, content: h.content || "", timestamp: h.timestamp || 0 });
        i++;
        continue;
      }
      // Surface image attachments on history-loaded user bubbles. The
      // server emits `imageUrls` on user turns whose ContentParts had
      // image_url blocks; map them into UserAttachment entries so the
      // existing bubble renderer (live-send path) handles them with no
      // additional branching.
      const attachments: UserAttachment[] | undefined =
        h.imageUrls && h.imageUrls.length > 0
          ? h.imageUrls.map((url, idx) => ({
              name: `image-${idx + 1}`,
              isImage: true,
              previewUrl: url,
            }))
          : undefined;
      const sender = h.senderName
        ? {
            name: h.senderName,
            avatarUrl: h.senderAvatarUrl,
            id: h.senderId,
            channel: h.senderChannel,
          }
        : undefined;
      msgs.push({ id: `h-${i}`, role: "user", content: h.content || "", timestamp: h.timestamp || 0, seq: h.seq, attachments, sender });
      i++;
    } else if (h.role === "assistant" && h.toolCalls && h.toolCalls.length > 0) {
      // Group: assistant tool_calls + following tool results + final assistant content
      const calls = h.toolCalls.map((tc) => ({
        ...tc,
        result: undefined as string | undefined,
        metadata: undefined as ToolResultMetadata | undefined,
      }));
      i++;
      // Collect tool results
      while (i < history.length && history[i].role === "tool") {
        const toolMsg = history[i];
        const call = calls.find((c) => c.id === toolMsg.toolCallId);
        if (call) {
          call.result = toolMsg.content;
          if (toolMsg.metadata) call.metadata = toolMsg.metadata;
        }
        i++;
      }
      // Defensive: any tool_use that still has no result by the time
      // we leave the tool-result run got orphaned (client aborted, server
      // crashed mid-turn, persistence path raced). Mark them stopped so
      // the UI shows a terminal state instead of spinning forever.
      // Newer turns will have these padded server-side via
      // padOrphanToolResults; this catches sessions that pre-date the fix.
      for (const c of calls) {
        if (c.result === undefined) {
          c.result = "(stopped)";
        }
      }
      // If this assistant turn produced text alongside its tool calls
      // (common with "final answer + closing tool" patterns like text +
      // update_goal), surface that text as its own agent bubble BEFORE
      // the tool-group instead of folding it into the tool-group's
      // content. Folded, the body reads as preamble to a collapsed tool
      // block; split, the model's actual answer stands as a first-class
      // reply.
      if (h.content && h.content.trim()) {
        msgs.push({ id: `h-pre-${i}`, role: "agent", content: h.content, timestamp: h.timestamp || 0, seq: h.seq, metadata: h.metadata });
      }
      msgs.push({
        id: `h-tool-${i}`,
        role: "tool-group",
        content: "",
        timestamp: h.timestamp || 0,
        seq: h.seq,
        toolCalls: calls,
      });
      // If next is assistant with ONLY content and no tool calls (final
      // answer), add it. Must skip when the next assistant also has tool
      // calls — in a multi-turn conversation that's the *start of the next
      // tool-group*, not a final answer, and consuming it here would drop
      // its tool calls on the floor and leave subsequent tool-result
      // messages orphaned.
      if (
        i < history.length &&
        history[i].role === "assistant" &&
        history[i].content &&
        !(history[i].toolCalls && history[i].toolCalls!.length > 0)
      ) {
        msgs.push({ id: `h-${i}`, role: "agent", content: history[i].content || "", timestamp: history[i].timestamp || 0, seq: history[i].seq, metadata: history[i].metadata });
        i++;
      }
    } else if (h.role === "assistant" && h.kind === "compaction_notice") {
      // Compaction notice entry from WebChatHistory — render as a
      // centered system bubble, not a normal assistant message.
      msgs.push({ id: `h-${i}`, role: "notice", kind: "compaction_notice", content: h.content || "", timestamp: h.timestamp || 0 });
      i++;
    } else if (h.role === "assistant") {
      // Skip whitespace-only / empty assistant turns — they'd render as
      // phantom blank bubbles. Pure tool-call turns are mapped to
      // tool-groups above.
      if (h.content && h.content.trim()) {
        msgs.push({ id: `h-${i}`, role: "agent", content: h.content, timestamp: h.timestamp || 0, seq: h.seq, metadata: h.metadata });
      }
      i++;
    } else {
      i++; // skip unexpected
    }
  }
  return msgs;
}

// latestHistorySeq returns the highest session_messages seq in a history
// page (ascending) — trailing rows may be notice entries with no seq, so
// walk back to the last row that has one. Maintains the message-space
// cursor lastMsgSeqRef; see syncNewer for why that cursor exists at all.
function latestHistorySeq(history: ChatHistoryMessage[]): number {
  for (let i = history.length - 1; i >= 0; i--) {
    const s = history[i].seq;
    if (typeof s === "number") return s;
  }
  return -1;
}

// isPendingPlanContent recognises the closing line we instructed the
// model to emit on plan-first turns ("Reply `go` to execute, or tell
// me what to change." or its Chinese rendering). Used to decide
// whether to show the inline approve / cancel buttons under an
// assistant bubble. Loose match — model wording drifts but the
// signal is always "go" near "execute / 执行" in the last few lines.
function isPendingPlanContent(content: string): boolean {
  if (!content) return false;
  // Tail-only check so a long plan with the word "go" in step 3 doesn't
  // false-positive — the model only ever closes with the cue line, never
  // opens with it.
  const lines = content.split("\n");
  const tail = lines.slice(Math.max(0, lines.length - 4)).join(" ");
  // English: "Reply `go` to execute" / "Reply 'go' to run"
  if (/reply[^.]*?\bgo\b[^.]*?(execute|run)/i.test(tail)) return true;
  // Chinese: "请回复 go 开始执行" / "回复 go 执行"
  if (/回复[^。]*?go[^。]*?(执行|开始)/.test(tail)) return true;
  // Generic safety net: closing imperative with `go` + execute keyword
  // very close together. Tighter than just "go" appearing anywhere.
  if (/[`'"]go[`'"][^\n]{0,40}(execute|执行)/i.test(tail)) return true;
  return false;
}

// Parse the per-route ids out of the pathname. ChatScreen is mounted
// once at the agent layout level and stays alive across these routes:
//
//   /agents/<aid>/                         — fresh loose chat
//   /agents/<aid>/chat/                    — fresh loose chat
//   /agents/<aid>/chat/<session>           — open existing chat by id
//   /agents/<aid>/project/<pid>            — fresh chat in a project
//
// Reading from `usePathname()` (instead of accepting props from the
// page tree) is what lets the component instance survive sidebar
// navigations — sessionId / projectId become reactive values that
// update in place rather than gating a remount.
function parseAgentRoute(pathname: string): {
  sessionId: string;
  projectId: string;
} {
  const sessMatch = pathname.match(/^\/agents\/[^/]+\/chat\/([^/]+)/);
  if (sessMatch) {
    const sid = sessMatch[1];
    // "_" is the build-time placeholder Next emits under output:'export'
    // for the dynamic [session] segment. Treat it as "no session".
    return { sessionId: sid === "_" ? "" : sid, projectId: "" };
  }
  const projMatch = pathname.match(/^\/agents\/[^/]+\/project\/([^/]+)/);
  if (projMatch) {
    const pid = projMatch[1];
    return { sessionId: "", projectId: pid === "_" ? "" : pid };
  }
  return { sessionId: "", projectId: "" };
}

export function ChatScreen() {
  const t = useT();
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  // When `?actAs=<uid>` is in the URL, this chat is being opened by a
  // super_admin viewing another user's session (read-only by middleware).
  // Forces the composer into a disabled state and surfaces a banner so
  // the admin can't try to type and get a silent 403.
  const actAsUserId = searchParams?.get("actAs") || "";
  const isActAsView = !!actAsUserId;
  const { sessionId: urlSessionId, projectId: urlProjectId } = useMemo(
    () => parseAgentRoute(pathname || ""),
    [pathname],
  );
  // Reactive: re-derives from pathname so switching agents (sidebar
  // dropdown, browser back/forward) immediately updates downstream
  // fetches. The previous useState(() => ...) flavor froze the id at
  // mount, so background loads kept hitting the old agent and the
  // panel showed stale history under the new URL.
  const selectedAgent = useAgentIdFromURL();
  const [agentName, setAgentName] = useState<string>("");
  // Resolved metadata for `urlProjectId`, surfaced as the
  // empty-state info card on /agents/<aid>/project/<pid>. Null until
  // the fetch lands; the card hides while loading rather than
  // flashing a placeholder.
  const [projectInfo, setProjectInfo] = useState<{
    id: string;
    name: string;
    description?: string;
    updatedAt?: string;
    createdAt?: string;
  } | null>(null);
  const [sessionId, setSessionId] = useState<string>(
    () => urlSessionId || generateSessionId(),
  );
  // Scope directory prefixes for THIS chat, as resolved by the backend
  // (listAgentFiles → scopePrefixes): loose chat ["sessions/<sid>/"],
  // project chat ["projects/<pid>/<sid>/", "projects/<pid>/"]. Drives
  // workspace-link resolution (/workspace/rel → projects/<pid>/rel),
  // produced-file chip path candidates, and the turn-end reconcile —
  // the chat URL has no project segment, so the client can't derive the
  // project a chat belongs to on its own. Empty until the listing lands
  // (or on failure); consumers fall back to a client-side guess.
  const [wsScopePrefixes, setWsScopePrefixes] = useState<string[]>([]);
  // Mirror for the sendChatStream callback: streaming closures capture
  // state at call time, so the turn's chip builder needs a ref to see
  // prefixes fetched by the loadSession that already ran.
  const wsPrefixesRef = useRef<string[]>([]);
  useEffect(() => {
    wsPrefixesRef.current = wsScopePrefixes;
  }, [wsScopePrefixes]);
  // The sandbox mounts this directory at /workspace: the SHALLOWEST scope
  // prefix (project root for project chats, the session dir for loose
  // chats). Passed to ChatMarkdown so `/workspace/<rel>` links and
  // images resolve correctly; client-side guesses cover the pre-listing
  // window (a brand-new project chat still carries urlProjectId).
  const workspaceRoot = useMemo(() => {
    if (wsScopePrefixes.length > 0) return wsScopePrefixes[wsScopePrefixes.length - 1];
    if (urlProjectId) return `projects/${urlProjectId}/`;
    return sessionId ? `sessions/${sessionId}/` : "";
  }, [wsScopePrefixes, urlProjectId, sessionId]);
  const [sessions, setSessions] = useState<ChatSession[]>([]);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  // Timestamp taken after first mount. Bubbles whose msg.timestamp is
  // newer than this get the .msg-in entrance — history always carries
  // older timestamps, so only genuinely live arrivals animate. Reset on
  // remount (session switch), which is exactly the semantics wanted.
  const liveAfterRef = useRef(0);
  useEffect(() => {
    liveAfterRef.current = Date.now();
  }, []);
  const [input, setInput] = useState("");
  // Per-session in-flight turns: taskqueue serializes per chat but the
  // global maxConcurrent lets different chats of one agent stream in
  // parallel, so "is sending" is a per-session fact, not a global one.
  // `sending` stays as the derived flag the composer/Stop/Enter routing
  // reads for the VIEWED session — replacing it kept every existing
  // call site's semantics while fixing the old global boolean that let
  // one session's finally reset another session's Stop button.
  const [sendingSessions, setSendingSessions] = useState<ReadonlySet<string>>(new Set());
  const sending = sessionId !== null && sendingSessions.has(sessionId);
  // todo.md state for the current session — agent maintains the file,
  // we re-fetch on every write_file/edit_file event that touches
  // todo.md plus once at mount. Empty `items` hides the panel.
  const [todoItems, setTodoItems] = useState<TodoItem[]>([]);
  // Last subagent_progress event from the active delegate_task run.
  // Cleared when the subagent reports phase="done" or when sending
  // turns off, so it never lingers across turns. Only one subagent
  // runs at a time (delegate_task is registered serial) so we don't
  // need to key this by tool_call_id.
  const [subagentProgress, setSubagentProgress] = useState<null | {
    iteration?: number;
    max?: number;
    phase?: "thinking" | "running" | "final-delivery" | "done";
    tools?: string[];
  }>(null);
  // Backend connection-retry tier is riding out a network outage — shown
  // as a transient pill under the message list. State, not a message:
  // it must never persist into history. Any subsequent progress event
  // (content / tool call / done) clears it.
  const [reconnecting, setReconnecting] = useState<null | { attempt?: number }>(null);
  // SSE terminal-offline state: EventSource auto-reconnects forever with
  // zero UI feedback, and a persistent 404 (session deleted) or dead
  // proxy just flaps silently. After repeated failed reconnects the
  // subscribe effect closes the connection and flips this on — the
  // message list renders a "connection lost" pill with a manual retry
  // (bumping subscribeRetryNonce re-runs the subscribe effect).
  const [subscribeDead, setSubscribeDead] = useState(false);
  const [subscribeRetryNonce, setSubscribeRetryNonce] = useState(0);
  const subscribeErrCountRef = useRef(0);
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [filesSheetOpen, setFilesSheetOpen] = useState(false);
  // File the user clicked inside the conversation (a FilesPanel row) —
  // handed to WorkspacePanel as initialFile so opening the panel also
  // opens that file's preview, instead of landing on an empty tree.
  const [pendingFile, setPendingFile] = useState<ProducedFile | null>(null);
  // The workspace/preview panel opens to the right of chat. The platform
  // sidebar is left in whatever state the user put it — opening a file in
  // the conversation no longer collapses it.
  const [sessionTitle, setSessionTitle] = useState<string>("");
  const [attachments, setAttachments] = useState<File[]>([]);
  // Lightbox for clicking either an attachment thumbnail (compose box)
  // or an inline image in a sent message bubble. `null` = closed.
  const [lightboxSrc, setLightboxSrc] = useState<string | null>(null);
  // Object URLs for image attachments in the compose box. Keyed by file
  // index so we can revoke on remove without re-computing for every
  // chip on every keystroke. Re-derived whenever `attachments` changes.
  const attachmentPreviews = useMemo(
    () =>
      attachments.map((f) =>
        f.type.startsWith("image/") ? URL.createObjectURL(f) : null,
      ),
    [attachments],
  );
  useEffect(() => {
    return () => {
      for (const url of attachmentPreviews) if (url) URL.revokeObjectURL(url);
    };
  }, [attachmentPreviews]);
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const messagesScrollRef = useRef<HTMLDivElement>(null);
  // Auto-scroll only when the user is already pinned to the bottom.
  // Toggled false the moment they scroll up so streaming agent output
  // can't yank them back down mid-read, then flipped back to true once
  // they return to the bottom (or hit the "scroll to latest" button).
  const stickToBottomRef = useRef(true);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Dedupe events arriving on both the active POST stream and the
  // parallel /api/chat/subscribe SSE — both subscribe to the same
  // chat-events hub on the server. Tracks the highest seq we've
  // already rendered for the current session.
  const maxSeqRef = useRef<number>(-1);
  // Resume cursor handed to /api/chat/subscribe so a freshly reloaded
  // page replays only deltas it didn't see. Captured from the chat
  // history endpoint, refreshed when sessionId changes.
  const subscribeSinceRef = useRef<number>(-1);
  // Transient assistant bubble created from subscribe-replayed content
  // events (i.e. when the user reloaded mid-turn and we're catching up
  // before the agent's "done" lands). Cleared on the next history
  // reload, which replaces the placeholder with the canonical message
  // pulled from session_messages.
  const transientBubbleIdRef = useRef<string | null>(null);
  // streamingMsgIdRef holds the id of the assistant bubble currently
  // accreting content_delta chunks via the active POST sendChatStream.
  // Shared with the parallel /api/chat/subscribe SSE handler so that
  // handler can detect "POST stream owns this turn" and skip its own
  // bubble-creation path for the trailing `content` event — both
  // handlers see the same content event from the hub, and if subscribe
  // wins the race the dedup-by-seq guard won't fire on the POST side,
  // producing a duplicate bubble. The ref is reset to null at startNewGroup
  // and when a tool_call rolls the bubble into a tool-group.
  const streamingMsgIdRef = useRef<string | null>(null);
  // Delta batching (flush throttled to ~50ms): the old path ran one
  // setMessages full-array copy + string concat per streamed chunk and
  // re-rendered the whole list — the O(n²) main cause of streaming jank
  // on long sessions. Chunks accumulate here and flush on a timer (rAF
  // pauses in hidden tabs; a timer keeps the final flush deterministic —
  // the trailing `content` event flushes and clears it anyway).
  const pendingDeltaRef = useRef("");
  const deltaFlushTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // pendingRollbackRef flags the next send as a resend of the trailing
  // failed user turn. handleRetry sets it when you re-click a message
  // with no successful assistant reply after it; handleSend consumes it
  // (passes rollbackPendingUser to the server, which drops the orphan
  // user row before appending the fresh turn). Ref not state so reading
  // it doesn't trigger a re-render.
  const pendingRollbackRef = useRef<string | null>(null);
  // First-send navigation (`/chat/` -> `/chat/<sid>/`) triggers the
  // history-loading effect while the POST stream is still in flight.
  // Keep that fetch from clearing optimistic bubbles or advancing the
  // seq cursor past events the POST handler is about to render.
  const inFlightSendSessionRef = useRef<string | null>(null);
  // Currently VIEWED session/agent, mirrored from state via the effects
  // below. Stream callbacks capture their ORIGIN session at send time;
  // comparing against these refs is how a stream whose user has since
  // navigated to another session stops writing into the new session's
  // message list (the closures themselves are stale by definition).
  const viewedSessionRef = useRef<string | null>(sessionId);
  const viewedAgentRef = useRef<string | null>(selectedAgent);
  useEffect(() => {
    viewedSessionRef.current = sessionId;
  }, [sessionId]);
  useEffect(() => {
    viewedAgentRef.current = selectedAgent;
  }, [selectedAgent]);
  // AbortControllers for in-flight chat streams, keyed by sessionId so
  // parallel streams (different chats of one agent) each keep their own
  // Stop — a single slot let the newer stream overwrite the older one's
  // controller, leaving it unstoppable.
  const abortMapRef = useRef<Map<string, AbortController>>(new Map());

  // Gates the EventSource effect: holds the sessionId whose history has
  // been fetched and whose `subscribeSinceRef` is now accurate. Without
  // this gate, the SSE effect (declared earlier and therefore run first)
  // would open `/api/chat/subscribe?since=-1` before the history fetch
  // resolved — server-side that means "replay every session_event" and
  // for a long deep-research chat the replay floods the client AND ties
  // up the EventSource HTTP connection slot. Stack a few of those across
  // rapid project↔chat navigations and the browser's 6-conn-per-origin
  // limit pins everything `pending`, the page goes white, and clicks
  // stop registering. Storing the loaded sessionId (not just a boolean)
  // keeps the gate honest if sessionId changes again before history
  // catches up.
  const [loadedSessionId, setLoadedSessionId] = useState<string | null>(null);

  // Reverse-infinite scroll for long sessions. The history endpoint
  // paginates by seq: earliestSeqRef holds the oldest seq rendered so
  // far and is the `before` cursor for the next older page; hasMoreRef
  // flips false once we've reached the session's first message so we
  // stop hitting the endpoint; loadingMoreRef serializes overlapping
  // fetches when the scroll handler fires in bursts. loadingOlder drives
  // the top-of-list "loading…" indicator. Refs (not state) for the
  // cursor/guard so the scroll handler — bound once — reads fresh values
  // without re-binding on every sessionId change.
  const HISTORY_PAGE = 50;
  const earliestSeqRef = useRef<number>(0);
  const hasMoreRef = useRef(false);
  const loadingMoreRef = useRef(false);
  const [loadingOlder, setLoadingOlder] = useState(false);

  // Slash-command menu state. The menu opens when the textarea holds a
  // token beginning with `/` at the caret; selecting a skill swaps that
  // token for `/<skill-name> `, leaving the cursor after the space so the
  // user can keep typing their prompt.
  const [globalSkills, setGlobalSkills] = useState<SkillInfo[]>([]);
  const [agentSkills, setAgentSkills] = useState<SkillInfo[]>([]);
  const [slashOpen, setSlashOpen] = useState(false);
  const [slashQuery, setSlashQuery] = useState("");
  const [slashIndex, setSlashIndex] = useState(0);

  // Slash-menu skills. The global catalog is agent-independent, so it's
  // fetched once on mount; only the agent-scoped list refetches per
  // agent switch (a skill installed from /agents/<id>/skills/ shows up
  // here without a reload). That endpoint is owner-only; a failure (or
  // a non-owner viewer) just degrades to the global list.
  useEffect(() => {
    let aborted = false;
    getSkills()
      .then((list) => {
        if (!aborted) setGlobalSkills(list);
      })
      .catch(() => {});
    return () => {
      aborted = true;
    };
  }, []);

  useEffect(() => {
    let aborted = false;
    const fetch = selectedAgent
      ? getAgentSkills(selectedAgent).catch(() => [] as SkillInfo[])
      : Promise.resolve([] as SkillInfo[]);
    fetch.then((list) => {
      if (!aborted) setAgentSkills(list);
    });
    return () => {
      aborted = true;
    };
  }, [selectedAgent]);

  // Agent-scope entries shadow same-named global skills — approximates
  // the loader precedence; the list endpoints don't apply the config
  // enabled/disabled gating, so a disabled skill may still be listed.
  const skills = useMemo<SkillInfo[]>(() => {
    const merged = new Map<string, SkillInfo>();
    for (const s of globalSkills) merged.set(s.name, s);
    for (const s of agentSkills) merged.set(s.name, s);
    return [...merged.values()];
  }, [globalSkills, agentSkills]);

  // Reset chat-pane state when the active agent changes. Without
  // this, switching agents from the sidebar would briefly render the
  // previous agent's history / sessions / title under the new URL
  // until the load effects below replaced them. Clearing eagerly is
  // cheaper than threading a "loading" flag through every render.
  // We deliberately keep `input` so a half-typed message survives an
  // accidental agent switch.
  const lastAgentRef = useRef(selectedAgent);
  useEffect(() => {
    if (lastAgentRef.current === selectedAgent) return;
    lastAgentRef.current = selectedAgent;
    setMessages([]);
    setSessions([]);
    setSessionTitle("");
    setAgentName("");
    setAttachments([]);
  }, [selectedAgent]);

  // Resolve the agent's display name once. The chat title and any
  // future header bits should show "Chat with My Helper", not the
  // opaque agt_xxx id. Uses /api/agents/{id} (owner or super_admin) so
  // an admin viewing another user's agent still gets the real name —
  // /api/agents (list) is owner-scoped and would miss it.
  useEffect(() => {
    if (!selectedAgent) return;
    let aborted = false;
    getAgent(selectedAgent)
      .then((a) => {
        if (aborted) return;
        setAgentName(a?.name || a?.id || selectedAgent);
      })
      .catch(() => {
        if (!aborted) setAgentName(selectedAgent);
      });
    return () => {
      aborted = true;
    };
  }, [selectedAgent]);

  // Resolve project name for the hero title when the URL points at a
  // project's empty new-chat state. Cheap enough to do via listProjects
  // — projects per (user, agent) is small and the sidebar has already
  // warmed the network cache.
  useEffect(() => {
    if (!selectedAgent || !urlProjectId) {
      setProjectInfo(null);
      return;
    }
    let aborted = false;
    listProjects(selectedAgent)
      .then((list) => {
        if (aborted) return;
        const p = list.find((x) => x.id === urlProjectId);
        setProjectInfo(p ?? null);
      })
      .catch(() => {
        if (!aborted) setProjectInfo(null);
      });
    return () => {
      aborted = true;
    };
  }, [selectedAgent, urlProjectId]);

  // Detect whether the caret is inside a /token and, if so, what's been
  // typed after the slash. Cheap enough to run every keystroke.
  const slashContext = (value: string, caret: number): { start: number; query: string } | null => {
    const before = value.slice(0, caret);
    // Require the slash to be at start-of-message or preceded by whitespace
    // so paths / URLs with slashes don't trigger the menu.
    const match = /(^|\s)\/([\w-]*)$/.exec(before);
    if (!match) return null;
    return { start: caret - match[2].length - 1, query: match[2] };
  };

  // Translated built-in commands. BUILTIN_COMMANDS is module-scoped (used
  // by the name-only checks above) so we translate descriptions here.
  const builtinCommands = useMemo<SlashCommand[]>(
    () => BUILTIN_COMMANDS.map((c) => ({ ...c, description: t(`chatScreen.cmd.${c.name}`) })),
    [t],
  );

  // Merged command + skill list for the slash menu. Commands first so
  // built-ins are easy to find; query matches both name and description.
  // Cap at 8 to keep the popover from outgrowing the composer.
  const filteredItems: SlashItem[] = slashOpen
    ? (() => {
        const q = slashQuery.toLowerCase();
        const match = (name: string, desc: string) =>
          !q || name.toLowerCase().includes(q) || desc.toLowerCase().includes(q);
        const cmds: SlashItem[] = builtinCommands
          .filter((c) => match(c.name, c.description))
          .map((c) => ({ kind: "command", ...c }));
        const sks: SlashItem[] = skills
          .filter((s) => match(s.name, s.description || ""))
          .map((s) => ({ kind: "skill", ...s }));
        return [...cmds, ...sks].slice(0, 8);
      })()
    : [];

  const selectItem = useCallback(
    (item: SlashItem) => {
      const el = textareaRef.current;
      if (!el) return;
      const caret = el.selectionStart ?? input.length;
      const ctx = slashContext(input, caret);
      if (!ctx) return;
      const before = input.slice(0, ctx.start);
      const after = input.slice(caret);
      const insert = `/${item.name} `;
      const next = before + insert + after;
      setInput(next);
      setSlashOpen(false);
      setSlashQuery("");
      setSlashIndex(0);
      requestAnimationFrame(() => {
        const pos = before.length + insert.length;
        el.focus();
        el.setSelectionRange(pos, pos);
      });
    },
    [input],
  );

  const getBuiltInSlashCommandName = useCallback((value: string) => {
    const trimmed = value.trim();
    const match = /^\/([\w-]+)(?:\s|$)/.exec(trimmed);
    if (!match) return "";
    return BUILTIN_COMMANDS.some((c) => c.name === match[1]) ? match[1] : "";
  }, []);

  const isReadOnlySafeSlashCommand = useCallback(
    (value: string) => READ_ONLY_SLASH_COMMANDS.has(getBuiltInSlashCommandName(value)),
    [getBuiltInSlashCommandName],
  );

  const isExactBuiltInSlashCommand = useCallback(
    (value: string) => {
      const trimmed = value.trim();
      const match = /^\/([\w-]+)$/.exec(trimmed);
      return !!match && getBuiltInSlashCommandName(trimmed) === match[1];
    },
    [getBuiltInSlashCommandName],
  );

  // Load sessions when agent changes
  const loadSessions = useCallback((agentId: string) => {
    getChatSessions(agentId)
      .then((list) => setSessions(list || []))
      .catch(() => setSessions([]));
  }, []);

  useEffect(() => {
    if (!selectedAgent) return;
    loadSessions(selectedAgent);
  }, [selectedAgent, loadSessions]);

  // Live + replay subscription. Two job:
  //
  //   1. Cron-fired (and other async) plain text messages routed
  //      through the server's WebChannel — these arrive with shape
  //      { text } and get appended as a new agent bubble.
  //
  //   2. Resume-on-reload of a turn that was in flight when the user
  //      refreshed. The server replays chat_events with seq > since
  //      and then keeps the connection live for new events. These
  //      arrive with ChatStreamEvent shape ({ seq, type, data }).
  //
  // Dedupe across this connection AND the parallel POST sendChatStream
  // (both subscribe to the same hub server-side) by skipping events
  // whose seq is <= maxSeqRef.
  useEffect(() => {
    if (!selectedAgent || !sessionId) return;
    // Wait for the history fetch to land for THIS sessionId before
    // opening the SSE — see the loadedSessionId comment for the
    // browser-connection-pool failure mode this prevents.
    if (loadedSessionId !== sessionId) return;
    const since = subscribeSinceRef.current;
    const url = `/api/chat/subscribe?agentId=${encodeURIComponent(selectedAgent)}&sessionId=${encodeURIComponent(sessionId)}&since=${since}`;
    const es = new EventSource(url, { withCredentials: true });
    subscribeErrCountRef.current = 0;
    setSubscribeDead(false);
    // A successful open resets the failure streak — transient network
    // blips fire onerror per attempt but shouldn't accumulate toward
    // the terminal-offline threshold.
    es.onopen = () => {
      subscribeErrCountRef.current = 0;
      setSubscribeDead(false);
    };
    es.onmessage = (ev) => {
      let data: {
        seq?: number;
        type?: string;
        text?: string;
        data?: {
          content?: string;
          message?: string;
          metadata?: ToolResultMetadata;
          // subagent_progress fields
          iteration?: number;
          max?: number;
          phase?: "thinking" | "running" | "final-delivery" | "done";
          tools?: string[];
          // skill_pending fields (catch-up rarely carries this — seq=-1,
          // not persisted — but kept for shape parity with the live path).
          count?: number;
          names?: string[];
          // auth_prompt fields — the persisted event replays the same
          // {description, options} payload the live handler renders, so a
          // reloaded tab still gets the tappable buttons.
          description?: string;
          options?: { cmd: string; label_zh: string; label_en: string }[];
          // reconnecting fields — live-only (not persisted), so catch-up
          // never carries this; kept for shape parity with the POST path.
          attempt?: number;
          backoffMs?: number;
        };
      };
      try {
        data = JSON.parse(ev.data);
      } catch {
        return;
      }
      // Shape A: ChatStreamEvent (in-flight turn deltas).
      if (typeof data.type === "string") {
        const seq = typeof data.seq === "number" ? data.seq : -1;
        if (seq >= 0 && seq <= maxSeqRef.current) return; // already rendered via POST stream
        // While the foreground POST /api/chat/stream is active for this
        // session, let that callback own rendering. Otherwise this
        // parallel subscribe connection can win the race, create a
        // transient slash bubble, then reload history on `done`; slash
        // replies are event-only, so that reload clears the visible
        // answer and makes the send look like it did nothing.
        if (inFlightSendSessionRef.current === sessionId) return;
        // CAREFUL: do NOT bump maxSeqRef before the switch. This handler
        // intentionally drops tool_call / tool_result during catch-up
        // (the post-`done` history reload renders them properly) — but
        // a pre-switch bump would mark those seqs as "rendered" and the
        // parallel POST sendChatStream callback would dedup-skip the
        // very same events when it tries to actually render them. Bump
        // only inside cases that really took ownership of this seq.
        const claim = () => {
          if (seq >= 0) maxSeqRef.current = seq;
        };
        // Any progress event means the backend's reconnecting tier
        // recovered — clear the transient indicator (no-op re-render
        // when already null, so content_delta storms stay cheap).
        if (data.type !== "reconnecting") setReconnecting(null);
        switch (data.type) {
          case "turn_pending": {
            // A slash command queued a continuation turn: the composer's
            // own stream closes with turn_pending instead of done, and
            // the real reply arrives on the next bus-fired turn. Show
            // the same transient pill the backend reconnecting tier uses
            // so the user knows the agent is still coming back.
            setReconnecting({});
            break;
          }
          case "reconnecting": {
            // Unbounded connection-retry tier is riding out a network
            // outage (attempt ≥ 2 — the 1st retry is usually a silent
            // blip). Not persisted server-side, so this only ever fires
            // live.
            claim();
            setReconnecting({ attempt: data.data?.attempt });
            break;
          }
          case "content": {
            const content = data.data?.content || "";
            const meta = data.data?.metadata;
            if (!content && !meta) break;
            // The active POST sendChatStream is rendering this turn
            // via content_delta into streamingMsgIdRef. Both
            // subscriptions sit on the same hub, so the `content`
            // event reaches both handlers; if subscribe wins the
            // race it would create a duplicate transient bubble
            // before the POST callback can dedup. Bail when the
            // POST handler is mid-stream — it will seal the bubble
            // itself when it processes the event.
            //
            // Metadata-only events (no content) still go through so
            // a forced-final-delivery retro-stamp lands.
            if (streamingMsgIdRef.current && content) {
              claim();
              break;
            }
            claim();
            setMessages((prev) => {
              if (transientBubbleIdRef.current) {
                const idx = prev.findIndex((m) => m.id === transientBubbleIdRef.current);
                if (idx >= 0) {
                  const updated = [...prev];
                  updated[idx] = {
                    ...updated[idx],
                    content: (updated[idx].content || "") + content,
                    metadata: meta ? { ...updated[idx].metadata, ...meta } : updated[idx].metadata,
                  };
                  return updated;
                }
              }
              // Metadata-only retro-stamp event with no transient
              // bubble: attach to the most recent agent bubble so the
              // badge sticks across an active subscribe session.
              if (!content && meta) {
                for (let i = prev.length - 1; i >= 0; i--) {
                  if (prev[i].role === "agent") {
                    const updated = [...prev];
                    updated[i] = { ...updated[i], metadata: { ...updated[i].metadata, ...meta } };
                    return updated;
                  }
                }
                return prev;
              }
              const id = `resume-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
              transientBubbleIdRef.current = id;
              return [...prev, { id, role: "agent", content, timestamp: Date.now(), metadata: meta }];
            });
            break;
          }
          case "error": {
            claim();
            const msg = data.data?.message || "Unknown error";
            // Older gateways persisted cancellation as an error event.
            // Ignore those on replay so Stop produces one clear status
            // instead of "(Stopped)" followed by a stale error bubble.
            if (/\bcontext canceled\b/i.test(msg)) break;
            // Same notice pill as the live path — the persisted
            // turn_error notice renders identically on history reload.
            setMessages((prev) => [
              ...prev,
              { id: `e-${Date.now()}`, role: "notice", kind: "turn_error_notice", content: msg, timestamp: Date.now() },
            ]);
            break;
          }
          case "auth_prompt": {
            // ask-mode outside-workspace/dangerous call was parked on
            // the session; surface the authorization request with the
            // same authOptions payload the live POST handler uses, so a
            // tab that reconnects via /api/chat/subscribe (reload,
            // network blip, or catch-up after the POST stream ended)
            // still renders the tappable /yes /no /auto /yolo buttons
            // instead of a text-only fallback. auth_prompt IS persisted
            // (events.go skips only content_delta), so this branch is
            // reachable on every catch-up — the prior "Minimal render"
            // stub left reloaders with text and no buttons.
            const desc = data.data?.description || "";
            const options = Array.isArray(data.data?.options)
              ? data.data!.options!
              : [];
            claim();
            for (const m of authPromptMessages(desc, options)) {
              setMessages((prev) => [...prev, m]);
            }
            break;
          }
          case "subagent_progress": {
            claim();
            if (data.data?.phase === "done") {
              setSubagentProgress(null);
            } else {
              setSubagentProgress({
                iteration: data.data?.iteration,
                max: data.data?.max,
                phase: data.data?.phase,
                tools: data.data?.tools,
              });
            }
            break;
          }
          case "steer": {
            claim();
            applySteerEvent(data.data?.content || "");
            break;
          }
          case "session_title": {
            const d = (data.data ?? {}) as Record<string, unknown>;
            const sk = d.sessionKey;
            const title = d.title;
            if (typeof sk === "string" && typeof title === "string" && title) {
              setSessions((prev) =>
                prev.map((s) => (s.id === sk || s.chatId === sk ? { ...s, title } : s)),
              );
              // The sidebar (nav-projects) holds its own sessions list
              // sourced via fluctio:sessions-changed — nudge it to
              // refetch so the new title lands there too.
              if (typeof window !== "undefined") {
                window.dispatchEvent(
                  new CustomEvent("fluctio:sessions-changed", {
                    detail: { agentId: selectedAgent },
                  }),
                );
              }
            }
            break;
          }
          case "done": {
            claim();
            // Defensive clear — content events should already have
            // sealed the streaming bubble, but a turn that errors out
            // before the trailing `content` event lands would leave
            // the ref dangling and cause the next turn's first
            // content_delta to write into the stale id.
            streamingMsgIdRef.current = null;
            // Only reload history when we actually built a transient
            // bubble from subscribe-replayed content events (i.e. the
            // user reloaded mid-turn and we need to swap the
            // placeholder for the canonical message saved in
            // session_messages). When the active POST stream rendered
            // the turn directly, transient bubble is null — a reload
            // here would clobber any rendered error bubbles too,
            // because LLM-error turns never write an assistant
            // message to session_messages.
            if (transientBubbleIdRef.current) {
              transientBubbleIdRef.current = null;
              getChatHistoryWithCursor(selectedAgent, sessionId, { limit: HISTORY_PAGE })
                .then(({ history, latestEventSeq, earliestSeq, hasMore }) => {
                  if (latestEventSeq > maxSeqRef.current) maxSeqRef.current = latestEventSeq;
                  subscribeSinceRef.current = latestEventSeq;
                  setMessages(buildChatMessages(history));
                  lastMsgSeqRef.current = latestHistorySeq(history || []);
                  earliestSeqRef.current = earliestSeq;
                  hasMoreRef.current = hasMore;
                })
                .catch(() => {});
            }
            // Tell the sidebar to refresh — the new turn may have
            // produced an updated session title.
            if (typeof window !== "undefined") {
              window.dispatchEvent(
                new CustomEvent("fluctio:sessions-changed", {
                  detail: { agentId: selectedAgent },
                }),
              );
            }
            break;
          }
          case "compaction_notice": {
            // Auto-compaction happened mid-turn — show a centered notice
            // bubble using the backend's pre-formatted content text so
            // the live display matches what history reload returns.
            const pills = compactionMessage(data.data?.content);
            if (pills.length > 0) {
              claim();
              for (const m of pills) {
                setMessages((prev) => [...prev, m]);
              }
            }
            break;
          }
          case "skill_pending": {
            // Skill(s) staged during this turn (or earlier) are still
            // awaiting `fluctio skill approve`. Surface as a centered
            // notice pill — same render path as compaction_notice, but
            // replace any prior skill_pending pill so they don't stack
            // turn after turn. Catch-up stream rarely carries this
            // (seq=-1, not persisted to chat_events) but kept for
            // shape parity with the live switch.
            const count = data.data?.count ?? 0;
            const names = Array.isArray(data.data?.names) ? data.data!.names! : [];
            const pills = skillPendingMessages(count, names, t);
            if (pills.length > 0) {
              claim();
              setMessages((prev) => [
                ...prev.filter((m) => m.kind !== "skill_pending"),
                ...pills,
              ]);
            }
            break;
          }
          // tool_call / tool_result during catch-up are skipped here —
          // the next history reload (on `done`) will render them
          // properly via buildChatMessages.
        }
        return;
      }
      // Shape B: legacy WebChannel { text } — cron-fired async messages.
      const text = data.text || "";
      if (!text) return;
      setMessages((prev) => [
        ...prev,
        {
          id: `async-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`,
          role: "agent",
          content: text,
          timestamp: Date.now(),
        },
      ]);
    };
    es.onerror = () => {
      // EventSource auto-reconnects on transient errors. Count the
      // consecutive failures: past 3 show the reconnecting pill (the
      // connection itself gives no feedback), past 8 give up — close
      // the EventSource and surface a manual retry. The old empty
      // handler left persistent failures (session deleted, 401 expired,
      // dead proxy) flapping forever with zero user-visible signal.
      subscribeErrCountRef.current += 1;
      const n = subscribeErrCountRef.current;
      if (n >= 8) {
        es.close();
        setReconnecting(null);
        setSubscribeDead(true);
        return;
      }
      if (n >= 3) setReconnecting({ attempt: n });
    };
    return () => {
      es.close();
    };
  }, [selectedAgent, sessionId, loadedSessionId, subscribeRetryNonce]);

  // Reactively swap sessionId when the URL changes underneath us.
  // Three URL transitions matter, all handled by the same branch logic:
  //   - /chat/A → /chat/B               : adopt the new session id
  //   - /chat/A → /chat/ or /project/P  : mint a fresh id, clear messages
  //   - /chat/  → /chat/A               : adopt the session id
  // `prevHadSessionRef` keeps the initial mount from re-minting on top
  // of the id useState() already picked, AND keeps two consecutive
  // no-session URLs (e.g. /chat/ → /project/P/) from churning the id.
  // Messages are cleared on any id change so the in-place swap doesn't
  // briefly show the previous chat's content while the new history
  // fetch is in flight.
  const prevHadSessionRef = useRef(false);
  useEffect(() => {
    if (urlSessionId) {
      prevHadSessionRef.current = true;
      if (urlSessionId !== sessionId) {
        setSessionId(urlSessionId);
        setMessages([]);
      }
      return;
    }
    if (prevHadSessionRef.current) {
      prevHadSessionRef.current = false;
      setSessionId(generateSessionId());
      setMessages([]);
    }
  }, [urlSessionId, sessionId]);

  // Switching conversations (sidebar chat click, New chat, opening a project)
  // changes the URL ids — close the workspace panel so the previous chat's
  // files don't linger over a different conversation. Keyed on the URL ids,
  // not every render, so the user can still re-open it within the SAME chat.
  useEffect(() => {
    setFilesSheetOpen(false);
    setPendingFile(null);
  }, [urlSessionId, urlProjectId]);

  // Keep the local sessionTitle in sync with the session list. Unknown
  // sessions (brand-new, not saved yet) fall back to empty so the header
  // can render "New chat".
  useEffect(() => {
    const s = sessions.find((x) => x.id === sessionId);
    setSessionTitle(s?.title || s?.preview || "");
  }, [sessionId, sessions]);

  // Channel of the currently-open session, derived from the sessions
  // list. Brand-new web chats don't have a row yet — the fallback to
  // "web" keeps composing enabled for them. IM sessions get a banner +
  // disabled input because composing here would write to the agent's
  // session but never reach the upstream messenger.
  const currentChannel = useMemo<string>(() => {
    const s = sessions.find((x) => x.id === sessionId);
    return s?.channel || "web";
  }, [sessions, sessionId]);
  // Read-only channel / actAs views fully disable the composer (textarea,
  // attach, send) — the web path can't deliver into upstream IM platforms,
  // and single-user mode no longer needs slash commands typed here, so the
  // conversation is shown pure read-only and the user is pointed to the
  // originating channel to reply.
  const isReadOnlyChannel = currentChannel !== "web";
  const isReadOnlyView = isReadOnlyChannel || isActAsView;
  const inputIsReadOnlySafeSlashCommand = isReadOnlySafeSlashCommand(input);
  const canUseComposer = !!selectedAgent && !isReadOnlyView;
  const canSendComposer =
    canUseComposer && (!isReadOnlyView || inputIsReadOnlySafeSlashCommand);
  const canAttach = !!selectedAgent && !sending && !isReadOnlyView;

  // Chat-only mode: this session sends no tools to the LLM (pure
  // conversation — brainstorming without tool noise). The row carries
  // the flag once it exists; a brand-new chat before its first message
  // has no row, so the toggle lives locally and each send carries
  // params.chatOnly until the row lands and the effect below persists
  // it. Toggling off restores the full tool set on the next turn.
  const [chatOnlyLocal, setChatOnlyLocal] = useState<Record<string, boolean>>({});
  const rowChatOnly = useMemo(
    () => !!sessions.find((x) => x.id === sessionId)?.chatOnly,
    [sessions, sessionId],
  );
  const chatOnly = chatOnlyLocal[sessionId] ?? rowChatOnly;

  const handleToggleChatOnly = useCallback(() => {
    if (!selectedAgent || !sessionId || !canUseComposer) return;
    // Optimistic flip only — the sync effect below is the single writer
    // that persists it (or rolls back by dropping the override when the
    // PUT fails; the row then remains the visible truth).
    setChatOnlyLocal((prev) => ({ ...prev, [sessionId]: !chatOnly }));
  }, [selectedAgent, sessionId, chatOnly, canUseComposer]);

  // Single writer for the chat-only flag: whenever a local toggle meets
  // its session row, push it once (covers the brand-new-chat flow too —
  // first turn creates the row, this persists what the params channel
  // carried) and then drop the override so the row is the only truth
  // left behind. Entries for chats with no row yet (brand-new, nothing
  // sent) simply wait here; they ride params.chatOnly per send.
  useEffect(() => {
    if (!selectedAgent) return;
    for (const [sid, on] of Object.entries(chatOnlyLocal)) {
      const row = sessions.find((x) => x.id === sid);
      if (!row) continue;
      const drop = () =>
        setChatOnlyLocal((prev) => {
          const cp = { ...prev };
          delete cp[sid];
          return cp;
        });
      if (!!row.chatOnly === on) {
        drop(); // already persisted (e.g. by a previous run) — just clear
        continue;
      }
      setChatSessionChatOnly(selectedAgent, sid, on)
        .then(() => loadSessions(selectedAgent))
        .catch(() => {}) // drop reverts the UI to the row's truth
        .finally(drop);
    }
  }, [sessions, chatOnlyLocal, selectedAgent, loadSessions]);

  const handleRenameTitle = useCallback(
    async (next: string) => {
      const trimmed = next.trim();
      if (!trimmed || !selectedAgent || trimmed === sessionTitle) return;
      setSessionTitle(trimmed);
      try {
        await renameChatSession(selectedAgent, sessionId, trimmed);
      } finally {
        loadSessions(selectedAgent);
        // Tell the global sidebar to refetch its Chats list so the new
        // title shows up without a full page reload. AppSidebar's own
        // fetch only re-runs when activeAgentId changes, which doesn't
        // happen on rename.
        if (typeof window !== "undefined") {
          window.dispatchEvent(
            new CustomEvent("fluctio:sessions-changed", {
              detail: { agentId: selectedAgent },
            }),
          );
        }
      }
    },
    [selectedAgent, sessionId, sessionTitle, loadSessions],
  );

  // Render the editable title + the workspace-panel toggle into the
  // global sticky header (the chat container injects whatever JSX it
  // wants here, next to the sidebar toggle). The toggle stays wired
  // even when the panel is open so users can collapse it from the
  // same control they used to expand it.
  const headerSlot = useMemo(
    () => (
      <div className="flex flex-1 items-center justify-between gap-2 min-w-0">
        <ChatHeaderTitle
          title={sessionTitle}
          fallback={t("chat.chatWith", { name: agentName || selectedAgent })}
          onSave={handleRenameTitle}
        />
        <button
          type="button"
          onClick={() => setFilesSheetOpen((v) => !v)}
          className={`shrink-0 inline-flex h-8 w-8 items-center justify-center rounded-md transition-colors ${
            filesSheetOpen
              ? "bg-muted text-foreground"
              : "text-muted-foreground hover:bg-muted/50 hover:text-foreground"
          }`}
          title={filesSheetOpen ? t("chat.hideWorkspace") : t("chat.showWorkspace")}
          aria-pressed={filesSheetOpen}
        >
          <FolderOpen className="h-4 w-4" />
          <span className="sr-only">{t("chat.toggleWorkspace")}</span>
        </button>
      </div>
    ),
    [sessionTitle, agentName, selectedAgent, handleRenameTitle, filesSheetOpen, t],
  );
  usePageHeader(headerSlot, [headerSlot]);

  // Load history when session changes. After rebuilding messages from
  // server history we also re-attach this session's workspace files to
  // the trailing assistant bubble so the "Your files" panel survives a
  // refresh — server history doesn't carry per-turn file diffs, so we
  // approximate by listing everything under sessions/<sid>/ once and
  // hanging it off the last agent message.
  useEffect(() => {
    if (!selectedAgent || !sessionId) return;
    const sessionHasActivePost = inFlightSendSessionRef.current === sessionId;
    // Reset dedup state when session changes — events from a previous
    // session must not bias the new session's seq filter, and any
    // transient placeholder is no longer relevant.
    maxSeqRef.current = -1;
    subscribeSinceRef.current = -1;
    transientBubbleIdRef.current = null;
    // Close the SSE gate for this sessionId; reopens once the history
    // fetch lands and subscribeSinceRef has been set to the real cursor.
    setLoadedSessionId(null);
    // Reset the todo panel on session change so the previous chat's
    // checklist doesn't briefly flash before the new one's fetch lands.
    setTodoItems([]);
    // Same for the subagent progress indicator — never carry it across
    // sessions; a fresh load means no in-flight delegate_task to track.
    setSubagentProgress(null);
    // Refresh todo.md alongside the history fetch. We don't gate the
    // rest of the load on it — a 404 (no todo.md yet) is the normal
    // empty-session case.
    getChatTodo(selectedAgent, sessionId)
      .then((todo) => setTodoItems(todo.items))
      .catch(() => setTodoItems([]));
    // Reset reverse-infinite scroll cursors for the new session.
    earliestSeqRef.current = 0;
    hasMoreRef.current = false;
    loadingMoreRef.current = false;
    setLoadingOlder(false);
    lastMsgSeqRef.current = -1;
    let aborted = false;
    getChatHistoryWithCursor(selectedAgent, sessionId, { limit: HISTORY_PAGE })
      .then(async ({ history, latestEventSeq, earliestSeq, hasMore }) => {
        if (aborted) return;
        if (!sessionHasActivePost && latestEventSeq > maxSeqRef.current) {
          maxSeqRef.current = latestEventSeq;
        }
        subscribeSinceRef.current = latestEventSeq;
        if (!history || history.length === 0) {
          if (!sessionHasActivePost) {
            setMessages([]);
          }
          setLoadedSessionId(sessionId);
          return;
        }
        const built = buildChatMessages(history);
        try {
          // Attribute files by the tool call that produced them, not by
          // filesystem mtime. mtime is wiped to "now" whenever the
          // workspaces dir is copied (docker redeploy, volume migration,
          // backup restore), which used to pile every historical file onto
          // the latest bubble. The tool-call record lives in
          // session_messages and is immune to all of that. Files that no
          // tool claims (exec-script output, etc.) get no bubble — they
          // stay visible in the workspace browser (FilesSheet), which
          // lists listAgentFiles independently of bubble attribution.
          const listedRes = await listAgentFiles(selectedAgent, sessionId);
          const listed = listedRes.files;
          // The backend's scope resolution knows which project this chat
          // belongs to (the URL doesn't) — prefer its prefixes so shared
          // project-root files (math-course/…) attribute correctly too.
          setWsScopePrefixes(listedRes.scopePrefixes);
          const sizeByPath = new Map(listed.map((f) => [f.path, f.size]));
          // Ordinary agents scope project-chat writes to projects/<pid>/<sid>/
          // while coding-root agents land at the project root — try the
          // per-chat subdir first, fall back to the root (see
          // extractProducedFiles). Loose chats have a single layout.
          const scopePrefixes = listedRes.scopePrefixes.length
            ? listedRes.scopePrefixes
            : urlProjectId
              ? [`projects/${urlProjectId}/${sessionId}/`, `projects/${urlProjectId}/`]
              : [`sessions/${sessionId}/`];
          for (let idx = 0; idx < built.length; idx++) {
            const m = built[idx];
            if (m.role !== "tool-group" || !m.toolCalls || m.toolCalls.length === 0) continue;
            const produced = extractProducedFiles(m.toolCalls, scopePrefixes, (p) => sizeByPath.has(p))
              .filter((f) => sizeByPath.has(f.path))
              .map((f) => ({ path: f.path, size: sizeByPath.get(f.path) ?? f.size }));
            if (produced.length > 0) built[idx] = { ...m, files: produced };
          }
        } catch { /* listing failed — fall back to no panel */ }
        if (aborted) return;
        setMessages(built);
        earliestSeqRef.current = earliestSeq;
        hasMoreRef.current = hasMore;
        lastMsgSeqRef.current = latestHistorySeq(history);
        setLoadedSessionId(sessionId);
      })
      .catch(() => {
        if (aborted) return;
        if (!sessionHasActivePost) {
          setMessages([]);
        }
        // History fetch failed — open the SSE anyway so live events
        // still flow, but use seq=0 instead of -1 so we don't trigger a
        // full server-side replay as a side effect.
        subscribeSinceRef.current = 0;
        setLoadedSessionId(sessionId);
      });
    return () => {
      aborted = true;
    };
  }, [selectedAgent, sessionId, urlProjectId]);

  // loadMoreOlder pulls one older page of history when the user scrolls
  // to the top of the message list — reverse-infinite scroll. Prepends
  // the rebuilt messages, then restores the scroll position by the
  // height the new page added; without that compensation the viewport
  // snaps to the new top and the user loses their reading place.
  const loadMoreOlder = useCallback(() => {
    if (loadingMoreRef.current) return;
    if (!hasMoreRef.current) return;
    if (!selectedAgent || !sessionId) return;
    const el = messagesScrollRef.current;
    const prevScrollHeight = el ? el.scrollHeight : 0;
    const prevScrollTop = el ? el.scrollTop : 0;
    // Suspend bottom-stickiness while prepending — the [messages]
    // auto-scroll effect would otherwise fight the position restore.
    const wasStick = stickToBottomRef.current;
    stickToBottomRef.current = false;
    loadingMoreRef.current = true;
    setLoadingOlder(true);
    getChatHistoryWithCursor(selectedAgent, sessionId, { before: earliestSeqRef.current, limit: HISTORY_PAGE })
      .then(({ history, earliestSeq, hasMore }) => {
        // Stale-response guard: the user may have switched sessions while
        // this page was in flight — the closure's selectedAgent/sessionId
        // are the ORIGIN session, and prepending its history into the
        // newly viewed session's list was the reported cross-session
        // pollution. Compare against the live refs.
        if (viewedSessionRef.current !== sessionId || viewedAgentRef.current !== selectedAgent) return;
        if (!history || history.length === 0) {
          hasMoreRef.current = false;
          return;
        }
        const built = buildChatMessages(history);
        setMessages((prev) => [...built, ...prev]);
        earliestSeqRef.current = earliestSeq;
        hasMoreRef.current = hasMore;
        // The list grew upward by (newHeight − prevHeight); push
        // scrollTop down by that delta so the same content stays under
        // the cursor. rAF so the DOM has the new height by then.
        requestAnimationFrame(() => {
          const el2 = messagesScrollRef.current;
          if (!el2) return;
          el2.scrollTop = prevScrollTop + (el2.scrollHeight - prevScrollHeight);
        });
      })
      .catch(() => { /* leave the page as-is; the user can retry */ })
      .finally(() => {
        loadingMoreRef.current = false;
        setLoadingOlder(false);
        stickToBottomRef.current = wasStick;
      });
  }, [selectedAgent, sessionId]);

  // syncNewer is the bottom-of-list mirror of loadMoreOlder: when the
  // user scrolls back to the bottom after reading history, ask the
  // server whether newer messages landed while we were away (SSE dead
  // or asleep, another device/channel replied, cron push). Pulls the
  // latest page and appends only rows beyond lastMsgSeqRef — a
  // MESSAGE-space cursor (session_messages.seq), deliberately distinct
  // from maxSeqRef: that one lives in the session_events counter space
  // the SSE/POST handlers advance, and the two counters are independent
  // (after the 7-day retention prune the event table can sit at -1
  // while messages number in the hundreds — filtering messages against
  // it duplicated the whole page). Skipped while a foreground turn is
  // rendering live (POST stream or a subscribe-replay transient
  // bubble): history would only carry half-persisted rows for that
  // turn anyway.
  const loadingNewerRef = useRef(false);
  const lastNewerCheckRef = useRef(0);
  const lastMsgSeqRef = useRef(-1);
  const syncNewer = useCallback(() => {
    if (loadingNewerRef.current) return;
    if (Date.now() - lastNewerCheckRef.current < 3000) return;
    if (!selectedAgent || !sessionId) return;
    if (inFlightSendSessionRef.current === sessionId) return;
    if (transientBubbleIdRef.current) return;
    loadingNewerRef.current = true;
    lastNewerCheckRef.current = Date.now();
    // Cheap precheck first: one row (latestEventSeq is event-space and
    // deliberately not used as the gate). Nothing-new is the common case
    // — SSE alive (events already rendered) or a jitter-triggered
    // transition — and a full page carries 50 rows of tool-result
    // content, so only pay that when the newest row is genuinely beyond
    // what we've rendered.
    getChatHistoryWithCursor(selectedAgent, sessionId, { limit: 1 })
      .then((pre) => {
        // Stale-response guard (same rationale as loadMoreOlder).
        if (viewedSessionRef.current !== sessionId || viewedAgentRef.current !== selectedAgent) return null;
        if (latestHistorySeq(pre.history || []) <= lastMsgSeqRef.current) return null;
        return getChatHistoryWithCursor(selectedAgent, sessionId, { limit: HISTORY_PAGE });
      })
      .then((res) => {
        if (!res) return;
        // Second leg of the stale guard: the full page fetch may also
        // belong to an abandoned session.
        if (viewedSessionRef.current !== sessionId || viewedAgentRef.current !== selectedAgent) return;
        // Filter BEFORE advancing the cursor, or everything dedupes away.
        const fresh = buildChatMessages(res.history || []).filter(
          (m) => typeof m.seq === "number" && m.seq > lastMsgSeqRef.current,
        );
        const top = latestHistorySeq(res.history || []);
        if (top > lastMsgSeqRef.current) lastMsgSeqRef.current = top;
        // Keep the SSE resume cursor fresh too (event space) — harmless
        // when it lags, saves a reconnecting EventSource some replay.
        if (typeof res.latestEventSeq === "number" && res.latestEventSeq > maxSeqRef.current) {
          maxSeqRef.current = res.latestEventSeq;
          subscribeSinceRef.current = res.latestEventSeq;
        }
        if (fresh.length > 0) setMessages((prev) => [...prev, ...fresh]);
      })
      .catch(() => { /* offline or session gone — nothing to sync */ })
      .finally(() => {
        loadingNewerRef.current = false;
      });
  }, [selectedAgent, sessionId]);

  useEffect(() => {
    if (!stickToBottomRef.current) return;
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

  // Deep-link from the daily-diary #seq-N anchor: history renders
  // asynchronously after navigation, so the browser's native hash-scroll
  // fires before the target bubble exists. Poll briefly for the element,
  // then center it — and yield stickToBottom so the bottom-auto-scroll
  // doesn't yank the view away.
  useEffect(() => {
    const hash = typeof window !== "undefined" ? window.location.hash : "";
    if (!hash.startsWith("#seq-")) return;
    const id = hash.slice(1);
    let tries = 0;
    const iv = setInterval(() => {
      const el = document.getElementById(id);
      if (el) {
        clearInterval(iv);
        stickToBottomRef.current = false;
        el.scrollIntoView({ behavior: "smooth", block: "center" });
        return;
      }
      if (++tries > 25) clearInterval(iv); // ~5s, then give up
    }, 200);
    return () => clearInterval(iv);
  }, [sessionId]);

  // Watch the scroll container so we know whether to keep auto-scrolling
  // as new content arrives. 64px slack absorbs streaming jitter — without
  // it, a single token append can push the bottom past the viewport and
  // flip us off "sticky" for one tick before the next auto-scroll fires.
  useEffect(() => {
    const el = messagesScrollRef.current;
    if (!el) return;
    const onScroll = () => {
      const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
      const wasSticky = stickToBottomRef.current;
      stickToBottomRef.current = distance <= 64;
      // Reverse-infinite scroll: near the top, pull an older page.
      if (el.scrollTop < 60) loadMoreOlder();
      // Coming back to the bottom after reading history — check the
      // server for messages missed while scrolled up. Fires only on the
      // unsticky→sticky transition so streaming output at the bottom
      // (sticky→sticky) can't retrigger it.
      if (!wasSticky && stickToBottomRef.current) syncNewer();
    };
    el.addEventListener("scroll", onScroll, { passive: true });
    return () => el.removeEventListener("scroll", onScroll);
  }, [loadMoreOlder, syncNewer]);

  useEffect(() => {
    const el = textareaRef.current;
    if (el) {
      el.style.height = "auto";
      el.style.height = Math.min(el.scrollHeight, 200) + "px";
    }
  }, [input]);

  // applySteerEvent renders a server "steer" echo as a user bubble,
  // reconciling against the optimistic pendingSteer bubble (if any) so
  // the message isn't duplicated. seq-dedup is already applied upstream
  // by both event consumers.
  const applySteerEvent = useCallback((content: string) => {
    if (!content) return;
    setMessages((prev) => {
      const idx = prev.findIndex(
        (m) => m.role === "user" && m.pendingSteer && m.content === content,
      );
      if (idx >= 0) {
        const updated = [...prev];
        updated[idx] = { ...updated[idx], pendingSteer: undefined };
        return updated;
      }
      return [
        ...prev,
        { id: `s-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`, role: "user", content, timestamp: Date.now() },
      ];
    });
  }, []);

  const handleSend = useCallback(async (overrideText?: string, force?: boolean) => {
    // overrideText lets caller post a message that didn't come from
    // the composer (e.g. the plan-approval button clicking "go"). When
    // present it bypasses the input field entirely — composer state
    // stays as the user left it. force bypasses the in-flight guard;
    // used by the steer 409 fallback (server confirmed no active turn).
    const composerText = (overrideText ?? input).trim();
    const text = composerText;
    // Consume the rollback flag eagerly: even if this send throws or
    // the user abandons it, the next plain send must NOT carry a stale
    // rollback (would delete an unrelated later user turn). handleRetry
    // re-sets it on the next failed-message resend.
    const rollbackId = pendingRollbackRef.current;
    pendingRollbackRef.current = null;
    const rollbackPending = rollbackId !== null;
    const slashAllowedInReadOnlyView = isReadOnlySafeSlashCommand(text);
    // Allow sending with attachments only (no text), but require at least one.
    if (
      (!text && attachments.length === 0) ||
      !selectedAgent ||
      (sending && !force) ||
      (isReadOnlyView && !slashAllowedInReadOnlyView)
    ) {
      return;
    }
    setSlashOpen(false);

    // `/project/<pid>` is the lazy-create marker the sidebar dropped
    // us at. Captured here so it can ride the first chat request body;
    // once the session row exists, project_id is on the row and the URL
    // drops back to the bare chat form.
    const projectIdHint = urlProjectId;

    // Pin the sessionId into the URL on the first send so a refresh
    // keeps the user in the same conversation. We use the native
    // History API instead of `router.replace` because output:'export'
    // only pre-renders the `_` placeholder for /chat/[session]; a
    // router.replace to the real sid triggers an RSC fetch that the
    // SPA fallback can't satisfy, and Next falls back to a hard
    // window.location navigation — which kills the in-flight stream
    // we're about to start. Next 16's app-router patches
    // history.replaceState to dispatch ACTION_RESTORE, so usePathname /
    // useSearchParams (and the sidebar's navigateOnce dedupe that
    // derives from them) still see the new URL.
    const target = `/agents/${selectedAgent}/chat/${sessionId}/`;
    inFlightSendSessionRef.current = sessionId;
    if (pathname !== target) {
      window.history.replaceState(null, "", target);
    }

    // Upload attachments first so the agent can read them by name on its
    // first turn. Files land at sessions/<sid>/<basename> in the workspace
    // store, which is the same dir the docker sandbox bind-mounts as
    // /workspace. We also (a) build user-bubble preview metadata so images
    // render inline in the user's bubble without waiting on the server
    // round-trip, and (b) read images as data URLs so vision-capable
    // models receive them as image_url content parts.
    const filesToUpload = attachments;
    setAttachments([]);

    let userBubbleAttachments: UserAttachment[] = [];
    let imageDataUrls: string[] = [];

    if (filesToUpload.length > 0) {
      userBubbleAttachments = filesToUpload.map((f) => ({
        name: f.name,
        isImage: f.type.startsWith("image/"),
        previewUrl: f.type.startsWith("image/") ? URL.createObjectURL(f) : undefined,
      }));

      try {
        await uploadAgentFiles(selectedAgent, sessionId, filesToUpload, projectIdHint || undefined);
      } catch (err) {
        setMessages((prev) => [
          ...prev,
          { id: `e-${Date.now()}`, role: "agent", content: `File upload failed: ${err instanceof Error ? err.message : "unknown error"}`, timestamp: Date.now() },
        ]);
        return;
      }

      // Read each image as a base64 data URL. We do this AFTER upload —
      // upload only needs the File object; data URL conversion is for the
      // provider call. Done in parallel for snappy UX on multi-attach.
      imageDataUrls = (
        await Promise.all(
          filesToUpload.map(async (f) => {
            if (!f.type.startsWith("image/")) return null;
            return await new Promise<string | null>((resolve) => {
              const reader = new FileReader();
              reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : null);
              reader.onerror = () => resolve(null);
              reader.readAsDataURL(f);
            });
          }),
        )
      ).filter((s): s is string => !!s);
    }
    // Build the prompt actually sent to the model. Images travel as
    // `imageUrls` for vision, but the model also needs the on-disk path
    // for skills like image-tool that take `input: "/workspace/<file>"`.
    // We prepend `[Attached: /workspace/<name>]` lines for that — the
    // server's StripAttachedPrefix removes them on history read so user
    // bubbles, page titles, and sidebar previews stay clean.
    //
    // Filenames are client-controlled (user upload) and land in markdown
    // that gets rendered back into the bubble — a name containing `](``
    // could close the breadcrumb and open an arbitrary markdown link.
    // Strip markdown-significant characters from the path segment; the
    // server-side path uses the same sanitized name, so the reference
    // still resolves.
    const sanitizeUploadedName = (name: string) => name.replace(/[[\]()#`*_[!]/g, "_");
    const attachedPaths = filesToUpload.map((f) => `/workspace/${sanitizeUploadedName(f.name)}`);
    const breadcrumb = attachedPaths
      .map((p) => `[Attached: ${p}]`)
      .join("\n");
    const fullText = breadcrumb
      ? (text ? `${breadcrumb}\n${text}` : breadcrumb)
      : text;

    // Only clear the composer when the send came from it. Override
    // sends (plan-approval button etc.) leave whatever the user was
    // typing alone.
    if (overrideText === undefined) {
      setInput("");
    }
    // Sending always means "I want to see what happens next" — re-pin
    // to bottom even if the user had scrolled up to read earlier in the
    // conversation.
    stickToBottomRef.current = true;
    // Resend of a trailing failed user turn: the server is rolling back
    // the orphan user row (rollbackPending), so appendUserTurn also drops
    // the stale failed user bubble AND any trailing error bubbles before
    // adding the fresh one — otherwise the UI shows both until a refresh.
    setMessages((prev) => appendUserTurn(prev, rollbackId, text, userBubbleAttachments));
    setSendingSessions((prev) => {
      const next = new Set(prev);
      next.add(sessionId);
      return next;
    });
    const ac = new AbortController();
    abortMapRef.current.set(sessionId, ac);

    // Snapshot the workspace before the turn so we can diff at `done` and
    // attach newly-created / modified files (PDFs, images, …) to the
    // final reply. Fire-and-forget; if the snapshot fails we just won't
    // surface files this turn. `path → size|modTime` key.
    const preTurnFilesPromise = listAgentFiles(selectedAgent)
      .then(({ files: items }) => {
        const m = new Map<string, string>();
        for (const f of items) m.set(f.path, `${f.size}|${f.modTime}`);
        return m;
      })
      .catch(() => new Map<string, string>());

    // One fresh accumulation state per turn. The factory owns the whole
    // streaming switch — delta flush, content seal, tool-group rounds,
    // notice pills — plus the seq-dedup and session-switch guards.
    const turn: TurnState = { curGroupId: "", curCalls: [], turnFiles: [], seenPaths: new Set() };
    const onStreamEvent = createTurnEventHandler({
      agentId: selectedAgent,
      sessionId,
      projectIdHint,
      streamingMsgIdRef,
      pendingDeltaRef,
      deltaFlushTimerRef,
      maxSeqRef,
      viewedSessionRef,
      wsPrefixesRef,
      turn,
      actions: {
        setMessages,
        setReconnecting,
        setSubagentProgress,
        applySteerEvent,
        handleNewChat,
        loadSessions,
        refreshTodo: () => {
          getChatTodo(selectedAgent, sessionId)
            .then((todo) => setTodoItems(todo.items))
            .catch(() => {});
        },
        t,
      },
    });

    try {
      await sendChatStream(selectedAgent, sessionId, fullText, onStreamEvent, ac.signal, imageDataUrls, projectIdHint, chatOnly ? { chatOnly: true } : undefined, rollbackPending);
      // The streamed turn's bubbles carry no seq (content_delta path), so
      // the message-space cursor lastMsgSeqRef is stale until re-anchored —
      // a scroll-to-bottom syncNewer right after would re-append the whole
      // turn. One single-row fetch fixes that; it also covers error turns,
      // whose only persisted row is the user message.
      getChatHistoryWithCursor(selectedAgent, sessionId, { limit: 1 })
        .then((res) => {
          const top = latestHistorySeq(res.history || []);
          if (top > lastMsgSeqRef.current) lastMsgSeqRef.current = top;
        })
        .catch(() => {});
      // Diff the workspace against the pre-turn snapshot to detect files
      // produced by *exec* (a Python script that saves PDFs, etc.) — these
      // have no tool-call record to anchor them to a bubble, so by design
      // they don't enter the message panel; they're surfaced only via the
      // workspace browser (FilesSheet), which lists listAgentFiles
      // independently. We still compute the diff for the diagnostic log
      // below (it tells us whether a missing bubble is because the file
      // wasn't produced vs. the tool record didn't capture it).
      const postTurnFiles = (await listAgentFiles(selectedAgent).catch(() => null))?.files ?? [];
      const preSnap = await preTurnFilesPromise;
      // Bubbles carry only tool-produced files (turnFiles). exec output
      // (diffFiles) intentionally stays out of bubbles — it's dev-log
      // diagnostics only. reconcileTurnFiles also rewrites each chip to
      // the authoritative listed path (the live prefix guess can't know
      // the store layout) and dedupes.
      const wsRoot = wsPrefixesRef.current.length
        ? wsPrefixesRef.current[wsPrefixesRef.current.length - 1]
        : "";
      const { diffFiles, allFiles } = reconcileTurnFiles({
        postTurnFiles,
        preSnap,
        turnFiles: turn.turnFiles,
        seenPaths: turn.seenPaths,
        sessionId,
        projectIdHint,
        wsRoot,
      });
      // Diagnostic: when sandbox-exec produces a file but the Files
      // panel doesn't show, we need to know whether the API returned
      // the file at all and where the diff dropped it. Dev-only — a
      // shared/demo deployment shouldn't leak the workspace structure
      // (file paths, names, counts) into the browser console.
      if (process.env.NODE_ENV !== "production") {
        console.log("[chat] post-turn files diff", {
          agent: selectedAgent,
          sessionId,
          preSnapSize: preSnap.size,
          postTurnCount: postTurnFiles.length,
          postTurnPaths: postTurnFiles.map((f) => f.path),
          turnFiles: turn.turnFiles,
          diffFiles,
          attached: allFiles.length,
        });
      }
      if (allFiles.length > 0) {
        if (process.env.NODE_ENV !== "production") {
          console.log("[chat] attaching files to last message", { files: allFiles });
        }
        setMessages((prev) => attachFilesToLast(prev, allFiles));
      }
      loadSessions(selectedAgent);
      // First-turn of a brand-new session just got persisted — tell the
      // global sidebar to refetch its Chats list so the new title shows
      // up without a full page reload.
      if (typeof window !== "undefined") {
        window.dispatchEvent(
          new CustomEvent("fluctio:sessions-changed", {
            detail: { agentId: selectedAgent },
          }),
        );
      }
    } catch (err) {
      // AbortError from the user clicking Stop is expected — surface a
      // brief "Stopped" line so they see the cancellation took effect,
      // not a generic failure message.
      const isAbort = err instanceof DOMException && err.name === "AbortError";
      // Surface the underlying error in DevTools so future "Failed to
      // get a response" reports come with a concrete cause (network,
      // parse, post-turn fetch, …) rather than the generic message.
      if (typeof console !== "undefined") {
        console.error("[chat] handleSend error", err);
      }
      // Keyboard-stack abort + post-stream tear-down can both throw an
      // AbortError after a successful turn (the SSE reader is
      // released on `done`, then a stray reader.cancel() races with a
      // late server EOF and surfaces as one). Both look identical to
      // user-pressed-Stop here, so we additionally suppress the
      // toast when at least one agent reply already landed for this
      // turn — the user just got their answer; we shouldn't tack on
      // a confusing failure bubble.
      if (isAbort) {
        // Resolve any in-flight tools in the current tool-group so they
        // stop spinning. Server-side padOrphanToolResults will write a
        // matching record on its end; this just keeps the UI consistent
        // until the next history fetch overwrites it.
        setMessages(markStoppedTools);
        setMessages((prev) => [...prev, stoppedNotice()]);
      } else {
        // appendTurnFailure suppresses the bubble when the turn already
        // produced visible output after the last user message.
        setMessages((prev) => appendTurnFailure(prev, err));
      }
    } finally {
      // Cancel a pending batched flush — the turn is over and the final
      // `content` event (or the error path) already settled the text.
      if (deltaFlushTimerRef.current !== null) {
        clearTimeout(deltaFlushTimerRef.current);
        deltaFlushTimerRef.current = null;
      }
      pendingDeltaRef.current = "";
      if (inFlightSendSessionRef.current === sessionId) {
        inFlightSendSessionRef.current = null;
      }
      // Per-session cleanup: only THIS session's controller and sending
      // flag are retired — the old global reset let one session's turn
      // end flip another session's Stop button and composer state. The
      // map entry drop also releases the controller for GC.
      abortMapRef.current.delete(sessionId);
      setSendingSessions((prev) => {
        if (!prev.has(sessionId)) return prev;
        const next = new Set(prev);
        next.delete(sessionId);
        return next;
      });
      // Belt-and-suspenders: the subagent's done event clears this on
      // the happy path, but if a network blip drops that event we don't
      // want a stale "iteration 5/20" sitting under a finished turn.
      setSubagentProgress(null);
      // Re-fetch todo.md at turn end as a defensive resync. The archive
      // lifecycle (prompt_modules.go) has the agent clear todo.md via
      // write_file once the plan is terminal; the per-tool_result refetch
      // at ~L1959 already covers that path. This catch-all covers any
      // path that slips past the whitelist (a future tool, a manual FS
      // change, a dropped event) so todoItems never pins to a stale
      // snapshot that would reshow the panel next turn via `sending`.
      getChatTodo(selectedAgent, sessionId)
        .then((todo) => setTodoItems(todo.items))
        .catch(() => {});
      textareaRef.current?.focus();
    }
  }, [input, attachments, selectedAgent, sessionId, sending, isReadOnlyView, isReadOnlySafeSlashCommand, loadSessions, pathname, router, urlProjectId, chatOnly]);

  const handleStop = useCallback(() => {
    // Ask the server to cancel the running turn first — aborting the
    // fetch alone no longer stops it (disconnects now deliberately let
    // turns finish server-side so refreshes/network blips don't kill
    // work mid-tool). The abort then ends the local stream read and
    // shows "(Stopped)". Fire-and-forget: even if the stop call races
    // or fails, the abort still unblocks the UI.
    if (selectedAgent && sessionId) {
      stopChat(selectedAgent, sessionId, urlProjectId).catch(() => {});
    }
    abortMapRef.current.get(sessionId)?.abort();
  }, [selectedAgent, sessionId, urlProjectId]);

  // handleSteer fires while a turn is streaming: it buffers the message
  // into the running turn (the agent folds it in between tool rounds and
  // streams a "steer" echo on the existing SSE). On 409 (no active turn
  // — the turn just ended) it falls back to a normal send so nothing is
  // lost.
  const handleSteer = useCallback(async (overrideText?: string) => {
    const text = (overrideText ?? input).trim();
    // Called from handleKeyDown's `if (sending)` branch, or from an
    // approval button tapped mid-stream (overrideText set to /yes etc.).
    // Within one render React state is snapshot-consistent, so `sending`
    // is necessarily true here.
    if (!text || !selectedAgent || !sending) return;
    // Read-only views (actAs viewer, IM read-only) must not steer: the
    // composer sends would have been blocked by the same isReadOnlyView
    // check handleSend applies, but the mid-stream path (Enter while
    // streaming, approval buttons) skipped it — users got a raw
    // "Steer failed: 403" from the server middleware instead.
    if (isReadOnlyView) return;
    if (!overrideText) setInput("");
    const optimisticId = `s-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
    setMessages((prev) => [
      ...prev,
      { id: optimisticId, role: "user", content: text, timestamp: Date.now(), pendingSteer: true },
    ]);
    let ok = false;
    try {
      ok = await steerChat(selectedAgent, sessionId, text, urlProjectId);
    } catch (err) {
      setMessages((prev) => [
        ...prev.filter((m) => m.id !== optimisticId),
        { id: `e-${Date.now()}`, role: "agent", content: `Steer failed: ${err instanceof Error ? err.message : "unknown error"}`, timestamp: Date.now() },
      ]);
      return;
    }
    if (!ok) {
      setMessages((prev) => prev.filter((m) => m.id !== optimisticId));
      await handleSend(text, true);
    }
  }, [input, selectedAgent, sending, sessionId, urlProjectId, handleSend, isReadOnlyView]);

  const handleFilePick = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const picked = e.target.files;
    if (!picked || picked.length === 0) return;
    // Snapshot the FileList into a stable File[] BEFORE we reset the
    // input. FileList is tied to the input element — setting value=""
    // empties it. Under StrictMode React invokes the setState updater
    // twice for purity checks; if the closure references the live
    // FileList, the second invocation sees an empty list and the state
    // ends up empty even though the user picked a file.
    const newFiles = Array.from(picked);
    e.target.value = "";
    setAttachments((prev) => [...prev, ...newFiles]);
  }, []);

  const removeAttachment = useCallback((idx: number) => {
    setAttachments((prev) => prev.filter((_, i) => i !== idx));
  }, []);

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // Don't submit while an IME composition is active — Enter in that state
    // is the user confirming the IME candidate (e.g. pinyin → 好), not
    // sending the message. keyCode 229 also signals "composing" on some
    // browsers where isComposing isn't set.
    if (e.nativeEvent.isComposing || e.keyCode === 229) return;

    // Slash menu keyboard handling takes precedence when open: arrows move
    // the selection, Enter confirms, Escape closes without sending.
    if (slashOpen && filteredItems.length > 0 && !isExactBuiltInSlashCommand(input)) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setSlashIndex((i) => (i + 1) % filteredItems.length);
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setSlashIndex((i) => (i - 1 + filteredItems.length) % filteredItems.length);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        selectItem(filteredItems[slashIndex]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setSlashOpen(false);
        return;
      }
    }

    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      // While a turn is streaming, Enter steers the running turn instead
      // of being blocked; otherwise it's a normal send.
      if (sending) {
        handleSteer();
      } else {
        handleSend();
      }
    }
  };

  // onChange wrapper: update input + slash menu visibility in one pass.
  const handleInputChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const next = e.target.value;
    setInput(next);
    const caret = e.target.selectionStart ?? next.length;
    const ctx = slashContext(next, caret);
    if (ctx) {
      setSlashOpen(true);
      setSlashQuery(ctx.query);
      setSlashIndex(0);
    } else {
      setSlashOpen(false);
    }
  };

  const handleCopy = (msg: ChatMessage) => {
    // clipboard.writeText rejects on non-secure origins or when the
    // permission is denied — an unhandled rejection used to surface as a
    // console error with the UI still flashing "copied".
    navigator.clipboard.writeText(msg.content).catch(() => {
      // Non-secure origin fallback: the classic textarea+execCommand path.
      const ta = document.createElement("textarea");
      ta.value = msg.content;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try {
        document.execCommand("copy");
      } catch {
        // Give up silently — the bubble stays readable either way.
      }
      document.body.removeChild(ta);
    });
    setCopiedId(msg.id);
    setTimeout(() => setCopiedId(null), 1500);
  };

  // handleRetry refills the composer with this message's content so the
  // user can verify/edit before resending. Deliberately not auto-sending —
  // a one-click resend that quietly discards the existing agent reply is
  // too easy to fire by accident. When the retried message is the
  // trailing failed user turn (no successful assistant reply after it,
  // only optional error bubbles), the next send also signals the server
  // to roll that orphan row back so the LLM sees one copy, not two.
  // Middle-of-history retries (a user turn with a real reply after it)
  // stay as a plain refill for now; fork is a later feature.
  const handleRetry = (msg: ChatMessage) => {
    setInput(msg.content);
    const idx = messages.findIndex((m) => m.id === msg.id);
    const isFailedTail =
      idx >= 0 &&
      msg.role === "user" &&
      messages.slice(idx + 1).every((m) => m.id.startsWith("e-"));
    pendingRollbackRef.current = isFailedTail ? msg.id : null;
    setTimeout(() => {
      const el = textareaRef.current;
      if (el) {
        el.focus();
        const end = el.value.length;
        el.setSelectionRange(end, end);
      }
    }, 0);
  };

  const handleNewChat = () => {
    const newId = generateSessionId();
    setSessionId(newId);
    setMessages([]);
    router.replace(`/agents/${selectedAgent}/chat/`);
  };

  const handleSelectSession = (sid: string) => {
    setSessionId(sid);
    // history.replaceState (not router.replace) for the same reason as
    // handleSend: /chat/[session] is only pre-rendered for the `_`
    // placeholder under output:'export', so router-driven navigation to
    // a real sid hard-reloads. See the longer note in handleSend.
    window.history.replaceState(null, "", `/agents/${selectedAgent}/chat/${sid}/`);
  };

  // handleFork spins up a new session B from the assistant turn `msg`.
  // B inherits this session's archive[0..msg.seq] as a read-only prefix
  // and opens empty; we switch to it immediately. Inherited turns (seq<0
  // in the merged history) carry no fork button — only B's own turns do.
  const handleFork = async (msg: ChatMessage) => {
    if (!selectedAgent || !sessionId) return;
    if (msg.seq == null || msg.seq < 0) return;
    try {
      const res = await forkChatSession(selectedAgent, sessionId, msg.seq);
      if (res.sessionKey) {
        setSessionId(res.sessionKey);
        setMessages([]);
        window.history.replaceState(null, "", `/agents/${selectedAgent}/chat/${res.sessionKey}/`);
        loadSessions(selectedAgent);
      } else {
        console.warn("fork failed", res.error);
      }
    } catch (e) {
      console.warn("fork error", e);
    }
  };

  // Day-divider label: relative word for today/yesterday, else a localized
  // long date ("8月12日 周三" / "Aug 12, Wed") for sessions spanning days.
  const formatDayDivider = (ts: number) => {
    const d = new Date(ts);
    const today = new Date();
    if (d.toDateString() === today.toDateString()) return t("chatScreen.today");
    if (d.toDateString() === new Date(today.getTime() - 86400_000).toDateString())
      return t("chatScreen.yesterday");
    return d.toLocaleDateString([], { month: "long", day: "numeric", weekday: "short" });
  };

  // Empty new-chat state: collapse the messages scroll out of the
  // flex-1 lane and center title + composer vertically, Manus-style.
  // Once any message exists the layout swings back to the standard
  // "scroll above, sticky composer at bottom" shape.
  const isEmpty = messages.length === 0;
  // Compute the id of the latest agent bubble that's a pending plan
  // (numbered plan + "Reply `go` to execute" footer), only when no
  // user message has followed it. This is the single bubble that gets
  // the inline approve/cancel buttons; older plans further up the
  // history never get buttons re-rendered on them.
  const pendingPlanId: string | null = (() => {
    if (sending) return null;
    for (let i = messages.length - 1; i >= 0; i--) {
      const m = messages[i];
      if (m.role === "user") return null; // user already replied → plan is no longer pending
      if (m.role === "agent" && isPendingPlanContent(m.content)) return m.id;
    }
    return null;
  })();
  // Hero title is the same on both the bare agent home and a project
  // landing page — Manus-style "what can I do" prompt. Project pages
  // render a small info card UNDER the hero (folder + name + meta)
  // instead of taking over the headline, so users always know which
  // agent they're chatting with first.
  const heroTitle = t("chat.heroTitle");

  return (
    <div className="flex h-[calc(100vh-3rem)] flex-row">
      <div
        className={
          "flex flex-1 min-w-0 flex-col" +
          // pb-12 (3rem) matches the header height we already subtracted
          // from the parent's h-[calc(100vh-3rem)]. Without it `justify-
          // center` centers content inside the post-header area, which
          // sits visually ~24px below the true viewport mid-line — the
          // user notices the hero + composer pair drifting low. Adding
          // an equal bottom padding biases the centered group upward by
          // half the header height so the optical centre lines up with
          // the geometric centre of the screen.
          // Empty state now also carries the dashboard board below the
          // composer: `safe_center` keeps the Manus-style centering while
          // the content fits, and falls back to start-aligned scrolling
          // (instead of clipping the top) when the board overflows the
          // viewport — plain justify-center would cut the hero off-screen.
          (isEmpty
            ? " [justify-content:safe_center] overflow-y-auto pb-12"
            : "")
        }
      >
      {/* Messages */}
        <div
          ref={messagesScrollRef}
          className={
            // scrollbar-gutter:stable always reserves the 6px scrollbar track
            // so message rows keep a fixed content width that lines up with the
            // composer below (which gets a matching right inset) — otherwise the
            // scrollbar shifts message edges out of alignment on a narrow panel.
            "min-h-0 px-4 [scrollbar-gutter:stable] " +
            (isEmpty ? "shrink-0" : "flex-1 overflow-y-auto py-4")
          }
        >
          <div className="mx-auto max-w-2xl space-y-3">
            {isEmpty && (
              <div className="pb-10 pt-8 text-center motion-safe:animate-in motion-safe:fade-in motion-safe:slide-in-from-bottom-1 motion-safe:duration-200">
                <h1 className="text-3xl md:text-4xl font-semibold tracking-tight">
                  {heroTitle}
                </h1>
              </div>
            )}

            {/* Reverse-infinite scroll: spinner while an older page loads. */}
            {loadingOlder && (
              <div className="flex justify-center py-2">
                <RefreshCw className="h-3.5 w-3.5 animate-spin text-muted-foreground" />
              </div>
            )}

            {(() => {
              // Tool-group artefacts (e.g. an image rendered to base64 by
              // a Python script inside the sandbox) are attached to the
              // *next* agent reply bubble — so they appear as part of the
              // assistant's answer, not inside the tool panel. If no agent
              // reply follows (tool still running or chain didn't finish),
              // we skip surfacing the image; it will show up with the next
              // reply. `surfacedSrcs` tracks every image src we've
              // surfaced so bubbles can suppress duplicate inline copies.
              // Tool-output images are NOT surfaced on the agent bubble —
              // only images the LLM writes into its own final reply are
              // rendered (via renderContentWithDataImages in
              // renderRegularBubble). Raw tool result bytes stay inside the
              // collapsible tool panel until the model chooses to surface
              // them in its reply, instead of being forwarded unprocessed.
              const attachedImages = new Map<string, Array<{ alt: string; src: string }>>();
              const surfacedSrcs = new Set<string>();
              // Calendar day of the last rendered regular message — drives
              // the day divider so long-lived (IM-synced) sessions don't
              // read as one undifferentiated stream.
              let lastDay: string | null = null;
              // Walk messages once so we can bundle consecutive
              // tool-group rounds into a single collapsible. Without
              // this, a long ReAct turn with seven sequential rounds
              // produces seven independently-collapsible boxes that
              // dominate the chat — the bundle hides them behind one
              // header until the user actually wants to dive in.
              const elements: React.ReactNode[] = [];
              for (let i = 0; i < messages.length; i++) {
                const msg = messages[i];
                // Compaction notice — render as a centered, muted pill
                // bubble instead of a normal user/agent message.
                if (msg.role === "notice") {
                  // Turn failures carry an actionable cause and deserve the
                  // destructive accent; everything else stays muted.
                  const isError = msg.kind === "turn_error_notice";
                  elements.push(
                    <div key={msg.id} className="flex justify-center my-3">
                      <span
                        className={`text-xs rounded-full px-3 py-1 ${
                          isError
                            ? "text-destructive bg-destructive/10"
                            : "text-muted-foreground bg-muted/40"
                        }`}
                      >
                        {msg.content}
                      </span>
                    </div>,
                  );
                  continue;
                }
                if (msg.role === "tool-group") {
                  const start = i;
                  while (
                    i + 1 < messages.length &&
                    messages[i + 1].role === "tool-group"
                  ) {
                    i++;
                  }
                  const rounds = messages.slice(start, i + 1);
                  // Keep the surfacing of any per-round produced files
                  // out here so each round's panel still renders below
                  // the bundle in chronological order.
                  const filePanels = rounds
                    .filter((r) => r.files && r.files.length > 0)
                    .map((r) => (
                      <FilesPanel
                        key={`files-${r.id}`}
                        agentId={selectedAgent}
                        files={r.files!}
                        onOpenFile={(f) => { setPendingFile(f); setFilesSheetOpen(true); }}
                      />
                    ));
                  if (rounds.length === 1) {
                    elements.push(
                      <div key={rounds[0].id}>
                        <ToolCallGroup
                          msg={rounds[0]}
                          surfacedSrcs={surfacedSrcs}
                          agentId={selectedAgent}
                          sessionId={sessionId}
                          workspaceRoot={workspaceRoot}
                          subagentProgress={subagentProgress}
                        />
                        {filePanels}
                      </div>,
                    );
                  } else {
                    elements.push(
                      <div key={`bundle-${rounds[0].id}`}>
                        <ToolRoundsBundle
                          rounds={rounds}
                          surfacedSrcs={surfacedSrcs}
                          agentId={selectedAgent}
                          sessionId={sessionId}
                          workspaceRoot={workspaceRoot}
                          subagentProgress={subagentProgress}
                        />
                        {filePanels}
                      </div>,
                    );
                  }
                  continue;
                }
                // Day divider before regular bubbles that start a new
                // calendar day (transitions only — notices and tool
                // groups don't own a conversation timestamp).
                if (msg.role === "user" || msg.role === "agent") {
                  const day = new Date(msg.timestamp).toDateString();
                  if (lastDay && day !== lastDay) {
                    elements.push(
                      <div key={`day-${msg.id}`} className={`flex justify-center my-3 ${msg.timestamp > liveAfterRef.current ? "msg-in" : ""}`}>
                        <span className="text-xs text-muted-foreground bg-muted/40 rounded-full px-3 py-1">
                          {formatDayDivider(msg.timestamp)}
                        </span>
                      </div>,
                    );
                  }
                  lastDay = day;
                }
                // Agent bubbles may carry the `<|split|>` marker the
                // LLM emits for multi-bubble output (mirrors IM channel
                // behavior). Expand into one bubble per chunk so the
                // marker never surfaces as literal text. Attach files /
                // metadata only to the last chunk to match the IM
                // dispatcher's "attach to last chunk" rule.
                if (msg.role === "agent" && msg.content.includes(SPLIT_MARKER)) {
                  const parts = splitOnMarker(msg.content);
                  parts.forEach((part, idx) => {
                    const isLast = idx === parts.length - 1;
                    elements.push(
                      <RegularBubble
                        key={`${msg.id}-s${idx}`}
                        msg={{ ...msg, id: `${msg.id}-s${idx}`, content: part }}
                        selectedAgent={selectedAgent}
                        sessionId={sessionId}
                        workspaceRoot={workspaceRoot}
                        sending={sending}
                        copiedId={copiedId}
                        liveAfter={liveAfterRef.current}
                        attachedImages={attachedImages}
                        surfacedSrcs={surfacedSrcs}
                        suppressAllInlineImages={false}
                        pendingPlanId={pendingPlanId}
                        t={t}
                        onQuickReply={(text) => (sending ? handleSteer(text) : handleSend(text))}
                        onCopy={handleCopy}
                        onRetry={handleRetry}
                        onFork={handleFork}
                        onOpenFile={(f) => {
                          setPendingFile(f);
                          setFilesSheetOpen(true);
                        }}
                        onOpenFilesSheet={() => setFilesSheetOpen(true)}
                        onPlanEdit={() => {
                          setInput("");
                          textareaRef.current?.focus();
                        }}
                      />,
                    );
                  });
                  continue;
                }
                elements.push(
                  <RegularBubble
                    key={msg.id}
                    msg={msg}
                    selectedAgent={selectedAgent}
                    sessionId={sessionId}
                    workspaceRoot={workspaceRoot}
                    sending={sending}
                    copiedId={copiedId}
                    liveAfter={liveAfterRef.current}
                    attachedImages={attachedImages}
                    surfacedSrcs={surfacedSrcs}
                    suppressAllInlineImages={false}
                    pendingPlanId={pendingPlanId}
                    t={t}
                    onQuickReply={(text) => (sending ? handleSteer(text) : handleSend(text))}
                    onCopy={handleCopy}
                    onRetry={handleRetry}
                    onFork={handleFork}
                    onOpenFile={(f) => {
                      setPendingFile(f);
                      setFilesSheetOpen(true);
                    }}
                    onOpenFilesSheet={() => setFilesSheetOpen(true)}
                    onPlanEdit={() => {
                      setInput("");
                      textareaRef.current?.focus();
                    }}
                  />
                );
              }
              // Transient reconnecting indicator: the backend's
              // unbounded connection-retry tier is riding out a network
              // outage. Rendered below the last message while waiting;
              // cleared by the first progress event that follows
              // recovery (see the SSE handlers).
              if (reconnecting) {
                elements.push(
                  <div key="reconnecting-pill" className="flex justify-center my-3">
                    <span className="text-xs text-muted-foreground bg-muted/40 rounded-full px-3 py-1 animate-pulse">
                      {t("chat.reconnecting", { attempt: reconnecting.attempt ?? "…" })}
                    </span>
                  </div>,
                );
              }
              // Terminal SSE offline: the subscribe connection gave up
              // after repeated failures. Manual retry re-opens it.
              if (subscribeDead) {
                elements.push(
                  <div key="subscribe-dead-pill" className="flex justify-center my-3">
                    <button
                      type="button"
                      className="text-xs text-muted-foreground bg-muted/40 hover:bg-muted/70 rounded-full px-3 py-1"
                      onClick={() => {
                        setSubscribeDead(false);
                        setSubscribeRetryNonce((n) => n + 1);
                      }}
                    >
                      Connection lost — click to reconnect
                    </button>
                  </div>,
                );
              }
              return elements;

            })()}

            {sending && (
              <div className="flex justify-start">
                <div className="bg-muted rounded-2xl rounded-bl-md px-4 py-3">
                  {/* Surge-cyan droplets phasing like the logo's wave
                      bands — flowing water, not a gray IM ellipsis. */}
                  <div className="flex items-center gap-1">
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-primary/80" style={{ animationDelay: "0ms" }} />
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-primary/80" style={{ animationDelay: "200ms" }} />
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-primary/80" style={{ animationDelay: "400ms" }} />
                  </div>
                </div>
              </div>
            )}

            <div ref={messagesEndRef} />
          </div>
        </div>

        {/* Live progress panel: agent maintains a per-session `todo.md`
            checklist and we render it here right above the composer so
            the user's eye is on the next step they're about to authorize,
            not buried at the top behind a long scroll history. Auto-
            hides when the file doesn't exist, has no checkbox items, OR
            every item is done and the agent isn't mid-turn — a finished
            plan has nothing left to track and leaving it pinned clutters
            later, unrelated conversation. Kept visible while sending so
            the completion state is seen before dismiss and a follow-up
            step (if any) still has room to appear. */}
        {!isEmpty && todoItems.length > 0 && (!todoItems.every((i) => i.done || i.cancelled) || sending) && (
          <TodoPanel items={todoItems} active={sending} />
        )}

        {/* Input — right inset matches the messages' reserved scrollbar gutter
            (6px) so the composer's edges line up with the message rows above. */}
        <div className="shrink-0 pl-4 pr-[calc(1rem+6px)] pb-6 pt-2">
          <div className="mx-auto max-w-2xl relative">
            {isReadOnlyChannel && (
              // The web compose path can't deliver into upstream IM
              // platforms (no reverse channel adapter, no outbound
              // routing), so writing here would silently corrupt the
              // session: the agent would process the turn, the IM
              // user would never see it, and on refresh the original
              // session's history wins because the orphan write
              // landed under a triple lookup that didn't match.
              // Block the input outright and tell the user where to
              // reply.
              <div className="mb-2 rounded-lg border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                {t("chatScreen.conversationLivesOn")}{" "}
                <span className="font-medium text-foreground">
                  {channelLabel(currentChannel)}
                </span>
                {t("chatScreen.replyFromThere")}
              </div>
            )}
            {isActAsView && !isReadOnlyChannel && (
              // Super_admin viewing another user's chat via the admin
              // Chats page (?actAs=<uid>). The middleware gates this as
              // read-only for the whole request, so any send would 403
              // — disable the composer and surface why.
              <div className="mb-2 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning dark:text-warning">
                {t("chatScreen.actAsReadonly")}
              </div>
            )}
            {slashOpen && filteredItems.length > 0 && (
              <SlashMenu
                items={filteredItems}
                activeIndex={slashIndex}
                onHover={setSlashIndex}
                onSelect={selectItem}
                agentId={selectedAgent}
              />
            )}
            <div
              className={
                "border border-border bg-card focus-within:ring-1 focus-within:ring-primary/50 transition-shadow " +
                (isEmpty
                  ? "rounded-2xl px-5 pt-4 pb-3 motion-safe:animate-in motion-safe:fade-in motion-safe:slide-in-from-bottom-2 motion-safe:duration-200 motion-safe:delay-75"
                  : "rounded-xl px-4 py-3")
              }
            >
              {attachments.length > 0 && (
                <div className="flex flex-wrap gap-2 mb-2 pb-2 border-b border-border/60">
                  {attachments.map((f, i) => {
                    const preview = attachmentPreviews[i];
                    if (preview) {
                      return (
                        <div
                          key={`${f.name}-${i}`}
                          className="group relative h-14 w-14 overflow-hidden rounded-md border border-border bg-muted"
                        >
                          <button
                            type="button"
                            onClick={() => setLightboxSrc(preview)}
                            className="block h-full w-full cursor-zoom-in"
                            aria-label={`Preview ${f.name}`}
                          >
                            {/* eslint-disable-next-line @next/next/no-img-element */}
                            <img
                              src={preview}
                              alt={f.name}
                              className="h-full w-full object-cover"
                            />
                          </button>
                          <button
                            type="button"
                            onClick={() => removeAttachment(i)}
                            className="absolute right-0.5 top-0.5 flex h-4 w-4 items-center justify-center rounded-full bg-background/80 text-muted-foreground opacity-0 transition group-hover:opacity-100 hover:text-foreground"
                            aria-label="Remove attachment"
                          >
                            <X className="h-3 w-3" />
                          </button>
                        </div>
                      );
                    }
                    return (
                      <div
                        key={`${f.name}-${i}`}
                        className="flex items-center gap-1.5 rounded-md bg-muted/60 pl-2 pr-1 py-1 text-xs"
                      >
                        <Paperclip className="h-3 w-3 text-muted-foreground" />
                        <span className="max-w-[160px] truncate">{f.name}</span>
                        <button
                          type="button"
                          onClick={() => removeAttachment(i)}
                          className="p-0.5 rounded-md hover:bg-muted-foreground/15 text-muted-foreground hover:text-foreground"
                          aria-label="Remove attachment"
                        >
                          <X className="h-3 w-3" />
                        </button>
                      </div>
                    );
                  })}
                </div>
              )}
              {/* Empty-state composer: Manus-style — textarea fills the
                  top, action row sits below it. Once messages exist we
                  swing back to the compact single-row layout so the
                  composer doesn't dominate the chat. */}
              {isEmpty ? (
                <>
                  <textarea
                    ref={textareaRef}
                    value={input}
                    onChange={handleInputChange}
                    onKeyDown={handleKeyDown}
                    onBlur={() => setTimeout(() => setSlashOpen(false), 120)}
                    placeholder={
                      isReadOnlyView
                        ? t("chat.readOnlyPlaceholder")
                        : selectedAgent
                          ? t("chat.messagePlaceholder", { name: agentName || selectedAgent })
                          : t("chat.selectAgentFirst")
                    }
                    disabled={!canUseComposer}
                    rows={3}
                    className="block w-full resize-none bg-transparent text-[15px] placeholder:text-muted-foreground/50 outline-none disabled:opacity-50"
                    style={{ maxHeight: 240, minHeight: 72 }}
                  />
                  <div className="mt-2 flex items-center justify-between">
                    <div className="flex items-center gap-2 min-w-0">
                      <label
                        className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-md border border-border text-muted-foreground transition-colors ${
                          !canAttach
                            ? "opacity-50"
                            : "hover:bg-muted hover:text-foreground cursor-pointer"
                        }`}
                        aria-label={t("chat.attachFiles")}
                      >
                        <Paperclip className="h-4 w-4" />
                        <input
                          ref={fileInputRef}
                          type="file"
                          multiple
                          className="sr-only"
                          onChange={handleFilePick}
                          disabled={!canAttach}
                        />
                      </label>
                      <button
                        type="button"
                        onClick={handleToggleChatOnly}
                        disabled={!canUseComposer}
                        aria-pressed={chatOnly}
                        title={t("chat.chatOnlyHint")}
                        className={`flex h-9 shrink-0 items-center gap-1.5 rounded-md border px-2.5 text-xs transition-colors ${
                          chatOnly
                            ? "border-primary/60 bg-primary/10 text-primary"
                            : "border-border text-muted-foreground hover:bg-muted hover:text-foreground"
                        } disabled:opacity-50`}
                      >
                        <MessageCircle className="h-3.5 w-3.5" />
                        {t("chat.chatOnly")}
                      </button>
                      {urlProjectId && projectInfo && (
                        <div
                          className="flex h-9 min-w-0 items-center gap-1.5 rounded-full border border-border px-3 text-xs text-muted-foreground"
                          title={projectInfo.name}
                        >
                          <FolderOpen className="size-3.5 shrink-0" />
                          <span className="truncate max-w-[20ch]">
                            {projectInfo.name}
                          </span>
                        </div>
                      )}
                    </div>
                    {sending && !input.trim() && attachments.length === 0 ? (
                      <Button
                        onClick={handleStop}
                        size="icon"
                        className="h-9 w-9 shrink-0"
                        aria-label={t("chat.stopGenerating")}
                      >
                        <Square className="h-3.5 w-3.5 fill-current" />
                      </Button>
                    ) : (
                      <Button
                        onClick={() => {
                          if (sending) handleSteer();
                          else handleSend();
                        }}
                        disabled={(!input.trim() && attachments.length === 0) || !canSendComposer}
                        size="icon"
                        className="h-9 w-9 shrink-0 active:scale-95"
                        aria-label={t("chat.sendMessage")}
                      >
                        <Send className="h-4 w-4 -translate-x-px translate-y-px" />
                      </Button>
                    )}
                  </div>
                </>
              ) : (
                <div className="flex items-center gap-2">
                  <button
                    type="button"
                    onClick={handleToggleChatOnly}
                    disabled={!canUseComposer}
                    aria-pressed={chatOnly}
                    aria-label={t("chat.chatOnly")}
                    title={t("chat.chatOnlyHint")}
                    className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-md transition-colors ${
                      chatOnly
                        ? "bg-primary/15 text-primary"
                        : "text-muted-foreground hover:bg-muted hover:text-foreground"
                    } disabled:opacity-50`}
                  >
                    <MessageCircle className="h-4 w-4" />
                  </button>
                  <label
                    className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors ${
                      !canAttach
                        ? "opacity-50"
                        : "hover:bg-muted hover:text-foreground cursor-pointer"
                    }`}
                    aria-label={t("chat.attachFiles")}
                  >
                    <Paperclip className="h-4 w-4" />
                    <input
                      ref={fileInputRef}
                      type="file"
                      multiple
                      className="sr-only"
                      onChange={handleFilePick}
                      disabled={!canAttach}
                    />
                  </label>
                  <textarea
                    ref={textareaRef}
                    value={input}
                    onChange={handleInputChange}
                    onKeyDown={handleKeyDown}
                    onBlur={() => setTimeout(() => setSlashOpen(false), 120)}
                    placeholder={
                      isReadOnlyView
                        ? t("chat.readOnlyPlaceholder")
                        : selectedAgent
                          ? t("chat.messagePlaceholder", { name: agentName || selectedAgent })
                          : t("chat.selectAgentFirst")
                    }
                    disabled={!canUseComposer}
                    rows={1}
                    className="flex-1 resize-none bg-transparent text-[15px] leading-6 placeholder:text-muted-foreground/50 outline-none disabled:opacity-50"
                    style={{ maxHeight: 200, minHeight: 24 }}
                  />
                  {sending && !input.trim() && attachments.length === 0 ? (
                    <Button
                      onClick={handleStop}
                      size="icon"
                      className="h-8 w-8 shrink-0"
                      aria-label={t("chat.stopGenerating")}
                    >
                      <Square className="h-3.5 w-3.5 fill-current" />
                    </Button>
                  ) : (
                    <Button
                      onClick={() => {
                        if (sending) handleSteer();
                        else handleSend();
                      }}
                      disabled={(!input.trim() && attachments.length === 0) || !canSendComposer}
                      size="icon"
                      className="h-8 w-8 shrink-0 active:scale-95"
                      aria-label={t("chat.sendMessage")}
                    >
                      <Send className="h-4 w-4 -translate-x-px translate-y-px" />
                    </Button>
                  )}
                </div>
              )}
            </div>
          </div>
        </div>

        {/* Dashboard board — B-variant empty state: the hero + composer
            keep their Manus-style centering, and the board (today's
            cards / latest todo / recent chats) fills the space below.
            Mounted for the whole life of the empty state so module state
            (deck position, fetched lists) survives; visibility toggles
            with the hidden class because a fresh keystroke means the
            user is composing — the board collapsing restores the clean
            centered hero. Attachments count as composing too. */}
        {isEmpty && !urlSessionId && selectedAgent && (
          <div
            className={
              "shrink-0 px-4 pb-8 pt-4 " +
              (input.trim() || attachments.length > 0 ? "hidden" : "")
            }
          >
            <ChatDashboard agentId={selectedAgent} />
          </div>
        )}
        {lightboxSrc && (
          <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/80 p-6 cursor-zoom-out"
            onClick={() => setLightboxSrc(null)}
            role="dialog"
            aria-modal="true"
            aria-label="Image preview"
            tabIndex={-1}
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            ref={(el) => {
              // Focus the dialog so Escape reaches its key handler; restore
              // focus to the triggering element on close is handled by the
              // browser's natural focus return after the node unmounts.
              (el as HTMLDivElement | null)?.focus();
            }}
            onKeyDown={(e) => {
              if (e.key === "Escape") setLightboxSrc(null);
            }}
          >
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img
              src={lightboxSrc}
              alt="Preview"
              className="max-h-full max-w-full rounded-lg shadow-2xl"
              onClick={(e) => e.stopPropagation()}
            />
            <button
              type="button"
              onClick={() => setLightboxSrc(null)}
              className="absolute right-4 top-4 flex h-9 w-9 items-center justify-center rounded-full bg-background/80 text-foreground hover:bg-background"
              aria-label="Close preview"
            >
              <X className="h-5 w-5" />
            </button>
          </div>
        )}
      </div>
      {filesSheetOpen && selectedAgent && (sessionId || urlProjectId) && (
        <WorkspacePanel
          agentId={selectedAgent}
          // On a project landing (no urlSessionId), sessionId here is the
          // synthetic id chat-screen mints for the upcoming "New chat" —
          // it doesn't correspond to anything on disk, so we suppress it
          // and let projectId drive the scope. Inside an actual chat,
          // urlSessionId is set and we pass the real sessionId.
          sessionId={urlSessionId ? sessionId : ""}
          projectId={!urlSessionId && urlProjectId ? urlProjectId : undefined}
          initialFile={pendingFile}
          onClose={() => { setFilesSheetOpen(false); setPendingFile(null); }}
        />
      )}
    </div>
  );
}

interface ChatHeaderTitleProps {
  title: string;
  fallback: string;
  onSave: (next: string) => void | Promise<void>;
}

/** Editable chat title rendered into the global sticky header via
 *  usePageHeader. Click / focus to edit; Enter or blur commits. */
function ChatHeaderTitle({ title, fallback, onSave }: ChatHeaderTitleProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(title);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!editing) setDraft(title);
  }, [title, editing]);

  useEffect(() => {
    if (editing) inputRef.current?.select();
  }, [editing]);

  const commit = () => {
    setEditing(false);
    const next = draft.trim();
    if (!next || next === title) return;
    onSave(next);
  };

  if (editing) {
    // field-sizing: content grows the input to match its text; min width
    // keeps it reasonable right after entering edit mode even if the
    // current title is very short.
    return (
      <input
        ref={inputRef}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          // Ignore Enter while the user is mid-composition (CJK IME). Both
          // conditions matter: isComposing is the modern signal, keyCode 229
          // is the legacy flag some browsers (and macOS Pinyin in particular)
          // still emit without isComposing set.
          if (e.nativeEvent.isComposing || e.keyCode === 229) return;
          if (e.key === "Enter") {
            e.preventDefault();
            commit();
          } else if (e.key === "Escape") {
            setDraft(title);
            setEditing(false);
          }
        }}
        onBlur={commit}
        style={{ fieldSizing: "content" } as React.CSSProperties}
        className="h-7 min-w-[8ch] max-w-[40ch] rounded-md bg-transparent px-2 text-sm outline-none ring-1 ring-border focus:ring-primary/40"
      />
    );
  }

  return (
    <button
      onClick={() => {
        setDraft(title);
        setEditing(true);
      }}
      // Cap the title width responsively so a long auto-summary doesn't
      // push the whole header off-screen on small viewports. The
      // arbitrary `min(...)` keeps narrow widths on phones (60vw) while
      // capping at ~32rem on desktop; sm:/md: bumps give intermediate
      // breakpoints a deterministic width too.
      className="group flex min-w-0 max-w-[min(60vw,18rem)] sm:max-w-[24rem] md:max-w-[28rem] lg:max-w-[32rem] items-center gap-1.5 rounded-md px-2 py-1 text-sm text-foreground hover:bg-muted/50"
      title={title || fallback}
    >
      <span className="truncate">{title || fallback}</span>
      <Pencil className="h-3 w-3 shrink-0 text-muted-foreground/50 opacity-0 transition-opacity group-hover:opacity-100" />
    </button>
  );
}

// Tool family theming — every built-in tool gets a distinct icon keyed
// off its name, so related tools read as one group at a glance (every
// knowledgebase_* tool is BookOpen, every web_* is Globe, etc.). Color
// is intentionally uniform across all families: only the icon differs,
// so a screen full of tool calls stays calm. The spinner (info) and
// completion check (success) carry status, not category. Unknown names
// (MCP/dynamic tools) fall back to DEFAULT_FAMILY's Wrench.
type ToolFamily = {
  icon: typeof Wrench;
  accent: string;        // group header + per-tool family icon color
  title: string;         // per-tool title text color
  dot: string;           // per-tool spinner border color
  check: string;         // per-tool check icon color
  nonDefault: boolean;   // render the family icon beside per-tool title
};

// Single source of color for every family — identical so hue never
// implies a category. Only the icon (per family below) varies.
const DEFAULT_FAMILY: ToolFamily = {
  icon: Wrench,
  accent: "text-muted-foreground",
  title: "text-foreground",
  dot: "border-info/60",
  check: "text-success",
  nonDefault: false,
};

const TOOL_FAMILIES: Array<{ match: (name: string) => boolean; icon: typeof Wrench; nonDefault: boolean }> = [
  { match: (n) => n.startsWith("memory_"), icon: Brain, nonDefault: true },
  { match: (n) => n.startsWith("knowledgebase_"), icon: BookOpen, nonDefault: true },
  { match: (n) => n.startsWith("web_"), icon: Globe, nonDefault: true },
  { match: (n) => n === "image_gen", icon: ImageIcon, nonDefault: true },
  { match: (n) => n === "vision", icon: Eye, nonDefault: true },
  { match: (n) => n === "tts", icon: Music, nonDefault: true },
  { match: (n) => ["read_file", "write_file", "list_dir", "edit_file"].includes(n), icon: FileText, nonDefault: true },
  { match: (n) => n === "apply_patch", icon: FileCode, nonDefault: true },
  { match: (n) => n === "exec" || n === "host_exec" || n === "bash_output" || n === "kill_shell", icon: Terminal, nonDefault: true },
  { match: (n) => n.endsWith("_cron_job"), icon: Clock, nonDefault: true },
  { match: (n) => n === "load_skill" || n === "search_skills" || n === "install_skill", icon: Puzzle, nonDefault: true },
  { match: (n) => n === "spawn_subagent" || n === "delegate_task", icon: Bot, nonDefault: true },
  { match: (n) => n === "message" || n === "fetch_messages", icon: Send, nonDefault: true },
  { match: (n) => n.startsWith("set_"), icon: SlidersHorizontal, nonDefault: true },
  { match: (n) => n === "update_goal", icon: Target, nonDefault: true },
  { match: (n) => n === "get_billing_usage", icon: CreditCard, nonDefault: true },
];

function familyOf(name: string): ToolFamily {
  for (const f of TOOL_FAMILIES) if (f.match(name)) return { ...DEFAULT_FAMILY, icon: f.icon, nonDefault: f.nonDefault };
  return DEFAULT_FAMILY;
}

/** Renders a group of tool calls as a collapsible summary. When
 *  `nested`, the outer flex/max-width wrappers are dropped so a parent
 *  container (ToolRoundsBundle) can stack rounds without each one
 *  re-imposing its own bubble alignment. */
function ToolCallGroup({ msg, surfacedSrcs, agentId, sessionId, workspaceRoot, nested = false, roundIndex, subagentProgress }: { msg: ChatMessage; surfacedSrcs?: ReadonlySet<string>; agentId: string; sessionId: string; workspaceRoot?: string; nested?: boolean; roundIndex?: number; subagentProgress?: { iteration?: number; max?: number; phase?: "thinking" | "running" | "final-delivery" | "done"; tools?: string[] } | null }) {
  const t = useT();
  const [groupOpen, setGroupOpen] = useState(false);
  const [expandedTool, setExpandedTool] = useState<Record<string, boolean>>({});

  const tools = msg.toolCalls || [];
  const doneCount = tools.filter((tc) => tc.result != null).length;
  const allDone = doneCount === tools.length;

  // delegate_task is registered serial, so only the FIRST not-yet-
  // returned delegate_task in this round corresponds to the active
  // subagentProgress event stream. Older ones already finished;
  // later ones are queued on the mutex and have no progress yet.
  const activeDelegateId = (() => {
    for (const tc of tools) {
      if (tc.name === "delegate_task" && tc.result == null) {
        return tc.id;
      }
    }
    return null;
  })();

  const toggleTool = (id: string) =>
    setExpandedTool((prev) => ({ ...prev, [id]: !prev[id] }));

  // Group accent: only when every tool in the round belongs to the same
  // non-default family — a mixed round (e.g. knowledgebase_search +
  // write_file) stays generic so a family color isn't misleading.
  const groupFamily =
    tools.length > 0 && tools.every((tc) => familyOf(tc.name) === familyOf(tools[0].name))
      ? familyOf(tools[0].name)
      : DEFAULT_FAMILY;

  // A round whose every tool is a regex_hook match is themed blue and
  // relabeled (matched / running, hook name without the "regex_hook: "
  // prefix) so it reads as a hook firing, not an ordinary tool call.
  // Detected by name prefix because the backend marks a hook hit by
  // emitting a synthetic tool call named "regex_hook: <hook>".
  const isRH =
    tools.length > 0 && tools.every((tc) => tc.name.startsWith("regex_hook"));

  const inner = (
    <>
      {/* Content before tools */}
      {msg.content && (() => {
        // Mid-turn tool-group text: strip options blocks too (buttons
        // only render on the final bubble; a stray block here would show
        // as raw JSON otherwise).
        const { text: midText } = splitReplyOptions(msg.content);
        return (
          <div className="bg-muted rounded-2xl rounded-bl-md px-4 py-2.5">
            {renderContentWithDataImages(midText, surfacedSrcs, false, agentId, sessionId, msg.metadata?.knowledgeSources, workspaceRoot) ?? (
              <ChatMarkdown text={midText} agentId={agentId} sessionId={sessionId} workspaceRoot={workspaceRoot} knowledgeSources={msg.metadata?.knowledgeSources} />
            )}
          </div>
        );
      })()}
      {/* Collapsed tool group summary */}
      <div className="rounded-lg border border-border bg-card/50 overflow-hidden">
          <button
            onClick={() => setGroupOpen(!groupOpen)}
            className="rounded-md flex w-full items-center gap-2 px-3 py-2 text-xs hover:bg-muted/50 transition-colors"
          >
            {!allDone ? (
              <div className="h-5 w-5 shrink-0 rounded-full border-2 border-info border-t-transparent animate-spin" />
            ) : roundIndex !== undefined ? (
              // When this group is a round inside a bundle, the leading
              // glyph carries the round number — gives the bundle's
              // expanded view a built-in step indicator without an
              // extra "ROUND N" label row above each card.
              <span className="h-5 w-5 shrink-0 inline-flex items-center justify-center rounded-full bg-info/10 text-2xs font-semibold text-info dark:text-info">
                {roundIndex}
              </span>
            ) : isRH ? (
              <Zap className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            ) : (
              (() => {
                const FamIcon = groupFamily.icon;
                return <FamIcon className={`h-3.5 w-3.5 ${groupFamily.accent} shrink-0`} />;
              })()
            )}
            <span className="font-medium text-foreground">
              {isRH
                ? (allDone ? "Regex Hook matched" : "Regex Hook running...")
                : (allDone
                  ? `Executed ${tools.length} tool${tools.length > 1 ? "s" : ""}`
                  : `Running tools (${doneCount}/${tools.length})...`)}
            </span>
            <span className="text-muted-foreground/60 text-2xs flex-1 text-left truncate">
              {tools.map((tc) => isRH ? tc.name.replace(/^regex_hook:\s*/, "") : tc.name).join(", ")}
            </span>
            {groupOpen ? (
              <ChevronDown className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            ) : (
              <ChevronRight className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            )}
          </button>

          {groupOpen && (
            <div className="border-t border-border">
              {tools.map((tc) => (
                <div key={tc.id} className="border-b border-border last:border-b-0">
                  <button
                    onClick={() => toggleTool(tc.id)}
                    className="rounded-md flex w-full items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted/30 transition-colors"
                  >
                    {(() => {
                      const fam = familyOf(tc.name);
                      const FamIcon = fam.nonDefault ? fam.icon : null;
                      const label = isRH ? tc.name.replace(/^regex_hook:\s*/, "") : tc.name;
                      return (
                        <>
                          {tc.result === undefined ? (
                            <div className={`h-3 w-3 shrink-0 rounded-full border-2 ${fam.dot} border-t-transparent animate-spin`} />
                          ) : (
                            <Check className={`h-3 w-3 ${fam.check} shrink-0`} />
                          )}
                          {FamIcon && <FamIcon className={`h-3 w-3 ${fam.accent} shrink-0`} />}
                          <span className={`font-medium ${fam.title}`}>{label}</span>
                        </>
                      );
                    })()}
                    {tc.metadata?.sandbox && (
                      <span
                        className="flex items-center gap-0.5 rounded bg-success/10 px-1 py-0.5 text-2xs font-medium text-success dark:text-success"
                        title={t("chat.sandboxedExec")}
                      >
                        <ShieldCheck className="h-2.5 w-2.5" />
                        sandbox
                      </span>
                    )}
                    <span className="text-muted-foreground/50 font-mono truncate flex-1 text-left text-2xs">
                      {(() => {
                        try {
                          const args = JSON.parse(tc.arguments);
                          // delegate_task's `task` arg always opens with
                          // the same boilerplate ("You are a B2B lead
                          // researcher…"); the differentiating part is a
                          // markdown heading further down ("## Target:
                          // <industry>"). Surface that line instead of
                          // the head so a fan-out of N delegates doesn't
                          // look like N copies of the same call.
                          if (tc.name === "delegate_task" && typeof args.task === "string") {
                            const m = args.task.match(/^#+\s*Target:\s*(.+)$/m) ||
                                      args.task.match(/^#+\s+(.+)$/m);
                            if (m) return m[1].trim();
                            return args.task.replace(/\s+/g, " ").slice(0, 120);
                          }
                          return Object.values(args).join(", ");
                        } catch {
                          return tc.arguments;
                        }
                      })()}
                    </span>
                    {expandedTool[tc.id] ? (
                      <ChevronDown className="h-3 w-3 text-muted-foreground/50 shrink-0" />
                    ) : (
                      <ChevronRight className="h-3 w-3 text-muted-foreground/50 shrink-0" />
                    )}
                  </button>
                  {expandedTool[tc.id] && (
                    <div className="px-3 py-2 space-y-2 bg-muted/20">
                      <div>
                        <p className="text-xs font-medium text-muted-foreground uppercase mb-1">Input</p>
                        <pre className="text-xs font-mono bg-muted/50 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all max-h-40">
                          {(() => {
                            try { return JSON.stringify(JSON.parse(tc.arguments), null, 2); }
                            catch { return tc.arguments; }
                          })()}
                        </pre>
                      </div>
                      {tc.result != null ? (
                        <div>
                          <p className="text-xs font-medium text-muted-foreground uppercase mb-1">Output</p>
                          <pre className="text-xs font-mono bg-muted/50 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all max-h-60">
                            {tc.result.length > 2000 ? tc.result.slice(0, 2000) + "..." : tc.result}
                          </pre>
                        </div>
                      ) : tc.name === "delegate_task" && tc.id === activeDelegateId && subagentProgress ? (
                        <div className="text-xs text-muted-foreground/80 italic">
                          {(() => {
                            const it = subagentProgress.iteration;
                            const mx = subagentProgress.max;
                            const phase = subagentProgress.phase;
                            const tools = subagentProgress.tools;
                            const counter = it && mx ? `Iteration ${it}/${mx}` : "Sub-agent running";
                            let detail = "";
                            if (phase === "thinking") detail = "thinking";
                            else if (phase === "running" && tools?.length) detail = `running ${tools.join(", ")}`;
                            else if (phase === "final-delivery") detail = "synthesizing final answer";
                            return detail ? `${counter} · ${detail}` : counter;
                          })()}
                        </div>
                      ) : tc.name === "delegate_task" && tc.result == null && tc.id !== activeDelegateId ? (
                        <p className="text-xs text-muted-foreground/60 italic">Queued (waiting on prior sub-agent)…</p>
                      ) : (
                        <p className="text-xs text-muted-foreground/60 italic">Executing...</p>
                      )}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </>
    );
  if (nested) {
    return <div className="space-y-2">{inner}</div>;
  }
  return (
    <div className="flex justify-start">
      <div className="max-w-full space-y-2">{inner}</div>
    </div>
  );
}

/** ToolRoundsBundle wraps consecutive tool-group rounds (the agent
 *  ran tools, got results, then ran more tools, …) in a single
 *  collapsible header so a long ReAct turn doesn't take over the chat
 *  with seven independent "Executed N tools" boxes. The aggregate
 *  badge shows total rounds + total tools; expanding reveals each
 *  round as a regular ToolCallGroup, which itself stays collapsible
 *  per the existing per-round UX. Single-round bundles aren't built
 *  here — those still render as a flat ToolCallGroup so the extra
 *  layer doesn't show up unless it earns its keep. */
function ToolRoundsBundle({
  rounds,
  surfacedSrcs,
  agentId,
  sessionId,
  workspaceRoot,
  subagentProgress,
}: {
  rounds: ChatMessage[];
  surfacedSrcs?: ReadonlySet<string>;
  agentId: string;
  sessionId: string;
  workspaceRoot?: string;
  subagentProgress?: { iteration?: number; max?: number; phase?: "thinking" | "running" | "final-delivery" | "done"; tools?: string[] } | null;
}) {
  const [open, setOpen] = useState(false);
  const allTools = rounds.flatMap((r) => r.toolCalls || []);
  const totalTools = allTools.length;
  const doneCount = allTools.filter((tc) => tc.result != null).length;
  const allDone = doneCount === totalTools;
  return (
    <div className="flex justify-start">
      <div className="max-w-full w-full">
        <div className="rounded-lg border border-border bg-card/50 overflow-hidden">
          <button
            onClick={() => setOpen(!open)}
            className="rounded-md flex w-full items-center gap-2 px-3 py-2 text-xs hover:bg-muted/50 transition-colors"
          >
            {!allDone ? (
              <div className="h-3.5 w-3.5 shrink-0 rounded-full border-2 border-info border-t-transparent animate-spin" />
            ) : (
              <Wrench className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            )}
            <span className="font-medium text-foreground">
              {allDone
                ? `Used ${totalTools} tool${totalTools === 1 ? "" : "s"} across ${rounds.length} round${rounds.length === 1 ? "" : "s"}`
                : `Running tools… (${doneCount}/${totalTools} across ${rounds.length} rounds)`}
            </span>
            <span className="ml-auto" />
            {open ? (
              <ChevronDown className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            ) : (
              <ChevronRight className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            )}
          </button>
          {open && (
            <div className="border-t border-border p-2 space-y-1.5 bg-background/30">
              {rounds.map((round, idx) => (
                <ToolCallGroup
                  key={round.id || idx}
                  msg={round}
                  surfacedSrcs={surfacedSrcs}
                  agentId={agentId}
                  sessionId={sessionId}
                  workspaceRoot={workspaceRoot}
                  nested
                  roundIndex={idx + 1}
                  subagentProgress={subagentProgress}
                />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
