"use client";

import { useEffect, useState, useCallback, useRef } from "react";
import { useT } from "@/lib/i18n";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Search, Loader2 } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  getAgentRecallTuning,
  getRecentRecalls,
  sendRecallFeedback,
  previewRecall,
  type RecallTuningState,
  type RecallTestHit,
  type RecallEventView,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { PageHeader, SettingsCard, CardHead } from "@/components/settings-ui";

// Per-agent recall test page — a query box to preview which memories a
// message recalls (the backend runs the production [MEM] lane with
// recording off, so this can never drift from chat behavior), plus the
// audit list of what the lanes actually injected. 👍/👎 are audit marks
// (only votes on ε-greedy-explored memory_search recalls feed the λ
// bandit; the injection lane has no λ). The tuning knobs that used to
// live here never produced a perceptible effect at personal scale and
// misled more than they helped — semantic ranking + injection floors
// decide relevance now, so there is nothing left to tune by hand.
export default function AgentRecallTuningPage() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [state, setState] = useState<RecallTuningState | null>(null);
  const [loading, setLoading] = useState(true);

  // test box
  const [testQuery, setTestQuery] = useState("");
  const [testHits, setTestHits] = useState<RecallTestHit[] | null>(null);
  const [testing, setTesting] = useState(false);
  const [testNote, setTestNote] = useState<string | null>(null);
  const [recalls, setRecalls] = useState<RecallEventView[] | null>(null);
  const [recallsError, setRecallsError] = useState<string | null>(null);
  // Audit-list time filter (days; 0 = all time). Server-side so the fetch
  // cap (100) isn't wasted on months of history the filter would hide.
  const [days, setDays] = useState("0");
  // Out-of-order guard for the audit list: only the fetch issued for the
  // CURRENT (agentId, days) pair may write state. A slow response for a
  // previous agent/filter used to overwrite the newer view.
  const recallsReqRef = useRef(0);

  const refresh = useCallback(async () => {
    try {
      setState(await getAgentRecallTuning(agentId));
    } catch (e) {
      // getAgentRecallTuning only maps HTTP failures; a network-level
      // error must land in the same {ok:false,error} shape so the page
      // can show WHY it's unavailable instead of a bare empty state.
      setState({ ok: false, error: e instanceof Error ? e.message : String(e) });
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  const refreshRecalls = useCallback(
    async (d: string) => {
      const reqId = ++recallsReqRef.current;
      setRecallsError(null);
      try {
        const res = await getRecentRecalls(agentId, {
          limit: 100,
          days: d === "0" ? undefined : Number(d),
        });
        if (reqId !== recallsReqRef.current) return; // superseded
        if (res.ok === false) {
          // Distinguish "no records" from "the request failed" — the
          // failure used to render as a clean empty list.
          setRecallsError(res.error || "Failed to load recent recalls");
          return; // keep whatever list is already on screen
        }
        setRecalls(res.events ?? []);
      } catch (e) {
        if (reqId !== recallsReqRef.current) return; // superseded
        setRecallsError(e instanceof Error ? e.message : String(e));
      }
    },
    [agentId],
  );

  // The tuning fetch is an availability probe (the page renders no stats),
  // so it runs once on mount; the audit list refetches when the filter
  // changes.
  useEffect(() => {
    refresh();
  }, [refresh]);

  useEffect(() => {
    refreshRecalls(days);
  }, [refreshRecalls, days]);

  const [testError, setTestError] = useState<string | null>(null);
  const runTest = async () => {
    if (!testQuery.trim() || testing) return;
    setTesting(true);
    setTestHits(null);
    setTestNote(null);
    setTestError(null);
    try {
      const res = await previewRecall(agentId, testQuery);
      if (res.ok === false) {
        // A failed probe used to render as a clean "no results" empty state.
        setTestError(res.error || "Recall test failed");
        return;
      }
      setTestHits(res.results ?? []);
      if (res.note) setTestNote(res.note);
    } catch (e) {
      setTestError(e instanceof Error ? e.message : String(e));
    } finally {
      setTesting(false);
    }
  };

  const vote = async (recallId: string, up: boolean) => {
    // Optimistic mark so the click is visible immediately; the server
    // response (or refetch) confirms with the latest-vote state.
    const prevVote = recalls?.find((rc) => rc.recall_id === recallId)?.vote;
    setRecalls((rs) =>
      rs?.map((rc) =>
        rc.recall_id === recallId
          ? { ...rc, vote: up ? ("up" as const) : ("down" as const) }
          : rc,
      ) ?? rs,
    );
    const rollback = () =>
      setRecalls((rs) =>
        rs?.map((rc) =>
          rc.recall_id === recallId ? { ...rc, vote: prevVote } : rc,
        ) ?? rs,
      );
    try {
      const res = await sendRecallFeedback(recallId, up);
      if (res.ok === false) {
        // Roll the optimistic mark back and say so — a silently dropped
        // vote reads as "recorded" until the next refetch erases it.
        rollback();
        window.alert(`Failed to record vote: ${res.error || "unknown error"}`);
        return;
      }
    } catch (e) {
      rollback();
      window.alert(`Failed to record vote: ${e instanceof Error ? e.message : String(e)}`);
      return;
    }
    // Only refetch after a confirmed vote — a failed refetch used to wipe
    // the whole list (optimistic marks included) with no error shown.
    await refreshRecalls(days);
  };

  if (loading) return <Skeleton className="h-40 w-full" />;
  if (!state || state.ok === false) {
    return (
      <div className="p-4 text-sm text-muted-foreground">
        {state?.error || t("recallTuning.unavailable")}
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <PageHeader
        title={t("recallTuning.title")}
        desc={t("recallTuning.description")}
      />

      <SettingsCard>
        <CardHead title={t("recallTuning.testBox")} />
        <div className="mt-4 flex gap-2">
          <Input
            value={testQuery}
            onChange={(e) => setTestQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") runTest();
            }}
            placeholder={t("recallTuning.testPlaceholder")}
          />
          <Button onClick={runTest} disabled={testing || !testQuery.trim()}>
            {testing ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <Search className="h-4 w-4" />
            )}
          </Button>
        </div>
        {testNote && <p className="mt-2 text-xs text-muted-foreground">{testNote}</p>}
        {testError && <p className="mt-2 text-xs text-destructive">{testError}</p>}
        {testHits && testHits.length === 0 && (
          <p className="mt-3 text-sm text-muted-foreground">{t("recallTuning.noResults")}</p>
        )}
        {testHits && testHits.length > 0 && (
          <ul className="mt-3 space-y-2">
            {testHits.map((h) => (
              <li key={h.id} className="rounded-md border p-2 text-sm">
                {h.topic && <div className="font-medium">{h.topic}</div>}
                <div className="text-muted-foreground">{h.summary}</div>
              </li>
            ))}
          </ul>
        )}
      </SettingsCard>

      <SettingsCard>
        <CardHead
          title={t("recallTuning.recentRecalls")}
          control={
            <div className="flex items-center gap-2">
              {recalls && (
                <span className="text-xs text-muted-foreground tabular-nums">
                  {recalls.length}
                </span>
              )}
              <Select value={days} onValueChange={(v) => v && setDays(v)}>
                <SelectTrigger className="h-7 w-auto text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="0">{t("recallTuning.filterAll")}</SelectItem>
                  <SelectItem value="1">{t("recallTuning.filter24h")}</SelectItem>
                  <SelectItem value="7">{t("recallTuning.filter7d")}</SelectItem>
                  <SelectItem value="30">{t("recallTuning.filter30d")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          }
        />
        <p className="mt-2 text-xs text-muted-foreground">{t("recallTuning.auditNote")}</p>
        {recallsError && (
          <p className="mt-3 text-sm text-destructive">
            {recallsError}
            <Button variant="ghost" size="sm" className="ml-2 h-6 text-xs" onClick={() => refreshRecalls(days)}>
              Retry
            </Button>
          </p>
        )}
        {!recallsError && (recalls?.length ?? 0) === 0 ? (
          <p className="mt-4 text-sm text-muted-foreground">{t("recallTuning.noRecalls")}</p>
        ) : (
          <ul className="mt-4 space-y-2">
            {(recalls ?? []).map((rc) => (
              <li key={rc.recall_id} className="rounded border p-2 text-sm">
                <div className="mb-1 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                  <span className="font-mono">{new Date(rc.created_at).toLocaleString()}</span>
                  {/* λ only exists on memory_search-tool recalls; the [MEM]
                      injection lane has no MMR and records 0 — don't show a
                      fake knob value on those rows. */}
                  {rc.lambda > 0 && <span>λ={rc.lambda.toFixed(2)}</span>}
                  {rc.bandit_explored && (
                    <span className="rounded bg-muted px-1">{t("recallTuning.banditExplored")}</span>
                  )}
                  {rc.consumed && (
                    <span className="rounded bg-muted px-1">{t("recallTuning.consumed")}</span>
                  )}
                </div>
                <div className="mb-1 text-xs">
                  <span className="text-muted-foreground">{t("recallTuning.triggerQuery")}: </span>
                  <span className="font-medium">{rc.query || t("recallTuning.queryUnknown")}</span>
                </div>
                {rc.summaries.map((sm) => (
                  <div key={sm.id} className="border-b border-border/60 py-1.5 last:border-b-0 last:pb-0 first:pt-0">
                    <div className="flex items-baseline gap-2">
                      <span
                        className="w-8 shrink-0 text-right tabular-nums font-mono text-2xs text-muted-foreground"
                        title={t("recallTuning.relevanceHint")}
                      >
                        {sm.relevance != null ? sm.relevance.toFixed(2) : ""}
                      </span>
                      <span className="font-medium">{sm.topic || "—"}</span>
                    </div>
                    <div className="pl-10 text-xs leading-relaxed text-muted-foreground">{sm.summary}</div>
                  </div>
                ))}
                <div className="mt-1 flex gap-2">
                  <Button
                    size="sm"
                    variant={rc.vote === "up" ? "default" : "outline"}
                    onClick={() => vote(rc.recall_id, true)}
                  >
                    👍
                  </Button>
                  <Button
                    size="sm"
                    variant={rc.vote === "down" ? "default" : "outline"}
                    onClick={() => vote(rc.recall_id, false)}
                  >
                    👎
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </SettingsCard>
    </div>
  );
}
