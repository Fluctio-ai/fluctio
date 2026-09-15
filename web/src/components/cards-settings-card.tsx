"use client";

import { useCallback, useEffect, useState } from "react";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Layers } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { channelLabel } from "@/components/channel-icon";
import { getAgentConfig, updateAgent } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import { SaveButton } from "@/components/save-button";
import { SettingsCard, CardHead, Field, GroupHead, NumberField } from "@/components/settings-ui";

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
  const [dailyLimit, setDailyLimit] = useState(10);
  const [reviewLimit, setReviewLimit] = useState(20);
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
          setDailyLimit(c.dailyLimit || 10);
          setReviewLimit(c.reviewLimit || 20);
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
        dailyLimit: dailyLimit || 10,
        reviewLimit: reviewLimit || 20,
        pushEnabled,
        pushTime,
        pushChannel,
      },
    });
    if (res?.error) throw new Error(res.error);
  }, [agentId, enabled, cronTime, dailyLimit, reviewLimit, pushEnabled, pushTime, pushChannel]);

  return (
    <SettingsCard className="space-y-4">
      <CardHead
        icon={Layers}
        title={t("cards.settings.title")}
        desc={t("cards.settings.desc")}
        control={
          <Switch checked={enabled} onCheckedChange={setEnabled} disabled={!configLoaded} />
        }
      />

      {enabled && (
        <div className="space-y-4 border-t border-border pt-4">
          <div className="grid grid-cols-2 gap-4">
            <Field label={t("cards.settings.genTime")} hint={t("cards.settings.genTimeDesc")}>
              <Input
                type="time"
                value={cronTime}
                onChange={(e) => setCronTime(e.target.value)}
              />
            </Field>
            <Field label={t("cards.settings.dailyLimit")} hint={t("cards.settings.dailyLimitDesc")}>
              <NumberField
                min={1}
                max={50}
                value={dailyLimit}
                onChange={setDailyLimit}
              />
            </Field>
            <Field label={t("cards.settings.reviewLimit")} hint={t("cards.settings.reviewLimitDesc")}>
              <NumberField
                min={1}
                max={200}
                value={reviewLimit}
                onChange={setReviewLimit}
              />
            </Field>
          </div>

          {/* Push digest — border-t group (same shape as the KB card's
              flash-recall group), no nested inset box. */}
          <div className="space-y-4 border-t border-border pt-4">
            <GroupHead
              title={t("cards.settings.push")}
              control={
                <Switch checked={pushEnabled} onCheckedChange={setPushEnabled} />
              }
            />
            {pushEnabled && (
              <div className="grid grid-cols-2 gap-4">
                <Field label={t("cards.settings.pushTime")} hint={t("cards.settings.pushTimeDesc")}>
                  <Input
                    type="time"
                    value={pushTime}
                    onChange={(e) => setPushTime(e.target.value)}
                  />
                </Field>
                <Field label={t("cards.settings.pushChannel")}>
                  <Select value={pushChannel} onValueChange={(v) => v && setPushChannel(v)}>
                    <SelectTrigger>
                      <SelectValue>{(v: unknown) => channelLabel(v as string)}</SelectValue>
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
                </Field>
              </div>
            )}
          </div>
        </div>
      )}

      <div className="flex justify-end border-t border-border pt-4">
        <SaveButton onSave={handleSave} disabled={!configLoaded} />
      </div>
    </SettingsCard>
  );
}
