"use client";

import { Sun, Moon, Monitor } from "lucide-react";
import { useTheme, type Theme } from "@/components/theme-provider";
import { useLocale, useT, LOCALES } from "@/lib/i18n";

const choices: Array<{ value: Theme; labelKey: string; icon: React.ComponentType<{ className?: string }> }> = [
  { value: "light", labelKey: "general.theme.light", icon: Sun },
  { value: "dark", labelKey: "general.theme.dark", icon: Moon },
  { value: "system", labelKey: "general.theme.system", icon: Monitor },
];

export default function GeneralSettingsPage() {
  const { theme, setTheme } = useTheme();
  const { locale, setLocale } = useLocale();
  const t = useT();

  return (
    <div className="space-y-6">
      <div>
        <h3 className="text-xl font-semibold tracking-tight">{t("general.title")}</h3>
        <p className="text-sm text-muted-foreground mt-1">{t("general.description")}</p>
      </div>

      <div className="rounded-lg border border-border bg-card p-5">
        <h4 className="font-medium mb-1">{t("general.theme")}</h4>
        <p className="text-sm text-muted-foreground mb-4">{t("general.themeDesc")}</p>
        <div className="grid grid-cols-3 gap-3 max-w-md">
          {choices.map((c) => {
            const active = theme === c.value;
            const Icon = c.icon;
            return (
              <button
                key={c.value}
                type="button"
                onClick={() => setTheme(c.value)}
                className={
                  "flex flex-col items-center gap-2 rounded-md border px-3 py-4 text-sm transition " +
                  (active
                    ? "border-primary bg-primary/10 text-primary"
                    : "border-border hover:bg-muted")
                }
              >
                <Icon className="size-5" />
                {t(c.labelKey)}
              </button>
            );
          })}
        </div>
      </div>

      <div className="rounded-lg border border-border bg-card p-5">
        <h4 className="font-medium mb-1">{t("general.language")}</h4>
        <p className="text-sm text-muted-foreground mb-4">{t("general.languageDesc")}</p>
        <div className="grid grid-cols-2 gap-3 max-w-md">
          {LOCALES.map((l) => {
            const active = locale === l.value;
            return (
              <button
                key={l.value}
                type="button"
                onClick={() => setLocale(l.value)}
                className={
                  "flex items-center justify-center gap-2 rounded-md border px-3 py-4 text-sm transition " +
                  (active
                    ? "border-primary bg-primary/10 text-primary"
                    : "border-border hover:bg-muted")
                }
              >
                {l.label}
              </button>
            );
          })}
        </div>
      </div>
    </div>
  );
}
