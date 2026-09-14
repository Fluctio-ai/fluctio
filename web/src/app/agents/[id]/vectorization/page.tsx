"use client";

import { useEffect, useState, useCallback } from "react";
import { useT } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { SaveButton } from "@/components/save-button";
import { MemoryTestButton } from "@/components/memory-test-button";
import { PageHeader, SettingsCard, CardHead, Field, ToggleRow } from "@/components/settings-ui";
import { Database, Boxes, Settings2, Layers, Loader2, RefreshCw } from "lucide-react";
import {
  getAgentMemory,
  setAgentMemory,
  getAgentVectorization,
  setAgentVectorization,
  getSystemVectorization,
  reindexAgentMemory,
  reindexWikiEmbeddings,
  type MemoryConfig,
  type VectorizationConfig,
  type MemoryEmbeddingConfig,
  type MemoryRerankerConfig,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

// Per-agent Memory page — reads/writes the agent-scope "memory" override
// (MemoryCfg JSON via /api/agents/{id}/memory). Each Embedding/Reranker
// block has an inline Test button (POST /api/memory/test-embedding or
// /test-reranker) that pings the endpoint with the form's inline
// credentials before saving. Reindex re-vectorizes all summaries.
export default function AgentMemoryPage() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);

  const [loading, setLoading] = useState(true);
  const [reindexing, setReindexing] = useState(false);
  const [reindexMsg, setReindexMsg] = useState<string | null>(null);
  const [summaryModel, setSummaryModel] = useState("");

  const [embedding, setEmbedding] = useState<MemoryEmbeddingConfig>({
    enabled: false, provider: "", model: "", apiKey: "", apiBase: "", dim: 1024, dimEnabled: false,
  });
  const [reranker, setReranker] = useState<MemoryRerankerConfig>({
    enabled: false, provider: "", model: "", apiKey: "", apiBase: "",
  });
  const [settings, setSettings] = useState<{ enabled?: boolean }>({ enabled: true });
  // Apply-scope toggles: which modules share the embedder/reranker above.
  const [kbEmbedding, setKbEmbedding] = useState(false);
  const [wikiEmbedding, setWikiEmbedding] = useState(false);
  const [wikiThreshold, setWikiThreshold] = useState(0.45);
  const [wikiReindexing, setWikiReindexing] = useState(false);
  const [wikiReindexMsg, setWikiReindexMsg] = useState<string | null>(null);

  // System-level vectorization defaults (set from Runtime → 向量化服务默认值).
  // When embCustom/rerCustom is false the agent inherits these by saving
  // WITHOUT an embedding/reranker key (scope.Setting merge is per top-level
  // key, so an absent key falls through to the system row — true reuse).
  const [sysEmbedding, setSysEmbedding] = useState<MemoryEmbeddingConfig | null>(null);
  const [sysReranker, setSysReranker] = useState<MemoryRerankerConfig | null>(null);
  const [embCustom, setEmbCustom] = useState(false);
  const [rerCustom, setRerCustom] = useState(false);

  const refresh = useCallback(async () => {
    try {
      // Vector fields live under /vectorization; settings + summaryModel
      // remain under /memory (they aren't vector config).
      const [memRes, vecRes, sysRes] = await Promise.all([
        getAgentMemory(agentId),
        getAgentVectorization(agentId),
        getSystemVectorization(),
      ]);
      const mem: MemoryConfig = memRes.memory || {};
      if (mem.settings) {
        setSettings({ enabled: mem.settings.enabled ?? true });
      }
      setSummaryModel(mem.summaryModel || "");
      const vec: VectorizationConfig = vecRes.vectorization || {};
      if (vec.embedding) {
        setEmbedding({
          enabled: vec.embedding.enabled ?? false,
          provider: vec.embedding.provider || "",
          model: vec.embedding.model || "",
          apiKey: vec.embedding.apiKey || "",
          apiBase: vec.embedding.apiBase || "",
          dim: vec.embedding.dim || 1024,
          dimEnabled: vec.embedding.dimEnabled ?? false,
        });
      }
      if (vec.reranker) {
        setReranker({
          enabled: vec.reranker.enabled ?? false,
          provider: vec.reranker.provider || "",
          model: vec.reranker.model || "",
          apiKey: vec.reranker.apiKey || "",
          apiBase: vec.reranker.apiBase || "",
        });
      }
      setKbEmbedding(vec.kbEmbedding ?? false);
      setWikiEmbedding(vec.wikiEmbedding ?? false);
      setWikiThreshold(vec.wikiThreshold ?? 0.45);
      // System defaults + whether this agent is inheriting them. The agent
      // GET returns the MERGED view, so an inheriting agent shows the
      // system's values; "custom" iff those differ from the system row.
      const sys: VectorizationConfig = sysRes.vectorization || {};
      const sysEmb = sys.embedding;
      const sysRer = sys.reranker;
      setSysEmbedding(sysEmb ?? null);
      setSysReranker(sysRer ?? null);
      const sameEmb = (a?: MemoryEmbeddingConfig, b?: MemoryEmbeddingConfig) =>
        !!a && !!b && a.provider === b.provider && a.model === b.model && a.apiBase === b.apiBase && a.enabled === b.enabled;
      const sameRer = (a?: MemoryRerankerConfig, b?: MemoryRerankerConfig) =>
        !!a && !!b && a.provider === b.provider && a.model === b.model && a.apiBase === b.apiBase && a.enabled === b.enabled;
      setEmbCustom(!sameEmb(vec.embedding, sysEmb));
      setRerCustom(!sameRer(vec.reranker, sysRer));
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  useEffect(() => { refresh(); }, [refresh]);

  // SaveButton owns the saving/saved/error visuals — surface backend
  // {error} responses by throwing.
  const handleSave = async () => {
    // Omit embedding/reranker when inheriting the system default — saving
    // no key lets the system row show through (true reuse; a system change
    // propagates without re-saving each agent).
    const payload: Record<string, unknown> = { kbEmbedding, wikiEmbedding, wikiThreshold };
    if (embCustom) payload.embedding = embedding;
    if (rerCustom) payload.reranker = reranker;
    // Vector fields → vectorization namespace.
    const vecRes = await setAgentVectorization(agentId, payload as any);
    if (vecRes?.error) throw new Error(vecRes.error);
    // Non-vector fields → memory namespace. Spread the existing memory
    // first so we don't clobber sibling fields this page doesn't edit
    // (wikiAutoGen, autoPersist, …).
    const cur = await getAgentMemory(agentId).catch(() => null);
    const base = (cur?.memory || {}) as MemoryConfig;
    const memRes = await setAgentMemory(agentId, { ...base, settings, summaryModel } as any);
    if (memRes?.error) throw new Error(memRes.error);
    await refresh();
  };

  const handleReindex = async () => {
    if (!embedding.enabled) return;
    if (!window.confirm(t("memory.reindexConfirm") || "Force re-embed all summaries?")) return;
    setReindexing(true);
    setReindexMsg(null);
    try {
      const res = await reindexAgentMemory(agentId);
      if (res.ok) {
        const failedPart = res.failed ? ` · ${res.failed} failed` : "";
        setReindexMsg(`${res.processed ?? 0} processed${failedPart}`);
      } else {
        setReindexMsg(`Failed: ${res.error || ""}`);
      }
    } catch (e) {
      setReindexMsg(`Failed: ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setReindexing(false);
    }
  };

  const handleWikiReindex = async (force: boolean) => {
    if (!wikiEmbedding) return;
    if (force && !window.confirm(t("memory.wikiReindexForceConfirm") || "将清空现有向量并对全部 wiki 页面重新 embedding，确定?")) return;
    setWikiReindexing(true);
    setWikiReindexMsg(null);
    try {
      const res = await reindexWikiEmbeddings(agentId, force);
      if (res.ok) {
        const failedPart = res.failed ? ` · ${res.failed} failed` : "";
        setWikiReindexMsg(`${res.processed ?? 0} processed${failedPart}`);
      } else {
        setWikiReindexMsg(`Failed: ${res.error || ""}`);
      }
    } catch (e) {
      setWikiReindexMsg(`Failed: ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setWikiReindexing(false);
    }
  };

  if (loading) {
    return (
      <div className="p-6 space-y-6 max-w-5xl mx-auto">
        <Skeleton className="h-10 w-48" />
        {[1, 2, 3].map((i) => <Skeleton key={i} className="h-32" />)}
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      {/* Header */}
      <PageHeader
        title={t("memory.title")}
        desc={(t("memory.agentSubtitle") || "Memory settings for {name}").replace("{name}", agentName || agentId)}
        actions={
          <div className="flex items-center gap-2">
            {embedding.enabled && (
              <Button variant="outline" onClick={handleReindex} disabled={reindexing}>
                {reindexing ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />}
                {reindexing ? (t("memory.reindexing") || "Reindexing…") : (t("memory.forceReindex") || "Force reindex")}
              </Button>
            )}
            <SaveButton onSave={handleSave} />
          </div>
        }
      />
      {reindexMsg && <p className="-mt-3 text-sm text-muted-foreground">{reindexMsg}</p>}

      {/* Settings — master switch + summary model */}
      <SettingsCard>
        <CardHead
          icon={Settings2}
          title={t("memory.memorySettings") || "Memory"}
          desc={t("memory.settingsDesc")}
          control={
            <Switch checked={settings.enabled ?? true} onCheckedChange={(v: boolean) => setSettings({ enabled: v })} />
          }
        />
        <div className="mt-4 space-y-1.5 border-t border-border pt-4">
          <Label htmlFor="memory-summary-model">{t("memory.summaryModel") || "Summary model"}</Label>
          <Input id="memory-summary-model" value={summaryModel} onChange={(e) => setSummaryModel(e.target.value)}
            placeholder="e.g. openai/gpt-4o-mini" className="font-mono" />
          <p className="text-xs text-muted-foreground">{t("memory.summaryModelDesc")}</p>
        </div>
      </SettingsCard>

      {/* Embedding */}
      <SettingsCard>
        <CardHead
          icon={Database}
          title={t("memory.embedding") || "Embedding"}
          badge={
            (!embCustom ? !!(sysEmbedding?.enabled && sysEmbedding?.model) : embedding.enabled) ? (
              <Badge className="bg-success/15 text-success hover:bg-success/15 text-[10px]">{t("memory.configured") || "configured"}</Badge>
            ) : (
              <Badge variant="outline" className="text-muted-foreground text-[10px]">{t("memory.notConfigured") || "not configured"}</Badge>
            )
          }
          desc={t("memory.embeddingDesc")}
        />
        <div className="mt-4 border-t border-border pt-4">
          <ToggleRow
            title={t("memory.useSystemDefault") || "使用系统默认（复用运行时配置）"}
            checked={!embCustom}
            onCheckedChange={(v) => setEmbCustom(!v)}
          />
          {!embCustom && (
            <p className="mt-2 text-sm text-muted-foreground">
              {sysEmbedding?.enabled && sysEmbedding?.model ? (
                <>{t("memory.inheritsSys") || "继承系统配置"}：<span className="font-mono text-foreground">{[sysEmbedding?.provider, sysEmbedding.model].filter(Boolean).join("/")}</span></>
              ) : (
                <>{t("memory.sysNotConfigured") || "系统默认未启用或未配置模型，请在 运行时 → 向量化服务默认值 设置并启用。"}</>
              )}
            </p>
          )}
        </div>
        {embCustom && (
          <div className="mt-4 space-y-4 border-t border-border pt-4">
            <ToggleRow
              title={t("memory.enabled") || "启用"}
              checked={embedding.enabled}
              onCheckedChange={(v: boolean) => setEmbedding({ ...embedding, enabled: v })}
            />
            {embedding.enabled && (
              <>
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field label={t("memory.provider") || "Provider"}>
                    <Input value={embedding.provider || ""} onChange={(e) => setEmbedding({ ...embedding, provider: e.target.value })}
                      placeholder="openai / jina / ..." className="font-mono" />
                  </Field>
                  <Field label={t("memory.model") || "Model"}>
                    <Input value={embedding.model || ""} onChange={(e) => setEmbedding({ ...embedding, model: e.target.value })}
                      placeholder="text-embedding-3-small" className="font-mono" />
                  </Field>
                </div>
                <Field label={t("memory.apiBase") || "API base"}>
                  <Input value={embedding.apiBase || ""} onChange={(e) => setEmbedding({ ...embedding, apiBase: e.target.value })}
                    placeholder="https://api.openai.com/v1" className="font-mono" />
                </Field>
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field label={t("memory.apiKey") || "API key"}>
                    <Input type="password" value={embedding.apiKey || ""} onChange={(e) => setEmbedding({ ...embedding, apiKey: e.target.value })}
                      placeholder="sk-..." className="font-mono" />
                  </Field>
                  <Field label={t("memory.dimensions") || "Dimensions"} hint={t("memory.sendDimensions") || "Send dimensions"}>
                    <div className="flex items-center gap-2">
                      <Input type="number" value={embedding.dim || 1024}
                        onChange={(e) => setEmbedding({ ...embedding, dim: parseInt(e.target.value) || 1024 })}
                        placeholder="1024" className="flex-1 font-mono" />
                      <Switch checked={!!embedding.dimEnabled} onCheckedChange={(v) => setEmbedding({ ...embedding, dimEnabled: v })}
                        aria-label={t("memory.sendDimensions") || "Send dimensions"} />
                    </div>
                  </Field>
                </div>
                <MemoryTestButton
                  kind="embedding"
                  apiBase={embedding.apiBase || ""}
                  apiKey={embedding.apiKey || ""}
                  model={embedding.model || ""}
                  dim={embedding.dim}
                  dimEnabled={embedding.dimEnabled}
                />
              </>
            )}
          </div>
        )}
      </SettingsCard>

      {/* Reranker */}
      <SettingsCard>
        <CardHead
          icon={Boxes}
          title={t("memory.reranker") || "Reranker"}
          badge={
            (!rerCustom ? !!(sysReranker?.enabled && sysReranker?.model) : reranker.enabled) ? (
              <Badge className="bg-success/15 text-success hover:bg-success/15 text-[10px]">{t("memory.configured") || "configured"}</Badge>
            ) : (
              <Badge variant="outline" className="text-muted-foreground text-[10px]">{t("memory.notConfigured") || "not configured"}</Badge>
            )
          }
          desc={t("memory.rerankerDesc")}
        />
        <div className="mt-4 border-t border-border pt-4">
          <ToggleRow
            title={t("memory.useSystemDefault") || "使用系统默认（复用运行时配置）"}
            checked={!rerCustom}
            onCheckedChange={(v) => setRerCustom(!v)}
          />
          {!rerCustom && (
            <p className="mt-2 text-sm text-muted-foreground">
              {sysReranker?.enabled && sysReranker?.model ? (
                <>{t("memory.inheritsSys") || "继承系统配置"}：<span className="font-mono text-foreground">{[sysReranker?.provider, sysReranker.model].filter(Boolean).join("/")}</span></>
              ) : (
                <>{t("memory.sysNotConfigured") || "系统默认未启用或未配置模型，请在 运行时 → 向量化服务默认值 设置并启用。"}</>
              )}
            </p>
          )}
        </div>
        {rerCustom && (
          <div className="mt-4 space-y-4 border-t border-border pt-4">
            <ToggleRow
              title={t("memory.enabled") || "启用"}
              checked={reranker.enabled}
              onCheckedChange={(v: boolean) => setReranker({ ...reranker, enabled: v })}
            />
            {reranker.enabled && (
              <>
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field label={t("memory.provider") || "Provider"}>
                    <Input value={reranker.provider || ""} onChange={(e) => setReranker({ ...reranker, provider: e.target.value })}
                      placeholder="jina" className="font-mono" />
                  </Field>
                  <Field label={t("memory.model") || "Model"}>
                    <Input value={reranker.model || ""} onChange={(e) => setReranker({ ...reranker, model: e.target.value })}
                      placeholder="jina-reranker-v2-base-multilingual" className="font-mono" />
                  </Field>
                </div>
                <Field label={t("memory.apiBase") || "API base"}>
                  <Input value={reranker.apiBase || ""} onChange={(e) => setReranker({ ...reranker, apiBase: e.target.value })}
                    placeholder="https://api.jina.ai/v1" className="font-mono" />
                </Field>
                <Field label={t("memory.apiKey") || "API key"}>
                  <Input type="password" value={reranker.apiKey || ""} onChange={(e) => setReranker({ ...reranker, apiKey: e.target.value })}
                    placeholder="jina_..." className="font-mono" />
                </Field>
                <MemoryTestButton
                  kind="reranker"
                  apiBase={reranker.apiBase || ""}
                  apiKey={reranker.apiKey || ""}
                  model={reranker.model || ""}
                />
              </>
            )}
          </div>
        )}
      </SettingsCard>

      {/* Apply scope — which modules share the embedder/reranker above */}
      <SettingsCard>
        <CardHead
          icon={Layers}
          title={t("memory.applyScope") || "应用范围"}
          desc={t("memory.applyScopeDesc")}
        />
        <div className="mt-4 space-y-4 border-t border-border pt-4">
          <ToggleRow
            title={t("memory.kbEmbedding") || "知识库"}
            hint={t("memory.kbEmbeddingDesc")}
            checked={kbEmbedding}
            onCheckedChange={(v: boolean) => setKbEmbedding(v)}
          />
          <ToggleRow
            title={t("memory.wikiEmbedding") || "维基"}
            hint={t("memory.wikiEmbeddingDesc")}
            checked={wikiEmbedding}
            onCheckedChange={(v: boolean) => setWikiEmbedding(v)}
          />
          {wikiEmbedding && (
            <>
              <Field
                label={t("memory.wikiThreshold") || "相似度阈值"}
                hint={t("memory.wikiThresholdDesc") || "wiki 生成与搜索时的 cosine 门槛；越高越严格（结果更少更准）。默认 0.45。"}
                labelTrailing={wikiThreshold.toFixed(2)}
              >
                <input type="range" min="0" max="1" step="0.05" value={wikiThreshold}
                  onChange={(e) => setWikiThreshold(parseFloat(e.target.value))}
                  className="w-full accent-primary" />
              </Field>
              <div className="flex flex-wrap items-center gap-3 pt-1">
                <Button variant="outline" onClick={() => handleWikiReindex(false)} disabled={wikiReindexing}>
                  {wikiReindexing ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />}
                  {wikiReindexing ? (t("memory.reindexing") || "Reindexing…") : (t("memory.wikiReindex") || "补缺失向量")}
                </Button>
                <Button variant="ghost" className="text-muted-foreground" onClick={() => handleWikiReindex(true)} disabled={wikiReindexing}>
                  {t("memory.wikiReindexForce") || "强制全部重向量化"}
                </Button>
                {wikiReindexMsg && <span className="text-xs text-muted-foreground">{wikiReindexMsg}</span>}
              </div>
            </>
          )}
        </div>
      </SettingsCard>
    </div>
  );
}
