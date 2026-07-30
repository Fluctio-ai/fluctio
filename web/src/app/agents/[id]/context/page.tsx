"use client";

import { useCallback, useEffect, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Brain, Check, Languages, Link2, MessageSquare, MessagesSquare, Puzzle, Archive } from "lucide-react";
import { getAgent, getAgentMemory, setAgentMemory, updateAgent, getCompactionPreview, type CompactionPreview } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";
import { useT } from "@/lib/i18n";

// Per-agent Context page — one knob (mode), one extension point (plugins).
//
// "Context" rather than "Tools" because the page is really about how
// the LLM's context window gets assembled: which framework sections
// participate in the system prompt AND which built-in tools come
// along. Prompt Mode picks both in one go. There's no per-agent
// allowlist anymore — what each mode includes is documented inline
// next to the dropdown; for the live tool list at runtime, look at
// the agent's chat session (tool calls in the transcript) or the
// /api/agents/{id}/tools/registered endpoint.

type PromptModeValue = "" | "agent" | "chatbot" | "customize";

const MODE_LABEL_KEY: Record<string, string> = {
  agent: "context.modeAgent",
  chatbot: "context.modeChatbot",
  customize: "context.modeCustomize",
};

type GuidanceValue = "autonomous" | "guided";

const GUIDANCE_LABEL_KEY: Record<GuidanceValue, string> = {
  guided: "context.guidanceGuided",
  autonomous: "context.guidanceAutonomous",
};

