"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Separator } from "@/components/ui/separator";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Save, Check, Clock, Container, Database, Boxes } from "lucide-react";
import { getConfig, updateConfig, getMe, getSystemVectorization, setSystemVectorization, type ConfigResponse, type MemoryEmbeddingConfig, type MemoryRerankerConfig } from "@/lib/api";
import { useT } from "@/lib/i18n";

export default function RuntimeSettingsPage() {
  const tt = useT();
  const router = useRouter();
  const [config, setConfig] = useState<ConfigResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [saveError, setSaveError] = useState("");

  const [sandboxEnabled, setSandboxEnabled] = useState(false);
  const [sandboxBackend, setSandboxBackend] = useState("docker");
  const [sandboxDockerImage, setSandboxDockerImage] = useState("");
  const [sandboxE2BTemplate, setSandboxE2BTemplate] = useState("base");
  const [sandboxE2BKey, setSandboxE2BKey] = useState("");
  const [sandboxBoxliteImage, setSandboxBoxliteImage] = useState("");
  const [sandboxBoxliteKey, setSandboxBoxliteKey] = useState("");
  const [sandboxBoxliteURL, setSandboxBoxliteURL] = useState("");
  const [defaultTimezone, setDefaultTimezone] = useState("");
  // System vectorization defaults — embedding/reranker inherited by agents
  // that don't define their own (scope.Setting merges system→agent).
  const [sysEmbedding, setSysEmbedding] = useState<MemoryEmbeddingConfig>({ enabled: false, provider: "", model: "", apiKey: "", apiBase: "", dim: 1024, dimEnabled: false });
  const [sysReranker, setSysReranker] = useState<MemoryRerankerConfig>({ enabled: false, provider: "", model: "", apiKey: "", apiBase: "" });

  useEffect(() => {
    // Belt-and-suspenders gate: the layout already hides the nav item,
    // but a direct URL hit needs to bounce too.
    getMe().then((m) => {
      if (m?.user?.role !== "super_admin") {
        router.replace("/settings/general");
        return;
      }
      setLoading(true);
      getConfig()
        .then((cfg) => {
          setConfig(cfg);
          setSandboxEnabled(cfg.sandbox?.enabled || false);
          const backend = cfg.sandbox?.backend || "docker";
          setSandboxBackend(backend);
          // Each backend has its own persisted field. For configs
          // predating the split there's only the legacy `image` slot,
          // so we migrate it into the backend it belonged to (the saved
          // `backend`) and leave the other two empty.
          const savedImage = cfg.sandbox?.image || "";
          setSandboxDockerImage(
            cfg.sandbox?.dockerImage ?? (backend === "docker" ? savedImage : ""),
          );
          setSandboxE2BTemplate(
            cfg.sandbox?.e2bTemplate ?? (backend === "e2b" ? savedImage || "base" : "base"),
          );
          setSandboxBoxliteImage(
            cfg.sandbox?.boxliteSnapshot ?? (backend === "boxlite" ? savedImage : ""),
          );
          setSandboxE2BKey(cfg.sandbox?.e2bKey || "");
          setSandboxBoxliteKey(cfg.sandbox?.boxliteKey || "");
          setSandboxBoxliteURL(cfg.sandbox?.boxliteUrl || "");
          setDefaultTimezone(cfg.prefs?.timezone || "");
        })
        .catch(() => {})
        .finally(() => setLoading(false));
      getSystemVectorization()
        .then((res) => {
          const v = res.vectorization;
          if (v?.embedding) setSysEmbedding({ enabled: v.embedding.enabled ?? false, provider: v.embedding.provider || "", model: v.embedding.model || "", apiKey: v.embedding.apiKey || "", apiBase: v.embedding.apiBase || "", dim: v.embedding.dim || 1024, dimEnabled: v.embedding.dimEnabled ?? false });
          if (v?.reranker) setSysReranker({ enabled: v.reranker.enabled ?? false, provider: v.reranker.provider || "", model: v.reranker.model || "", apiKey: v.reranker.apiKey || "", apiBase: v.reranker.apiBase || "" });
        })
        .catch(() => {});
    });
  }, [router]);

  const handleSave = async () => {
    setSaving(true);
    setSaved(false);
    setSaveError("");
    // Persist every backend's field so switching the dropdown after a
    // save still surfaces the value the user typed for that backend.
    // Also mirror the active backend's value into the legacy `image`
    // slot so consumers that haven't migrated still resolve correctly.
    const activeImage =
      sandboxBackend === "e2b"
        ? sandboxE2BTemplate
        : sandboxBackend === "boxlite"
          ? sandboxBoxliteImage
          : sandboxDockerImage;
    try {
      const result = await updateConfig({
        prefs: {
          timezone: defaultTimezone.trim() || undefined,
        },
        sandbox: {
          enabled: sandboxEnabled,
          backend: sandboxBackend,
          image: activeImage || undefined,
          dockerImage: sandboxDockerImage || undefined,
          e2bTemplate: sandboxE2BTemplate || undefined,
          boxliteSnapshot: sandboxBoxliteImage || undefined,
          e2bKey: sandboxE2BKey || undefined,
          boxliteKey: sandboxBoxliteKey || undefined,
          boxliteUrl: sandboxBoxliteURL || undefined,
        },
      });
      if (result?.ok === false) {
        setSaveError(result.error || tt("common.saveFailed"));
        return;
      }
      // System vectorization defaults live in their own namespace.
      const vecRes = await setSystemVectorization({ embedding: sysEmbedding, reranker: sysReranker } as any);
      if (vecRes?.error) {
        setSaveError(vecRes.error);
        return;
      }
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : tt("common.saveFailed"));
      return;
    } finally {
      setSaving(false);
    }
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  };

  if (loading) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-10 w-48" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }
  if (!config) return null;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-xl font-semibold tracking-tight">{tt("runtime.title")}</h3>
          <p className="text-sm text-muted-foreground mt-1">
            {tt("runtime.configDesc")}
          </p>
        </div>
        <Button
          onClick={handleSave}
          disabled={saving}
          variant={saved ? "outline" : "default"}
          className={saved ? "border-success/30 text-success dark:text-success" : ""}
        >
          {saved ? (
            <>
              <Check className="h-4 w-4 mr-2" />
              {tt("common.saved")}
            </>
          ) : (
            <>
              <Save className="h-4 w-4 mr-2" />
              {saving ? tt("common.saving") : tt("common.save")}
            </>
          )}
        </Button>
      </div>
      {saveError && (
        <div className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
          {saveError}
        </div>
      )}

      <div className="rounded-lg border border-border bg-card">
        <div className="p-5">
          <div className="flex items-start gap-3">
            <Clock className="mt-0.5 h-4 w-4 text-info" />
            <div className="grid flex-1 gap-4 sm:grid-cols-[1fr_260px] sm:items-start">
              <div>
                <h3 className="font-medium">{tt("runtime.defaultTimezone")}</h3>
                <p className="mt-1 text-sm text-muted-foreground">
                  {tt("runtime.timezoneDesc", { tz: config.meta?.serverTimezone || "Local" })}
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="default-timezone">{tt("runtime.ianaTimezone")}</Label>
                <Input
                  id="default-timezone"
                  value={defaultTimezone}
                  onChange={(e) => setDefaultTimezone(e.target.value)}
                  placeholder="Asia/Shanghai"
                  className="font-mono text-sm"
                />
              </div>
            </div>
          </div>
        </div>
        <Separator />
        <div className="p-5">
          <div className="flex items-center justify-between">
            <div>
              <div className="flex items-center gap-2 mb-1">
                <Container className="h-4 w-4 text-accent" />
                <h3 className="font-medium">{tt("runtime.sandbox")}</h3>
              </div>
              <p className="text-sm text-muted-foreground">
                {tt("runtime.sandboxDesc")}
              </p>
            </div>
            <Switch checked={sandboxEnabled} onCheckedChange={setSandboxEnabled} />
          </div>
        </div>
        {sandboxEnabled && (
          <div className="px-5 pb-5 space-y-4">
            <Separator />
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label>{tt("runtime.backend")}</Label>
                <Select value={sandboxBackend} onValueChange={(v) => v && setSandboxBackend(v)}>
                  <SelectTrigger>
                    <SelectValue>
                      {(v: unknown) =>
                        ({ docker: tt("runtime.backendDocker"), e2b: tt("runtime.backendE2b"), boxlite: tt("runtime.backendBoxlite") } as Record<string, string>)[
                          v as string
                        ] ?? (v as string) ?? ""
                      }
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="docker">{tt("runtime.backendDocker")}</SelectItem>
                    <SelectItem value="e2b">{tt("runtime.backendE2b")}</SelectItem>
                    <SelectItem value="boxlite">{tt("runtime.backendBoxlite")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              {sandboxBackend === "e2b" ? (
                <>
                  <div className="space-y-2">
                    <Label>{tt("runtime.e2bApiKey")}</Label>
                    <Input
                      type="password"
                      value={sandboxE2BKey}
                      onChange={(e) => setSandboxE2BKey(e.target.value)}
                      placeholder="e2b_..."
                      className="font-mono text-sm"
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>{tt("runtime.e2bTemplate")}</Label>
                    <Input
                      value={sandboxE2BTemplate}
                      onChange={(e) => setSandboxE2BTemplate(e.target.value)}
                      placeholder="base"
                      className="font-mono text-sm"
                    />
                  </div>
                </>
              ) : sandboxBackend === "boxlite" ? (
                <>
                  <div className="space-y-2">
                    <Label>{tt("runtime.boxliteApiKey")}</Label>
                    <Input
                      type="password"
                      value={sandboxBoxliteKey}
                      onChange={(e) => setSandboxBoxliteKey(e.target.value)}
                      placeholder="client_secret"
                      className="font-mono text-sm"
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>{tt("runtime.snapshot")}</Label>
                    <Input
                      value={sandboxBoxliteImage}
                      onChange={(e) => setSandboxBoxliteImage(e.target.value)}
                      placeholder="fluctio-sandbox"
                      className="font-mono text-sm"
                    />
                    <p className="text-xs text-muted-foreground">
                      {tt("runtime.snapshotHint")}
                    </p>
                  </div>
                  <div className="space-y-2 sm:col-span-2">
                    <Label>{tt("runtime.apiUrl")}</Label>
                    <Input
                      value={sandboxBoxliteURL}
                      onChange={(e) => setSandboxBoxliteURL(e.target.value)}
                      placeholder="https://api.dev.boxlite.ai/api/v1"
                      className="font-mono text-sm"
                    />
                  </div>
                </>
              ) : (
                <div className="space-y-2">
                  <Label>{tt("runtime.dockerImage")}</Label>
                  <Input
                    value={sandboxDockerImage}
                    onChange={(e) => setSandboxDockerImage(e.target.value)}
                    placeholder="ghcr.io/fluctio-ai/fluctio-sandbox:latest"
                    className="font-mono text-sm"
                  />
                </div>
              )}
            </div>
          </div>
        )}
      </div>

      {/* System vectorization defaults — embedding & reranker inherited by agents */}
      <div className="rounded-lg border border-border bg-card p-5 space-y-4">
        <div>
          <div className="flex items-center gap-2 mb-1">
            <Database className="h-4 w-4 text-primary" />
            <h3 className="font-medium">{tt("runtime.vectorizationDefaults") || "向量化服务默认值"}</h3>
          </div>
          <p className="text-sm text-muted-foreground">{tt("runtime.vectorizationDefaultsDesc") || "系统级 embedding/reranker 默认配置。未自建向量配置的智能体会继承这些值（与 LLM 模型默认同理）。"}</p>
        </div>
        <div className="space-y-2 rounded-md border border-border/60 p-3">
          <div className="flex items-center justify-between">
            <span className="text-sm font-medium">{tt("memory.embedding") || "Embedding"}</span>
            <Switch checked={sysEmbedding.enabled} onCheckedChange={(v) => setSysEmbedding({ ...sysEmbedding, enabled: v })} />
          </div>
          {sysEmbedding.enabled && (
            <div className="grid gap-2 sm:grid-cols-2">
              <Input value={sysEmbedding.model || ""} onChange={(e) => setSysEmbedding({ ...sysEmbedding, model: e.target.value })} placeholder="BAAI/bge-m3" className="font-mono text-sm" />
              <Input value={sysEmbedding.apiBase || ""} onChange={(e) => setSysEmbedding({ ...sysEmbedding, apiBase: e.target.value })} placeholder="https://api.siliconflow.cn/v1" className="font-mono text-sm" />
              <Input type="password" value={sysEmbedding.apiKey || ""} onChange={(e) => setSysEmbedding({ ...sysEmbedding, apiKey: e.target.value })} placeholder="sk-..." className="font-mono text-sm" />
              <Input type="number" value={sysEmbedding.dim || 1024} onChange={(e) => setSysEmbedding({ ...sysEmbedding, dim: parseInt(e.target.value) || 1024 })} placeholder="1024" className="font-mono text-sm" />
            </div>
          )}
        </div>
        <div className="space-y-2 rounded-md border border-border/60 p-3">
          <div className="flex items-center justify-between">
            <span className="text-sm font-medium">{tt("memory.reranker") || "Reranker"}</span>
            <Switch checked={sysReranker.enabled} onCheckedChange={(v) => setSysReranker({ ...sysReranker, enabled: v })} />
          </div>
          {sysReranker.enabled && (
            <div className="grid gap-2 sm:grid-cols-2">
              <Input value={sysReranker.model || ""} onChange={(e) => setSysReranker({ ...sysReranker, model: e.target.value })} placeholder="jina-reranker-v2-base-multilingual" className="font-mono text-sm" />
              <Input value={sysReranker.apiBase || ""} onChange={(e) => setSysReranker({ ...sysReranker, apiBase: e.target.value })} placeholder="https://api.jina.ai/v1" className="font-mono text-sm" />
              <Input type="password" value={sysReranker.apiKey || ""} onChange={(e) => setSysReranker({ ...sysReranker, apiKey: e.target.value })} placeholder="jina_..." className="font-mono text-sm sm:col-span-2" />
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
