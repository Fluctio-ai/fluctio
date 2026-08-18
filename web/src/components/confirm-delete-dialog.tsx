"use client";

// ConfirmDeleteDialog — the shared KB delete confirmation. Mirrors the wiki
// page's AlertDialog flow (deleteTarget + confirm) so every knowledge view
// asks before destroying data. `name` identifies the item in the body copy;
// long names (flash/todo bodies) are truncated here so callers pass raw text.

import { useT } from "@/lib/i18n";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";

export function ConfirmDeleteDialog({
  open,
  name,
  onOpenChange,
  onConfirm,
}: {
  open: boolean;
  name: string;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void | Promise<void>;
}) {
  const t = useT();
  const shown = name.length > 60 ? name.slice(0, 60) + "…" : name;
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("common.deleteConfirmTitle")}</AlertDialogTitle>
          <AlertDialogDescription>
            {t("common.deleteConfirmBody", { name: shown })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
          <AlertDialogAction
            className="bg-destructive text-destructive-foreground"
            onClick={(e) => {
              e.preventDefault(); // keep the dialog open until onConfirm settles
              void onConfirm();
            }}
          >
            {t("common.delete")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
