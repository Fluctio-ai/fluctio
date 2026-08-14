"use client";

import * as React from "react";
import { useEffect, useRef, useState } from "react";
import { load as yLoad, dump as yDump } from "js-yaml";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { Skeleton } from "@/components/ui/skeleton";
import { Plus, Trash2, Save } from "lucide-react";
import {
  getChatSessions,
  getWorkflowYAML,
  listAgentRegisteredTools,
  listKBSources,
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
  title?: string;
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

const NODE_COLORS: Record<string, string> = { llm: "#6366f1", tool: "#10b981", code: "#f59e0b", reply: "#06b6d4", question_rewrite: "#a855f7", http: "#f97316", kb_search: "#14b8a6", set: "#64748b", condition: "#ef4444" };
const LANGS = ["python", "sh"];
const TYPES = ["string", "number", "integer", "boolean", "object", "array"];
const OPS = [">", "<", ">=", "<=", "==", "!=", "contain", "not_contain"];

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyNetwork = { destroy: () => void; on: (e: string, cb: (p: any) => void) => void; redraw?: () => void; fit?: () => void };

export function WorkflowEditor({
  agentId,
  wfID,
  onSaved,
  onDelete,
}: {
  agentId: string;
  wfID: string;
  onSaved: () => void;
  onDelete: () => void;
}) {
  const t = useT();
  const [def, setDef] = useState<AnyDef | null>(null);
  const [yamlText, setYamlText] = useState("");
  const [selNode, setSelNode] = useState<string | null>(null);
  const [selEdge, setSelEdge] = useState<number | null>(null);
  const [tab, setTab] = useState<"basic" | "visual" | "yaml">("visual");
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
        // Center the graph in the viewport instead of leaving it stuck in the
        // top-left corner on first render.
        network.fit?.();
        networkRef.current = network;
      },
    );
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [def]);

  // Switching back to the visual tab: the canvas container was display:none on
  // the other tabs, so vis-network drew it at size 0. Redraw now that it's
  // visible so it picks up the real size.
  useEffect(() => {
    if (tab === "visual" && networkRef.current) {
      networkRef.current.redraw?.();
    }
  }, [tab]);

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

  const addNode = (kind: "tool" | "llm" | "code" | "reply" | "question_rewrite" | "http" | "kb_search" | "set" | "condition" | "form") => {
    const name = `${kind}_${Date.now().toString(36).slice(-4)}`;
    mutate((d) => {
      if (kind === "question_rewrite") {
        // query-reformulation node: prefill a default rewrite instruction + a
        // {query} output schema so downstream nodes can pick ${node.query} from
        // the FieldRef dropdown out of the box.
        d.nodes!.push({ name, kind, prompt: "请将以下问题改写为适合知识库检索的简洁查询：${input.question}", output: { query: { type: "string" } } });
      } else if (kind === "http") {
        // http node: prefill a GET scaffold; url is required (validated).
        d.nodes!.push({ name, kind, input: { method: "GET", url: "" } });
      } else if (kind === "kb_search") {
        // kb_search wraps the builtin knowledgebase_search; prefill query bound
        // to the workflow input so a newly-added node runs out of the box.
        d.nodes!.push({ name, kind, input: { query: "${input.question}" } });
      } else if (kind === "form") {
        // M6 form node: prefill an approve/notes schema so a newly-added node
        // demonstrates both a required boolean and an optional text field.
        d.nodes!.push({ name, kind, input: { properties: { approve: { type: "boolean" }, notes: { type: "string" } }, required: ["approve"] } });
      } else {
        d.nodes!.push({ name, kind });
      }
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
      <div className="flex items-center gap-2 flex-wrap">
        <div className="flex gap-1 border-b text-xs">
          {([["basic", "workflow.tabBasic"], ["visual", "workflow.tabVisual"], ["yaml", "workflow.tabYaml"]] as const).map(([k, key]) => (
            <button
              key={k}
              type="button"
              onClick={() => setTab(k)}
              className={"px-2 py-1 border-b-2 " + (tab === k ? "border-primary font-semibold" : "border-transparent text-muted-foreground")}
            >
              {t(key)}
            </button>
          ))}
        </div>
        <div className="flex-1" />
        <Button size="sm" onClick={save} disabled={saving}>
          <Save className="h-3.5 w-3.5" /> {saving ? t("workflow.saving") : t("workflow.save")}
        </Button>
        <Button size="sm" variant="outline" onClick={onDelete}>
          <Trash2 className="h-3.5 w-3.5" /> {t("workflow.delete")}
        </Button>
        {msg && (
          <span className={msg.ok ? "text-xs text-green-600" : "text-xs text-destructive"}>
            {msg.text}
          </span>
        )}
      </div>

      {tab === "basic" && (
        <div className="space-y-3">
          <h3 className="font-semibold">{t("workflow.flowProps")}</h3>
            <Field
              label={t("workflow.workflowTitle")}
              value={def.title || wfID}
              onChange={(v) => mutate((d) => { if (v && v !== wfID) d.title = v; else delete d.title; })}
            />
            <Field
              label={t("workflow.description")}
              value={def.description || ""}
              onChange={(v) => mutate((d) => { if (v) d.description = v; else delete d.description; })}
            />
            <label className="block">
              {t("workflow.concurrency")}
              <select
                className="ml-1 border rounded-lg px-2.5 py-1 h-9 bg-transparent w-full text-sm"
                value={def.concurrency || ""}
                onChange={(e) => mutate((d) => { if (e.target.value) d.concurrency = e.target.value; else delete d.concurrency; })}
              >
                <option value="">{t("workflow.concurrencyAllow")}</option>
                <option value="serial">{t("workflow.concurrencySerial")}</option>
                <option value="cancel_previous">{t("workflow.concurrencyCancel")}</option>
              </select>
            </label>
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
      )}

      <div style={{ display: tab === "visual" ? undefined : "none" }} className="space-y-3">
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
            <Button size="sm" variant="outline" onClick={() => addNode("reply")}>
              <Plus className="h-3.5 w-3.5" /> {t("workflow.addReply")}
            </Button>
            <Button size="sm" variant="outline" onClick={() => addNode("question_rewrite")}>
              <Plus className="h-3.5 w-3.5" /> {t("workflow.addRewrite")}
            </Button>
            <Button size="sm" variant="outline" onClick={() => addNode("http")}>
              <Plus className="h-3.5 w-3.5" /> {t("workflow.addHTTP")}
            </Button>
            <Button size="sm" variant="outline" onClick={() => addNode("kb_search")}>
              <Plus className="h-3.5 w-3.5" /> {t("workflow.addKBSearch")}
            </Button>
            <Button size="sm" variant="outline" onClick={() => addNode("set")}>
              <Plus className="h-3.5 w-3.5" /> {t("workflow.addSet")}
            </Button>
            <Button size="sm" variant="outline" onClick={() => addNode("condition")}>
              <Plus className="h-3.5 w-3.5" /> {t("workflow.addCondition")}
            </Button>
            <Button size="sm" variant="outline" onClick={() => addNode("form")}>
              <Plus className="h-3.5 w-3.5" /> {t("workflow.addForm")}
            </Button>
            <Button size="sm" variant="outline" onClick={deleteSelected} disabled={!selNode && selEdge == null}>
              <Trash2 className="h-3.5 w-3.5" /> {t("workflow.deleteSel")}
            </Button>
          </div>
          <div className="flex gap-3">
            <div ref={containerRef} className="h-[480px] flex-1 rounded border bg-muted/30" />
            <div className="w-72 shrink-0 overflow-y-auto max-h-[480px] space-y-2 text-xs">
              <h4 className="font-semibold">{t("workflow.props")}</h4>
              {selNodeObj ? (
                <NodeProps
                  node={selNodeObj}
                  others={def.nodes!}
                  tools={tools}
                  selTool={selTool}
                  refOptions={refOptions}
                  agentId={agentId}
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
        </div>

      <div style={{ display: tab === "yaml" ? undefined : "none" }}>
          <h4 className="text-xs font-semibold">YAML</h4>
          <Textarea
            value={yamlText}
            onChange={(e) => onYamlChange(e.target.value)}
            rows={28}
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
        className="border rounded-lg px-2.5 py-1 h-9 bg-transparent flex-1 font-mono text-sm min-w-0"
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
      />
      <select
        className="border rounded-lg px-2.5 py-1 h-9 bg-background text-sm shrink-0"
        value=""
        onChange={(e) => { if (e.target.value) onChange(e.target.value); }}
      >
        <option value="" className="bg-background text-foreground">${"{}"}</option>
        {options.map((o) => (
          <option key={o.ref} value={o.ref} className="bg-background text-foreground">{o.label}</option>
        ))}
      </select>
    </div>
  );
}

// InsertField — a "+ 字段" dropdown whose selection the caller splices into a
// textarea at the cursor (prompt / code bodies where references embed inside
// larger text).
function InsertField({ options, onInsert }: { options: RefOption[]; onInsert: (ref: string) => void }) {
  const t = useT();
  return (
    <select
      className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm"
      value=""
      onChange={(e) => { if (e.target.value) onInsert(e.target.value); }}
    >
      <option value="">{t("workflow.insertField")}</option>
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
  const t = useT();
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
            className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm w-24 shrink-0"
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
              className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm flex-1"
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
        <Plus className="h-3 w-3" /> {valueMode === "type" ? t("workflow.addField") : t("workflow.addItem")}
      </Button>
    </div>
  );
}

// InputSchemaEditor — structured editor for the workflow entry input schema
// (field name + type + required + description), replacing the raw JSON
// textarea. The description flows into the tool schema the LLM sees when the
// workflow is registered as a tool, so the model knows what each field is for.
function InputSchemaEditor({ schema, onChange }: {
  schema: Record<string, unknown> | undefined;
  onChange: (s: Record<string, unknown> | undefined) => void;
}) {
  const t = useT();
  type Prop = { type?: string; description?: string };
  const props = (schema?.properties || {}) as Record<string, Prop>;
  const required = new Set((schema?.required || []) as string[]);
  const entries = Object.entries(props);
  const commit = (nextProps: Record<string, Prop>, nextReq: Set<string>) => {
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
            type="checkbox"
            checked={required.has(name)}
            title="required"
            onChange={(e) => {
              const nr = new Set(required);
              if (e.target.checked) nr.add(name); else nr.delete(name);
              commit(props, nr);
            }}
          />
          <input
            className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm w-20 shrink-0"
            value={name}
            onChange={(e) => {
              const nn = e.target.value;
              const next: Record<string, Prop> = { ...props };
              const v = next[name];
              delete next[name];
              next[nn] = v;
              const nr = new Set(required);
              if (nr.has(name)) { nr.delete(name); nr.add(nn); }
              commit(next, nr);
            }}
          />
          <select
            className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm shrink-0"
            value={p?.type || "string"}
            onChange={(e) => commit({ ...props, [name]: { ...p, type: e.target.value } }, required)}
          >
            {TYPES.map((ty) => <option key={ty} value={ty}>{ty}</option>)}
          </select>
          <input
            className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm flex-1 min-w-0"
            value={p?.description || ""}
            placeholder={t("workflow.fieldDescPh")}
            onChange={(e) => commit({ ...props, [name]: { ...p, description: e.target.value } }, required)}
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
        <Plus className="h-3 w-3" /> {t("workflow.addField")}
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
  const t = useT();
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
            className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm w-24 shrink-0"
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
        <Plus className="h-3 w-3" /> {t("workflow.addOutput")}
      </Button>
    </div>
  );
}

// SessionPicker — a combobox for the message tool's channel + chat_id: type a
// keyword and matching chat sessions (title / preview / chatId) drop down live,
// so picking "AI 新闻" fills BOTH fields. A flat <select> doesn't scale as chats
// accumulate, so this filters as you type.
function SessionPicker({ agentId, channel, chatId, account, onChange }: {
  agentId: string;
  channel: string;
  chatId: string;
  account?: string;
  onChange: (channel: string, chatId: string, account: string) => void;
}) {
  type Sess = { channel?: string; chatId?: string; title?: string; preview?: string; accountId?: string };
  const t = useT();
  const [sessions, setSessions] = useState<Sess[]>([]);
  const [q, setQ] = useState("");
  const [open, setOpen] = useState(false);
  useEffect(() => {
    let off = false;
    getChatSessions(agentId)
      .then((s) => { if (!off) setSessions(s as Sess[]); })
      .catch(() => {});
    return () => { off = true; };
  }, [agentId]);
  const ql = q.trim().toLowerCase();
  const filtered = sessions.filter((s) =>
    !ql ||
    (s.title || "").toLowerCase().includes(ql) ||
    (s.preview || "").toLowerCase().includes(ql) ||
    (s.chatId || "").toLowerCase().includes(ql)
  );
  const cur = sessions.find((s) => s.chatId === chatId && s.channel === channel);
  const display = q || (cur ? `${cur.title || cur.preview || cur.chatId} · ${cur.channel}` : "");
  return (
    <div className="relative">
      <input
        className="border rounded-lg px-2.5 py-1 h-9 bg-transparent w-full text-sm"
        placeholder={t("workflow.sessionSearchPh")}
        value={display}
        onChange={(e) => { setQ(e.target.value); setOpen(true); }}
        onFocus={() => setOpen(true)}
        onBlur={() => setTimeout(() => setOpen(false), 150)}
      />
      {open && (
        <div className="absolute z-10 left-0 right-0 mt-0.5 max-h-48 overflow-auto bg-background border rounded shadow text-xs">
          {filtered.length === 0 && <p className="px-1 py-1 text-muted-foreground">{t("workflow.noMatch")}</p>}
          {filtered.map((s) => (
            <button
              type="button"
              key={s.chatId}
              onMouseDown={(e) => {
                e.preventDefault();
                onChange(s.channel || "", s.chatId || "", s.accountId || "");
                setQ("");
                setOpen(false);
              }}
              className="block w-full text-left px-1 py-0.5 hover:bg-muted"
            >
              <div>{s.title || s.preview || s.chatId}</div>
              <div className="text-muted-foreground text-[10px]">{s.channel} · {(s.chatId || "").slice(0, 16)}</div>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// ArticlePicker — a combobox over the agent's KB sources (articles) for the
// knowledgebase_* tools' source_id (single) / source_ids (array). Type a title
// to filter, click to pick; multi-pick toggles membership and shows chips.
// Saves pasting opaque uuids by hand.
function ArticlePicker({ agentId, multiple, value, onChange }: {
  agentId: string;
  multiple: boolean;
  value: string | string[];
  onChange: (v: string | string[]) => void;
}) {
  const [srcs, setSrcs] = useState<{ id: string; title?: string }[]>([]);
  const t = useT();
  const [q, setQ] = useState("");
  const [open, setOpen] = useState(false);
  useEffect(() => {
    let off = false;
    listKBSources(agentId)
      .then((s) => { if (!off) setSrcs(s as { id: string; title?: string }[]); })
      .catch(() => {});
    return () => { off = true; };
  }, [agentId]);
  const ql = q.trim().toLowerCase();
  const filtered = srcs.filter((s) => !ql || (s.title || "").toLowerCase().includes(ql) || s.id.includes(ql));
  const selected: string[] = multiple
    ? (Array.isArray(value) ? value : typeof value === "string" && value ? [value] : [])
    : (typeof value === "string" && value ? [value] : []);
  const cur = multiple ? null : srcs.find((s) => s.id === value);
  const display = q || (multiple ? (selected.length ? t("workflow.Nselected", { n: selected.length }) : "") : (cur ? cur.title || cur.id : ""));
  const pick = (id: string) => {
    if (multiple) {
      const next = selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id];
      onChange(next);
    } else {
      onChange(id);
      setQ("");
      setOpen(false);
    }
  };
  return (
    <div className="relative">
      <input
        className="border rounded-lg px-2.5 py-1 h-9 bg-transparent w-full text-sm"
        placeholder={multiple ? t("workflow.articleMultiPh") : t("workflow.articleSearchPh")}
        value={display}
        onChange={(e) => { setQ(e.target.value); setOpen(true); }}
        onFocus={() => setOpen(true)}
        onBlur={() => setTimeout(() => setOpen(false), 150)}
      />
      {multiple && selected.length > 0 && (
        <div className="flex flex-wrap gap-1 mt-0.5">
          {selected.map((id) => {
            const s = srcs.find((x) => x.id === id);
            return (
              <span key={id} className="text-[10px] bg-muted px-1 rounded flex items-center gap-0.5">
                {(s?.title || id).slice(0, 16)}
                <button type="button" onMouseDown={(e) => { e.preventDefault(); pick(id); }} className="text-destructive">✕</button>
              </span>
            );
          })}
        </div>
      )}
      {open && (
        <div className="absolute z-10 left-0 right-0 mt-0.5 max-h-48 overflow-auto bg-background border rounded shadow text-xs">
          {filtered.length === 0 && <p className="px-1 py-1 text-muted-foreground">{t("workflow.noMatch")}</p>}
          {filtered.map((s) => (
            <button
              type="button"
              key={s.id}
              onMouseDown={(e) => { e.preventDefault(); pick(s.id); }}
              className={"block w-full text-left px-1 py-0.5 hover:bg-muted " + (selected.includes(s.id) ? "bg-muted/60" : "")}
            >
              {multiple ? (selected.includes(s.id) ? "☑ " : "☐ ") : ""}{s.title || s.id}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// ValuePicker — the right-hand side of a comparison: pick "变量" to choose a
// ${node.field} from a dropdown, or "值" to type a literal (0.8 / "en" / ...).
// One consistent choose-style for the value slot, whether it's a var or not.
function ValuePicker({ options, value, onChange, t }: {
  options: RefOption[];
  value: string;
  onChange: (v: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  const isVar = /^\$\{[^}]+\}$/.test(value);
  const [mode, setMode] = React.useState<"var" | "literal">(isVar ? "var" : "literal");
  return (
    <div className="flex gap-1">
      <select
        className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm shrink-0"
        value={mode}
        onChange={(e) => { setMode(e.target.value as "var" | "literal"); onChange(""); }}
      >
        <option value="var">{t("workflow.valueVar")}</option>
        <option value="literal">{t("workflow.valueLiteral")}</option>
      </select>
      {mode === "var" ? (
        <select
          className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm flex-1 min-w-0"
          value={value}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="">—</option>
          {options.map((o) => <option key={o.ref} value={o.ref}>{o.label}</option>)}
        </select>
      ) : (
        <input
          className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm flex-1 min-w-0"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder="0.8 / en / …"
        />
      )}
    </div>
  );
}

// ConditionRows edits one edge's `when` as structured rows (field + operator +
// value, AND/OR combine). Falls back to a raw input when the expression can't
// be structurally parsed. Shared by EdgeProps and the condition node's branches.
function ConditionRows({ when, refOptions, onChange, t, defaultExpr = false }: {
  when: string;
  refOptions: RefOption[];
  onChange: (when: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
  defaultExpr?: boolean;
}) {
  // condition-node branches default straight into the condition editor (field
  // op value) without first picking the "条件" mode; an edge `when` still
  // defaults to plain.
  const isExpr = (when !== "" || defaultExpr) && when !== "default" && when !== "llm_route";
  const parsed = isExpr ? (when === "" ? { combine: "&&" as const, rows: [{ field: "", op: "==", value: "" }] } : parseWhen(when)) : null;
  const [combine, setCombine] = React.useState<"&&" | "||">(parsed?.combine || "&&");
  const [rows, setRows] = React.useState<CondRow[]>(parsed?.rows || [{ field: "", op: "==", value: "" }]);
  const update = (c: "&&" | "||", rs: CondRow[]) => {
    setCombine(c); setRows(rs); onChange(rowsToWhen(c, rs));
  };
  return (
    <div className="space-y-1">
      <select
        className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm w-full"
        value={isExpr ? "__expr" : when}
        onChange={(e) => {
          const v = e.target.value;
          if (v === "__expr") update(combine, rows.length ? rows : [{ field: "", op: "==", value: "" }]);
          else onChange(v);
        }}
      >
        <option value="">plain（顺序）</option>
        <option value="default">default（兜底）</option>
        <option value="llm_route">llm_route（LLM 选）</option>
        <option value="__expr">条件…</option>
      </select>
      {isExpr && (
        <div className="space-y-1 border rounded p-1 bg-muted/30">
          <select className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm w-full" value={combine}
            onChange={(e) => update(e.target.value as "&&" | "||", rows)}>
            <option value="&&">全部满足 (AND)</option>
            <option value="||">任一满足 (OR)</option>
          </select>
          {rows.map((r, i) => (
            <div key={i} className="space-y-1">
              <div className="flex gap-1 items-center">
                <select className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm flex-1 min-w-0"
                  value={r.field ? `\${${r.field}}` : ""}
                  onChange={(e) => {
                    const m = e.target.value.match(/^\$\{([^}]+)\}$/);
                    update(combine, rows.map((x, j) => j === i ? { ...x, field: m ? m[1] : x.field } : x));
                  }}>
                  <option value="">— 字段 —</option>
                  {refOptions.map((o) => <option key={o.ref} value={o.ref}>{o.label}</option>)}
                </select>
                <select className="border rounded-lg px-2.5 py-1 h-9 bg-transparent text-sm" value={r.op}
                  onChange={(e) => update(combine, rows.map((x, j) => j === i ? { ...x, op: e.target.value } : x))}>
                  {OPS.map((o) => <option key={o} value={o}>{o}</option>)}
                </select>
                <button type="button" className="text-destructive text-xs px-1" disabled={rows.length <= 1}
                  onClick={() => update(combine, rows.filter((_, j) => j !== i))}>✕</button>
              </div>
              <ValuePicker options={refOptions} value={r.value} t={t}
                onChange={(val) => update(combine, rows.map((x, j) => j === i ? { ...x, value: val } : x))} />
            </div>
          ))}
          <Button size="sm" variant="outline" className="w-full" onClick={() => update(combine, [...rows, { field: "", op: "==", value: "" }])}>
            <Plus className="h-3 w-3" /> {t("workflow.condAddRow")}
          </Button>
        </div>
      )}
    </div>
  );
}

// FormSchemaEditor edits a form node's field schema (M6): a JSON textarea
// (properties / required) with live validation — a parse error keeps the text
// but flags it, so half-typed JSON never silently drops the schema. External
// changes (YAML pane) are adopted only when they differ from what this editor
// last produced, so typing here isn't clobbered mid-keystroke.
function FormSchemaEditor({
  value,
  onChange,
  t,
}: {
  value: Record<string, unknown> | undefined;
  onChange: (v: Record<string, unknown>) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  const [text, setText] = useState(() => (value && Object.keys(value).length ? JSON.stringify(value, null, 2) : ""));
  const [err, setErr] = useState<string | null>(null);
  const lastPushed = useRef<string>("");
  useEffect(() => {
    const cur = value && Object.keys(value).length ? JSON.stringify(value, null, 2) : "";
    if (cur !== lastPushed.current) setText(cur);
  }, [value]);
  const apply = (v: string) => {
    setText(v);
    try {
      const parsed = v.trim() ? JSON.parse(v) : {};
      if (typeof parsed !== "object" || Array.isArray(parsed) || parsed === null) {
        setErr(t("workflow.formSchemaObject"));
        return;
      }
      setErr(null);
      lastPushed.current = JSON.stringify(parsed, null, 2);
      onChange(parsed as Record<string, unknown>);
    } catch {
      setErr(t("workflow.yamlError"));
    }
  };
  return (
    <div className="space-y-1">
      <span className="text-muted-foreground">{t("workflow.formSchemaHint")}</span>
      <Textarea
        rows={6}
        className="font-mono text-xs"
        value={text}
        onChange={(e) => apply(e.target.value)}
        placeholder={'{\n  "properties": { "email": { "type": "string" } },\n  "required": ["email"]\n}'}
      />
      {err && <p className="text-destructive">{err}</p>}
    </div>
  );
}

// SchemaForm renders a typed field per property of an OpenAI-style JSON schema.
// It's the standard surface every tool exposes itself through (registry
// ToolInfo.Parameters), so the editor needs no tool-specific code: a builtin
// get_time, an MCP tool, and a plugin tool all produce the same kind of form.
// Exported for the workflows page, which reuses it to render a waiting run's
// pending form (M6) with the exact same field surface.
export function SchemaForm({
  schema,
  values,
  onChange,
  agentId,
  header,
}: {
  schema?: Record<string, unknown> | null;
  values: Record<string, unknown>;
  onChange: (v: Record<string, unknown>) => void;
  agentId?: string;
  /** Optional override for the field-group label — the run page's waiting
   * form (M6) passes a fill-it-in hint instead of the editor's ref-syntax one. */
  header?: string;
}) {
  const props = (schema?.properties || {}) as Record<string, { type?: string; description?: string; enum?: string[] }>;
  const required = new Set((schema?.required || []) as string[]);
  const keys = Object.keys(props);
  if (keys.length === 0) return null;
  // message-style channel + chat_id pair: one SessionPicker fills both (search
  // by session title/preview) instead of pasting opaque IDs by hand. Covers
  // both naming conventions: chat_id/chatId (message vs create_cron_job),
  // account/accountId.
  const chatIdKey = keys.includes("chat_id") ? "chat_id" : keys.includes("chatId") ? "chatId" : null;
  const accountKey = keys.includes("account") ? "account" : keys.includes("accountId") ? "accountId" : null;
  const hasChannelChat = !!chatIdKey && keys.includes("channel") && !!agentId;
  return (
    <div className="space-y-1.5">
      <span className="text-muted-foreground font-medium">
        {header ?? `参数（可用 \${input.x} / \${node.y} 引用）`}
      </span>
      {keys.map((k) => {
        if (hasChannelChat && (k === "channel" || (accountKey !== null && k === accountKey))) return null; // filled by the session picker
        const p = props[k];
        const v = values[k];
        const isComplex = p.type === "object" || p.type === "array";
        return (
          <label key={k} className="block">
            <span className="text-muted-foreground">
              {k}
              {required.has(k) ? "*" : ""} <span className="opacity-60">({p.type})</span>
            </span>
            {hasChannelChat && k === chatIdKey ? (
              <SessionPicker
                agentId={agentId!}
                channel={String(values.channel || "")}
                chatId={String(values[chatIdKey!] || "")}
                account={accountKey ? String(values[accountKey] || "") : undefined}
                onChange={(c, cid, acc) => {
                  const next: Record<string, unknown> = { ...values, channel: c, [chatIdKey!]: cid };
                  if (accountKey) next[accountKey] = acc;
                  onChange(next);
                }}
              />
            ) : k === "source_id" && agentId ? (
              <ArticlePicker
                agentId={agentId}
                multiple={false}
                value={typeof v === "string" ? v : ""}
                onChange={(val) => onChange({ ...values, [k]: val })}
              />
            ) : k === "source_ids" && agentId ? (
              <ArticlePicker
                agentId={agentId}
                multiple={true}
                value={Array.isArray(v) ? (v as string[]) : (typeof v === "string" && v ? [v] : [])}
                onChange={(val) => onChange({ ...values, [k]: val })}
              />
            ) : p.enum ? (
              <select
                className="border rounded-lg px-2.5 py-1 h-9 bg-transparent w-full text-sm"
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
                className="border rounded-lg px-2.5 py-1 h-8 bg-transparent w-full font-mono text-sm"
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
  agentId,
  onEdit,
  onAddEdge,
  t,
}: {
  node: AnyNode;
  others: AnyNode[];
  tools: AgentRegisteredTool[];
  selTool: AgentRegisteredTool | undefined;
  refOptions: RefOption[];
  agentId: string;
  onEdit: (p: string, v: unknown) => void;
  onAddEdge: (target: string) => void;
  t: (k: string, vars?: Record<string, string | number>) => string;
}) {
  const [target, setTarget] = React.useState("");
  const locale = useLocale().locale;
  // kb_search fixes the tool to the builtin knowledgebase_search; find its
  // schema once so the panel renders the same SchemaForm a tool node would.
  const kbSearchTool = node.kind === "kb_search" ? tools.find((x) => x.name === "knowledgebase_search") : undefined;
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
          className="ml-1 border rounded-lg px-2.5 py-1 h-9 bg-background text-sm"
          value={node.kind}
          onChange={(e) => onEdit("kind", e.target.value)}
        >
          <option value="tool">tool</option>
          <option value="llm">llm</option>
          <option value="code">code</option>
          <option value="reply">reply</option>
          <option value="question_rewrite">question_rewrite</option>
          <option value="http">http</option>
          <option value="kb_search">kb_search</option>
          <option value="set">set</option>
          <option value="condition">condition</option>
          <option value="form">form</option>
        </select>
      </label>

      {node.kind === "tool" && (
        <>
          <label className="block">
            {t("workflow.tool")}
            <select
              className="border rounded-lg px-2.5 py-1 h-9 bg-background text-sm w-full"
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
              agentId={agentId}
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
              className="ml-1 border rounded-lg px-2.5 py-1 h-9 bg-background text-sm"
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

      {(node.kind === "reply" || node.kind === "question_rewrite") && (
        <div className="space-y-1">
          <div className="flex items-center justify-between gap-1">
            <span className="text-muted-foreground">
              {node.kind === "reply" ? t("workflow.replyBody") : t("workflow.rewritePrompt")}
            </span>
            <InsertField options={refOptions} onInsert={(ref) => insertAt(ref, "prompt", promptRef.current)} />
          </div>
          <Textarea
            ref={promptRef}
            rows={4}
            className="font-mono text-xs"
            value={node.prompt || ""}
            onChange={(e) => onEdit("prompt", e.target.value)}
            placeholder={node.kind === "reply"
              ? "回复内容；可用 ${node.x} / ${input.x} 引用上游"
              : "改写指令；用 ${input.x} / ${node.x} 引用要改写的内容（输出 {query}）"}
          />
        </div>
      )}

      {node.kind === "http" && (
        <div className="space-y-1">
          <label className="block">
            {t("workflow.httpMethod")}
            <select
              className="ml-1 border rounded-lg px-2.5 py-1 h-9 bg-background text-sm"
              value={(node.input?.method as string) || "GET"}
              onChange={(e) => onEdit("input", { ...(node.input || {}), method: e.target.value })}
            >
              {["GET", "POST", "PUT", "PATCH", "DELETE"].map((m) => <option key={m} value={m}>{m}</option>)}
            </select>
          </label>
          <label className="block">
            {t("workflow.httpUrl")}
            <input
              className="border rounded-lg px-2.5 py-1 h-8 bg-transparent w-full font-mono text-sm"
              value={(node.input?.url as string) || ""}
              onChange={(e) => onEdit("input", { ...(node.input || {}), url: e.target.value })}
              placeholder="https://… 可用 ${input.x} / ${node.y} 引用"
            />
          </label>
          <div>
            <span className="text-muted-foreground">{t("workflow.httpHeaders")}</span>
            <KVRows
              obj={((node.input?.headers as Record<string, unknown>) || {}) as Record<string, unknown>}
              options={refOptions}
              onChange={(v) => onEdit("input", { ...(node.input || {}), headers: v })}
            />
          </div>
          <label className="block">
            <span className="text-muted-foreground">{t("workflow.httpBody")}</span>
            <Textarea
              rows={3}
              className="font-mono text-xs"
              value={(node.input?.body as string) || ""}
              onChange={(e) => onEdit("input", { ...(node.input || {}), body: e.target.value })}
              placeholder="请求体；可用 ${input.x} / ${node.y} 引用（GET 留空）"
            />
          </label>
        </div>
      )}

      {node.kind === "kb_search" && (
        <>
          <p className="text-muted-foreground italic">
            {kbSearchTool
              ? (locale === "zh-CN" ? "搜索知识库（wiki 文章 / 灵感闪记 / 待办）" : kbSearchTool.description)
              : "knowledgebase_search 工具未注册"}
          </p>
          {kbSearchTool?.parameters ? (
            <SchemaForm
              schema={kbSearchTool.parameters}
              values={(node.input || {}) as Record<string, unknown>}
              onChange={(v) => onEdit("input", v)}
              agentId={agentId}
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

      {node.kind === "set" && (
        <div>
          <span className="text-muted-foreground">{t("workflow.setHint")}</span>
          <KVRows
            obj={(node.input || {}) as Record<string, unknown>}
            options={refOptions}
            onChange={(v) => onEdit("input", v)}
          />
        </div>
      )}

      {node.kind === "condition" && (
        <p className="text-muted-foreground italic">{t("workflow.condNodeHint")}</p>
      )}

      {node.kind === "form" && (
        <FormSchemaEditor value={node.input as Record<string, unknown> | undefined} onChange={(v) => onEdit("input", v)} t={t} />
      )}

      <label className="block">
        {t("workflow.sideEffect")}
        <select
          className="ml-1 border rounded-lg px-2.5 py-1 h-9 bg-transparent w-full text-sm"
          value={node.side_effect || ""}
          onChange={(e) => onEdit("side_effect", e.target.value)}
        >
          <option value="">{t("workflow.sideEffectDefault")}</option>
          <option value="pure">{t("workflow.sideEffectPure")}</option>
          <option value="idempotent">{t("workflow.sideEffectIdempotent")}</option>
          <option value="non-idempotent">{t("workflow.sideEffectNonIdempotent")}</option>
        </select>
      </label>

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
          className="border rounded-lg px-2.5 py-1 h-9 bg-background text-sm w-full"
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
  return (
    <div className="space-y-1.5">
      <p className="text-muted-foreground">
        {edge.from} → {edge.to}
      </p>
      <ConditionRows when={edge.when || ""} refOptions={refOptions} onChange={(w) => onEdit("when", w)} t={t} defaultExpr />
      {edge.when === "llm_route" && (
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
          className="border rounded-lg px-2.5 py-1 h-8 bg-transparent w-full font-mono text-sm"
          value={value}
          placeholder={placeholder}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </label>
  );
}
