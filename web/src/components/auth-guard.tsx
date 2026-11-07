"use client";

import { useState, useEffect } from "react";
import { useRouter, usePathname } from "next/navigation";
import { getMe } from "@/lib/api";
import { LoginScreen } from "./login-screen";
import { BootSkeleton } from "./boot-skeleton";

interface AuthGuardProps {
  children: React.ReactNode;
}

// Routes that require an admin (super_admin) role. Server APIs enforce this
// authoritatively; the client gate just stops non-admins from landing on a
// page that would render an empty / 403'd shell.
//
// /settings, /models, and /apikeys are intentionally NOT here —
// settings hides Runtime; models merges system+user with badges;
// apikeys lets non-admins issue type=user/agent (only type=admin
// requires super_admin and that gate lives inside the create handler).
const ADMIN_PATH_PREFIXES = [
  "/admin/",
  "/skills",
  "/tools",
];

function isAdminPath(pathname: string): boolean {
  return ADMIN_PATH_PREFIXES.some((p) => {
    const norm = p.replace(/\/$/, "");
    // exact match (with or without trailing slash) or a proper sub-path
    return pathname === norm || pathname.startsWith(norm + "/");
  });
}

export function AuthGuard({ children }: AuthGuardProps) {
  const router = useRouter();
  const pathname = usePathname();
  // Session auth + role. isAdminRole: null = unknown for this session,
  // true = a getMe proved super_admin at some point (roles are session-
  // stable, so one proof covers every restricted path — no refetch per
  // navigation), false = proved non-admin.
  const [authed, setAuthed] = useState(false);
  const [isAdminRole, setIsAdminRole] = useState<boolean | null>(null);
  // The initial config/session check — BootSkeleton while it runs.
  const [initialPending, setInitialPending] = useState(true);
  const restricted = isAdminPath(pathname);

  useEffect(() => {
    // Already-authed fast paths: nothing to do on ordinary navigations,
    // and a proven super_admin covers every restricted path.
    if (authed && (!restricted || isAdminRole === true)) return;

    let aborted = false;
    (async () => {
      if (!authed) {
        // Decide between three states:
        //   - users table empty → /onboard
        //   - users exist, caller has a session → render children
        //   - users exist, caller has no session → show LoginScreen
        let configured = false;
        try {
          const res = await fetch("/api/status", { credentials: "same-origin" });
          if (res.ok) {
            const status = await res.json();
            configured = !!status.configured;
          }
        } catch {
          // server down — fall through to LoginScreen
        }
        if (aborted) return;

        if (!configured) {
          const onOnboard = pathname === "/onboard" || pathname.startsWith("/onboard/");
          if (!onOnboard) {
            router.replace("/onboard/");
            return;
          }
          setAuthed(true);
          setInitialPending(false);
          return;
        }

        try {
          const me = await getMe();
          if (me.ok && me.user) {
            const admin = me.user.role === "super_admin";
            setIsAdminRole(admin);
            setAuthed(true);
            if (restricted && !admin) {
              // Non-admin on a restricted path: hold the skeleton until
              // the redirect lands — never mount the restricted children.
              router.replace("/overview/");
              return;
            }
          }
        } catch {
          // network failure — fall through to LoginScreen
        }
        setInitialPending(false);
        return;
      }

      // Authed on a restricted path whose role isn't proven yet (first
      // restricted navigation, or the login callback below). Re-run the
      // role gate before the children may mount — previously `checked`
      // stayed true across client navigations, so restricted pages fully
      // mounted and fired their admin API calls in the window before
      // router.replace landed.
      try {
        const me = await getMe();
        if (aborted) return;
        if (me.ok && me.user) {
          const admin = me.user.role === "super_admin";
          setIsAdminRole(admin);
          if (!admin) {
            router.replace("/overview/");
          }
        } else {
          // Session gone: back to the login flow for the next check.
          setAuthed(false);
          setIsAdminRole(null);
          setInitialPending(false);
        }
      } catch {
        // Transient network failure — keep holding the skeleton (fail
        // closed); a later navigation or reload retries the check.
      }
    })();
    return () => { aborted = true; };
  }, [authed, restricted, isAdminRole, pathname, router]);

  if (!authed) {
    if (initialPending) return <BootSkeleton />;
    return (
      <LoginScreen
        onSuccess={() => {
          // Re-run the role gate after login: a non-admin account logging
          // in while sitting on /admin/* must not land on the admin
          // children — the pre-login role check ran without a session.
          setIsAdminRole(null);
          setAuthed(true);
        }}
      />
    );
  }
  // Restricted path with unproven (or disproven) role: hold the skeleton,
  // never the children. isAdminRole === false also holds while the
  // redirect to /overview/ is in flight.
  if (restricted && isAdminRole !== true) {
    return <BootSkeleton />;
  }
  return <>{children}</>;
}
