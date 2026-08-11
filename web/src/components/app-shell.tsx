"use client";

import * as React from "react";
import { usePathname } from "next/navigation";
import { SidebarLayout } from "@/components/sidebar";
import { rememberAgent } from "@/lib/last-agent";

// Paths that render on their own (no sidebar chrome).
const BARE_PATHS = ["/", "/onboard"];

function wantsSidebar(pathname: string) {
  if (BARE_PATHS.includes(pathname)) return false;
  if (pathname.startsWith("/onboard/")) return false;
  return true;
}

// AppShell mounts SidebarLayout once for every authenticated page and keeps
// that instance alive across client-side navigations. Previously each route
// segment had its own layout.tsx that re-wrapped SidebarLayout, so Next
// unmounted and remounted the sidebar on every top-level nav — triggering a
// fresh status / agents / sessions fetch and a visible flash. One shell at
// the root means the sidebar (and its effects) persists across navigations.
export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  // Remember the most recently visited agent so the login landing can drop
  // the user straight back into it instead of forcing an agent switch.
  React.useEffect(() => {
    const m = pathname?.match(/\/agents\/([^/]+)\//);
    if (m && m[1]) rememberAgent(m[1]);
  }, [pathname]);
  if (!wantsSidebar(pathname)) {
    return <>{children}</>;
  }
  return <SidebarLayout>{children}</SidebarLayout>;
}
