"use client";

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { SaveButton } from "@/components/save-button";
import { Skeleton } from "@/components/ui/skeleton";
import { Save, Check, Loader2, RotateCcw } from "lucide-react";
import { apiFetch } from "@/lib/api";
import { useT } from "@/lib/i18n";

import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

const CUSTOMIZE_FILES = [
  { name: "SOUL.md", label: "customize.tab.soul" },
  { name: "IDENTITY.md", label: "customize.tab.identity" },
  { name: "USER.md", label: "customize.tab.user" },
  { name: "TOOLS.md", label: "customize.tab.tools" },
  { name: "BOOTSTRAP.md", label: "customize.tab.bootstrap" },
  { name: "HEARTBEAT.md", label: "customize.tab.heartbeat" },
  { name: "MEMORY.md", label: "customize.tab.memory" },
  { name: "AGENTS.md", label: "customize.tab.agents" },
];

// FileState mirrors the backend's GET response: `content` is what's
// effectively loaded, `source` says where it came from, and `baseContent`
// (only set when source==="db" with a different owner row to revert to)
// is what the user would fall back to on Revert.
//
//   - "db":      the caller's own per-user override row (USER.md /
//                MEMORY.md only) — distinct from the owner's content.
//   - "owner":   the agent owner's row, the canonical "shared template"
//                — what identity files (SOUL/IDENTITY/BOOTSTRAP/...)
//                always render as, and what per-user files fall back to.
//   - "fs":      legacy filesystem default. Kept for back-compat.
//   - "default": neither caller nor owner row exists; tab is empty.
type FileSource = "db" | "owner" | "fs" | "default";
type FileState = { content: string; source: FileSource; baseContent?: string };

export default function AgentCustomizePage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const t = useT();
  const [activeTab, setActiveTab] = useState("SOUL.md");
  const [files, setFiles] = useState<Record<string, FileState>>({});
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  const loadAll = async () => {
    const entries = await Promise.all(
      CUSTOMIZE_FILES.map(async (f) => {
        try {
          const res = await apiFetch(`/api/agents/${agentId}/system-files/${f.name}`);
          if (res.ok) {
            const data = await res.json();
            return [
              f.name,
              {
                content: data.content || "",
                source: (data.source || "default") as FileSource,
                baseContent: data.baseContent,
              },
            ] as [string, FileState];
          }
        } catch {}
        return [f.name, { content: "", source: "default" as FileSource }] as [string, FileState];
      })
    );
    setFiles(Object.fromEntries(entries));
  };

  useEffect(() => {
    setLoading(true);
    loadAll().then(() => setLoading(false));
  }, [agentId]);

  const active = files[activeTab];

  const handleSave = async () => {
    const res = await apiFetch(`/api/agents/${agentId}/system-files/${activeTab}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: active?.content || "" }),
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    // Reload so source/baseContent stay accurate after save.
    loadAll();
  };

  // Revert deletes the DB override so the runtime falls back to the FS base
  // shipped with the agent definition. Only meaningful when source==="db"
  // AND a baseContent exists (otherwise the tab just becomes empty).
  const handleRevert = async () => {
    if (!active || active.source !== "db") return;
    if (!confirm(t("customize.revertConfirm", { file: activeTab }))) return;
    setSaving(true);
    try {
      await apiFetch(`/api/agents/${agentId}/system-files/${activeTab}`, {
        method: "DELETE",
      });
      await loadAll();
    } catch {}
    setSaving(false);
  };

  if (loading) {
    return (
      <div className="p-6 space-y-4">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-96 w-full" />
      </div>
    );
  }

  const sourceBadge = (source: FileSource | undefined) => {
    if (source === "db") {
      return (
        <span className="text-xs px-2 py-0.5 rounded-md border border-warning/30 text-warning">
          {t("customize.edited")}
        </span>
      );
    }
    if (source === "fs") {
      return (
        <span className="text-xs px-2 py-0.5 rounded-md border border-success/30 text-success">
          {t("customize.fromRepo")}
        </span>
      );
    }
    return null;
  };

  return (
    <div className="p-6 max-w-4xl mx-auto">
      <div className="flex items-center justify-between mb-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">{t("customize.title")}</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {t("customize.subtitle", { name: agentName })}
          </p>
        </div>
        <div className="flex gap-2">
          {active?.source === "db" && (
            <Button
              onClick={handleRevert}
              disabled={saving}
              variant="outline"
              title={
                active.baseContent
                  ? t("customize.revertTitle")
                  : t("customize.revertTitleNoBase")
              }
            >
              <RotateCcw className="h-4 w-4 mr-2" /> {t("customize.revert")}
            </Button>
          )}
          <SaveButton onSave={handleSave} />
        </div>
      </div>

      {/* Tabs */}
      <div className="flex gap-1 border-b border-border mb-4 overflow-x-auto">
        {CUSTOMIZE_FILES.map((f) => (
          <button
            key={f.name}
            onClick={() => setActiveTab(f.name)}
            className={`rounded-md px-3 py-2 text-sm font-medium whitespace-nowrap border-b-2 transition-colors flex items-center gap-2 ${
              activeTab === f.name
                ? "border-primary text-primary"
                : "border-transparent text-muted-foreground hover:text-foreground"
            }`}
          >
            {t(f.label)}
            {files[f.name]?.source === "db" && (
              <span className="size-1.5 rounded-full bg-warning" />
            )}
          </button>
        ))}
      </div>

      {/* Active-tab status line — only shows when there's something
          actionable to say (override active / loaded from repo). The
          "default" case (empty + no repo base) is silent. */}
      {(active?.source === "db" || active?.source === "fs") && (
        <div className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
          {sourceBadge(active?.source)}
          {active?.source === "db" && active.baseContent && (
            <span>{t("customize.overrideActive", { n: active.baseContent.length })}</span>
          )}
          {active?.source === "fs" && (
            <span>{t("customize.loadedFromRepo", { code: `<agent home>/${activeTab}` })}</span>
          )}
        </div>
      )}

      {/* Editor */}
      <textarea
        value={active?.content || ""}
        onChange={(e) =>
          setFiles((prev) => ({
            ...prev,
            [activeTab]: { ...(prev[activeTab] || { source: "default" }), content: e.target.value },
          }))
        }
        spellCheck={false}
        className="w-full rounded-lg border border-border bg-card px-4 py-3 font-mono text-sm leading-relaxed outline-none focus:ring-1 focus:ring-primary/30 resize-none"
        // Bounded so the editor stays a reasonable size inside the
        // Settings dialog (85vh modal) — the previous
        // `calc(100vh - 240px)` made the textarea swallow nearly the
        // whole dialog. The clamp keeps the standalone /customize/
        // page usable too: still grows on tall screens, but stops
        // short of "fills the viewport".
        style={{ height: "min(55vh, 480px)", minHeight: 280 }}
        placeholder={`# ${activeTab}\n\n${t("customize.placeholder")}`}
      />
    </div>
  );
}
