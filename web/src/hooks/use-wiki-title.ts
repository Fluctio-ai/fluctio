"use client";

import { useEffect, useState } from "react";

import { getWikiPage } from "@/lib/api";

// useWikiTitle resolves a card's wiki source_ref (a "type:slug" page id)
// into the page's title, so the source display / deep link reads as the
// page's name instead of the generic "查看 Wiki 原文". Returns null while
// unknown or for non-wiki sources. Keyed by card id: an advancing deck or
// a re-selected card invalidates the previous title immediately, so it
// never flashes the old card's name. Shared by the cards deck and the
// cards library detail pane.
export function useWikiTitle(
  agentId: string,
  cardId: string,
  sourceType?: string,
  sourceRef?: string,
): string | null {
  const [title, setTitle] = useState<string | null>(null);
  const active = sourceType === "wiki" && !!sourceRef;
  useEffect(() => {
    setTitle(null);
    if (!active || !sourceRef) return;
    let alive = true;
    getWikiPage(agentId, sourceRef)
      .then((p) => {
        if (alive) setTitle(p?.title || null);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
    // cardId participates so switching cards re-runs (sourceRef alone
    // would skip the reset when two cards share a page).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId, cardId, sourceRef, active]);
  return title;
}
