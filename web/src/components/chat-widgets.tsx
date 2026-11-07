"use client";

// Small presentational chat widgets extracted from chat-screen.tsx: the
// composer's slash-command menu (SlashMenu) and the live todo checklist
// above the conversation (TodoPanel), plus the slash command table they
// share with chat-screen's composer logic. Mirror of the dispatch table
// in internal/agent/slash.go — keep in sync when commands are
// added/removed/renamed there.

import { useState } from "react";
import Link from "next/link";
import { Check, ChevronDown, ChevronRight, Minus, Puzzle, SlidersHorizontal, Terminal } from "lucide-react";

import { type SkillInfo, type TodoItem } from "@/lib/api";
import { useT } from "@/lib/i18n";

export type SlashCommand = { name: string; description: string };
export const BUILTIN_COMMANDS: SlashCommand[] = [
  { name: "new", description: "Clear session history" },
  { name: "reset", description: "Clear session history" },
  { name: "retry", description: "Re-run last message" },
  { name: "undo", description: "Undo last turn" },
  { name: "compact", description: "Compress context window" },
  { name: "status", description: "Agent status & memory info" },
  { name: "usage", description: "Billing usage and session stats" },
  { name: "insights", description: "Activity insights (last N days)" },
  { name: "personality", description: "List or switch personality" },
  { name: "model", description: "Show or switch LLM model" },
  { name: "bookmark", description: "Save a web URL as a bookmark" },
  { name: "plan", description: "Plan mode: numbered plan, no tool calls" },
  { name: "goal", description: "Persistent multi-turn objective" },
  { name: "help", description: "Show command help" },
  { name: "version", description: "Show version" },
];
export const READ_ONLY_SLASH_COMMANDS = new Set([
  "help",
  "status",
  "usage",
  "insights",
  "version",
]);
export type SlashItem =
  | ({ kind: "command" } & SlashCommand)
  | ({ kind: "skill" } & SkillInfo);

export function SlashMenu({
  items,
  activeIndex,
  onHover,
  onSelect,
  agentId,
}: {
  items: SlashItem[];
  activeIndex: number;
  onHover: (i: number) => void;
  onSelect: (s: SlashItem) => void;
  agentId?: string;
}) {
  const t = useT();
  const manageHref = agentId ? `/agents/${agentId}/skills/` : "/skills/";
  return (
    <div className="absolute bottom-full left-0 right-0 mb-2 rounded-xl border border-border bg-popover shadow-lg overflow-hidden z-20">
      <div className="max-h-[320px] overflow-y-auto py-1">
        {items.map((it, i) => {
          const isCmd = it.kind === "command";
          const Icon = isCmd ? Terminal : Puzzle;
          const badge = isCmd ? "command" : (it.type || "skill");
          const label = isCmd ? `/${it.name}` : it.name;
          return (
            <button
              key={`${it.kind}-${it.name}`}
              // onMouseDown fires before the textarea's onBlur so the click
              // isn't swallowed by the blur-driven menu close.
              onMouseDown={(e) => {
                e.preventDefault();
                onSelect(it);
              }}
              onMouseEnter={() => onHover(i)}
              className={`w-full flex items-start gap-3 rounded-md px-3 py-2 text-left transition-colors ${
                i === activeIndex ? "bg-muted/60" : "hover:bg-muted/40"
              }`}
            >
              <Icon className="h-4 w-4 mt-0.5 shrink-0 text-muted-foreground" />
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2">
                  <p className="text-sm font-medium truncate">{label}</p>
                  <span className="text-xs uppercase tracking-wider text-muted-foreground/70">
                    {badge}
                  </span>
                </div>
                {it.description && (
                  <p className="text-xs text-muted-foreground line-clamp-1">
                    {it.description}
                  </p>
                )}
              </div>
            </button>
          );
        })}
      </div>
      <Link
        href={manageHref}
        className="flex items-center gap-2 border-t border-border px-3 py-2 text-xs text-muted-foreground hover:text-foreground hover:bg-muted/30 transition-colors"
      >
        <SlidersHorizontal className="h-3.5 w-3.5" />
        {t("chatScreen.manageSkills")}
      </Link>
    </div>
  );
}

