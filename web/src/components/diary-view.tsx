"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
  listDiary,
  getDiary,
  generateDiary,
  type DailyDiary,
  type DiaryTheme,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import {
  ArrowLeftIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  SparklesIcon,
  AlertTriangleIcon,
  FileTextIcon,
} from "lucide-react";
import { cn } from "@/lib/utils";

const PAGE_SIZE = 10;

function todayCST(): string {
  return new Date(Date.now() + 8 * 3600 * 1000).toISOString().slice(0, 10);
}
function shiftMonth(ym: string, delta: number): string {
  const [y, m] = ym.split("-").map(Number);
  const d = new Date(Date.UTC(y, m - 1 + delta, 1));
  return d.toISOString().slice(0, 7);
}
function monthEnd(ym: string): string {
  const [y, m] = ym.split("-").map(Number);
  return new Date(Date.UTC(y, m, 0)).toISOString().slice(0, 10); // last day
}
function monthLabel(ym: string): string {
  const [y, m] = ym.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, 1)).toLocaleDateString(undefined, {
    year: "numeric",
    month: "long",
  });
}
// diaryWeight is the "content amount" a day carries — drives the heatmap
// shade. Themes + blindspots is a decent proxy for how much happened.
function diaryWeight(d: DailyDiary): number {
  return (d.themes?.length || 0) + (d.blindspots?.length || 0);
}
function heatLevel(w: number): string {
  if (w <= 0) return "bg-muted/50";
  if (w <= 2) return "bg-primary/25";
  if (w <= 4) return "bg-primary/45";
  if (w <= 7) return "bg-primary/65";
  return "bg-primary/85";
}

type CalDay = { date: string; day: number; has: boolean; weight: number; empty: boolean };
function buildMonthDays(month: string, diaries: DailyDiary[]): CalDay[] {
  const [y, m] = month.split("-").map(Number);
  const firstDow = new Date(Date.UTC(y, m - 1, 1)).getUTCDay();
  const daysInMonth = new Date(Date.UTC(y, m, 0)).getUTCDate();
  // byDate holds the row itself so we can tell "tried but empty" (themes
  // + blindspots both 0 → red框) from "has real content" (heatmap shade)
  // from "never generated" (absent → grey strike-through). Empty rows are
  // filtered out of the list — they live only as calendar markers.
  const byDate = new Map(diaries.map((d) => [d.date, d]));
  const out: CalDay[] = [];
  for (let i = 0; i < firstDow; i++) out.push({ date: "", day: 0, has: false, weight: 0, empty: false });
  for (let d = 1; d <= daysInMonth; d++) {
    const date = `${month}-${String(d).padStart(2, "0")}`;
    const dia = byDate.get(date);
    if (dia) {
      const empty = dia.themes.length === 0 && dia.blindspots.length === 0;
      out.push({ date, day: d, has: true, weight: diaryWeight(dia), empty });
    } else {
      out.push({ date, day: d, has: false, weight: 0, empty: false });
    }
  }
  return out;
}

const WEEKDAYS = ["日", "一", "二", "三", "四", "五", "六"];

