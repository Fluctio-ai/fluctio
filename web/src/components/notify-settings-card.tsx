"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { getAgentConfig, updateAgent } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import { useAutoSave } from "@/hooks/use-auto-save";
import { SettingsCard, CardHead, GroupLabel, ToggleRow, SaveStatus } from "@/components/settings-ui";
import { cn } from "@/lib/utils";
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
  { key: "llmFailure", label: "notify.llmFailure", hint: "notify.llmFailureDesc" },
] as const;

type Kind = "chatReply" | "imReply" | "todoReminder" | "cardsDigest" | "shelvedNudge" | "blindspot" | "llmFailure";

const ALL_ON: Record<Kind, boolean> = {
  chatReply: true,
  imReply: true,
  todoReminder: true,
  cardsDigest: true,
  shelvedNudge: true,
  blindspot: true,
  llmFailure: true,
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
  // Which agentId's config is loaded / whose load failed — flags are
  // derived per agent so switching agents instantly re-arms the gate
  // (useAutoSave won't persist until the new agent's config lands) with
  // no synchronous reset needed in the load effect.
  const [loadedFor, setLoadedFor] = useState<string | null>(null);
  const [loadFailedFor, setLoadFailedFor] = useState<string | null>(null);
  const configLoaded = !!agentId && loadedFor === agentId;
  // Load-failure surfacing: swallowing the error used to leave loaded=false
  // forever, so every toggle only changed local state and never persisted
  // — with no indication that anything was wrong.
  const loadFailed = !!agentId && loadFailedFor === agentId;
  // Latest agentId — a config response that arrives after the user
  // switched agents must be discarded, or the stale values overwrite the
  // toggles and the next autosave writes the previous agent's
  // notification blob into the new agent (whole-replace contract).
  const agentIdRef = useRef(agentId);
  useEffect(() => {
    agentIdRef.current = agentId;
  }, [agentId]);

  const loadConfig = useCallback(() => {
    if (!agentId) return Promise.resolve();
    const requestedFor = agentId;
    return getAgentConfig(agentId)
      .then((cfg) => {
        if (agentIdRef.current !== requestedFor) return; // stale — discard
        const n = cfg.notifications;
        if (n) {
          const next = { ...ALL_ON };
          for (const k of [...CONVERSATION_KINDS, ...SWEEP_KINDS]) {
            next[k.key] = n[k.key] ?? true;
          }
          setEnabled(next);
        }
        setLoadedFor(requestedFor);
        setLoadFailedFor(null);
      })
      .catch(() => {
        if (agentIdRef.current !== requestedFor) return;
        setLoadFailedFor(requestedFor);
      });
  }, [agentId]);

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
      {loadFailed ? (
        // Fail closed on load failure: show the error instead of toggles
        // whose edits would silently never persist. English copy — no
        // generic load-failure i18n key exists in the locales.
        <div className="flex items-center justify-between gap-3 rounded-md border border-destructive/40 bg-destructive/5 px-4 py-3 text-sm text-destructive">
          <span>Failed to load configuration — edits are disabled until it loads.</span>
          <button
            type="button"
            onClick={() => void loadConfig()}
            className="text-xs font-medium underline underline-offset-2"
          >
            Retry
          </button>
        </div>
      ) : (
        <>
          <div
            className={cn(
              "space-y-4 border-t border-border pt-4",
              !configLoaded && "pointer-events-none opacity-60",
            )}
          >
            <GroupLabel>{t("notify.conversations")}</GroupLabel>
            {rows(CONVERSATION_KINDS)}
          </div>
          <div
            className={cn(
              "space-y-4 border-t border-border pt-4",
              !configLoaded && "pointer-events-none opacity-60",
            )}
          >
            <GroupLabel>{t("notify.sweeps")}</GroupLabel>
            {rows(SWEEP_KINDS)}
          </div>
        </>
      )}
    </SettingsCard>
  );
}
