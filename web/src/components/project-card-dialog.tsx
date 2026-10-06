"use client";

import * as React from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { DocMarkdown } from "@/components/doc-markdown";
import { RefreshCwIcon } from "lucide-react";
import { useT } from "@/lib/i18n";
import { fileUrl } from "@/lib/api";

// Read-only viewer for a project's shared PROJECT.md card. The card
// lives at the project-shared workspace layer and is kept in sync with
// the sessions table by the reconciler (agent/projectcard.go); this
// dialog just fetches and renders it — manual refresh covers "the card
// changed mid-conversation".
export function ProjectCardDialog({
  target,
  agentId,
  onClose,
}: {
  // Minimal shape so callers can pass a ProjectEntry directly.
  target: { id: string; name: string } | null;
  agentId: string;
  onClose: () => void;
}) {
  const t = useT();
  // null = not loaded yet or 404; "" would still be a (weird) file.
  const [content, setContent] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(false);

  const load = React.useCallback(async () => {
    if (!target) return;
    setLoading(true);
    try {
      // Same-origin fileUrl — cookie-authenticated like every other
      // workspace file the UI renders.
      const res = await fetch(
        fileUrl(agentId, `projects/${target.id}/PROJECT.md`),
      );
      setContent(res.ok ? await res.text() : null);
    } catch {
      setContent(null);
    } finally {
      setLoading(false);
    }
  }, [agentId, target]);

  React.useEffect(() => {
    if (target) {
      setContent(null);
      load();
    }
  }, [target, load]);

  return (
    <Dialog open={!!target} onOpenChange={(v) => !v && onClose()}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>
            {t("projects.card.title", { name: target?.name ?? "" })}
          </DialogTitle>
          <DialogDescription>{t("projects.card.desc")}</DialogDescription>
        </DialogHeader>
        <div className="max-h-[65vh] overflow-y-auto">
          {loading ? (
            <div className="py-8 text-center text-sm text-muted-foreground">
              {t("common.loading")}
            </div>
          ) : content ? (
            <DocMarkdown text={content} />
          ) : (
            <div className="py-8 text-center text-sm text-muted-foreground">
              {t("projects.card.empty")}
            </div>
          )}
        </div>
        <div className="flex justify-end">
          <Button variant="outline" size="sm" onClick={load} disabled={loading}>
            <RefreshCwIcon className={loading ? "animate-spin" : ""} />
            {t("projects.card.refresh")}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
