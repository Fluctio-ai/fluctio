"use client";

import * as React from "react";
import {
  BookOpenIcon,
  BrainIcon,
  Bug,
  ClockIcon,
  CoinsIcon,
  DatabaseIcon,
  HardDrive,
  IdCardIcon,
  InfoIcon,
  LayersIcon,
  Palette,
  Plug,
  RadioIcon,
  Regex,
  ServerIcon,
  SparklesIcon,
  UserCog,
  Wand2Icon,
  ShieldCheck,
  SlidersHorizontal,
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
import { KBSettingsCard } from "@/components/kb-settings-card";
import { DiarySettingsCard } from "@/components/diary-settings-card";
import { WikiAutoGenSettingsCard } from "@/components/wiki-autogen-settings-card";
import AgentMemoryPage from "@/app/agents/[id]/vectorization/page";
import AgentPrivacyPage from "@/app/agents/[id]/privacy/page";
import AgentRecallTuningPage from "@/app/agents/[id]/recall-tuning/page";
import AgentMCPPage from "@/app/agents/[id]/mcp/page";
import AgentUsagePage from "@/app/agents/[id]/usage/page";
import AccountSettingsPage from "@/app/settings/account/page";
import GeneralSettingsPage from "@/app/settings/general/page";
import UserModelsPage from "@/app/models/page";
import AboutSettingsPage from "@/app/settings/about/page";
import BackupSettingsPage from "@/app/settings/backup/page";
import DiagReportPage from "@/app/settings/diag/page";

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
  | "vectorization"
  | "privacy"
  | "usage"
  | "account"
  | "general"
  | "about"
  | "backup"
  | "recall-tuning"
  | "diag";

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
  { id: "vectorization", label: "settings.memory", icon: DatabaseIcon },
  { id: "privacy", label: "settings.privacy", icon: ShieldCheck },
  { id: "recall-tuning", label: "settings.recallTuning", icon: SlidersHorizontal },
  { id: "usage", label: "settings.usage", icon: CoinsIcon },
];

// Runtime intentionally lives only on the standalone /settings/runtime
// page (super_admin-gated) — it's a deployment-wide knob, not the kind
// of thing the average chatter wants in their per-agent dialog.
const USER_TABS: Array<{ id: AgentSettingsTab; label: string; icon: TabIcon }> = [
  { id: "account", label: "settings.account", icon: UserCog },
  { id: "general", label: "settings.general", icon: Palette },
  { id: "backup", label: "settings.backup", icon: HardDrive },
  { id: "diag", label: "settings.diag", icon: Bug },
  // About surfaces the gateway version + upgrade hint — only useful
  // to operators (super_admin), filtered out below for regular users.
  { id: "about", label: "settings.about", icon: InfoIcon },
];

// Tabbed configuration panel. Hosts both the per-agent pages
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
  userOnly = false,
  isAdmin = false,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  defaultTab?: AgentSettingsTab;
  role?: "owner" | "viewer";
  // userOnly hides the Agent section entirely. Used by the platform
  // sidebar's Settings button, which has no agent context — it should
  // only expose Account + General.
  userOnly?: boolean;
  // isAdmin gates super_admin-only tabs (currently just About — the
  // gateway version + upgrade hint is operator info, not end-user info).
  isAdmin?: boolean;
}) {
  const tt = useT();
  const agentTabs = userOnly
    ? []
    : role === "viewer"
      ? AGENT_TABS.filter((t) => t.id === "models" || t.id === "channels")
      : AGENT_TABS;
  // User tabs (Account / General / About) live ONLY on the platform sidebar's
  // Settings (userOnly) — the per-agent dialog drops them to avoid duplicating
  // what's already reachable from the post-login global settings.
  const userTabs = userOnly
    ? (isAdmin ? USER_TABS : USER_TABS.filter((t) => t.id !== "about"))
    : [];
  // Pick the landing tab: userOnly opens on General (User section);
  // viewers land on Models (the first Agent tab they have); owners on
  // Profile.
  const initialTab: AgentSettingsTab =
    defaultTab ??
    (userOnly ? "general" : role === "viewer" ? "models" : "profile");
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
          // Mobile: full-screen panel anchored to the top. A centered
          // floating box is cramped on a phone, and the fixed 220px rail
          // steals half the viewport. Drop rounding, span 100dvh, and pad
          // with safe-area insets so the notch / home indicator never
          // clips a tab or a save button.
          "flex flex-col h-[100dvh] w-full max-w-none rounded-none",
          "pt-[env(safe-area-inset-top)] pb-[env(safe-area-inset-bottom)]",
          // ≥sm: the original centered two-column dialog.
          "sm:h-[85vh] sm:w-[95vw] sm:max-w-[1100px] sm:rounded-xl sm:pt-0 sm:pb-0",
          "sm:grid sm:grid-cols-[220px_1fr] sm:grid-rows-1",
        )}
      >
        <aside
          className={cn(
            // Mobile: a single-row, horizontally scrollable tab strip
            // under the notch. pr-10 keeps the last tab clear of the
            // absolutely-positioned close button in the top-right.
            "flex shrink-0 gap-1 overflow-x-auto overflow-y-hidden border-b bg-muted/40 p-2 pr-10",
            // ≥sm: the vertical left rail (the historical layout).
            "sm:flex-col sm:overflow-x-visible sm:overflow-y-auto sm:border-r sm:border-b-0 sm:p-3 sm:pr-3",
          )}
        >
          {agentTabs.length > 0 && (
            <>
              <SectionLabel className="hidden sm:block">
                {tt("dialog.agentSection")}
              </SectionLabel>
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
          {userTabs.length > 0 && agentTabs.length > 0 && (
            <div
              aria-hidden
              className="mx-1 w-px self-stretch shrink-0 bg-border sm:hidden"
            />
          )}
          {userTabs.length > 0 && (
            <SectionLabel
              className={cn("hidden sm:block", agentTabs.length > 0 && "sm:mt-3")}
            >
              {tt("dialog.userSection")}
            </SectionLabel>
          )}
          {userTabs.map((t) => (
            <TabButton
              key={t.id}
              tab={t}
              active={tab === t.id}
              onSelect={setTab}
            />
          ))}
        </aside>
        <div className="min-h-0 flex-1 overflow-y-auto">
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
          {tab === "knowledge" && (
            <div className="mx-auto w-full max-w-5xl space-y-4 p-4">
              <KBSettingsCard />
              <DiarySettingsCard />
              <WikiAutoGenSettingsCard />
            </div>
          )}
          {tab === "vectorization" && <AgentMemoryPage />}
          {tab === "privacy" && <AgentPrivacyPage />}
          {tab === "recall-tuning" && <AgentRecallTuningPage />}
          {tab === "usage" && <AgentUsagePage />}
          {tab === "account" && (
            <div className="p-6 max-w-5xl mx-auto">
              <AccountSettingsPage />
            </div>
          )}
          {tab === "general" && (
            <div className="p-6 max-w-5xl mx-auto">
              <GeneralSettingsPage />
            </div>
          )}
          {tab === "backup" && (
            <div className="p-6 max-w-5xl mx-auto">
              <BackupSettingsPage />
            </div>
          )}
          {tab === "about" && (
            <div className="p-6 max-w-5xl mx-auto">
              <AboutSettingsPage />
            </div>
          )}
          {tab === "diag" && (
            <div className="p-6 max-w-5xl mx-auto">
              <DiagReportPage />
            </div>
          )}
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
        "flex shrink-0 items-center gap-2 whitespace-nowrap rounded-md px-2.5 py-2 text-left text-sm transition-colors",
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
