"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import {
  Flame,
  Layers,
  Loader2,
  ListTodo,
  MessageSquare,
  Timer,
} from "lucide-react";
import { CardDeck } from "@/components/cards-review";
import { ChannelIcon, channelLabel } from "@/components/channel-icon";
import {
  type AgentCronJob,
  type ChatSessionEntry,
  type KBCardStats,
  type KBSource,
  getCardStats,
  getChatSessions,
  kbListTodos,
  listAgentCronJobs,
} from "@/lib/api";
import { useT } from "@/lib/i18n";

// ChatDashboard — the agent board that replaces the blank Manus-style
// empty state. Four light modules over data the backend already has:
// today's due cards (the shared CardDeck, inlined, so a review session
// is one tap from landing), the agent's OPEN knowledge-base todos
// (kb_sources type=todo — NOT the per-chat todo.md checklists), the
// latest conversations across channels (web + IM via ChannelIcon), and
// the enabled cron jobs with their next fire time. The parent only
// mounts this on a fresh chat with an untouched composer — typing hides
// it again (the "empty state IS the dashboard" behavior); each module
// is an entry into an existing page, no new interaction surfaces beyond
// the deck itself.
//
// Panes size to their content (grid items-start, lists clamp with
// max-h + scroll) so a pane with little data reads as a small card
// instead of a fixed-height box of air.

const RECENT_SESSIONS = 5;
const TODO_PREVIEW = 6;

// Open KB todos only — done/cancelled rows stay in the todos page.
const isOpenTodo = (s: KBSource) => s.status === "pending" || s.status === "in_progress";

