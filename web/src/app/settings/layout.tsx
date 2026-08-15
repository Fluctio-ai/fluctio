"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useState } from "react";
import { getMe } from "@/lib/api";
import { useT } from "@/lib/i18n";

// Mirrors the AgentSettingsDialog USER_TABS order (General first — the
// dialog's landing tab — About last, Runtime admin-only) so a direct
// hit on /settings/* shows the same set of pages the dialog exposes.
// Labels are i18n keys resolved at render time.
const items = [
  { href: "/settings/general", label: "settings.general", adminOnly: false },
  { href: "/settings/account", label: "settings.account", adminOnly: false },
  { href: "/settings/backup", label: "settings.backup", adminOnly: false },
  { href: "/settings/diag", label: "settings.diag", adminOnly: false },
  { href: "/settings/about", label: "settings.about", adminOnly: false },
  { href: "/settings/runtime", label: "settings.runtime", adminOnly: true },
];

export default function SettingsLayout({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const t = useT();
  const [isSuperAdmin, setIsSuperAdmin] = useState(false);

  useEffect(() => {
    getMe()
      .then((m) => setIsSuperAdmin(m?.user?.role === "super_admin"))
      .catch(() => {});
  }, []);

  const visible = items.filter((i) => !i.adminOnly || isSuperAdmin);

  return (
    <div className="flex flex-col md:flex-row md:gap-8 p-4 md:p-6 max-w-6xl mx-auto md:min-h-[calc(100vh-3.5rem)]">
      <aside className="md:w-48 md:shrink-0 mb-4 md:mb-0">
        <h2 className="text-lg font-semibold tracking-tight mb-3 md:mb-4">{t("sidebar.settings")}</h2>
        {/* Horizontal scroll-tabs on mobile, vertical list on desktop. */}
        <nav className="flex flex-row md:flex-col gap-1 overflow-x-auto md:overflow-visible -mx-1 px-1 md:mx-0 md:px-0">
          {visible.map((it) => {
            const active = pathname === it.href || pathname.startsWith(it.href + "/");
            return (
              <Link
                key={it.href}
                href={it.href}
                className={
                  "shrink-0 md:shrink-0 whitespace-nowrap rounded-md px-3 py-2 text-sm transition " +
                  (active
                    ? "bg-accent text-accent-foreground"
                    : "text-muted-foreground hover:bg-muted hover:text-foreground")
                }
              >
                {t(it.label)}
              </Link>
            );
          })}
        </nav>
      </aside>
      <div className="flex-1 min-w-0">{children}</div>
    </div>
  );
}
