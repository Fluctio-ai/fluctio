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
  output?: Record<string, unknown>;
  nodes?: AnyNode[];
  edges?: AnyEdge[];
  [k: string]: unknown;
};
type AnyNode = {
  name: string;
  kind: string;
  tool?: string;
  prompt?: string;
  code?: string;
  lang?: string;
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

const NODE_COLORS: Record<string, string> = { llm: "#6366f1", tool: "#10b981", code: "#f59e0b" };
const LANGS = ["python", "sh"];
const TYPES = ["string", "number", "integer", "boolean", "object", "array"];
const OPS = [">", "<", ">=", "<=", "==", "!="];

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

  const addNode = (kind: "tool" | "llm" | "code") => {
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
  const refOptions = buildRefOptions(def);

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
            <InputSchemaEditor
              schema={def.input?.schema as Record<string, unknown> | undefined}
              onChange={(s) => mutate((d) => { if (s) d.input = { schema: s }; else delete d.input; })}
            />
          </div>
          <div>
            <span className="text-muted-foreground">{t("workflow.outputMap")}</span>
            <OutputMapEditor
              output={def.output}
              options={refOptions}
              onChange={(o) => mutate((d) => { if (o) d.output = o; else delete d.output; })}
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
        <Button size="sm" variant="outline" onClick={() => addNode("code")}>
          <Plus className="h-3.5 w-3.5" /> {t("workflow.addCode")}
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
              refOptions={refOptions}
              onEdit={editNode}
              onAddEdge={addEdge}
              t={t}
            />
          ) : selEdgeObj ? (
            <EdgeProps key={selEdge} edge={selEdgeObj} refOptions={refOptions} onEdit={editEdge} t={t} />
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

// --- M1 humanization helpers (MaxKB-style field pickers + structured editors) ---

type RefOption = { ref: string; label: string };

// buildRefOptions lists every field a downstream node may reference: the
// workflow's input fields plus each node's declared output fields. A node with
// no declared output schema exposes a whole-node ${name} entry. This is the
// dropdown source for FieldRef / InsertField, so users pick instead of typing
// ${node.field} by hand.
function buildRefOptions(def: AnyDef): RefOption[] {
  const opts: RefOption[] = [];
  const inputProps = (def.input?.schema?.properties || {}) as Record<string, unknown>;
  for (const k of Object.keys(inputProps)) {
    opts.push({ ref: `\${input.${k}}`, label: `input.${k}` });
  }
  for (const n of def.nodes || []) {
    const out = (n.output || {}) as Record<string, unknown>;
    const keys = Object.keys(out);
    if (keys.length > 0) {
      for (const k of keys) opts.push({ ref: `\${${n.name}.${k}}`, label: `${n.name}.${k}` });
    } else {
      opts.push({ ref: `\${${n.name}}`, label: `${n.name}（整个输出）` });
    }
  }
  return opts;
}

// FieldRef — a value that is (or contains) a reference. The text box accepts
// free-form; the dropdown inserts/replaces with a picked ${node.field}. Used
// for whole-value slots (tool arg, output-map value, condition value).
function FieldRef({ options, value, onChange, placeholder }: {
  options: RefOption[];
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
}) {
  return (
    <div className="flex gap-1">
      <input
        className="border rounded px-1 bg-background flex-1 font-mono text-xs min-w-0"
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
      />
      <select
        className="border rounded px-1 bg-background text-xs shrink-0"
        value=""
        onChange={(e) => { if (e.target.value) onChange(e.target.value); }}
      >
        <option value="">${"{}"}</option>
        {options.map((o) => (
          <option key={o.ref} value={o.ref}>{o.label}</option>
        ))}
      </select>
    </div>
  );
}

// InsertField — a "+ 字段" dropdown whose selection the caller splices into a
// textarea at the cursor (prompt / code bodies where references embed inside
// larger text).
function InsertField({ options, onInsert }: { options: RefOption[]; onInsert: (ref: string) => void }) {
  return (
    <select
      className="border rounded px-1 bg-background text-xs"
      value=""
      onChange={(e) => { if (e.target.value) onInsert(e.target.value); }}
    >
      <option value="">+ 插入字段</option>
      {options.map((o) => (
        <option key={o.ref} value={o.ref}>{o.label}</option>
      ))}
    </select>
  );
}

// KVRows — generic key/value row editor. valueMode="ref" renders values as
// FieldRef (tool input without a schema, output-map entries); "type" renders a
// type dropdown (node output schema declarations).
function KVRows({ obj, options, onChange, valueMode = "ref" }: {
  obj: Record<string, unknown>;
  options?: RefOption[];
  onChange: (v: Record<string, unknown> | undefined) => void;
  valueMode?: "ref" | "type";
}) {
  const entries = Object.entries(obj);
  const commit = (next: Record<string, unknown>) => {
    const cleaned = Object.fromEntries(Object.entries(next).filter(([k]) => k));
    onChange(Object.keys(cleaned).length ? cleaned : undefined);
  };
  const addKey = valueMode === "type" ? `field_${entries.length + 1}` : `key_${entries.length + 1}`;
  return (
    <div className="space-y-1 mt-1">
      {entries.map(([k, v], i) => (
        <div key={i} className="flex gap-1 items-center">
          <input
            className="border rounded px-1 bg-background text-xs w-24 shrink-0"
            value={k}
            onChange={(e) => {
              const next: Record<string, unknown> = { ...obj };
              const val = next[k];
              delete next[k];
              next[e.target.value] = val;
              commit(next);
            }}
          />
          {valueMode === "type" ? (
            <select
              className="border rounded px-1 bg-background text-xs flex-1"
              value={v && typeof v === "object" && "type" in v ? String((v as { type: string }).type) : "string"}
              onChange={(e) => commit({ ...obj, [k]: { type: e.target.value } })}
            >
              {TYPES.map((ty) => <option key={ty} value={ty}>{ty}</option>)}
            </select>
          ) : (
            <FieldRef
              options={options || []}
              value={typeof v === "string" ? v : JSON.stringify(v)}
              onChange={(val) => commit({ ...obj, [k]: val })}
              placeholder="${node.field} 或文本"
            />
          )}
          <button
            className="text-destructive text-xs px-1"
            onClick={() => { const next = { ...obj }; delete next[k]; commit(next); }}
          >✕</button>
        </div>
      ))}
      <Button
        size="sm"
        variant="outline"
        onClick={() => commit({ ...obj, [addKey]: valueMode === "type" ? "string" : "" })}
      >
        <Plus className="h-3 w-3" /> {valueMode === "type" ? "+ 字段" : "+ 项"}
      </Button>
    </div>
  );
}

// InputSchemaEditor — structured editor for the workflow entry input schema
// (field name + type + required checkbox), replacing the raw JSON textarea.
function InputSchemaEditor({ schema, onChange }: {
  schema: Record<string, unknown> | undefined;
  onChange: (s: Record<string, unknown> | undefined) => void;
}) {
  const props = (schema?.properties || {}) as Record<string, { type?: string }>;
  const required = new Set((schema?.required || []) as string[]);
  const entries = Object.entries(props);
  const commit = (nextProps: Record<string, { type?: string }>, nextReq: Set<string>) => {
    const np = Object.fromEntries(Object.entries(nextProps).filter(([, v]) => v));
    if (Object.keys(np).length === 0) { onChange(undefined); return; }
    onChange({
      type: "object",
      properties: np,
      required: Array.from(nextReq).filter((r) => np[r]),
    });
  };
  return (
    <div className="space-y-1 mt-1">
      {entries.map(([name, p], i) => (
        <div key={i} className="flex gap-1 items-center">
          <input
            className="border rounded px-1 bg-background text-xs flex-1 min-w-0"
            value={name}
            onChange={(e) => {
              const nn = e.target.value;
              const next: Record<string, { type?: string }> = { ...props };
              const v = next[name];
              delete next[name];
              next[nn] = v;
              const nr = new Set(required);
              if (nr.has(name)) { nr.delete(name); nr.add(nn); }
              commit(next, nr);
            }}
          />
          <select
            className="border rounded px-1 bg-background text-xs"
            value={p?.type || "string"}
            onChange={(e) => commit({ ...props, [name]: { ...p, type: e.target.value } }, required)}
          >
            {TYPES.map((ty) => <option key={ty} value={ty}>{ty}</option>)}
          </select>
          <input
            type="checkbox"
            checked={required.has(name)}
            title="required"
            onChange={(e) => {
              const nr = new Set(required);
              if (e.target.checked) nr.add(name); else nr.delete(name);
              commit(props, nr);
            }}
          />
          <button
            className="text-destructive text-xs px-1"
            onClick={() => { const next = { ...props }; delete next[name]; const nr = new Set(required); nr.delete(name); commit(next, nr); }}
          >✕</button>
        </div>
      ))}
      <Button
        size="sm"
        variant="outline"
        onClick={() => commit({ ...props, [`field_${entries.length + 1}`]: { type: "string" } }, required)}
      >
        <Plus className="h-3 w-3" /> + 字段
      </Button>
    </div>
  );
}

// OutputMapEditor — structured editor for Definition.Output (key + FieldRef
// value rows), replacing the raw JSON textarea.
function OutputMapEditor({ output, options, onChange }: {
  output: Record<string, unknown> | undefined;
  options: RefOption[];
  onChange: (o: Record<string, unknown> | undefined) => void;
}) {
  const entries = Object.entries(output || {});
  const commit = (next: Record<string, unknown>) => {
    const cleaned = Object.fromEntries(Object.entries(next).filter(([k]) => k));
    onChange(Object.keys(cleaned).length ? cleaned : undefined);
  };
  return (
    <div className="space-y-1 mt-1">
      {entries.map(([k, v], i) => (
        <div key={i} className="flex gap-1 items-center">
          <input
            className="border rounded px-1 bg-background text-xs w-24 shrink-0"
            value={k}
            onChange={(e) => {
              const next: Record<string, unknown> = { ...output };
              const val = next[k];
              delete next[k];
              next[e.target.value] = val;
              commit(next);
            }}
          />
          <FieldRef
            options={options}
            value={typeof v === "string" ? v : JSON.stringify(v)}
            onChange={(val) => commit({ ...output, [k]: val })}
            placeholder="${node.field} 或文本"
          />
          <button
            className="text-destructive text-xs px-1"
            onClick={() => { const next = { ...output }; delete next[k]; commit(next); }}
          >✕</button>
        </div>
      ))}
      <Button
        size="sm"
        variant="outline"
        onClick={() => commit({ ...output, [`key_${entries.length + 1}`]: "" })}
      >
        <Plus className="h-3 w-3" /> + 输出项
      </Button>
    </div>
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
  refOptions,
  onEdit,
  onAddEdge,
  t,
}: {
  node: AnyNode;
  others: AnyNode[];
  tools: AgentRegisteredTool[];
  selTool: AgentRegisteredTool | undefined;
  refOptions: RefOption[];
  onEdit: (p: string, v: unknown) => void;
  onAddEdge: (target: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  const [target, setTarget] = React.useState("");
  const locale = useLocale().locale;
  const promptRef = useRef<HTMLTextAreaElement>(null);
  const codeRef = useRef<HTMLTextAreaElement>(null);
  const insertAt = (ref: string, field: "prompt" | "code", ta: HTMLTextAreaElement | null) => {
    const cur = (node[field] as string) || "";
    if (!ta) { onEdit(field, cur + ref); return; }
    const s = ta.selectionStart ?? cur.length;
    const e = ta.selectionEnd ?? s;
    onEdit(field, cur.slice(0, s) + ref + cur.slice(e));
  };
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
          <option value="code">code</option>
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
              values={(node.input || {}) as Record<string, unknown>}
              onChange={(v) => onEdit("input", v)}
            />
          ) : (
            <div>
              <span className="text-muted-foreground">{t("workflow.toolInput")}</span>
              <KVRows
                obj={(node.input || {}) as Record<string, unknown>}
                options={refOptions}
                onChange={(v) => onEdit("input", v)}
              />
            </div>
          )}
        </>
      )}

      {node.kind === "llm" && (
        <div className="space-y-1">
          <div className="flex items-center justify-between gap-1">
            <span className="text-muted-foreground">{t("workflow.prompt")}</span>
            <InsertField options={refOptions} onInsert={(ref) => insertAt(ref, "prompt", promptRef.current)} />
          </div>
          <Textarea
            ref={promptRef}
            rows={4}
            className="font-mono text-xs"
            value={node.prompt || ""}
            onChange={(e) => onEdit("prompt", e.target.value)}
          />
        </div>
      )}

      {node.kind === "code" && (
        <div className="space-y-1">
          <label className="block">
            {t("workflow.codeLang")}
            <select
              className="ml-1 border rounded px-1 bg-background"
              value={node.lang || "python"}
              onChange={(e) => onEdit("lang", e.target.value)}
            >
              {LANGS.map((l) => <option key={l} value={l}>{l}</option>)}
            </select>
          </label>
          <div className="flex items-center justify-between gap-1">
            <span className="text-muted-foreground">{t("workflow.codeBody")}</span>
            <InsertField options={refOptions} onInsert={(ref) => insertAt(ref, "code", codeRef.current)} />
          </div>
          <Textarea
            ref={codeRef}
            rows={4}
            className="font-mono text-xs"
            value={node.code || ""}
            onChange={(e) => onEdit("code", e.target.value)}
            placeholder="# python/sh 代码；print 结果到 stdout；可用 ${input.x} / ${node.y}"
          />
        </div>
      )}

      <Field
        label={t("workflow.sideEffect")}
        value={node.side_effect || ""}
        placeholder="pure / idempotent / non-idempotent"
        onChange={(v) => onEdit("side_effect", v)}
      />

      <details>
        <summary className="text-muted-foreground cursor-pointer">{t("workflow.outputSchema")}</summary>
        <KVRows
          obj={(node.output || {}) as Record<string, unknown>}
          valueMode="type"
          onChange={(v) => onEdit("output", v)}
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

type CondRow = { field: string; op: string; value: string };

// parseWhen reads a `when` expression back into structured rows for the editor.
// Returns null for plain/default/llm_route or anything that isn't a flat
// && / || combination of ${ref} OP literal terms — the editor then falls back
// to the raw textarea so an advanced expression is never destroyed.
function parseWhen(when: string): { combine: "&&" | "||"; rows: CondRow[] } | null {
  const w = when.trim();
  if (!w || w === "default" || w === "llm_route") return null;
  let combine: "&&" | "||" = "&&";
  let parts: string[];
  if (w.includes("&&")) { combine = "&&"; parts = w.split("&&"); }
  else if (w.includes("||")) { combine = "||"; parts = w.split("||"); }
  else parts = [w];
  const rows: CondRow[] = [];
  for (const p of parts) {
    const m = p.trim().match(/^\$\{([^}]+)\}\s*(>=|<=|==|!=|>|<)\s*(.+)$/);
    if (!m) return null;
    rows.push({ field: m[1], op: m[2], value: m[3].trim() });
  }
  return { combine, rows };
}

