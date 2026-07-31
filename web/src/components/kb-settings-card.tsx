"use client";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { getAgentConfig, updateAgent } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";

// KBSettingsCard — the KB auto-query configuration card. Lives in the
// Settings dialog's Knowledge tab. The data-source *list* is browsed
// from /knowledge/ instead; this card is only the retrieval behavior
// (enable, trigger mode, max results, wiki/concept ratio, threshold,
// keywords, search/no-result action) plus its own Save button.
export function KBSettingsCard() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const [kbEnabled, setKbEnabled] = useState(false);
  const [autoMode, setAutoMode] = useState("always");
  const [keywords, setKeywords] = useState("");
  const [maxResults, setMaxResults] = useState(5);
  const [searchMode, setSearchMode] = useState("augment");
  const [emptyAction, setEmptyAction] = useState("llm");
  const [wikiRatio, setWikiRatio] = useState(0.5);
  const [threshold, setThreshold] = useState(0.45);
  const [configLoaded, setConfigLoaded] = useState(false);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (!agentId) return;
    getAgentConfig(agentId)
      .then((cfg) => {
        const kb = cfg.kb;
        if (kb) {
          setKbEnabled(kb.enabled ?? false);
          setAutoMode(kb.autoMode ?? "always");
          setKeywords((kb.keywords ?? []).join(", "));
          setMaxResults(kb.maxResults || 5);
          setSearchMode(kb.searchMode ?? "augment");
          setEmptyAction(kb.emptyAction ?? "llm");
          setWikiRatio(kb.wikiRatio ?? 0.5);
          setThreshold(kb.threshold ?? 0.45);
        }
        setConfigLoaded(true);
      })
      .catch(() => {});
  }, [agentId]);

  const handleSave = useCallback(async () => {
    if (!agentId) return;
    setSaving(true);
    try {
      await updateAgent(agentId, {
        kb: {
          enabled: kbEnabled,
          autoMode,
          keywords: keywords
            .split(/[,\n]/)
            .map((s) => s.trim())
            .filter(Boolean),
          maxResults,
          searchMode,
          emptyAction,
          wikiRatio,
          threshold,
        },
      } as any);
    } catch {}
    setSaving(false);
  }, [
    agentId,
    kbEnabled,
    autoMode,
    keywords,
    maxResults,
    searchMode,
    emptyAction,
    wikiRatio,
    threshold,
  ]);

  return (
    <div className="space-y-3 rounded-lg border border-border bg-card p-5">
      <div className="flex items-center justify-between">
        <div className="space-y-1">
          <Label className="text-sm font-medium">{t("knowledge.autoQuery")}</Label>
          <p className="text-xs text-muted-foreground">
            {t("knowledge.autoQueryDesc")}
          </p>
        </div>
        <Switch
          checked={kbEnabled}
          onCheckedChange={setKbEnabled}
          disabled={!configLoaded}
        />
      </div>

      {kbEnabled && (
        <div className="space-y-3 pt-1">
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label className="text-xs">{t("knowledge.triggerMode")}</Label>
              <Select value={autoMode} onValueChange={(v) => v && setAutoMode(v)}>
                <SelectTrigger className="h-8 text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="always">{t("knowledge.modeAlways")}</SelectItem>
                  <SelectItem value="keyword">{t("knowledge.modeKeyword")}</SelectItem>
                  <SelectItem value="disabled">{t("knowledge.modeDisabled")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">{t("knowledge.maxResults")}</Label>
              <Input
                type="number"
                min={1}
                max={20}
                value={maxResults}
                onChange={(e) => setMaxResults(Number(e.target.value))}
                className="h-8 text-xs"
              />
            </div>
          </div>

          <div className="space-y-1.5">
            <div className="flex items-center justify-between">
              <Label className="text-xs">{t("knowledge.wikiRatio")}</Label>
              <span className="text-xs text-muted-foreground tabular-nums">
                {t("knowledge.sourceLabel")} {Math.round(wikiRatio * 100)}% ·{" "}
                {t("knowledge.conceptLabel")} {100 - Math.round(wikiRatio * 100)}%
              </span>
            </div>
            <input
              type="range"
              min={0}
              max={100}
              step={10}
              value={Math.round(wikiRatio * 100)}
              onChange={(e) => setWikiRatio(Number(e.target.value) / 100)}
              className="w-full accent-primary"
            />
            <p className="text-[11px] text-muted-foreground">
              {t("knowledge.wikiRatioDesc")}
            </p>
          </div>

          <div className="space-y-1.5">
            <div className="flex items-center justify-between">
              <Label className="text-xs">{t("knowledge.threshold")}</Label>
              <span className="text-xs text-muted-foreground tabular-nums">
                {threshold.toFixed(2)}
              </span>
            </div>
            <input
              type="range"
              min={0}
              max={1}
              step={0.01}
              value={threshold}
              onChange={(e) => setThreshold(Number(e.target.value))}
              className="w-full accent-primary"
            />
            <p className="text-[11px] text-muted-foreground">
              {t("knowledge.thresholdDesc")}
            </p>
          </div>

          {autoMode === "keyword" && (
            <div className="space-y-1.5">
              <Label className="text-xs">{t("knowledge.keywords")}</Label>
              <Input
                value={keywords}
                onChange={(e) => setKeywords(e.target.value)}
                placeholder={t("knowledge.keywordsPlaceholder")}
                className="h-8 text-xs"
              />
            </div>
          )}

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label className="text-xs">{t("knowledge.searchMode")}</Label>
              <Select value={searchMode} onValueChange={(v) => v && setSearchMode(v)}>
                <SelectTrigger className="h-8 text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="augment">{t("knowledge.searchAugment")}</SelectItem>
                  <SelectItem value="strict">{t("knowledge.searchStrict")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">{t("knowledge.noResultAction")}</Label>
              <Select value={emptyAction} onValueChange={(v) => v && setEmptyAction(v)}>
                <SelectTrigger className="h-8 text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="llm">{t("knowledge.actionLLM")}</SelectItem>
                  <SelectItem value="stop">{t("knowledge.actionStop")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
        </div>
      )}

      <div className="flex justify-end pt-1">
        <Button size="sm" onClick={handleSave} disabled={saving || !configLoaded}>
          {saving ? t("common.saving") : t("common.save")}
        </Button>
      </div>
    </div>
  );
}
