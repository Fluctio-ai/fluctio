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
  selectLabel,
} from "@/components/ui/select";
import { getAgentConfig, updateAgent } from "@/lib/api";
import { channelLabel, PUSH_CHANNELS } from "@/components/channel-icon";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import { useAutoSave } from "@/hooks/use-auto-save";
import { SettingsCard, CardHead, Field, GroupHead, SaveStatus } from "@/components/settings-ui";

// DiarySettingsCard — daily-diary generation config. Lives in the
// Settings dialog's Knowledge tab next to KBSettingsCard. When enabled,
// the backend sweeps this agent's conversation_summaries once per day
// at cronTime (UTC+8) and distills a themed diary plus a "you might
// have missed" blindspot section.
export function DiarySettingsCard() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const thinkingModeLabel = selectLabel({
    off: t("diary.modeOff"),
    blindspots: t("diary.modeBlindspots"),
    deep: t("diary.modeDeep"),
  });
  const [enabled, setEnabled] = useState(false);
  const [cronTime, setCronTime] = useState("02:30");
  const [thinkingMode, setThinkingMode] = useState("blindspots");
  const [shelvedEnabled, setShelvedEnabled] = useState(false);
  const [shelvedPushEnabled, setShelvedPushEnabled] = useState(false);
  const [shelvedPushTime, setShelvedPushTime] = useState("09:00");
  const [shelvedPushChannel, setShelvedPushChannel] = useState("wechat");
  const [blindspotPushEnabled, setBlindspotPushEnabled] = useState(false);
  const [blindspotPushTime, setBlindspotPushTime] = useState("09:00");
  const [blindspotPushChannel, setBlindspotPushChannel] = useState("wechat");
  const [configLoaded, setConfigLoaded] = useState(false);

  const loadConfig = useCallback(
    () =>
      !agentId
        ? Promise.resolve()
        : getAgentConfig(agentId)
            .then((cfg) => {
              const d = cfg.diary;
              if (d) {
                setEnabled(d.enabled ?? false);
                setCronTime(d.cronTime || "02:30");
                setThinkingMode(d.thinkingMode || "blindspots");
                setShelvedEnabled(d.shelvedEnabled ?? false);
                setShelvedPushEnabled(d.shelvedPushEnabled ?? false);
                setShelvedPushTime(d.shelvedPushTime || "09:00");
                setShelvedPushChannel(d.shelvedPushChannel || "wechat");
                setBlindspotPushEnabled(d.blindspotPushEnabled ?? false);
                setBlindspotPushTime(d.blindspotPushTime || "09:00");
                setBlindspotPushChannel(d.blindspotPushChannel || "wechat");
              }
              setConfigLoaded(true);
            })
            // Keep the card disabled when the config can't be fetched.
            .catch(() => {}),
    [agentId],
  );

  useEffect(() => {
    void loadConfig();
  }, [loadConfig]);

  const handleSave = useCallback(async () => {
    if (!agentId) return;
    const res = await updateAgent(agentId, {
      diary: {
        enabled,
        cronTime,
        thinkingMode,
        shelvedEnabled,
        shelvedPushEnabled,
        shelvedPushTime,
        shelvedPushChannel,
        blindspotPushEnabled,
        blindspotPushTime,
        blindspotPushChannel,
      },
    } as any);
    if (res?.error) throw new Error(res.error);
  }, [agentId, enabled, cronTime, thinkingMode, shelvedEnabled, shelvedPushEnabled, shelvedPushTime, shelvedPushChannel, blindspotPushEnabled, blindspotPushTime, blindspotPushChannel]);

  const { failed, saved, wrap } = useAutoSave({
    loaded: configLoaded,
    save: handleSave,
    reload: loadConfig,
  });

  return (
    <SettingsCard className="space-y-4">
      <CardHead
        icon={Calendar}
        title={t("diary.title")}
        desc={t("diary.desc")}
        control={
          <div className="flex items-center gap-2">
            <SaveStatus
              failed={failed}
              saved={saved}
              failedLabel={t("common.saveFailed")}
              savedLabel={t("common.saved")}
            />
            <Switch checked={enabled} onCheckedChange={wrap(setEnabled)} disabled={!configLoaded} />
          </div>
        }
      />

      {enabled && (
        <div className="space-y-4 border-t border-border pt-4">
          <div className="grid grid-cols-2 gap-4">
            <Field label={t("diary.cronTime")} hint={t("diary.cronTimeDesc")}>
              <Input
                type="time"
                value={cronTime}
                onChange={(e) => wrap(setCronTime)(e.target.value)}
              />
            </Field>
            <Field label={t("diary.thinkingMode")} hint={t("diary.thinkingModeDesc")}>
              <Select value={thinkingMode} onValueChange={(v) => v && wrap(setThinkingMode)(v)}>
                <SelectTrigger>
                  <SelectValue>{thinkingModeLabel}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="off">{t("diary.modeOff")}</SelectItem>
                  <SelectItem value="blindspots">{t("diary.modeBlindspots")}</SelectItem>
                  <SelectItem value="deep">{t("diary.modeDeep")}</SelectItem>
                </SelectContent>
              </Select>
            </Field>
          </div>

          {/* Shelved-topics ledger — opt-in (it tracks the user's own
              "I'll get back to this" promises, so nothing runs implicitly).
              Push group mirrors the cards digest shape. */}
          <div className="space-y-4 border-t border-border pt-4">
            <GroupHead
              title={t("diary.shelvedTitle")}
              desc={t("diary.shelvedDesc")}
              control={
                <Switch checked={shelvedEnabled} onCheckedChange={wrap(setShelvedEnabled)} />
              }
            />
            {shelvedEnabled && (
              <div className="space-y-4">
                <GroupHead
                  title={t("diary.shelvedPush")}
                  desc={t("diary.shelvedPushDesc")}
                  control={
                    <Switch checked={shelvedPushEnabled} onCheckedChange={wrap(setShelvedPushEnabled)} />
                  }
                />
                {shelvedPushEnabled && (
                  <div className="grid grid-cols-2 gap-4">
                    <Field label={t("diary.shelvedPushTime")} hint={t("diary.shelvedPushTimeDesc")}>
                      <Input
                        type="time"
                        value={shelvedPushTime}
                        onChange={(e) => wrap(setShelvedPushTime)(e.target.value)}
                      />
                    </Field>
                    <Field label={t("diary.shelvedPushChannel")}>
                      <Select value={shelvedPushChannel} onValueChange={(v) => v && wrap(setShelvedPushChannel)(v)}>
                        <SelectTrigger>
                          <SelectValue>{(v: unknown) => channelLabel(v as string)}</SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          {PUSH_CHANNELS.map((c) => (
                            <SelectItem key={c.value} value={c.value}>
                              {c.label}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    </Field>
                  </div>
                )}
              </div>
            )}
          </div>

          {/* Blindspot nudge — amplifies the diary's "you might have
              missed" section; an empty list (the common case by design)
              pushes nothing. */}
          <div className="space-y-4 border-t border-border pt-4">
            <GroupHead
              title={t("diary.blindspotPush")}
              desc={t("diary.blindspotPushDesc")}
              control={
                <Switch checked={blindspotPushEnabled} onCheckedChange={wrap(setBlindspotPushEnabled)} />
              }
            />
            {blindspotPushEnabled && (
              <div className="grid grid-cols-2 gap-4">
                <Field label={t("diary.blindspotPushTime")} hint={t("diary.blindspotPushTimeDesc")}>
                  <Input
                    type="time"
                    value={blindspotPushTime}
                    onChange={(e) => wrap(setBlindspotPushTime)(e.target.value)}
                  />
                </Field>
                <Field label={t("diary.blindspotPushChannel")}>
                  <Select value={blindspotPushChannel} onValueChange={(v) => v && wrap(setBlindspotPushChannel)(v)}>
                    <SelectTrigger>
                      <SelectValue>{(v: unknown) => channelLabel(v as string)}</SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      {PUSH_CHANNELS.map((c) => (
                        <SelectItem key={c.value} value={c.value}>
                          {c.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </Field>
              </div>
            )}
          </div>
        </div>
      )}
    </SettingsCard>
  );
}
