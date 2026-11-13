"use client";

// ShareFileDialog mints capability-limited public links (/f/{token}) for
// one workspace file: duration presets (1/3/7 days or permanent), an
// optional use cap, copy-to-clipboard, and the file's existing shares with
// per-row revoke. The token is the only credential — recipients open the
// link without logging in (smart speakers, IM contacts, email).

import { useCallback, useEffect, useState } from "react";
import { Check, Copy, Loader2, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { deleteFileShare, listAgentFileShares, shareAgentFile, type FileShare } from "@/lib/api";
import { useT } from "@/lib/i18n";

// Duration presets in hours; "0" = permanent. Mirrors the share_file tool's
// semantics (0 is a legal explicit value, not "unset").
const DURATIONS: { key: string; hours: number }[] = [
  { key: "1d", hours: 24 },
  { key: "3d", hours: 72 },
  { key: "7d", hours: 168 },
  { key: "forever", hours: 0 },
];

export function ShareFileDialog({
  agentId,
  file,
  onOpenChange,
}: {
  agentId: string;
  file: { path: string } | null;
  onOpenChange: (v: boolean) => void;
}) {
  const t = useT();
  const [duration, setDuration] = useState("7d");
  const [maxUses, setMaxUses] = useState("");
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<{ url: string } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [shares, setShares] = useState<FileShare[]>([]);
  const [copied, setCopied] = useState(false);

  const loadShares = useCallback(() => {
    listAgentFileShares(agentId)
      .then((all) => setShares(all.filter((s) => s.path === file?.path)))
      .catch(() => setShares([]));
  }, [agentId, file?.path]);

  // Per-file state (created link, error) resets via the parent's key remount
  // — no synchronous setState in this effect.
  useEffect(() => {
    if (file) loadShares();
  }, [file, loadShares]);

  if (!file) return null;
  const basename = file.path.split("/").pop() || file.path;

  const create = async () => {
    const preset = DURATIONS.find((d) => d.key === duration) ?? DURATIONS[2];
    const uses = parseInt(maxUses, 10);
    setCreating(true);
    setError(null);
    try {
      const res = await shareAgentFile(agentId, file.path, {
        ttlHours: preset.hours,
        maxUses: Number.isFinite(uses) && uses > 0 ? uses : 0,
      });
      if (!res.ok || !res.token) {
        setError(res.error || `HTTP error`);
        return;
      }
      // The URL prefers the server-configured FLUCTIO_PUBLIC_BASE_URL,
      // else falls back to the request Host — which a reverse proxy may
      // have rewritten to localhost. When the minted URL names loopback
      // but the dashboard itself was reached from a real origin, the
      // page origin is the address the recipient can actually reach.
      const hostIsLoopback = (u: string) =>
        /^https?:\/\/(localhost|127\.0\.0\.1|\[::1\])(:|\/|$)/i.test(u);
      let url = res.url?.startsWith("http")
        ? res.url
        : `${window.location.origin}/f/${res.token}`;
      if (hostIsLoopback(url) && !hostIsLoopback(window.location.origin)) {
        url = `${window.location.origin}/f/${res.token}`;
      }
      setCreated({ url });
      setCopied(false);
      loadShares();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setCreating(false);
    }
  };

  const revoke = async (token: string) => {
    try {
      await deleteFileShare(token);
    } finally {
      loadShares();
    }
  };

  const copy = async () => {
    if (!created) return;
    await navigator.clipboard.writeText(created.url);
    setCopied(true);
  };

  return (
    <Dialog open={!!file} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("share.dialogTitle")}</DialogTitle>
          <DialogDescription>
            {t("share.dialogDesc", { name: basename })}
          </DialogDescription>
        </DialogHeader>
        {/* min-w-0: the grid-implemented DialogContent sizes this item to
            its min-content by default; one unbroken ~90-char URL would
            stretch the grid track past the dialog edge. */}
        <div className="min-w-0 space-y-4 py-1">
          {/* Duration presets — segmented buttons, same dialect as the
              Changed/All files switch in chat-workspace. */}
          <div className="space-y-1.5">
            <p className="text-xs font-medium text-muted-foreground">{t("share.duration")}</p>
            <div className="flex items-center gap-1">
              {DURATIONS.map((d) => (
                <button
                  key={d.key}
                  onClick={() => setDuration(d.key)}
                  className={`rounded-md px-2.5 py-1 text-sm transition-colors ${
                    duration === d.key
                      ? "bg-muted font-medium text-foreground"
                      : "text-muted-foreground hover:text-foreground"
                  }`}
                >
                  {t(`share.${d.key}`)}
                </button>
              ))}
            </div>
          </div>
          <div className="space-y-1.5">
            <label htmlFor="share-max-uses" className="block text-xs font-medium text-muted-foreground">
              {t("share.maxUses")}
            </label>
            <input
              id="share-max-uses"
              type="number"
              min={1}
              value={maxUses}
              onChange={(e) => setMaxUses(e.target.value)}
              placeholder={t("share.unlimited")}
              className="w-full rounded-md border border-border bg-transparent px-2.5 py-1.5 text-sm outline-none focus:ring-1 focus:ring-ring"
            />
            <p className="text-2xs text-muted-foreground/70">{t("share.maxUsesHint")}</p>
          </div>

          {error && <p className="text-sm text-destructive">{t("share.createFailed", { error })}</p>}

          {created && (
            <div className="space-y-2 rounded-lg border border-border bg-muted/30 p-3">
              <p className="text-xs text-muted-foreground">{t("share.linkReady")}</p>
              <div className="flex items-center gap-2">
                <code className="min-w-0 flex-1 truncate rounded bg-background px-2 py-1.5 font-mono text-xs">
                  {created.url}
                </code>
                <Button size="sm" variant="outline" onClick={copy} className="shrink-0">
                  {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                  {copied ? t("share.copied") : t("share.copy")}
                </Button>
              </div>
            </div>
          )}

          {shares.length > 0 && (
            <div className="space-y-1.5">
              <p className="text-xs font-medium text-muted-foreground">{t("share.existing")}</p>
              <ul className="space-y-1">
                {shares.map((s) => (
                  <li
                    key={s.token}
                    className="flex items-center justify-between gap-2 rounded-md border border-border px-2.5 py-1.5 text-xs"
                  >
                    <span className="min-w-0 truncate text-muted-foreground">
                      {describeShare(t, s)}
                    </span>
                    <button
                      onClick={() => void revoke(s.token)}
                      className="shrink-0 rounded p-1 text-muted-foreground/60 transition-colors hover:bg-muted hover:text-destructive"
                      title={t("share.revoke")}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </button>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
        <DialogFooter>
          <Button onClick={create} disabled={creating}>
            {creating && <Loader2 className="h-4 w-4 animate-spin" />}
            {t("share.create")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// describeShare renders one existing row: validity window + use budget.
function describeShare(
  t: (k: string, params?: Record<string, string | number>) => string,
  s: FileShare,
): string {
  const now = Date.now();
  const expired = s.expiresAt !== null && new Date(s.expiresAt).getTime() < now;
  let when: string;
  if (expired) {
    when = t("share.expired");
  } else if (s.expiresAt === null) {
    when = t("share.permanent");
  } else {
    when = t("share.expiresAt", { time: new Date(s.expiresAt).toLocaleString() });
  }
  if (s.maxUses > 0) {
    const left = Math.max(0, s.maxUses - s.useCount);
    return `${when} · ${t("share.usesLeft", { n: left })}`;
  }
  return when;
}
