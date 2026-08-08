"use client";

import { useCallback, useEffect, useState } from "react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Button } from "@/components/ui/button";
import { useT } from "@/lib/i18n";
import { SaveButton } from "@/components/save-button";
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
      <div>
        <h3 className="text-xl font-semibold tracking-tight">{t("backup.title")}</h3>
        <p className="text-sm text-muted-foreground mt-1">{t("backup.desc")}</p>
      </div>

      <div className="space-y-3 rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between">
          <div className="space-y-1">
            <Label className="text-sm font-medium">{t("backup.enabled")}</Label>
          </div>
          <Switch checked={enabled} onCheckedChange={setEnabled} disabled={!loaded} />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1.5">
            <Label className="text-xs">{t("backup.cronTime")}</Label>
            <Input
              type="time"
              value={cronTime}
              onChange={(e) => setCronTime(e.target.value)}
              className="h-8 text-xs"
            />
            <p className="text-[11px] text-muted-foreground">{t("backup.cronTimeDesc")}</p>
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs">{t("backup.maxKeep")}</Label>
            <Input
              type="number"
              min={1}
              value={maxKeep}
              onChange={(e) => setMaxKeep(Math.max(1, Number(e.target.value) || 1))}
              className="h-8 text-xs"
            />
            <p className="text-[11px] text-muted-foreground">{t("backup.maxKeepDesc")}</p>
          </div>
        </div>
        <div className="flex justify-end pt-1">
          <SaveButton size="sm" onSave={handleSave} disabled={!loaded} />
        </div>
      </div>

      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between">
          <h4 className="font-medium">{t("backup.listTitle")}</h4>
          <Button size="sm" variant="secondary" onClick={handleNow} disabled={busy}>
            {busy ? t("backup.busy") : t("backup.now")}
          </Button>
        </div>
        {toast && <p className="mt-3 text-xs text-muted-foreground">{toast}</p>}
        {items.length === 0 ? (
          <p className="mt-4 text-sm text-muted-foreground">{t("backup.empty")}</p>
        ) : (
          <ul className="mt-4 divide-y divide-border">
            {items.map((b) => (
              <li key={b.name} className="flex items-center justify-between gap-3 py-2.5">
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium">{b.name}</p>
                  <p className="text-[11px] text-muted-foreground">
                    {formatSize(b.size)} · {formatTime(b.modified)}
                  </p>
                </div>
                <div className="flex shrink-0 gap-2">
                  <Button size="sm" variant="ghost" onClick={() => handleDownload(b.name)}>
                    {t("backup.download")}
                  </Button>
                  <Button
                    size="sm"
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
      </div>
    </div>
  );
}
