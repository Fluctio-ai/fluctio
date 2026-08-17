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
import {
  BookMarkedIcon,
  CalendarIcon,
  CheckSquareIcon,
  ChevronRightIcon,
  FileTextIcon,
  LightbulbIcon,
  LinkIcon,
  StickyNoteIcon,
} from "lucide-react";
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
  // navTimerRef holds the pending hard-nav fallback setTimeout. It is
  // cleared on every new click AND whenever the pathname actually changes
  // (navigation succeeded). Without this, rapid back-and-forth clicking
  // leaves a stale timer holding an out-of-date `before` snapshot; when it
  // fires it can mis-detect "pathname unchanged" (a later click moved
  // pathname back to that old value) and force a full-page reload — the
  // "switching data-source <-> wiki reloads the page" symptom under fast
  // clicking.
  const navTimerRef = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  React.useEffect(() => {
    inFlightTargetRef.current = null;
    if (navTimerRef.current) {
      clearTimeout(navTimerRef.current);
      navTimerRef.current = null;
    }
  }, [pathname]);
  const navigateOnce = React.useCallback(
    (target: string) => {
      const here =
        pathname === target || pathname === target.replace(/\/$/, "");
      if (here) return;
      if (inFlightTargetRef.current === target) return;
      inFlightTargetRef.current = target;
      // Cancel any pending fallback from an earlier click — only the
      // latest click's fallback should ever fire (see navTimerRef).
      if (navTimerRef.current) clearTimeout(navTimerRef.current);
      const before = window.location.pathname;
      router.push(target);
      // Safety net: under static-export + spaHandler a router.push can
      // stall (RSC fetch slow / silent no-op), leaving pathname unchanged
      // and inFlightTargetRef stuck on `target` — the "sidebar item
      // won't click" symptom. After 2s reset inFlight so later clicks
      // work, and if the URL still hasn't moved force a hard navigation.
      // Mirrors NavMain's fallback.
      navTimerRef.current = setTimeout(() => {
        navTimerRef.current = null;
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

  // Prefetch both targets on mount so the first click soft-navigates
  // instead of stalling. Mirrors NavMain: without this, only hover
  // prefetches, so a fast click or a tap (no hover event) hits
  // router.push before the target route's chunk is ready. Under
  // static export / dev that fetch can exceed the 2s safety net above
  // and downgrade every click to a full page reload — the
  // "switching data-source <-> wiki reloads the page" symptom.
  React.useEffect(() => {
    if (!agentId) return;
    router.prefetch(`/agents/${agentId}/knowledge/`);
    router.prefetch(`/agents/${agentId}/wiki/`);
  }, [agentId, router]);

  if (!agentId) return null;

  // Knowledge section used to be two entries (Data Sources / Wiki) with the
  // data-source page owning an articles/flashes/todos/diary tab bar. That bar
  // is gone — each KB view is now its own sidebar entry (plus Wiki), so the
  // user lands on a view directly instead of through a tab switch. Icons are
  // lucide to match the rest of the sidebar (FileText/Lightbulb/CheckSquare/
  // Calendar/Link/StickyNote/BookMarked).
  const items = [
    { title: t("knowledge.articles"), url: `/agents/${agentId}/knowledge/`, icon: FileTextIcon },
    { title: t("knowledge.notes"), url: `/agents/${agentId}/knowledge/notes/`, icon: StickyNoteIcon },
    { title: t("knowledge.flashes"), url: `/agents/${agentId}/knowledge/flashes/`, icon: LightbulbIcon },
    { title: t("knowledge.todos"), url: `/agents/${agentId}/knowledge/todos/`, icon: CheckSquareIcon },
    { title: t("knowledge.diary"), url: `/agents/${agentId}/knowledge/diary/`, icon: CalendarIcon },
    { title: t("knowledge.bookmarks"), url: `/agents/${agentId}/knowledge/bookmarks/`, icon: LinkIcon },
    { title: t("nav.knowledge.wiki"), url: `/agents/${agentId}/wiki/`, icon: BookMarkedIcon },
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
        // Two tiles per row: the entry list outgrew the one-per-row menu
        // (7 entries pushed Sessions/Projects off-screen on short viewports),
        // so entries render as a compact 2-col grid of icon+label tiles.
        // The dedupe/active navigation logic is identical to the old
        // SidebarMenuButton rows; only the presentation is denser.
        <SidebarMenu>
          <div className="grid grid-cols-2 gap-1">
            {items.map((item) => {
              const norm = (s: string) => s.replace(/\/$/, "");
              // articles (/knowledge) is the parent route of flashes/todos/
              // diary/notes, so a prefix match lights it on every KB
              // sub-view — the bug where selecting 灵感/待办/日记 also
              // highlighted 文章. Only exact-match a url that is another
              // item's parent; other items still allow prefix matching
              // (e.g. wiki /wiki/<slug>).
              const isParentOfSibling = items.some(
                (o) => o.url !== item.url && norm(o.url).startsWith(norm(item.url) + "/"),
              );
              const active =
                norm(pathname) === norm(item.url) ||
                (!isParentOfSibling && norm(pathname).startsWith(norm(item.url) + "/"));
              return (
                <SidebarMenuItem key={item.url}>
                  <SidebarMenuButton
                    isActive={active}
                    tooltip={item.title}
                    className="h-auto min-h-0 justify-start gap-1.5 px-2 py-1.5 [&>span]:truncate"
                    onClick={() => {
                      if (norm(pathname) === norm(item.url)) {
                        // Already on this page — clicking the active item
                        // resets the pane (e.g. deselect the wiki page)
                        // instead of being a silent no-op.
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
                    <item.icon className="size-4 shrink-0" />
                    <span className="text-xs">{item.title}</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              );
            })}
          </div>
        </SidebarMenu>
      )}
    </SidebarGroup>
  );
}
