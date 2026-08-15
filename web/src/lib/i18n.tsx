"use client";

import React, { createContext, useContext, useState, useEffect, useCallback } from "react";

export type Locale = "en" | "zh-CN";
export const LOCALES: { value: Locale; label: string }[] = [
  { value: "en", label: "English" },
  { value: "zh-CN", label: "中文" },
];

const STORAGE_KEY = "fluctio-locale";

import en from "./locales/en";
import zhCN from "./locales/zh-CN";

const dictionaries: Record<Locale, Record<string, string>> = {
  en,
  "zh-CN": zhCN,
};

function detectLocale(): Locale {
  if (typeof window === "undefined") return "en";
  const stored = localStorage.getItem(STORAGE_KEY);
  if (stored && stored in dictionaries) return stored as Locale;
  const nav = navigator.language;
  if (nav.startsWith("zh")) return "zh-CN";
  return "en";
}

type TFunction = (key: string, vars?: Record<string, string | number>) => string;

interface I18nContextValue {
  locale: Locale;
  setLocale: (l: Locale) => void;
  t: TFunction;
}

const I18nContext = createContext<I18nContextValue>({
  locale: "en",
  setLocale: () => {},
  t: (k) => k,
});

export function I18nProvider({ children }: { children: React.ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>("en");
  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    setLocaleState(detectLocale());
    setMounted(true);
  }, []);

  const setLocale = useCallback((l: Locale) => {
    setLocaleState(l);
    localStorage.setItem(STORAGE_KEY, l);
  }, []);

  // Keep <html lang> in sync with the active locale. It used to update
  // only inside manual switches, leaving the initially detected locale
  // stuck on the static lang="en" — charset-based font fallback, screen
  // readers, and translation tools all read the wrong value.
  useEffect(() => {
    document.documentElement.lang = locale === "zh-CN" ? "zh-CN" : "en";
  }, [locale]);

  const t = useCallback<TFunction>(
    (key: string, vars?: Record<string, string | number>) => {
      let text = dictionaries[locale]?.[key] ?? dictionaries.en[key] ?? key;
      if (vars) {
        for (const [k, v] of Object.entries(vars)) {
          text = text.replace(new RegExp(`\\{${k}\\}`, "g"), String(v));
        }
      }
      return text;
    },
    [locale],
  );

  if (!mounted) {
    // Avoid hydration mismatch — render with en defaults on server
    return (
      <I18nContext.Provider value={{ locale: "en", setLocale: () => {}, t: (k) => dictionaries.en[k] ?? k }}>
        {children}
      </I18nContext.Provider>
    );
  }

  return (
    <I18nContext.Provider value={{ locale, setLocale, t }}>
      {children}
    </I18nContext.Provider>
  );
}

export function useLocale() {
  return useContext(I18nContext);
}

export function useT(): TFunction {
  return useContext(I18nContext).t;
}
