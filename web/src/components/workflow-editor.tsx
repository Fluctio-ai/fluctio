"use client";

import * as React from "react";
import { useEffect, useRef, useState } from "react";
import { load as yamlLoad, dump as yamlDump } from "js-yaml";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { Skeleton } from "@/components/ui/skeleton";
import { Plus, Trash2, Save } from "lucide-react";
import { getWorkflowYAML, saveWorkflow } from "@/lib/api";
import { useT } from "@/lib/i18n";

// WorkflowEditor is the visual + YAML editor for one workflow (ticket 09).
// The model is the raw YAML object (js-yaml load), so fields the UI doesn't
// surface are preserved through edits (spec decision 8 pass-through). The
// canvas is vis-network (same stack as the wiki graph); adding nodes/edges is
// button-driven, dragging moves nodes; selecting a node/edge opens its property
// panel. The YAML pane is two-way: editing it reparses into the canvas, and
// canvas edits re-dump to YAML. Save goes through PUT (parse + Validate +
// version+1 on the server).

type AnyDef = {
  id?: string;
  version?: number;
  description?: string;
  concurrency?: string;
  input?: { schema?: Record<string, unknown> };
  nodes?: AnyNode[];
  edges?: AnyEdge[];
  [k: string]: unknown;
};
type AnyNode = {
  name: string;
  kind: string;
  tool?: string;
  prompt?: string;
  input?: Record<string, unknown>;
  output?: Record<string, unknown>;
  side_effect?: string;
  [k: string]: unknown;
};
type AnyEdge = {
  from: string;
  to: string;
  when?: string;
  desc?: string;
  [k: string]: unknown;
};

