"use client";

import { useEffect, useState, useCallback, type ReactNode } from "react";
import { useT } from "@/lib/i18n";
import { Skeleton } from "@/components/ui/skeleton";
import { Database, Sparkles, FlaskConical } from "lucide-react";
import { getAgentRecallTuning, type RecallTuningState } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

// Per-agent recall-tuning panel — surfaces the otherwise-black-box MMR
// lambda bandit: current lambda, recall counts, per-lambda feedback
// stats. Lets the user observe how the recall scorer auto-tunes. Read-
// only for now (a test-query box + manual override follow).
export default function AgentRecallTuningPage() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [state, setState] = useState<RecallTuningState | null>(null);
  const [loading, setLoading] = useState(true);

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
