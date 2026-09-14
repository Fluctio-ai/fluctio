"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { SaveButton } from "@/components/save-button";
import { TestButton } from "@/components/test-button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Clock, Container, Database } from "lucide-react";
import { getConfig, updateConfig, getMe, getSystemVectorization, setSystemVectorization, testEmbedding, testReranker, type ConfigResponse, type MemoryEmbeddingConfig, type MemoryRerankerConfig } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { PageHeader, SettingsCard, CardHead, Field, ToggleRow } from "@/components/settings-ui";

export default function RuntimeSettingsPage() {
  const tt = useT();
  const router = useRouter();
  const [config, setConfig] = useState<ConfigResponse | null>(null);
  const [loading, setLoading] = useState(true);

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
    const activeImage =
      sandboxBackend === "e2b"
        ? sandboxE2BTemplate
        : sandboxBackend === "boxlite"
          ? sandboxBoxliteImage
          : sandboxDockerImage;
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
    if (result?.ok === false) throw new Error(result.error || tt("common.saveFailed"));
    // System vectorization defaults live in their own namespace.
    const vecRes = await setSystemVectorization({ embedding: sysEmbedding, reranker: sysReranker } as any);
    if (vecRes?.error) throw new Error(vecRes.error);
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
      <PageHeader
        title={tt("runtime.title")}
        desc={tt("runtime.configDesc")}
        actions={<SaveButton onSave={handleSave} />}
      />

      {/* Sandbox + timezone share one card: sections split by border-t. */}
      <SettingsCard padded={false}>
        <div className="p-5 space-y-4">
          <CardHead
            icon={Clock}
            title={tt("runtime.defaultTimezone")}
            desc={tt("runtime.timezoneDesc", { tz: config.meta?.serverTimezone || "Local" })}
          />
          <Field label={tt("runtime.ianaTimezone")} htmlFor="default-timezone">
            <Input
              id="default-timezone"
              value={defaultTimezone}
              onChange={(e) => setDefaultTimezone(e.target.value)}
              placeholder="Asia/Shanghai"
              className="max-w-sm font-mono"
            />
          </Field>
        </div>
        <div className="border-t border-border p-5 space-y-4">
          <CardHead
            icon={Container}
            title={tt("runtime.sandbox")}
            desc={tt("runtime.sandboxDesc")}
            control={
              <Switch checked={sandboxEnabled} onCheckedChange={setSandboxEnabled} />
            }
          />
          {sandboxEnabled && (
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 pt-1">
              <Field label={tt("runtime.backend")}>
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
              </Field>
              {sandboxBackend === "e2b" ? (
                <>
                  <Field label={tt("runtime.e2bApiKey")}>
                    <Input
                      type="password"
                      value={sandboxE2BKey}
                      onChange={(e) => setSandboxE2BKey(e.target.value)}
                      placeholder="e2b_..."
                      className="font-mono"
                    />
                  </Field>
                  <Field label={tt("runtime.e2bTemplate")}>
                    <Input
                      value={sandboxE2BTemplate}
                      onChange={(e) => setSandboxE2BTemplate(e.target.value)}
                      placeholder="base"
                      className="font-mono"
                    />
                  </Field>
                </>
              ) : sandboxBackend === "boxlite" ? (
                <>
                  <Field label={tt("runtime.boxliteApiKey")}>
                    <Input
                      type="password"
                      value={sandboxBoxliteKey}
                      onChange={(e) => setSandboxBoxliteKey(e.target.value)}
                      placeholder="client_secret"
                      className="font-mono"
                    />
                  </Field>
                  <Field label={tt("runtime.snapshot")} hint={tt("runtime.snapshotHint")}>
                    <Input
                      value={sandboxBoxliteImage}
                      onChange={(e) => setSandboxBoxliteImage(e.target.value)}
                      placeholder="fluctio-sandbox"
                      className="font-mono"
                    />
                  </Field>
                  <Field label={tt("runtime.apiUrl")} className="sm:col-span-2">
                    <Input
                      value={sandboxBoxliteURL}
                      onChange={(e) => setSandboxBoxliteURL(e.target.value)}
                      placeholder="https://api.dev.boxlite.ai/api/v1"
                      className="font-mono"
                    />
                  </Field>
                </>
              ) : (
                <Field label={tt("runtime.dockerImage")}>
                  <Input
                    value={sandboxDockerImage}
                    onChange={(e) => setSandboxDockerImage(e.target.value)}
                    placeholder="ghcr.io/fluctio-ai/fluctio-sandbox:latest"
                    className="font-mono"
                  />
                </Field>
              )}
            </div>
          )}
        </div>
      </SettingsCard>

      {/* System vectorization defaults — embedding & reranker inherited by agents */}
      <SettingsCard className="space-y-4">
        <CardHead
          icon={Database}
          title={tt("runtime.vectorizationDefaults") || "向量化服务默认值"}
          desc={tt("runtime.vectorizationDefaultsDesc") || "系统级 embedding/reranker 默认配置。未自建向量配置的智能体会继承这些值（与 LLM 模型默认同理）。"}
        />
        <div className="space-y-3 rounded-md border border-border/60 p-3">
          <ToggleRow
            title={tt("memory.embedding") || "Embedding"}
            checked={sysEmbedding.enabled}
            onCheckedChange={(v) => setSysEmbedding({ ...sysEmbedding, enabled: v })}
          />
          {sysEmbedding.enabled && (
            <>
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label={tt("memory.model") || "Model"}>
                  <Input value={sysEmbedding.model || ""} onChange={(e) => setSysEmbedding({ ...sysEmbedding, model: e.target.value })} placeholder="BAAI/bge-m3" className="font-mono" />
                </Field>
                <Field label={tt("memory.apiBase") || "API base"}>
                  <Input value={sysEmbedding.apiBase || ""} onChange={(e) => setSysEmbedding({ ...sysEmbedding, apiBase: e.target.value })} placeholder="https://api.siliconflow.cn/v1" className="font-mono" />
                </Field>
                <Field label={tt("memory.apiKey") || "API key"}>
                  <Input type="password" value={sysEmbedding.apiKey || ""} onChange={(e) => setSysEmbedding({ ...sysEmbedding, apiKey: e.target.value })} placeholder="sk-..." className="font-mono" />
                </Field>
                <Field label={tt("memory.dimensions") || "Dimensions"}>
                  <Input type="number" value={sysEmbedding.dim || 1024} onChange={(e) => setSysEmbedding({ ...sysEmbedding, dim: parseInt(e.target.value) || 1024 })} placeholder="1024" className="font-mono" />
                </Field>
              </div>
              <div className="flex justify-end">
                <TestButton
                  disabled={!sysEmbedding.apiBase || !sysEmbedding.model}
                  onTest={async () => {
                    const r = await testEmbedding({ apiBase: sysEmbedding.apiBase || "", apiKey: sysEmbedding.apiKey || "", model: sysEmbedding.model || "", dim: sysEmbedding.dim, dimEnabled: sysEmbedding.dimEnabled });
                    if (!r.ok) throw new Error(r.error || tt("common.testFailed"));
                  }}
                />
              </div>
            </>
          )}
        </div>
        <div className="space-y-3 rounded-md border border-border/60 p-3">
          <ToggleRow
            title={tt("memory.reranker") || "Reranker"}
            checked={sysReranker.enabled}
            onCheckedChange={(v) => setSysReranker({ ...sysReranker, enabled: v })}
          />
          {sysReranker.enabled && (
            <>
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label={tt("memory.model") || "Model"}>
                  <Input value={sysReranker.model || ""} onChange={(e) => setSysReranker({ ...sysReranker, model: e.target.value })} placeholder="jina-reranker-v2-base-multilingual" className="font-mono" />
                </Field>
                <Field label={tt("memory.apiBase") || "API base"}>
                  <Input value={sysReranker.apiBase || ""} onChange={(e) => setSysReranker({ ...sysReranker, apiBase: e.target.value })} placeholder="https://api.jina.ai/v1" className="font-mono" />
                </Field>
                <Field label={tt("memory.apiKey") || "API key"} className="sm:col-span-2">
                  <Input type="password" value={sysReranker.apiKey || ""} onChange={(e) => setSysReranker({ ...sysReranker, apiKey: e.target.value })} placeholder="jina_..." className="font-mono" />
                </Field>
              </div>
              <div className="flex justify-end">
                <TestButton
                  disabled={!sysReranker.apiBase || !sysReranker.model}
                  onTest={async () => {
                    const r = await testReranker({ apiBase: sysReranker.apiBase || "", apiKey: sysReranker.apiKey || "", model: sysReranker.model || "" });
                    if (!r.ok) throw new Error(r.error || tt("common.testFailed"));
                  }}
                />
              </div>
            </>
          )}
        </div>
      </SettingsCard>
    </div>
  );
}
