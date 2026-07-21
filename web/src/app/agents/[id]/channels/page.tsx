"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
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
import {
  Radio,
  Plus,
  Trash2,
  Send,
  CheckCircle2,
  ExternalLink,
  Loader2,
  QrCode,
  AlertCircle,
  RotateCcw,
} from "lucide-react";
import {
  listAgentChannels,
  connectAgentTelegram,
  connectAgentDiscord,
  connectAgentSlack,
  connectAgentLINE,
  connectAgentFeishu,
  connectAgentQQ,
  startAgentWeChatLogin,
  pollAgentWeChatLoginStatus,
  disconnectAgentChannel,
  retryAgentChannel,
  type AgentChannel,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";
import { useT } from "@/lib/i18n";
import { ImOwnerClaimSection } from "@/components/im-claim-panel";

// Channels page: per-agent IM bot bindings. One card per channel type
// in the catalog — connected types show bot info + Disconnect, others
// show a Connect button. The backend supports multiple bots per type;
// the UI intentionally surfaces only the first binding for now to keep
// the mental model simple (one bot per channel per agent). When we add
// multi-bot management later, this card can expand to a list.

const CATALOG: { type: string; label: string; description: string; available: boolean }[] = [
  {
    type: "telegram",
    label: "Telegram",
    description: "channels.telegramDesc",
    available: true,
  },
  {
    type: "discord",
    label: "Discord",
    description: "channels.discordDesc",
    available: true,
  },
  {
    type: "slack",
    label: "Slack",
    description: "channels.slackDesc",
    available: true,
  },
  {
    type: "line",
    label: "LINE",
    description: "channels.lineDesc",
    available: true,
  },
  {
    type: "wechat",
    label: "WeChat",
    description: "channels.wechatDesc",
    available: true,
  },
  {
    type: "feishu",
    label: "Feishu",
    description: "channels.feishuDesc",
    available: true,
  },
  {
    type: "qq",
    label: "QQ",
    description: "channels.qqDesc",
    available: true,
  },
];

export default function AgentChannelsPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const t = useT();

  const [channels, setChannels] = useState<AgentChannel[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const [telegramOpen, setTelegramOpen] = useState(false);
  const [discordOpen, setDiscordOpen] = useState(false);
  const [slackOpen, setSlackOpen] = useState(false);
  const [lineOpen, setLineOpen] = useState(false);
  const [wechatOpen, setWechatOpen] = useState(false);
  const [feishuOpen, setFeishuOpen] = useState(false);
  const [qqOpen, setQqOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<AgentChannel | null>(null);

  const refresh = useCallback(() => {
    if (!agentId) return;
    setLoading(true);
    listAgentChannels(agentId)
      .then((list) => setChannels(list))
      .catch((e) => setError(e instanceof Error ? e.message : t("channels.failedToLoad")))
      .finally(() => setLoading(false));
  }, [agentId, t]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // First binding per channel type — the UI is currently single-bot,
  // even though the backend allows multiple. If multiple exist (legacy
  // data), the rest are still wired up server-side, just hidden here.
  const byType = useMemo(() => {
    const m: Record<string, AgentChannel> = {};
    for (const ch of channels) {
      if (!m[ch.type]) m[ch.type] = ch;
    }
    return m;
  }, [channels]);

  const handleDelete = async () => {
    if (!deleteTarget || !agentId) return;
    const target = deleteTarget;
    setDeleteTarget(null);
    const res = await disconnectAgentChannel(agentId, target.type, target.accountId);
    if (res.error) setError(res.error);
    refresh();
  };

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <div className="flex items-center gap-2">
            <Radio className="size-5 text-muted-foreground" />
            <h2 className="text-2xl font-semibold tracking-tight">{t("channels.channelsTitle")}</h2>
          </div>
          <p className="text-sm text-muted-foreground mt-1">
            {t("channels.channelsSubtitle")} <strong>{agentName || t("channels.thisAgent")}</strong>{" "}
            {t("channels.channelsSubtitleSuffix")}
          </p>
        </div>
      </div>

      <ImOwnerClaimSection agentId={agentId} channels={channels} />

      {error && (
        <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-4">
          <p className="text-sm text-destructive">{error}</p>
        </div>
      )}

      {loading ? (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          <Skeleton className="h-40" />
          <Skeleton className="h-40" />
          <Skeleton className="h-40" />
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {CATALOG.map((entry) => {
            const connected = byType[entry.type];
            return connected ? (
              <ConnectedCard
                key={entry.type}
                label={entry.label}
                channel={connected}
                onDelete={() => setDeleteTarget(connected)}
                onRetry={
                  connected.failureType
                    ? async () => {
                        const res = await retryAgentChannel(
                          agentId,
                          connected.type,
                          connected.accountId,
                        );
                        if (res.error) setError(res.error);
                        refresh();
                      }
                    : undefined
                }
                onReconnect={
                  connected.failureType && connected.type === "wechat"
                    ? async () => {
                        // Drop the dead failed account first (a rescan
                        // mints a fresh accountID, so the old row would
                        // otherwise linger as failed), then open the QR.
                        const res = await disconnectAgentChannel(
                          agentId,
                          "wechat",
                          connected.accountId,
                        );
                        if (res.error) setError(res.error);
                        setWechatOpen(true);
                        refresh();
                      }
                    : undefined
                }
              />
            ) : (
              <CatalogCard
                key={entry.type}
                type={entry.type}
                label={entry.label}
                description={entry.description}
                available={entry.available}
                onConnect={() => {
                  if (entry.type === "telegram") setTelegramOpen(true);
                  else if (entry.type === "discord") setDiscordOpen(true);
                  else if (entry.type === "slack") setSlackOpen(true);
                  else if (entry.type === "line") setLineOpen(true);
                  else if (entry.type === "wechat") setWechatOpen(true);
                  else if (entry.type === "feishu") setFeishuOpen(true);
                  else if (entry.type === "qq") setQqOpen(true);
                }}
              />
            );
          })}
        </div>
      )}

      <ConnectTelegramDialog
        open={telegramOpen}
        onOpenChange={setTelegramOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectDiscordDialog
        open={discordOpen}
        onOpenChange={setDiscordOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectSlackDialog
        open={slackOpen}
        onOpenChange={setSlackOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectLINEDialog
        open={lineOpen}
        onOpenChange={setLineOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectWeChatDialog
        open={wechatOpen}
        onOpenChange={setWechatOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectFeishuDialog
        open={feishuOpen}
        onOpenChange={setFeishuOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectQQDialog
        open={qqOpen}
        onOpenChange={setQqOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <AlertDialog open={!!deleteTarget} onOpenChange={(v) => !v && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("channels.disconnectTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("channels.disconnectNamePrefix")}{" "}
              <strong>
                {deleteTarget?.botUsername || deleteTarget?.accountId || deleteTarget?.type}
              </strong>
              ? {t("channels.disconnectDesc")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {t("channels.disconnectBtn")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function CatalogCard({
  type,
  label,
  description,
  available,
  onConnect,
}: {
  type: string;
  label: string;
  description: string;
  available: boolean;
  onConnect: () => void;
}) {
  const t = useT();
  return (
    <div className="rounded-lg border border-border bg-card p-4 flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <ChannelIcon type={type} />
        <span className="font-medium">{label}</span>
      </div>
      <p className="text-xs text-muted-foreground flex-1">{t(description)}</p>
      <Button
        size="sm"
        variant={available ? "outline" : "ghost"}
        disabled={!available}
        onClick={onConnect}
        className="w-full"
      >
        <Plus className="h-3.5 w-3.5 mr-1.5" />
        {available ? t("channels.connectBtn") : t("channels.comingSoon")}
      </Button>
    </div>
  );
}

function ConnectedCard({
  label,
  channel,
  onDelete,
  onRetry,
  onReconnect,
}: {
  label: string;
  channel: AgentChannel;
  onDelete: () => void;
  onRetry?: () => void;
  onReconnect?: () => void;
}) {
  const t = useT();
  // Telegram is the only provider with a public profile URL pattern
  // (t.me/<username>); Discord/Slack don't expose one from a bot
  // username alone, so we render plain text for those.
  const botLink =
    channel.type === "telegram" && channel.botUsername
      ? `https://t.me/${channel.botUsername}`
      : null;

  return (
    <div className="rounded-lg border border-border bg-card p-4 flex flex-col gap-3">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 min-w-0">
          <ChannelIcon type={channel.type} />
          <span className="font-medium truncate">{label}</span>
        </div>
        {channel.failureType ? (
          <span className="inline-flex items-center gap-1 text-xs text-destructive">
            <AlertCircle className="h-3 w-3" />
            {t("channels.failed")}
          </span>
        ) : channel.enabled ? (
          <span className="inline-flex items-center gap-1 text-xs text-success">
            <CheckCircle2 className="h-3 w-3" />
            {t("channels.connected")}
          </span>
        ) : null}
      </div>

      <div className="flex-1 space-y-1.5 min-w-0">
        {channel.botUsername && (
          botLink ? (
            <a
              href={botLink}
              target="_blank"
              rel="noreferrer"
              className="text-xs text-muted-foreground hover:text-foreground inline-flex items-center gap-1 truncate max-w-full"
            >
              @{channel.botUsername}
              <ExternalLink className="h-3 w-3 shrink-0" />
            </a>
          ) : (
            <p className="text-xs text-muted-foreground truncate">
              @{channel.botUsername}
            </p>
          )
        )}
        <code className="text-xs text-muted-foreground/80 font-mono truncate block">
          {channel.botToken}
        </code>
        {channel.failureType && (
          <p className="text-xs text-destructive flex items-center gap-1.5">
            <AlertCircle className="h-3 w-3 shrink-0" />
            {t(`channels.failure.${channel.failureType}`)}
          </p>
        )}
      </div>

      {channel.failureType && (
        <div className="flex gap-2">
          {onRetry && (
            <Button size="sm" variant="outline" onClick={onRetry} className="flex-1">
              <RotateCcw className="h-3.5 w-3.5 mr-1.5" />
              {t("channels.retryBtn")}
            </Button>
          )}
          {onReconnect && (
            <Button size="sm" variant="outline" onClick={onReconnect} className="flex-1">
              {t("channels.reconnectBtn")}
            </Button>
          )}
        </div>
      )}

      <Button
        size="sm"
        variant="outline"
        onClick={onDelete}
        className="w-full text-destructive hover:text-destructive hover:bg-destructive/5"
      >
        <Trash2 className="h-3.5 w-3.5 mr-1.5" />
        {t("channels.disconnectBtn")}
      </Button>
    </div>
  );
}

function ChannelIcon({ type }: { type: string }) {
  // Brand SVG/PNG assets live in /public/channels — copied from the
  // workany-web icon set. We size them at 16x16 to match the lucide
  // icons they replace; the asset's intrinsic colors carry the brand
  // tint so we don't need a `text-*` class. WeChat has no asset yet so
  // it falls through to the lucide MessageSquare in emerald.
  const asset: Record<string, string> = {
    telegram: "/channels/telegram.svg",
    discord: "/channels/discord.svg",
    slack: "/channels/slack.svg",
    line: "/channels/line.png",
    feishu: "/channels/feishu.png",
    wechat: "/channels/wechat.svg",
    qq: "/channels/qq.svg",
  };
  if (asset[type]) {
    // WeChat's artwork is non-square (50×40) — object-contain letterboxes
    // it inside the 16×16 box, leaving a visible gap on top/bottom. Scale
    // up just this one so it reads at the same visual weight as the
    // square brand icons next to it.
    const extra = type === "wechat" ? "scale-150" : "";
    return (
      <img
        src={asset[type]}
        alt={type}
        className={`h-4 w-4 object-contain ${extra}`}
      />
    );
  }
  return <Radio className="h-4 w-4 text-muted-foreground" />;
}

function ConnectTelegramDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const [token, setToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const t = useT();
  const [connected, setConnected] = useState<{ botUsername: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setToken("");
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!token.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentTelegram(agentId, token.trim());
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || t("channels.connectFailed"));
      return;
    }
    setConnected({ botUsername: res.botUsername || "" });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/telegram.svg" alt="Telegram" className="h-5 w-5 object-contain" />
            {t("channels.connectTelegramBot")}
          </DialogTitle>
          <DialogDescription>
            {t("channels.tgDesc1")}{" "}
            <a
              href="https://t.me/BotFather"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              @BotFather
            </a>{" "}
            {t("channels.tgDesc2")} <code>/newbot</code>
            {t("channels.tgDesc3")} <code>getMe</code>
            {t("channels.tgDesc4")}
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-success/30 bg-success/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-success" />
              <span className="text-sm font-medium">{t("channels.connected")}</span>
            </div>
            <p className="text-sm">
              {t("channels.botLiveAs")}{" "}
              <a
                href={`https://t.me/${connected.botUsername}`}
                target="_blank"
                rel="noreferrer"
                className="font-mono text-info hover:underline inline-flex items-center gap-1"
              >
                @{connected.botUsername}
                <ExternalLink className="h-3 w-3" />
              </a>
              . {t("channels.tgLiveSuffix")}
            </p>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="bot-token">{t("channels.botToken")}</Label>
              <Input
                id="bot-token"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="123456789:ABCdef..."
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            {error && (
              <p className="text-xs text-destructive">{error}</p>
            )}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>{t("channels.doneBtn")}</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                {t("common.cancel")}
              </Button>
              <Button onClick={submit} disabled={submitting || !token.trim()}>
                {submitting ? t("channels.connecting") : t("channels.connectBtn")}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ConnectDiscordDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const [token, setToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const t = useT();
  const [connected, setConnected] = useState<{ botUsername: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setToken("");
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!token.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentDiscord(agentId, token.trim());
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || t("channels.connectFailed"));
      return;
    }
    setConnected({ botUsername: res.botUsername || "" });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/discord.svg" alt="Discord" className="h-5 w-5 object-contain" />
            {t("channels.connectDiscordBot")}
          </DialogTitle>
          <DialogDescription>
            {t("channels.dcDesc1")}{" "}
            <a
              href="https://discord.com/developers/applications"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              Discord Developer Portal
            </a>
            {t("channels.dcDesc2")} <strong>MESSAGE CONTENT INTENT</strong>
            {t("channels.dcDesc3")} <code>/users/@me</code>
            {t("channels.dcDesc4")}
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-success/30 bg-success/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-success" />
              <span className="text-sm font-medium">{t("channels.connected")}</span>
            </div>
            <p className="text-sm">
              {t("channels.botLiveAs")}{" "}
              <span className="font-mono">{connected.botUsername}</span>.
              {" "}{t("channels.dcLiveSuffix")}
            </p>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="discord-bot-token">{t("channels.botToken")}</Label>
              <Input
                id="discord-bot-token"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="MTEx..."
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>{t("channels.doneBtn")}</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                {t("common.cancel")}
              </Button>
              <Button onClick={submit} disabled={submitting || !token.trim()}>
                {submitting ? t("channels.connecting") : t("channels.connectBtn")}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ConnectSlackDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const t = useT();
  const [botToken, setBotToken] = useState("");
  const [appToken, setAppToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{ teamName: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setBotToken("");
      setAppToken("");
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!botToken.trim() || !appToken.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentSlack(agentId, botToken.trim(), appToken.trim());
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || t("channels.connectFailed"));
      return;
    }
    setConnected({ teamName: res.teamName || "" });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/slack.svg" alt="Slack" className="h-5 w-5 object-contain" />
            {t("channels.connectSlackApp")}
          </DialogTitle>
          <DialogDescription>
            {t("channels.slDesc1")}{" "}
            <a
              href="https://api.slack.com/apps"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              api.slack.com/apps
            </a>
            {t("channels.slDesc2")} <strong>Socket Mode</strong>
            {t("channels.slDesc3")} <strong>app-level token</strong>{" "}
            {t("channels.slDesc4")} <code>connections:write</code>
            {t("channels.slDesc5")} <strong>OAuth & Permissions</strong>
            {t("channels.slDesc6")} <strong>Bot User OAuth Token</strong>
            {t("channels.slDesc7")} <strong>Event Subscriptions → Subscribe to bot events</strong>
            {t("channels.slDesc8")} <code>message.channels</code>, <code>message.im</code>, and{" "}
            <code>app_mention</code>{" "}
            {t("channels.slDesc9")} <code>channels:history</code>, <code>im:history</code>,{" "}
            <code>app_mentions:read</code>{" "}
            {t("channels.slDesc10")}
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-success/30 bg-success/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-success" />
              <span className="text-sm font-medium">{t("channels.connected")}</span>
            </div>
            <p className="text-sm">
              {t("channels.slLivePrefix")}{" "}
              <strong>{connected.teamName}</strong>
              {t("channels.slLiveMid")} <code>/invite @bot</code>{" "}
              {t("channels.slLiveSuffix")}
            </p>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="slack-bot-token">{t("channels.botUserOauthToken")}</Label>
              <Input
                id="slack-bot-token"
                value={botToken}
                onChange={(e) => setBotToken(e.target.value)}
                placeholder="xoxb-..."
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="slack-app-token">{t("channels.appLevelToken")}</Label>
              <Input
                id="slack-app-token"
                value={appToken}
                onChange={(e) => setAppToken(e.target.value)}
                placeholder="xapp-..."
                className="font-mono text-sm"
              />
            </div>
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>{t("channels.doneBtn")}</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                {t("common.cancel")}
              </Button>
              <Button
                onClick={submit}
                disabled={submitting || !botToken.trim() || !appToken.trim()}
              >
                {submitting ? t("channels.connecting") : t("channels.connectBtn")}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// LINE Messaging API connect dialog. Two-step UX matching Feishu:
//   1. User pastes Channel access token + Channel secret; we hit
//      /v2/bot/info to validate and capture the bot's userId.
//   2. On success, surface the public webhook URL — user pastes it
//      into LINE Developers Console under "Messaging API → Webhook URL"
//      and toggles "Use webhook" on.
function ConnectLINEDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const t = useT();
  const [channelToken, setChannelToken] = useState("");
  const [channelSecret, setChannelSecret] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{ botName: string; basicId: string; webhookUrl: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setChannelToken("");
      setChannelSecret("");
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!channelToken.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentLINE(
      agentId,
      channelToken.trim(),
      channelSecret.trim(),
    );
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || t("channels.connectFailed"));
      return;
    }
    setConnected({
      botName: res.botName || "",
      basicId: res.basicId || "",
      webhookUrl: res.webhookUrl || "",
    });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/line.png" alt="LINE" className="h-5 w-5 object-contain" />
            {t("channels.connectLineChannel")}
          </DialogTitle>
          <DialogDescription>
            {t("channels.lnDesc1")}{" "}
            <a
              href="https://developers.line.biz"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              developers.line.biz
            </a>
            {t("channels.lnDesc2")} <strong>Messaging API</strong>
            {t("channels.lnDesc3")} <strong>Channel access token</strong>
            {t("channels.lnDesc4")} <strong>Channel secret</strong>
            {t("channels.lnDesc5")} <em>Use webhook</em>
            {t("channels.lnDesc6")}
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="space-y-3 py-2">
            <div className="rounded-lg border border-success/30 bg-success/5 p-4 space-y-2">
              <div className="flex items-center gap-2">
                <CheckCircle2 className="h-4 w-4 text-success" />
                <span className="text-sm font-medium">{t("channels.credentialsValid")}</span>
              </div>
              <p className="text-sm">
                {t("channels.botIdentifiedAs")}{" "}
                <strong>{connected.botName || "(unnamed)"}</strong>{" "}
                {connected.basicId && (
                  <code className="font-mono text-xs">{connected.basicId}</code>
                )}.
              </p>
            </div>
            <div className="rounded-lg border bg-muted/30 p-4 space-y-2">
              <p className="text-sm font-medium">{t("channels.oneLastStep")}</p>
              <p className="text-xs text-muted-foreground">
                {t("channels.lnPasteDesc1")}{" "}
                <strong>Messaging API → Webhook URL</strong>
                {t("channels.lnPasteDesc2")} <em>Verify</em>
                {t("channels.lnPasteDesc3")} <strong>Use webhook</strong>
                {t("channels.lnPasteDesc4")}
              </p>
              <Input
                readOnly
                value={connected.webhookUrl}
                className="font-mono text-xs"
                onFocus={(e) => e.currentTarget.select()}
              />
              <p className="text-xs text-muted-foreground">
                {t("channels.lnFriendHint")}
              </p>
            </div>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="line-channel-token">{t("channels.channelAccessToken")}</Label>
              <Input
                id="line-channel-token"
                value={channelToken}
                onChange={(e) => setChannelToken(e.target.value)}
                placeholder={t("channels.lineTokenPlaceholder")}
                type="password"
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="line-channel-secret">{t("channels.channelSecret")}</Label>
              <Input
                id="line-channel-secret"
                value={channelSecret}
                onChange={(e) => setChannelSecret(e.target.value)}
                placeholder={t("channels.lineSecretPlaceholder")}
                className="font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                {t("channels.optionalButRecommended")}
              </p>
            </div>
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>{t("channels.doneBtn")}</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                {t("common.cancel")}
              </Button>
              <Button
                onClick={submit}
                disabled={submitting || !channelToken.trim()}
              >
                {submitting ? t("channels.validating") : t("channels.connectBtn")}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ConnectWeChatDialog drives the QR-scan login: fetch a session token,
// render its `qrCode` string as a QR image, then poll the server every
// 3s for state. The polling endpoint does ONE upstream round-trip per
// call (no long-poll on our side), so the lifecycle is purely client-
// driven — closing the dialog cleans up via the polling ref.
function ConnectWeChatDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  type WechatStatus = "wait" | "scaned" | "confirmed" | "expired" | "";
  const t = useT();
  const [qrPayload, setQrPayload] = useState("");
  const [sessionId, setSessionId] = useState("");
  const [status, setStatus] = useState<WechatStatus>("");
  const [accountId, setAccountId] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  // Cleanup on unmount and on dialog close.
  useEffect(() => () => stopPolling(), [stopPolling]);
  useEffect(() => {
    if (!open) {
      stopPolling();
      setQrPayload("");
      setSessionId("");
      setStatus("");
      setAccountId("");
      setError("");
      setLoading(false);
    }
  }, [open, stopPolling]);

  const startLogin = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    setError("");
    setStatus("");
    setAccountId("");
    setQrPayload("");
    stopPolling();
    const res = await startAgentWeChatLogin(agentId);
    setLoading(false);
    if (res.error || !res.sessionId || !res.qrCodeImg) {
      setError(res.error || t("channels.failedToFetchQR"));
      return;
    }
    setSessionId(res.sessionId);
    setQrPayload(res.qrCodeImg);
    setStatus("wait");
    pollRef.current = setInterval(async () => {
      const s = await pollAgentWeChatLoginStatus(agentId, res.sessionId!);
      if (s.error) {
        // Don't kill the loop on a single transient error — iLink's
        // status endpoint occasionally hiccups, and the next tick
        // usually recovers. Surface it as a banner only.
        setError(s.error);
        return;
      }
      setError("");
      if (s.status) setStatus(s.status as WechatStatus);
      if (s.connected) {
        stopPolling();
        if (s.accountId) setAccountId(s.accountId);
        onConnected();
      }
      if (s.status === "expired") {
        stopPolling();
      }
    }, 3000);
  }, [agentId, onConnected, stopPolling, t]);

  // Auto-fetch a QR as soon as the dialog opens (no separate "name"
  // step — fluctio doesn't surface per-account names, accountID is
  // ilink_bot_id).
  useEffect(() => {
    if (open && !qrPayload && !loading && !error) {
      startLogin();
    }
  }, [open, qrPayload, loading, error, startLogin]);

  const connected = !!accountId;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-[420px]">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/wechat.svg" alt="WeChat" className="h-5 w-5 object-contain scale-150" />
            {t("channels.connectWechat")}
          </DialogTitle>
          <DialogDescription>
            {t("channels.wcDesc")}
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-success/30 bg-success/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-success" />
              <span className="text-sm font-medium">{t("channels.connected")}</span>
            </div>
            <p className="text-sm">
              {t("channels.botLiveAs")} <code className="font-mono text-xs">{accountId}</code>.
              {" "}{t("channels.wcLiveSuffix")}
            </p>
          </div>
        ) : (
          <div className="flex flex-col items-center gap-4 py-2">
            {loading ? (
              <div className="flex h-56 w-56 items-center justify-center">
                <Loader2 className="h-8 w-8 animate-spin text-muted-foreground" />
              </div>
            ) : qrPayload ? (
              <div className="rounded-lg border bg-white p-4">
                <QRCodeSVG value={qrPayload} size={224} level="M" />
              </div>
            ) : (
              <div className="flex h-56 w-56 items-center justify-center text-sm text-muted-foreground">
                <QrCode className="h-8 w-8 opacity-50" />
              </div>
            )}

            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              {status === "wait" && <>{t("channels.waitingForScan")}</>}
              {status === "scaned" && (
                <>
                  <CheckCircle2 className="h-4 w-4 text-success" />
                  {t("channels.scannedConfirm")}
                </>
              )}
              {status === "confirmed" && (
                <>
                  <Loader2 className="h-4 w-4 animate-spin" />
                  {t("channels.connecting")}
                </>
              )}
              {status === "expired" && (
                <span className="text-destructive">{t("channels.qrExpired")}</span>
              )}
            </div>

            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>{t("channels.doneBtn")}</Button>
          ) : (
            <>
              {status === "expired" && (
                <Button onClick={startLogin} disabled={loading}>
                  {loading ? t("channels.refreshing") : t("channels.refreshQR")}
                </Button>
              )}
              <Button variant="outline" onClick={() => onOpenChange(false)}>
                {t("common.cancel")}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// Feishu / Feishu connect dialog. Two-step UX:
//   1. User pastes App ID + App Secret + Verification Token, we validate
//      via /tenant_access_token + /bot/v3/info.
//   2. On success, we surface the webhook URL — user must paste it
//      into the Feishu Developer Console under "Event Subscriptions →
//      Request URL" and re-trigger Feishu's URL verification handshake
//      from there before the bot starts receiving messages.
function ConnectFeishuDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const t = useT();
  const [appId, setAppId] = useState("");
  const [appSecret, setAppSecret] = useState("");
  const [verificationToken, setVerificationToken] = useState("");
  const [encryptKey, setEncryptKey] = useState("");
  const [useLongConn, setUseLongConn] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{
    botName: string;
    webhookUrl: string;
    useLongConn: boolean;
  } | null>(null);

  useEffect(() => {
    if (!open) {
      setAppId("");
      setAppSecret("");
      setVerificationToken("");
      setEncryptKey("");
      setUseLongConn(true);
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!appId.trim() || !appSecret.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentFeishu(
      agentId,
      appId.trim(),
      appSecret.trim(),
      verificationToken.trim(),
      encryptKey.trim(),
      useLongConn,
    );
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || t("channels.connectFailed"));
      return;
    }
    setConnected({
      botName: res.botName || "",
      webhookUrl: res.webhookUrl || "",
      useLongConn: !!res.useLongConn,
    });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/feishu.png" alt="Feishu" className="h-5 w-5 object-contain" />
            {t("channels.connectFeishuApp")}
          </DialogTitle>
          <DialogDescription>
            {t("channels.fsDesc1")}{" "}
            <a
              href="https://open.feishu.cn"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              open.feishu.cn
            </a>
            {t("channels.fsDesc2")} <code>im:message</code> + <code>im:message:send_as_bot</code>{" "}
            {t("channels.fsDesc3")} <strong>Credentials & Basic Info</strong>
            {t("channels.fsDesc4")} <strong>Event Subscriptions</strong>
            {t("channels.fsDesc5")}
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="space-y-3 py-2">
            <div className="rounded-lg border border-success/30 bg-success/5 p-4 space-y-2">
              <div className="flex items-center gap-2">
                <CheckCircle2 className="h-4 w-4 text-success" />
                <span className="text-sm font-medium">{t("channels.credentialsValid")}</span>
              </div>
              <p className="text-sm">
                {t("channels.botIdentifiedAs")}{" "}
                <strong>{connected.botName || "(unnamed)"}</strong>.
              </p>
            </div>
            {connected.useLongConn ? (
              <div className="rounded-lg border bg-muted/30 p-4 space-y-2">
                <p className="text-sm font-medium">{t("channels.longConnectionMode")}</p>
                <p className="text-xs text-muted-foreground">
                  {t("channels.fsLongConnDesc1")}{" "}
                  <strong>事件与回调 → 事件配置 → 订阅方式</strong>
                  {t("channels.fsLongConnDesc2")} <strong>使用长连接接收事件</strong>
                  {t("channels.fsLongConnDesc3")} <strong>Subscribe to bot events</strong>
                  {t("channels.fsLongConnDesc4")} <code>im.message.receive_v1</code>.
                </p>
              </div>
            ) : (
              <div className="rounded-lg border bg-muted/30 p-4 space-y-2">
                <p className="text-sm font-medium">{t("channels.oneLastStep")}</p>
                <p className="text-xs text-muted-foreground">
                  {t("channels.fsWebhookDesc1")}{" "}
                  <strong>Event Subscriptions → Request URL</strong>
                  {t("channels.fsWebhookDesc2")} <em>Save</em>
                  {t("channels.fsWebhookDesc3")}
                </p>
                <Input
                  readOnly
                  value={connected.webhookUrl}
                  className="font-mono text-xs"
                  onFocus={(e) => e.currentTarget.select()}
                />
                <p className="text-xs text-muted-foreground">
                  {t("channels.fsSubscribeHint1")} <code>im.message.receive_v1</code>{" "}
                  {t("channels.fsSubscribeHint2")}
                </p>
              </div>
            )}
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="flex items-start justify-between gap-3 rounded-lg border bg-muted/30 p-3">
              <div className="space-y-0.5">
                <Label htmlFor="feishu-long-conn" className="text-sm">
                  {t("channels.longConnectionMode")}
                </Label>
                <p className="text-xs text-muted-foreground">
                  {t("channels.longConnDesc")}
                </p>
              </div>
              <Switch
                id="feishu-long-conn"
                checked={useLongConn}
                onCheckedChange={setUseLongConn}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="feishu-app-id">{t("channels.appId")}</Label>
              <Input
                id="feishu-app-id"
                value={appId}
                onChange={(e) => setAppId(e.target.value)}
                placeholder="cli_..."
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="feishu-app-secret">{t("channels.appSecret")}</Label>
              <Input
                id="feishu-app-secret"
                value={appSecret}
                onChange={(e) => setAppSecret(e.target.value)}
                placeholder="..."
                type="password"
                className="font-mono text-sm"
              />
            </div>
            {!useLongConn && (
              <>
            <div className="space-y-1.5">
              <Label htmlFor="feishu-verification-token">{t("channels.verificationToken")}</Label>
              <Input
                id="feishu-verification-token"
                value={verificationToken}
                onChange={(e) => setVerificationToken(e.target.value)}
                placeholder={t("channels.fsVerifyTokenPlaceholder")}
                className="font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                {t("channels.fsTokenHint1")} <code>header.token</code>
                {t("channels.fsTokenHint2")}
              </p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="feishu-encrypt-key">{t("channels.encryptKey")}</Label>
              <Input
                id="feishu-encrypt-key"
                value={encryptKey}
                onChange={(e) => setEncryptKey(e.target.value)}
                placeholder={t("channels.fsEncryptKeyPlaceholder")}
                type="password"
                className="font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                {t("channels.fsEncryptHint1")}{" "}
                <strong>加密策略</strong>
                {t("channels.fsEncryptHint2")}
              </p>
            </div>
              </>
            )}
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>{t("channels.doneBtn")}</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                {t("common.cancel")}
              </Button>
              <Button
                onClick={submit}
                disabled={submitting || !appId.trim() || !appSecret.trim()}
              >
                {submitting ? t("channels.validating") : t("channels.connectBtn")}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// QQ connect dialog. Stores the QQ Official Bot Platform AppID +
// ClientSecret pair and the markdown toggle. Unlike Telegram/Discord
// there is no verify step — QQ's getAppAccessToken call is the token
// endpoint itself and doesn't return a public bot identity, so
// validation happens later when the WS Identify frame succeeds or fails.
function ConnectQQDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const t = useT();
  const [appId, setAppId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [useMarkdown, setUseMarkdown] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{ appId: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setAppId("");
      setClientSecret("");
      setUseMarkdown(false);
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!appId.trim() || !clientSecret.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentQQ(
      agentId,
      appId.trim(),
      clientSecret.trim(),
      useMarkdown,
    );
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || t("channels.connectFailed"));
      return;
    }
    setConnected({ appId: res.appId || appId.trim() });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/qq.svg" alt="QQ" className="h-5 w-5 object-contain" />
            {t("channels.connectQQBot")}
          </DialogTitle>
          <DialogDescription>
            {t("channels.qqDesc1")}{" "}
            <a
              href="https://q.qq.com"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              q.qq.com
            </a>
            {t("channels.qqDesc2")} <strong>群聊 + C2C 私聊</strong>
            {t("channels.qqDesc3")} <code>AppID</code> + <code>ClientSecret</code>
            {t("channels.qqDesc4")}
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-success/30 bg-success/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-success" />
              <span className="text-sm font-medium">{t("channels.credentialsValid")}</span>
            </div>
            <p className="text-sm">
              {t("channels.botLiveAs")}{" "}
              <code className="font-mono text-xs">{connected.appId}</code>.
              {" "}{t("channels.qqLiveSuffix")}
            </p>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="qq-app-id">{t("channels.qqAppID")}</Label>
              <Input
                id="qq-app-id"
                value={appId}
                onChange={(e) => setAppId(e.target.value)}
                placeholder={t("channels.qqAppIDPlaceholder")}
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="qq-client-secret">{t("channels.qqClientSecret")}</Label>
              <Input
                id="qq-client-secret"
                value={clientSecret}
                onChange={(e) => setClientSecret(e.target.value)}
                placeholder={t("channels.qqClientSecretPlaceholder")}
                type="password"
                className="font-mono text-sm"
              />
            </div>
            <div className="flex items-start justify-between gap-3 rounded-lg border bg-muted/30 p-3">
              <div className="space-y-0.5">
                <Label htmlFor="qq-use-markdown" className="text-sm">
                  {t("channels.qqUseMarkdown")}
                </Label>
                <p className="text-xs text-muted-foreground">
                  {t("channels.qqUseMarkdownHint")}
                </p>
              </div>
              <Switch
                id="qq-use-markdown"
                checked={useMarkdown}
                onCheckedChange={setUseMarkdown}
              />
            </div>
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>{t("channels.doneBtn")}</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                {t("common.cancel")}
              </Button>
              <Button
                onClick={submit}
                disabled={submitting || !appId.trim() || !clientSecret.trim()}
              >
                {submitting ? t("channels.connecting") : t("channels.connectBtn")}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