// DiaryView — two-pane like the Articles view: left = month heatmap +
// paginated diary list (newest first), right = the selected day's diary.
// Heatmap shade signals how much each day carries (themes + blindspots).
export function DiaryView({ notify }: { notify: (msg: string) => void }) {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [month, setMonth] = useState(() => todayCST().slice(0, 7));
  const [diaries, setDiaries] = useState<DailyDiary[]>([]);
  const [selectedDate, setSelectedDate] = useState<string | null>(null);
  const [detail, setDetail] = useState<DailyDiary | null>(null);
  const [loadingMonth, setLoadingMonth] = useState(false);
  const [generating, setGenerating] = useState(false);
  const [page, setPage] = useState(0);
  const [genDate, setGenDate] = useState(() => todayCST());
  const [leftWidth, setLeftWidth] = useState(340);

  const loadMonth = useCallback(
    async (m: string) => {
      if (!agentId) return;
      setLoadingMonth(true);
      try {
        setDiaries(await listDiary(agentId, `${m}-01`, monthEnd(m)));
      } catch {
        setDiaries([]);
      }
      setLoadingMonth(false);
    },
    [agentId],
  );

  useEffect(() => {
    loadMonth(month);
  }, [month, loadMonth]);

  const selectDate = useCallback(
    async (d: string) => {
      setSelectedDate(d);
      setDetail(null);
      if (!agentId) return;
      try {
        setDetail(await getDiary(agentId, d));
      } catch {
        setDetail(null);
      }
    },
    [agentId],
  );

  // Deep link: /knowledge/diary/?date=YYYY-MM-DD selects that day (and
  // its month) once on mount — the cards source link lands here. One-shot
  // via ref so later in-app navigation to the diary page doesn't
  // re-select; window.location.search (not useSearchParams) keeps the
  // static-export page out of a suspense boundary.
  const deeplinkedRef = useRef(false);
  useEffect(() => {
    if (!agentId || deeplinkedRef.current) return;
    const d = new URLSearchParams(window.location.search).get("date");
    if (d && /^\d{4}-\d{2}-\d{2}$/.test(d)) {
      deeplinkedRef.current = true;
      setMonth(d.slice(0, 7));
      selectDate(d);
    }
  }, [agentId, selectDate]);

  const listSorted = useMemo(
    () =>
      [...diaries]
        .filter((d) => d.themes.length > 0 || d.blindspots.length > 0)
        .sort((a, b) => b.date.localeCompare(a.date)),
    [diaries],
  );
  // genDateIsEmpty blocks the generate button when the picked day is
  // already marked empty (no summaries → regenerating yields nothing).
  const genDateIsEmpty = useMemo(() => {
    const d = diaries.find((x) => x.date === genDate);
    return !!d && d.themes.length === 0 && d.blindspots.length === 0;
  }, [diaries, genDate]);
  const totalPages = Math.ceil(listSorted.length / PAGE_SIZE);
  const pageItems = listSorted.slice(page * PAGE_SIZE, page * PAGE_SIZE + PAGE_SIZE);

  const calendarDays = useMemo(() => buildMonthDays(month, diaries), [month, diaries]);

  const handleGenerate = useCallback(
    async (date: string) => {
      if (!agentId) return;
      setGenerating(true);
      try {
        const before = await getDiary(agentId, date);
        const beforeAt = before?.generatedAt ?? "";
        await generateDiary(agentId, date);
        // Generation runs async server-side (LLM); poll until the row
        // appears (new day) or its generatedAt advances (regenerate),
        // then refresh so the new content shows without a manual reload.
        const deadline = Date.now() + 120000;
        while (Date.now() < deadline) {
          await new Promise((r) => setTimeout(r, 3000));
          const d = await getDiary(agentId, date);
          if (d && !d.generating && (d.generatedAt || "") !== beforeAt) break;
        }
        loadMonth(month);
        if (selectedDate === date) selectDate(date);
      } catch {
        /* silent — button resets, user can retry */
      }
      setGenerating(false);
    },
    [agentId, month, selectedDate, selectDate, loadMonth],
  );

  // Drag the vertical divider to resize month+list vs. detail.
  const startDrag = useCallback(
    (e: React.PointerEvent) => {
      e.preventDefault();
      const startX = e.clientX;
      const startW = leftWidth;
      const move = (ev: PointerEvent) => {
        const w = Math.max(260, Math.min(560, startW + ev.clientX - startX));
        setLeftWidth(w);
      };
      const up = () => {
        window.removeEventListener("pointermove", move);
        window.removeEventListener("pointerup", up);
      };
      window.addEventListener("pointermove", move);
      window.addEventListener("pointerup", up);
    },
    [leftWidth],
  );

  return (
    <div className="flex min-h-0 flex-1">
      {/* Left pane: heatmap + list */}
      <div
        style={{ "--pane-lw": `${leftWidth}px` } as any}
        className={cn(
          "border-r bg-muted/30 flex-col w-full md:w-[var(--pane-lw)] md:shrink-0",
          selectedDate ? "hidden md:flex" : "flex",
        )}
      >
        {/* Month heatmap */}
        <div className="space-y-2 border-b p-3">
          <div className="flex items-center justify-between">
            <button
              type="button"
              className="rounded-md p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
              onClick={() => {
                setMonth((m) => shiftMonth(m, -1));
                setPage(0);
                setSelectedDate(null);
              }}
              aria-label={t("diary.prevMonth")}
            >
              <ChevronLeftIcon className="h-4 w-4" />
            </button>
            <span className="text-sm font-medium">{monthLabel(month)}</span>
            <button
              type="button"
              className="rounded-md p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
              onClick={() => {
                setMonth((m) => shiftMonth(m, 1));
                setPage(0);
                setSelectedDate(null);
              }}
              aria-label={t("diary.nextMonth")}
            >
              <ChevronRightIcon className="h-4 w-4" />
            </button>
          </div>
          <div className="grid grid-cols-7 gap-1 text-center text-[10px] text-muted-foreground">
            {WEEKDAYS.map((w) => (
              <div key={w}>{w}</div>
            ))}
          </div>
          <div className="grid grid-cols-7 gap-1">
            {calendarDays.map((d, i) =>
              d.day === 0 ? (
                <div key={`b-${i}`} />
              ) : (
                <button
                  key={d.date}
                  type="button"
                  disabled={d.date > todayCST()}
                  title={d.empty ? `${d.date} · 无对话内容` : d.has ? `${d.date} · ${d.weight}` : d.date}
                  onClick={() => {
                    if (d.date > todayCST()) return;
                    setGenDate(d.date);
                    selectDate(d.date);
                  }}
                  className={cn(
                    "aspect-square rounded text-[10px] tabular-nums flex items-center justify-center transition-all",
                    d.date > todayCST()
                      ? "opacity-30 text-muted-foreground cursor-default"
                      : d.empty
                        ? "border border-destructive text-destructive cursor-pointer hover:scale-110"
                        : d.has
                          ? cn(heatLevel(d.weight), "text-foreground cursor-pointer hover:scale-110")
                          : "opacity-40 text-muted-foreground cursor-pointer hover:scale-110",
                    d.date === selectedDate && "ring-2 ring-primary ring-offset-1 ring-offset-background",
                  )}
                >
                  {d.day}
                </button>
              ),
            )}
          </div>
          <p className="text-[10px] text-muted-foreground">{t("diary.heatHint")}</p>
          <div className="flex items-center gap-1.5 pt-1">
            <input
              type="date"
              value={genDate}
              max={todayCST()}
              onClick={(e) => (e.currentTarget as HTMLInputElement).showPicker?.()}
              onChange={(e) => setGenDate(e.target.value)}
              className="h-8 flex-1 rounded border border-border bg-background px-2 text-xs"
            />
            <Button
              size="sm"
              variant="outline"
              className="h-8 shrink-0 px-2"
              onClick={() => handleGenerate(genDate)}
              disabled={generating || genDateIsEmpty}
            >
              <SparklesIcon className="h-3.5 w-3.5" />
              <span className="ml-1 text-xs">{generating ? t("diary.generating") : t("diary.generate")}</span>
            </Button>
          </div>
        </div>

        {/* List (newest first, paginated) */}
        <ScrollArea className="flex-1">
          <div className="space-y-1 p-2">
            {loadingMonth ? (
              <p className="px-2 py-1.5 text-xs text-muted-foreground">{t("diary.loading")}</p>
            ) : listSorted.length === 0 ? (
              <p className="px-2 py-1.5 text-xs text-muted-foreground">{t("diary.noDiaryMonth")}</p>
            ) : (
              pageItems.map((d) => (
                <button
                  key={d.date}
                  type="button"
                  onClick={() => selectDate(d.date)}
                  className={cn(
                    "w-full rounded-md px-3 py-2 text-left transition-colors hover:bg-accent",
                    d.date === selectedDate && "bg-accent",
                  )}
                >
                  <p className="truncate text-sm font-medium">{d.overview || d.date}</p>
                  <p className="tabular-nums text-xs text-muted-foreground">
                    {d.date} · {d.themes.length}
                    {t("diary.topicUnit")} · {d.blindspots.length}
                    {t("diary.blindspotUnit")}
                  </p>
                </button>
              ))
            )}
          </div>
          {totalPages > 1 && (
            <div className="flex items-center justify-center gap-3 border-t p-2">
              <button
                type="button"
                disabled={page === 0}
                onClick={() => setPage((p) => p - 1)}
                className="rounded-md p-1 text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-30"
              >
                <ChevronLeftIcon className="h-4 w-4" />
              </button>
              <span className="tabular-nums text-xs text-muted-foreground">
                {page + 1} / {totalPages}
              </span>
              <button
                type="button"
                disabled={page >= totalPages - 1}
                onClick={() => setPage((p) => p + 1)}
                className="rounded-md p-1 text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-30"
              >
                <ChevronRightIcon className="h-4 w-4" />
              </button>
            </div>
          )}
        </ScrollArea>
      </div>

      {/* Drag divider */}
      <div
        onPointerDown={startDrag}
        className="hidden md:block w-1 shrink-0 cursor-col-resize transition-colors hover:bg-primary/40"
      />

      {/* Right pane: detail */}
      <div className={cn("flex-1 flex-col min-w-0", selectedDate ? "flex" : "hidden md:flex")}>
        {selectedDate ? (
          <DetailPane
            date={selectedDate}
            detail={detail}
            onBack={() => setSelectedDate(null)}
            onGenerate={handleGenerate}
            generating={generating}
            agentId={agentId || ""}
          />
        ) : (
          <div className="flex h-full items-center justify-center p-6 text-center text-sm text-muted-foreground">
            {t("diary.selectHint")}
          </div>
        )}
      </div>
    </div>
  );
}

