"use client";

import * as React from "react";
import {
  BookMarkedIcon,
  BookOpenIcon,
  BrainIcon,
  ClockIcon,
  CoinsIcon,
  DatabaseIcon,
  IdCardIcon,
  LayersIcon,
  Plug,
  RadioIcon,
  Regex,
  ServerIcon,
  SparklesIcon,
  Wand2Icon,
} from "lucide-react";

import { Dialog, DialogContent } from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { useT } from "@/lib/i18n";

import AgentProfilePanel from "@/components/agent-profile-panel";
import AgentCustomizePage from "@/app/agents/[id]/customize/page";
import AgentModelsPage from "@/app/agents/[id]/models/page";
import AgentContextPage from "@/app/agents/[id]/context/page";
import AgentSkillsPage from "@/app/agents/[id]/skills/page";
import AgentPluginsPage from "@/app/agents/[id]/plugins/page";
import AgentChannelsPage from "@/app/agents/[id]/channels/page";
import AgentSchedulerPage from "@/app/agents/[id]/scheduler/page";
import AgentRegexHooksPage from "@/app/agents/[id]/regex-hooks/page";
import AgentKnowledgePage from "@/app/agents/[id]/knowledge/page";
import AgentWikiPage from "@/app/agents/[id]/wiki/page";
import AgentMemoryPage from "@/app/agents/[id]/memory/page";
import AgentMCPPage from "@/app/agents/[id]/mcp/page";
import AgentUsagePage from "@/app/agents/[id]/usage/page";
import UserModelsPage from "@/app/models/page";

export type AgentSettingsTab =
  | "profile"
  | "customize"
  | "models"
  | "context"
  | "skills"
  | "mcp"
  | "plugins"
  | "channels"
  | "scheduler"
  | "regex-hooks"
  | "knowledge"
  | "wiki"
  | "memory"
  | "usage";

type TabIcon = React.ComponentType<{ className?: string }>;

const AGENT_TABS: Array<{ id: AgentSettingsTab; label: string; icon: TabIcon }> = [
  { id: "profile", label: "settings.profile", icon: IdCardIcon },
  { id: "customize", label: "settings.customize", icon: Wand2Icon },
  { id: "models", label: "settings.models", icon: BrainIcon },
  { id: "context", label: "settings.context", icon: LayersIcon },
  { id: "skills", label: "settings.skills", icon: SparklesIcon },
  { id: "mcp", label: "settings.mcp", icon: ServerIcon },
  { id: "plugins", label: "settings.plugins", icon: Plug },
  { id: "channels", label: "settings.channels", icon: RadioIcon },
  { id: "scheduler", label: "settings.scheduler", icon: ClockIcon },
  { id: "regex-hooks", label: "settings.regexHooks", icon: Regex },
  { id: "knowledge", label: "settings.knowledge", icon: BookOpenIcon },
  { id: "wiki", label: "settings.wiki", icon: BookMarkedIcon },
  { id: "memory", label: "settings.memory", icon: DatabaseIcon },
  { id: "usage", label: "settings.usage", icon: CoinsIcon },
];

// Tabbed configuration panel. Hosts the per-agent pages
// (Customize / Models / Skills / Channels / Scheduler) and the
// per-user pages (Account / General / Runtime[admin-only]) so a
// click on the sidebar Settings button covers everything the user
// could want to change. Each tab mounts the existing page component
// lazily — switching tabs unmounts the previous panel, which is fine
// because the pages are self-contained and re-fetch on mount.
//
// role="viewer" hides the owner-only Agent tabs (Profile, Customize,
// Skills, Scheduler, Usage) and only exposes Models + Channels under
// Agent — viewers can pin their own model for the shared agent and
// bind their own IM accounts, but can't touch the agent's identity /
// skills / scheduling. The Models tab id is shared with owners; the
// render branch below picks the agent-scope page for owners and the
// user-scope page for viewers (same tab slot, different writer).
export function AgentSettingsDialog({
  open,
  onOpenChange,
  defaultTab,
  role = "owner",
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  defaultTab?: AgentSettingsTab;
  role?: "owner" | "viewer";
}) {
  const tt = useT();
  const agentTabs =
    role === "viewer"
      ? AGENT_TABS.filter((t) => t.id === "models" || t.id === "channels")
      : AGENT_TABS;
  // Pick the landing tab: viewers land on Models; owners on Profile.
  const initialTab: AgentSettingsTab =
    defaultTab ?? (role === "viewer" ? "models" : "profile");
  const [tab, setTab] = React.useState<AgentSettingsTab>(initialTab);

  // Reset to the requested tab whenever the dialog re-opens, so a fresh
  // click on the sidebar Settings button always lands on the same place.
  React.useEffect(() => {
    if (open) setTab(initialTab);
  }, [open, initialTab]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className={cn(
          "p-0 gap-0 overflow-hidden",
          "h-[85vh] w-[95vw] max-w-[1100px] sm:max-w-[1100px]",
          "grid grid-cols-[220px_1fr] grid-rows-1",
        )}
      >
        <aside className="flex flex-col gap-1 border-r bg-muted/40 p-3 overflow-y-auto">
          {agentTabs.length > 0 && (
            <>
              <SectionLabel>{tt("dialog.agentSection")}</SectionLabel>
              {agentTabs.map((t) => (
                <TabButton
                  key={t.id}
                  tab={t}
                  active={tab === t.id}
                  onSelect={setTab}
                />
              ))}
            </>
          )}
        </aside>
        <div className="overflow-y-auto">
          {tab === "profile" && <AgentProfilePanel />}
          {tab === "customize" && <AgentCustomizePage />}
          {tab === "models" &&
            (role === "viewer" ? <UserModelsPage /> : <AgentModelsPage />)}
          {tab === "context" && <AgentContextPage />}
          {tab === "skills" && <AgentSkillsPage />}
          {tab === "mcp" && <AgentMCPPage />}
          {tab === "plugins" && <AgentPluginsPage />}
          {tab === "channels" && <AgentChannelsPage />}
          {tab === "scheduler" && <AgentSchedulerPage />}
          {tab === "regex-hooks" && <AgentRegexHooksPage />}
          {tab === "knowledge" && <AgentKnowledgePage />}
          {tab === "wiki" && <AgentWikiPage />}
          {tab === "memory" && <AgentMemoryPage />}
          {tab === "usage" && <AgentUsagePage />}
        </div>
      </DialogContent>
    </Dialog>
  );
}

function SectionLabel({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "px-2 pt-1 pb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground",
        className,
      )}
    >
      {children}
    </div>
  );
}

function TabButton({
  tab,
  active,
  onSelect,
}: {
  tab: { id: AgentSettingsTab; label: string; icon: TabIcon };
  active: boolean;
  onSelect: (id: AgentSettingsTab) => void;
}) {
  const Icon = tab.icon;
  const t = useT();
  return (
    <button
      type="button"
      onClick={() => onSelect(tab.id)}
      className={cn(
        "flex items-center gap-2 rounded-md px-2.5 py-2 text-sm text-left transition-colors",
        active
          ? "bg-accent text-accent-foreground font-medium"
          : "text-foreground/80 hover:bg-accent/50",
      )}
    >
      <Icon className="size-4 shrink-0" />
      <span>{t(tab.label)}</span>
    </button>
  );
}
