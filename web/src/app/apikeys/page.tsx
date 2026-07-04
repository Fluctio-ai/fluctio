"use client";

import { useEffect, useState } from "react";
import { useT } from "@/lib/i18n";
import {
  listApikeys,
  createApikey,
  deleteApikey,
  rotateApikey,
} from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
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
import { KeyRound, RotateCw, Trash2, Copy, Check, Plus } from "lucide-react";

interface ApiKey {
  id: string;
  userId: string;
  name?: string;
  key: string;
  createdAt: string;
}

export default function ApikeysPage() {
  const t = useT();
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [error, setError] = useState("");
  const [createName, setCreateName] = useState("");
  const [showToken, setShowToken] = useState<{ id: string; token: string } | null>(null);
  const [copied, setCopied] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<ApiKey | null>(null);
  const [rotateTarget, setRotateTarget] = useState<ApiKey | null>(null);
  const [createOpen, setCreateOpen] = useState(false);

  async function refresh() {
    setError("");
    const r = await listApikeys();
    if (r.apikeys) setKeys(r.apikeys);
    if (r.error) setError(r.error);
  }
  useEffect(() => {
    refresh();
  }, []);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    if (!createName.trim()) return;
    const res = await createApikey({
      name: createName.trim(),
    });
    if (res.error) {
      setError(res.error);
      return;
    }
    if (res.token) setShowToken({ id: res.apikey.id, token: res.token });
    setCreateName("");
    setCreateOpen(false);
    refresh();
  }

  async function handleDelete(row: ApiKey) {
    const res = await deleteApikey(row.id);
    if (res.error) setError(res.error);
    setDeleteTarget(null);
    refresh();
  }

  async function handleRotate(id: string) {
    const res = await rotateApikey(id);
    if (res.error) {
      setError(res.error);
      setRotateTarget(null);
      return;
    }
    if (res.token) setShowToken({ id, token: res.token });
    setRotateTarget(null);
    refresh();
  }

  async function copyToken() {
    if (!showToken) return;
    await navigator.clipboard.writeText(showToken.token);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  }

  function openCreateDialog() {
    setCreateName("");
    setError("");
    setCreateOpen(true);
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">{t("apikeys.page.title")}</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {t("apikeys.page.subtitle")}
          </p>
        </div>
        <Button onClick={openCreateDialog}>
          <Plus className="h-4 w-4 mr-2" />
          {t("apikeys.page.add")}
        </Button>
      </div>

      {showToken && (
        <div className="rounded-lg border border-warning/40 bg-warning/5 p-4 space-y-3">
          <p className="text-sm font-medium">{t("apikeys.page.tokenIssued")}</p>
          <div className="flex items-center gap-2">
            <code className="flex-1 break-all rounded border bg-background px-3 py-2 font-mono text-xs">
              {showToken.token}
            </code>
            <Button size="sm" variant="outline" onClick={copyToken}>
              {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
            </Button>
          </div>
          <Button size="sm" variant="ghost" onClick={() => setShowToken(null)}>
            {t("apikeys.page.gotIt")}
          </Button>
        </div>
      )}

      {error && (
        <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-4">
          <p className="text-sm text-destructive">{error}</p>
        </div>
      )}

      {keys.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <KeyRound className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">{t("apikeys.page.empty")}</p>
            <p className="text-xs text-muted-foreground/60 mb-4">
              {t("apikeys.page.emptyDesc")}
            </p>
            <Button variant="outline" size="sm" onClick={openCreateDialog}>
              <Plus className="h-4 w-4 mr-2" />
              {t("apikeys.page.add")}
            </Button>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("apikeys.page.col.name")}</TableHead>
                <TableHead>{t("apikeys.page.col.key")}</TableHead>
                <TableHead>{t("apikeys.page.col.access")}</TableHead>
                <TableHead>{t("apikeys.page.col.created")}</TableHead>
                <TableHead className="text-right">{t("apikeys.page.col.actions")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.map((k) => (
                <TableRow key={k.id}>
                  <TableCell className="font-medium">{k.name || k.id}</TableCell>
                  <TableCell>
                    <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{k.key}</code>
                  </TableCell>
                  <TableCell>
                    <span className="text-xs text-muted-foreground">{t("apikeys.page.accessOwner")}</span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {new Date(k.createdAt).toLocaleString()}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button size="icon" variant="ghost" onClick={() => setRotateTarget(k)} title={t("apikeys.page.rotate")}>
                        <RotateCw className="size-4" />
                      </Button>
                      <Button
                        size="icon"
                        variant="ghost"
                        className="text-destructive hover:text-destructive"
                        onClick={() => setDeleteTarget(k)}
                        title={t("common.delete")}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t("apikeys.page.add")}</DialogTitle>
            <DialogDescription>
              {t("apikeys.page.createDesc")}
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleCreate} className="space-y-4 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="key-name">{t("apikeys.page.col.name")}</Label>
              <Input
                id="key-name"
                value={createName}
                onChange={(e) => setCreateName(e.target.value)}
                placeholder={t("apikeys.page.namePlaceholder")}
                autoFocus
              />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>
                {t("common.cancel")}
              </Button>
              <Button type="submit" disabled={!createName.trim()}>
                {t("apikeys.page.create")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={deleteTarget !== null} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("apikeys.page.deleteTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("apikeys.page.deleteDesc", { name: deleteTarget?.name || deleteTarget?.id || "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => deleteTarget && handleDelete(deleteTarget)}>
              {t("common.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={rotateTarget !== null} onOpenChange={(o) => !o && setRotateTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("apikeys.page.rotateTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("apikeys.page.rotateDesc", { name: rotateTarget?.name || rotateTarget?.id || "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => rotateTarget && handleRotate(rotateTarget.id)}>
              {t("apikeys.page.rotate")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
