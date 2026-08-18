"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  CalendarIcon,
  XIcon,
} from "lucide-react";
import {
  type KBCard,
  reviewCard,
} from "@/lib/api";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";

// CardsReview — the full-screen spaced-repetition flow (the Anki-style
// "one card at a time" session). The queue arrives pre-fetched (due cards,
// oldest-due first is the caller's job; the list endpoint returns newest-
// created first which is fine). Interactions: click/space flips the card,
// then 1/2/3 (or the buttons) grade it and advance. Esc exits mid-session
// (grades already given are kept). Ends with a summary: counts + remember
// rate + tomorrow's due count (re-read via the parent's onDone callback).

type Grade = "forgot" | "fuzzy" | "remembered";
const GRADES: { key: Grade; kbd: string; cls: string }[] = [
  { key: "forgot", kbd: "1", cls: "border-destructive/40 text-destructive hover:bg-destructive/10" },
  { key: "fuzzy", kbd: "2", cls: "border-warning/50 text-warning hover:bg-warning/10" },
  { key: "remembered", kbd: "3", cls: "border-emerald-500/40 text-emerald-600 hover:bg-emerald-500/10 dark:text-emerald-400" },
];

export function CardsReview({
  agentId,
  queue,
  onDone,
}: {
  agentId: string;
  queue: KBCard[];
  onDone: (result: { done: number; remembered: number; fuzzy: number; forgot: number }) => void;
}) {
  const t = useT();
  const [idx, setIdx] = useState(0);
  const [flipped, setFlipped] = useState(false);
  const [saving, setSaving] = useState(false);
  const [result, setResult] = useState({ done: 0, remembered: 0, fuzzy: 0, forgot: 0 });
  const resultRef = useRef(result);
  // Keep the ref in sync for the Esc handler (which needs the latest
  // counts without re-binding the keydown listener on every grade).
  useEffect(() => { resultRef.current = result; }, [result]);

  const finished = idx >= queue.length;

  const handleGrade = useCallback(
    async (grade: Grade) => {
      if (!flipped || saving || finished) return;
      const card = queue[idx];
      setSaving(true);
      try {
        await reviewCard(agentId, card.id, grade);
      } catch {
        // Grade failed (network/etc) — still advance so a broken card
        // can't wedge the session; the card stays due for tomorrow.
      }
      setSaving(false);
      setResult((r) => ({
        done: r.done + 1,
        remembered: r.remembered + (grade === "remembered" ? 1 : 0),
        fuzzy: r.fuzzy + (grade === "fuzzy" ? 1 : 0),
        forgot: r.forgot + (grade === "forgot" ? 1 : 0),
      }));
      setFlipped(false);
      setIdx((i) => i + 1);
    },
    [flipped, saving, finished, queue, idx, agentId],
  );

  // Keyboard: space/enter flip, 1/2/3 grade (only once flipped), Esc exits
  // (onDone keeps the grades already given).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onDone(resultRef.current);
        return;
      }
      if (finished) return;
      if (e.key === " " || e.key === "Enter") {
        e.preventDefault();
        setFlipped((f) => !f);
        return;
      }
      if (flipped && (e.key === "1" || e.key === "2" || e.key === "3")) {
        e.preventDefault();
        const g = GRADES[Number(e.key) - 1];
        if (g) handleGrade(g.key);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [finished, flipped, handleGrade, onDone]);

  if (queue.length === 0) return null;

  if (finished) {
    const total = result.done;
    const rate = total > 0 ? Math.round((result.remembered / total) * 100) : 0;
    return (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-background/95 p-6">
        <div className="w-full max-w-md space-y-5 text-center">
          <h2 className="text-xl font-semibold">{t("cards.reviewDoneTitle")}</h2>
          <p className="text-sm text-muted-foreground">
            {t("cards.reviewDoneStats", { n: total, r: result.remembered, f: result.fuzzy, g: result.forgot })}
          </p>
          <p className="text-3xl font-bold tabular-nums text-primary">{rate}%</p>
          <Button onClick={() => onDone(result)}>{t("common.close")}</Button>
        </div>
      </div>
    );
  }

  const card = queue[idx];
  return (
    <div className="fixed inset-0 z-50 flex flex-col bg-background/95">
      {/* progress */}
      <div className="flex items-center gap-3 px-4 py-3">
        <span className="text-xs tabular-nums text-muted-foreground">
          {t("cards.reviewProgress", { cur: idx + 1, total: queue.length })}
        </span>
        <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
          <div
            className="h-full rounded-full bg-primary transition-all"
            style={{ width: `${((idx + 1) / queue.length) * 100}%` }}
          />
        </div>
        <Button
          variant="ghost"
          size="sm"
          className="h-7 w-7 p-0"
          aria-label={t("cards.reviewExit")}
          onClick={() => onDone(result)}
        >
          <XIcon className="size-4" />
        </Button>
      </div>

      {/* the card */}
      <div className="flex flex-1 items-center justify-center px-4">
        <button
          type="button"
          onClick={() => setFlipped((f) => !f)}
          className={cn(
            "w-full max-w-lg rounded-2xl border bg-card p-8 text-left shadow-lg transition-colors",
            flipped ? "border-primary/50" : "hover:border-primary/30",
          )}
        >
          <div className="mb-3 flex items-center gap-1.5">
            <Badge variant="outline" className="px-1.5 py-0 text-[10px] text-muted-foreground">
              {t(`cards.source.${card.source_type}`)}
            </Badge>
            {card.review_count > 0 && (
              <span className="text-[11px] text-muted-foreground">
                {t("cards.intervalProgress", { cur: card.interval_index, total: 6 })}
              </span>
            )}
          </div>
          {flipped ? (
            <div className="space-y-4">
              <p className="break-words text-sm text-muted-foreground">{card.question}</p>
              <div className="whitespace-pre-wrap break-words text-xl font-medium leading-relaxed">
                {card.answer}
              </div>
              {card.source_excerpt && (
                <p className="border-l-2 border-border pl-3 text-xs text-muted-foreground">{card.source_excerpt}</p>
              )}
              {card.source_ref && card.source_type === "diary" && (
                <p className="flex items-center gap-1 text-xs text-muted-foreground">
                  <CalendarIcon className="size-3" />
                  {t("cards.fromDiary", { date: card.source_ref })}
                </p>
              )}
            </div>
          ) : (
            <p className="break-words pt-2 text-2xl font-medium leading-relaxed">{card.question}</p>
          )}
          {!flipped && (
            <p className="pt-8 text-xs text-muted-foreground">{t("cards.flipHint")}</p>
          )}
        </button>
      </div>

      {/* grades */}
      <div className="px-4 pb-6 pt-2">
        {flipped ? (
          <div className="mx-auto flex max-w-lg gap-2">
            {GRADES.map((g) => (
              <Button
                key={g.key}
                variant="outline"
                className={cn("h-11 flex-1 flex-col gap-0 py-1", g.cls)}
                disabled={saving}
                onClick={() => handleGrade(g.key)}
              >
                <span className="text-sm">{t(`cards.grade.${g.key}`)}</span>
                <span className="text-[10px] opacity-60">{g.kbd}</span>
              </Button>
            ))}
          </div>
        ) : (
          <p className="text-center text-xs text-muted-foreground">
            {t("cards.flipHint")} · 空格
          </p>
        )}
      </div>
    </div>
  );
}
