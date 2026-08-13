"use client";

import * as React from "react";
import { useEffect, useRef, useState } from "react";
import { load as yLoad, dump as yDump } from "js-yaml";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { Skeleton } from "@/components/ui/skeleton";
import { Plus, Trash2, Save } from "lucide-react";
import {
  getWorkflowYAML,
  listAgentRegisteredTools,
  saveWorkflow,
  type AgentRegisteredTool,
} from "@/lib/api";
import { useLocale, useT } from "@/lib/i18n";
import { BUILTIN_TOOL_ZH } from "@/lib/workflow-tools-zh";

// WorkflowEditor (ticket 09, n8n-style). The model is the raw YAML object so
// unknown fields pass through (decision 8). Canvas = vis-network; double-click
// empty canvas adds a node. Tool nodes pick the tool from the agent's live
// registry (dropdown, not typed blind), and the parameter form is generated
// from that tool's JSON schema — the standard surface every tool (builtin /
// MCP / plugin) exposes itself through. Edges pick a branch kind (plain /
// default / llm_route / expression) from a dropdown.

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

const NODE_COLORS: Record<string, string> = { llm: "#6366f1", tool: "#10b981" };

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyNetwork = { destroy: () => void; on: (e: string, cb: (p: any) => void) => void };

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
  const [def, setDef] = useState<AnyDef | null>(null);
  const [yamlText, setYamlText] = useState("");
  const [selNode, setSelNode] = useState<string | null>(null);
  const [selEdge, setSelEdge] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [tools, setTools] = useState<AgentRegisteredTool[]>([]);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const networkRef = useRef<AnyNetwork | null>(null);

  useEffect(() => {
    let aborted = false;
    setLoading(true);
    setMsg(null);
    Promise.all([
      getWorkflowYAML(agentId, wfID),
      listAgentRegisteredTools(agentId)
        .then((x) => (x || []) as AgentRegisteredTool[])
        .catch(() => [] as AgentRegisteredTool[]),
    ]).then(([y, tl]) => {
      if (aborted) return;
      setYamlText(y);
      setTools(tl);
      try {
        const p = (yLoad(y) || {}) as AnyDef;
        if (!p.nodes) p.nodes = [];
        if (!p.edges) p.edges = [];
        setDef(p);
      } catch {
        setDef({ nodes: [], edges: [] });
      }
      setLoading(false);
    });
    return () => {
      aborted = true;
    };
  }, [agentId, wfID]);

  useEffect(() => {
    if (!def || !containerRef.current) return;
    let cancelled = false;
    Promise.all([import("vis-network/standalone"), import("vis-data/standalone")]).then(
      ([nw, ds]) => {
        if (cancelled || !containerRef.current) return;
        networkRef.current?.destroy();
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        const DataSet = (ds as any).DataSet;
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        const Network = (nw as any).Network;
        const nodes = new DataSet(
          def.nodes!.map((n) => ({
            id: n.name,
            label: `${n.name}\n(${n.kind})`,
            color: { background: NODE_COLORS[n.kind] || "#6b7280", border: "#374151" },
          })),
        );
        const edges = new DataSet(
          def.edges!.map((e, i) => ({ id: i, from: e.from, to: e.to, label: e.when || "" })),
        );
        const network: AnyNetwork = new Network(
          containerRef.current,
          { nodes, edges },
          {
            nodes: { shape: "box", margin: 12, font: { size: 13 } },
            edges: { arrows: "to", font: { size: 11, align: "middle" } },
            interaction: { hover: true },
          },
        );
        network.on("click", (p) => {
          if (p.nodes.length > 0) {
            setSelNode(p.nodes[0]);
            setSelEdge(null);
          } else if (p.edges.length > 0) {
            setSelEdge(p.edges[0]);
            setSelNode(null);
          } else {
            setSelNode(null);
            setSelEdge(null);
          }
        });
        network.on("doubleClick", (p) => {
          if (p.nodes.length === 0 && p.edges.length === 0) {
            const name = `node_${Date.now().toString(36).slice(-4)}`;
            mutate((d) => {
              d.nodes!.push({ name, kind: "tool" });
            });
            setSelNode(name);
          }
        });
        networkRef.current = network;
      },
    );
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [def]);

  const dump = (d: AnyDef) => {
    try {
      return yDump(d, { lineWidth: -1, noRefs: true });
    } catch {
      return "";
    }
  };
  const mutate = (fn: (d: AnyDef) => void) => {
    setDef((cur) => {
      if (!cur) return cur;
      const next: AnyDef = JSON.parse(JSON.stringify(cur));
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
  const editNode = (prop: string, value: unknown) => {
    if (!selNode) return;
    const n = selNode;
    mutate((d) => {
      const x = d.nodes!.find((y) => y.name === n);
      if (x) {
        if (value === undefined || value === "") delete (x as Record<string, unknown>)[prop];
        else (x as Record<string, unknown>)[prop] = value;
      }
    });
  };
  const editEdge = (prop: string, value: string) => {
    if (selEdge == null) return;
    const i = selEdge;
    mutate((d) => {
      const e = d.edges![i];
      if (!e) return;
      if (value === "") delete (e as Record<string, unknown>)[prop];
      else (e as Record<string, unknown>)[prop] = value;
    });
  };
  const onYamlChange = (v: string) => {
    setYamlText(v);
    try {
      const p = (yLoad(v) || {}) as AnyDef;
      if (p && Array.isArray(p.nodes)) {
        setDef({ ...p, nodes: p.nodes || [], edges: p.edges || [] });
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
  const selTool = selNodeObj?.tool ? tools.find((x) => x.name === selNodeObj.tool) : undefined;

  return (
    <section className="space-y-3">
      <details className="border rounded p-2 text-xs">
        <summary className="font-semibold cursor-pointer">{t("workflow.flowProps")}</summary>
        <div className="mt-2 space-y-2">
          <Field
            label={t("workflow.description")}
            value={def.description || ""}
            onChange={(v) => mutate((d) => { if (v) d.description = v; else delete d.description; })}
          />
          <Field
            label={t("workflow.concurrency")}
            value={def.concurrency || ""}
            onChange={(v) => mutate((d) => { if (v) d.concurrency = v; else delete d.concurrency; })}
            placeholder="allow / serial / cancel_previous"
          />
          <div>
            <span className="text-muted-foreground">{t("workflow.inputSchema")}</span>
            <Textarea
              rows={4}
              className="font-mono text-xs mt-1"
              value={def.input?.schema ? JSON.stringify(def.input.schema, null, 2) : ""}
              onChange={(e) => {
                let s: Record<string, unknown> | undefined;
                try {
                  s = e.target.value ? JSON.parse(e.target.value) : undefined;
                } catch {
                  return;
                }
                mutate((d) => {
                  if (s) d.input = { schema: s };
                  else delete d.input;
                });
              }}
            />
          </div>
        </div>
      </details>

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
        <div ref={containerRef} className="h-80 flex-1 rounded border bg-muted/30" />
        <div className="w-72 shrink-0 overflow-y-auto max-h-80 space-y-2 text-xs">
          <h4 className="font-semibold">{t("workflow.props")}</h4>
          {selNodeObj ? (
            <NodeProps
              node={selNodeObj}
              others={def.nodes!}
              tools={tools}
              selTool={selTool}
              onEdit={editNode}
              onAddEdge={addEdge}
              t={t}
            />
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

// SchemaForm renders a typed field per property of an OpenAI-style JSON schema.
// It's the standard surface every tool exposes itself through (registry
// ToolInfo.Parameters), so the editor needs no tool-specific code: a builtin
// get_time, an MCP tool, and a plugin tool all produce the same kind of form.
function SchemaForm({
  schema,
  values,
  onChange,
}: {
  schema?: Record<string, unknown> | null;
  values: Record<string, unknown>;
  onChange: (v: Record<string, unknown>) => void;
}) {
  const props = (schema?.properties || {}) as Record<string, { type?: string; description?: string; enum?: string[] }>;
  const required = new Set((schema?.required || []) as string[]);
  const keys = Object.keys(props);
  if (keys.length === 0) return null;
  return (
    <div className="space-y-1.5">
      <span className="text-muted-foreground font-medium">
        {`参数（可用 \${input.x} / \${node.y} 引用）`}
      </span>
      {keys.map((k) => {
        const p = props[k];
        const v = values[k];
        const isComplex = p.type === "object" || p.type === "array";
        return (
          <label key={k} className="block">
            <span className="text-muted-foreground">
              {k}
              {required.has(k) ? "*" : ""} <span className="opacity-60">({p.type})</span>
            </span>
            {p.enum ? (
              <select
                className="border rounded px-1 bg-background w-full text-xs"
                value={typeof v === "string" ? v : ""}
                onChange={(e) => onChange({ ...values, [k]: e.target.value })}
              >
                <option value="">—</option>
                {p.enum.map((o) => (
                  <option key={o} value={o}>{o}</option>
                ))}
              </select>
            ) : isComplex ? (
              <Textarea
                rows={2}
                className="font-mono text-xs"
                value={typeof v === "string" ? v : JSON.stringify(v ?? "")}
                placeholder={p.description || "${...} JSON"}
                onChange={(e) => onChange({ ...values, [k]: e.target.value })}
              />
            ) : (
              <input
                className="border rounded px-1 bg-background w-full font-mono text-xs"
                value={typeof v === "string" || typeof v === "number" ? String(v) : ""}
                placeholder={p.description || "${...}"}
                onChange={(e) => onChange({ ...values, [k]: e.target.value })}
              />
            )}
          </label>
        );
      })}
    </div>
  );
}

function NodeProps({
  node,
  others,
  tools,
  selTool,
  onEdit,
  onAddEdge,
  t,
}: {
  node: AnyNode;
  others: AnyNode[];
  tools: AgentRegisteredTool[];
  selTool: AgentRegisteredTool | undefined;
  onEdit: (p: string, v: unknown) => void;
  onAddEdge: (target: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  const [target, setTarget] = React.useState("");
  const locale = useLocale().locale;
  const inputVals = (node.input || {}) as Record<string, unknown>;
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
        <>
          <label className="block">
            {t("workflow.tool")}
            <select
              className="border rounded px-1 bg-background w-full"
              value={node.tool || ""}
              onChange={(e) => onEdit("tool", e.target.value)}
            >
              <option value="">— 选择工具 —</option>
              {tools.map((x) => (
                <option key={x.name} value={x.name}>
                  {x.name} ({x.source})
                </option>
              ))}
            </select>
          </label>
          {node.tool && selTool && (
            <p className="text-muted-foreground italic">
              {locale === "zh-CN" && selTool.source === "builtin" && BUILTIN_TOOL_ZH[selTool.name]
                ? BUILTIN_TOOL_ZH[selTool.name]
                : selTool.description}
            </p>
          )}
          {selTool?.parameters ? (
            <SchemaForm
              schema={selTool.parameters}
              values={inputVals}
              onChange={(v) => onEdit("input", v)}
            />
          ) : (
            <div>
              <span className="text-muted-foreground">{t("workflow.toolInput")}</span>
              <Textarea
                rows={3}
                className="font-mono text-xs mt-1"
                value={node.input ? JSON.stringify(node.input, null, 2) : ""}
                onChange={(e) => {
                  let v: Record<string, unknown> | undefined;
                  try {
                    v = e.target.value ? JSON.parse(e.target.value) : undefined;
                  } catch {
                    return;
                  }
                  onEdit("input", v);
                }}
              />
            </div>
          )}
        </>
      )}
      {node.kind === "llm" && (
        <Field label={t("workflow.prompt")} value={node.prompt || ""} onChange={(v) => onEdit("prompt", v)} multiline />
      )}
      <Field
        label={t("workflow.sideEffect")}
        value={node.side_effect || ""}
        onChange={(v) => onEdit("side_effect", v)}
      />
      <details>
        <summary className="text-muted-foreground cursor-pointer">{t("workflow.outputSchema")}</summary>
        <Textarea
          rows={3}
          className="font-mono text-xs mt-1"
          value={node.output ? JSON.stringify(node.output, null, 2) : ""}
          onChange={(e) => {
            let v: Record<string, unknown> | undefined;
            try {
              v = e.target.value ? JSON.parse(e.target.value) : undefined;
            } catch {
              return;
            }
            onEdit("output", v);
          }}
        />
      </details>
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
              <option key={o.name} value={o.name}>{o.name}</option>
            ))}
        </select>
        <Button size="sm" variant="outline" className="mt-1" disabled={!target} onClick={() => onAddEdge(target)}>
          → {target || "…"}
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
  onEdit: (p: string, v: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  const when = edge.when || "";
  const isExpr = when !== "" && when !== "default" && when !== "llm_route";
  return (
    <div className="space-y-1.5">
      <p className="text-muted-foreground">
        {edge.from} → {edge.to}
      </p>
      <label className="block">
        {t("workflow.edgeWhen")}
        <select
          className="border rounded px-1 bg-background w-full"
          value={isExpr ? "__expr" : when}
          onChange={(e) => {
            const v = e.target.value;
            if (v === "__expr") onEdit("when", "${score} > 0.8");
            else onEdit("when", v);
          }}
        >
          <option value="">plain（顺序）</option>
          <option value="default">default（兜底）</option>
          <option value="llm_route">llm_route（LLM 选）</option>
          <option value="__expr">表达式…</option>
        </select>
      </label>
      {isExpr && (
        <Field
          label={t("workflow.edgeExpr")}
          value={when}
          onChange={(v) => onEdit("when", v)}
          placeholder="${node.score} > 0.8"
        />
      )}
      {when === "llm_route" && (
        <Field label={t("workflow.edgeDesc")} value={edge.desc || ""} onChange={(v) => onEdit("desc", v)} />
      )}
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
        <Textarea
          value={value}
          onChange={(e) => onChange(e.target.value)}
          rows={3}
          className="font-mono text-xs"
          placeholder={placeholder}
        />
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
