"use client";

// KbRow — the shared list row for KB master-detail lists (articles / notes
// / wiki / cards / flashes). Layout contract (web UI optimization plan,
// 右下槽位规则):
//
//   › 标题                                  14:20   ← 时间挂标题行右
//     描述灰小 · meta          [徽标 ⇄ hover 操作组]
//   ─────────────────────────────────── 发丝线从标题左缘起
//
// - The desc row's right slot is shared: the badge shows by default and
//   fades out on hover while the action group fades in over it; rows with
//   no badge simply reveal their actions on hover.
// - Single-line rows (no description and no badge) swap their title-row
//   TIME for the action group on hover instead.
// - Action order convention: primary (edit/open) → secondary → delete
//   (always rightmost, destructive). Hover-reveal is the visibility model
//   (mouse/keyboard first); the wrapper forces a ≥32px hit area via the
//   after:-inset-2 pseudo on every button.

import type { ReactNode } from "react";
import { ChevronRightIcon, type LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";
import { useLocale } from "@/lib/i18n";
import { formatFullTime, formatListTime } from "@/lib/time";
import { Button } from "@/components/ui/button";

const HIDE_ON_HOVER = "transition-opacity group-hover:opacity-0 group-focus-within:opacity-0";
const SHOW_ON_HOVER = "opacity-0 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100";
// Every action button gets a ≥32px hit area without growing the row.
const HIT_AREA = "[&_button]:relative [&_button]:after:absolute [&_button]:after:-inset-2";

export function KbRow({
  title,
  titleClassName,
  timeAt,
  description,
  badge,
  actions,
  selected,
  onClick,
  className,
}: {
  title: ReactNode;
  titleClassName?: string;
  /** Row timestamp — rendered as the formatListTime tier label in the title
      row's right slot, with the formatFullTime string as its tooltip. */
  timeAt?: string | number | Date | null;
  /** Gray-small second line (left). */
  description?: ReactNode;
  /** Desc-row right slot, default content (due badges, markers). */
  badge?: ReactNode;
  /** Hover action group — occupies the badge slot (or the title-row time
      slot on single-line rows). */
  actions?: ReactNode;
  selected?: boolean;
  onClick?: () => void;
  className?: string;
}) {
  const { locale } = useLocale();
  const time = formatListTime(timeAt, locale);
  const timeTitle = formatFullTime(timeAt, locale);
  const hasDescRow = description != null || badge != null;
  // Single-line rows promote the action group into the title row, where it
  // swaps places with the time label on hover.
  const titleSwap = !hasDescRow && actions != null;
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => {
        if (onClick && (e.key === "Enter" || e.key === " ")) {
          e.preventDefault();
          onClick();
        }
      }}
      className={cn(
        "group relative w-full cursor-pointer rounded-md px-3 py-1.5 text-left text-sm hover:bg-accent",
        // Hairline starts at the title's left edge (chevron gutter stays
        // clean); the last row in a list drops it.
        "after:absolute after:bottom-0 after:left-8 after:right-3 after:h-px after:bg-border/50 last:after:hidden",
        selected && "bg-accent",
        className,
      )}
    >
      <div className="flex items-center gap-2">
        <ChevronRightIcon className="h-3 w-3 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          <div className="relative flex items-center gap-2">
            <p className={cn("min-w-0 flex-1 truncate", titleClassName)}>{title}</p>
            {time && (
              <span
                className={cn(
                  "shrink-0 text-xs tabular-nums text-muted-foreground",
                  titleSwap && HIDE_ON_HOVER,
                )}
                title={timeTitle}
              >
                {time}
              </span>
            )}
            {titleSwap && (
              <span
                className={cn(
                  "absolute right-0 top-1/2 flex -translate-y-1/2 items-center gap-1",
                  SHOW_ON_HOVER,
                  HIT_AREA,
                )}
              >
                {actions}
              </span>
            )}
          </div>
          {hasDescRow && (
            <div className="mt-0.5 flex items-center gap-1.5 text-xs tabular-nums text-muted-foreground">
              {description != null && (
                <div className="flex min-w-0 flex-1 items-center gap-1.5 overflow-hidden whitespace-nowrap">
                  {description}
                </div>
              )}
              <span className="relative ml-auto flex shrink-0 items-center">
                {badge != null && (
                  <span className={cn("flex items-center gap-1", actions != null && HIDE_ON_HOVER)}>
                    {badge}
                  </span>
                )}
                {actions != null && (
                  <span
                    className={cn(
                      "flex items-center gap-1",
                      SHOW_ON_HOVER,
                      HIT_AREA,
                      badge != null && "absolute right-0 top-1/2 -translate-y-1/2",
                    )}
                  >
                    {actions}
                  </span>
                )}
              </span>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

// ListEmptyState — the unified empty state for KB list panes: icon in a
// tinted rounded square + one-line hint + primary action button (the
// /chats empty-state pattern, scaled down for narrow list panes).
export function ListEmptyState({
  icon: Icon,
  text,
  actionLabel,
  onAction,
}: {
  icon: LucideIcon;
  text: string;
  actionLabel?: string;
  onAction?: () => void;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 px-4 py-10 text-center">
      <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-primary/10">
        <Icon className="h-5 w-5 text-primary" />
      </div>
      <p className="text-xs text-muted-foreground">{text}</p>
      {actionLabel && onAction && (
        <Button variant="outline" size="sm" className="mt-1 h-7 text-xs" onClick={onAction}>
          {actionLabel}
        </Button>
      )}
    </div>
  );
}