export function WorkflowEditor({
  agentId,
  wfID,
  onSaved,
}: {
  agentId: string;
  wfID: string;
  onSaved: () => void;
}) {
  const t = useT();
  const [def, setDef] = React.useState<AnyDef | null>(null);
  const [yamlText, setYamlText] = useState("");
  const [selNode, setSelNode] = useState<string | null>(null);
  const [selEdge, setSelEdge] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const networkRef = useRef<unknown>(null);
  const nodesDsRef = useRef<unknown>(null);
  const edgesDsRef = useRef<unknown>(null);

  // Load YAML → model.
  useEffect(() => {
    let aborted = false;
    setLoading(true);
    setMsg(null);
    getWorkflowYAML(agentId, wfID).then((y) => {
      if (aborted) return;
      setYamlText(y);
      try {
        const parsed = (yamlLoad(y) || {}) as AnyDef;
        if (!parsed.nodes) parsed.nodes = [];
        if (!parsed.edges) parsed.edges = [];
        setDef(parsed);
      } catch {
        setDef({ nodes: [], edges: [] });
      }
      setLoading(false);
    });
    return () => {
      aborted = true;
    };
  }, [agentId, wfID]);

  // (Re)build the vis-network canvas whenever the model changes.
  useEffect(() => {
    if (!def || !containerRef.current) return;
    let cancelled = false;
    Promise.all([import("vis-network/standalone"), import("vis-data/standalone")]).then(
      ([nw, ds]) => {
        if (cancelled || !containerRef.current) return;
        if (networkRef.current) {
          (networkRef.current as { destroy: () => void }).destroy();
        }
        const nodes = new (ds as unknown as { DataSet: new (d: unknown[]) => unknown }).DataSet(
          def.nodes!.map((n) => ({
            id: n.name,
            label: `${n.name}\n(${n.kind})`,
            color: n.kind === "llm" ? "#6366f1" : "#10b981",
          })),
        );
        const edges = new (ds as unknown as { DataSet: new (d: unknown[]) => unknown }).DataSet(
          def.edges!.map((e, i) => ({ id: i, from: e.from, to: e.to, label: e.when || "" })),
        );
        nodesDsRef.current = nodes;
        edgesDsRef.current = edges;
        const network = new (nw as unknown as { Network: new (...a: unknown[]) => unknown }).Network(
          containerRef.current,
          { nodes, edges },
          {
            layout: { hierarchical: false },
            nodes: { shape: "box", margin: 10, font: { size: 13 } },
            edges: { arrows: "to", font: { size: 11, align: "middle" } },
            interaction: { hover: true },
          },
        );
        (network as unknown as { on: (e: string, cb: (p: { nodes: string[]; edges: number[] }) => void) => void }).on("click", (params) => {
          if (params.nodes.length > 0) {
            setSelNode(params.nodes[0]);
            setSelEdge(null);
          } else if (params.edges.length > 0) {
            setSelEdge(params.edges[0]);
            setSelNode(null);
          } else {
            setSelNode(null);
            setSelEdge(null);
          }
        });
        networkRef.current = network;
      },
    );
    return () => {
      cancelled = true;
    };
  }, [def]);

  const dump = (d: AnyDef) => {
    try {
      return yamlDump(d, { lineWidth: -1, noRefs: true });
    } catch {
      return "";
    }
  };

  // Apply a model mutation, then re-dump YAML (model → YAML sync).
  const mutate = (fn: (d: AnyDef) => void) => {
    setDef((cur) => {
      if (!cur) return cur;
      const next: AnyDef = JSON.parse(JSON.stringify(cur)); // deep clone, preserves unknown fields
      fn(next);
      setYamlText(dump(next));
      return next;
    });
  };

  const addNode = (kind: "tool" | "llm") => {
    const name = `${kind}_${Date.now().toString(36).slice(-4)}`;
    mutate((d) => {
      d.nodes!.push({ name, kind });
    });
    setSelNode(name);
    setSelEdge(null);
  };

  const deleteSelected = () => {
    if (selNode) {
      const n = selNode;
      mutate((d) => {
        d.nodes = d.nodes!.filter((x) => x.name !== n);
        d.edges = d.edges!.filter((e) => e.from !== n && e.to !== n);
      });
      setSelNode(null);
    } else if (selEdge != null) {
      const i = selEdge;
      mutate((d) => {
        d.edges!.splice(i, 1);
      });
      setSelEdge(null);
    }
  };

  const addEdge = (target: string) => {
    if (!selNode || selNode === target) return;
    mutate((d) => {
      d.edges!.push({ from: selNode, to: target });
    });
  };

  const editNode = (prop: keyof AnyNode, value: string) => {
    if (!selNode) return;
    const n = selNode;
    mutate((d) => {
      const node = d.nodes!.find((x) => x.name === n);
      if (node) {
        if (value === "") delete (node as Record<string, unknown>)[prop as string];
        else (node as Record<string, unknown>)[prop as string] = value;
      }
    });
  };

  const editEdge = (prop: keyof AnyEdge, value: string) => {
    if (selEdge == null) return;
    const i = selEdge;
    mutate((d) => {
      const e = d.edges![i];
      if (!e) return;
      if (value === "") delete (e as Record<string, unknown>)[prop as string];
      else (e as Record<string, unknown>)[prop as string] = value;
    });
  };

  // YAML → model (two-way). Parse errors surface but keep the text so the user
  // can fix them; a successful parse refreshes the canvas.
  const onYamlChange = (v: string) => {
    setYamlText(v);
    try {
      const parsed = (yamlLoad(v) || {}) as AnyDef;
      if (parsed && Array.isArray(parsed.nodes)) {
        setDef({ ...parsed, nodes: parsed.nodes || [], edges: parsed.edges || [] });
        setMsg(null);
      }
    } catch (e) {
      setMsg({ ok: false, text: t("workflow.yamlError") + ": " + (e as Error).message });
    }
  };

  const save = async () => {
    setSaving(true);
    setMsg(null);
    const res = await saveWorkflow(agentId, wfID, yamlText);
    setSaving(false);
    if (res.ok) {
      setMsg({ ok: true, text: t("workflow.saved", { v: res.version ?? 0 }) });
      onSaved();
    } else {
      setMsg({ ok: false, text: res.error || t("workflow.saveFailed") });
    }
  };

  if (loading) return <Skeleton className="h-64 w-full" />;
  if (!def) return null;
  const selNodeObj = selNode ? def.nodes!.find((n) => n.name === selNode) : null;
  const selEdgeObj = selEdge != null ? def.edges![selEdge] : null;

  return (
    <section className="space-y-3">
      <div className="flex items-center gap-2 flex-wrap">
        <Button size="sm" variant="outline" onClick={() => addNode("tool")}>
          <Plus className="h-3.5 w-3.5" /> {t("workflow.addTool")}
        </Button>
        <Button size="sm" variant="outline" onClick={() => addNode("llm")}>
          <Plus className="h-3.5 w-3.5" /> {t("workflow.addLLM")}
        </Button>
        <Button size="sm" variant="outline" onClick={deleteSelected} disabled={!selNode && selEdge == null}>
          <Trash2 className="h-3.5 w-3.5" /> {t("workflow.deleteSel")}
        </Button>
        <Button size="sm" onClick={save} disabled={saving}>
          <Save className="h-3.5 w-3.5" /> {saving ? t("workflow.saving") : t("workflow.save")}
        </Button>
        {msg && (
          <span className={msg.ok ? "text-xs text-green-600" : "text-xs text-destructive"}>
            {msg.text}
          </span>
        )}
      </div>

      <div className="flex gap-3">
        <div ref={containerRef} className="h-72 flex-1 rounded border bg-muted/30" />
        <div className="w-64 shrink-0 space-y-2 text-xs">
          <h4 className="font-semibold">{t("workflow.props")}</h4>
          {selNodeObj ? (
            <NodeProps node={selNodeObj} others={def.nodes!} onEdit={editNode} onAddEdge={addEdge} t={t} />
          ) : selEdgeObj ? (
            <EdgeProps edge={selEdgeObj} onEdit={editEdge} t={t} />
          ) : (
            <p className="text-muted-foreground">{t("workflow.selectHint2")}</p>
          )}
        </div>
      </div>

      <div className="space-y-1">
        <h4 className="text-xs font-semibold">YAML</h4>
        <Textarea
          value={yamlText}
          onChange={(e) => onYamlChange(e.target.value)}
          rows={10}
          className="font-mono text-xs"
        />
      </div>
    </section>
  );
}

