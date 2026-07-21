"use client";
import { useT } from "@/lib/i18n";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
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
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Zap,
  Trash2,
  Plus,
  GripVertical,
  Terminal,
  Pencil,
  Regex as RegexIcon,
} from "lucide-react";
import {
  listRegexHooks,
  saveRegexHook,
  deleteRegexHook,
  reorderRegexHooks,
  listHookScripts,
  uploadHookScript,
  deleteHookScript,
  type RegexHook,
  type HookScript,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

type FormData = {
  id: string;
  name: string;
  pattern: string;
  cliCommand: string;
  sortOrder: number;
  continueOnMatch: boolean;
  enabled: boolean;
  showError: boolean;
  errorMessage: string;
};

const emptyForm: FormData = {
  id: "",
  name: "",
  pattern: "",
  cliCommand: "",
  sortOrder: 0,
  continueOnMatch: false,
  enabled: true,
  showError: true,
  errorMessage: "",
};

export default function AgentRegexHooksPage() {
  const t = useT();
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);

  const [hooks, setHooks] = useState<RegexHook[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [editOpen, setEditOpen] = useState(false);
  const [form, setForm] = useState<FormData>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<RegexHook | null>(null);
  const [dragIdx, setDragIdx] = useState<number | null>(null);
  const [scripts, setScripts] = useState<HookScript[]>([]);

  const refresh = useCallback(() => {
    if (!agentId) return;
    setLoading(true);
    listRegexHooks(agentId)
      .then((list) => {
        setHooks(list);
        setError("");
      })
      .catch((e) =>
        setError(e instanceof Error ? e.message : t("regexHooks.loadFailed")),
      )
      .finally(() => setLoading(false));
    listHookScripts(agentId).then(setScripts).catch(() => {});
  }, [agentId, t]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const openCreate = () => {
    setForm({ ...emptyForm, sortOrder: hooks.length });
    setEditOpen(true);
  };

  const openEdit = (hook: RegexHook) => {
    setForm({
      id: hook.id,
      name: hook.name,
      pattern: hook.pattern,
      cliCommand: hook.cliCommand,
      sortOrder: hook.sortOrder,
      continueOnMatch: hook.continueOnMatch,
      enabled: hook.enabled,
      showError: hook.showError,
      errorMessage: hook.errorMessage,
    });
    setEditOpen(true);
  };

  const handleSave = async () => {
    if (!agentId) return;
    setSaving(true);
    const res = await saveRegexHook(agentId, {
      id: form.id || undefined,
      name: form.name,
      pattern: form.pattern,
      cliCommand: form.cliCommand,
      sortOrder: form.sortOrder,
      continueOnMatch: form.continueOnMatch,
      enabled: form.enabled,
      showError: form.showError,
      errorMessage: form.errorMessage,
    });
    setSaving(false);
    if (res.error) {
      setError(res.error);
      return;
    }
    setEditOpen(false);
    refresh();
  };

  const handleDelete = async () => {
    if (!deleteTarget || !agentId) return;
    const target = deleteTarget;
    setDeleteTarget(null);
    const res = await deleteRegexHook(agentId, target.id);
    if (res.error) setError(res.error);
    refresh();
  };

  const handleToggle = async (hook: RegexHook, enabled: boolean) => {
    if (!agentId) return;
    setHooks((prev) =>
      prev.map((h) => (h.id === hook.id ? { ...h, enabled } : h)),
    );
    const res = await saveRegexHook(agentId, { ...hook, enabled });
    if (res.error) {
      setError(res.error);
      refresh();
    }
  };

  // Drag & drop reorder
  const onDragStart = (idx: number) => setDragIdx(idx);

  const onDragOver = (e: React.DragEvent, idx: number) => {
    e.preventDefault();
    if (dragIdx === null || dragIdx === idx) return;
    const reordered = [...hooks];
    const [moved] = reordered.splice(dragIdx, 1);
    reordered.splice(idx, 0, moved);
    setHooks(reordered);
    setDragIdx(idx);
  };

  const onDragEnd = async () => {
    setDragIdx(null);
    if (!agentId) return;
    const ids = hooks.map((h) => h.id);
    await reorderRegexHooks(agentId, ids);
  };

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <div className="flex items-center gap-2">
            <RegexIcon className="size-5 text-muted-foreground" />
            <h2 className="text-2xl font-semibold tracking-tight">
              {t("regexHooks.title")}
            </h2>
          </div>
          <p className="text-sm text-muted-foreground mt-1">
            {t("regexHooks.subtitle")}
          </p>
        </div>
        <Button onClick={openCreate} size="sm">
          <Plus className="size-4 mr-1" />
          {t("regexHooks.create")}
        </Button>
      </div>

      {error && (
        <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-4">
          <p className="text-sm text-destructive">{error}</p>
        </div>
      )}

      {loading ? (
        <div className="space-y-2">
          <Skeleton className="h-24" />
          <Skeleton className="h-24" />
          <Skeleton className="h-24" />
        </div>
      ) : hooks.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border bg-card/50 p-10 text-center">
          <Zap className="mx-auto size-8 text-muted-foreground/50 mb-3" />
          <p className="text-sm text-muted-foreground">
            {t("regexHooks.noHooks")}
          </p>
          <p className="text-xs text-muted-foreground/70 mt-1">
            {t("regexHooks.emptyHint")}
          </p>
        </div>
      ) : (
        <div className="grid gap-3">
          {hooks.map((hook, idx) => (
            <HookRow
              key={hook.id}
              hook={hook}
              onToggle={(enabled) => handleToggle(hook, enabled)}
              onEdit={() => openEdit(hook)}
              onDelete={() => setDeleteTarget(hook)}
              onDragStart={() => onDragStart(idx)}
              onDragOver={(e) => onDragOver(e, idx)}
              onDragEnd={onDragEnd}
            />
          ))}
        </div>
      )}


      {/* Scripts Section */}
      <div>
        <div className="flex items-center justify-between mb-3">
          <div className="flex items-center gap-2">
            <Terminal className="size-5 text-muted-foreground" />
            <h3 className="text-lg font-semibold">{t("regexHooks.scripts")}</h3>
          </div>
          <label className="cursor-pointer">
            <input
              type="file"
              className="hidden"
              accept=".py,.sh,.bat,.cmd,.exe,.js,.ts,.ps1"
              onChange={(e) => {
                const file = e.target.files?.[0];
                if (!file || !agentId) return;
                uploadHookScript(agentId, file).then((res) => {
                  if (res.error) setError(res.error);
                  listHookScripts(agentId).then(setScripts);
                  e.target.value = "";
                });
              }}
            />
            <span className="inline-flex items-center gap-1 rounded-md bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground hover:bg-primary/90">
              <Plus className="size-3.5" />
              {t("regexHooks.upload")}
            </span>
          </label>
        </div>
        <p className="text-xs text-muted-foreground mb-3">
          {t("regexHooks.scriptDirHint1")}{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono">~/.fluctio/agents/{agentId}/hooks/</code>
          {t("regexHooks.scriptDirHint2")}{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono">hooks/script_name</code>.
        </p>
        {scripts.length === 0 ? (
          <div className="rounded-lg border border-dashed border-border bg-card/50 p-6 text-center">
            <Terminal className="mx-auto size-6 text-muted-foreground/50 mb-2" />
            <p className="text-xs text-muted-foreground">{t("regexHooks.noScripts")}</p>
          </div>
        ) : (
          <div className="grid gap-2">
            {scripts.map((s) => (
              <div key={s.name} className="flex items-center justify-between rounded-lg border border-border bg-card px-3 py-2">
                <div className="flex items-center gap-2 min-w-0">
                  <Terminal className="size-3.5 text-muted-foreground shrink-0" />
                  <code className="text-sm font-mono truncate">{s.name}</code>
                  <span className="text-[10px] text-muted-foreground">{(s.size / 1024).toFixed(1)} KB</span>
                </div>
                <div className="flex items-center gap-1 shrink-0">
                  <Button
                    size="sm"
                    variant="ghost"
                    className="text-xs h-7"
                    onClick={() => {
                      setForm((f) => ({ ...f, cliCommand: "hooks/" + s.name }));
                    }}
                  >
                    {t("regexHooks.useScript")}
                  </Button>
                  <Button
                    size="icon"
                    variant="ghost"
                    className="text-destructive hover:text-destructive size-7"
                    onClick={async () => {
                      if (!agentId) return;
                      await deleteHookScript(agentId, s.name);
                      listHookScripts(agentId).then(setScripts);
                    }}
                  >
                    <Trash2 className="size-3.5" />
                  </Button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Edit / Create Dialog */}
      <Dialog open={editOpen} onOpenChange={setEditOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>
              {form.id ? t("regexHooks.editHook") : t("regexHooks.createHook")}
            </DialogTitle>
          </DialogHeader>
          <div className="space-y-4">
            <div>
              <label className="text-sm font-medium">{t("regexHooks.hookName")}</label>
              <Input
                value={form.name}
                onChange={(e) =>
                  setForm((f) => ({ ...f, name: e.target.value }))
                }
                placeholder={t("regexHooks.namePlaceholder")}
              />
            </div>
            <div>
              <label className="text-sm font-medium">{t("regexHooks.patternLabel")}</label>
              <Input
                value={form.pattern}
                onChange={(e) =>
                  setForm((f) => ({ ...f, pattern: e.target.value }))
                }
                placeholder={t("regexHooks.patternPlaceholder")}
                className="font-mono"
              />
              <p className="text-xs text-muted-foreground mt-1">
                {t("regexHooks.patternHint")}
              </p>
            </div>
            <div>
              <label className="text-sm font-medium">{t("regexHooks.cliCommandLabel")}</label>
              <Input
                value={form.cliCommand}
                onChange={(e) =>
                  setForm((f) => ({ ...f, cliCommand: e.target.value }))
                }
                placeholder={t("regexHooks.cliPlaceholder")}
                className="font-mono"
              />
              <p className="text-xs text-muted-foreground mt-1">
                {t("regexHooks.cliHint")}
              </p>
            </div>
            <div className="flex items-center gap-6">
              <div className="flex items-center gap-2">
                <Switch
                  checked={form.continueOnMatch}
                  onCheckedChange={(v) =>
                    setForm((f) => ({ ...f, continueOnMatch: !!v }))
                  }
                />
                <label className="text-sm">{t("regexHooks.continueMatch")}</label>
              </div>
              <div className="flex items-center gap-2">
                <Switch
                  checked={form.enabled}
                  onCheckedChange={(v) =>
                    setForm((f) => ({ ...f, enabled: !!v }))
                  }
                />
                <label className="text-sm">{t("regexHooks.enabled")}</label>
              </div>
            </div>
            <div className="flex items-center gap-6">
              <div className="flex items-center gap-2">
                <Switch
                  checked={form.showError}
                  onCheckedChange={(v) =>
                    setForm((f) => ({ ...f, showError: !!v }))
                  }
                />
                <label className="text-sm">{t("regexHooks.showError")}</label>
              </div>
            </div>
            {form.showError && (
              <div>
                <label className="text-sm font-medium">
                  {t("regexHooks.errorMessage")}
                </label>
                <Textarea
                  value={form.errorMessage}
                  onChange={(e) =>
                    setForm((f) => ({ ...f, errorMessage: e.target.value }))
                  }
                  placeholder={t("regexHooks.errorPlaceholder")}
                  rows={2}
                />
              </div>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              onClick={handleSave}
              disabled={saving || !form.name || !form.pattern || !form.cliCommand}
            >
              {saving ? t("common.saving") : t("common.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete Confirm */}
      <AlertDialog
        open={!!deleteTarget}
        onOpenChange={(v) => !v && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("regexHooks.deleteHook")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("regexHooks.deleteConfirmPrefix")}{" "}
              <strong>{deleteTarget?.name || deleteTarget?.id}</strong>
              {t("regexHooks.deleteConfirmSuffix")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {t("common.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function HookRow({
  hook,
  onToggle,
  onEdit,
  onDelete,
  onDragStart,
  onDragOver,
  onDragEnd,
}: {
  hook: RegexHook;
  onToggle: (enabled: boolean) => void;
  onEdit: () => void;
  onDelete: () => void;
  onDragStart: () => void;
  onDragOver: (e: React.DragEvent) => void;
  onDragEnd: () => void;
}) {
  const t = useT();
  return (
    <div
      className="rounded-lg border border-border bg-card p-4 cursor-default"
      draggable
      onDragStart={onDragStart}
      onDragOver={onDragOver}
      onDragEnd={onDragEnd}
    >
      <div className="flex items-start gap-3">
        <div className="pt-1 cursor-grab text-muted-foreground/50 hover:text-muted-foreground">
          <GripVertical className="size-4" />
        </div>
        <div className="flex-1 min-w-0 space-y-2">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="font-medium truncate">{hook.name}</span>
            {hook.continueOnMatch && (
              <Badge
                variant="outline"
                className="inline-flex items-center gap-1 text-[10px]"
              >
                <Zap className="size-3" />
                {t("regexHooks.badgeContinue")}
              </Badge>
            )}
            {!hook.enabled && (
              <Badge variant="secondary" className="text-[10px]">
                {t("regexHooks.badgeDisabled")}
              </Badge>
            )}
          </div>
          <div className="flex items-start gap-1.5 text-xs text-muted-foreground">
            <RegexIcon className="size-3.5 mt-0.5 shrink-0" />
            <code className="font-mono text-[11px] break-all">
              {hook.pattern}
            </code>
          </div>
          <div className="flex items-start gap-1.5 text-xs text-muted-foreground">
            <Terminal className="size-3.5 mt-0.5 shrink-0" />
            <code className="font-mono text-[11px] break-all">
              {hook.cliCommand}
            </code>
          </div>
        </div>
        <div className="flex items-center gap-1 shrink-0">
          <Switch
            checked={hook.enabled}
            onCheckedChange={(v) => onToggle(v)}
            aria-label={hook.enabled ? t("regexHooks.disable") : t("regexHooks.enable")}
          />
          <Button
            size="icon"
            variant="ghost"
            onClick={onEdit}
            title={t("common.edit")}
          >
            <Pencil className="size-4" />
          </Button>
          <Button
            size="icon"
            variant="ghost"
            className="text-destructive hover:text-destructive"
            onClick={onDelete}
            title={t("common.delete")}
          >
            <Trash2 className="size-4" />
          </Button>
        </div>
      </div>
    </div>
  );
}
