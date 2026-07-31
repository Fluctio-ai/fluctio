"use client";

import * as React from "react";
import { usePathname, useRouter } from "next/navigation";
import {
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { BookMarkedIcon, ChevronRightIcon, DatabaseIcon } from "lucide-react";
import { useT } from "@/lib/i18n";

// NavKnowledge is the "Knowledge" section of the agent sidebar: a
// collapsible group sitting between "New chat" and "Projects" that links
// to the data-source list (/knowledge/) and the wiki browser (/wiki/).
// The KB *settings* (auto-query, wiki auto-gen) stay in the Settings
// dialog — this group is only the content-browsing entry points.
//
// Single-user build: no owner/viewer gate — if an agent is active the
// group shows. Mirrors NavSessions' collapsible-section pattern (click
// the label to collapse the whole group).
export function NavKnowledge({ agentId }: { agentId: string | null }) {
  const pathname = usePathname();
  const router = useRouter();
  const t = useT();
  const [sectionCollapsed, setSectionCollapsed] = React.useState(false);

  // Dedupe rapid double-clicks on the same row — same guard as
  // NavSessions / NavProjectsList to avoid stacking router.push calls
  // under static-export RSC fetches.
  const inFlightTargetRef = React.useRef<string | null>(null);
  React.useEffect(() => {
    inFlightTargetRef.current = null;
  }, [pathname]);
  const navigateOnce = React.useCallback(
    (target: string) => {
      const here =
        pathname === target || pathname === target.replace(/\/$/, "");
      if (here) return;
      if (inFlightTargetRef.current === target) return;
      inFlightTargetRef.current = target;
      const before = window.location.pathname;
      router.push(target);
      // Safety net: under static-export + spaHandler a router.push can
      // stall (RSC fetch slow / silent no-op), leaving pathname unchanged
      // and inFlightTargetRef stuck on `target` — the "sidebar item
      // won't click" symptom. After 2s reset inFlight so later clicks
      // work, and if the URL still hasn't moved force a hard navigation.
      // Mirrors NavMain's fallback.
      setTimeout(() => {
        if (inFlightTargetRef.current === target) {
          inFlightTargetRef.current = null;
        }
        if (window.location.pathname === before) {
          window.location.href = target;
        }
      }, 2000);
    },
    [pathname, router],
  );

  if (!agentId) return null;

  const items = [
    {
      title: t("nav.knowledge.sources"),
      url: `/agents/${agentId}/knowledge/`,
      icon: DatabaseIcon,
    },
    {
      title: t("nav.knowledge.wiki"),
      url: `/agents/${agentId}/wiki/`,
      icon: BookMarkedIcon,
    },
  ];

  return (
    <SidebarGroup className="group-data-[collapsible=icon]:hidden">
      <SidebarGroupLabel
        onClick={() => setSectionCollapsed((c) => !c)}
        className="cursor-pointer select-none hover:text-sidebar-foreground"
      >
        <ChevronRightIcon
          className={
            "mr-1 transition-transform " +
            (sectionCollapsed ? "rotate-0" : "rotate-90")
          }
        />
        {t("nav.group.knowledge")}
      </SidebarGroupLabel>
      {!sectionCollapsed && (
        <SidebarMenu>
          {items.map((item) => {
            const active =
              pathname === item.url || pathname.startsWith(item.url);
            return (
              <SidebarMenuItem key={item.url}>
                <SidebarMenuButton
                  isActive={active}
                  tooltip={item.title}
                  onClick={() => {
                    const here =
                      pathname === item.url ||
                      pathname === item.url.replace(/\/$/, "");
                    if (here) {
                      // Already on this page — clicking the active item
                      // resets the pane (deselect the wiki page / data
                      // source) instead of being a silent no-op.
                      window.dispatchEvent(
                        new CustomEvent("fluctio:nav-reselect", {
                          detail: { url: item.url },
                        }),
                      );
                      return;
                    }
                    navigateOnce(item.url);
                  }}
                  onMouseEnter={() => router.prefetch(item.url)}
                >
                  <item.icon />
                  <span>{item.title}</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            );
          })}
        </SidebarMenu>
      )}
    </SidebarGroup>
  );
}