function DetailPane({
  date,
  detail,
  onBack,
  onGenerate,
  generating,
  agentId,
}: {
  date: string;
  detail: DailyDiary | null;
  onBack: () => void;
  onGenerate: (date: string) => void;
  generating: boolean;
  agentId: string;
}) {
  const t = useT();
  const dateLabel = new Date(date + "T00:00:00").toLocaleDateString(undefined, {
    year: "numeric",
    month: "long",
    day: "numeric",
    weekday: "long",
  });
  return (
    <>
      <div className="flex items-center gap-2 border-b p-3">
        <button
          type="button"
          onClick={onBack}
          className="-ml-1 shrink-0 rounded-md p-1 text-muted-foreground hover:bg-accent hover:text-foreground md:hidden"
          aria-label={t("common.back")}
        >
          <ArrowLeftIcon className="h-5 w-5" />
        </button>
        <h2 className="flex-1 truncate text-base font-semibold">{dateLabel}</h2>
        <Button size="sm" variant="outline" onClick={() => onGenerate(date)} disabled={generating || (!!detail && detail.themes.length === 0 && detail.blindspots.length === 0)}>
          <SparklesIcon className="mr-1.5 h-3.5 w-3.5" />
          {generating ? t("diary.generating") : t("diary.regenerate")}
        </Button>
      </div>
      <ScrollArea className="flex-1">
        <div className="space-y-5 p-4">
          {detail?.generating ? (
            <p className="flex items-center gap-2 text-sm text-muted-foreground">
              <SparklesIcon className="h-4 w-4 animate-pulse" /> {t("diary.generating")}
            </p>
          ) : !detail ? (
            <p className="text-sm text-muted-foreground">{t("diary.noDiary")}</p>
          ) : detail.themes.length === 0 && detail.blindspots.length === 0 ? (
            <p className="text-sm text-muted-foreground">{detail.overview || t("diary.noThemes")}</p>
          ) : (
            <DiaryContent agentId={agentId} diary={detail} />
          )}
        </div>
      </ScrollArea>
    </>
  );
}