export function ChatDashboard({ agentId }: { agentId: string }) {
  const t = useT();
  // undefined = still loading, null = loaded-and-empty (only todos makes
  // that distinction; sessions' empty state renders an inline hint).
  const [stats, setStats] = useState<KBCardStats | null>(null);
  const [todos, setTodos] = useState<KBSource[] | null | undefined>(undefined);
  const [sessions, setSessions] = useState<ChatSessionEntry[] | null>(null);
  const [crons, setCrons] = useState<AgentCronJob[] | null>(null);
  // "Now" for the relative timestamps — stamped alongside the async data
  // arrival (not during render: Date.now() there trips the purity lint
  // and risks hydration mismatch under static export). 0 = not yet
  // stamped → render no time.
  const [nowTs, setNowTs] = useState(0);

  useEffect(() => {
    let alive = true;
    getCardStats(agentId).then((s) => alive && setStats(s));
    // Board rows show title only (the full markdown body lives in KB
    // entries — fetching those is an N+1 the board doesn't need).
    kbListTodos(agentId).then((ts) => {
      if (!alive) return;
      setTodos(ts.filter(isOpenTodo));
    });
    // Order isn't guaranteed by the backend across modes — sort locally
    // so the board always leads with the most recently touched thread.
    getChatSessions(agentId).then((ss) => {
      if (!alive) return;
      setNowTs(Date.now());
      setSessions([...ss].sort((a, b) => (b.updatedAt ?? 0) - (a.updatedAt ?? 0)).slice(0, RECENT_SESSIONS));
    });
    listAgentCronJobs(agentId).then((js) =>
      alive && setCrons(js.filter((j) => j.enabled).slice(0, RECENT_SESSIONS)),
    );
    return () => {
      alive = false;
    };
  }, [agentId]);

  // Deck finished → refetch so the pane flips to the "nothing due"
  // resting state (or a fresh count, if tomorrow's queue leaked in).
  const onDeckFinish = () => {
    getCardStats(agentId).then((s) => setStats(s));
  };

  const relativeTime = (ts?: number): string => {
    if (!ts || !nowTs) return "";
    const diff = nowTs - ts;
    if (diff < 60_000) return t("dashboard.time.justNow");
    if (diff < 3600_000) return t("dashboard.time.minAgo", { n: Math.floor(diff / 60_000) });
    if (diff < 86400_000) return t("dashboard.time.hourAgo", { n: Math.floor(diff / 3600_000) });
    if (diff < 7 * 86400_000) return t("dashboard.time.dayAgo", { n: Math.floor(diff / 86400_000) });
    return new Date(ts).toLocaleDateString([], { month: "short", day: "numeric" });
  };

  return (
    // max-w-6xl: four panes need more room than the composer's max-w-2xl,
    // but stay centered under the hero for visual kinship. items-start
    // lets each pane keep its own content-driven height.
    <div className="mx-auto grid w-full max-w-6xl items-start gap-4 md:grid-cols-2 xl:grid-cols-4">
      {/* ── Today's cards ── */}
      <section className="flex flex-col overflow-hidden rounded-xl border bg-card">
        <div className="flex items-center gap-2 px-4 pt-3 text-sm font-medium">
          <Layers className="size-4 text-muted-foreground" />
          {t("dashboard.cards.title")}
          {stats && stats.due_today > 0 && (
            <span className="ml-auto rounded-full bg-primary/10 px-2 py-0.5 text-xs tabular-nums text-primary">
              {t("dashboard.cards.dueCount", { n: stats.due_today })}
            </span>
          )}
        </div>
        <div className="min-h-0 flex-1">
          {stats === null ? (
            <div className="flex h-24 items-center justify-center">
              <Loader2 className="size-5 animate-spin text-muted-foreground" />
            </div>
          ) : stats.due_today > 0 ? (
            <CardDeck agentId={agentId} variant="inline" onFinish={onDeckFinish} />
          ) : (
            <div className="flex flex-col items-center justify-center gap-3 p-6 text-center">
              <Layers className="size-8 text-muted-foreground/50" />
              <p className="text-sm text-muted-foreground">{t("dashboard.cards.none")}</p>
              {stats.streak_days > 0 && (
                <p className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                  <Flame className="size-3.5 text-warning" />
                  {t("dashboard.cards.streak", { n: stats.streak_days })}
                </p>
              )}
              <Link
                href={`/agents/${agentId}/knowledge/cards`}
                className="text-xs text-primary hover:underline"
              >
                {t("dashboard.cards.browse")}
              </Link>
            </div>
          )}
        </div>
      </section>

      {/* ── Open KB todos ── */}
      <section className="flex flex-col overflow-hidden rounded-xl border bg-card">
        <div className="flex items-center gap-2 px-4 pt-3 text-sm font-medium">
          <ListTodo className="size-4 text-muted-foreground" />
          {t("dashboard.todo.title")}
          {todos && todos.length > 0 && (
            <span className="ml-auto text-xs tabular-nums text-muted-foreground">{todos.length}</span>
          )}
        </div>
        {todos === undefined ? (
          <div className="flex h-24 items-center justify-center">
            <Loader2 className="size-5 animate-spin text-muted-foreground" />
          </div>
        ) : todos === null || todos.length === 0 ? (
          <div className="flex flex-col items-center justify-center gap-3 p-6 text-center">
            <ListTodo className="size-8 text-muted-foreground/50" />
            <p className="text-sm text-muted-foreground">{t("dashboard.todo.none")}</p>
          </div>
        ) : (
          <div className="flex min-h-0 flex-col">
            <ul className="max-h-[420px] space-y-1 overflow-y-auto px-3 py-2 text-sm">
              {[...todos]
                .sort((a, b) => {
                  // Overdue + soonest due first; undated ones trail by recency.
                  const due = (s: KBSource) => (s.end_at ? new Date(s.end_at).getTime() : Infinity);
                  const da = due(a);
                  const db = due(b);
                  if (da !== db) return da - db;
                  return (b.updated_at ? new Date(b.updated_at).getTime() : 0) -
                    (a.updated_at ? new Date(a.updated_at).getTime() : 0);
                })
                .slice(0, TODO_PREVIEW)
                .map((td) => (
                  <TodoRow key={td.id} todo={td} nowTs={nowTs} />
                ))}
            </ul>
            <Link
              href={`/agents/${agentId}/knowledge/todos/`}
              className="border-t border-border/60 px-4 py-2.5 text-xs text-primary hover:underline"
            >
              {t("dashboard.todo.view")}
            </Link>
          </div>
        )}
      </section>

      {/* ── Recent chats ── */}
      <section className="flex flex-col overflow-hidden rounded-xl border bg-card">
        <div className="flex items-center gap-2 px-4 pt-3 text-sm font-medium">
          <MessageSquare className="size-4 text-muted-foreground" />
          {t("dashboard.recent.title")}
        </div>
        {sessions === null ? (
          <div className="flex h-24 items-center justify-center">
            <Loader2 className="size-5 animate-spin text-muted-foreground" />
          </div>
        ) : sessions.length === 0 ? (
          <div className="flex flex-col items-center justify-center gap-3 p-6 text-center">
            <MessageSquare className="size-8 text-muted-foreground/50" />
            <p className="text-sm text-muted-foreground">{t("dashboard.recent.none")}</p>
          </div>
        ) : (
          <ul className="max-h-[420px] divide-y divide-border/60 overflow-y-auto py-1 text-sm">
            {sessions.map((s) => (
              <li key={s.id}>
                <Link
                  href={`/agents/${agentId}/chat/${s.id}`}
                  className="flex items-center gap-2 px-4 py-2.5 hover:bg-muted/40"
                  title={channelLabel(s.channel)}
                >
                  <ChannelIcon channel={s.channel} />
                  <span className="min-w-0 flex-1 truncate">{s.title || s.preview || s.id}</span>
                  <span className="shrink-0 text-xs text-muted-foreground">
                    {relativeTime(s.updatedAt)}
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* ── Cron jobs ── */}
      <section className="flex flex-col overflow-hidden rounded-xl border bg-card">
        <div className="flex items-center gap-2 px-4 pt-3 text-sm font-medium">
          <Timer className="size-4 text-muted-foreground" />
          {t("dashboard.cron.title")}
        </div>
        {crons === null ? (
          <div className="flex h-24 items-center justify-center">
            <Loader2 className="size-5 animate-spin text-muted-foreground" />
          </div>
        ) : crons.length === 0 ? (
          <div className="flex flex-col items-center justify-center gap-3 p-6 text-center">
            <Timer className="size-8 text-muted-foreground/50" />
            <p className="text-sm text-muted-foreground">{t("dashboard.cron.none")}</p>
          </div>
        ) : (
          <div className="flex min-h-0 flex-col">
            <ul className="max-h-[420px] divide-y divide-border/60 overflow-y-auto py-1 text-sm">
              {crons.map((j) => (
                <li key={j.id} className="flex items-center gap-2 px-4 py-2.5">
                  <span className="min-w-0 flex-1 truncate" title={j.schedule}>
                    {j.name || j.message || j.id}
                  </span>
                  <span className="shrink-0 text-xs text-muted-foreground" title={j.schedule}>
                    {j.nextRun
                      ? t("dashboard.cron.next", { time: shortDateTime(j.nextRun ?? "", nowTs) })
                      : t("dashboard.cron.notScheduled")}
                  </span>
                </li>
              ))}
            </ul>
            <Link
              href={`/agents/${agentId}/scheduler`}
              className="border-t border-border/60 px-4 py-2.5 text-xs text-primary hover:underline"
            >
              {t("dashboard.cron.manage")}
            </Link>
          </div>
        )}
      </section>
    </div>
  );
}

// shortDateTime renders an ISO timestamp for the todo/cron due labels:
// HH:mm on the stamped "today", else a short date + time. nowTs is the
// load-time stamp (0 → best-effort with the parsed date only).
function shortDateTime(iso: string, nowTs: number): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const sameDay = nowTs !== 0 && new Date(nowTs).toDateString() === d.toDateString();
  return sameDay
    ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleDateString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// TodoRow renders one open KB todo: a hollow bullet for pending, the
// warning ring for in_progress (matching the TodoCard status colors),
// title text, and the due time — destructive-styled once past.
function TodoRow({ todo, nowTs }: { todo: KBSource; nowTs: number }) {
  const t = useT();
  const overdue = nowTs !== 0 && new Date(todo.end_at ?? "").getTime() < nowTs;
  return (
    <li className="flex items-start gap-2 rounded px-1.5 py-0.5">
      {todo.status === "in_progress" ? (
        <div className="mt-1 size-3 shrink-0 rounded-full border-2 border-warning" />
      ) : (
        <div className="mt-1 size-2.5 shrink-0 rounded-full border border-muted-foreground/40" />
      )}
      <span className="min-w-0 flex-1 break-words">{todo.title}</span>
      {todo.end_at && (
        <span
          className={
            "shrink-0 text-xs " +
            (overdue ? "font-medium text-destructive" : "text-muted-foreground")
          }
        >
          {overdue
            ? t("dashboard.todo.overdue")
            : t("dashboard.todo.dueAt", { time: shortDateTime(todo.end_at, nowTs) })}
        </span>
      )}
    </li>
  );
}
