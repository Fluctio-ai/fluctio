"use client";

import { useEffect, useState, useCallback } from "react";
import { useT } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import { Loader2, Check } from "lucide-react";
import { getAgentPrivacy, setAgentPrivacy } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

// Per-agent Privacy page — reads/writes the agent-scope "privacy" override
// (PrivacyCfg JSON via /api/agents/{id}/privacy). PII Scrubbing is the main
// toggle; Entropy Fallback is a conservative sub-toggle (off by default).
export default function AgentPrivacyPage() {
  const t = useT();
  const agentId = useAgentIdFromURL();

  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [entropy, setEntropy] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    const res = await getAgentPrivacy(agentId);
    const pii = res.privacy?.piiScrubbing;
    setEnabled(!!pii?.enabled);
    setEntropy(!!pii?.entropy);
    setLoading(false);
  }, [agentId]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const save = async () => {
    setSaving(true);
    setError(null);
    const res = await setAgentPrivacy(agentId, { piiScrubbing: { enabled, entropy } });
    setSaving(false);
    if (res.error) {
      setError(res.error);
    } else {
      setSaved(true);
      setTimeout(() => setSaved(false), 1500);
    }
  };

  if (loading) {
    return <Skeleton className="h-40 w-full" />;
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">{t("settings.privacy") || "隐私脱敏"}</h2>
        <p className="text-sm text-muted-foreground mt-1">
          {t("privacy.scrubDesc") ||
            "在消息发送给 LLM 前脱敏邮箱、手机号、身份证、银行卡、API 密钥等敏感信息。"}
        </p>
      </div>

      <div className="flex items-center justify-between rounded-lg border p-4">
        <div className="space-y-0.5 pr-4">
          <Label>{t("privacy.scrubTitle") || "PII 脱敏"}</Label>
          <p className="text-xs text-muted-foreground">
            {t("privacy.scrubHint") || "基于正则规则脱敏已知敏感格式（推荐开启）。"}
          </p>
        </div>
        <Switch checked={enabled} onCheckedChange={setEnabled} />
      </div>

      <div className="flex items-center justify-between rounded-lg border p-4">
        <div className="space-y-0.5 pr-4">
          <Label>{t("privacy.entropyTitle") || "高熵兜底（实验）"}</Label>
          <p className="text-xs text-muted-foreground">
            {t("privacy.entropyHint") ||
              "仅在周围出现密钥语义词时才检测未知高熵随机串。可能误伤 base64 数据，默认关闭。"}
          </p>
        </div>
        <Switch checked={entropy} onCheckedChange={setEntropy} disabled={!enabled} />
      </div>

      {error && <p className="text-sm text-destructive">{error}</p>}

      <Button onClick={save} disabled={saving}>
        {saving ? <Loader2 className="h-4 w-4 animate-spin mr-2" /> : null}
        {saving
          ? t("common.saving") || "保存中…"
          : saved
            ? t("common.saved") || "已保存"
            : t("common.save") || "保存"}
        {saved && !saving ? <Check className="h-4 w-4 ml-2" /> : null}
      </Button>
    </div>
  );
}
