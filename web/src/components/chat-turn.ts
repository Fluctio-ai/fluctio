// Chat turn extraction from chat-screen.tsx: the streaming SSE event
// handler (delta flush, content seal, tool-group rounds, notice pills)
// as a per-turn factory, the produced-file extraction shared with the
// history-reload path, and the post-turn workspace reconciliation that
// turns raw file diffs into bubble chips.
//
// The factory owns everything event-shaped for one turn; chat-screen
// keeps the orchestration around it (guards, uploads, POST, abort,
// cleanup) and passes refs/actions in through TurnEventContext.

import type { RefObject } from "react";
import type { ChatStreamEvent, ToolResultMetadata, WorkspaceFile } from "@/lib/api";
import {
  authPromptMessages,
  compactionMessage,
  dropBubbleIfWhitespace,
  errorMessage,
  newAgentBubbleId,
  patchToolGroup,
  replaceSkillPills,
  sealStreamedBubble,
  skillPendingMessages,
  stampLastAgentMeta,
  upsertToolGroup,
  appendDelta,
  type ChatMessage,
  type ToolCallRecord,
} from "@/components/chat-events";
import { isSystemFile } from "@/components/chat-file-tree";

type TFn = (key: string, vars?: Record<string, string | number>) => string;

export interface ProducedFile {
  path: string; // path relative to workspace
  size?: number;
  modTime?: number; // unix ms — places the file under the message that produced it
}

export interface SubagentProgressState {
  iteration?: number;
  max?: number;
  phase?: "thinking" | "running" | "final-delivery" | "done";
  tools?: string[];
}

export type ReconnectingState = null | { attempt?: number };

// Per-turn mutable state the event handler accumulates into. chat-screen
// creates one per send; startNewGroup / tool_call / tool_result mutate it.
export interface TurnState {
  curGroupId: string;
  curCalls: ToolCallRecord[];
  turnFiles: ProducedFile[];
  seenPaths: Set<string>;
}

export interface TurnEventContext {
  agentId: string;
  sessionId: string;
  // Lazy-create marker from the URL on a project chat's first send —
  // drives the fallback scope-prefix guess for produced files.
  projectIdHint: string;
  // Shared with the parallel /api/chat/subscribe SSE handler and the
  // session-switch effects — the same refs chat-screen owns.
  streamingMsgIdRef: RefObject<string | null>;
  pendingDeltaRef: RefObject<string>;
  deltaFlushTimerRef: RefObject<ReturnType<typeof setTimeout> | null>;
  maxSeqRef: RefObject<number>;
  viewedSessionRef: RefObject<string | null>;
  wsPrefixesRef: RefObject<string[]>;
  turn: TurnState;
  actions: {
    setMessages: (update: (prev: ChatMessage[]) => ChatMessage[]) => void;
    setReconnecting: (v: ReconnectingState) => void;
    setSubagentProgress: (v: SubagentProgressState | null) => void;
    applySteerEvent: (content: string) => void;
    handleNewChat: () => void;
    loadSessions: (agentId: string) => void;
    // Refetch todo.md — fired when a file-mutation tool touched it.
    refreshTodo: () => void;
    t: TFn;
  };
}

