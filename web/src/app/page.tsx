"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { getStatus, getMe, login as loginApi } from "@/lib/api";
import { lastAgentPath } from "@/lib/last-agent";
import { logout } from "@/lib/auth";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { BootSkeleton } from "@/components/boot-skeleton";
import { useT } from "@/lib/i18n";
import { LogoWave } from "@/components/logo-wave";

export default function RootPage() {
  const tt = useT();
  const router = useRouter();
  const [showLogin, setShowLogin] = useState(false);
  const [loginField, setLoginField] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [loading, setLoading] = useState(true);
  const [statusError, setStatusError] = useState(false);
  const [retryTick, setRetryTick] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setStatusError(false);
    getStatus()
      .then(async (status) => {
        if (cancelled) return;
        // Never trust a stale localStorage token when the backend reports
        // the system is unconfigured — that token belongs to a previous
        // deployment and would otherwise short-circuit onboarding. But act
        // only on an EXPLICIT configured:false: getStatus() resolves 5xx /
        // malformed bodies too (configured undefined), and treating that
        // "unknown" as "unconfigured" used to logout() and bounce to
        // /onboard — an endless / ↔ /onboard round-trip whenever the
        // backend hiccuped.
        if (status.configured === false) {
          logout();
          router.replace("/onboard/");
          return;
        }
        if (status.configured !== true) {
          setStatusError(true);
          setLoading(false);
          return;
        }
        const me = await getMe().catch(() => null);
        if (cancelled) return;
        if (me?.ok && me.user) {
          router.replace(lastAgentPath() ?? "/overview/");
        } else {
          setShowLogin(true);
          setLoading(false);
        }
      })
      .catch(() => {
        if (cancelled) return;
        // Network-level failure: state unknown. Redirecting to /onboard
        // here caused the logout loop — show a retryable error instead.
        setStatusError(true);
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [router, retryTick]);

  const handleLogin = async (e?: React.FormEvent) => {
    e?.preventDefault();
    if (!loginField.trim() || !password) return;
    setError("");
    setSubmitting(true);
    try {
      const res = await loginApi(loginField.trim(), password);
      if (!res.ok) {
        setError(res.error || tt("login.invalidCredentials"));
        return;
      }
      router.replace(lastAgentPath() ?? "/overview/");
    } catch {
      setError(tt("login.cannotReach"));
    } finally {
      setSubmitting(false);
    }
  };

  if (loading && !showLogin) {
    return (
      <BootSkeleton />
    );
  }

  if (statusError) {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-background">
        <div className="w-full max-w-sm space-y-4 p-6 text-center">
          <div className="flex flex-col items-center gap-3">
            <LogoWave size={48} />
            <p className="text-sm text-destructive">{tt("login.cannotReach")}</p>
          </div>
          <Button
            className="w-full"
            onClick={() => {
              setLoading(true);
              setRetryTick((x) => x + 1);
            }}
          >
            Retry
          </Button>
        </div>
      </div>
    );
  }

  if (showLogin) {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-background">
        <div className="w-full max-w-sm space-y-6 p-6">
          <div className="flex flex-col items-center gap-3">
            <LogoWave size={48} />
            <h1 className="text-xl font-bold">Fluctio</h1>
            <p className="text-sm text-muted-foreground">{tt("login.continuePrompt")}</p>
          </div>

          <form onSubmit={handleLogin} className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="login-field">{tt("login.userLabel")}</Label>
              <Input
                id="login-field"
                value={loginField}
                onChange={(e) => setLoginField(e.target.value)}
                autoComplete="username"
                autoFocus
                placeholder="alice"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="login-password">{tt("login.passwordLabel")}</Label>
              <Input
                id="login-password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
              />
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button
              type="submit"
              disabled={!loginField.trim() || !password || submitting}
              className="w-full"
            >
              {submitting ? tt("login.submitting") : tt("login.submit")}
            </Button>
          </form>
        </div>
      </div>
    );
  }

  return null;
}
