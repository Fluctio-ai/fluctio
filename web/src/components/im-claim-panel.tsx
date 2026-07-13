"use client";

import { useCallback, useEffect, useState } from "react";
import { Loader2, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { ChannelIcon } from "@/components/channel-icon";
import { useT } from "@/lib/i18n";
import {
  getAgentConfig,
  createAgentIMClaim,
  unbindAgentIM,
  rebindAgentIM,
  type AgentChannel,
} from "@/lib/api";

// ImOwnerClaimSection manages the IM admin-identity claim flow: list each
// connected IM channel, show its bound admin platform IDs, and offer
// Generate code (mint) / Rebind (void + new code) / Unbind (remove one ID).
export function ImOwnerClaimSection({
  agentId,
  channels,
}: {
  agentId: string;
  channels: AgentChannel[];
}) {
  const t = useT();
  const [admins, setAdmins] = useState<Record<string, string[]>>({});
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [claim, setClaim] = useState<{ channel: string; code: string; expiresAt: number } | null>(null);

  const refresh = useCallback(() => {
    if (!agentId) return;
    getAgentConfig(agentId)
      .then((cfg) => setAdmins(cfg?.admins || {}))
      .catch(() => {})
      .finally(() => setLoaded(true));
  }, [agentId]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const imChannels = channels.filter((c) => c.type !== "web" && c.type !== "api");
  if (imChannels.length === 0) return null;

  const startClaim = async (channel: string) => {
    setBusy(channel);
    setError("");
    const res = await createAgentIMClaim(agentId, channel);
    setBusy("");
    if (res.error || !res.code) {
      setError(res.error || "Failed");
      return;
    }
    setClaim({ channel, code: res.code, expiresAt: Date.parse(res.expiresAt || "") || 0 });
  };

  const rebind = async (channel: string) => {
    setBusy("rebind:" + channel);
    setError("");
    const res = await rebindAgentIM(agentId, channel);
    setBusy("");
    if (res.error || !res.code) {
      setError(res.error || "Failed");
      return;
    }
    setAdmins((m) => {
      const next = { ...m };
      delete next[channel];
      return next;
    });
    setClaim({ channel, code: res.code, expiresAt: Date.parse(res.expiresAt || "") || 0 });
  };

  const unbind = async (channel: string, platformId: string) => {
    setBusy("unbind:" + channel + platformId);
    setError("");
    const res = await unbindAgentIM(agentId, channel, platformId);
    setBusy("");
    if (res.error) {
      setError(res.error);
      return;
    }
    refresh();
  };

  return (
    <div className="space-y-4">
      <div>
        <h3 className="text-lg font-semibold tracking-tight">{t("channels.claim.title")}</h3>
        <p className="text-sm text-muted-foreground mt-1 max-w-2xl">{t("channels.claim.subtitle")}</p>
      </div>
      {error && (
        <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-3">
          <p className="text-xs text-destructive">{error}</p>
        </div>
      )}
      {!loaded ? (
        <Skeleton className="h-24" />
      ) : (
        <div className="space-y-3">
          {imChannels.map((c) => {
            const ids = admins[c.type] || [];
            return (
              <div key={c.type} className="rounded-lg border bg-card p-4 space-y-3">
                <div className="flex items-center justify-between gap-2">
                  <div className="flex items-center gap-2">
                    <ChannelIcon channel={c.type} />
                    <span className="font-medium capitalize">{c.type}</span>
                    {ids.length > 0 && (
                      <span className="rounded-full bg-emerald-500/10 px-1.5 py-0.5 text-[11px] font-medium text-emerald-600 dark:text-emerald-400 tabular-nums">
                        {ids.length}
                      </span>
                    )}
                  </div>
                  <div className="flex gap-2">
                    {ids.length === 0 ? (
                      <Button size="sm" variant="outline" onClick={() => startClaim(c.type)} disabled={!!busy}>
                        {busy === c.type && <Loader2 className="h-3.5 w-3.5 animate-spin" />}
                        {t("channels.claim.claimBtn")}
                      </Button>
                    ) : (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => {
                          if (window.confirm(t("channels.claim.rebindConfirm"))) rebind(c.type);
                        }}
                        disabled={!!busy}
                      >
                        {t("channels.claim.rebindBtn")}
                      </Button>
                    )}
                  </div>
                </div>
                {ids.length === 0 ? (
                  <p className="text-xs text-muted-foreground">{t("channels.claim.noneClaimed")}</p>
                ) : (
                  <ul className="space-y-1">
                    {ids.map((id) => (
                      <li key={id} className="flex items-center justify-between gap-2 text-xs">
                        <code className="font-mono text-muted-foreground truncate">{id}</code>
                        <Button
                          size="sm"
                          variant="ghost"
                          className="text-destructive hover:text-destructive h-6 px-2"
                          onClick={() => unbind(c.type, id)}
                          disabled={!!busy}
                        >
                          <Trash2 className="h-3 w-3" />
                        </Button>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            );
          })}
        </div>
      )}
      <ClaimCodeDialog open={!!claim} onOpenChange={(v) => !v && setClaim(null)} data={claim} />
    </div>
  );
}

function ClaimCodeDialog({
  open,
  onOpenChange,
  data,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  data: { channel: string; code: string; expiresAt: number } | null;
}) {
  const t = useT();
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!open) return;
    setNow(Date.now());
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [open]);
  if (!data) return null;
  const secsLeft = Math.max(0, Math.floor((data.expiresAt - now) / 1000));
  const expired = secsLeft <= 0;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("channels.claim.dialogTitle")}</DialogTitle>
          <DialogDescription>{t("channels.claim.dialogDesc")}</DialogDescription>
        </DialogHeader>
        <div className="space-y-3 py-2">
          <div className="rounded-lg border bg-muted/30 p-4 text-center">
            <div className="text-3xl font-mono font-bold tracking-[0.3em]">{data.code}</div>
            <div className="text-xs text-muted-foreground mt-2">
              {expired ? (
                t("channels.claim.expired")
              ) : (
                <>
                  {t("channels.claim.expiresIn")} {Math.floor(secsLeft / 60)}:
                  {String(secsLeft % 60).padStart(2, "0")}
                </>
              )}
            </div>
          </div>
          <p className="text-sm text-muted-foreground">
            {t("channels.claim.sendInstruction", { channel: data.channel })}{" "}
            <code className="font-mono">/claim {data.code}</code>
          </p>
        </div>
        <DialogFooter>
          <Button onClick={() => onOpenChange(false)}>{t("channels.doneBtn")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
