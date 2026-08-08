"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import { Globe, RefreshCwIcon, SparklesIcon } from "lucide-react";
import {
  type KBSource,
  type WikiAutoGenCfg,
  type WikiAutogenStatus,
  generateWiki,
  getAgentMemory,
  getWikiAutogenStatus,
  getWikiProgress,
  listKBSources,
  setAgentMemory,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";

// WikiAutoGenSettingsCard — wiki background generation config plus the
// manual generate actions. Lives in the Settings dialog's Knowledge tab
// (merged under the same "Knowledge" section as KBSettingsCard). The
// wiki *browser* at /wiki/ is display-only.
//
// Self-contained: loads memory.wikiAutoGen + KB sources, polls auto-gen
// status while enabled, and polls generation progress while a run is in
// flight. The done-transition does NOT refresh a wiki list from here
// (the browser page re-fetches on visit), so the card only resets its
// own progress state.
export function WikiAutoGenSettingsCard() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [wikiCfg, setWikiCfg] = useState<WikiAutoGenCfg>({ enabled: false });
  const [wikiSaving, setWikiSaving] = useState(false);
  const wikiSavingRef = useRef(false);
  const [autogenStatus, setAutogenStatus] = useState<WikiAutogenStatus | null>(
    null,
  );
  const [generating, setGenerating] = useState(false);
  const [progress, setProgress] = useState<{
    done: number;
    total: number;
    status: string;
  } | null>(null);
  const [kbSources, setKbSources] = useState<KBSource[]>([]);

  // Load auto-gen config + KB source list (sources drive the generate
  // buttons' enabled state and the unprocessed count).
  useEffect(() => {
    if (!agentId) return;
    getAgentMemory(agentId)
      .then((m) => setWikiCfg(m.memory?.wikiAutoGen || { enabled: false }))
      .catch(() => {});
    listKBSources(agentId).then(setKbSources).catch(() => {});
  }, [agentId]);

  // Poll auto-gen status while enabled so the status line reflects the
  // last sweep.
  useEffect(() => {
    if (!agentId || !wikiCfg.enabled) {
      setAutogenStatus(null);
      return;
    }
    let cancelled = false;
    const fetchStatus = () =>
      getWikiAutogenStatus(agentId)
        .then((s) => {
          if (!cancelled) setAutogenStatus(s);
        })
        .catch(() => {});
    fetchStatus();
    const id = setInterval(fetchStatus, 10000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [agentId, wikiCfg.enabled]);

  const saveWikiCfg = async (next: WikiAutoGenCfg) => {
    if (wikiSavingRef.current) return;
    wikiSavingRef.current = true;
    setWikiCfg(next);
    setWikiSaving(true);
    try {
      const cur = await getAgentMemory(agentId).catch(() => null);
      const base = cur?.memory || {};
      await setAgentMemory(agentId, { ...base, wikiAutoGen: next });
    } finally {
      setWikiSaving(false);
      wikiSavingRef.current = false;
    }
  };

  const handleGenerate = useCallback(
    async (force?: boolean) => {
      if (!agentId || kbSources.length === 0) return;
      setGenerating(true);
      setProgress({ done: 0, total: kbSources.length, status: "running" });
      try {
        await generateWiki(
          agentId,
          kbSources.map((s) => s.id),
          force,
        );
      } catch {
        setGenerating(false);
        setProgress(null);
      }
    },
    [agentId, kbSources],
  );

  // Poll generation progress while a run is in flight.
  useEffect(() => {
    if (!agentId || !generating) return;
    let cancelled = false;
    const poll = async () => {
      try {
        const p = await getWikiProgress(agentId);
        if (cancelled) return;
        if (p.status === "idle") return;
        setProgress({
          done: p.done ?? 0,
          total: p.total ?? 0,
          status: p.status,
        });
        if (p.status === "done") {
          setGenerating(false);
          // Refresh KB sources so the "unprocessed" count / disabled
          // state updates after a run completes.
          listKBSources(agentId).then(setKbSources).catch(() => {});
        }
      } catch {}
    };
    poll();
    const id = setInterval(poll, 2000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [agentId, generating]);

  const handleForceGenerate = useCallback(() => {
    if (!window.confirm(t("wiki.forceRegenConfirm"))) return;
    handleGenerate(true);
  }, [handleGenerate, t]);

  const unprocessedCount = kbSources.filter((s) => !s.wiki_generated_at).length;

  return (
    <div className="space-y-3 rounded-lg border border-border bg-card p-5">
      <div>
        <div className="flex items-center gap-2 mb-1">
          <Globe className="h-4 w-4 text-primary" />
          <h3 className="font-medium">{t("wiki.autoGen")}</h3>
        </div>
        <p className="text-sm text-muted-foreground mb-3">{t("wiki.autoGenHint")}</p>
        <Switch
          checked={wikiCfg.enabled}
          onCheckedChange={(v) => saveWikiCfg({ ...wikiCfg, enabled: v })}
          disabled={wikiSaving}
        />
      </div>

      {wikiCfg.enabled && (
        <div className="space-y-2 pt-1">
          <div className="flex items-center justify-between gap-2">
            <label className="text-xs text-muted-foreground">
              {t("wiki.autoGenInterval")}
            </label>
            <div className="flex items-center gap-1.5">
              <Input
                type="number"
                min={1}
                className="w-16 h-7 text-xs"
                value={
                  wikiCfg.interval
                    ? Math.round(wikiCfg.interval / 3600000000000)
                    : 6
                }
                onChange={(e) => {
                  const hours = Math.max(1, Number(e.target.value) || 6);
                  saveWikiCfg({ ...wikiCfg, interval: hours * 3600000000000 });
                }}
                disabled={wikiSaving}
              />
              <span className="text-xs text-muted-foreground">
                {t("wiki.autoGenHours")}
              </span>
            </div>
          </div>
          <div className="flex items-center justify-between gap-2">
            <label className="text-xs text-muted-foreground">
              {t("wiki.autoGenMaxTokens")}
            </label>
            <Input
              type="number"
              min={0}
              step={512}
              className="w-20 h-7 text-xs"
              value={
                wikiCfg.maxTokens && wikiCfg.maxTokens > 0
                  ? wikiCfg.maxTokens
                  : 8192
              }
              onChange={(e) => {
                const v = Math.max(0, Number(e.target.value) || 0);
                saveWikiCfg({ ...wikiCfg, maxTokens: v });
              }}
              disabled={wikiSaving}
            />
          </div>
          {autogenStatus && (
            <div className="text-[11px] leading-tight text-muted-foreground space-y-0.5 pt-1">
              <div>
                {t("wiki.autoGenLastRun")}:{" "}
                {autogenStatus.last_run
                  ? new Date(autogenStatus.last_run).toLocaleString()
                  : t("wiki.autoGenNever")}
              </div>
              {autogenStatus.last_status &&
                autogenStatus.last_status !== "ok" &&
                autogenStatus.last_status !== "no_sources" && (
                  <div className="text-warning">
                    {t(`wiki.autoGenStatus.${autogenStatus.last_status}`)}
                    {autogenStatus.last_error
                      ? ` — ${autogenStatus.last_error}`
                      : ""}
                  </div>
                )}
              {typeof autogenStatus.pending === "number" &&
                autogenStatus.pending > 0 && (
                  <div>
                    {t("wiki.autoGenPending", { n: autogenStatus.pending })}
                  </div>
                )}
            </div>
          )}
        </div>
      )}

      <Separator />

      {/* Manual generation actions */}
      <div className="space-y-2">
        {generating && progress && (
          <span className="text-xs text-muted-foreground flex items-center gap-1">
            <RefreshCwIcon className="h-3 w-3 animate-spin" />
            {t("wiki.generatingProgress", {
              done: progress.done,
              total: progress.total,
            })}
          </span>
        )}
        <div className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            size="sm"
            className="h-8 text-xs"
            onClick={() => handleGenerate()}
            disabled={generating || unprocessedCount === 0}
            title={
              unprocessedCount === 0
                ? t("wiki.allProcessed")
                : t("wiki.generateUnprocessed")
            }
          >
            <SparklesIcon className="h-3.5 w-3.5 mr-1" />
            {t("wiki.generateUnprocessed")}
          </Button>
          <Button
            variant="outline"
            size="sm"
            className="h-8 text-xs"
            onClick={handleForceGenerate}
            disabled={generating || kbSources.length === 0}
            title={t("wiki.forceRegenAll")}
          >
            <RefreshCwIcon className="h-3.5 w-3.5 mr-1" />
            {t("wiki.forceRegenAll")}
          </Button>
        </div>
      </div>
    </div>
  );
}
