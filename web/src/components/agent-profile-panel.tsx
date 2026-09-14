"use client";

import * as React from "react";
import { Bot } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Skeleton } from "@/components/ui/skeleton";
import { SaveButton } from "@/components/save-button";
import { PageHeader, SettingsCard, CardHead } from "@/components/settings-ui";
import { apiFetch, getAgent, updateAgent, type AgentDetail } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useT } from "@/lib/i18n";

// AgentProfilePanel is the "Profile" tab inside the Settings dialog —
// the same fields the admin Edit Agent dialog at /agents/page.tsx
// exposes (avatar, name, description, public toggle), gated to the
// agent's owner. Viewers (super_admin browsing or public-link users)
// see read-only fields. The panel reads agentId from the URL via
// useAgentIdFromURL so the dialog component doesn't have to thread
// it through.

export default function AgentProfilePanel() {
  const agentId = useAgentIdFromURL();
  const t = useT();
  const [agent, setAgent] = React.useState<AgentDetail | null>(null);
  const [loading, setLoading] = React.useState(true);

  // Form state — independent from `agent` so users can revert with a
  // refresh and so the Save button can compare-then-write.
  const [name, setName] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [avatar, setAvatar] = React.useState<File | null>(null);
  const [avatarPreview, setAvatarPreview] = React.useState<string | null>(null);
  const [avatarBust, setAvatarBust] = React.useState<number>(0);
  const fileInputRef = React.useRef<HTMLInputElement>(null);

  const refresh = React.useCallback(() => {
    if (!agentId) return;
    setLoading(true);
    getAgent(agentId)
      .then((a) => {
        if (!a) {
          setAgent(null);
          return;
        }
        setAgent(a);
        setName(a.name || "");
        setDescription(a.description || "");
      })
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [agentId]);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  // Revoke blob URLs we own when the file changes or the panel
  // unmounts — without this the page leaks one URL per attachment
  // swap, mostly harmless but eslint flags it on long sessions.
  React.useEffect(() => {
    return () => {
      if (avatarPreview) URL.revokeObjectURL(avatarPreview);
    };
  }, [avatarPreview]);

  const isOwner = agent?.role === "owner";
  const dirty =
    !!agent &&
    (name.trim() !== (agent.name || "") ||
      description.trim() !== (agent.description || "") ||
      avatar !== null);

  const onPickAvatar = (e: React.ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0] ?? null;
    setAvatar(f);
    if (avatarPreview) URL.revokeObjectURL(avatarPreview);
    setAvatarPreview(f ? URL.createObjectURL(f) : null);
  };

  const uploadAvatar = async (file: File) => {
    const fd = new FormData();
    fd.append("file", file, "avatar.png");
    await apiFetch(`/api/agents/${agentId}/files`, { method: "POST", body: fd });
    setAvatarBust(Date.now());
  };

  // SaveButton owns the saving/saved/error visuals — throw to surface them.
  const onSave = async () => {
    if (!agentId || !agent || !isOwner) return;
    if (!name.trim()) throw new Error(t("profile.nameRequired"));
    const resp = await updateAgent(agentId, {
      name: name.trim(),
      description: description.trim(),
    });
    if (resp && (resp.ok === false || resp.error)) {
      throw new Error(resp.error || t("profile.updateFailed"));
    }
    if (avatar) {
      try {
        await uploadAvatar(avatar);
      } catch {
        // Non-fatal: text fields saved, only the avatar upload
        // failed. The next Save can retry the image.
      }
      setAvatar(null);
      if (avatarPreview) URL.revokeObjectURL(avatarPreview);
      setAvatarPreview(null);
    }
    refresh();
  };

  if (loading) {
    return (
      <div className="p-6 max-w-5xl mx-auto space-y-4">
        <Skeleton className="h-8 w-32" />
        <Skeleton className="h-20 w-full" />
        <Skeleton className="h-12 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>
    );
  }

  if (!agent) {
    return (
      <div className="p-6 max-w-5xl mx-auto">
        <p className="text-sm text-muted-foreground">{t("profile.notFound")}</p>
      </div>
    );
  }

  // Avatar src: the editable preview wins, then a fresh URL bust on
  // upload (so the cached image refreshes), then the canonical avatar
  // route.
  const avatarSrc =
    avatarPreview ||
    `/api/agents/${agent.id}/files/avatar.png${avatarBust ? `?v=${avatarBust}` : ""}`;

  return (
    <div className="p-6 max-w-5xl mx-auto space-y-6">
      <PageHeader
        title={t("profile.title")}
        desc={isOwner ? t("profile.ownerDesc") : t("profile.viewerDesc")}
        actions={
          isOwner ? (
            <SaveButton onSave={onSave} disabled={!dirty || !name.trim()} />
          ) : undefined
        }
      />

      <SettingsCard className="space-y-5">
        {/* Avatar + name on the same row, mirrors the admin Edit dialog. */}
        <div className="flex items-start gap-4">
          <button
            type="button"
            onClick={() => isOwner && fileInputRef.current?.click()}
            disabled={!isOwner}
            className="group relative flex size-20 shrink-0 items-center justify-center overflow-hidden rounded-xl border border-dashed bg-muted/40 transition hover:bg-muted disabled:cursor-not-allowed"
            aria-label={t("profile.uploadAvatar")}
          >
            <AgentAvatarImg src={avatarSrc} />
            <input
              ref={fileInputRef}
              type="file"
              accept="image/*"
              className="hidden"
              onChange={onPickAvatar}
              disabled={!isOwner}
            />
          </button>
          <div className="flex-1 space-y-2">
            <Label htmlFor="agent-profile-name">{t("profile.name")}</Label>
            <Input
              id="agent-profile-name"
              value={name}
              onChange={(e) => {
                setName(e.target.value);
              }}
              placeholder={t("profile.namePlaceholder")}
              disabled={!isOwner}
            />
            <p className="text-xs text-muted-foreground">
              {t("profile.idLabel")}{" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
                {agent.id}
              </code>
            </p>
          </div>
        </div>

        <div className="space-y-2">
          <Label htmlFor="agent-profile-desc">{t("profile.description")}</Label>
          <Textarea
            id="agent-profile-desc"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder={t("profile.descPlaceholder")}
            rows={3}
            disabled={!isOwner}
          />
        </div>
      </SettingsCard>

      <SettingsCard>
        <CardHead title={t("profile.visibility")} desc={t("profile.visibilityDesc")} />
      </SettingsCard>
    </div>
  );
}

// AgentAvatarImg renders the avatar with a Bot fallback so an agent
// without an uploaded avatar.png doesn't show a broken-image icon.
// Mirrors the team-switcher's AgentAvatar but takes a plain src so we
// can swap in the local blob URL during edit.
function AgentAvatarImg({ src }: { src: string }) {
  const [failed, setFailed] = React.useState(false);
  React.useEffect(() => {
    setFailed(false);
  }, [src]);
  if (failed) {
    return (
      <div className="flex h-full w-full items-center justify-center bg-primary/10 dark:bg-primary/15">
        <Bot className="h-9 w-9 text-primary" />
      </div>
    );
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={src}
      alt=""
      className="h-full w-full object-cover"
      onError={() => setFailed(true)}
    />
  );
}
