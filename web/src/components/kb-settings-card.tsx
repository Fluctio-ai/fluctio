"use client";

import { useCallback, useEffect, useState } from "react";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  selectLabel,
} from "@/components/ui/select";
import { getAgentConfig, updateAgent } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";
import { SaveButton } from "@/components/save-button";
import { SettingsCard, CardHead, Field, GroupLabel, GroupHead, NumberField } from "@/components/settings-ui";
import { channelLabel } from "@/components/channel-icon";
import { BookOpen } from "lucide-react";

// KBSettingsCard — the KB auto-query configuration card. Lives in the
// Settings dialog's Knowledge tab. The data-source *list* is browsed
// from /knowledge/ instead; this card is only the retrieval behavior
// (enable, trigger mode, max results, wiki/concept ratio, threshold,
// keywords, search/no-result action) plus its own Save button.
export function KBSettingsCard() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const autoModeLabel = selectLabel({
    always: t("knowledge.modeAlways"),
    keyword: t("knowledge.modeKeyword"),
    disabled: t("knowledge.modeDisabled"),
  });
  const searchModeLabel = selectLabel({
    augment: t("knowledge.searchAugment"),
    strict: t("knowledge.searchStrict"),
  });
  const emptyActionLabel = selectLabel({
    llm: t("knowledge.actionLLM"),
    stop: t("knowledge.actionStop"),
  });
  const [kbEnabled, setKbEnabled] = useState(false);
  const [autoMode, setAutoMode] = useState("always");
  const [keywords, setKeywords] = useState("");
  const [maxResults, setMaxResults] = useState(5);
  const [searchMode, setSearchMode] = useState("augment");
  const [emptyAction, setEmptyAction] = useState("llm");
  const [wikiRatio, setWikiRatio] = useState(0.5);
  const [threshold, setThreshold] = useState(0.45);
  const [reminderChannel, setReminderChannel] = useState("wechat");
  const [articleDupHigh, setArticleDupHigh] = useState(0.90);
  const [articleDupMid, setArticleDupMid] = useState(0.72);
  const [flashDupThreshold, setFlashDupThreshold] = useState(0.85);
  const [todoDupThreshold, setTodoDupThreshold] = useState(0.78);
  const [ftEnabled, setFtEnabled] = useState(false);
  const [ftAutoMode, setFtAutoMode] = useState("disabled");
  const [ftKeywords, setFtKeywords] = useState("");
  const [ftMaxResults, setFtMaxResults] = useState(3);
  const [ftThreshold, setFtThreshold] = useState(0.6);
  const [configLoaded, setConfigLoaded] = useState(false);

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
          setReminderChannel(kb.reminderChannel || "wechat");
          setArticleDupHigh(kb.articleDupHigh ?? 0.90);
          setArticleDupMid(kb.articleDupMid ?? 0.72);
          setFlashDupThreshold(kb.flashDupThreshold ?? 0.85);
          setTodoDupThreshold(kb.todoDupThreshold ?? 0.78);
          setFtEnabled(kb.flashTodoEnabled ?? false);
          setFtAutoMode(kb.flashTodoAutoMode ?? "disabled");
          setFtKeywords((kb.flashTodoKeywords ?? []).join(", "));
          setFtMaxResults(kb.flashTodoMaxResults || 3);
          setFtThreshold(kb.flashTodoThreshold ?? 0.6);
        }
        setConfigLoaded(true);
      })
      .catch(() => {});
  }, [agentId]);

  const handleSave = useCallback(async () => {
    if (!agentId) return;
    const res = await updateAgent(agentId, {
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
        reminderChannel,
        articleDupHigh: articleDupHigh || undefined,
        articleDupMid: articleDupMid || undefined,
        flashDupThreshold: flashDupThreshold || undefined,
        todoDupThreshold: todoDupThreshold || undefined,
        flashTodoEnabled: ftEnabled,
        flashTodoAutoMode: ftAutoMode,
        flashTodoKeywords: ftKeywords
          .split(/[,\n]/)
          .map((s) => s.trim())
          .filter(Boolean),
        flashTodoMaxResults: ftMaxResults,
        flashTodoThreshold: ftThreshold,
      },
    } as any);
    if (res?.error) throw new Error(res.error);
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
    reminderChannel,
    articleDupHigh,
    articleDupMid,
    flashDupThreshold,
    todoDupThreshold,
    ftEnabled,
    ftAutoMode,
    ftKeywords,
    ftMaxResults,
    ftThreshold,
  ]);

  return (
    <SettingsCard className="space-y-4">
      <CardHead
        icon={BookOpen}
        title={t("knowledge.autoQuery")}
        desc={t("knowledge.autoQueryDesc")}
        control={
          <Switch
            checked={kbEnabled}
            onCheckedChange={setKbEnabled}
            disabled={!configLoaded}
          />
        }
      />

      {kbEnabled && (
        <div className="space-y-4 border-t border-border pt-4">
          <GroupLabel>{t("knowledge.wikiRecall")}</GroupLabel>
          <div className="grid grid-cols-2 gap-4">
            <Field label={t("knowledge.triggerMode")}>
              <Select value={autoMode} onValueChange={(v) => v && setAutoMode(v)}>
                <SelectTrigger>
                  <SelectValue>{autoModeLabel}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="always">{t("knowledge.modeAlways")}</SelectItem>
                  <SelectItem value="keyword">{t("knowledge.modeKeyword")}</SelectItem>
                  <SelectItem value="disabled">{t("knowledge.modeDisabled")}</SelectItem>
                </SelectContent>
              </Select>
            </Field>
            <Field label={t("knowledge.maxResults")}>
              <NumberField
                min={1}
                max={20}
                value={maxResults}
                onChange={setMaxResults}
              />
            </Field>
          </div>

          {/* Keywords input lives right under the trigger mode that
              enables it — it used to render at the card tail, far from
              the Wiki 触发模式 select that shows it. */}
          {autoMode === "keyword" && (
            <Field label={t("knowledge.keywords")}>
              <Input
                value={keywords}
                onChange={(e) => setKeywords(e.target.value)}
                placeholder={t("knowledge.keywordsPlaceholder")}
              />
            </Field>
          )}

          <Field
            label={t("knowledge.wikiRatio")}
            hint={t("knowledge.wikiRatioDesc")}
            labelTrailing={
              <>
                {t("knowledge.sourceLabel")} {Math.round(wikiRatio * 100)}% ·{" "}
                {t("knowledge.conceptLabel")} {100 - Math.round(wikiRatio * 100)}%
              </>
            }
          >
            <input
              type="range"
              min={0}
              max={100}
              step={10}
              value={Math.round(wikiRatio * 100)}
              onChange={(e) => setWikiRatio(Number(e.target.value) / 100)}
              className="w-full accent-primary"
            />
          </Field>

          <Field
            label={t("knowledge.threshold")}
            hint={t("knowledge.thresholdDesc")}
            labelTrailing={threshold.toFixed(2)}
          >
            <input
              type="range"
              min={0}
              max={1}
              step={0.01}
              value={threshold}
              onChange={(e) => setThreshold(Number(e.target.value))}
              className="w-full accent-primary"
            />
          </Field>

          <div className="space-y-4 border-t border-border pt-4">
            <GroupHead
              title={t("knowledge.flashRecall")}
              desc={t("knowledge.flashRecallDesc")}
              control={
                <Switch checked={ftEnabled} onCheckedChange={setFtEnabled} />
              }
            />
            {ftEnabled && (
              <>
                <div className="grid grid-cols-2 gap-4">
                  <Field label={t("knowledge.triggerMode")}>
                    <Select value={ftAutoMode} onValueChange={(v) => v && setFtAutoMode(v)}>
                      <SelectTrigger>
                        <SelectValue>{autoModeLabel}</SelectValue>
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="always">{t("knowledge.modeAlways")}</SelectItem>
                        <SelectItem value="keyword">{t("knowledge.modeKeyword")}</SelectItem>
                        <SelectItem value="disabled">{t("knowledge.modeDisabled")}</SelectItem>
                      </SelectContent>
                    </Select>
                  </Field>
                  <Field label={t("knowledge.maxResults")}>
                    <NumberField
                      min={1}
                      max={20}
                      value={ftMaxResults}
                      onChange={setFtMaxResults}
                    />
                  </Field>
                </div>
                <Field
                  label={t("knowledge.threshold")}
                  hint={t("knowledge.ftThresholdDesc")}
                  labelTrailing={ftThreshold.toFixed(2)}
                >
                  <input
                    type="range"
                    min={0}
                    max={1}
                    step={0.01}
                    value={ftThreshold}
                    onChange={(e) => setFtThreshold(Number(e.target.value))}
                    className="w-full accent-primary"
                  />
                </Field>
                {ftAutoMode === "keyword" && (
                  <Field label={t("knowledge.keywords")}>
                    <Input
                      value={ftKeywords}
                      onChange={(e) => setFtKeywords(e.target.value)}
                      placeholder={t("knowledge.keywordsPlaceholder")}
                    />
                  </Field>
                )}
              </>
            )}
          </div>

          {/* Search behavior + todo reminders each get their own labeled
              group — they used to float unlabeled after the dedup block
              and read as dedup sub-fields. */}
          <div className="space-y-4 border-t border-border pt-4">
            <GroupLabel>{t("knowledge.dedupThresholds")}</GroupLabel>
            <div className="grid grid-cols-2 gap-4">
              <Field label={<span className="text-xs">{t("knowledge.dedupArticleHigh")}</span>}>
                <NumberField min={0} max={1} value={articleDupHigh} onChange={setArticleDupHigh} />
              </Field>
              <Field label={<span className="text-xs">{t("knowledge.dedupArticleMid")}</span>}>
                <NumberField min={0} max={1} value={articleDupMid} onChange={setArticleDupMid} />
              </Field>
              <Field label={<span className="text-xs">{t("knowledge.dedupFlash")}</span>}>
                <NumberField min={0} max={1} value={flashDupThreshold} onChange={setFlashDupThreshold} />
              </Field>
              <Field label={<span className="text-xs">{t("knowledge.dedupTodo")}</span>}>
                <NumberField min={0} max={1} value={todoDupThreshold} onChange={setTodoDupThreshold} />
              </Field>
            </div>
          </div>

          <div className="space-y-4 border-t border-border pt-4">
            <GroupLabel>{t("knowledge.searchBehavior")}</GroupLabel>
            <div className="grid grid-cols-2 gap-4">
              <Field label={t("knowledge.searchMode")}>
                <Select value={searchMode} onValueChange={(v) => v && setSearchMode(v)}>
                  <SelectTrigger>
                    <SelectValue>{searchModeLabel}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="augment">{t("knowledge.searchAugment")}</SelectItem>
                    <SelectItem value="strict">{t("knowledge.searchStrict")}</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
              <Field label={t("knowledge.noResultAction")}>
                <Select value={emptyAction} onValueChange={(v) => v && setEmptyAction(v)}>
                  <SelectTrigger>
                    <SelectValue>{emptyActionLabel}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="llm">{t("knowledge.actionLLM")}</SelectItem>
                    <SelectItem value="stop">{t("knowledge.actionStop")}</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
            </div>
          </div>

          <div className="space-y-4 border-t border-border pt-4">
            <GroupLabel>{t("knowledge.todoReminders")}</GroupLabel>
            <Field label={t("knowledge.reminderChannel")} hint={t("knowledge.reminderChannelDesc")}>
              <Select value={reminderChannel} onValueChange={(v) => v && setReminderChannel(v)}>
                <SelectTrigger>
                  <SelectValue>{(v: unknown) => channelLabel(v as string)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="wechat">WeChat</SelectItem>
                  <SelectItem value="qq">QQ</SelectItem>
                  <SelectItem value="telegram">Telegram</SelectItem>
                  <SelectItem value="discord">Discord</SelectItem>
                  <SelectItem value="slack">Slack</SelectItem>
                  <SelectItem value="feishu">Feishu</SelectItem>
                  <SelectItem value="line">LINE</SelectItem>
                </SelectContent>
              </Select>
            </Field>
          </div>
        </div>
      )}

      <div className="flex justify-end border-t border-border pt-4">
        <SaveButton onSave={handleSave} disabled={!configLoaded} />
      </div>
    </SettingsCard>
  );
}
