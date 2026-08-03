"use client";

import { useEffect, useMemo, useState } from "react";
import { Coins, RefreshCcw, ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  getAgentTokenUsage,
  getChatSessions,
  type AgentTokenUsage,
  type ChatSessionEntry,
  type TokenUsageRange,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";

const RANGES: { value: TokenUsageRange; label: string }[] = [
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
  { value: "30d", label: "30d" },
];

// Client-side pagination — the usage API only takes a limit (no offset), so
// we fetch a generous window and page over it in the browser.
const PAGE_SIZE = 15;

// fmt collapses big counts into 12.3K / 4.5M for the table. Below 1000
// keep the raw count so a quick test session reads "47" not "0.0K".
function fmt(n: number): string {
  if (!Number.isFinite(n)) return "—";
  if (Math.abs(n) < 1000) return n.toString();
  const abs = Math.abs(n);
  if (abs < 1_000_000) return (n / 1_000).toFixed(1) + "K";
  if (abs < 1_000_000_000) return (n / 1_000_000).toFixed(2) + "M";
  return (n / 1_000_000_000).toFixed(2) + "B";
}

export default function AgentUsagePage() {
  const agentId = useAgentIdFromURL();
  const t = useT();
  const [range, setRange] = useState<TokenUsageRange>("7d");
  const [data, setData] = useState<AgentTokenUsage | null>(null);
  const [sessions, setSessions] = useState<ChatSessionEntry[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [page, setPage] = useState(1);

  // Pull session metadata once so the table can render titles
  // instead of opaque session_keys. getChatSessions returns the
  // owner's view of this agent's chats; rows whose key doesn't
  // appear there (e.g. cron-fired sessions for a different
  // chatter on a public agent) fall back to the truncated key.
  useEffect(() => {
    if (!agentId) return;
    let aborted = false;
    (async () => {
      try {
        const list = await getChatSessions(agentId);
        if (!aborted) setSessions(list);
      } catch {
        // Non-fatal — table just shows raw keys.
      }
    })();
    return () => {
      aborted = true;
    };
  }, [agentId]);

  const sessionTitles = useMemo(() => {
    const m: Record<string, string> = {};
    for (const s of sessions) {
      m[s.id] = s.title || s.preview || s.id;
    }
    return m;
  }, [sessions]);

  async function load(r: TokenUsageRange) {
    if (!agentId) return;
    setLoading(true);
    setError("");
    try {
      const d = await getAgentTokenUsage(agentId, r, 200);
      setData(d);
    } catch (e) {
      setError(e instanceof Error ? e.message : t("usage.failedLoad"));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    load(range);
    setPage(1);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId, range]);

  function renderSessionLabel(key: string): string {
    if (!key) return t("usage.untracked");
    const title = sessionTitles[key];
    if (title) return title;
    // Keys are opaque hashes — truncate so the row stays readable.
    return key.length > 14 ? key.slice(0, 14) + "…" : key;
  }

  const rows = data?.sessions ?? [];
  const totalPages = Math.max(1, Math.ceil(rows.length / PAGE_SIZE));
  const safePage = Math.min(page, totalPages);
  const pageStart = (safePage - 1) * PAGE_SIZE;
  const pageRows = rows.slice(pageStart, pageStart + PAGE_SIZE);

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold tracking-tight">{t("usage.title")}</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {t("usage.subtitle")}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Tabs value={range} onValueChange={(v) => setRange(v as TokenUsageRange)}>
            <TabsList>
              {RANGES.map((r) => (
                <TabsTrigger key={r.value} value={r.value}>
                  {r.label}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
          <Button variant="outline" size="sm" onClick={() => load(range)} disabled={loading}>
            <RefreshCcw className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} />
          </Button>
        </div>
      </div>

      {error && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent>
            <p className="text-sm text-destructive">{error}</p>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardContent>
          {rows.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-10 text-center">
              <Coins className="h-8 w-8 text-muted-foreground mb-3" />
              <p className="text-sm text-muted-foreground">
                {t("usage.empty")}
              </p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("usage.col.session")}</TableHead>
                  <TableHead className="text-right">{t("usage.col.input")}</TableHead>
                  <TableHead className="text-right">{t("usage.col.output")}</TableHead>
                  <TableHead className="text-right">{t("usage.col.cache")}</TableHead>
                  <TableHead className="text-right">{t("usage.col.total")}</TableHead>
                  <TableHead className="text-right">{t("usage.col.requests")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {pageRows.map((r) => {
                  // Cache = total - input - output; the API rolls
                  // cache_read + cache_creation into `tokens` but
                  // doesn't break them out on the wire (yet). Showing
                  // a single "Cache" column makes the row math add
                  // up (input + output + cache = total) without
                  // pretending prompt-cache hits don't exist.
                  const cache = Math.max(0, r.tokens - r.inputTokens - r.outputTokens);
                  return (
                    <TableRow key={r.key || "untracked"}>
                      <TableCell className="font-medium max-w-[260px] truncate" title={r.key}>
                        {renderSessionLabel(r.key)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{fmt(r.inputTokens)}</TableCell>
                      <TableCell className="text-right tabular-nums">{fmt(r.outputTokens)}</TableCell>
                      <TableCell className="text-right tabular-nums text-muted-foreground">{fmt(cache)}</TableCell>
                      <TableCell className="text-right tabular-nums font-medium">{fmt(r.tokens)}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.requestCount}</TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {totalPages > 1 && (
        <div className="flex items-center justify-between text-sm">
          <span className="text-muted-foreground">
            {t("usage.range", { start: pageStart + 1, end: Math.min(pageStart + PAGE_SIZE, rows.length), total: rows.length })}
          </span>
          <div className="flex items-center gap-1">
            <Button variant="outline" size="icon" onClick={() => setPage((p) => Math.max(1, p - 1))} disabled={safePage <= 1}>
              <ChevronLeft className="size-4" />
            </Button>
            <span className="px-3 text-muted-foreground">
              {t("usage.page", { page: safePage, total: totalPages })}
            </span>
            <Button variant="outline" size="icon" onClick={() => setPage((p) => Math.min(totalPages, p + 1))} disabled={safePage >= totalPages}>
              <ChevronRight className="size-4" />
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
