"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CalendarIcon, ExternalLinkIcon, FileTextIcon, Loader2Icon, XIcon } from "lucide-react";
import { type KBCard, getAgentConfig, getWikiPage, listCards, reviewCard } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";

// CardDeck — the swipe-deck review session (the reference screenshot's
// stacked-card UX). Today's due cards render as a fanned pile; the front
// card can be tapped to flip (question → answer + source) or dragged:
// past the threshold to the LEFT grades 忘了 (stays in rotation), to the
// RIGHT grades 记得 (advances the Ebbinghaus ladder). 模糊 has no swipe —
// it stays a button. One component, two mounts: `overlay` (开始复习 /
// ?review=1 deep link) and `inline` (the cards page's right pane shows
// the deck by default while nothing is selected). The deck fetches its
// own due queue on mount and reports the session tally via onFinish.

type Grade = "forgot" | "fuzzy" | "remembered";

export type CardDeckResult = { done: number; remembered: number; fuzzy: number; forgot: number };

// SWIPE_PX is the horizontal drag distance past which releasing grades
// the card (in the drag direction) instead of springing back. The
// vertical (down = 模糊) threshold is a touch larger so accidental
// downward drift during a horizontal swipe doesn't trigger it.
const SWIPE_PX = 90;
const SWIPE_PY = 110;
// CLICK_PX distinguishes a tap (flip) from a drag (swipe).
const CLICK_PX = 8;
// FLY_MS is the fly-off animation before the next card advances.
const FLY_MS = 240;