export default function AgentContextPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const t = useT();

  // "" = no override saved; runtime falls back to "agent".
  const [promptMode, setPromptMode] = useState<PromptModeValue>("");
  // guidance: "guided" (default, firm rules) vs "autonomous" (soft).
  // Backend "" is normalized to "guided" on load.
  const [guidance, setGuidance] = useState<GuidanceValue>("guided");
  // Per-agent multi-bubble toggle. Applies to every IM channel the
  // agent is bound to. False is the default; null on the wire is
  // treated as false here.
  const [splitReplies, setSplitReplies] = useState(false);
  const [splitRepliesSaving, setSplitRepliesSaving] = useState(false);
  // Per-agent auto-persist toggle. Off by default; null on the wire is
  // treated as false here. When on, every N turns the runtime fires an
  // LLM-driven distill pass that appends to USER.md / MEMORY.md.
  const [autoPersist, setAutoPersist] = useState(false);
  const [autoPersistSaving, setAutoPersistSaving] = useState(false);
  // Auto-title lives in memory.autoTitle (system-level), unlike the
  // agent-level toggles above. On = LLM summarises opening turns into
  // sessions.title.
  const [autoTitleEnabled, setAutoTitleEnabled] = useState(false);
  const [autoTitleModel, setAutoTitleModel] = useState("");
  const [autoTitleSaving, setAutoTitleSaving] = useState(false);
  const [sharedIdentity, setSharedIdentity] = useState(false);
  const [sharedIdentitySaving, setSharedIdentitySaving] = useState(false);
  // Per-agent default UI language for slash-command replies on IM
  // channels (web forwards its i18n locale per-request, IM can't).
  // "" = no override saved → runtime default (Chinese).
  const [language, setLanguage] = useState("");
  const [languageSaving, setLanguageSaving] = useState(false);
  // Compaction mode selector: "" = balanced (default), "conservative" /
  // "balanced" / "aggressive" = preset margins, "manual" = operator-set
  // fixed threshold. The radio value "manual" is a UI-only sentinel —
  // what actually gets saved is compactionMode="" + compactionThreshold>0.
  const [compactionPreview, setCompactionPreview] = useState<CompactionPreview | null>(null);
  const [compactionRadio, setCompactionRadio] = useState<string>("");
  const [compactionManual, setCompactionManual] = useState<string>("");
  const [compactionSaving, setCompactionSaving] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  const fetchAll = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      const agentRec = await getAgent(agentId).catch(() => null);
      const pm = agentRec?.promptMode || "";
      if (pm === "agent" || pm === "chatbot" || pm === "customize") {
        setPromptMode(pm);
      } else {
        setPromptMode("");
      }
      setGuidance(agentRec?.guidance === "autonomous" ? "autonomous" : "guided");
      setSplitReplies(agentRec?.splitReplies === true);
      setAutoPersist(agentRec?.autoPersist === true);
      setSharedIdentity(agentRec?.sharedIdentity === true);
      const mem = await getAgentMemory(agentId).catch(() => null);
      const at = mem?.memory?.autoTitle;
      setAutoTitleEnabled(at?.enabled === true);
      setAutoTitleModel(at?.model || "");
      const lang = (agentRec?.config?.language as string) || "";
      setLanguage(lang === "en" || lang === "zh-CN" ? lang : "");
      // Load compaction preview + derive the radio selection from the
      // saved state. If manualThreshold > 0, the "manual" radio is
      // selected and the input is pre-filled. Otherwise the saved mode
      // (or "" for balanced default) is selected.
      const cp = await getCompactionPreview(agentId).catch(() => null);
      setCompactionPreview(cp);
      if (cp) {
        if (cp.manualThreshold && cp.manualThreshold > 0) {
          setCompactionRadio("manual");
          setCompactionManual(String(cp.manualThreshold));
        } else {
          setCompactionRadio(cp.compactionMode || "balanced");
        }
      }
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  useEffect(() => {
    fetchAll();
  }, [fetchAll]);

  const flashSaved = () => {
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  };

  // Save memory.autoTitle (enabled + model). afterRounds / maxTries /
  // maxChars keep their defaults — only enabled + model are operator-
  // facing. Reads the current memory first so we don't clobber sibling
  // keys (wikiAutoGen, autoPersist, ...).
  const saveAutoTitle = async (patch: { enabled?: boolean; model?: string }) => {
    if (!agentId) return;
    const next = {
      enabled: patch.enabled ?? autoTitleEnabled,
      afterRounds: 3,
      model: patch.model ?? autoTitleModel,
    };
    setAutoTitleSaving(true);
    try {
      const cur = await getAgentMemory(agentId).catch(() => null);
      const base = cur?.memory || {};
      await setAgentMemory(agentId, { ...base, autoTitle: next });
      if (patch.enabled !== undefined) setAutoTitleEnabled(patch.enabled);
      if (patch.model !== undefined) setAutoTitleModel(patch.model);
      flashSaved();
    } finally {
      setAutoTitleSaving(false);
    }
  };

  const handlePromptModeChange = async (next: PromptModeValue) => {
    const prev = promptMode;
    setPromptMode(next);
    setSaving(true);
    try {
      await updateAgent(agentId, { promptMode: next });
      flashSaved();
    } catch {
      setPromptMode(prev);
    } finally {
      setSaving(false);
    }
  };

  const handleGuidanceChange = async (next: GuidanceValue) => {
    const prev = guidance;
    setGuidance(next);
    setSaving(true);
    try {
      await updateAgent(agentId, { guidance: next });
      flashSaved();
    } catch {
      setGuidance(prev);
    } finally {
      setSaving(false);
    }
  };

  // Optimistic toggle for splitReplies. No "inherit" state anymore —
  // system-level fallback was removed; false is the absolute default
  // when nothing is saved.
  const handleSplitRepliesChange = async (next: boolean) => {
    const prev = splitReplies;
    setSplitReplies(next);
    setSplitRepliesSaving(true);
    try {
      await updateAgent(agentId, { splitReplies: next });
      flashSaved();
    } catch {
      setSplitReplies(prev);
    } finally {
      setSplitRepliesSaving(false);
    }
  };

  // Optimistic toggle for autoPersist. Same shape as splitReplies; on
  // failure roll back. The runtime falls back to system default (off
  // in practice today, since the dead-code NewAgentWithFullCfg path
  // never gets called) when no per-agent override is saved.
  const handleAutoPersistChange = async (next: boolean) => {
    const prev = autoPersist;
    setAutoPersist(next);
    setAutoPersistSaving(true);
    try {
      await updateAgent(agentId, { autoPersist: next });
      flashSaved();
    } catch {
      setAutoPersist(prev);
    } finally {
      setAutoPersistSaving(false);
    }
  };

  const handleSharedIdentityChange = async (next: boolean) => {
    const prev = sharedIdentity;
    setSharedIdentity(next);
    setSharedIdentitySaving(true);
    try {
      await updateAgent(agentId, { sharedIdentity: next });
      flashSaved();
    } catch {
      setSharedIdentity(prev);
    } finally {
      setSharedIdentitySaving(false);
    }
  };

  // Reply language: "en" / "zh-CN". Drives slash-command reply language
  // on IM channels (web carries its own locale). Optimistic, rolls back
  // on failure — same shape as the toggles above.
  const handleLanguageChange = async (next: string) => {
    const prev = language;
    setLanguage(next);
    setLanguageSaving(true);
    try {
      await updateAgent(agentId, { language: next });
      flashSaved();
    } catch {
      setLanguage(prev);
    } finally {
      setLanguageSaving(false);
    }
  };

  // formatK renders a token count in a human-friendly way using the
  // "K" suffix, which is locale-neutral and readable in both EN/CJK.
  const formatK = (n: number): string => {
    if (n >= 1000) return `${Math.round(n / 1000)}K`;
    return String(n);
  };

  // handleCompactionRadioChange saves the mode immediately when a preset
  // is picked. "manual" is a UI-only sentinel that just expands the
  // input — no save happens until the user types a threshold.
  const handleCompactionRadioChange = async (next: string) => {
    if (next === compactionRadio) return;
    const prev = compactionRadio;
    setCompactionRadio(next);
    // "manual" doesn't save — just expands the input.
    if (next === "manual") return;
    setCompactionSaving(true);
    try {
      const mode = next === "balanced" ? "" : next;
      await updateAgent(agentId, { compactionMode: mode as "" | "conservative" | "balanced" | "aggressive", compactionThreshold: 0 });
      setCompactionManual("");
      flashSaved();
    } catch {
      setCompactionRadio(prev);
    } finally {
      setCompactionSaving(false);
    }
  };

  // handleCompactionManualSave fires on input blur — writes the fixed
  // threshold and clears the mode override. An empty input (NaN) is
  // treated as "no change" and skips the save entirely so we don't
  // clobber an already-saved mode with a 0 threshold.
  const handleCompactionManualSave = async () => {
    const parsed = parseInt(compactionManual, 10);
    if (isNaN(parsed)) return; // empty input — skip save
    const threshold = parsed < 0 ? 0 : parsed;
    setCompactionSaving(true);
    try {
      await updateAgent(agentId, { compactionMode: "", compactionThreshold: threshold });
      flashSaved();
    } catch {
      // Roll back is implicit — the radio stays where the user put it.
    } finally {
      setCompactionSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="p-6 space-y-6 max-w-5xl mx-auto">
        <Skeleton className="h-10 w-48" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">{t("context.title")}</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {t("context.subtitle", { name: agentName || t("context.thisAgent") })}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {saved && (
            <span className="inline-flex items-center gap-1.5 text-xs text-success">
              <Check className="h-3.5 w-3.5" /> {t("context.saved")}
            </span>
          )}
        </div>
      </div>

      {/* Prompt Mode */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between gap-2 mb-3">
          <div className="flex items-center gap-2">
            <MessageSquare className="h-4 w-4 text-primary" />
            <h3 className="font-medium">{t("context.promptMode")}</h3>
            {promptMode === "" || promptMode === "agent" ? (
              <Badge variant="outline" className="text-[10px]">
                {t("context.default")}
              </Badge>
            ) : (
              <Badge className="bg-primary/10 text-primary hover:bg-primary/10 text-[10px]">
                {t(MODE_LABEL_KEY[promptMode])}
              </Badge>
            )}
          </div>
        </div>
        <Select
          value={promptMode || "agent"}
          onValueChange={(v: string | null) => {
            if (v === "agent" || v === "chatbot" || v === "customize") {
              handlePromptModeChange(v);
            }
          }}
          disabled={saving}
        >
          <SelectTrigger className="text-sm max-w-[240px]">
            {/* Explicit children override SelectValue's auto-extraction
                from the active SelectItem — shadcn sometimes falls back
                to rendering the raw `value` string. */}
            <SelectValue>{t(MODE_LABEL_KEY[promptMode || "agent"])}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="agent">{t("context.modeAgent")}</SelectItem>
            <SelectItem value="chatbot">{t("context.modeChatbot")}</SelectItem>
            <SelectItem value="customize">{t("context.modeCustomize")}</SelectItem>
          </SelectContent>
        </Select>
        <div className="mt-3 text-xs text-muted-foreground space-y-1.5">
          <div>
            <strong>{t("context.modeAgent")}</strong> — {t("context.agentDesc")}
          </div>
          <div>
            <strong>{t("context.modeChatbot")}</strong> — {t("context.chatbotDescP1")}{" "}
            <code className="text-[10px]">image_gen</code>,{" "}
            <code className="text-[10px]">tts</code>,{" "}
            <code className="text-[10px]">write_file</code>,{" "}
            <code className="text-[10px]">edit_file</code>{" "}
            {t("context.chatbotDescP2")}{" "}
            <code className="text-[10px]">memory_search</code>{" "}
            {t("context.chatbotDescP3")}
          </div>
          <div>
            <strong>{t("context.modeCustomize")}</strong> — {t("context.customizeDesc")}
          </div>
        </div>
        <div className="mt-4 pt-3 border-t border-border flex items-start gap-2 text-xs text-muted-foreground">
          <Puzzle className="h-3.5 w-3.5 mt-0.5 shrink-0" />
          <span>
            {t("context.pluginToolsNote")}{" "}
            <code className="text-[11px]">
              ~/.fluctio/plugins/fluctio-plugin-demo
            </code>{" "}
            {t("context.pluginToolsExample")}
          </span>
        </div>
      </div>

      {/* Guidance: autonomous vs guided operational-constraint strength */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between gap-2 mb-3">
          <div className="flex items-center gap-2">
            <Brain className="h-4 w-4 text-primary" />
            <h3 className="font-medium">{t("context.guidance")}</h3>
            {guidance === "guided" ? (
              <Badge variant="outline" className="text-[10px]">
                {t("context.default")}
              </Badge>
            ) : (
              <Badge className="bg-primary/10 text-primary hover:bg-primary/10 text-[10px]">
                {t("context.guidanceAutonomous")}
              </Badge>
            )}
          </div>
        </div>
        <Select
          value={guidance}
          onValueChange={(v: string | null) => {
            if (v === "autonomous" || v === "guided") {
              handleGuidanceChange(v);
            }
          }}
          disabled={saving}
        >
          <SelectTrigger className="text-sm max-w-[240px]">
            <SelectValue>{t(GUIDANCE_LABEL_KEY[guidance])}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="guided">{t("context.guidanceGuided")}</SelectItem>
            <SelectItem value="autonomous">{t("context.guidanceAutonomous")}</SelectItem>
          </SelectContent>
        </Select>
      </div>

      {/* Reply language — IM channels can't forward the web client's
          i18n locale, so this per-agent default picks the language for
          slash-command replies (/usage /status /help …) there. */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between gap-2 mb-3">
          <div className="flex items-center gap-2">
            <Languages className="h-4 w-4 text-primary" />
            <h3 className="font-medium">{t("context.replyLanguage")}</h3>
          </div>
        </div>
        <Select
          value={language || "zh-CN"}
          onValueChange={(v: string | null) => {
            if (v === "zh-CN" || v === "en") {
              handleLanguageChange(v);
            }
          }}
          disabled={languageSaving}
        >
          <SelectTrigger className="text-sm max-w-[240px]">
            <SelectValue>
              {language === "en" ? t("context.langEn") : t("context.langZh")}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="zh-CN">{t("context.langZh")}</SelectItem>
            <SelectItem value="en">{t("context.langEn")}</SelectItem>
          </SelectContent>
        </Select>
        <p className="mt-3 text-xs text-muted-foreground">
          {t("context.replyLanguageDesc")}
        </p>
      </div>

      {/* Multi-bubble replies — applies to every IM channel. Lives here
          rather than in the Channels tab because it's a property of how
          the LLM communicates, not of the channel binding. */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <MessagesSquare className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <h3 className="font-medium">{t("context.multiBubble")}</h3>
              <p className="text-sm text-muted-foreground mt-1">
                {t("context.splitRepliesDesc")}
              </p>
            </div>
          </div>
          <Switch
            checked={splitReplies}
            onCheckedChange={handleSplitRepliesChange}
            disabled={splitRepliesSaving}
            aria-label={t("context.multiBubble")}
          />
        </div>
      </div>

      {/* Auto-remember chatter — lives here because it's about how the
          agent retains context across turns / sessions, parallel to how
          Multi-bubble is about how it emits replies. */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <Brain className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <h3 className="font-medium">{t("context.autoPersist")}</h3>
              <p className="text-sm text-muted-foreground mt-1">
                {t("context.autoPersistDescP1")}{" "}
                <code className="text-[10px]">write_file</code> /{" "}
                <code className="text-[10px]">edit_file</code>{" "}
                {t("context.autoPersistDescP2")}
              </p>
            </div>
          </div>
          <Switch
            checked={autoPersist}
            onCheckedChange={handleAutoPersistChange}
            disabled={autoPersistSaving}
            aria-label={t("context.autoPersist")}
          />
        </div>
      </div>

      {/* Auto-title: LLM summarises opening turns into sessions.title */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0 flex-1">
            <MessageSquare className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0 flex-1">
              <h3 className="font-medium">{t("context.autoTitle")}</h3>
              <p className="text-sm text-muted-foreground mt-1">{t("context.autoTitleDesc")}</p>
              {autoTitleEnabled && (
                <div className="mt-3 space-y-1">
                  <label className="text-xs text-muted-foreground">{t("context.autoTitleModel")}</label>
                  <Input
                    placeholder={t("context.autoTitleModelPlaceholder")}
                    value={autoTitleModel}
                    onChange={(e) => setAutoTitleModel(e.target.value)}
                    onBlur={() => saveAutoTitle({ model: autoTitleModel })}
                    disabled={autoTitleSaving}
                    className="h-8"
                  />
                  <p className="text-[11px] text-muted-foreground">{t("context.autoTitleModelHint")}</p>
                </div>
              )}
            </div>
          </div>
          <Switch
            checked={autoTitleEnabled}
            onCheckedChange={(v) => saveAutoTitle({ enabled: v })}
            disabled={autoTitleSaving}
            aria-label={t("context.autoTitle")}
          />
        </div>
      </div>

      {/* Shared identity across channels */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3 min-w-0">
            <Link2 className="h-4 w-4 text-primary mt-0.5 shrink-0" />
            <div className="min-w-0">
              <h3 className="font-medium">{t("context.sharedIdentity")}</h3>
              <p className="text-sm text-muted-foreground mt-1">
                {t("context.sharedIdentityDesc")}
              </p>
            </div>
          </div>
          <Switch
            checked={sharedIdentity}
            onCheckedChange={handleSharedIdentityChange}
            disabled={sharedIdentitySaving}
            aria-label={t("context.sharedIdentity")}
          />
        </div>
      </div>

      {/* Compaction threshold mode selector — three presets + custom.
          Each preset shows the estimated trigger threshold derived from
          the agent model's context window. When the model is off-table
          (contextWindow=0), thresholds are floored at 1000 and we show
          an "unknown window" hint instead of misleading large numbers. */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center gap-2 mb-3">
          <Archive className="h-4 w-4 text-primary" />
          <h3 className="font-medium">{t("context.compaction")}</h3>
        </div>
        <p className="text-sm text-muted-foreground mb-4">
          {t("context.compactionDesc")}
        </p>
        {compactionPreview?.contextWindow === 0 ? (
          <p className="text-xs text-muted-foreground italic">
            {t("context.compactionUnknownWindow")}
          </p>
        ) : (
          <div className="space-y-2">
            {([
              { value: "conservative", label: t("context.compactionConservative"), desc: t("context.compactionConservativeDesc") },
              { value: "balanced", label: t("context.compactionBalanced"), desc: t("context.compactionBalancedDesc") },
              { value: "aggressive", label: t("context.compactionAggressive"), desc: t("context.compactionAggressiveDesc") },
            ] as const).map((opt) => (
              <label
                key={opt.value}
                className={`flex items-start gap-3 p-3 rounded-md border cursor-pointer transition-colors ${
                  compactionRadio === opt.value
                    ? "border-primary bg-primary/5"
                    : "border-border hover:bg-muted/50"
                }`}
              >
                <input
                  type="radio"
                  name="compaction-mode"
                  value={opt.value}
                  checked={compactionRadio === opt.value}
                  onChange={() => handleCompactionRadioChange(opt.value)}
                  disabled={compactionSaving}
                  className="mt-0.5"
                />
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="text-sm font-medium">{opt.label}</span>
                    {opt.value === "balanced" && (compactionRadio === "" || compactionRadio === "balanced") && (
                      <Badge variant="outline" className="text-[10px]">
                        {t("context.compactionDefault")}
                      </Badge>
                    )}
                    {compactionPreview && (
                      <span className="text-xs text-muted-foreground">
                        {t("context.compactionEstimate", { tokens: formatK(compactionPreview.modes[opt.value]) })}
                      </span>
                    )}
                  </div>
                  <p className="text-xs text-muted-foreground mt-0.5">{opt.desc}</p>
                </div>
              </label>
            ))}
            {/* Manual threshold — UI-only "manual" sentinel radio.
                Selecting it expands the input; the actual save fires on
                blur with compactionThreshold>0 + compactionMode="". */}
            <label
              className={`flex items-start gap-3 p-3 rounded-md border cursor-pointer transition-colors ${
                compactionRadio === "manual"
                  ? "border-primary bg-primary/5"
                  : "border-border hover:bg-muted/50"
              }`}
            >
              <input
                type="radio"
                name="compaction-mode"
                value="manual"
                checked={compactionRadio === "manual"}
                onChange={() => handleCompactionRadioChange("manual")}
                disabled={compactionSaving}
                className="mt-0.5"
              />
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium">{t("context.compactionManual")}</span>
                  {compactionPreview?.manualThreshold && compactionPreview.manualThreshold > 0 && (
                    <span className="text-xs text-muted-foreground">
                      {t("context.compactionEstimate", { tokens: formatK(compactionPreview.manualThreshold) })}
                    </span>
                  )}
                </div>
                <p className="text-xs text-muted-foreground mt-0.5">{t("context.compactionManualDesc")}</p>
                {compactionRadio === "manual" && (
                  <div className="mt-2 space-y-1">
                    <Input
                      type="number"
                      placeholder={t("context.compactionManualPlaceholder")}
                      value={compactionManual}
                      onChange={(e) => setCompactionManual(e.target.value)}
                      onBlur={handleCompactionManualSave}
                      disabled={compactionSaving}
                      className="h-8 max-w-[200px]"
                    />
                    <p className="text-[11px] text-muted-foreground">{t("context.compactionManualHint")}</p>
                  </div>
                )}
              </div>
            </label>
          </div>
        )}
      </div>
    </div>
  );
}
