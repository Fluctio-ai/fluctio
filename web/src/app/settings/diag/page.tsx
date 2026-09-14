"use client";

import { useEffect, useState } from "react";
import { Download, FileText, Loader2, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  deleteDiagReport,
  diagReportDownloadUrl,
  generateDiagReport,
  listDiagReports,
  type DiagReportEntry,
} from "@/lib/api";
import { useT } from "@/lib/i18n";
import { PageHeader, SettingsCard, CardHead, Field, SettingsError } from "@/components/settings-ui";

// DiagReportPage is the manual error-report generator: it pulls recent failed
// LLM calls from llm_call_diag, has the default agent's LLM compose a
// structured Markdown report, and lists/downloads past reports. Backend lives
// at POST/GET /api/diag/reports (setup/handlers_diag.go).
export default function DiagReportPage() {
  const t = useT();
  const [days, setDays] = useState(3);
  const [agentId, setAgentId] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const [reports, setReports] = useState<DiagReportEntry[]>([]);
  const [deleting, setDeleting] = useState<string | null>(null);

  const refresh = () => {
    listDiagReports()
      .then(setReports)
      .catch(() => setReports([]));
  };
  useEffect(refresh, []);

  const generate = async () => {
    setBusy(true);
    setError(null);
    setDone(false);
    try {
      const opts: { days: number; agentId?: string } = { days };
      if (agentId.trim()) opts.agentId = agentId.trim();
      await generateDiagReport(opts);
      setDone(true);
      refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (name: string) => {
    if (!window.confirm(t("diag.confirmDelete"))) return;
    setDeleting(name);
    setError(null);
    try {
      await deleteDiagReport(name);
      refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setDeleting(null);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader title={t("diag.title")} desc={t("diag.desc")} />

      <SettingsCard className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-[10rem_1fr_auto] sm:items-end">
          <Field label={t("diag.days")}>
            <Input
              type="number"
              min={1}
              max={30}
              value={days}
              onChange={(e) => setDays(Math.max(1, Number(e.target.value) || 3))}
              disabled={busy}
            />
          </Field>
          <Field label={t("diag.agentFilter")}>
            <Input
              type="text"
              placeholder={t("diag.agentFilterPlaceholder")}
              value={agentId}
              onChange={(e) => setAgentId(e.target.value)}
              disabled={busy}
            />
          </Field>
          <Button onClick={generate} disabled={busy}>
            {busy ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <FileText className="h-4 w-4" />
            )}
            {busy ? t("diag.generating") : t("diag.generate")}
          </Button>
        </div>
        {error && <SettingsError>{error}</SettingsError>}
        {done && !error && (
          <p className="text-sm text-muted-foreground">{t("diag.generated")}</p>
        )}
        <p className="text-xs text-muted-foreground">{t("diag.note")}</p>
      </SettingsCard>

      <SettingsCard>
        <CardHead icon={FileText} title={t("diag.history")} />
        {reports.length === 0 ? (
          <p className="mt-4 text-sm text-muted-foreground">{t("diag.empty")}</p>
        ) : (
          <ul className="mt-4 divide-y divide-border">
            {reports.map((r) => (
              <li
                key={r.name}
                className="flex items-center justify-between gap-3 py-2.5"
              >
                <div className="min-w-0">
                  <p className="text-sm font-mono truncate">{r.name}</p>
                  <p className="text-xs text-muted-foreground">
                    {new Date(r.time).toLocaleString()} ·{" "}
                    {Math.max(1, Math.round(r.size / 1024))} KB
                  </p>
                </div>
                <div className="flex items-center gap-2">
                  <Button
                    variant="outline"
                    onClick={() =>
                      window.open(
                        diagReportDownloadUrl(r.name),
                        "_blank",
                        "noopener,noreferrer",
                      )
                    }
                  >
                    <Download className="h-4 w-4" />
                    {t("diag.download")}
                  </Button>
                  <Button
                    variant="ghost"
                    onClick={() => remove(r.name)}
                    disabled={deleting === r.name}
                    className="text-muted-foreground hover:text-destructive"
                  >
                    {deleting === r.name ? (
                      <Loader2 className="h-4 w-4 animate-spin" />
                    ) : (
                      <Trash2 className="h-4 w-4" />
                    )}
                    {t("diag.delete")}
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