function NodeProps({
  node,
  others,
  onEdit,
  onAddEdge,
  t,
}: {
  node: AnyNode;
  others: AnyNode[];
  onEdit: (p: keyof AnyNode, v: string) => void;
  onAddEdge: (target: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  const [target, setTarget] = React.useState("");
  return (
    <div className="space-y-1.5">
      <Field label={t("workflow.nodeName")} value={node.name} onChange={(v) => onEdit("name", v)} />
      <label className="block">
        {t("workflow.nodeKind")}
        <select
          className="ml-1 border rounded px-1 bg-background"
          value={node.kind}
          onChange={(e) => onEdit("kind", e.target.value)}
        >
          <option value="tool">tool</option>
          <option value="llm">llm</option>
        </select>
      </label>
      {node.kind === "tool" && (
        <Field label={t("workflow.tool")} value={node.tool || ""} onChange={(v) => onEdit("tool", v)} />
      )}
      {node.kind === "llm" && (
        <Field label={t("workflow.prompt")} value={node.prompt || ""} onChange={(v) => onEdit("prompt", v)} multiline />
      )}
      <Field
        label={t("workflow.sideEffect")}
        value={node.side_effect || ""}
        onChange={(v) => onEdit("side_effect", v)}
      />
      <div>
        <label className="block">{t("workflow.addEdge")}</label>
        <select
          className="border rounded px-1 bg-background w-full"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
        >
          <option value="">—</option>
          {others
            .filter((o) => o.name !== node.name)
            .map((o) => (
              <option key={o.name} value={o.name}>
                {o.name}
              </option>
            ))}
        </select>
        <Button size="sm" variant="outline" className="mt-1" disabled={!target} onClick={() => onAddEdge(target)}>
          →
        </Button>
      </div>
    </div>
  );
}

function EdgeProps({
  edge,
  onEdit,
  t,
}: {
  edge: AnyEdge;
  onEdit: (p: keyof AnyEdge, v: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  return (
    <div className="space-y-1.5">
      <p className="text-muted-foreground">
        {edge.from} → {edge.to}
      </p>
      <Field
        label={t("workflow.edgeWhen")}
        value={edge.when || ""}
        onChange={(v) => onEdit("when", v)}
        placeholder='${score} > 0.8 / default / llm_route'
      />
      <Field label={t("workflow.edgeDesc")} value={edge.desc || ""} onChange={(v) => onEdit("desc", v)} />
    </div>
  );
}

function Field({
  label,
  value,
  onChange,
  placeholder,
  multiline,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  multiline?: boolean;
}) {
  return (
    <label className="block">
      <span className="text-muted-foreground">{label}</span>
      {multiline ? (
        <Textarea value={value} onChange={(e) => onChange(e.target.value)} rows={2} className="font-mono text-xs" />
      ) : (
        <input
          className="border rounded px-1 bg-background w-full font-mono text-xs"
          value={value}
          placeholder={placeholder}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </label>
  );
}
