"use client";

import { useCallback, useEffect, useState } from "react";
import { History, Loader2, RotateCcw } from "lucide-react";
import { apiFetch } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { diffLines } from "@/lib/diff";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

// Mirrors store.AgentFileRevision / AgentFileRevisionDetail.
type Revision = {
  id: number;
  writer: string;
  reason: string;
  size: number;
  created_at: string;
};
type RevisionDetail = Revision & { content: string; prev_content: string };

// Writer keys come from store.WithFileWriter call sites.
const WRITERS = ["agent", "auto-persist", "tidy", "ui", "cli", "system"] as const;

function writerLabel(t: (k: string) => string, writer: string): string {
  if ((WRITERS as readonly string[]).includes(writer)) {
    return t(`customize.history.writer.${writer}`);
  }
  return writer;
}

export function FileRevisionHistoryDialog({
  agentId,
  filename,
  open,
  onOpenChange,
  onRestored,
}: {
  agentId: string;
  filename: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onRestored: () => void;
}) {
  const t = useT();
  // null = still loading; derived loading state keeps setState out of
  // the effect body (react-hooks/set-state-in-effect).
  const [revisions, setRevisions] = useState<Revision[] | null>(null);
  const [selected, setSelected] = useState<RevisionDetail | null>(null);
  const [loadingDetail, setLoadingDetail] = useState(false);
  const [restoring, setRestoring] = useState(false);

  // Single fetch+parse path for the revision list — the open-effect and
  // the post-restore refresh both go through here. The effect applies
  // results inside a .then callback (setState-in-effect lint shape).
  const fetchRevisions = useCallback(async (): Promise<Revision[] | null> => {
    const res = await apiFetch(`/api/agents/${agentId}/system-files/${filename}/revisions`);
    if (!res.ok) return null;
    const data = await res.json();
    return data.revisions || [];
  }, [agentId, filename]);

  const applyRevisions = (revs: Revision[]) => {
    setRevisions(revs);
    setSelected(null);
  };

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    fetchRevisions()
      .then((revs) => {
        if (!cancelled && revs !== null) applyRevisions(revs);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [open, fetchRevisions]);

  const refresh = useCallback(async () => {
    const revs = await fetchRevisions();
    if (revs !== null) applyRevisions(revs);
  }, [fetchRevisions]);

  const loadDetail = async (id: number) => {
    setLoadingDetail(true);
    try {
      const res = await apiFetch(`/api/agents/${agentId}/system-files/${filename}/revisions/${id}`);
      if (res.ok) setSelected(await res.json());
    } catch {
      /* leave previous selection */
    }
    setLoadingDetail(false);
  };

  const restore = async () => {
    if (!selected) return;
    const time = new Date(selected.created_at).toLocaleString();
    if (!confirm(t("customize.history.restoreConfirm", { file: filename, time }))) return;
    setRestoring(true);
    try {
      const res = await apiFetch(
        `/api/agents/${agentId}/system-files/${filename}/revisions/${selected.id}/restore`,
        { method: "POST" }
      );
      if (res.ok) {
        onRestored();
        await refresh();
      }
    } catch {
      /* keep dialog open; user can retry */
    }
    setRestoring(false);
  };

  // First list row is the newest revision == current content; restoring
  // it would be a no-op, so the button only appears on older rows.
  const isCurrent = revisions != null && revisions.length > 0 && selected?.id === revisions[0].id;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-4xl">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <History className="size-4" />
            {t("customize.history.title", { file: filename })}
          </DialogTitle>
        </DialogHeader>

        <div className="grid grid-cols-1 sm:grid-cols-[240px_1fr] gap-4 min-h-0 max-h-[70vh]">
          {/* Revision list */}
          <div className="overflow-y-auto border rounded-md divide-y min-h-0">
            {revisions === null && (
              <div className="flex items-center justify-center py-8 text-muted-foreground">
                <Loader2 className="size-4 animate-spin" />
              </div>
            )}
            {revisions !== null && revisions.length === 0 && (
              <div className="py-8 text-center text-sm text-muted-foreground">
                {t("customize.history.empty")}
              </div>
            )}
            {(revisions || []).map((r, i) => (
              <button
                key={r.id}
                onClick={() => loadDetail(r.id)}
                className={cn(
                  "w-full px-3 py-2 text-left text-sm transition-colors",
                  selected?.id === r.id ? "bg-accent" : "hover:bg-accent/50"
                )}
              >
                <div className="flex items-center gap-2">
                  <Badge variant="outline" className="text-muted-foreground">
                    {writerLabel(t, r.writer)}
                  </Badge>
                  {i === 0 && (
                    <Badge variant="outline" className="border-success/30 text-success">
                      {t("customize.history.current")}
                    </Badge>
                  )}
                </div>
                <div className="mt-1 text-xs text-muted-foreground">
                  {new Date(r.created_at).toLocaleString()} · {t("customize.history.bytes", { n: r.size })}
                </div>
                {r.reason && <div className="mt-0.5 text-xs line-clamp-2">{r.reason}</div>}
              </button>
            ))}
          </div>

          {/* Diff vs previous revision */}
          <div className="overflow-y-auto border rounded-md font-mono text-xs min-h-0">
            {loadingDetail && (
              <div className="flex items-center justify-center py-8 text-muted-foreground">
                <Loader2 className="size-4 animate-spin" />
              </div>
            )}
            {!loadingDetail && !selected && (
              <div className="py-8 text-center text-muted-foreground">
                {t("customize.history.pickOne")}
              </div>
            )}
            {!loadingDetail && selected && (
              <div className="py-1">
                <div className="px-3 py-1.5 text-muted-foreground border-b sticky top-0 bg-popover">
                  {t("customize.history.diff")}
                </div>
                {diffLines(selected.prev_content, selected.content).map((row, i) => (
                  <div
                    key={i}
                    className={cn(
                      "px-3 whitespace-pre-wrap break-all",
                      row.type === "add"
                        ? "bg-success/10 text-success"
                        : row.type === "del"
                          ? "bg-destructive/10 text-destructive line-through"
                          : "text-muted-foreground"
                    )}
                  >
                    {row.type === "add" ? "+ " : row.type === "del" ? "- " : "  "}
                    {row.text}
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>

        <DialogFooter>
          {selected && !isCurrent && (
            <Button onClick={restore} disabled={restoring} variant="outline">
              {restoring ? <Loader2 className="size-4 animate-spin" /> : <RotateCcw className="size-4" />}
              {t("customize.history.restore")}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
