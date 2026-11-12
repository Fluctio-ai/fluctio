"use client";

import { useEffect, useState, useCallback } from "react";
import { useT } from "@/lib/i18n";
import { Skeleton } from "@/components/ui/skeleton";
import { SaveButton } from "@/components/save-button";
import { PageHeader, SettingsCard, ToggleRow, Field } from "@/components/settings-ui";
import { Textarea } from "@/components/ui/textarea";
import { getAgentPrivacy, setAgentPrivacy } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

// Per-agent Privacy page — reads/writes the agent-scope "privacy" override
// (PrivacyCfg JSON via /api/agents/{id}/privacy). PII Scrubbing is the main
// toggle; Entropy Fallback is a conservative sub-toggle (off by default).
export default function AgentPrivacyPage() {
  const t = useT();
  const agentId = useAgentIdFromURL();

  const [loading, setLoading] = useState(true);
  const [enabled, setEnabled] = useState(false);
  const [entropy, setEntropy] = useState(false);
  const [wordsText, setWordsText] = useState("");

  const refresh = useCallback(async () => {
    setLoading(true);
    const res = await getAgentPrivacy(agentId);
    const pii = res.privacy?.piiScrubbing;
    setEnabled(!!pii?.enabled);
    setEntropy(!!pii?.entropy);
    setWordsText((pii?.words || []).join("\n"));
    setLoading(false);
  }, [agentId]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const save = async () => {
    const words = wordsText
      .split(/[\n,，;；]+/)
      .map((w) => w.trim())
      .filter(Boolean);
    const res = await setAgentPrivacy(agentId, { piiScrubbing: { enabled, entropy, words } });
    if (res.error) throw new Error(res.error);
  };

  if (loading) {
    return <Skeleton className="h-40 w-full" />;
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <PageHeader
        title={t("settings.privacy") || "隐私脱敏"}
        desc={
          t("privacy.scrubDesc") ||
          "在消息发送给 LLM 前脱敏邮箱、手机号、身份证、银行卡、API 密钥等敏感信息。"
        }
        actions={<SaveButton onSave={save} />}
      />

      <SettingsCard className="space-y-4">
        <ToggleRow
          title={t("privacy.scrubTitle") || "PII 脱敏"}
          hint={t("privacy.scrubHint") || "基于正则规则脱敏已知敏感格式（推荐开启）。"}
          checked={enabled}
          onCheckedChange={setEnabled}
        />
        <div className="border-t border-border" />
        <ToggleRow
          title={t("privacy.entropyTitle") || "高熵兜底（实验）"}
          hint={
            t("privacy.entropyHint") ||
            "仅在周围出现密钥语义词时才检测未知高熵随机串。可能误伤 base64 数据，默认关闭。"
          }
          checked={entropy}
          onCheckedChange={setEntropy}
          disabled={!enabled}
        />
      </SettingsCard>

      <SettingsCard>
        <Field
          label={t("privacy.wordsTitle") || "自定义敏感词"}
          hint={
            t("privacy.wordsHint") ||
            "登记人名、项目代号、主机名等正则抓不到的词，命中即脱敏。每行一个或逗号分隔；中文按子串匹配，英文按整词匹配（大小写不敏感）。"
          }
        >
          <Textarea
            className="min-h-28 font-mono"
            value={wordsText}
            onChange={(e) => setWordsText(e.target.value)}
            disabled={!enabled}
            placeholder={"夜莺项目\nmumu-nas\nCAT"}
          />
        </Field>
      </SettingsCard>
    </div>
  );
}
