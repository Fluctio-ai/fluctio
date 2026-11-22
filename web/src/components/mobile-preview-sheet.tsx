"use client";

// MobilePreviewSheet — the mobile counterpart of the desktop split preview:
// a floating eye button (absolute, top-right of the nearest positioned
// ancestor) opens the rendered body in a full-screen sheet shaped like the
// chat file viewer. Desktop never sees it (md:hidden on both parts).
// Render it INSIDE the editor's relative container so the button anchors
// correctly; the fixed sheet itself is viewport-anchored regardless.

import * as React from "react";
import { EyeIcon, XIcon } from "lucide-react";
import { useT } from "@/lib/i18n";

export function MobilePreviewSheet({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  const t = useT();
  const [open, setOpen] = React.useState(false);
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="absolute right-3 top-3 z-10 rounded-full border bg-background/80 p-2 text-muted-foreground shadow-sm backdrop-blur transition-colors hover:bg-muted hover:text-foreground md:hidden"
        aria-label={t("knowledge.notes.preview")}
      >
        <EyeIcon className="size-4" />
      </button>
      {open && (
        <div
          className="fixed inset-0 z-50 flex flex-col bg-background md:hidden"
          style={{
            paddingTop: "env(safe-area-inset-top)",
            paddingBottom: "env(safe-area-inset-bottom)",
          }}
        >
          <div className="flex h-12 shrink-0 items-center justify-between gap-2 border-b border-border px-4">
            <span className="min-w-0 truncate text-sm font-medium">{title}</span>
            <button
              type="button"
              onClick={() => setOpen(false)}
              className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
              aria-label={t("preview.close")}
            >
              <XIcon className="h-4 w-4" />
            </button>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto p-4">{children}</div>
        </div>
      )}
    </>
  );
}
