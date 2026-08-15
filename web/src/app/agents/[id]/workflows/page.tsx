"use client";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { ArrowLeft, Play, Plus, Trash2 } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  deleteWorkflow,
  deleteWorkflowRun,
  getWorkflowRun,
  listWorkflows,
  listWorkflowRuns,
  resumeWorkflowStream,
  runWorkflowStream,
  saveWorkflow,
  type WorkflowNodeOutput,
  type WorkflowRunEvent,
  type WorkflowRunRow,
  type WorkflowSummary,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";
import { SchemaForm, WorkflowEditor } from "@/components/workflow-editor";

// Workflows page (ticket 08): list this agent's workflows, manually trigger one
// (JSON input), and browse run history with per-node output + the failing
// node's error. Visibility is ownership — only this agent's workflows show.

// StartedAt arrives as a raw RFC3339 UTC string ("2026-08-14T01:59:29Z");
// render it in the viewer's local zone as a compact "MM-DD HH:mm" so the
// history rows stay scannable instead of leaking ISO noise.
function formatRunTime(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime())
    ? iso
    : d.toLocaleString([], { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}

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
  const [liveEvents, setLiveEvents] = useState<WorkflowRunEvent[]>([]);
  const [selRun, setSelRun] = useState<{ run: WorkflowRunRow; nodes: WorkflowNodeOutput[] } | null>(null);
  // M6 form interaction: the run the live stream / picked history row is
  // waiting on, the values being typed into its form, and the in-flight flag.
  const [waitingForm, setWaitingForm] = useState<{ runID: string; node: string; schema: Record<string, unknown> } | null>(null);
  const [formValues, setFormValues] = useState<Record<string, unknown>>({});
  const [resuming, setResuming] = useState(false);
  // One-shot command to the editor: import a node's actual run output as its
  // output schema (nonce makes each click a fresh command even for same node).
  const [schemaInject, setSchemaInject] = useState<{ node: string; schema: Record<string, unknown>; nonce: number } | null>(null);
  // Draggable width of the workflow list pane — same pattern as the
  // knowledge-base source list (pointer events on the document, 220–520px).
  const [listW, setListW] = useState(320);
  const startListDrag = (e: React.PointerEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startW = listW;
    const move = (ev: PointerEvent) => {
      const dx = ev.clientX - startX;
      setListW(Math.min(520, Math.max(220, startW + dx)));
    };
    const up = () => {
      document.removeEventListener("pointermove", move);
      document.removeEventListener("pointerup", up);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
    };
    document.addEventListener("pointermove", move);
    document.addEventListener("pointerup", up);
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
  };

  // inferOutputSchema turns an actual node output object into the {field:{type}}
  // declaration the editor's output schema (and downstream FieldRef) expects.
  const inferOutputSchema = (out: Record<string, unknown> | null | undefined): Record<string, unknown> => {
    const s: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(out || {})) {
      if (typeof v === "string") s[k] = { type: "string" };
      else if (typeof v === "boolean") s[k] = { type: "boolean" };
      else if (typeof v === "number") s[k] = { type: Number.isInteger(v) ? "integer" : "number" };
      else if (Array.isArray(v)) s[k] = { type: "array" };
      else if (v !== null && typeof v === "object") s[k] = { type: "object" };
    }
    return s;
  };

  useEffect(() => {
    if (!agentId) return;
    let aborted = false;
    setLoading(true);
    listWorkflows(agentId).then((list) => {
      if (aborted) return;
      setWorkflows(list);
      setLoading(false);
      // Desktop keeps auto-selecting the first workflow; mobile lands on
      // the list first (master-detail) instead of jumping into the detail.
      setSelected(
        (cur) => cur ?? (window.matchMedia("(min-width: 768px)").matches ? list[0]?.id ?? null : null),
      );
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

  // asWaitingResult narrows the terminal SSE "result" payload to the fields
  // the waiting path reads (status / run_id / pending_form).
  const asWaitingResult = (
    r: Record<string, unknown> | undefined,
  ): { run_id?: string; pending_form?: { node: string; schema: Record<string, unknown> } } | undefined =>
    r as { run_id?: string; pending_form?: { node: string; schema: Record<string, unknown> } } | undefined;

  const onRun = async () => {
    if (!agentId || !selected) return;
    setRunning(true);
    setLastResult("");
    setLiveEvents([]);
    setWaitingForm(null);
    setFormValues({});
    try {
      const parsed = JSON.parse(input || "{}");
      await runWorkflowStream(agentId, selected, parsed, (e) => {
        setLiveEvents((cur) => [...cur, e]);
        if (e.type === "result") {
          const wr = asWaitingResult(e.result);
          if (e.result?.status === "waiting" && wr?.pending_form) {
            setWaitingForm({ runID: wr.run_id || "", node: wr.pending_form.node, schema: wr.pending_form.schema });
            setLastResult("");
          } else {
            setLastResult(e.result ? JSON.stringify(e.result, null, 2) : "");
          }
        } else if (e.type === "error") {
          setLastResult("Error: " + (e.error || "run failed"));
        }
      });
      refreshRuns();
    } catch (e) {
      setLastResult("Error: " + (e instanceof Error ? e.message : String(e)));
    } finally {
      setRunning(false);
    }
  };

  // onSubmitForm resumes the waiting run with the typed values. The resumed
  // walk streams back through the same live-events surface; a failed resume
  // (e.g. a required field missing) keeps the form up — the run stays waiting.
  const onSubmitForm = async () => {
    if (!agentId || !selected || !waitingForm) return;
    const runID = waitingForm.runID;
    setResuming(true);
    try {
      await resumeWorkflowStream(agentId, selected, runID, formValues, (e) => {
        setLiveEvents((cur) => [...cur, e]);
        if (e.type === "result") {
          const wr = asWaitingResult(e.result);
          if (e.result?.status === "waiting" && wr?.pending_form) {
            // Multi-form workflow: the walk parked on the next form.
            setWaitingForm({ runID: wr.run_id || runID, node: wr.pending_form.node, schema: wr.pending_form.schema });
            setFormValues({});
            setLastResult("");
          } else {
            setWaitingForm(null);
            setLastResult(e.result ? JSON.stringify(e.result, null, 2) : "");
          }
        } else if (e.type === "error") {
          setLastResult("Error: " + (e.error || "resume failed"));
        }
      });
      refreshRuns();
      if (selRun?.run.ID === runID) setSelRun(await getWorkflowRun(agentId, selected, runID));
    } catch (e) {
      setLastResult("Error: " + (e instanceof Error ? e.message : String(e)));
    } finally {
      setResuming(false);
    }
  };

  // liveNodes aggregates the event stream into per-node status (M4 streaming):
  // node_start marks a node running; node_complete finalizes it.
  const liveNodes = liveEvents.reduce<Record<string, { status: string; error?: string }>>(
    (acc, e) => {
      if (!e.node) return acc;
      if (e.type === "node_start") acc[e.node] = { status: "running" };
      else if (e.type === "node_complete") acc[e.node] = { status: e.status || "", error: e.error };
      return acc;
    },
    {},
  );

  const onPickRun = async (runId: string) => {
    if (!agentId || !selected) return;
    const d = await getWorkflowRun(agentId, selected, runId);
    setSelRun(d);
    // A waiting history row reopens its form (M6): render + submit resumes it.
    if (d.run.Status === "waiting" && d.run.PendingFormNode && d.run.PendingFormSchema) {
      try {
        setWaitingForm({ runID: runId, node: d.run.PendingFormNode, schema: JSON.parse(d.run.PendingFormSchema) });
        setFormValues({});
      } catch {
        // bad persisted schema JSON — leave the form closed
      }
    }
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
      <div
        style={{ "--pane-lw": `${listW}px` } as any}
        className={cn(
          "border-r bg-muted/30 flex-col w-full md:w-[var(--pane-lw)] md:shrink-0 overflow-y-auto p-3 space-y-1",
          selected ? "hidden md:flex" : "flex",
        )}
      >
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
              {w.description && (
                <div className="text-xs text-muted-foreground truncate">{w.description}</div>
              )}
            </button>
          ))
        )}
      </div>
      {/* Drag handle: widen/narrow the workflow list pane (220–520px),
          styled to match the knowledge-base source list divider. Hidden on
          mobile where the list/detail panes swap instead of sitting side by side. */}
      <div
        className="hidden md:block w-1 shrink-0 cursor-col-resize hover:bg-primary/40 transition-colors"
        onPointerDown={startListDrag}
      />

      <div className={cn("flex-1 min-w-0 overflow-y-auto p-4 space-y-6", selected ? "block" : "hidden md:block")}>
        {!selected ? (
          <p className="text-muted-foreground">{t("workflow.selectHint")}</p>
        ) : (
          <>
            <section className="space-y-2">
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => setSelected(null)}
                  className="md:hidden -ml-1 shrink-0 text-muted-foreground hover:text-foreground"
                  aria-label={t("common.back")}
                >
                  <ArrowLeft className="h-5 w-5" />
                </button>
                <h3 className="font-semibold">{t("workflow.editor")}</h3>
              </div>
              <WorkflowEditor
                agentId={agentId ?? ""}
                wfID={selected}
                injectOutput={schemaInject}
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
              {Object.keys(liveNodes).length > 0 && (
                <div className="space-y-1">
                  {Object.entries(liveNodes).map(([name, n]) => (
                    <div key={name} className="flex items-center gap-2 text-xs border rounded px-2 py-1">
                      <StatusBadge status={n.status} />
                      <span className="font-medium">{name}</span>
                      {n.error && <span className="text-destructive truncate">{n.error}</span>}
                    </div>
                  ))}
                </div>
              )}
              {lastResult && (
                <pre className="text-xs bg-muted p-2 rounded max-h-56 overflow-auto whitespace-pre-wrap">
                  {lastResult}
                </pre>
              )}
              {waitingForm && (
                <div className="space-y-2 border rounded-lg p-3">
                  <div className="flex items-center gap-2">
                    <StatusBadge status="waiting" />
                    <span className="text-sm font-medium">{t("workflow.formWaiting")}</span>
                    <span className="text-xs text-muted-foreground font-mono">{waitingForm.node}</span>
                  </div>
                  <SchemaForm
                    schema={waitingForm.schema}
                    values={formValues}
                    onChange={setFormValues}
                    header={t("workflow.formFillHint")}
                  />
                  <Button size="sm" onClick={onSubmitForm} disabled={resuming}>
                    {resuming ? t("workflow.running") : t("workflow.formSubmit")}
                  </Button>
                </div>
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
                        <span className="text-xs text-muted-foreground truncate" title={r.StartedAt}>{formatRunTime(r.StartedAt)}</span>
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
                        <>
                          <pre className="mt-1 bg-muted p-1 rounded whitespace-pre-wrap">
                            {JSON.stringify(n.Output, null, 2)}
                          </pre>
                          {n.Status === "succeeded" && Object.keys(n.Output).length > 0 && (
                            <Button
                              size="sm"
                              variant="ghost"
                              className="h-6 px-2 text-xs"
                              onClick={() =>
                                setSchemaInject({ node: n.NodeID, schema: inferOutputSchema(n.Output as Record<string, unknown>), nonce: Date.now() })
                              }
                            >
                              {t("workflow.importSchema")}
                            </Button>
                          )}
                        </>
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

// Status semantics ride the dedicated semantic tokens (globals.css single
// source of truth). Attention is allocated by rarity: succeeded is the
// common case and stays a quiet tint — it used to take the filled brand
// blue and drowned the failed/waiting rows it should defer to. Failed
// keeps the loudest treatment; waiting carries the warning axis.
function StatusBadge({ status }: { status: string }) {
  if (status === "succeeded")
    return (
      <Badge variant="outline" className="border-success/40 bg-success/10 text-success">
        {status}
      </Badge>
    );
  if (status === "failed" || status === "needs_intervention")
    return <Badge variant="destructive">{status}</Badge>;
  if (status === "waiting")
    return (
      <Badge variant="outline" className="border-warning/40 bg-warning/10 text-warning">
        {status}
      </Badge>
    );
  return <Badge variant="secondary">{status}</Badge>;
}
