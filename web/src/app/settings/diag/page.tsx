"use client";

import { useEffect, useState } from "react";
import { Download, FileText, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  diagReportDownloadUrl,
  generateDiagReport,
  listDiagReports,
  type DiagReportEntry,
} from "@/lib/api";
import { useT } from "@/lib/i18n";

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

  return (
    <div className="space-y-6">
      <div>
        <h3 className="text-xl font-semibold tracking-tight">{t("diag.title")}</h3>
        <p className="text-sm text-muted-foreground mt-1">{t("diag.desc")}</p>
      </div>

      <div className="rounded-lg border border-border bg-card p-5 space-y-4">
        <div className="flex flex-wrap items-end gap-3">
          <div className="space-y-1.5">
            <label className="text-xs text-muted-foreground">{t("diag.days")}</label>
            <Input
              type="number"
              min={1}
              max={30}
              value={days}
              onChange={(e) => setDays(Math.max(1, Number(e.target.value) || 3))}
              className="w-24"
              disabled={busy}
            />
          </div>
          <div className="space-y-1.5">
            <label className="text-xs text-muted-foreground">{t("diag.agentFilter")}</label>
            <Input
              type="text"
              placeholder={t("diag.agentFilterPlaceholder")}
              value={agentId}
              onChange={(e) => setAgentId(e.target.value)}
              className="w-64"
              disabled={busy}
            />
          </div>
          <Button onClick={generate} disabled={busy}>
            {busy ? (
              <Loader2 className="h-4 w-4 mr-2 animate-spin" />
            ) : (
              <FileText className="h-4 w-4 mr-2" />
            )}
            {busy ? t("diag.generating") : t("diag.generate")}
          </Button>
        </div>
        {error && <p className="text-sm text-destructive">{error}</p>}
        {done && !error && (
          <p className="text-sm text-muted-foreground">{t("diag.generated")}</p>
        )}
        <p className="text-xs text-muted-foreground">{t("diag.note")}</p>
      </div>

      <div className="space-y-2">
        <h4 className="text-sm font-medium">{t("diag.history")}</h4>
        {reports.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("diag.empty")}</p>
        ) : (
          <ul className="divide-y rounded-lg border">
            {reports.map((r) => (
              <li
                key={r.name}
                className="flex items-center justify-between gap-3 px-4 py-2.5"
              >
                <div className="min-w-0">
                  <p className="text-sm font-mono truncate">{r.name}</p>
                  <p className="text-xs text-muted-foreground">
                    {new Date(r.time).toLocaleString()} ·{" "}
                    {Math.max(1, Math.round(r.size / 1024))} KB
                  </p>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() =>
                    window.open(
                      diagReportDownloadUrl(r.name),
                      "_blank",
                      "noopener,noreferrer",
                    )
                  }
                >
                  <Download className="h-4 w-4 mr-1.5" />
                  {t("diag.download")}
                </Button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
