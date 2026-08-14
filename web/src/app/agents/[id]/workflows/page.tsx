"use client";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { Play, Plus, Trash2 } from "lucide-react";
import {
  deleteWorkflow,
  deleteWorkflowRun,
  getWorkflowRun,
  listWorkflows,
  listWorkflowRuns,
  runWorkflow,
  saveWorkflow,
  type WorkflowNodeOutput,
  type WorkflowRunRow,
  type WorkflowSummary,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";
import { WorkflowEditor } from "@/components/workflow-editor";

// Workflows page (ticket 08): list this agent's workflows, manually trigger one
// (JSON input), and browse run history with per-node output + the failing
// node's error. Visibility is ownership — only this agent's workflows show.
export default function WorkflowsPage() {
  const agentId = useAgentIdFromURL();
  const t = useT();
  const [workflows, setWorkflows] = useState<WorkflowSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<string | null>(null);
  const [runs, setRuns] = useState<WorkflowRunRow[]>([]);
  const [runsLoading, setRunsLoading] = useState(false);
  const [input, setInput] = useState("{}");
  const [running, setRunning] = useState(false);
  const [lastResult, setLastResult] = useState<string>("");
  const [selRun, setSelRun] = useState<{ run: WorkflowRunRow; nodes: WorkflowNodeOutput[] } | null>(null);

  useEffect(() => {
    if (!agentId) return;
    let aborted = false;
    setLoading(true);
    listWorkflows(agentId).then((list) => {
      if (aborted) return;
      setWorkflows(list);
      setLoading(false);
      setSelected((cur) => cur ?? list[0]?.id ?? null);
    });
    return () => {
      aborted = true;
    };
  }, [agentId]);

  const refreshRuns = useCallback(async () => {
    if (!agentId || !selected) return;
    setRunsLoading(true);
    setRuns(await listWorkflowRuns(agentId, selected));
    setRunsLoading(false);
  }, [agentId, selected]);

  useEffect(() => {
    if (!selected) return;
    setSelRun(null);
    refreshRuns();
  }, [selected, refreshRuns]);

  const onRun = async () => {
    if (!agentId || !selected) return;
    setRunning(true);
    setLastResult("");
    try {
      const parsed = JSON.parse(input || "{}");
      const res = await runWorkflow(agentId, selected, parsed);
      setLastResult(
        res.ok && res.result
          ? JSON.stringify(res.result, null, 2)
          : "Error: " + (res.error || "run failed"),
      );
      refreshRuns();
    } catch (e) {
      setLastResult("Error: " + (e instanceof Error ? e.message : String(e)));
    } finally {
      setRunning(false);
    }
  };

  const onPickRun = async (runId: string) => {
    if (!agentId || !selected) return;
    setSelRun(await getWorkflowRun(agentId, selected, runId));
  };

  const onDeleteRun = async (runId: string) => {
    if (!agentId || !selected) return;
    await deleteWorkflowRun(agentId, selected, runId);
    if (selRun?.run.ID === runId) setSelRun(null);
    refreshRuns();
  };
  const onDeleteWorkflow = async () => {
    if (!agentId || !selected) return;
    if (!confirm(t("workflow.deleteConfirm"))) return;
    await deleteWorkflow(agentId, selected);
    const list = await listWorkflows(agentId);
    setWorkflows(list);
    setSelected(list[0]?.id ?? null);
  };
  const onCreateWorkflow = async () => {
    if (!agentId) return;
    const id = prompt(t("workflow.newIdPrompt"));
    if (!id) return;
    await saveWorkflow(agentId, id, "version: 1\nnodes: []\n");
    const list = await listWorkflows(agentId);
    setWorkflows(list);
    setSelected(id);
  };

  usePageHeader(
    <div className="flex items-center gap-2">
      <h1 className="text-sm font-semibold">{t("workflow.title")}</h1>
      <Button size="sm" variant="outline" onClick={onCreateWorkflow}>
        <Plus className="h-3.5 w-3.5" /> {t("workflow.create")}
      </Button>
    </div>,
    [t("workflow.create")],
  );
  return (
    <div className="flex h-full">
      <div className="w-60 shrink-0 border-r overflow-y-auto p-3 space-y-1">
        {loading ? (
          <Skeleton className="h-8 w-full" />
        ) : workflows.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("workflow.empty")}</p>
        ) : (
          workflows.map((w) => (
            <button
              key={w.id}
              onClick={() => setSelected(w.id)}
              className={
                "w-full text-left rounded px-2 py-1.5 text-sm hover:bg-accent " +
                (selected === w.id ? "bg-accent" : "")
              }
            >
              <div className="font-medium truncate">{w.id}</div>
              <div className="text-xs text-muted-foreground truncate">{w.description || "—"}</div>
            </button>
          ))
        )}
      </div>

      <div className="flex-1 overflow-y-auto p-4 space-y-6">
        {!selected ? (
          <p className="text-muted-foreground">{t("workflow.selectHint")}</p>
        ) : (
          <>
            <section className="space-y-2">
              <h3 className="font-semibold">{t("workflow.editor")}</h3>
              <WorkflowEditor
                agentId={agentId ?? ""}
                wfID={selected}
                onSaved={() => {
                  if (agentId) listWorkflows(agentId).then(setWorkflows);
                }}
                onDelete={onDeleteWorkflow}
              />
            </section>
            <section className="space-y-2">
              <h3 className="font-semibold">{t("workflow.trigger")}</h3>
              <p className="text-xs text-muted-foreground">{t("workflow.inputHint")}</p>
              <Textarea
                value={input}
                onChange={(e) => setInput(e.target.value)}
                rows={3}
                className="font-mono text-xs"
              />
              <Button onClick={onRun} disabled={running}>
                <Play className="h-4 w-4" />
                {running ? t("workflow.running") : t("workflow.run")}
              </Button>
              {lastResult && (
                <pre className="text-xs bg-muted p-2 rounded max-h-56 overflow-auto whitespace-pre-wrap">
                  {lastResult}
                </pre>
              )}
            </section>

            <section className="space-y-2">
              <h3 className="font-semibold">{t("workflow.history")}</h3>
              {runsLoading ? (
                <Skeleton className="h-8 w-full" />
              ) : runs.length === 0 ? (
                <p className="text-sm text-muted-foreground">{t("workflow.noRuns")}</p>
              ) : (
                <div className="space-y-1">
                  {runs.map((r) => (
                    <div
                      key={r.ID}
                      className="flex items-center gap-2 text-sm border rounded px-2 py-1.5"
                    >
                      <button
                        onClick={() => onPickRun(r.ID)}
                        className="flex-1 text-left flex items-center gap-2 min-w-0"
                      >
                        <StatusBadge status={r.Status} />
                        <span className="font-mono text-xs truncate">{r.ID}</span>
                        <span className="text-xs text-muted-foreground shrink-0">v{r.Version}</span>
                        <span className="text-xs text-muted-foreground truncate">{r.StartedAt}</span>
                      </button>
                      <Button size="sm" variant="ghost" onClick={() => onDeleteRun(r.ID)}>
                        <Trash2 className="h-3 w-3" />
                      </Button>
                    </div>
                  ))}
                </div>
              )}
            </section>

            {selRun && (
              <section className="space-y-2">
                <h3 className="font-semibold">{t("workflow.runDetail")}</h3>
                {selRun.run.Error && (
                  <p className="text-sm text-destructive whitespace-pre-wrap">{selRun.run.Error}</p>
                )}
                <div className="space-y-1">
                  {selRun.nodes.map((n, i) => (
                    <div key={i} className="border rounded p-2 text-xs">
                      <div className="flex items-center gap-2">
                        <StatusBadge status={n.Status} />
                        <span className="font-medium">{n.NodeID}</span>
                        <span className="text-muted-foreground">attempt {n.Attempt}</span>
                      </div>
                      {n.Error && (
                        <p className="text-destructive mt-1 whitespace-pre-wrap">{n.Error}</p>
                      )}
                      {n.Output && (
                        <pre className="mt-1 bg-muted p-1 rounded whitespace-pre-wrap">
                          {JSON.stringify(n.Output, null, 2)}
                        </pre>
                      )}
                    </div>
                  ))}
                </div>
              </section>
            )}
          </>
        )}
      </div>
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  const variant =
    status === "succeeded"
      ? "default"
      : status === "failed" || status === "needs_intervention"
        ? "destructive"
        : "secondary";
  return <Badge variant={variant as "default" | "destructive" | "secondary"}>{status}</Badge>;
}
