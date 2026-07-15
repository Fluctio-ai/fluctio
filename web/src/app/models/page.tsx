"use client";

import { useEffect, useState } from "react";
import { useT } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Brain, Plus, Pencil, Trash2, Check, Cpu, Loader2, Download } from "lucide-react";
import {
  getAgent,
  getConfig,
  updateConfig,
  getMe,
  testProvider,
  testStoredProvider,
  listProviders,
  createProvider,
  updateProvider,
  deleteProvider,
  fetchModelsByConfig,
  type ModelEntry,
  type ProviderRow,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

// Keep these maps in sync with onboard's ProviderStep so the two flows
// look and behave identically — same preset set, same labels, same
// SelectValue render-children pattern.
const PROVIDER_PRESETS: Record<
  string,
  { apiBase: string; apiType: string; authType: string; models: string[] }
> = {
  openai: { apiBase: "https://api.openai.com/v1", apiType: "openai-chat", authType: "bearer-token", models: ["gpt-5.5"] },
  openrouter: { apiBase: "https://openrouter.ai/api/v1", apiType: "openai-chat", authType: "bearer-token", models: [] },
  anthropic: { apiBase: "https://api.anthropic.com", apiType: "anthropic-messages", authType: "api-key", models: ["claude-opus-4-7", "claude-sonnet-4-7", "claude-haiku-4-5"] },
  deepseek: { apiBase: "https://api.deepseek.com", apiType: "openai-chat", authType: "bearer-token", models: ["deepseek-v4-pro", "deepseek-v4-flash"] },
  ollama: { apiBase: "http://localhost:11434/v1", apiType: "openai-chat", authType: "bearer-token", models: [] },
  custom: { apiBase: "", apiType: "openai-chat", authType: "bearer-token", models: [] },
};

const PROVIDER_LABELS: Record<string, string> = {
  openai: "OpenAI",
  openrouter: "OpenRouter",
  anthropic: "Anthropic",
  deepseek: "DeepSeek",
  ollama: "Ollama",
  custom: "Custom",
};

const API_TYPE_LABELS: Record<string, string> = {
  "openai-chat": "OpenAI Chat Completions",
  "anthropic-messages": "Anthropic Messages",
};

const AUTH_TYPE_LABELS: Record<string, string> = {
  "bearer-token": "Bearer Token",
  "api-key": "API Key Header",
};

interface ProviderEntry {
  id: string;
  name: string;
  apiBase: string;
  apiKey: string;
  maskedKey: string;
  apiType: string;
  authType: string;
  models: ModelEntry[];
  scope: "system" | "user" | "agent";
}

function emptyModel(): ModelEntry {
  return {
    id: "",
    name: "",
    reasoning: false,
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: 200000,
    maxTokens: 8192,
  };
}

function presetModelRows(preset: string): ModelEntry[] {
  const ids = PROVIDER_PRESETS[preset]?.models || [];
  return ids.map((id) => ({ ...emptyModel(), id, name: id }));
}

export default function ModelsPage() {
  const tt = useT();
  const urlAgentId = useAgentIdFromURL();
  const inAgentContext = urlAgentId !== "default" && urlAgentId !== "";
  const [agentName, setAgentName] = useState("");
  const [agentScopeModel, setAgentScopeModel] = useState("");
  const [agentShares, setAgentShares] = useState(false);

  const [providers, setProviders] = useState<ProviderEntry[]>([]);
  const [model, setModel] = useState("");
  const [systemDefault, setSystemDefault] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  const [me, setMe] = useState<{ id: string; role: string } | null>(null);
  const isSuperAdmin = me?.role === "super_admin";
  const writeScope: "system" | "user" = isSuperAdmin ? "system" : "user";
  const writeScopeId = isSuperAdmin ? "" : (me?.id || "");

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingName, setEditingName] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [formPreset, setFormPreset] = useState("openrouter");
  const [formName, setFormName] = useState("");
  const [formApiBase, setFormApiBase] = useState("");
  const [formApiKey, setFormApiKey] = useState("");
  const [formApiType, setFormApi] = useState("openai-chat");
  const [formAuthType, setFormAuthType] = useState("api-key");
  const [formModels, setFormModels] = useState<ModelEntry[]>([]);
  type ModelTestResult = { status: "idle" | "testing" | "success" | "error"; error?: string };
  const [modelTests, setModelTests] = useState<Record<number, ModelTestResult>>({});
  const [batchTesting, setBatchTesting] = useState(false);
  const [fetching, setFetching] = useState(false);
  const [fetchResults, setFetchResults] = useState<{ id: string; contextWindow: number }[] | null>(null);
  const [fetchError, setFetchError] = useState<string | null>(null);

  const cleanModelRows = formModels
    .map((m, idx) => ({ idx, id: m.id.trim() }))
    .filter((t) => t.id);
  const allModelsPassed =
    cleanModelRows.length === 0 ||
    cleanModelRows.every((t) => modelTests[t.idx]?.status === "success");

  const allModelOptions: { value: string; label: string }[] = (() => {
    const seen = new Set<string>();
    const out: { value: string; label: string }[] = [];
    for (const p of providers) {
      for (const m of p.models) {
        const value = `${p.name}/${m.id}`;
        if (seen.has(value)) continue;
        seen.add(value);
        out.push({ value, label: `${p.name}/${m.name || m.id}` });
      }
    }
    return out;
  })();

  const fetchConfig = async (asAdmin: boolean, userId: string) => {
    setLoading(true);
    try {
      const [cfg, sysRes, userRes, agentRec, agentRes] = await Promise.all([
        getConfig().catch(() => null),
        listProviders("system", "").catch(() => null),
        asAdmin ? Promise.resolve(null) : listProviders("user", userId).catch(() => null),
        inAgentContext ? getAgent(urlAgentId).catch(() => null) : Promise.resolve(null),
        inAgentContext ? listProviders("agent", urlAgentId).catch(() => null) : Promise.resolve(null),
      ]);
      const sysRows: ProviderRow[] = (sysRes && Array.isArray(sysRes.providers)) ? (sysRes.providers as ProviderRow[]) : [];
      const userRows: ProviderRow[] = (userRes && Array.isArray(userRes.providers)) ? (userRes.providers as ProviderRow[]) : [];
      const agentRows: ProviderRow[] = (agentRes && Array.isArray(agentRes.providers)) ? (agentRes.providers as ProviderRow[]) : [];
      const toEntry = (r: ProviderRow, sc: "system" | "user" | "agent"): ProviderEntry => ({
        id: r.id, name: r.name, apiBase: r.apiBase || "", apiKey: "",
        maskedKey: r.apiKey || "", apiType: r.apiType || "openai-chat",
        authType: r.authType || "bearer-token", models: r.models || [], scope: sc,
      });
      const entries: ProviderEntry[] = asAdmin
        ? sysRows.map((r) => toEntry(r, "system"))
        : [
            ...agentRows.map((r) => toEntry(r, "agent")),
            ...userRows.map((r) => toEntry(r, "user")),
            ...sysRows.map((r) => toEntry(r, "system")),
          ];
      setProviders(entries);
      setModel(cfg?.agents?.defaults?.model || "");
      setSystemDefault(cfg?.meta?.systemDefaultModel || "");
      const ag = (agentRec as { agent?: { name?: string; model?: string; shareModelConfig?: boolean } } | null)?.agent;
      setAgentName(ag?.name || "");
      setAgentScopeModel(ag?.model || "");
      setAgentShares(!!ag?.shareModelConfig);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    getMe().then((m) => {
      if (!m?.user) return;
      const meRec = { id: m.user.id, role: m.user.role };
      setMe(meRec);
      fetchConfig(meRec.role === "super_admin", meRec.id);
    });
  }, []);

  const openAddDialog = () => {
    setEditingName(null); setEditingId(null);
    setFormPreset("openai"); setFormName("openai");
    setFormApiBase(PROVIDER_PRESETS["openai"].apiBase);
    setFormApi(PROVIDER_PRESETS["openai"].apiType);
    setFormAuthType(PROVIDER_PRESETS["openai"].authType);
    setFormApiKey(""); setFormModels(presetModelRows("openai"));
    setModelTests({}); setFetchResults(null); setFetchError(null);
    setDialogOpen(true);
  };

  const openEditDialog = (provider: ProviderEntry) => {
    setEditingName(provider.name); setEditingId(provider.id);
    const preset = Object.keys(PROVIDER_PRESETS).includes(provider.name) ? provider.name : "custom";
    setFormPreset(preset); setFormName(provider.name);
    setFormApiBase(provider.apiBase); setFormApi(provider.apiType);
    setFormAuthType(provider.authType || "bearer-token"); setFormApiKey("");
    setFormModels(
      (provider.models || []).map((m) => {
        const base = emptyModel();
        return { ...base, ...m, cost: { ...base.cost, ...(m.cost || {}) }, input: m.input && m.input.length > 0 ? [...m.input] : base.input };
      }),
    );
    setModelTests(
      provider.models
        ? Object.fromEntries(provider.models.map((_m, idx) => [idx, { status: "success" as const }]))
        : {},
    );
    setFetchResults(null); setFetchError(null);
    setDialogOpen(true);
  };

  const handlePresetChange = (preset: string) => {
    setFormPreset(preset);
    const cfg = PROVIDER_PRESETS[preset];
    if (cfg) { setFormApiBase(cfg.apiBase); setFormApi(cfg.apiType); setFormAuthType(cfg.authType); }
    setFormName(preset === "custom" ? "" : preset);
    setFormModels(presetModelRows(preset));
    setModelTests({});
  };

  const handleTestConnection = async () => {
    const targets = formModels.map((m, idx) => ({ idx, id: m.id.trim() })).filter((t) => t.id);
    if (targets.length === 0) return;
    const editingRow = editingId ? providers.find((p) => p.id === editingId) : undefined;
    const useStoredKey = !!editingRow && !formApiKey.trim();
    setBatchTesting(true);
    setModelTests((prev) => {
      const next = { ...prev };
      for (const t of targets) next[t.idx] = { status: "testing" };
      return next;
    });
    await Promise.all(
      targets.map(async ({ idx, id }) => {
        try {
          const result = useStoredKey && editingRow
            ? await testStoredProvider(editingRow.id, id, { apiBase: formApiBase, apiType: formApiType, authType: formAuthType })
            : await testProvider({ apiBase: formApiBase, apiKey: formApiKey, model: id, apiType: formApiType, authType: formAuthType });
          setModelTests((prev) => ({
            ...prev,
            [idx]: result.ok ? { status: "success" } : { status: "error", error: result.error || tt("models.connectionFailed") },
          }));
        } catch {
          setModelTests((prev) => ({ ...prev, [idx]: { status: "error", error: tt("models.connectionFailed") } }));
        }
      }),
    );
    setBatchTesting(false);
  };

  const handleAddModel = () => setFormModels((prev) => [...prev, emptyModel()]);

  // "Fetch model list" — calls the upstream provider's list-models
  // endpoint using the config currently in the form. Mirrors
  // handleTestConnection's useStoredKey: if editing an existing provider
  // and the key field is empty, pass providerId so the backend resolves
  // the stored key server-side.
  const handleFetchModels = async () => {
    setFetching(true);
    setFetchError(null);
    setFetchResults(null);
    try {
      const editingRow = editingId ? providers.find((p) => p.id === editingId) : undefined;
      const useStoredKey = !!editingRow && !formApiKey.trim();
      const list = useStoredKey
        ? await fetchModelsByConfig({ apiBase: formApiBase, apiType: formApiType, providerId: editingRow!.id })
        : await fetchModelsByConfig({ apiBase: formApiBase, apiKey: formApiKey, apiType: formApiType });
      setFetchResults(list);
    } catch {
      setFetchError(tt("models.fetchFailed"));
    } finally {
      setFetching(false);
    }
  };

  const handlePickFetchedModel = (id: string, contextWindow: number) => {
    setFormModels((prev) => [
      ...prev,
      { ...emptyModel(), id, name: id, contextWindow: contextWindow || 200000 },
    ]);
  };

  const handleUpdateModel = (index: number, field: string, value: unknown) => {
    setFormModels((prev) => {
      const updated = [...prev];
      const m = { ...updated[index], cost: { ...updated[index].cost }, input: [...updated[index].input] };
      if (field === "id") m.id = value as string;
      else if (field === "name") m.name = value as string;
      else if (field === "reasoning") m.reasoning = value as boolean;
      else if (field === "contextWindow") m.contextWindow = Number(value) || 0;
      else if (field === "maxTokens") m.maxTokens = Number(value) || 0;
      updated[index] = m;
      return updated;
    });
    if (field === "id") {
      setModelTests((prev) => {
        if (prev[index] === undefined) return prev;
        const { [index]: _drop, ...rest } = prev;
        void _drop;
        return rest;
      });
    }
  };

  const handleRemoveModel = (index: number) => {
    setFormModels((prev) => prev.filter((_, i) => i !== index));
    setModelTests((prev) => {
      const next: Record<number, ModelTestResult> = {};
      for (const [k, v] of Object.entries(prev)) {
        const i = Number(k);
        if (i === index) continue;
        next[i > index ? i - 1 : i] = v;
      }
      return next;
    });
  };

  const flashSaved = () => { setSaved(true); setTimeout(() => setSaved(false), 2000); };

  const handleSaveProvider = async () => {
    const name = formName.toLowerCase().trim().replace(/\s+/g, "-");
    if (!name) return;
    const cleanedModels = formModels.filter((m) => m.id.trim());
    const editingRow = editingId ? providers.find((p) => p.id === editingId) : undefined;
    setSaving(true);
    try {
      if (editingRow) {
        await updateProvider(editingRow.id, { apiBase: formApiBase, apiKey: formApiKey || undefined, apiType: formApiType, authType: formAuthType, models: cleanedModels });
      } else {
        await createProvider({ scope: writeScope, scopeId: writeScopeId, name, apiBase: formApiBase, apiKey: formApiKey, apiType: formApiType, authType: formAuthType, models: cleanedModels });
      }
      flashSaved();
    } finally { setSaving(false); }
    setDialogOpen(false);
    await fetchConfig(isSuperAdmin, me?.id || "");
  };

  const handleDeleteProvider = async (row: ProviderEntry) => {
    setSaving(true);
    try { await deleteProvider(row.id); flashSaved(); } finally { setSaving(false); }
    await fetchConfig(isSuperAdmin, me?.id || "");
  };

  const handleSaveAll = async () => {
    setSaving(true);
    try { await updateConfig({ agents: { defaults: { model: model.trim() } } }); flashSaved(); await fetchConfig(isSuperAdmin, me?.id || ""); } finally { setSaving(false); }
  };

  const handleDefaultModelChange = async (value: string) => {
    setModel(value);
    if (!value.trim()) return;
    setSaving(true);
    try { await updateConfig({ agents: { defaults: { model: value.trim() } } }); flashSaved(); await fetchConfig(isSuperAdmin, me?.id || ""); } finally { setSaving(false); }
  };

  const handleClearOverride = async () => {
    setSaving(true);
    try { await updateConfig({ agents: { defaults: { model: "" } } }); flashSaved(); await fetchConfig(isSuperAdmin, me?.id || ""); } finally { setSaving(false); }
  };

  if (loading) {
    return (
      <div className="p-6 space-y-6 max-w-5xl mx-auto">
        <Skeleton className="h-10 w-48" />
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {[1, 2, 3].map((i) => (<Skeleton key={i} className="h-48" />))}
        </div>
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">{tt("models.title")}</h2>
          <p className="text-sm text-muted-foreground mt-1">{tt("models.subtitle")}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={openAddDialog}>
            <Plus className="h-4 w-4 mr-2" />
            {tt("models.addProvider")}
          </Button>
          <Button onClick={handleSaveAll} disabled={saving} variant={saved ? "outline" : "default"} className={saved ? "border-success/30 text-success" : ""}>
            {saved ? (<><Check className="h-4 w-4 mr-2" />{tt("common.saved")}</>) : saving ? tt("common.saving") : tt("common.save")}
          </Button>
        </div>
      </div>

      {(() => {
        const inheriting = !isSuperAdmin && !model.trim();
        const overridden = !isSuperAdmin && !inheriting;
        const effectiveFallback = inAgentContext && agentShares && agentScopeModel ? agentScopeModel : systemDefault;
        const fallbackSource = inAgentContext && agentShares && agentScopeModel ? "agent" : "system";
        return (
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between gap-2 mb-3">
          <div className="flex items-center gap-2">
            <Cpu className="h-4 w-4 text-primary" />
            <h3 className="font-medium">{inAgentContext ? tt("models.activeModel") : tt("models.defaultModel")}</h3>
            {!isSuperAdmin && (inheriting ? (
              <Badge variant="outline" className="text-[10px]">{tt("models.inheriting")}</Badge>
            ) : (
              <Badge className="bg-primary/10 text-primary hover:bg-primary/10 text-[10px]">{tt("models.override")}</Badge>
            ))}
          </div>
          {overridden && (
            <Button variant="ghost" size="sm" className="h-7 text-xs" onClick={handleClearOverride} disabled={saving}>
              {tt("models.clearOverride")}
            </Button>
          )}
        </div>
        {allModelOptions.length > 0 ? (
          <Select value={inheriting ? "" : model} onValueChange={(v: string | null) => v && handleDefaultModelChange(v)}>
            <SelectTrigger className="font-mono text-sm max-w-md">
              <SelectValue placeholder={inheriting ? `${tt("models.inherit")} (${effectiveFallback || tt("models.noDefault")})` : tt("models.selectModel")} />
            </SelectTrigger>
            <SelectContent className="!w-auto !min-w-[var(--anchor-width)] !overflow-x-visible">
              {allModelOptions.map((opt) => (
                <SelectItem key={opt.value} value={opt.value}>
                  <span className="font-mono text-sm whitespace-nowrap">{opt.value}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        ) : (
          <Input value={inheriting ? "" : model} onChange={(e) => setModel(e.target.value)}
            placeholder={inheriting ? (effectiveFallback ? `${tt("models.inherit")} (${effectiveFallback})` : "e.g. openai/gpt-4o") : "e.g. openai/gpt-4o"}
            className="font-mono text-sm max-w-md" />
        )}
        <p className="text-xs text-muted-foreground mt-2">
          {isSuperAdmin ? tt("models.usedByAgents") : inheriting ? (
            fallbackSource === "agent" ? (
              <><strong>{agentName || tt("models.thisAgent")}</strong> {tt("models.usingSystemDefault")} <code className="text-[11px]">{effectiveFallback}</code></>
            ) : (
              <>{tt("models.usingSystemDefault")}{effectiveFallback ? <>: <code className="text-[11px]">{effectiveFallback}</code></> : <> {tt("models.noneConfigured")}</>}. {tt("models.pickModelOverride")} {inAgentContext ? tt("models.thisAgent") : tt("models.only")}</>
            )
          ) : (
            <>{tt("models.overrideAppliesTo")} {inAgentContext ? <strong>{tt("models.thisAgent")}</strong> : <>agents</>}. {tt("models.overrideInFormat")} <code className="text-[11px]">provider/modelId</code>.</>
          )}
        </p>
      </div>
        );
      })()}

      {providers.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-warning/10 mb-4">
              <Brain className="h-7 w-7 text-warning" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">{tt("models.noProviders")}</p>
            <p className="text-xs text-muted-foreground/60 mb-4">{tt("models.addProviderHint")}</p>
            <Button variant="outline" size="sm" onClick={openAddDialog}>
              <Plus className="h-4 w-4 mr-2" />{tt("models.addProvider")}
            </Button>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{tt("models.colName")}</TableHead>
                <TableHead>{tt("models.colApiBase")}</TableHead>
                <TableHead>{tt("models.colApiKey")}</TableHead>
                <TableHead>{tt("models.colModels")}</TableHead>
                <TableHead>{tt("models.colSource")}</TableHead>
                <TableHead className="text-right">{tt("models.colActions")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {providers.map((provider) => {
                const editable = isSuperAdmin ? provider.scope === "system" : provider.scope === "user";
                const sourceLabel =
                  provider.scope === "agent" ? tt("models.inheritedFromAgent")
                  : editable ? tt("models.mine") : tt("models.inheritedFromAdmin");
                const sourceTitle =
                  provider.scope === "agent" ? tt("models.ownerShared")
                  : editable ? "" : tt("models.adminShared");
                return (
                <TableRow key={`${provider.scope}:${provider.id}`}>
                  <TableCell className="font-medium">{provider.name}</TableCell>
                  <TableCell><code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{provider.apiBase || "—"}</code></TableCell>
                  <TableCell><code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{provider.maskedKey || "—"}</code></TableCell>
                  <TableCell className="text-xs text-muted-foreground">{provider.models.length}</TableCell>
                  <TableCell>
                    {editable ? (
                      <Badge variant="outline" className="bg-success/10 text-success border-success/20">{sourceLabel}</Badge>
                    ) : (
                      <Badge variant="outline" className="text-muted-foreground" title={sourceTitle}>{sourceLabel}</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button size="icon" variant="ghost" onClick={() => openEditDialog(provider)} title={editable ? tt("models.editProvider") : tt("models.readOnlyInherited")} disabled={!editable}>
                        <Pencil className="size-4" />
                      </Button>
                      <Button size="icon" variant="ghost" className="text-destructive hover:text-destructive" onClick={() => handleDeleteProvider(provider)} title={editable ? tt("common.delete") : tt("models.readOnlyInherited")} disabled={!editable}>
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              );})}
            </TableBody>
          </Table>
        </div>
      )}

      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-2xl max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{editingName ? tt("models.editProvider") : tt("models.addProvider")}</DialogTitle>
            <DialogDescription>{tt("models.providerDialogDesc")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{tt("models.providerLabel")}</Label>
                <Select value={formPreset} onValueChange={(v: string | null) => v && handlePresetChange(v)} disabled={!!editingName}>
                  <SelectTrigger className="w-full">
                    <SelectValue>{(v: unknown) => PROVIDER_LABELS[v as string] ?? (v as string) ?? ""}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {Object.keys(PROVIDER_PRESETS).map((p) => (<SelectItem key={p} value={p}>{PROVIDER_LABELS[p] ?? p}</SelectItem>))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>{tt("models.providerNameLabel")}</Label>
                <Input value={formName} onChange={(e) => setFormName(e.target.value)} placeholder="openai" className="font-mono text-sm" disabled={!!editingName} />
              </div>
            </div>

            <div className="space-y-1.5">
              <Label>{tt("models.apiBaseLabel")}</Label>
              <Input value={formApiBase} onChange={(e) => setFormApiBase(e.target.value)} placeholder="https://api.openai.com/v1" className="font-mono text-sm" />
            </div>

            <div className="space-y-1.5">
              <Label>{tt("models.apiKeyLabel")}</Label>
              <Input type={editingName && !formApiKey ? "text" : "password"} value={formApiKey} onChange={(e) => setFormApiKey(e.target.value)}
                placeholder={editingName ? (() => { const row = providers.find((p) => p.id === editingId); return row?.maskedKey || "sk-…"; })() : "sk-…"}
                className="font-mono text-sm placeholder:text-muted-foreground/70" />
              {editingName && (<p className="text-[11px] text-muted-foreground/60">{tt("models.keepExistingKey")}</p>)}
            </div>

            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{tt("models.apiTypeLabel")}</Label>
                <Select value={formApiType} onValueChange={(v: string | null) => v && setFormApi(v)}>
                  <SelectTrigger className="w-full"><SelectValue>{(v: unknown) => API_TYPE_LABELS[v as string] ?? (v as string) ?? ""}</SelectValue></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="openai-chat">OpenAI Chat Completions</SelectItem>
                    <SelectItem value="anthropic-messages">Anthropic Messages</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>{tt("models.authTypeLabel")}</Label>
                <Select value={formAuthType} onValueChange={(v: string | null) => v && setFormAuthType(v)}>
                  <SelectTrigger className="w-full"><SelectValue>{(v: unknown) => AUTH_TYPE_LABELS[v as string] ?? (v as string) ?? ""}</SelectValue></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="bearer-token">Bearer Token</SelectItem>
                    <SelectItem value="api-key">API Key Header</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="space-y-3 pt-2 border-t border-border">
              <div className="flex items-center justify-between">
                <Label className="text-base">{tt("models.modelsLabel")}</Label>
                <div className="flex items-center gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={handleFetchModels}
                    disabled={fetching || !formApiBase}
                  >
                    {fetching ? (
                      <Loader2 className="h-3 w-3 mr-1.5 animate-spin" />
                    ) : (
                      <Download className="h-3 w-3 mr-1.5" />
                    )}
                    {tt("models.fetchProviderList")}
                  </Button>
                  <Button variant="outline" size="sm" onClick={handleAddModel}>
                    <Plus className="h-3 w-3 mr-1.5" />{tt("models.addModel")}
                  </Button>
                </div>
              </div>

              {/* Fetched model list — click an item to add it as a model row. */}
              {fetchError && (
                <p className="text-xs text-destructive text-center py-1">{fetchError}</p>
              )}
              {fetchResults && (
                <div className="rounded-lg border border-border bg-muted/30 p-3 space-y-1.5 max-h-56 overflow-y-auto">
                  {fetchResults.length === 0 ? (
                    <p className="text-xs text-muted-foreground text-center py-2">
                      {tt("models.noModelsReturned")}
                    </p>
                  ) : (
                    fetchResults.map((fm) => {
                      const exists = formModels.some((m) => m.id.trim() === fm.id);
                      return (
                        <button
                          key={fm.id}
                          type="button"
                          disabled={exists}
                          onClick={() => handlePickFetchedModel(fm.id, fm.contextWindow)}
                          className="flex items-center justify-between w-full px-2.5 py-1.5 rounded text-xs hover:bg-accent font-mono disabled:opacity-40 disabled:cursor-not-allowed"
                        >
                          <span>{fm.id}</span>
                          <span className="text-muted-foreground text-[10px]">
                            {fm.contextWindow >= 1000 ? `${Math.round(fm.contextWindow / 1000)}K` : fm.contextWindow || "—"}
                          </span>
                        </button>
                      );
                    })
                  )}
                </div>
              )}

              {formModels.length === 0 && (
                <p className="text-sm text-muted-foreground/60 text-center py-4">{tt("models.noModelsConfigured")}</p>
              )}

              {formModels.map((m, idx) => {
                const tm = modelTests[idx];
                return (
                <div key={idx} className="rounded-lg border border-border bg-muted/30 p-4 space-y-3">
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-2 min-w-0">
                      <span className="text-sm font-medium text-muted-foreground">{tt("models.modelN", { n: idx + 1 })}</span>
                      {tm?.status === "testing" && (<Badge variant="outline" className="text-[10px]"><Loader2 className="mr-1 size-3 animate-spin" /> {tt("models.testing")}</Badge>)}
                      {tm?.status === "success" && (<Badge className="bg-success/15 text-success hover:bg-success/15 text-[10px]"><Check className="mr-1 size-3" /> {tt("models.connected")}</Badge>)}
                      {tm?.status === "error" && (<Badge variant="outline" className="border-destructive/40 text-destructive text-[10px]" title={tm.error}>{tt("models.failed")}</Badge>)}
                    </div>
                    <Button variant="ghost" size="sm" className="h-7 text-xs text-destructive hover:text-destructive" onClick={() => handleRemoveModel(idx)}>
                      <Trash2 className="h-3 w-3 mr-1" />{tt("models.removeModel")}
                    </Button>
                  </div>
                  <div className="grid grid-cols-2 gap-3">
                    <div className="space-y-1">
                      <Label className="text-xs">{tt("models.modelIdLabel")}</Label>
                      <Input value={m.id} onChange={(e) => handleUpdateModel(idx, "id", e.target.value)} placeholder="e.g. gpt-4o" className="font-mono text-xs h-8" />
                    </div>
                    <div className="space-y-1">
                      <Label className="text-xs">{tt("models.displayNameLabel")}</Label>
                      <Input value={m.name} onChange={(e) => handleUpdateModel(idx, "name", e.target.value)} placeholder="e.g. GPT-4o" className="text-xs h-8" />
                    </div>
                  </div>
                  <div className="grid grid-cols-2 gap-3">
                    <div className="space-y-1">
                      <Label className="text-xs">{tt("models.contextWindowLabel")}</Label>
                      <Input type="number" value={m.contextWindow || ""} onChange={(e) => handleUpdateModel(idx, "contextWindow", e.target.value)} placeholder="200000" className="font-mono text-xs h-8" />
                    </div>
                    <div className="space-y-1">
                      <Label className="text-xs">{tt("models.maxTokensLabel")}</Label>
                      <Input type="number" value={m.maxTokens || ""} onChange={(e) => handleUpdateModel(idx, "maxTokens", e.target.value)} placeholder="8192" className="font-mono text-xs h-8" />
                    </div>
                  </div>
                </div>
                );
              })}

              <div className="flex flex-col gap-2 pt-2">
                <div className="flex items-center gap-3">
                  <Button type="button" variant="outline" size="sm" onClick={handleTestConnection} disabled={batchTesting || !formApiBase || cleanModelRows.length === 0}>
                    {batchTesting ? (<><Loader2 className="mr-1 size-4 animate-spin" /> {tt("models.testingLabel")}</>) : tt("models.testConnection")}
                  </Button>
                  <span className="text-xs text-muted-foreground">
                    {cleanModelRows.length === 0 ? tt("models.addOneModel") : tt("models.pingEveryModel")}
                  </span>
                </div>
                {Object.values(modelTests).some((t) => t.status === "error") && (
                  <ul className="space-y-0.5">
                    {formModels.map((m, idx) => {
                      const tm = modelTests[idx];
                      if (!tm || tm.status !== "error" || !m.id.trim()) return null;
                      return (<li key={idx} className="text-xs text-destructive break-all"><code className="font-mono">{m.id}</code>: {tm.error}</li>);
                    })}
                  </ul>
                )}
              </div>
            </div>
          </div>
          <DialogFooter className="flex flex-col gap-2 sm:flex-row sm:items-center">
            {!allModelsPassed && (<span className="text-xs text-muted-foreground sm:mr-auto">{tt("models.testEveryModel")}</span>)}
            <Button variant="outline" onClick={() => setDialogOpen(false)}>{tt("common.cancel")}</Button>
            <Button onClick={handleSaveProvider} disabled={!formName.trim() || saving || !allModelsPassed}>
              {editingName ? tt("models.updateBtn") : tt("models.addBtn")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
