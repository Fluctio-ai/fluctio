"use client";

import { useCallback, useEffect, useState } from "react";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Button } from "@/components/ui/button";
import { HardDrive } from "lucide-react";
import { useT } from "@/lib/i18n";
import { SaveButton } from "@/components/save-button";
import { PageHeader, SettingsCard, CardHead, Field } from "@/components/settings-ui";
import {
  apiFetch,
  getSystemBackup,
  setSystemBackup,
  listBackups,
  backupNow,
  deleteBackup,
  type BackupConfig,
  type BackupInfo,
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
            <Input
              type="number"
              min={1}
              value={maxKeep}
              onChange={(e) => setMaxKeep(Math.max(1, Number(e.target.value) || 1))}
            />
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
    </div>
  );
}
