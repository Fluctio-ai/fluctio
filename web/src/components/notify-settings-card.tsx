"use client";

import { useCallback, useEffect, useState } from "react";
import { getAgentConfig, updateAgent } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import { useAutoSave } from "@/hooks/use-auto-save";
import { SettingsCard, CardHead, GroupLabel, ToggleRow, SaveStatus } from "@/components/settings-ui";
import { BellRing } from "lucide-react";

// The toggle set, grouped as rendered. `key` doubles as the config field
// name — adding a kind means one entry here plus the backend gate.
const CONVERSATION_KINDS = [
  { key: "chatReply", label: "notify.chatReply", hint: "notify.chatReplyDesc" },
  { key: "imReply", label: "notify.imReply", hint: "notify.imReplyDesc" },
] as const;
const SWEEP_KINDS = [
  { key: "todoReminder", label: "notify.todoReminder", hint: "notify.todoReminderDesc" },
  { key: "cardsDigest", label: "notify.cardsDigest", hint: "notify.cardsDigestDesc" },
  { key: "shelvedNudge", label: "notify.shelvedNudge", hint: "notify.shelvedNudgeDesc" },
  { key: "blindspot", label: "notify.blindspot", hint: "notify.blindspotDesc" },
] as const;

type Kind = "chatReply" | "imReply" | "todoReminder" | "cardsDigest" | "shelvedNudge" | "blindspot";

const ALL_ON: Record<Kind, boolean> = {
  chatReply: true,
  imReply: true,
  todoReminder: true,
  cardsDigest: true,
  shelvedNudge: true,
  blindspot: true,
};

// NotifySettingsCard — which events reach the mobile notification queue
// (the Android app's direct-delivery channel; no FCM). Lives in the
// Settings dialog's Knowledge tab. Every switch is ON by default: the
// channel's designed behaviour is to deliver, and these exist to mute a
// noisy kind, not to opt in. Auto-saves like the rest of the dialog.
export function NotifySettingsCard() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [enabled, setEnabled] = useState<Record<Kind, boolean>>(ALL_ON);
  const [configLoaded, setConfigLoaded] = useState(false);

  const loadConfig = useCallback(
    () =>
      !agentId
        ? Promise.resolve()
        : getAgentConfig(agentId)
            .then((cfg) => {
              const n = cfg.notifications;
              if (n) {
                const next = { ...ALL_ON };
                for (const k of [...CONVERSATION_KINDS, ...SWEEP_KINDS]) {
                  next[k.key] = n[k.key] ?? true;
                }
                setEnabled(next);
              }
              setConfigLoaded(true);
            })
            .catch(() => {}),
    [agentId],
  );

  useEffect(() => {
    void loadConfig();
  }, [loadConfig]);

  const handleSave = useCallback(async () => {
    if (!agentId) return;
    // Whole-replace blob, same contract as kb/diary/cards: send every
    // field so an omitted one can't silently fall back to ON.
    const res = await updateAgent(agentId, { notifications: { ...enabled } });
    if (res?.error) throw new Error(res.error);
  }, [agentId, enabled]);

  const { failed, saved, wrap } = useAutoSave({
    loaded: configLoaded,
    save: handleSave,
    reload: loadConfig,
  });

  const rows = (kinds: readonly { key: Kind; label: string; hint: string }[]) =>
    kinds.map((k) => (
      <ToggleRow
        key={k.key}
        title={t(k.label)}
        hint={t(k.hint)}
        checked={enabled[k.key]}
        onCheckedChange={(v) => wrap(setEnabled)({ ...enabled, [k.key]: v })}
      />
    ));

  return (
    <SettingsCard className="space-y-4">
      <CardHead
        icon={BellRing}
        title={t("notify.title")}
        desc={t("notify.desc")}
        control={
          <SaveStatus
            failed={failed}
            saved={saved}
            failedLabel={t("common.saveFailed")}
            savedLabel={t("common.saved")}
          />
        }
      />
      <div className="space-y-4 border-t border-border pt-4">
        <GroupLabel>{t("notify.conversations")}</GroupLabel>
        {rows(CONVERSATION_KINDS)}
      </div>
      <div className="space-y-4 border-t border-border pt-4">
        <GroupLabel>{t("notify.sweeps")}</GroupLabel>
        {rows(SWEEP_KINDS)}
      </div>
    </SettingsCard>
  );
}
