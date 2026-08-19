"use client";

import { useCallback, useEffect, useState } from "react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Layers } from "lucide-react";
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

// CardsSettingsCard — Q&A flashcard config. Lives in the Settings dialog's
// Knowledge tab next to DiarySettingsCard. Generation: nightly LLM pass
// over yesterday's diary + wiki delta (enabled / cronTime / dailyLimit).
// Push: daily due-card digest to one IM channel (pushEnabled / pushTime /
// pushChannel). Both stored on the agent's "cards" config sub-object.
export function CardsSettingsCard() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [enabled, setEnabled] = useState(false);
  const [cronTime, setCronTime] = useState("03:00");
  const [dailyLimit, setDailyLimit] = useState("10");
  const [reviewLimit, setReviewLimit] = useState("20");
  const [pushEnabled, setPushEnabled] = useState(false);
  const [pushTime, setPushTime] = useState("09:00");
  const [pushChannel, setPushChannel] = useState("wechat");
  const [configLoaded, setConfigLoaded] = useState(false);

  useEffect(() => {
    if (!agentId) return;
    getAgentConfig(agentId)
      .then((cfg) => {
        const c = cfg.cards;
        if (c) {
          setEnabled(c.enabled ?? false);
          setCronTime(c.cronTime || "03:00");
          setDailyLimit(String(c.dailyLimit || 10));
          setReviewLimit(String(c.reviewLimit || 20));
          setPushEnabled(c.pushEnabled ?? false);
          setPushTime(c.pushTime || "09:00");
          setPushChannel(c.pushChannel || "wechat");
        }
        setConfigLoaded(true);
      })
      .catch(() => {});
  }, [agentId]);

  const handleSave = useCallback(async () => {
    if (!agentId) return;
    const res = await updateAgent(agentId, {
      cards: {
        enabled,
        cronTime,
        dailyLimit: parseInt(dailyLimit, 10) || 10,
        reviewLimit: parseInt(reviewLimit, 10) || 20,
        pushEnabled,
        pushTime,
        pushChannel,
      },
    });
    if (res?.error) throw new Error(res.error);
  }, [agentId, enabled, cronTime, dailyLimit, reviewLimit, pushEnabled, pushTime, pushChannel]);

  return (
    <div className="space-y-3 rounded-lg border border-border bg-card p-5">
      <div>
        <div className="mb-1 flex items-center gap-2">
          <Layers className="h-4 w-4 text-primary" />
          <h3 className="font-medium">{t("cards.settings.title")}</h3>
        </div>
        <p className="mb-3 text-sm text-muted-foreground">{t("cards.settings.desc")}</p>
        <Switch checked={enabled} onCheckedChange={setEnabled} disabled={!configLoaded} />
      </div>

      {enabled && (
        <div className="space-y-3 pt-1">
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label className="text-xs">{t("cards.settings.genTime")}</Label>
              <Input
                type="time"
                value={cronTime}
                onChange={(e) => setCronTime(e.target.value)}
                className="h-8 text-xs"
              />
              <p className="text-[11px] text-muted-foreground">{t("cards.settings.genTimeDesc")}</p>
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">{t("cards.settings.dailyLimit")}</Label>
              <Input
                type="number"
                min={1}
                max={50}
                value={dailyLimit}
                onChange={(e) => setDailyLimit(e.target.value)}
                className="h-8 text-xs"
              />
              <p className="text-[11px] text-muted-foreground">{t("cards.settings.dailyLimitDesc")}</p>
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">{t("cards.settings.reviewLimit")}</Label>
              <Input
                type="number"
                min={1}
                max={200}
                value={reviewLimit}
                onChange={(e) => setReviewLimit(e.target.value)}
                className="h-8 text-xs"
              />
              <p className="text-[11px] text-muted-foreground">{t("cards.settings.reviewLimitDesc")}</p>
            </div>
          </div>

          <div className="rounded-md border border-border/60 p-3">
            <div className="mb-2 flex items-center justify-between">
              <Label className="text-xs">{t("cards.settings.push")}</Label>
              <Switch checked={pushEnabled} onCheckedChange={setPushEnabled} />
            </div>
            {pushEnabled && (
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-1.5">
                  <Label className="text-xs">{t("cards.settings.pushTime")}</Label>
                  <Input
                    type="time"
                    value={pushTime}
                    onChange={(e) => setPushTime(e.target.value)}
                    className="h-8 text-xs"
                  />
                  <p className="text-[11px] text-muted-foreground">{t("cards.settings.pushTimeDesc")}</p>
                </div>
                <div className="space-y-1.5">
                  <Label className="text-xs">{t("cards.settings.pushChannel")}</Label>
                  <Select value={pushChannel} onValueChange={(v) => v && setPushChannel(v)}>
                    <SelectTrigger className="h-8 text-xs">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="wechat">WeChat</SelectItem>
                      <SelectItem value="qq">QQ</SelectItem>
                      <SelectItem value="telegram">Telegram</SelectItem>
                      <SelectItem value="discord">Discord</SelectItem>
                      <SelectItem value="slack">Slack</SelectItem>
                      <SelectItem value="feishu">Feishu</SelectItem>
                      <SelectItem value="line">LINE</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
              </div>
            )}
          </div>
        </div>
      )}

      <div className="flex justify-end pt-1">
        <SaveButton size="sm" onSave={handleSave} disabled={!configLoaded} />
      </div>
    </div>
  );
}
