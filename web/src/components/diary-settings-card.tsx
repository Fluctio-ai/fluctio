"use client";

import { useCallback, useEffect, useState } from "react";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Calendar } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { getAgentConfig, updateAgent } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import { SaveButton } from "@/components/save-button";
import { SettingsCard, CardHead, Field } from "@/components/settings-ui";

// DiarySettingsCard — daily-diary generation config. Lives in the
// Settings dialog's Knowledge tab next to KBSettingsCard. When enabled,
// the backend sweeps this agent's conversation_summaries once per day
// at cronTime (UTC+8) and distills a themed diary plus a "you might
// have missed" blindspot section.
export function DiarySettingsCard() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [enabled, setEnabled] = useState(false);
  const [cronTime, setCronTime] = useState("02:30");
  const [thinkingMode, setThinkingMode] = useState("blindspots");
  const [configLoaded, setConfigLoaded] = useState(false);

  useEffect(() => {
    if (!agentId) return;
    getAgentConfig(agentId)
      .then((cfg) => {
        const d = cfg.diary;
        if (d) {
          setEnabled(d.enabled ?? false);
          setCronTime(d.cronTime || "02:30");
          setThinkingMode(d.thinkingMode || "blindspots");
        }
        setConfigLoaded(true);
      })
      .catch(() => {});
  }, [agentId]);

  const handleSave = useCallback(async () => {
    if (!agentId) return;
    const res = await updateAgent(agentId, {
      diary: {
        enabled,
        cronTime,
        thinkingMode,
      },
    } as any);
    if (res?.error) throw new Error(res.error);
  }, [agentId, enabled, cronTime, thinkingMode]);

  return (
    <SettingsCard className="space-y-4">
      <CardHead
        icon={Calendar}
        title={t("diary.title")}
        desc={t("diary.desc")}
        control={
          <Switch checked={enabled} onCheckedChange={setEnabled} disabled={!configLoaded} />
        }
      />

      {enabled && (
        <div className="grid grid-cols-2 gap-4 border-t border-border pt-4">
          <Field label={t("diary.cronTime")} hint={t("diary.cronTimeDesc")}>
            <Input
              type="time"
              value={cronTime}
              onChange={(e) => setCronTime(e.target.value)}
            />
          </Field>
          <Field label={t("diary.thinkingMode")} hint={t("diary.thinkingModeDesc")}>
            <Select value={thinkingMode} onValueChange={(v) => v && setThinkingMode(v)}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="off">{t("diary.modeOff")}</SelectItem>
                <SelectItem value="blindspots">{t("diary.modeBlindspots")}</SelectItem>
                <SelectItem value="deep">{t("diary.modeDeep")}</SelectItem>
              </SelectContent>
            </Select>
          </Field>
        </div>
      )}

      <div className="flex justify-end border-t border-border pt-4">
        <SaveButton onSave={handleSave} disabled={!configLoaded} />
      </div>
    </SettingsCard>
  );
}
