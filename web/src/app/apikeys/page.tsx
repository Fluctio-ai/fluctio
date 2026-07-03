"use client";

import { useEffect, useState } from "react";
import {
  listApikeys,
  createApikey,
  deleteApikey,
  rotateApikey,
} from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
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
          <h2 className="text-2xl font-semibold tracking-tight">API Keys</h2>
          <p className="text-sm text-muted-foreground mt-1">
            Issue programmatic credentials. Each key is scoped to a subset of your agents.
          </p>
        </div>
        <Button onClick={openCreateDialog}>
          <Plus className="h-4 w-4 mr-2" />
          Add API Key
        </Button>
      </div>

      {showToken && (
        <Card className="border-warning/40 bg-warning/5">
          <CardContent className="space-y-3 pt-6">
            <p className="text-sm font-medium">Token issued — copy it now, you won&apos;t see it again.</p>
            <div className="flex items-center gap-2">
              <code className="flex-1 break-all rounded border bg-background px-3 py-2 font-mono text-xs">
                {showToken.token}
              </code>
              <Button size="sm" variant="outline" onClick={copyToken}>
                {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
              </Button>
            </div>
            <Button size="sm" variant="ghost" onClick={() => setShowToken(null)}>
              Got it
            </Button>
          </CardContent>
        </Card>
      )}

      {error && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="pt-6">
            <p className="text-sm text-destructive">{error}</p>
          </CardContent>
        </Card>
      )}

      {keys.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <KeyRound className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">No API keys yet</p>
            <p className="text-xs text-muted-foreground/60 mb-4">
              Issue one to let an external client call your agents
            </p>
            <Button variant="outline" size="sm" onClick={openCreateDialog}>
              <Plus className="h-4 w-4 mr-2" />
              Add API Key
            </Button>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Key</TableHead>
                <TableHead>Access</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="text-right">Actions</TableHead>
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
                    <span className="text-xs text-muted-foreground">All agents (owner-level)</span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {new Date(k.createdAt).toLocaleString()}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button size="icon" variant="ghost" onClick={() => setRotateTarget(k)} title="Rotate">
                        <RotateCw className="size-4" />
                      </Button>
                      <Button
                        size="icon"
                        variant="ghost"
                        className="text-destructive hover:text-destructive"
                        onClick={() => setDeleteTarget(k)}
                        title="Delete"
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
            <DialogTitle>Add API Key</DialogTitle>
            <DialogDescription>
              Issue a new bearer token scoped to a subset of your agents.
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleCreate} className="space-y-4 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="key-name">Name</Label>
              <Input
                id="key-name"
                value={createName}
                onChange={(e) => setCreateName(e.target.value)}
                placeholder="e.g. thinkany-web"
                autoFocus
              />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={!createName.trim()}>
                Create key
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={deleteTarget !== null} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete API key?</AlertDialogTitle>
            <AlertDialogDescription>
              <code className="rounded bg-muted px-1.5 py-0.5 text-xs">{deleteTarget?.name || deleteTarget?.id}</code>{" "}
              will stop working immediately for any client using it.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => deleteTarget && handleDelete(deleteTarget)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={rotateTarget !== null} onOpenChange={(o) => !o && setRotateTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Rotate API key?</AlertDialogTitle>
            <AlertDialogDescription>
              The current token for{" "}
              <code className="rounded bg-muted px-1.5 py-0.5 text-xs">{rotateTarget?.name || rotateTarget?.id}</code>{" "}
              will stop working immediately. A new token will be issued and shown once.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => rotateTarget && handleRotate(rotateTarget.id)}>
              Rotate
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