function rowsToWhen(combine: "&&" | "||", rows: CondRow[]): string {
  return rows.map((r) => `\${${r.field}} ${r.op} ${r.value}`).join(combine === "&&" ? " && " : " || ");
}

function EdgeProps({
  edge,
  refOptions,
  onEdit,
  t,
}: {
  edge: AnyEdge;
  refOptions: RefOption[];
  onEdit: (p: string, v: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  const when = edge.when || "";
  const isExpr = when !== "" && when !== "default" && when !== "llm_route";
  const parsed = isExpr ? parseWhen(when) : null;
  const [combine, setCombine] = React.useState<"&&" | "||">(parsed?.combine || "&&");
  const [rows, setRows] = React.useState<CondRow[]>(parsed?.rows || [{ field: "", op: "==", value: "" }]);
  const update = (c: "&&" | "||", rs: CondRow[]) => {
    setCombine(c);
    setRows(rs);
    onEdit("when", rowsToWhen(c, rs));
  };
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
            if (v === "__expr") update(combine, rows.length ? rows : [{ field: "", op: "==", value: "" }]);
            else onEdit("when", v);
          }}
        >
          <option value="">plain（顺序）</option>
          <option value="default">default（兜底）</option>
          <option value="llm_route">llm_route（LLM 选）</option>
          <option value="__expr">条件…</option>
        </select>
      </label>
      {isExpr && (
        <div className="space-y-1.5 border rounded p-1.5 bg-muted/30">
          {parsed ? (
            <>
              <div className="flex items-center gap-1">
                <span className="text-xs text-muted-foreground shrink-0">匹配</span>
                <select
                  className="border rounded px-1 bg-background text-xs"
                  value={combine}
                  onChange={(e) => update(e.target.value as "&&" | "||", rows)}
                >
                  <option value="&&">全部满足 (AND)</option>
                  <option value="||">任一满足 (OR)</option>
                </select>
              </div>
              {rows.map((r, i) => (
                <div key={i} className="space-y-1 border-t pt-1">
                  <div className="flex gap-1 items-center">
                    <select
                      className="border rounded px-1 bg-background text-xs flex-1 min-w-0"
                      value={r.field ? `\${${r.field}}` : ""}
                      onChange={(e) => {
                        const m = e.target.value.match(/^\$\{([^}]+)\}$/);
                        update(combine, rows.map((x, j) => j === i ? { ...x, field: m ? m[1] : x.field } : x));
                      }}
                    >
                      <option value="">— 选字段 —</option>
                      {refOptions.map((o) => <option key={o.ref} value={o.ref}>{o.label}</option>)}
                    </select>
                    <select
                      className="border rounded px-1 bg-background text-xs"
                      value={r.op}
                      onChange={(e) => update(combine, rows.map((x, j) => j === i ? { ...x, op: e.target.value } : x))}
                    >
                      {OPS.map((o) => <option key={o} value={o}>{o}</option>)}
                    </select>
                    <button
                      className="text-destructive text-xs px-1"
                      disabled={rows.length <= 1}
                      onClick={() => update(combine, rows.filter((_, j) => j !== i))}
                    >✕</button>
                  </div>
                  <input
                    className="border rounded px-1 bg-background text-xs w-full"
                    value={r.value}
                    placeholder="值（数字或字符串）"
                    onChange={(e) => update(combine, rows.map((x, j) => j === i ? { ...x, value: e.target.value } : x))}
                  />
                </div>
              ))}
              <Button size="sm" variant="outline" className="w-full" onClick={() => update(combine, [...rows, { field: "", op: "==", value: "" }])}>
                <Plus className="h-3 w-3" /> + 条件
              </Button>
            </>
          ) : (
            <Field
              label={t("workflow.edgeExpr")}
              value={when}
              onChange={(v) => onEdit("when", v)}
              placeholder="${node.score} > 0.8"
            />
          )}
        </div>
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