export function CardDeck({
  agentId,
  variant,
  practice,
  onFinish,
}: {
  agentId: string;
  variant: "overlay" | "inline";
  // practice switches to self-test mode: the queue is the active pool
  // instead of today's due set, and grades are counted locally but NOT
  // sent to the API — a re-run never advances/resets the Ebbinghaus
  // schedule. Backs the 再练一轮 button once today's due set is cleared.
  practice?: boolean;
  onFinish: (result: CardDeckResult) => void;
}) {
  const t = useT();
  const [queue, setQueue] = useState<KBCard[] | null>(null); // null = loading
  const [idx, setIdx] = useState(0);
  const [flipped, setFlipped] = useState(false);
  const [result, setResult] = useState<CardDeckResult>({ done: 0, remembered: 0, fuzzy: 0, forgot: 0 });
  // anim: "" idle | "out-left"/"out-right"/"out-down" the graded card flying off.
  const [anim, setAnim] = useState<"" | "out-left" | "out-right" | "out-down">("");
  const [saving, setSaving] = useState(false);
  // Drag state: dragX/dragY are the live offsets (state-driven so the
  // verdict badges track), moved remembers whether the pointer ever left
  // click radius. dragStartRef holds the pointerdown origin.
  const [dragX, setDragX] = useState(0);
  const [dragY, setDragY] = useState(0);
  const [dragging, setDragging] = useState(false);
  const dragStartRef = useRef({ x: 0, y: 0 });
  const movedRef = useRef(false);
  const resultRef = useRef(result);
  useEffect(() => { resultRef.current = result; }, [result]);
  const closedRef = useRef(false);
  useEffect(() => { closedRef.current = false; }, []);

  useEffect(() => {
    let alive = true;
    // (async iife: the due queue needs the agent's reviewLimit first, so
    // one capped fetch replaces the old pull-everything-then-reverse.)
    (async () => {
      let limit = 20;
      try {
        const cfg = await getAgentConfig(agentId);
        const n = cfg?.cards?.reviewLimit ?? 0;
        if (n > 0) limit = n;
      } catch { /* cfg read failed — default cap */ }
      const opts = practice
        ? { filter: "active", limit: 50 } // practice pool: everything in rotation
        : { queue: true, limit }; // review feed: most-overdue first, capped
      const cards = await listCards(agentId, opts);
      if (alive && !closedRef.current) setQueue(cards);
    })();
    return () => { alive = false; closedRef.current = true; };
  }, [agentId, practice]);

  const finished = queue !== null && idx >= queue.length;

  // wikiTitle resolves the current card's wiki source_ref (a "type:slug"
  // page id) into the page's title so the flip-side deep link reads as the
  // page's name, not the generic "查看 Wiki 原文". Diary links already
  // carry their date. Stashed with the card id it was fetched for — the
  // render only trusts it while that card is still front, so an advancing
  // deck never flashes the previous card's title.
  const [wikiTitle, setWikiTitle] = useState<{ id: string; title: string } | null>(null);
  useEffect(() => {
    const c = queue !== null && idx < queue.length ? queue[idx] : null;
    if (!c || c.source_type !== "wiki" || !c.source_ref) return;
    let alive = true;
    getWikiPage(agentId, c.source_ref)
      .then((p) => { if (alive && p?.title) setWikiTitle({ id: c.id, title: p.title }); })
      .catch(() => {});
    return () => { alive = false; };
  }, [agentId, queue, idx]);

  const grade = useCallback(
    async (g: Grade, via: "swipe" | "button" | "key") => {
      if (saving || anim !== "" || queue === null || idx >= queue.length) return;
      const card = queue[idx];
      if (via === "swipe") setAnim(g === "remembered" ? "out-right" : g === "fuzzy" ? "out-down" : "out-left");
      if (!practice) {
        setSaving(true);
        try {
          await reviewCard(agentId, card.id, g);
        } catch {
          // Grade failed (network/etc) — still advance so one broken card
          // can't wedge the session; it stays due for tomorrow.
        }
        setSaving(false);
      }
      setResult((r) => ({
        done: r.done + 1,
        remembered: r.remembered + (g === "remembered" ? 1 : 0),
        fuzzy: r.fuzzy + (g === "fuzzy" ? 1 : 0),
        forgot: r.forgot + (g === "forgot" ? 1 : 0),
      }));
      setFlipped(false);
      setDragX(0);
      setDragY(0);
      if (via === "swipe") {
        window.setTimeout(() => { setAnim(""); setIdx((i) => i + 1); }, FLY_MS);
      } else {
        setIdx((i) => i + 1);
      }
    },
    [saving, anim, queue, idx, agentId, practice],
  );

  // Keyboard: space/enter flip, ← 忘了 / ↓ 模糊 / → 记得 (matching the
  // swipe directions), 1/2/3 the buttons, Esc exits early (overlay only)
  // with the grades already given kept. Overlay-only by design: the
  // inline deck stays mounted beneath the overlay, and two window-level
  // listeners would double-grade every keypress.
  useEffect(() => {
    if (variant !== "overlay") return;
    const onKey = (e: KeyboardEvent) => {
      if (finished) return;
      if (e.key === "Escape" && variant === "overlay") {
        e.preventDefault();
        onFinish(resultRef.current);
        return;
      }
      if (e.key === " " || e.key === "Enter") {
        e.preventDefault();
        setFlipped((f) => !f);
        return;
      }
      if (e.key === "ArrowLeft" || e.key === "1") { e.preventDefault(); grade("forgot", "key"); return; }
      if (e.key === "ArrowDown" || e.key === "2") { e.preventDefault(); grade("fuzzy", "key"); return; }
      if (e.key === "ArrowRight" || e.key === "3") { e.preventDefault(); grade("remembered", "key"); }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [finished, grade, onFinish, variant]);

  // ── Drag mechanics (pointer events cover mouse + touch). ──
  const onPointerDown = (e: React.PointerEvent) => {
    if (anim !== "" || finished || queue === null || idx >= queue.length) return;
    // Capture keeps moves flowing when the cursor leaves the card. Can
    // throw for a pointerId with no live pointer (synthetic events, or a
    // released-mid-air race) — the drag still works without capture.
    try {
      (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    } catch { /* pointer already gone — proceed capture-less */ }
    dragStartRef.current = { x: e.clientX, y: e.clientY };
    movedRef.current = false;
    setDragging(true);
  };
  const onPointerMove = (e: React.PointerEvent) => {
    if (!dragging) return;
    const dx = e.clientX - dragStartRef.current.x;
    const dy = e.clientY - dragStartRef.current.y;
    if (Math.abs(dx) > CLICK_PX || Math.abs(dy) > CLICK_PX) movedRef.current = true;
    setDragX(dx);
    setDragY(dy);
  };
  const onPointerUp = () => {
    if (!dragging) return;
    setDragging(false);
    if (!movedRef.current) {
      setDragX(0);
      setDragY(0);
      setFlipped((f) => !f); // tap = flip
      return;
    }
    // The dominant axis wins: down = 模糊, otherwise left/right.
    if (dragY > SWIPE_PY && dragY > Math.abs(dragX)) grade("fuzzy", "swipe");
    else if (dragX > SWIPE_PX) grade("remembered", "swipe");
    else if (dragX < -SWIPE_PX) grade("forgot", "swipe");
    else { setDragX(0); setDragY(0); } // spring back
  };

  const card = queue !== null && idx < queue.length ? queue[idx] : null;
  const next = queue !== null && idx + 1 < queue.length ? queue[idx + 1] : null;
  const next2 = queue !== null && idx + 2 < queue.length ? queue[idx + 2] : null;

  const body = (() => {
    if (queue === null) {
      return (
        <div className="flex flex-1 items-center justify-center">
          <Loader2Icon className="size-5 animate-spin text-muted-foreground" />
        </div>
      );
    }
    if (finished) {
      const total = result.done;
      const rate = total > 0 ? Math.round((result.remembered / total) * 100) : 0;
      return (
        <div className="flex flex-1 flex-col items-center justify-center gap-5 p-6 text-center">
          <h2 className="text-xl font-semibold">
            {practice ? t("cards.practiceDoneTitle") : t("cards.reviewDoneTitle")}
          </h2>
          <p className="text-sm text-muted-foreground">
            {t("cards.reviewDoneStats", { n: total, r: result.remembered, f: result.fuzzy, g: result.forgot })}
          </p>
          <p className="text-3xl font-bold tabular-nums text-primary">{rate}%</p>
          <Button onClick={() => onFinish(result)}>{t("common.close")}</Button>
        </div>
      );
    }
    return (
      <>
        {/* the pile: front card + up to two peeking behind. Both variants
            cap at max-w-md so the card spans exactly the grade-button row
            below (忘了+模糊+记得 combined width). */}
        <div className={cn("relative flex flex-1 items-center justify-center", variant === "inline" ? "px-2" : "px-6")}>
          <div className="relative w-full max-w-md">
            {[next2, next].map((c, i) =>
              c ? (
                <div
                  key={c.id}
                  aria-hidden
                  className="absolute inset-0 rounded-2xl border bg-card shadow-sm"
                  style={{
                    transform: `translateY(${(2 - i) * 10}px) scale(${1 - (2 - i) * 0.04})`,
                    opacity: 1 - (2 - i) * 0.25,
                  }}
                />
              ) : null,
            )}
            {card && (
              <div
                role="button"
                tabIndex={0}
                aria-label={card.question}
                onPointerDown={onPointerDown}
                onPointerMove={onPointerMove}
                onPointerUp={onPointerUp}
                onPointerCancel={() => { setDragging(false); setDragX(0); setDragY(0); }}
                onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); setFlipped((f) => !f); } }}
                style={{
                  transform:
                    anim === "out-left" ? "translateX(-130%) rotate(-14deg)"
                    : anim === "out-right" ? "translateX(130%) rotate(14deg)"
                    : anim === "out-down" ? "translateY(130%)"
                    : `translate(${dragX}px, ${dragY}px) rotate(${dragX * 0.05}deg)`,
                  transition: dragging ? "none" : "transform 220ms cubic-bezier(.2,.8,.3,1)",
                }}
                className={cn(
                  "relative z-10 min-h-[280px] cursor-grab select-none rounded-2xl border bg-card p-6 text-left shadow-lg",
                  variant === "inline" && "min-h-[320px] md:min-h-[380px]",
                  "touch-none focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary",
                  flipped ? "border-primary/50" : "hover:border-primary/30",
                  anim === "" || "pointer-events-none",
                )}
              >
                {/* swipe verdict badges — opacity follows the drag. Left =
                    忘了, right = 记得, down = 模糊. */}
                <span
                  aria-hidden
                  style={{ opacity: Math.min(1, Math.max(0, -dragX / SWIPE_PX)) }}
                  className="pointer-events-none absolute left-3 top-3 -rotate-12 rounded-md border border-destructive/50 bg-destructive/10 px-2 py-0.5 text-xs font-medium text-destructive"
                >
                  {t("cards.deckLeft")}
                </span>
                <span
                  aria-hidden
                  style={{ opacity: Math.min(1, Math.max(0, dragX / SWIPE_PX)) }}
                  className="pointer-events-none absolute right-3 top-3 rotate-12 rounded-md border border-emerald-500/50 bg-emerald-500/10 px-2 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400"
                >
                  {t("cards.deckRight")}
                </span>
                <span
                  aria-hidden
                  style={{ opacity: Math.min(1, Math.max(0, dragY / SWIPE_PY)) }}
                  className="pointer-events-none absolute bottom-3 left-1/2 -translate-x-1/2 rounded-md border border-warning/50 bg-warning/10 px-2 py-0.5 text-xs font-medium text-warning"
                >
                  {t("cards.grade.fuzzy")}
                </span>

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
                  <div className="space-y-3">
                    <p className="break-words text-sm text-muted-foreground">{card.question}</p>
                    <div className="whitespace-pre-wrap break-words text-lg font-medium leading-relaxed">{card.answer}</div>
                    {card.source_excerpt && (
                      <p className="border-l-2 border-border pl-3 text-xs text-muted-foreground">{card.source_excerpt}</p>
                    )}
                    {/* Source deep link — new tab so the review session
                        stays mounted underneath. Pointer handlers stop
                        propagation or a tap on the link would flip the
                        card before the click navigates. */}
                    {(card.source_type === "diary" || card.source_type === "wiki") && card.source_ref && (
                      <a
                        href={
                          card.source_type === "diary"
                            ? `/agents/${agentId}/knowledge/diary/?date=${encodeURIComponent(card.source_ref)}`
                            : `/agents/${agentId}/wiki/?page=${encodeURIComponent(card.source_ref)}`
                        }
                        target="_blank"
                        rel="noopener noreferrer"
                        className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
                        onPointerDown={(e) => e.stopPropagation()}
                        onPointerUp={(e) => e.stopPropagation()}
                        onClick={(e) => e.stopPropagation()}
                      >
                        {card.source_type === "diary" ? (
                          <CalendarIcon className="size-3" />
                        ) : (
                          <FileTextIcon className="size-3" />
                        )}
                        {card.source_type === "diary"
                          ? t("cards.fromDiary", { date: card.source_ref })
                          : wikiTitle?.id === card.id
                            ? wikiTitle.title
                            : t("cards.viewWiki")}
                        <ExternalLinkIcon className="size-3" />
                      </a>
                    )}
                  </div>
                ) : (
                  <p className="break-words pt-2 text-xl font-medium leading-relaxed">{card.question}</p>
                )}
                {!flipped && (
                  <p className="pt-6 text-xs text-muted-foreground">{t("cards.flipHint")}</p>
                )}
              </div>
            )}
          </div>
        </div>

        {/* grade buttons — mirror the swipe directions (↓ = 模糊) so the
            deck is fully operable without dragging */}
        <div className="px-4 pb-6 pt-2">
          <div className="mx-auto flex max-w-md gap-2">
            <Button
              variant="outline"
              className="h-11 flex-1 flex-col gap-0 border-destructive/40 py-1 text-destructive hover:bg-destructive/10"
              disabled={saving}
              onClick={() => grade("forgot", "button")}
            >
              <span className="text-sm">{t("cards.grade.forgot")}</span>
              <span className="text-[10px] opacity-60">←</span>
            </Button>
            <Button
              variant="outline"
              className="h-11 flex-1 flex-col gap-0 border-warning/50 py-1 text-warning hover:bg-warning/10"
              disabled={saving}
              onClick={() => grade("fuzzy", "button")}
            >
              <span className="text-sm">{t("cards.grade.fuzzy")}</span>
              <span className="text-[10px] opacity-60">↓</span>
            </Button>
            <Button
              variant="outline"
              className="h-11 flex-1 flex-col gap-0 border-emerald-500/40 py-1 text-emerald-600 hover:bg-emerald-500/10 dark:text-emerald-400"
              disabled={saving}
              onClick={() => grade("remembered", "button")}
            >
              <span className="text-sm">{t("cards.grade.remembered")}</span>
              <span className="text-[10px] opacity-60">→</span>
            </Button>
          </div>
        </div>
      </>
    );
  })();

  return (
    <div
      className={cn(
        "flex flex-col",
        variant === "overlay"
          ? "fixed inset-0 z-50 bg-background/95"
          // w-full: the inline deck sits in a flex-row pane — without it
          // the column shrinks to content width and hugs the left edge.
          : "h-full min-h-0 w-full",
      )}
    >
      {/* progress dots + exit (overlay) / label (inline) */}
      <div className="flex items-center gap-3 px-4 py-3">
        {queue !== null && !finished && (
          <>
            <span className="text-xs tabular-nums text-muted-foreground">
              {t("cards.reviewProgress", { cur: idx + 1, total: queue.length })}
            </span>
            <div className="flex flex-1 items-center gap-1 overflow-hidden">
              {queue.slice(0, 30).map((c, i) => (
                <span
                  key={c.id}
                  className={cn(
                    "h-1.5 w-1.5 shrink-0 rounded-full",
                    i < idx ? "bg-primary/70" : i === idx ? "bg-primary" : "bg-muted",
                  )}
                />
              ))}
            </div>
          </>
        )}
        {(finished || queue === null) && <div className="flex-1" />}
        {variant === "overlay" && (
          <Button
            variant="ghost"
            size="sm"
            className="h-7 w-7 p-0"
            aria-label={t("cards.reviewExit")}
            onClick={() => onFinish(resultRef.current)}
          >
            <XIcon className="size-4" />
          </Button>
        )}
      </div>
      {body}
    </div>
  );
}

// CardsReview is retained as the overlay entry: 开始复习 / ?review=1 wrap
// the shared deck in a full-screen mount.
export function CardsReview({
  agentId,
  practice,
  onDone,
}: {
  agentId: string;
  practice?: boolean;
  onDone: (result: CardDeckResult) => void;
}) {
  return <CardDeck agentId={agentId} variant="overlay" practice={practice} onFinish={onDone} />;
}
