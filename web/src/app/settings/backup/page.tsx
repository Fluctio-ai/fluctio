"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Button } from "@/components/ui/button";
import { HardDrive, Database } from "lucide-react";
import { useT } from "@/lib/i18n";
import { SaveButton } from "@/components/save-button";
import { PageHeader, SettingsCard, CardHead, Field, NumberField } from "@/components/settings-ui";
import {
  apiFetch,
  getSystemBackup,
  setSystemBackup,
  listBackups,
  backupNow,
  deleteBackup,
  getMaintenanceStatus,
  startMaintenanceVacuum,
  type BackupConfig,
  type BackupInfo,
  type MaintenanceStatus,
  type MaintenanceDBStats,
} from "@/lib/api";

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(2)} MB`;
}

function formatTime(unix: number): string {
  return new Date(unix * 1000).toLocaleString();
}

// BackupSettingsPage — system-level scheduled SQLite backup config.
// Mirrors the gateway backup ticker: daily VACUUM INTO snapshot at
// cronTime (UTC+8), rotated to maxKeep. Plus a manual "back up now" +
// list/download/delete of existing snapshots.
export default function BackupSettingsPage() {
  const t = useT();
  const [enabled, setEnabled] = useState(false);
  const [cronTime, setCronTime] = useState("03:00");
  const [maxKeep, setMaxKeep] = useState(7);
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [items, setItems] = useState<BackupInfo[]>([]);
  const [toast, setToast] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    const res = await listBackups();
    setItems(res.backups ?? []);
  }, []);

  useEffect(() => {
    getSystemBackup()
      .then((res) => {
        const b = res.backup;
        if (b) {
          setEnabled(b.enabled ?? false);
          setCronTime(b.cronTime || "03:00");
          setMaxKeep(b.maxKeep ?? 7);
        }
        setLoaded(true);
      })
      .catch(() => setLoaded(true));
    refresh();
  }, [refresh]);

  const handleSave = useCallback(async () => {
    const cfg: BackupConfig = { enabled, cronTime, maxKeep };
    const res = await setSystemBackup(cfg);
    if (res?.error) throw new Error(res.error);
  }, [enabled, cronTime, maxKeep]);

  const handleNow = useCallback(async () => {
    setBusy(true);
    setToast(null);
    try {
      const res = await backupNow();
      if (res.error) {
        setToast(res.error);
        return;
      }
      setToast(t("backup.created"));
      await refresh();
    } finally {
      setBusy(false);
    }
  }, [refresh, t]);

  const handleDelete = useCallback(
    async (name: string) => {
      if (!window.confirm(t("backup.confirmDelete"))) return;
      const res = await deleteBackup(name);
      if (res.error) {
        setToast(res.error);
        return;
      }
      await refresh();
    },
    [refresh, t],
  );

  const handleDownload = useCallback(async (name: string) => {
    const res = await apiFetch(`/api/backup/download?file=${encodeURIComponent(name)}`);
    if (!res.ok) {
      setToast(`HTTP ${res.status}`);
      return;
    }
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  }, []);

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("backup.title")}
        desc={t("backup.desc")}
        actions={<SaveButton onSave={handleSave} disabled={!loaded} />}
      />

      <SettingsCard className="space-y-4">
        <CardHead
          icon={HardDrive}
          title={t("backup.enabled")}
          control={
            <Switch checked={enabled} onCheckedChange={setEnabled} disabled={!loaded} />
          }
        />
        <div className="grid grid-cols-2 gap-4">
          <Field label={t("backup.cronTime")} hint={t("backup.cronTimeDesc")}>
            <Input
              type="time"
              value={cronTime}
              onChange={(e) => setCronTime(e.target.value)}
            />
          </Field>
          <Field label={t("backup.maxKeep")} hint={t("backup.maxKeepDesc")}>
            <NumberField value={maxKeep} onChange={setMaxKeep} min={1} />
          </Field>
        </div>
      </SettingsCard>

      <SettingsCard>
        <CardHead
          title={t("backup.listTitle")}
          control={
            <Button variant="secondary" onClick={handleNow} disabled={busy}>
              {busy ? t("backup.busy") : t("backup.now")}
            </Button>
          }
        />
        {toast && <p className="mt-3 text-xs text-muted-foreground">{toast}</p>}
        {items.length === 0 ? (
          <p className="mt-4 text-sm text-muted-foreground">{t("backup.empty")}</p>
        ) : (
          <ul className="mt-4 divide-y divide-border">
            {items.map((b) => (
              <li key={b.name} className="flex items-center justify-between gap-3 py-2.5">
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium">{b.name}</p>
                  <p className="text-xs text-muted-foreground">
                    {formatSize(b.size)} · {formatTime(b.modified)}
                  </p>
                </div>
                <div className="flex shrink-0 gap-2">
                  <Button variant="ghost" onClick={() => handleDownload(b.name)}>
                    {t("backup.download")}
                  </Button>
                  <Button
                    variant="ghost"
                    className="text-destructive"
                    onClick={() => handleDelete(b.name)}
                  >
                    {t("backup.delete")}
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </SettingsCard>
      <MaintenanceCard />
    </div>
  );
}

// MaintenanceCard — online SQLite maintenance coordinator surface.
// Polls status every 2s while a run is active (waiting/backup/vacuum),
// scheduling the next poll only after the previous response lands (a
// stalled request must not stack up overlaps); otherwise one pull on
// mount. Shows honest stage + elapsed wall time — never a progress
// percentage, matching the backend contract.
const MAINTENANCE_STAGE_TEXT: Record<string, string> = {
  waiting: "maintenance.waiting",
  backup: "maintenance.backup",
  vacuum: "maintenance.vacuum",
  done: "maintenance.done",
  failed: "maintenance.failed",
};

function MaintenanceCard() {
  const t = useT();
  // One snapshot per response: three sequential setStates would render
  // torn states between them.
  const [snap, setSnap] = useState<{
    st: MaintenanceStatus;
    db: MaintenanceDBStats | null;
    turnsActive: boolean;
  } | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);

  const active = !!snap && ["waiting", "backup", "vacuum"].includes(snap.st.stage);

  const refresh = useCallback(async () => {
    const res = await getMaintenanceStatus();
    if (res.error) return;
    // db is omitted by the backend while a run is in flight; keep the
    // last known bloat picture rather than blanking it.
    const db = res.db ?? snapRef.current?.db ?? null;
    setSnap({ st: res.maintenance ?? { stage: "idle" }, db, turnsActive: !!res.turnsActive });
  }, []);

  // snapRef mirrors snap for refresh() without re-creating the callback.
  const snapRef = useRef(snap);
  useEffect(() => { snapRef.current = snap; }, [snap]);

  useEffect(() => { refresh(); }, [refresh]);

  useEffect(() => {
    if (!active) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      await refresh();
      if (!stopped) timer = setTimeout(tick, 2000);
    };
    tick();
    return () => { stopped = true; clearTimeout(timer); };
  }, [active, refresh]);

  const handleStart = useCallback(async () => {
    if (!window.confirm(t("maintenance.confirm"))) return;
    setStarting(true);
    setErr(null);
    try {
      const res = await startMaintenanceVacuum();
      if (res.error) setErr(res.error);
      await refresh();
    } finally {
      setStarting(false);
    }
  }, [refresh, t]);

  const st = snap?.st;
  const db = snap?.db;

  return (
    <SettingsCard>
      <CardHead
        icon={Database}
        title={t("maintenance.title")}
        control={
          <Button
            variant="secondary"
            onClick={handleStart}
            disabled={active || starting}
          >
            {active || starting
              ? `${t("maintenance.elapsed")} ${Math.floor(st?.elapsedSeconds ?? 0)}s`
              : t("maintenance.start")}
          </Button>
        }
      />
      <p className="mt-3 text-xs text-muted-foreground">{t("maintenance.desc")}</p>
      {err && <p className="mt-3 text-xs text-destructive">{err}</p>}
      {db && (
        <dl className="mt-4 grid grid-cols-3 gap-4 text-sm">
          <div>
            <dt className="text-xs text-muted-foreground">{t("maintenance.dbSize")}</dt>
            <dd className="font-medium">
              {formatSize(db.dbBytes ?? 0)}
              {db.walBytes ? ` (+${formatSize(db.walBytes)} WAL)` : ""}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">{t("maintenance.freeRatio")}</dt>
            <dd className="font-medium">
              {db.freeRatio !== undefined ? `${(db.freeRatio * 100).toFixed(1)}%` : "—"}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">{t("maintenance.eventsRows")}</dt>
            <dd className="font-medium">{(db.sessionEventsRows ?? 0).toLocaleString()}</dd>
          </div>
        </dl>
      )}
      {st && st.stage !== "idle" && (
        <div className="mt-4 space-y-1.5 border-t border-border pt-3 text-sm">
          <p className="font-medium">
            {MAINTENANCE_STAGE_TEXT[st.stage] ? t(MAINTENANCE_STAGE_TEXT[st.stage]) : st.stage}
          </p>
          {st.stage === "failed" && st.error && (
            <p className="text-xs text-destructive">{st.error}</p>
          )}
          {st.backupName && (
            <p className="text-xs text-muted-foreground">
              {t("maintenance.backupName")}: {st.backupName}
            </p>
          )}
          {st.sizeBefore !== undefined && st.sizeAfter !== undefined && (
            <p className="text-xs text-muted-foreground">
              {t("maintenance.sizeChange")}: {formatSize(st.sizeBefore)} → {formatSize(st.sizeAfter)}
            </p>
          )}
        </div>
      )}
      {st && st.stage === "idle" && (
        <p className="mt-3 text-xs text-muted-foreground">
          {snap?.turnsActive ? t("maintenance.turnsActive") : t("maintenance.idle")}
        </p>
      )}
    </SettingsCard>
  );
}
