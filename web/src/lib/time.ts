// Shared local-time helpers. "Days" here are LOCAL calendar days — the
// backend groups its own day windows by UTC+8 (diary.CST); these are the
// client-side counterparts for due-today buckets and calendar keys.

// endOfToday returns the end of the local calendar day containing now
// (23:59:59.999) as a timestamp — the "due today" bucket edge.
export function endOfToday(now: number): number {
  const d = new Date(now);
  d.setHours(23, 59, 59, 999);
  return d.getTime();
}

// pad2 zero-pads to two digits — date/time formatting's constant companion.
export const pad2 = (n: number): string => String(n).padStart(2, "0");

// dayKey formats a local date as "YYYY-MM-DD" — the join key between
// timestamps and calendar cells (calendar days, not instants).
export function dayKey(d: Date): string {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
}

// daysSince returns whole days elapsed from a YYYY-MM-DD calendar date to
// nowMs, floored at 0. `nowMs` is a parameter (not Date.now() at call
// sites) so renderers can thread their load-time stamp through and stay
// purity-lint clean.
export function daysSince(dateStr: string, nowMs: number): number {
  return Math.max(0, Math.round((nowMs - new Date(dateStr + "T00:00:00").getTime()) / 86400000));
}

// formatListTime renders a list-row timestamp in five tiers, aligned with
// the app's RowMetaTime: today → HH:mm · yesterday → 昨天/Yesterday ·
// within the week (past or future) → weekday (周三/Wed) · same year →
// M/D · older → YYYY/M/D. Future dates matter because card due times and
// cron next-runs point forward, so ±1 day gets 明天/Tomorrow too.
// withTime appends HH:mm to the non-today tiers (dashboard due labels).
export function formatListTime(
  iso: string | number | Date | null | undefined,
  locale: "en" | "zh-CN" = "zh-CN",
  opts?: { withTime?: boolean },
): string {
  if (iso == null || iso === "") return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const hm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  const now = new Date();
  const dayStart = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const dayDiff = Math.round((dayStart(now) - dayStart(d)) / 86400000);
  if (dayDiff === 0) return hm;
  const tail = opts?.withTime ? ` ${hm}` : "";
  if (dayDiff === 1) return (locale === "en" ? "Yesterday" : "昨天") + tail;
  if (dayDiff === -1) return (locale === "en" ? "Tomorrow" : "明天") + tail;
  if (Math.abs(dayDiff) < 7)
    return d.toLocaleDateString(locale === "en" ? "en-US" : "zh-CN", { weekday: "short" }) + tail;
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}/${d.getDate()}${tail}`;
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}${tail}`;
}

// formatFullTime is the tooltip/detail companion to formatListTime — the
// full locale date+time string, for spots where the short tier dropped
// information (hover tooltips, detail headers).
export function formatFullTime(iso: string | number | Date | null | undefined): string {
  if (iso == null || iso === "") return "";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleString();
}
