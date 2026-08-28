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
