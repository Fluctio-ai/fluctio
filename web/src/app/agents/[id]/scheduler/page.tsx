"use client";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Clock,
  Trash2,
  Repeat,
  Calendar,
  Hourglass,
  MessageSquare,
} from "lucide-react";
import {
  listAgentCronJobs,
  deleteAgentCronJob,
  toggleAgentCronJob,
  type AgentCronJob,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";
import { useT } from "@/lib/i18n";

// Scheduler page: lists every cron job the agent has on file. The
// `create_cron_job` tool the agent itself uses writes here, so anything
// you said in chat ("每分钟讲笑话", "5 分钟后提醒我睡觉") shows up as a
// row. Disable to pause without losing the job; delete to remove it.

type TFunc = (key: string, vars?: Record<string, string | number>) => string;

function fmtSchedule(job: AgentCronJob, t: TFunc): string {
  switch (job.type) {
    case "interval":
      return t("scheduler.every", { schedule: job.schedule });
    case "once":
      return t("scheduler.at", { schedule: job.schedule });
    case "cron":
    default:
      return job.schedule;
  }
}

function fmtRelative(iso: string | undefined, t: TFunc): string {
  if (!iso) return "—";
  const ms = new Date(iso).getTime();
  if (Number.isNaN(ms)) return iso;
  const diff = ms - Date.now();
  const abs = Math.abs(diff);
  const mins = Math.round(abs / 60_000);
  if (mins < 1) return diff > 0 ? t("scheduler.inUnder1m") : t("scheduler.justNow");
  if (mins < 60) return diff > 0 ? t("scheduler.inMins", { n: mins }) : t("scheduler.minsAgo", { n: mins });
  const hours = Math.round(mins / 60);
  if (hours < 48) return diff > 0 ? t("scheduler.inHours", { n: hours }) : t("scheduler.hoursAgo", { n: hours });
  const days = Math.round(hours / 24);
  return diff > 0 ? t("scheduler.inDays", { n: days }) : t("scheduler.daysAgo", { n: days });
}

function typeIcon(type: string) {
  switch (type) {
    case "interval":
      return <Repeat className="h-3.5 w-3.5" />;
    case "once":
      return <Hourglass className="h-3.5 w-3.5" />;
    case "cron":
    default:
      return <Calendar className="h-3.5 w-3.5" />;
  }
}

export default function AgentSchedulerPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const t = useT();

  const [jobs, setJobs] = useState<AgentCronJob[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<AgentCronJob | null>(null);
  // Track in-flight toggles by job id so the row reflects optimistic
  // state and the switch doesn't double-fire while the request is open.
  const [toggling, setToggling] = useState<Record<string, boolean>>({});

  const refresh = useCallback(() => {
    if (!agentId) return;
    setLoading(true);
    listAgentCronJobs(agentId)
      .then((list) => {
        setJobs(list);
        setError("");
      })
      .catch((e) => setError(e instanceof Error ? e.message : t("scheduler.loadFailed")))
      .finally(() => setLoading(false));
  }, [agentId, t]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const handleToggle = async (job: AgentCronJob, enabled: boolean) => {
    if (!agentId || toggling[job.id]) return;
    setToggling((m) => ({ ...m, [job.id]: true }));
    // Optimistic update — flip immediately, server is authoritative on
    // refresh. Reverts on failure.
    setJobs((prev) =>
      prev.map((j) => (j.id === job.id ? { ...j, enabled } : j)),
    );
    const res = await toggleAgentCronJob(agentId, job.id, enabled);
    setToggling((m) => {
      const { [job.id]: _drop, ...rest } = m;
      void _drop;
      return rest;
    });
    if (res.error || !res.ok) {
      setError(res.error || t("scheduler.updateFailed"));
      // Revert by refetching the canonical state.
      refresh();
    }
  };

  const handleDelete = async () => {
    if (!deleteTarget || !agentId) return;
    const target = deleteTarget;
    setDeleteTarget(null);
    const res = await deleteAgentCronJob(agentId, target.id);
    if (res.error) setError(res.error);
    refresh();
  };

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <div className="flex items-center gap-2">
            <Clock className="size-5 text-muted-foreground" />
            <h2 className="text-2xl font-semibold tracking-tight">{t("scheduler.title")}</h2>
          </div>
          <p className="text-sm text-muted-foreground mt-1">
            {t("scheduler.subtitle", { name: agentName || t("scheduler.thisAgent") })}
          </p>
        </div>
      </div>

      {error && (
        <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-4">
          <p className="text-sm text-destructive">{error}</p>
        </div>
      )}

      {loading ? (
        <div className="space-y-2">
          <Skeleton className="h-20" />
          <Skeleton className="h-20" />
          <Skeleton className="h-20" />
        </div>
      ) : jobs.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border bg-card/50 p-10 text-center">
          <Clock className="mx-auto size-8 text-muted-foreground/50 mb-3" />
          <p className="text-sm text-muted-foreground">
            {t("scheduler.noTasks")}
          </p>
        </div>
      ) : (
        <div className="grid gap-3">
          {jobs.map((job) => (
            <JobRow
              key={job.id}
              job={job}
              busy={!!toggling[job.id]}
              onToggle={(enabled) => handleToggle(job, enabled)}
              onDelete={() => setDeleteTarget(job)}
            />
          ))}
        </div>
      )}

      <AlertDialog
        open={!!deleteTarget}
        onOpenChange={(v) => !v && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("scheduler.deleteTask")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("scheduler.removeBody", { name: deleteTarget?.name || deleteTarget?.id || "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {t("common.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function JobRow({
  job,
  busy,
  onToggle,
  onDelete,
}: {
  job: AgentCronJob;
  busy: boolean;
  onToggle: (enabled: boolean) => void;
  onDelete: () => void;
}) {
  const t = useT();
  return (
    <div className="rounded-lg border border-border bg-card p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="flex-1 min-w-0 space-y-2">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="font-medium truncate">{job.name || job.id}</span>
            <Badge
              variant="outline"
              className="inline-flex items-center gap-1 text-[10px]"
            >
              {typeIcon(job.type)}
              {job.type}
            </Badge>
            <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-[11px]">
              {fmtSchedule(job, t)}
            </code>
            {job.silent ? (
              <Badge variant="secondary" className="text-[10px]">
                {t("scheduler.silent")}
              </Badge>
            ) : job.channel ? (
              <span className="text-[11px] text-muted-foreground">
                {t("scheduler.via")} {job.channel}
              </span>
            ) : null}
          </div>
          <div className="flex items-start gap-1.5 text-xs text-muted-foreground">
            <MessageSquare className="size-3.5 mt-0.5 shrink-0" />
            <span className="break-words">{job.message}</span>
          </div>
          <div className="flex gap-4 text-[11px] text-muted-foreground/80">
            <span>
              {t("scheduler.lastRun")}:{" "}
              <span className="font-mono">{fmtRelative(job.lastRun, t)}</span>
            </span>
            <span>
              {t("scheduler.nextRun")}:{" "}
              <span className="font-mono">{fmtRelative(job.nextRun, t)}</span>
            </span>
          </div>
        </div>
        <div className="flex items-center gap-1 shrink-0">
          <Switch
            checked={job.enabled}
            disabled={busy}
            onCheckedChange={(v) => onToggle(v)}
            aria-label={job.enabled ? t("scheduler.disable") : t("scheduler.enable")}
          />
          <Button
            size="icon"
            variant="ghost"
            className="text-destructive hover:text-destructive"
            onClick={onDelete}
            title={t("common.delete")}
          >
            <Trash2 className="size-4" />
          </Button>
        </div>
      </div>
    </div>
  );
}
