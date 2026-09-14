"use client";

import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Upload, X } from "lucide-react";
import { getMe, updateMe, changeMyPassword } from "@/lib/api";
import { logout as doLogout } from "@/lib/auth";
import { useT } from "@/lib/i18n";
import { SaveButton } from "@/components/save-button";
import { PageHeader, SettingsCard, CardHead, Field, SettingsError } from "@/components/settings-ui";

const AVATAR_MAX_BYTES = 256 * 1024;

export default function AccountSettingsPage() {
  const t = useT();
  const [loading, setLoading] = useState(true);
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [avatarUrl, setAvatarUrl] = useState("");
  const [profileError, setProfileError] = useState("");

  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");

  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    getMe()
      .then((m) => {
        if (m?.user) {
          setUsername(m.user.username || "");
          setEmail(m.user.email || "");
          setDisplayName(m.user.displayName || "");
          setAvatarUrl(m.user.avatarUrl || "");
        }
      })
      .finally(() => setLoading(false));
  }, []);

  function pickAvatar() {
    fileRef.current?.click();
  }

  function onAvatarFile(e: React.ChangeEvent<HTMLInputElement>) {
    setProfileError("");
    const file = e.target.files?.[0];
    if (!file) return;
    if (!file.type.startsWith("image/")) {
      setProfileError(t("account.avatarMustBeImage"));
      return;
    }
    // Rough pre-check on raw bytes; the encoded data URL will be ~33%
    // larger, so reject anything that won't fit comfortably.
    if (file.size > Math.floor(AVATAR_MAX_BYTES * 0.7)) {
      setProfileError(t("account.avatarTooLarge"));
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      const result = String(reader.result || "");
      if (result.length > AVATAR_MAX_BYTES) {
        setProfileError(t("account.avatarEncodedTooLarge"));
        return;
      }
      setAvatarUrl(result);
    };
    reader.readAsDataURL(file);
    // Reset input so re-selecting the same file fires onchange again.
    e.target.value = "";
  }

  // SaveButton owns the saving/saved/error visuals; throw to surface the
  // error state (avatar pre-check errors stay inline via profileError).
  const saveProfile = async () => {
    const res = await updateMe({ displayName, avatarUrl });
    if (res?.error) throw new Error(res.error);
    // Tell the sidebar (and any other listener) to refetch /api/me so the
    // footer avatar/name picks up the new avatarUrl without a full reload.
    window.dispatchEvent(new Event("me-changed"));
  };

  async function savePassword() {
    if (!oldPassword || !newPassword) {
      throw new Error(t("account.bothFieldsRequired"));
    }
    if (newPassword !== confirmPassword) {
      throw new Error(t("account.passwordMismatch"));
    }
    const res = await changeMyPassword({ oldPassword, newPassword });
    if (res?.error) throw new Error(res.error);
    // Force re-login on the new password — also kicks any stale sessions
    // off this device. Brief delay so the user sees the success state.
    setTimeout(() => {
      doLogout();
      window.location.href = "/";
    }, 800);
  }

  if (loading) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-10 w-48" />
        <Skeleton className="h-64 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  const initials = (displayName || username || "?").slice(0, 2).toUpperCase();

  return (
    <div className="space-y-6">
      <PageHeader title={t("account.title")} desc={t("account.desc")} />

      {/* Profile */}
      <SettingsCard className="space-y-4">
        <CardHead title={t("account.profile")} />

        <div className="flex items-center gap-4">
          <div className="relative size-16 group">
            <div className="size-16 rounded-lg bg-muted overflow-hidden flex items-center justify-center text-lg font-bold text-muted-foreground">
              {avatarUrl ? (
                // eslint-disable-next-line @next/next/no-img-element
                <img src={avatarUrl} alt="avatar" className="size-full object-cover" />
              ) : (
                initials
              )}
            </div>
            {avatarUrl && (
              <button
                type="button"
                onClick={() => setAvatarUrl("")}
                aria-label={t("account.removeAvatar")}
                title={t("account.removeAvatar")}
                className="absolute -top-1 -right-1 hidden group-hover:flex items-center justify-center size-5 rounded-full bg-background border border-border text-muted-foreground hover:text-destructive hover:border-destructive transition shadow-sm"
              >
                <X className="size-3" />
              </button>
            )}
          </div>
          <Button variant="outline" onClick={pickAvatar}>
            <Upload className="size-4" />
            {t("account.uploadAvatar")}
          </Button>
          <input
            ref={fileRef}
            type="file"
            accept="image/*"
            onChange={onAvatarFile}
            className="hidden"
          />
        </div>

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Field label={t("account.username")}>
            <Input value={username} disabled />
          </Field>
          <Field label={t("account.email")}>
            <Input value={email} disabled />
          </Field>
          <Field label={t("account.displayName")} htmlFor="display-name" className="sm:col-span-2">
            <Input
              id="display-name"
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
              placeholder={t("account.displayNamePlaceholder")}
            />
          </Field>
        </div>

        {profileError && <SettingsError>{profileError}</SettingsError>}

        <div className="flex justify-end">
          <SaveButton onSave={saveProfile} label={t("account.saveProfile")} />
        </div>
      </SettingsCard>

      {/* Password */}
      <SettingsCard className="space-y-4">
        <CardHead title={t("account.changePassword")} desc={t("account.changePasswordDesc")} />

        <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
          <Field label={t("account.currentPassword")} htmlFor="old-pw">
            <Input
              id="old-pw"
              type="password"
              value={oldPassword}
              onChange={(e) => setOldPassword(e.target.value)}
              autoComplete="current-password"
            />
          </Field>
          <Field label={t("account.newPassword")} htmlFor="new-pw">
            <Input
              id="new-pw"
              type="password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              autoComplete="new-password"
            />
          </Field>
          <Field label={t("account.confirmPassword")} htmlFor="confirm-pw">
            <Input
              id="confirm-pw"
              type="password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              autoComplete="new-password"
            />
          </Field>
        </div>

        <div className="flex justify-end">
          <SaveButton onSave={savePassword} />
        </div>
      </SettingsCard>
    </div>
  );
}
