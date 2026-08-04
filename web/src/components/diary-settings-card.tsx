"use client";

import { useCallback, useEffect, useState } from "react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
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
    <div className="space-y-3 rounded-lg border border-border bg-card p-5">
      <div className="flex items-center justify-between">
        <div className="space-y-1">
          <Label className="text-sm font-medium">{t("diary.title")}</Label>
          <p className="text-xs text-muted-foreground">{t("diary.desc")}</p>
        </div>
        <Switch checked={enabled} onCheckedChange={setEnabled} disabled={!configLoaded} />
      </div>

      {enabled && (
        <div className="space-y-3 pt-1">
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label className="text-xs">{t("diary.cronTime")}</Label>
              <Input
                type="time"
                value={cronTime}
                onChange={(e) => setCronTime(e.target.value)}
                className="h-8 text-xs"
              />
              <p className="text-[11px] text-muted-foreground">{t("diary.cronTimeDesc")}</p>
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">{t("diary.thinkingMode")}</Label>
              <Select value={thinkingMode} onValueChange={(v) => v && setThinkingMode(v)}>
                <SelectTrigger className="h-8 text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="off">{t("diary.modeOff")}</SelectItem>
                  <SelectItem value="blindspots">{t("diary.modeBlindspots")}</SelectItem>
                  <SelectItem value="deep">{t("diary.modeDeep")}</SelectItem>
                </SelectContent>
              </Select>
              <p className="text-[11px] text-muted-foreground">{t("diary.thinkingModeDesc")}</p>
            </div>
          </div>
        </div>
      )}

      <div className="flex justify-end pt-1">
        <SaveButton size="sm" onSave={handleSave} disabled={!configLoaded} />
      </div>
    </div>
  );
}
