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
import { Brain, Check, Languages, Link2, MessageSquare, MessagesSquare, Puzzle } from "lucide-react";
import { getAgent, getAgentMemory, setAgentMemory, updateAgent } from "@/lib/api";
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

export default function AgentContextPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const t = useT();

  // "" = no override saved; runtime falls back to "agent".
  const [promptMode, setPromptMode] = useState<PromptModeValue>("");
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
      setSplitReplies(agentRec?.splitReplies === true);
      setAutoPersist(agentRec?.autoPersist === true);
      setSharedIdentity(agentRec?.sharedIdentity === true);
      const mem = await getAgentMemory(agentId).catch(() => null);
      const at = mem?.memory?.autoTitle;
      setAutoTitleEnabled(at?.enabled === true);
      setAutoTitleModel(at?.model || "");
      const lang = (agentRec?.config?.language as string) || "";
      setLanguage(lang === "en" || lang === "zh-CN" ? lang : "");
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
    </div>
  );
}
