"use client";

import { useEffect, useState, useCallback } from "react";
import { useT } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { MemoryTestButton } from "@/components/memory-test-button";
import { Database, Boxes, Settings2, Layers, Check, Loader2, RefreshCw } from "lucide-react";
import {
  getAgentMemory,
  setAgentMemory,
  reindexAgentMemory,
  type MemoryConfig,
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
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
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

  const refresh = useCallback(async () => {
    try {
      const res = await getAgentMemory(agentId);
      const mem: MemoryConfig = res.memory || {};
      if (mem.embedding) {
        setEmbedding({
          enabled: mem.embedding.enabled ?? false,
          provider: mem.embedding.provider || "",
          model: mem.embedding.model || "",
          apiKey: mem.embedding.apiKey || "",
          apiBase: mem.embedding.apiBase || "",
          dim: mem.embedding.dim || 1024,
          dimEnabled: mem.embedding.dimEnabled ?? false,
        });
      }
      if (mem.reranker) {
        setReranker({
          enabled: mem.reranker.enabled ?? false,
          provider: mem.reranker.provider || "",
          model: mem.reranker.model || "",
          apiKey: mem.reranker.apiKey || "",
          apiBase: mem.reranker.apiBase || "",
        });
      }
      if (mem.settings) {
        setSettings({ enabled: mem.settings.enabled ?? true });
      }
      setSummaryModel(mem.summaryModel || "");
      setKbEmbedding(mem.kbEmbedding ?? false);
      setWikiEmbedding(mem.wikiEmbedding ?? false);
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  useEffect(() => { refresh(); }, [refresh]);

  const flashSaved = () => { setSaved(true); setTimeout(() => setSaved(false), 2000); };

  const handleSave = async () => {
    setSaving(true);
    try {
      // Spread the existing memory first so we don't clobber sibling
      // fields this page doesn't edit (wikiAutoGen, autoPersist, …) —
      // writing only {embedding,reranker,settings,summaryModel} previously
      // wiped wiki auto-gen settings every time the Memory page was saved.
      const cur = await getAgentMemory(agentId).catch(() => null);
      const base = (cur?.memory || {}) as MemoryConfig;
      await setAgentMemory(agentId, { ...base, embedding, reranker, settings, summaryModel, kbEmbedding, wikiEmbedding } as any);
      flashSaved();
      await refresh();
    } finally {
      setSaving(false);
    }
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
      <div className="flex items-center justify-between gap-2">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">{t("memory.title")}</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {(t("memory.agentSubtitle") || "Memory settings for {name}").replace("{name}", agentName || agentId)}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button onClick={handleSave} disabled={saving} variant={saved ? "outline" : "default"}
            className={saved ? "border-success/30 text-success" : ""}>
            {saved ? (<><Check className="h-4 w-4 mr-2" />{t("common.saved")}</>)
              : saving ? (<><Loader2 className="h-4 w-4 mr-2 animate-spin" />{t("common.saving")}</>)
              : t("common.save")}
          </Button>
          {embedding.enabled && (
            <Button variant="outline" size="sm" className="h-9" onClick={handleReindex} disabled={reindexing || saving}>
              {reindexing ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <RefreshCw className="h-4 w-4 mr-2" />}
              {reindexing ? (t("memory.reindexing") || "Reindexing…") : (t("memory.forceReindex") || "Force reindex")}
            </Button>
          )}
        </div>
      </div>
      {reindexMsg && <p className="text-sm text-muted-foreground -mt-3">{reindexMsg}</p>}

      {/* Settings — master switch + summary model */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <Settings2 className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <h3 className="font-medium">{t("memory.memorySettings") || "Memory"}</h3>
              <p className="text-sm text-muted-foreground mt-1">{t("memory.settingsDesc")}</p>
            </div>
          </div>
          <Switch checked={settings.enabled ?? true} onCheckedChange={(v: boolean) => setSettings({ enabled: v })} />
        </div>
        <div className="mt-4 pt-4 border-t border-border space-y-1.5">
          <Label>{t("memory.summaryModel") || "Summary model"}</Label>
          <Input value={summaryModel} onChange={(e) => setSummaryModel(e.target.value)}
            placeholder="e.g. openai/gpt-4o-mini" className="font-mono text-sm" />
          <p className="text-xs text-muted-foreground/70">{t("memory.summaryModelDesc")}</p>
        </div>
      </div>

      {/* Embedding */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <Database className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <h3 className="font-medium">{t("memory.embedding") || "Embedding"}</h3>
                {embedding.enabled ? (
                  <Badge className="bg-success/15 text-success hover:bg-success/15 text-[10px]">{t("memory.configured") || "configured"}</Badge>
                ) : (
                  <Badge variant="outline" className="text-muted-foreground text-[10px]">{t("memory.notConfigured") || "not configured"}</Badge>
                )}
              </div>
              <p className="text-sm text-muted-foreground mt-1">{t("memory.embeddingDesc")}</p>
            </div>
          </div>
          <Switch checked={embedding.enabled} onCheckedChange={(v: boolean) => setEmbedding({ ...embedding, enabled: v })} />
        </div>
        {embedding.enabled && (
          <div className="mt-5 pt-5 border-t border-border space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{t("memory.provider") || "Provider"}</Label>
                <Input value={embedding.provider || ""} onChange={(e) => setEmbedding({ ...embedding, provider: e.target.value })}
                  placeholder="openai / jina / ..." className="font-mono text-sm" />
              </div>
              <div className="space-y-1.5">
                <Label>{t("memory.model") || "Model"}</Label>
                <Input value={embedding.model || ""} onChange={(e) => setEmbedding({ ...embedding, model: e.target.value })}
                  placeholder="text-embedding-3-small" className="font-mono text-sm" />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label>{t("memory.apiBase") || "API base"}</Label>
              <Input value={embedding.apiBase || ""} onChange={(e) => setEmbedding({ ...embedding, apiBase: e.target.value })}
                placeholder="https://api.openai.com/v1" className="font-mono text-sm" />
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{t("memory.apiKey") || "API key"}</Label>
                <Input type="password" value={embedding.apiKey || ""} onChange={(e) => setEmbedding({ ...embedding, apiKey: e.target.value })}
                  placeholder="sk-..." className="font-mono text-sm placeholder:text-muted-foreground/70" />
              </div>
              <div className="space-y-1.5">
                <Label>{t("memory.dimensions") || "Dimensions"}</Label>
                <div className="flex items-center gap-2">
                  <Input type="number" value={embedding.dim || 1024}
                    onChange={(e) => setEmbedding({ ...embedding, dim: parseInt(e.target.value) || 1024 })}
                    placeholder="1024" className="flex-1 font-mono text-sm" />
                  <Switch checked={!!embedding.dimEnabled} onCheckedChange={(v) => setEmbedding({ ...embedding, dimEnabled: v })}
                    aria-label={t("memory.sendDimensions") || "Send dimensions"} />
                </div>
                <p className="text-xs text-muted-foreground/70">{t("memory.sendDimensions") || "Send dimensions"}</p>
              </div>
            </div>
            <MemoryTestButton
              kind="embedding"
              apiBase={embedding.apiBase || ""}
              apiKey={embedding.apiKey || ""}
              model={embedding.model || ""}
              dim={embedding.dim}
              dimEnabled={embedding.dimEnabled}
            />
          </div>
        )}
      </div>

      {/* Reranker */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <Boxes className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <h3 className="font-medium">{t("memory.reranker") || "Reranker"}</h3>
                {reranker.enabled ? (
                  <Badge className="bg-success/15 text-success hover:bg-success/15 text-[10px]">{t("memory.configured") || "configured"}</Badge>
                ) : (
                  <Badge variant="outline" className="text-muted-foreground text-[10px]">{t("memory.notConfigured") || "not configured"}</Badge>
                )}
              </div>
              <p className="text-sm text-muted-foreground mt-1">{t("memory.rerankerDesc")}</p>
            </div>
          </div>
          <Switch checked={reranker.enabled} onCheckedChange={(v: boolean) => setReranker({ ...reranker, enabled: v })} />
        </div>
        {reranker.enabled && (
          <div className="mt-5 pt-5 border-t border-border space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{t("memory.provider") || "Provider"}</Label>
                <Input value={reranker.provider || ""} onChange={(e) => setReranker({ ...reranker, provider: e.target.value })}
                  placeholder="jina" className="font-mono text-sm" />
              </div>
              <div className="space-y-1.5">
                <Label>{t("memory.model") || "Model"}</Label>
                <Input value={reranker.model || ""} onChange={(e) => setReranker({ ...reranker, model: e.target.value })}
                  placeholder="jina-reranker-v2-base-multilingual" className="font-mono text-sm" />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label>{t("memory.apiBase") || "API base"}</Label>
              <Input value={reranker.apiBase || ""} onChange={(e) => setReranker({ ...reranker, apiBase: e.target.value })}
                placeholder="https://api.jina.ai/v1" className="font-mono text-sm" />
            </div>
            <div className="space-y-1.5">
              <Label>{t("memory.apiKey") || "API key"}</Label>
              <Input type="password" value={reranker.apiKey || ""} onChange={(e) => setReranker({ ...reranker, apiKey: e.target.value })}
                placeholder="jina_..." className="font-mono text-sm placeholder:text-muted-foreground/70" />
            </div>
            <MemoryTestButton
              kind="reranker"
              apiBase={reranker.apiBase || ""}
              apiKey={reranker.apiKey || ""}
              model={reranker.model || ""}
            />
          </div>
        )}
      </div>

      {/* Apply scope — which modules share the embedder/reranker above */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start gap-3 min-w-0">
          <Layers className="h-4 w-4 text-primary mt-0.5 shrink-0" />
          <div className="min-w-0">
            <h3 className="font-medium">{t("memory.applyScope") || "应用范围"}</h3>
            <p className="text-sm text-muted-foreground mt-1">{t("memory.applyScopeDesc")}</p>
          </div>
        </div>
        <div className="mt-4 pt-4 border-t border-border space-y-4">
          <div className="flex items-start justify-between gap-4">
            <div className="min-w-0">
              <h4 className="text-sm font-medium">{t("memory.kbEmbedding") || "知识库"}</h4>
              <p className="text-xs text-muted-foreground mt-1">{t("memory.kbEmbeddingDesc")}</p>
            </div>
            <Switch checked={kbEmbedding} onCheckedChange={(v: boolean) => setKbEmbedding(v)} />
          </div>
          <div className="flex items-start justify-between gap-4">
            <div className="min-w-0">
              <h4 className="text-sm font-medium">{t("memory.wikiEmbedding") || "维基"}</h4>
              <p className="text-xs text-muted-foreground mt-1">{t("memory.wikiEmbeddingDesc")}</p>
            </div>
            <Switch checked={wikiEmbedding} onCheckedChange={(v: boolean) => setWikiEmbedding(v)} />
          </div>
        </div>
      </div>
    </div>
  );
}
