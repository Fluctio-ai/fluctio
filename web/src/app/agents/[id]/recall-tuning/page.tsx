"use client";

import { useEffect, useState, useCallback, type ReactNode } from "react";
import { useT } from "@/lib/i18n";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Database, Sparkles, FlaskConical, Search, Loader2 } from "lucide-react";
import {
  getAgentRecallTuning,
  previewRecall,
  type RecallTuningState,
  type RecallTestHit,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

// Per-agent recall-tuning panel — surfaces the otherwise-black-box MMR
// lambda bandit (current lambda, recall counts, per-lambda feedback) and
// a test box to preview which summaries a query recalls. Read-only
// scoring state + a coverage preview (excludes vector/reranker/MMR).
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

  const refresh = useCallback(async () => {
    try {
      setState(await getAgentRecallTuning(agentId));
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const runTest = async () => {
    if (!testQuery.trim()) return;
    setTesting(true);
    setTestHits(null);
    setTestNote(null);
    try {
      const res = await previewRecall(agentId, testQuery);
      setTestHits(res.results ?? []);
      if (res.note) setTestNote(res.note);
    } finally {
      setTesting(false);
    }
  };

  if (loading) return <Skeleton className="h-40 w-full" />;
  if (!state || state.ok === false) {
    return (
      <div className="p-4 text-sm text-muted-foreground">
        {state?.error || t("recallTuning.unavailable")}
      </div>
    );
  }

  const exploreRate = state.total_recalls
    ? (state.explored_recalls ?? 0) / state.total_recalls
    : 0;

  return (
    <div className="space-y-6 p-4">
      <div className="flex items-center gap-2">
        <FlaskConical className="h-5 w-5 text-muted-foreground" />
        <h2 className="text-lg font-semibold">{t("recallTuning.title")}</h2>
      </div>
      <p className="text-sm text-muted-foreground">{t("recallTuning.description")}</p>

      <div className="grid grid-cols-3 gap-3">
        <Stat
          label={t("recallTuning.currentLambda")}
          value={state.mmr_lambda?.toFixed(2) ?? "—"}
          icon={<Sparkles className="h-4 w-4" />}
        />
        <Stat
          label={t("recallTuning.totalRecalls")}
          value={String(state.total_recalls ?? 0)}
          icon={<Database className="h-4 w-4" />}
        />
        <Stat
          label={t("recallTuning.exploreRate")}
          value={`${(exploreRate * 100).toFixed(0)}%`}
          icon={<Sparkles className="h-4 w-4" />}
        />
      </div>

      <div>
        <h3 className="mb-2 text-sm font-medium">{t("recallTuning.feedbackStats")}</h3>
        {(state.feedback_stats?.length ?? 0) === 0 ? (
          <p className="text-sm text-muted-foreground">{t("recallTuning.noFeedback")}</p>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-muted-foreground">
                <th className="py-1">{t("recallTuning.lambda")}</th>
                <th className="py-1">{t("recallTuning.ups")}</th>
                <th className="py-1">{t("recallTuning.downs")}</th>
                <th className="py-1">{t("recallTuning.winRate")}</th>
              </tr>
            </thead>
            <tbody>
              {state.feedback_stats!.map((s) => {
                const total = s.ups + s.downs;
                const rate = total ? (s.ups / total) * 100 : 0;
                return (
                  <tr key={s.lambda} className="border-t">
                    <td className="py-1">{s.lambda.toFixed(2)}</td>
                    <td className="py-1">{s.ups}</td>
                    <td className="py-1">{s.downs}</td>
                    <td className="py-1">{rate.toFixed(0)}%</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      <div>
        <h3 className="mb-2 text-sm font-medium">{t("recallTuning.testBox")}</h3>
        <div className="flex gap-2">
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
        {testNote && <p className="mt-1 text-xs text-muted-foreground">{testNote}</p>}
        {testHits && testHits.length === 0 && (
          <p className="mt-2 text-sm text-muted-foreground">{t("recallTuning.noResults")}</p>
        )}
        {testHits && testHits.length > 0 && (
          <ul className="mt-2 space-y-2">
            {testHits.map((h) => (
              <li key={h.id} className="rounded border p-2 text-sm">
                {h.topic && <div className="font-medium">{h.topic}</div>}
                <div className="text-muted-foreground">{h.summary}</div>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

function Stat({ label, value, icon }: { label: string; value: string; icon: ReactNode }) {
  return (
    <div className="rounded-lg border p-3">
      <div className="mb-1 flex items-center gap-1 text-xs text-muted-foreground">
        {icon}
        {label}
      </div>
      <div className="text-xl font-semibold">{value}</div>
    </div>
  );
}