function DiaryContent({ agentId, diary }: { agentId: string; diary: DailyDiary }) {
  const t = useT();
  return (
    <>
      {diary.overview && (
        <div className="rounded-lg border bg-card p-4">
          <h3 className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {t("diary.overview")}
          </h3>
          <p className="text-sm leading-relaxed">{diary.overview}</p>
        </div>
      )}

      <div className="space-y-3">
        <h3 className="flex items-center gap-1.5 text-sm font-semibold">
          <FileTextIcon className="h-4 w-4" /> {t("diary.themes")}
        </h3>
        {diary.themes.map((th, i) => (
          <ThemeCard key={i} agentId={agentId} theme={th} />
        ))}
      </div>

      {diary.blindspots.length > 0 && (
        <div className="space-y-2">
          <h3 className="flex items-center gap-1.5 text-sm font-semibold text-primary">
            <AlertTriangleIcon className="h-4 w-4" /> {t("diary.blindspots")}
          </h3>
          <div className="space-y-2">
            {diary.blindspots.map((b, i) => (
              <div key={i} className="rounded-lg border border-primary/30 bg-primary/5 p-3">
                <p className="text-sm font-medium">{b.point}</p>
                {b.reason && <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{b.reason}</p>}
              </div>
            ))}
          </div>
        </div>
      )}

      {diary.archives && diary.archives.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-sm font-semibold">{t("diary.archives")}</h3>
          <ul className="space-y-1">
            {diary.archives.map((a, i) => (
              <li key={i} className="text-sm text-muted-foreground">
                {a}
              </li>
            ))}
          </ul>
        </div>
      )}
    </>
  );
}

function ThemeCard({ agentId, theme }: { agentId: string; theme: DiaryTheme }) {
  return (
    <div className="rounded-lg border bg-card p-4">
      <h4 className="text-sm font-semibold">{theme.title}</h4>
      {theme.summary && <p className="mt-1 text-sm leading-relaxed text-muted-foreground">{theme.summary}</p>}
      {theme.points && theme.points.length > 0 && (
        <ul className="mt-2 space-y-1">
          {theme.points.map((p, i) => (
            <li key={i} className="text-sm leading-relaxed">
              · {p}
            </li>
          ))}
        </ul>
      )}
      {theme.segments && theme.segments.length > 0 && (
        <div className="mt-3 flex flex-wrap gap-1">
          {theme.segments.slice(0, 6).map((seg, i) => (
            <a
              key={i}
              href={`/agents/${agentId}/chat/${encodeURIComponent(seg.session)}#seq-${seg.start}`}
              className="rounded border border-primary/40 bg-primary/10 px-1.5 py-0.5 font-mono text-[11px] font-semibold text-primary hover:bg-primary/20"
            >
              #{seg.start === seg.end ? `seq-${seg.start}` : `seq-${seg.start}~${seg.end}`}
            </a>
          ))}
        </div>
      )}
    </div>
  );
}
