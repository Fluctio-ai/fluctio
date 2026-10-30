"use client";

import { useEffect, useState } from "react";
import { ShieldAlert } from "lucide-react";
import { useT } from "@/lib/i18n";
import { getConfig, updateConfig, type ConfigResponse } from "@/lib/api";
import { useAutoSave } from "@/hooks/use-auto-save";
import {
  SettingsCard,
  CardHead,
  Field,
  DecimalInput,
  SaveStatus,
} from "@/components/settings-ui";

// FailureRetryCard — runtime tuning for the failure-recovery watchdogs
// (quota-breaker probe cadence + failure-retry sweep backoff/cap). Written
// to the system "failure" setting row; the sweep re-reads it every tick
// and the breaker on every trip, so changes land without a restart.
// Fields are the settings dialect's string-state numeric form: empty =
// fall back to the env knob / built-in default (placeholder shows it).
type FailureWire = NonNullable<ConfigResponse["failure"]>;
type FailureDraft = { quotaProbeMin: string; backoffBaseMin: string; maxAttempts: string };

const EMPTY: FailureDraft = { quotaProbeMin: "", backoffBaseMin: "", maxAttempts: "" };
const DEFAULTS = { quotaProbeMin: "30", backoffBaseMin: "15", maxAttempts: "3" };

// toDraft/toPayload convert between the wire shape (ints, omitted when
// unset) and the edit shape (strings, "" = inherit).
function toDraft(f: FailureWire): FailureDraft {
  const s = (v?: number) => (v && v > 0 ? String(v) : "");
  return { quotaProbeMin: s(f.quotaProbeMin), backoffBaseMin: s(f.backoffBaseMin), maxAttempts: s(f.maxAttempts) };
}
function toPayload(d: FailureDraft): FailureWire {
  const n = (s: string) => {
    const v = Number(s);
    return s.trim() !== "" && Number.isFinite(v) && v > 0 ? Math.round(v) : undefined;
  };
  const out: FailureWire = {};
  const q = n(d.quotaProbeMin), b = n(d.backoffBaseMin), m = n(d.maxAttempts);
  if (q) out.quotaProbeMin = q;
  if (b) out.backoffBaseMin = b;
  if (m) out.maxAttempts = m;
  return out;
}

export function FailureRetryCard() {
  const t = useT();
  const [draft, setDraft] = useState<FailureDraft>(EMPTY);
  // Last accepted draft — the rollback target after a failed save.
  const [good, setGood] = useState<FailureDraft>(EMPTY);
  const [loaded, setLoaded] = useState(false);

  const { failed, saved, wrap } = useAutoSave({
    loaded,
    save: async () => {
      const res = await updateConfig({ failure: toPayload(draft) });
      if (!res.ok) throw new Error(res.error || "save failed");
    },
    reload: () => setDraft(good),
    onSaved: () => setGood(draft),
  });

  useEffect(() => {
    getConfig()
      .then((c) => {
        const d = toDraft(c.failure || {});
        setDraft(d);
        setGood(d);
        setLoaded(true);
      })
      .catch(() => setLoaded(true));
  }, []);

  const field = (key: keyof FailureDraft, def: string, label: string) => (
    <DecimalInput
      value={draft[key]}
      onChange={wrap((s: string) => setDraft((p) => ({ ...p, [key]: s.replace(/[^\d]/g, "") })))}
      placeholder={def}
      maxLength={4}
      aria-label={label}
    />
  );

  return (
    <SettingsCard className="space-y-4">
      <CardHead
        icon={ShieldAlert}
        title={t("failure.title")}
        desc={t("failure.desc")}
        control={
          <SaveStatus
            failed={failed}
            saved={saved}
            failedLabel={t("common.saveFailed")}
            savedLabel={t("common.saved")}
          />
        }
      />
      <div className="grid gap-4 sm:grid-cols-3">
        <Field label={t("failure.quotaProbe")} hint={t("failure.quotaProbeHint")}>
          {field("quotaProbeMin", DEFAULTS.quotaProbeMin, t("failure.quotaProbe"))}
        </Field>
        <Field label={t("failure.backoffBase")} hint={t("failure.backoffBaseHint")}>
          {field("backoffBaseMin", DEFAULTS.backoffBaseMin, t("failure.backoffBase"))}
        </Field>
        <Field label={t("failure.maxAttempts")} hint={t("failure.maxAttemptsHint")}>
          {field("maxAttempts", DEFAULTS.maxAttempts, t("failure.maxAttempts"))}
        </Field>
      </div>
    </SettingsCard>
  );
}
