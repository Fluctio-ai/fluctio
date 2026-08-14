"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import {
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import type { LucideIcon } from "lucide-react";

export interface NavItem {
  title: string;
  // url is the navigation target. Optional when onClick is provided —
  // a click-only item (e.g. one that opens a dialog) has no destination.
  url?: string;
  icon: LucideIcon;
  // active overrides the default pathname-based prefix match. Use this
  // when two items share the same pathname and only differ in query
  // (e.g. "New chat" vs an open session under /agents/<id>/chat/, where
  // the prefix rule would highlight both).
  active?: boolean;
  // onClick replaces the default router.push when present. Used for
  // items that open a dialog instead of navigating.
  onClick?: () => void;
}

function isActive(pathname: string, href: string) {
  const norm = (s: string) => s.replace(/\/$/, "");
  return norm(pathname) === norm(href) || norm(pathname).startsWith(norm(href) + "/");
}

// label is optional — when omitted the SidebarGroupLabel row is skipped
// so the section blends in as an unlabeled cluster (used for the standalone
// Overview link and the footer Settings entry).
export function NavMain({
  label,
  items,
}: {
  label?: string;
  items: NavItem[];
}) {
  const pathname = usePathname();
  const router = useRouter();

  // Prefetch target routes on idle so soft nav is ready when the user
  // clicks — mirrors what <Link> does automatically, but we're opting out
  // of Link below to guarantee client-side nav. Click-only items (no
  // url) have nothing to prefetch.
  React.useEffect(() => {
    items.forEach((item) => {
      if (item.url) router.prefetch(item.url);
    });
  }, [items, router]);

  // navTimerRef tracks the 2s hard-nav fallback below. It is cleared on every
  // pathname change (soft nav succeeded → fallback no longer needed) and on
  // every new click, so only the latest click's timer can fire. Without this,
  // rapid back-and-forth between two links lets a stale timer see an old
  // pathname snapshot, falsely conclude "nav didn't happen", and force a full
  // page reload (the rapid-click reload bug — same root cause as nav-knowledge).
  const navTimerRef = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  React.useEffect(() => {
    if (navTimerRef.current) {
      clearTimeout(navTimerRef.current);
      navTimerRef.current = null;
    }
  }, [pathname]);

  // The Base UI SidebarMenuButton `render` prop merges through
  // React.cloneElement, which intermittently dropped Next <Link>'s
  // internal click handler (every click became a full page reload →
  // visible sidebar flicker). A plain <button> + programmatic
  // router.push gives a guaranteed client-side transition.
  return (
    <SidebarGroup>
      {label && <SidebarGroupLabel>{label}</SidebarGroupLabel>}
      <SidebarMenu>
        {items.map((item) => {
          const active =
            item.active ?? (item.url ? isActive(pathname, item.url) : false);
          const handleClick = item.onClick
            ? item.onClick
            : item.url
              ? () => {
                  if (navTimerRef.current) clearTimeout(navTimerRef.current);
                  const before = window.location.pathname;
                  router.push(item.url!);
                  // Static export occasionally drops client-side nav
                  // silently — the URL doesn't change and no error
                  // surfaces (seen on /admin/* targets). Soft nav is
                  // async, though — the router must fetch the target
                  // route's RSC payload before it updates the URL, which
                  // takes a few hundred ms over the network. One rAF
                  // frame (~16ms) is far too short: the fetch hadn't
                  // even started, so pathname was still unchanged and
                  // every click got downgraded to a full reload (the
                  // "click any link → ~2s spinner" bug). 2s covers the
                  // slowest normal nav while still catching genuine
                  // /admin/* stalls. navTimerRef + the pathname effect
                  // above cancel a stale timer so rapid clicks don't
                  // misfire this fallback.
                  navTimerRef.current = setTimeout(() => {
                    navTimerRef.current = null;
                    if (window.location.pathname === before) {
                      window.location.href = item.url!;
                    }
                  }, 2000);
                }
              : undefined;
          return (
            <SidebarMenuItem key={item.url ?? item.title}>
              <SidebarMenuButton
                isActive={active}
                tooltip={item.title}
                onClick={handleClick}
                onMouseEnter={() => {
                  if (item.url) router.prefetch(item.url);
                }}
              >
                <item.icon />
                <span>{item.title}</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          );
        })}
      </SidebarMenu>
    </SidebarGroup>
  );
}

// Exported for pages that want a real anchor with Next client-nav
// without the sidebar button chrome.
export { Link as NavLink };
