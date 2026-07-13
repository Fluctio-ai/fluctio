"use client";

import { useEffect, useState } from "react";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Radio, MessageCircle, Hash, Send } from "lucide-react";
import { getChannels, type ChannelInfo } from "@/lib/api";
import { useT } from "@/lib/i18n";

const channelIcons: Record<string, React.ElementType> = {
  telegram: Send,
  discord: Hash,
  slack: MessageCircle,
};

// Channel brand colors as arbitrary values so they survive palette-token
// sweeps — these are functional recognition cues (users identify the platform
// at a glance), an intentional exception to the Geist single-accent rule.
const channelColors: Record<string, string> = {
  telegram: "from-[#229ED9] to-[#2AABEE]",
  discord: "from-[#5865F2] to-[#4752C4]",
  slack: "from-[#36C5F0] to-[#2EB67D]",
};

export default function ChannelsPage() {
  const tt = useT();
  const [channels, setChannels] = useState<ChannelInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [editChannel, setEditChannel] = useState<ChannelInfo | null>(null);

  const fetchChannels = () => {
    setLoading(true);
    getChannels()
      .then(setChannels)
      .catch(() => setChannels([]))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchChannels();
  }, []);

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">{tt("channels.channelsTitle")}</h2>
        <p className="text-sm text-muted-foreground mt-1">
          {tt("channels.manageSubtitle")}
        </p>
      </div>

      {loading ? (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-48" />
          ))}
        </div>
      ) : channels.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-muted mb-4">
              <Radio className="h-7 w-7 text-muted-foreground" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">{tt("channels.empty")}</p>
            <p className="text-xs text-muted-foreground/60">
              {tt("channels.emptyHint")}
            </p>
          </div>
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {channels.map((channel, i) => {
            const Icon = channelIcons[channel.type] || Radio;
            const gradient = channelColors[channel.type] || "from-muted-foreground/80 to-muted-foreground";
            const isConnected = channel.enabled !== false && channel.status !== "disconnected";

            return (
              <div
                key={i}
                className="group rounded-lg border border-border bg-card p-5 transition-colors hover:bg-muted/50 cursor-pointer"
                onClick={() => setEditChannel(channel)}
              >
                <div className="flex items-start justify-between mb-4">
                  <div className={`flex h-12 w-12 items-center justify-center rounded-xl bg-gradient-to-br ${gradient}`}>
                    <Icon className="h-6 w-6 text-white" />
                  </div>
                  <Badge
                    variant="outline"
                    className={
                      isConnected
                        ? "bg-success/10 text-success border-success/20"
                        : "bg-muted text-muted-foreground border-border"
                    }
                  >
                    <span
                      className={`mr-1.5 inline-block h-1.5 w-1.5 rounded-full ${
                        isConnected ? "bg-success" : "bg-muted-foreground"
                      }`}
                    />
                    {isConnected ? tt("channels.connected") : tt("channels.disconnected")}
                  </Badge>
                </div>
                <p className="text-base font-medium capitalize mb-1">
                  {channel.type}
                </p>
                <p className="text-sm text-muted-foreground">
                  {channel.botUsername
                    ? `@${channel.botUsername}`
                    : tt("channels.clickToConfigure")}
                </p>
              </div>
            );
          })}
        </div>
      )}

      {/* Channel Config Dialog */}
      <Dialog open={!!editChannel} onOpenChange={() => setEditChannel(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle className="capitalize">
              {tt("channels.configTitle", { type: editChannel?.type ?? "" })}
            </DialogTitle>
            <DialogDescription>
              {tt("channels.updateSettings")}
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label>{tt("channels.botToken")}</Label>
              <Input
                type="password"
                defaultValue="••••••••••••"
                className="font-mono"
              />
            </div>
            {editChannel?.botUsername && (
              <div className="space-y-2">
                <Label>{tt("channels.botUsername")}</Label>
                <Input
                  value={editChannel.botUsername}
                  disabled
                  className="opacity-60"
                />
              </div>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditChannel(null)}>
              {tt("common.cancel")}
            </Button>
            <Button>{tt("common.save")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
