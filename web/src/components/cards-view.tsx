"use client";

import { useT } from "@/lib/i18n";

import { useCallback, useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Textarea } from "@/components/ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
import {
  ArchiveRestoreIcon,
  ArrowLeftIcon,
  CalendarIcon,
  FlameIcon,
  LayersIcon,
  PencilIcon,
  PlayIcon,
  PlusIcon,
  SearchIcon,
  SendIcon,
  SparklesIcon,
  TrashIcon,
} from "lucide-react";
import {
  type KBCard,
  type KBCardReview,
  type KBCardStats,
  listCards,
  getCard,
  saveCard,
  updateCard,
  deleteCard,
  archiveCard,
  restoreCard,
  generateCards,
  getCardStats,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { cn } from "@/lib/utils";
import { readCache, writeCache } from "@/lib/page-data-cache";
import { ConfirmDeleteDialog } from "@/components/confirm-delete-dialog";
import { CardsReview } from "@/components/cards-review";

// CardsView — the Q&A flashcard library (卡片库): left list (filter chips +
// source select + search + paged loading), right detail (flip preview,
// source link, review timeline, edit/delete/archive). The review flow
// itself (due queue + grades) is the M5 overlay; this view manages the
// corpus. Mirrors the ArticleView master-detail shell.

type CardFilter = "all" | "active" | "new" | "mastered" | "archived";
const FILTERS: CardFilter[] = ["all", "active", "new", "mastered", "archived"];

const PAGE = 50;

// dueState labels a card against now: overdue (red), due today (amber),
// scheduled (muted), unscheduled/mastered (nothing).
function isDueSoon(c: KBCard, now: number): "overdue" | "today" | null {
  if (!c.due_at || c.status !== "active") return null;
  const due = new Date(c.due_at).getTime();
  if (isNaN(due)) return null;
  if (due <= now - 86400_000) return "overdue"; // before today
  if (due <= endOfToday(now)) return "today";
  return null;
}
function endOfToday(now: number): number {
  const d = new Date(now);
  d.setHours(23, 59, 59, 999);
  return d.getTime();
}

export function CardsView({ notify }: { notify: (msg: string) => void }) {
  const t = useT();
  const router = useRouter();
  const agentId = useAgentIdFromURL();

  const [cards, setCards] = useState<KBCard[]>([]);
  const [total, setTotal] = useState(0); // loaded-so-far marker: == PAGE means maybe more
  const [loading, setLoading] = useState(false);
  const [filter, setFilter] = useState<CardFilter>("all");
  const [source, setSource] = useState("");
  const [query, setQuery] = useState("");
  const queryRef = useRef(query);
  queryRef.current = query;

  const [selected, setSelected] = useState<KBCard | null>(null);
  const [reviews, setReviews] = useState<KBCardReview[]>([]);
  const [flipped, setFlipped] = useState(false);
  const [detailLoading, setDetailLoading] = useState(false);

  const [addOpen, setAddOpen] = useState(false);
  const [addQ, setAddQ] = useState("");
  const [addA, setAddA] = useState("");
  const [addSaving, setAddSaving] = useState(false);
  const [editOpen, setEditOpen] = useState(false);
  const [editQ, setEditQ] = useState("");
  const [editA, setEditA] = useState("");
  const [editSaving, setEditSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<KBCard | null>(null);
  const [generating, setGenerating] = useState(false);

  // Header dashboard + review flow (M5).
  const [stats, setStats] = useState<KBCardStats | null>(null);
  const [reviewQueue, setReviewQueue] = useState<KBCard[] | null>(null);
  const reviewStartedRef = useRef(false);

  const load = useCallback(
    async (offset: number, replace: boolean) => {
      if (!agentId) return;
      if (replace) setLoading(true);
      try {
        const page = await listCards(agentId, {
          filter, source: source || undefined,
          q: queryRef.current.trim() || undefined,
          limit: PAGE, offset,
        });
        setCards((prev) => (replace ? page : [...prev, ...page]));
        setTotal(offset + page.length);
      } catch {
        if (replace) setCards([]);
      }
      setLoading(false);
    },
    [agentId, filter, source],
  );

  useEffect(() => {
    const cached = agentId ? readCache<KBCard[]>(`kb-cards:${agentId}`) : undefined;
    if (cached?.length) {
      setCards(cached);
      setLoading(false);
    } else {
      setLoading(true);
    }
    load(0, true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [load]);

  useEffect(() => {
    if (cards.length) writeCache(`kb-cards:${agentId}`, cards.slice(0, 100));
  }, [agentId, cards]);

  // loadStats feeds the header dashboard; called on mount, after each
  // review session, and after any corpus mutation.
  const loadStats = useCallback(async () => {
    if (!agentId) return;
    setStats(await getCardStats(agentId));
  }, [agentId]);

  useEffect(() => { loadStats(); }, [loadStats]);

  // startReview builds the due queue (oldest-due first: overdue cards come
  // back before fresh ones) and opens the overlay. An empty result closes
  // immediately — the button is disabled in that case anyway.
  const startReview = useCallback(async () => {
    if (!agentId) return;
    const due = await listCards(agentId, { filter: "due", limit: 100 });
    if (!due.length) {
      await loadStats();
      return;
    }
    setReviewQueue(due.reverse()); // list is newest-created first; review oldest first
  }, [agentId, loadStats]);

  const endReview = useCallback(() => {
    setReviewQueue(null);
    load(0, true);
    loadStats();
  }, [load, loadStats]);

  // Deep link: /knowledge/cards?review=1 auto-starts a session once (IM
  // digest link). reviewStartedRef guards against remount retriggering.
  useEffect(() => {
    if (!agentId || reviewStartedRef.current) return;
    if (typeof window === "undefined") return;
    const params = new URLSearchParams(window.location.search);
    if (params.get("review") === "1") {
      reviewStartedRef.current = true;
      startReview();
      // Strip the param so a later manual reload of the page doesn't
      // re-trigger the session.
      params.delete("review");
      const qs = params.toString();
      router.replace(`${window.location.pathname}${qs ? `?${qs}` : ""}`);
    }
  }, [agentId, startReview, router]);

  // Selecting a card fetches its review timeline; the flip resets.
  const openCard = useCallback(
    async (c: KBCard) => {
      setSelected(c);
      setFlipped(false);
      setReviews([]);
      setDetailLoading(true);
      try {
        const d = agentId ? await getCard(agentId, c.id) : null;
        if (d) {
          setSelected(d.card);
          setReviews(d.reviews);
        }
      } catch { /* detail stays on the list row */ }
      setDetailLoading(false);
    },
    [agentId],
  );

  const refresh = useCallback(() => {
    setSelected((s) => (s ? cards.find((c) => c.id === s.id) ?? s : null));
    load(0, true);
  }, [cards, load]);

  const handleAdd = useCallback(async () => {
    if (!agentId || !addQ.trim()) return;
    setAddSaving(true);
    const res = await saveCard(agentId, addQ.trim(), addA.trim());
    setAddSaving(false);
    if (res.error) { notify(res.error); return; }
    setAddOpen(false); setAddQ(""); setAddA("");
    load(0, true);
    loadStats();
  }, [agentId, addQ, addA, notify, load, loadStats]);

  const handleEditSave = useCallback(async () => {
    if (!agentId || !selected) return;
    setEditSaving(true);
    const res = await updateCard(agentId, selected.id, {
      question: editQ.trim() || undefined,
      answer: editA.trim() || undefined,
    });
    setEditSaving(false);
    if (res.error) { notify(res.error); return; }
    setEditOpen(false);
    refresh();
    const d = await getCard(agentId, selected.id);
    if (d) { setSelected(d.card); setReviews(d.reviews); }
  }, [agentId, selected, editQ, editA, notify, refresh]);

  const handleDelete = useCallback(async () => {
    if (!agentId || !deleteTarget) return;
    const res = await deleteCard(agentId, deleteTarget.id);
    if (res.error) { notify(res.error); return; }
    if (selected?.id === deleteTarget.id) setSelected(null);
    setDeleteTarget(null);
    load(0, true);
    loadStats();
  }, [agentId, deleteTarget, selected, notify, load, loadStats]);

  const handleArchive = useCallback(async (c: KBCard, archive: boolean) => {
    if (!agentId) return;
    const res = archive ? await archiveCard(agentId, c.id) : await restoreCard(agentId, c.id);
    if (res.error) { notify(res.error); return; }
    if (selected?.id === c.id) {
      setSelected({ ...c, status: archive ? "archived" : "active" });
    }
    load(0, true);
    loadStats();
  }, [agentId, selected, notify, load, loadStats]);

  const handleGenerate = useCallback(async () => {
    if (!agentId) return;
    setGenerating(true);
    const res = await generateCards(agentId);
    setGenerating(false);
    if (res.error) { notify(res.error); return; }
    notify(t("cards.genDone", { n: res.created ?? 0 }));
    load(0, true);
  }, [agentId, notify, t, load]);

  const now = Date.now();
  // sourceRoute deep-links the card's origin: diary → that day (the diary
  // view consumes ?date= on mount), wiki → that page (the wiki view
  // consumes ?page= on mount — page ids are "type:slug" pairs).
  const sourceRoute = useCallback(
    (c: KBCard) => {
      if (!agentId) return null;
      if (c.source_type === "diary" && c.source_ref) {
        return `/agents/${agentId}/knowledge/diary/?date=${encodeURIComponent(c.source_ref)}`;
      }
      if (c.source_type === "wiki" && c.source_ref) {
        return `/agents/${agentId}/wiki/?page=${encodeURIComponent(c.source_ref)}`;
      }
      return null;
    },
    [agentId],
  );

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* Header dashboard: due today (CTA), streak, learning/mastered. */}
      <div className="flex items-center gap-3 border-b px-3 py-2">
        {stats && stats.due_today > 0 ? (
          <Button size="sm" className="h-8 text-xs" onClick={startReview}>
            <PlayIcon className="mr-1 size-3.5" />
            {t("cards.reviewCta")} · {t("cards.reviewN", { n: stats.due_today })}
          </Button>
        ) : (
          <span className="text-xs text-emerald-600 dark:text-emerald-400">{t("cards.dueNone")}</span>
        )}
        {stats && stats.streak_days > 0 && (
          <Badge variant="outline" className="gap-1 border-warning/40 px-1.5 py-0 text-[11px] text-warning">
            <FlameIcon className="size-3" />
            {t("cards.streakDays", { n: stats.streak_days })}
          </Badge>
        )}
        {stats && (
          <span className="ml-auto text-[11px] tabular-nums text-muted-foreground">
            {t("cards.statActive", { n: stats.active })} · {t("cards.statMastered", { n: stats.mastered })}
          </span>
        )}
      </div>

    <div className="flex min-h-0 flex-1">
      {/* ── Left: card list ── */}
      <div
        className={cn(
          "flex w-full flex-col border-r bg-muted/30 md:w-[340px] md:shrink-0",
          selected ? "hidden md:flex" : "flex",
        )}
      >
        <div className="border-b p-3 pb-2">
          <div className="flex items-center gap-2">
            <div className="relative min-w-0 flex-1">
              <SearchIcon className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={query}
                onChange={(e) => { setQuery(e.target.value); }}
                onBlur={() => load(0, true)}
                onKeyDown={(e) => { if (e.key === "Enter") load(0, true); }}
                placeholder={t("cards.search")}
                className="h-7 w-full rounded-md pl-8 text-xs"
              />
            </div>
            <Button variant="outline" size="sm" className="h-7 shrink-0 text-xs" onClick={() => setAddOpen(true)}>
              <PlusIcon className="mr-1 size-3" /> {t("cards.add")}
            </Button>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-1">
            {FILTERS.map((f) => (
              <button
                key={f}
                type="button"
                onClick={() => setFilter(f)}
                className={cn(
                  "rounded-full px-2 py-0.5 text-[11px] transition-colors",
                  filter === f ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:bg-accent",
                )}
              >
                {t(`cards.filter.${f}`)}
              </button>
            ))}
            <select
              value={source}
              onChange={(e) => setSource(e.target.value)}
              className="ml-auto rounded-md border bg-background px-1.5 py-0.5 text-[11px] text-muted-foreground"
              aria-label={t("cards.sourceFilter")}
            >
              <option value="">{t("cards.source.all")}</option>
              <option value="diary">{t("cards.source.diary")}</option>
              <option value="wiki">{t("cards.source.wiki")}</option>
              <option value="manual">{t("cards.source.manual")}</option>
            </select>
          </div>
          <button
            type="button"
            onClick={handleGenerate}
            disabled={generating}
            className="mt-2 flex w-full items-center justify-center gap-1 rounded-md border border-dashed px-2 py-1 text-[11px] text-muted-foreground hover:bg-accent disabled:opacity-50"
          >
            <SparklesIcon className="size-3" />
            {generating ? t("common.saving") : t("cards.genNow")}
          </button>
        </div>
        <ScrollArea className="flex-1">
          <div className="p-2">
            {loading ? (
              <p className="px-2 py-1.5 text-xs text-muted-foreground">{t("common.loading")}</p>
            ) : cards.length === 0 ? (
              <p className="px-2 py-1.5 text-xs text-muted-foreground">{t("cards.empty")}</p>
            ) : (
              cards.map((c) => {
                const due = isDueSoon(c, now);
                return (
                  <div
                    key={c.id}
                    role="button"
                    tabIndex={0}
                    className={cn(
                      "group mb-0.5 w-full cursor-pointer rounded-md px-3 py-2 text-left hover:bg-accent",
                      selected?.id === c.id && "bg-accent",
                    )}
                    onClick={() => openCard(c)}
                    onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openCard(c); } }}
                  >
                    <p className={cn("truncate text-sm", c.status === "archived" && "text-muted-foreground line-through")}>
                      {c.question}
                    </p>
                    <p className="mt-0.5 flex items-center gap-1.5 text-[11px] text-muted-foreground">
                      <LayersIcon className="size-3 shrink-0" />
                      {t(`cards.source.${c.source_type}`)}
                      {due === "overdue" && (
                        <Badge variant="outline" className="border-destructive/40 px-1 py-0 text-[10px] text-destructive">
                          {t("cards.overdue")}
                        </Badge>
                      )}
                      {due === "today" && (
                        <Badge variant="outline" className="border-warning/50 px-1 py-0 text-[10px] text-warning">
                          {t("cards.dueToday")}
                        </Badge>
                      )}
                      {c.status === "mastered" && (
                        <Badge variant="outline" className="border-emerald-500/40 px-1 py-0 text-[10px] text-emerald-600 dark:text-emerald-400">
                          {t("cards.mastered")}
                        </Badge>
                      )}
                      <button
                        type="button"
                        className="ml-auto shrink-0 rounded p-0.5 opacity-0 hover:text-destructive focus-visible:opacity-100 group-hover:opacity-100"
                        onClick={(e) => { e.stopPropagation(); setDeleteTarget(c); }}
                        aria-label={t("common.delete")}
                      >
                        <TrashIcon className="size-3" />
                      </button>
                    </p>
                  </div>
                );
              })
            )}
            {/* Paged loading: fetch another PAGE when the last page was full. */}
            {cards.length > 0 && total % PAGE === 0 && (
              <Button variant="ghost" size="sm" className="mt-1 w-full text-xs" disabled={loading} onClick={() => load(cards.length, false)}>
                {t("cards.loadMore")}
              </Button>
            )}
          </div>
        </ScrollArea>
      </div>

      {/* ── Right: detail ── */}
      {selected ? (
        <div className="flex min-w-0 flex-1 flex-col">
          <div className="flex items-center gap-2 border-b p-2 pl-3">
            <Button variant="ghost" size="sm" className="h-7 px-2 md:hidden" onClick={() => setSelected(null)}>
              <ArrowLeftIcon className="size-4" />
            </Button>
            <Badge variant="outline" className="px-1.5 py-0 text-[11px]">
              {t(`cards.source.${selected.source_type}`)}
              {selected.source_ref ? ` · ${selected.source_ref}` : ""}
            </Badge>
            <span className="text-[11px] text-muted-foreground">
              {t("cards.intervalProgress", { cur: selected.interval_index, total: 6 })}
              {selected.review_count > 0 && ` · ${t("cards.reviewedN", { n: selected.review_count })}`}
              {selected.lapse_count > 0 && ` · ${t("cards.lapsedN", { n: selected.lapse_count })}`}
            </span>
            <div className="ml-auto flex shrink-0 items-center gap-1">
              <Button variant="ghost" size="sm" className="h-7 w-7 p-0" aria-label={t("common.edit")}
                onClick={() => { setEditQ(selected.question); setEditA(selected.answer); setEditOpen(true); }}>
                <PencilIcon className="size-3.5" />
              </Button>
              {selected.status === "archived" ? (
                <Button variant="ghost" size="sm" className="h-7 w-7 p-0" aria-label={t("cards.restore")}
                  onClick={() => handleArchive(selected, false)}>
                  <ArchiveRestoreIcon className="size-3.5" />
                </Button>
              ) : (
                <Button variant="ghost" size="sm" className="h-7 w-7 p-0" aria-label={t("cards.archive")}
                  onClick={() => handleArchive(selected, true)}>
                  <SendIcon className="size-3.5" />
                </Button>
              )}
              <Button variant="ghost" size="sm" className="h-7 w-7 p-0 hover:text-destructive" aria-label={t("common.delete")}
                onClick={() => setDeleteTarget(selected)}>
                <TrashIcon className="size-3.5" />
              </Button>
            </div>
          </div>

          <ScrollArea className="flex-1">
            <div className="mx-auto max-w-2xl space-y-4 p-4">
              {/* Flip preview: click card to reveal the answer. */}
              <button
                type="button"
                onClick={() => setFlipped((f) => !f)}
                className={cn(
                  "w-full rounded-xl border bg-background p-6 text-left shadow-sm transition-colors",
                  flipped ? "border-primary/40" : "hover:border-primary/30",
                )}
              >
                <p className="text-[11px] uppercase tracking-wide text-muted-foreground">
                  {flipped ? t("cards.back") : t("cards.front")}
                </p>
                {flipped ? (
                  <div className="mt-2 whitespace-pre-wrap break-words text-base leading-relaxed">{selected.answer}</div>
                ) : (
                  <p className="mt-2 break-words text-lg font-medium">{selected.question}</p>
                )}
                <p className="mt-3 text-[11px] text-muted-foreground">{t("cards.flipHint")}</p>
              </button>

              {selected.source_excerpt && (
                <div className="rounded-lg border border-dashed p-3">
                  <p className="text-[11px] text-muted-foreground">{t("cards.excerpt")}</p>
                  <p className="mt-1 whitespace-pre-wrap break-words text-sm text-muted-foreground">{selected.source_excerpt}</p>
                </div>
              )}

              {selected.source_ref && sourceRoute(selected) && (
                <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                  <CalendarIcon className="size-3.5" />
                  <span>{t("cards.from")}</span>
                  <button
                    type="button"
                    className="text-primary hover:underline"
                    onClick={() => {
                      const r = sourceRoute(selected);
                      if (r) router.push(r);
                    }}
                  >
                    {selected.source_type === "diary"
                      ? t("cards.fromDiary", { date: selected.source_ref })
                      : selected.source_ref}
                  </button>
                </div>
              )}

              {/* Review timeline */}
              <div>
                <p className="mb-1.5 text-xs font-medium text-muted-foreground">
                  {t("cards.timeline")}
                  {selected.due_at && selected.status === "active" && (
                    <span className="ml-2 font-normal">
                      · {t("cards.nextDue", { date: new Date(selected.due_at).toLocaleDateString() })}
                    </span>
                  )}
                </p>
                {detailLoading ? (
                  <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
                ) : reviews.length === 0 ? (
                  <p className="text-xs text-muted-foreground">{t("cards.noReviews")}</p>
                ) : (
                  <div className="space-y-1">
                    {reviews.map((rv) => (
                      <div key={rv.id} className="flex items-center gap-2 rounded-md border bg-muted/20 px-2.5 py-1.5 text-xs">
                        <Badge
                          variant="outline"
                          className={cn(
                            "px-1.5 py-0 text-[10px]",
                            rv.grade === "remembered" && "border-emerald-500/40 text-emerald-600 dark:text-emerald-400",
                            rv.grade === "fuzzy" && "border-warning/50 text-warning",
                            rv.grade === "forgot" && "border-destructive/40 text-destructive",
                          )}
                        >
                          {t(`cards.grade.${rv.grade}`)}
                        </Badge>
                        <span className="text-muted-foreground">
                          {t("cards.intervalProgress", { cur: rv.prev_interval_index, total: 6 })}
                          {" → "}
                          {t("cards.intervalProgress", { cur: rv.new_interval_index, total: 6 })}
                        </span>
                        <span className="ml-auto text-muted-foreground">{new Date(rv.reviewed_at).toLocaleString()}</span>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </div>
          </ScrollArea>
        </div>
      ) : (
        <div className="hidden min-w-0 flex-1 items-center justify-center md:flex">
          <p className="text-sm text-muted-foreground">{t("cards.selectHint")}</p>
        </div>
      )}

      {/* Add dialog */}
      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("cards.add")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("cards.front")}</Label>
              <Input value={addQ} onChange={(e) => setAddQ(e.target.value)} placeholder={t("cards.questionPlaceholder")} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("cards.back")}</Label>
              <Textarea value={addA} onChange={(e) => setAddA(e.target.value)} placeholder={t("cards.answerPlaceholder")} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleAdd} disabled={addSaving || !addQ.trim()}>
              {addSaving ? t("common.saving") : t("common.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Edit dialog */}
      <Dialog open={editOpen} onOpenChange={setEditOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("cards.edit")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("cards.front")}</Label>
              <Input value={editQ} onChange={(e) => setEditQ(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("cards.back")}</Label>
              <Textarea value={editA} onChange={(e) => setEditA(e.target.value)} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleEditSave} disabled={editSaving || !editQ.trim()}>
              {editSaving ? t("common.saving") : t("common.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        name={deleteTarget ? deleteTarget.question : ""}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        onConfirm={handleDelete}
      />

      {/* Review overlay (due queue, one card at a time). */}
      {reviewQueue && reviewQueue.length > 0 && (
        <CardsReview agentId={agentId!} queue={reviewQueue} onDone={endReview} />
      )}
    </div>
    </div>
  );
}