// TodoPanel renders the per-session todo.md the agent maintains as a
// live progress checklist above the conversation. "Current step" is
// the first unchecked item — we surface it as a single line with a
// "<n>/<total>" counter, and the full list expands on click. Hidden
// entirely when no items exist (caller's responsibility — keeps this
// dumb-component pure).
export function TodoPanel({ items, active }: { items: TodoItem[]; active: boolean }) {
  const [open, setOpen] = useState(true);
  const total = items.length;
  const doneCount = items.filter((i) => i.done).length;
  const cancelledCount = items.filter((i) => i.cancelled).length;
  const allDone = doneCount === total;
  // A step counts as "finished" for dismiss purposes once it's done OR
  // cancelled — a cancelled plan has nothing left to track either, so the
  // panel should hide the same way an all-done one does.
  const allTerminated = doneCount + cancelledCount === total;
  // First open item is the live "in progress" step. When every step is
  // finished (done or cancelled), fall through to the last item so the
  // collapsed header still says something concrete.
  const currentIdx = allTerminated ? total - 1 : items.findIndex((i) => !i.done && !i.cancelled);
  const current = currentIdx >= 0 ? items[currentIdx] : null;
  return (
    // Wrapper keeps the panel aligned with the composer's max-w-2xl
    // column. Only the inner div carries border/background, so the
    // surrounding chat area stays clean — no full-width strip across
    // the page.
    <div className="shrink-0 px-4 pt-2">
      <div className="mx-auto max-w-2xl">
        <div className="rounded-lg border border-border bg-muted/40 px-3 py-2 shadow-sm">
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            className="flex w-full items-center gap-2 text-left text-sm"
            aria-expanded={open}
          >
            {allDone ? (
              <Check className="size-4 shrink-0 text-success" />
            ) : active ? (
              <div className="size-4 shrink-0 rounded-full border-2 border-warning border-t-transparent animate-spin" />
            ) : (
              // Paused: agent isn't streaming. Show a static amber ring
              // so the "where we are" cue is visible without implying
              // ongoing work.
              <div className="size-4 shrink-0 rounded-full border-2 border-warning/70" />
            )}
            <span className="font-medium tabular-nums text-muted-foreground">
              {doneCount}/{total}
            </span>
            <span className="truncate flex-1">
              {current ? current.text : "Plan checklist"}
            </span>
            {open ? (
              <ChevronDown className="size-4 shrink-0 text-muted-foreground" />
            ) : (
              <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
            )}
          </button>
          {open && (
            <ol className="mt-2 space-y-1 border-t border-border/60 pt-2 text-sm">
              {items.map((it, i) => {
                const isCurrent = i === currentIdx && !it.done && !it.cancelled;
                return (
                  <li
                    key={i}
                    className={
                      "flex items-start gap-2 rounded px-1.5 py-0.5 " +
                      (isCurrent ? "bg-warning/10" : "")
                    }
                  >
                    {it.done ? (
                      <Check className="mt-0.5 size-3.5 shrink-0 text-success" />
                    ) : it.cancelled ? (
                      <Minus className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
                    ) : isCurrent ? (
                      active ? (
                        <div className="mt-0.5 size-3.5 shrink-0 rounded-full border-2 border-warning border-t-transparent animate-spin" />
                      ) : (
                        <div className="mt-0.5 size-3.5 shrink-0 rounded-full border-2 border-warning/70" />
                      )
                    ) : (
                      <div className="mt-1 size-2.5 shrink-0 rounded-full border border-muted-foreground/40" />
                    )}
                    <span
                      className={
                        ((it.done || it.cancelled) ? "line-through text-muted-foreground/70 " : "") +
                        (isCurrent ? "font-medium" : "")
                      }
                    >
                      {it.text}
                    </span>
                  </li>
                );
              })}
            </ol>
          )}
        </div>
      </div>
    </div>
  );
}