export function createTurnEventHandler(ctx: TurnEventContext): (evt: ChatStreamEvent) => void {
  const {
    agentId,
    sessionId,
    projectIdHint,
    streamingMsgIdRef,
    pendingDeltaRef,
    deltaFlushTimerRef,
    maxSeqRef,
    viewedSessionRef,
    wsPrefixesRef,
    turn,
    actions,
  } = ctx;
  const { setMessages, setReconnecting, setSubagentProgress, applySteerEvent, handleNewChat, loadSessions, refreshTodo, t } = actions;
  let { curGroupId, curCalls } = turn;

  // streamingMsgIdRef tracks the in-flight assistant bubble for
  // content_delta accretion. Stored on a ref so the parallel
  // /api/chat/subscribe SSE handler can observe it and skip duplicating
  // the bubble when the trailing `content` event races through it ahead
  // of the POST callback. Reset at startNewGroup, after the `content`
  // seal, and on tool_call / done.
  const startNewGroup = () => {
    turn.curGroupId = `tg-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
    curGroupId = turn.curGroupId;
    turn.curCalls = [];
    curCalls = turn.curCalls;
    streamingMsgIdRef.current = null;
  };
  startNewGroup();

  // Flush accumulated content_delta chunks into the streaming bubble
  // (or create the bubble on the first flush). The old path ran one
  // setMessages full-array copy + string concat per streamed chunk and
  // re-rendered the whole list — O(n²) string work and the main cause
  // of streaming jank on long sessions. ~50ms throttle batches the
  // updates; the trailing `content` event replaces the text wholesale.
  const flushPendingDelta = () => {
    if (deltaFlushTimerRef.current !== null) {
      clearTimeout(deltaFlushTimerRef.current);
      deltaFlushTimerRef.current = null;
    }
    const chunk = pendingDeltaRef.current;
    if (!chunk) return;
    pendingDeltaRef.current = "";
    if (!streamingMsgIdRef.current) {
      const id = newAgentBubbleId();
      streamingMsgIdRef.current = id;
      setMessages((prev) => [
        ...prev,
        { id, role: "agent", content: chunk, timestamp: Date.now() },
      ]);
      return;
    }
    // Capture BEFORE dispatch: by the time React runs the updater a
    // concurrent seal may have nulled the ref, which would silently
    // drop the batched chunk.
    const id = streamingMsgIdRef.current;
    setMessages((prev) => appendDelta(prev, id, chunk));
  };

  return (evt: ChatStreamEvent) => {
    // Session-switch guard: this closure's sessionId is the ORIGIN
    // session. Once the user navigates elsewhere, every render-
    // affecting update here would land in the NEW session's message
    // list (viewedSessionRef no longer matches). Skip all handling
    // while away — the turn keeps consuming server-side, rendering
    // resumes if the user returns mid-stream, and the trailing
    // content/done events (or a history reload on switch) cover the
    // gap. Dedup/abort bookkeeping is intentionally dropped with the
    // render: maxSeqRef was reset by the switch anyway.
    if (viewedSessionRef.current !== sessionId) return;
    // Dedup against /api/chat/subscribe SSE, which subscribes to the
    // same chat-events hub server-side. Whichever path arrives first
    // renders; the other skips. seq < 0 means persistence failed for
    // this event — fall through and accept the possibility of a
    // double-render rather than dropping the event entirely.
    if (typeof evt.seq === "number" && evt.seq >= 0) {
      if (evt.seq <= maxSeqRef.current) return;
      maxSeqRef.current = evt.seq;
    }
    if (evt.type !== "reconnecting") setReconnecting(null);
    switch (evt.type) {
      case "turn_pending": {
        setReconnecting({});
        break;
      }
      case "reconnecting": {
        setReconnecting({ attempt: evt.data?.attempt });
        break;
      }
      case "content_delta": {
        // Incremental token chunk from the provider. Append to the
        // in-flight assistant bubble — create one on the first delta of
        // a round (and after a tool-group split). The final `content`
        // event still arrives with the full text when the turn
        // completes so refresh / replay paths stay intact even though
        // deltas aren't persisted.
        const delta = evt.data?.delta || "";
        if (!delta) break;
        if (curCalls.length > 0 && !streamingMsgIdRef.current) {
          // Content after tool calls = new round; flush the previous
          // bubble's pending text first, then reset state so the new
          // bubble is its own message, not appended onto the previous
          // tool-group's thinking text.
          flushPendingDelta();
          startNewGroup();
        }
        pendingDeltaRef.current += delta;
        if (!streamingMsgIdRef.current) {
          // First delta of a bubble: create it immediately so the
          // typing indicator has an anchor; later chunks batch.
          flushPendingDelta();
        } else if (deltaFlushTimerRef.current === null) {
          deltaFlushTimerRef.current = setTimeout(flushPendingDelta, 50);
        }
        break;
      }
      case "content": {
        // Final full text: cancel any pending batched flush — the seal
        // below writes the complete content directly.
        if (deltaFlushTimerRef.current !== null) {
          clearTimeout(deltaFlushTimerRef.current);
          deltaFlushTimerRef.current = null;
        }
        pendingDeltaRef.current = "";
        const content = evt.data?.content || "";
        const meta = evt.data?.metadata;
        if (content === "__NEW_SESSION__") {
          handleNewChat();
          loadSessions(agentId);
          return;
        }
        if (!content && !meta) {
          break;
        }
        // If the bubble was already streamed in via content_delta, the
        // final `content` carries the same text — just seal the
        // in-flight ID, optionally attach metadata, and skip creating a
        // duplicate bubble.
        if (streamingMsgIdRef.current) {
          const id = streamingMsgIdRef.current;
          streamingMsgIdRef.current = null;
          // Seal the streamed bubble. If it only collected whitespace
          // deltas this round (no real text), drop it instead of
          // leaving a phantom blank bubble.
          setMessages((prev) => sealStreamedBubble(prev, id, meta));
          break;
        }
        // Metadata-only event with empty content: the backend uses this
        // to retro-stamp the previous bubble (e.g. for the streaming
        // forced-final-delivery path where chunks flow through a
        // separate channel and the metadata follows after). Apply to
        // the last agent message instead of creating an empty new
        // bubble.
        if (!content && meta) {
          setMessages((prev) => stampLastAgentMeta(prev, meta));
          break;
        }
        if (curCalls.length > 0) {
          // Content after tool calls = new round. Finalize current group, start fresh.
          startNewGroup();
        }
        // Store as thinking content (may become part of next tool-group, or stay as final answer)
        setMessages((prev) => [
          ...prev,
          { id: newAgentBubbleId(), role: "agent", content, timestamp: Date.now(), metadata: meta },
        ]);
        break;
      }
      case "tool_call": {
        // The in-flight streamed bubble (if any) may hold only
        // whitespace (some providers emit a blank delta right before a
        // tool_call); the tool-group is appended as a separate entry, so
        // that empty bubble would linger as a phantom blank. Drop it to
        // match the refresh path, which folds tool-only turns into a
        // group. Clear the streaming ID too so a subsequent
        // content_delta on the next round spawns a fresh bubble instead
        // of writing into the now-defunct ID.
        const tcSid = streamingMsgIdRef.current;
        streamingMsgIdRef.current = null;
        if (tcSid) {
          setMessages((prev) => dropBubbleIfWhitespace(prev, tcSid));
        }
        // New round starts if every tool in the current group has
        // already resolved. Without this, two assistant turns that
        // happen back-to-back with no intervening content event get
        // merged into one visual group live — inconsistent with the
        // refresh path (buildChatMessages) which correctly splits per
        // assistant message.
        if (curCalls.length > 0 && curCalls.every((c) => c.result !== undefined)) {
          startNewGroup();
        }
        curCalls.push({
          id: evt.data?.id || "",
          name: evt.data?.name || "",
          arguments: evt.data?.arguments || "{}",
        });
        // upsertToolGroup updates the round's existing group (an
        // additional tool_call in the same turn) or appends the group as
        // its own message. Any streamed agent bubble stays in place —
        // mirrors the split buildChatMessages applies on history reload,
        // so live and reloaded views stay consistent.
        setMessages((prev) => upsertToolGroup(prev, curGroupId, [...curCalls]));
        break;
      }
      case "tool_result": {
        const tc = curCalls.find((c) => c.id === (evt.data?.id || ""));
        const resultText = evt.data?.result || "";
        if (tc) {
          tc.result = resultText;
          if (evt.data?.metadata) tc.metadata = evt.data.metadata as ToolResultMetadata | undefined;
        }
        // Capture files this turn deliberately produced (write_file /
        // deliver_file / image_gen). Shares extractProducedFiles with
        // the history-reload path so a refresh renders the same files
        // under the same bubbles, independent of filesystem mtime.
        // exec-script output isn't a tool product — it stays out of
        // bubbles (diffFiles in reconcileTurnFiles) and surfaces only in
        // the workspace browser.
        if (tc) {
          // Prefer the backend-resolved scope prefixes (ref — the
          // streaming closure predates the listing state); fall back to
          // the URL hint / loose-chat guess. The turn-end reconcile
          // below corrects any remaining miss.
          const prefixes = wsPrefixesRef.current.length
            ? wsPrefixesRef.current
            : projectIdHint
              ? [`projects/${projectIdHint}/${sessionId}/`, `projects/${projectIdHint}/`]
              : [`sessions/${sessionId}/`];
          for (const f of extractProducedFiles([tc], prefixes)) {
            if (!turn.seenPaths.has(f.path)) {
              turn.seenPaths.add(f.path);
              turn.turnFiles.push(f);
            }
          }
        }
        // Refresh the todo panel whenever a file-mutation tool just
        // touched todo.md. We inspect arguments rather than poll on
        // every tool_result so the network cost stays proportional to
        // actual updates (a long run with 50 web_search calls doesn't
        // trigger 50 refetches).
        if (tc && (tc.name === "write_file" || tc.name === "edit_file" || tc.name === "apply_patch")) {
          try {
            const args = JSON.parse(tc.arguments);
            const path: string =
              (typeof args?.path === "string" ? args.path : "") ||
              (typeof args?.file_path === "string" ? args.file_path : "");
            if (path && /(^|\/)todo\.md$/i.test(path)) {
              refreshTodo();
            }
          } catch { /* ignore bad args */ }
        }
        setMessages((prev) => patchToolGroup(prev, curGroupId, [...curCalls]));
        break;
      }
      case "auth_prompt": {
        // ask-mode outside-workspace/dangerous call was parked on the
        // session; surface it as an agent bubble carrying authOptions so
        // the chat panel renders tappable buttons.
        // handleSend(opt.cmd) dispatches the slash reply on click.
        const desc = evt.data?.description || "";
        const options = evt.data?.options || [];
        for (const m of authPromptMessages(desc, options)) {
          setMessages((prev) => [...prev, m]);
        }
        break;
      }
      case "subagent_progress": {
        // Subagent emitted a heartbeat. Stored as a single "current run
        // state" since delegate_task is registered serial — only one
        // subagent in flight at any time. phase="done" clears it.
        if (evt.data?.phase === "done") {
          setSubagentProgress(null);
        } else {
          setSubagentProgress({
            iteration: evt.data?.iteration,
            max: evt.data?.max,
            phase: evt.data?.phase,
            tools: evt.data?.tools,
          });
        }
        break;
      }
      case "steer": {
        // A message the user injected mid-turn was folded into the
        // running turn server-side. Render it as a user bubble
        // (reconciled against the optimistic pendingSteer bubble).
        applySteerEvent(evt.data?.content || "");
        break;
      }
      case "error": {
        // Surface backend errors as a notice pill. Without this the turn
        // just hangs — the model failed (provider 4xx/5xx, serialization
        // mismatch, etc.) and the only signal was a gateway log line the
        // user can't see. The backend persists the same text as a
        // turn_error notice, so a reload renders the identical pill —
        // live and reloaded views stay consistent.
        for (const m of errorMessage(evt.data?.message || "Unknown error")) {
          setMessages((prev) => [...prev, m]);
        }
        break;
      }
      case "compaction_notice": {
        // Auto-compaction happened mid-turn — insert a centered notice
        // bubble using the backend's content text.
        for (const m of compactionMessage(evt.data?.content)) {
          setMessages((prev) => [...prev, m]);
        }
        break;
      }
      case "skill_pending": {
        // Skill(s) staged during this turn are awaiting approval. Render
        // as a centered notice pill (same path as compaction_notice),
        // replacing any prior pill so consecutive turns don't stack
        // duplicates.
        const count = evt.data?.count ?? 0;
        const names = Array.isArray(evt.data?.names) ? evt.data!.names! : [];
        const pills = skillPendingMessages(count, names, t);
        setMessages((prev) => replaceSkillPills(prev, pills));
        break;
      }
    }
  };
}

function parseWrittenSize(result: string): number | undefined {
  const m = result.match(/^Written (\d+) bytes/);
  return m ? parseInt(m[1], 10) : undefined;
}

// extractProducedFiles pulls workspace paths out of a run of tool calls —
// the files a turn *deliberately* produced via write_file / deliver_file /
// image_gen. The source is the persisted tool record (name + arguments +
// result), NOT the filesystem mtime, so file attribution survives anything
// that rewrites mtime: docker redeploy copying the workspaces dir, volume
// migration, backup restore, etc. Previously history reload attributed
// files by mtime and every file piled onto the latest bubble after a
// redeploy (all mtimes reset to the copy moment).
//
// scopePrefixes mirrors the prefixes listAgentFiles can return, ordered
// most-specific first. Project chats have TWO possible layouts: ordinary
// agents write into the per-chat subdir projects/<pid>/<sid>/ (registry.
// scopeSessionID keeps the session segment), while coding-root agents
// write straight to the project root projects/<pid>/. The store path a
// write_file landed at isn't knowable from the tool record alone, so we
// try candidates in order — with an `exists` oracle (the live file list)
// the first candidate that actually exists wins; without one (live
// streaming, listing not fetched yet) the first candidate is the best
// guess and the turn-end reconcile against listAgentFiles fixes any
// coding-root miss. Real-time streaming and history reload both call
// this so a refresh shows the same files under the same bubbles.
//
// Files NOT produced through a tool call (exec scripts writing PDFs, etc.)
// are intentionally excluded — they have no reliable per-turn anchor and
// are surfaced only via the workspace browser (FilesSheet), which lists
// the full listAgentFiles set independently of bubble attribution.
export function extractProducedFiles(
  calls: { name: string; arguments: string; result?: string }[],
  scopePrefixes: string[],
  exists?: (p: string) => boolean,
): ProducedFile[] {
  const out: ProducedFile[] = [];
  const seen = new Set<string>();
  const add = (rawRel: string, size?: number) => {
    // Skip absolute paths (write_file to /etc/... isn't a workspace artifact)
    // and system identity files. isSystemFile matches on the bare name, so
    // check before prefixing — a prefixed "sessions/x/todo.md" contains "/"
    // and would slip past it.
    if (!rawRel || rawRel.startsWith("/") || isSystemFile(rawRel)) return;
    // "../x" explicitly addresses the project-shared layer (the file
    // tools' session-first scoping): strip the escape segments and
    // resolve against the SHALLOWEST prefix (the project root) instead
    // of the chat's own dir.
    const rel = rawRel.replace(/^(\.\.\/)+/, "");
    let full = "";
    if (rel !== rawRel) {
      const rootPrefix = scopePrefixes[scopePrefixes.length - 1];
      if (rootPrefix) full = rootPrefix + rel;
    } else {
      for (const p of scopePrefixes) {
        const cand = p + rel;
        if (!exists || exists(cand)) {
          full = cand;
          break;
        }
      }
    }
    if (full === "") full = scopePrefixes[0] + rel;
    if (seen.has(full)) return;
    seen.add(full);
    out.push(size !== undefined ? { path: full, size } : { path: full });
  };
  for (const c of calls) {
    const res = c.result || "";
    let args: Record<string, unknown> = {};
    try { args = JSON.parse(c.arguments || "{}") as Record<string, unknown>; } catch { /* ignore bad args */ }
    switch (c.name) {
      case "write_file":
        if (/^Written \d+ bytes/.test(res)) {
          add(typeof args.path === "string" ? args.path : "", parseWrittenSize(res));
        }
        break;
      case "deliver_file": {
        // Result: "Delivered <n> bytes to <abs>" or "Already in visible
        // workspace: <abs>". The visible name comes from arguments.dest
        // (relative); fall back to basename(src) when dest was omitted.
        if (!/^(Delivered \d+ bytes|Already in visible workspace)/.test(res)) break;
        const dest = typeof args.dest === "string" ? args.dest : "";
        const src = typeof args.src === "string" ? args.src : "";
        const dm = res.match(/^Delivered (\d+) bytes/);
        add(dest || (src ? src.split("/").pop() ?? "" : ""), dm ? parseInt(dm[1], 10) : undefined);
        break;
      }
      case "image_gen": {
        // persistImageGenOutput rewrites the result markdown to
        // ![](/workspace/<name>) with name = imagegen_<ms>_<idx><ext>.
        // No size is carried in the result.
        const re = /\/workspace\/([^\s)]+)/g;
        let m: RegExpExecArray | null;
        while ((m = re.exec(res))) add(m[1]);
        break;
      }
    }
  }
  return out;
}

// Post-turn workspace reconciliation. Diff the listing against the
// pre-turn snapshot, rewrite each tool-produced chip to the authoritative
// listed path (the live prefix guess can't know the store layout), and
// dedupe. Returns the exec-diff files (diagnostics only — they
// intentionally stay off bubbles) and the final chip set to attach to
// the last bubble.
export function reconcileTurnFiles(opts: {
  postTurnFiles: WorkspaceFile[];
  preSnap: Map<string, string>;
  turnFiles: ProducedFile[];
  seenPaths: Set<string>;
  sessionId: string;
  projectIdHint: string;
  wsRoot: string;
}): { diffFiles: ProducedFile[]; allFiles: ProducedFile[] } {
  const { postTurnFiles, preSnap, turnFiles, seenPaths, sessionId, projectIdHint, wsRoot } = opts;
  // exec-produced files (a Python script that saves PDFs, …) have no
  // tool-call record to anchor them to a bubble, so by design they don't
  // enter the message panel; the diff only feeds the dev diagnostic log.
  const diffFiles: ProducedFile[] = [];
  for (const f of postTurnFiles) {
    if (isSystemFile(f.path)) continue;
    const key = `${f.size}|${f.modTime}`;
    if (preSnap.get(f.path) === key) continue; // unchanged
    if (seenPaths.has(f.path)) continue;
    diffFiles.push({ path: f.path, size: f.size });
  }
  // Reconcile chip paths against the authoritative listing first: the
  // live prefix guess can't know the store layout (per-chat subdir
  // projects/<pid>/<sid>/ vs coding-root projects/<pid>/), and the URL
  // project hint is only present on a project chat's first send — so a
  // chip built mid-stream can point at a path that doesn't exist (its
  // download/preview 404s). Rewrite each chip to the listed path that
  // carries the same relative tail under this chat's scope.
  const listedByPath = new Map(postTurnFiles.map((f) => [f.path, f]));
  const reconcileChipPath = (p: string): string => {
    if (listedByPath.has(p)) return p;
    // Strip the scope prefix the chip was built with to recover rel.
    let rel = "";
    const sessPrefix = `sessions/${sessionId}/`;
    const projChat = projectIdHint ? `projects/${projectIdHint}/${sessionId}/` : "";
    const projRoot = projectIdHint ? `projects/${projectIdHint}/` : "";
    if (projChat && p.startsWith(projChat)) rel = p.slice(projChat.length);
    else if (projRoot && p.startsWith(projRoot)) rel = p.slice(projRoot.length);
    else if (p.startsWith(sessPrefix)) rel = p.slice(sessPrefix.length);
    if (!rel) return p;
    // Candidates across every layout this chat could write to: the
    // loose-chat session dir, this chat's subdir under any project
    // (listing is agent-wide, so the pid comes from the match itself),
    // the hinted project's root (coding-root agents), and the
    // backend-resolved workspace root (shared project files).
    const cands = [sessPrefix + rel];
    for (const lp of listedByPath.keys()) {
      const i = lp.indexOf(`/${sessionId}/`);
      if (i >= 0 && lp.slice(i + 1 + sessionId.length + 1) === rel) cands.push(lp);
    }
    if (projRoot) cands.push(projRoot + rel);
    if (wsRoot) cands.push(wsRoot + rel);
    for (const c of cands) if (listedByPath.has(c)) return c;
    return p;
  };
  const allFiles: ProducedFile[] = [];
  const chipSeen = new Set<string>();
  for (const f of turnFiles) {
    const p = reconcileChipPath(f.path);
    if (chipSeen.has(p)) continue;
    chipSeen.add(p);
    const listed = listedByPath.get(p);
    allFiles.push(listed ? { path: p, size: listed.size } : f);
  }
  return { diffFiles, allFiles };
}
