"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Button } from "@/components/ui/button";
import { Plug } from "lucide-react";
import {
  apiFetch,
  getAgent,
  updateAgent,
  type HookPlugin,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";
import { useT } from "@/lib/i18n";
import { PageHeader } from "@/components/settings-ui";

// Backend agentScopePlugins filters to booleans; mirror that on the client
// instead of trusting a config blob that may hold arbitrary JSON.
function boolMap(v: unknown): Record<string, boolean> {
  const out: Record<string, boolean> = {};
  if (v && typeof v === "object") {
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
      if (typeof val === "boolean") out[k] = val;
    }
  }
  return out;
}

// Per-agent plugin enable tab. Mirrors the Skills page layout (cards
// grid with header). Off by default — plugins listed here come from
// the system install; flipping a toggle attaches the plugin's hooks
// to THIS agent only. See registerHookPluginsForAgent in
// internal/gateway/userspace.go for the opt-in semantics.
export default function AgentPluginsPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const t = useT();
  const [hookPlugins, setHookPlugins] = useState<HookPlugin[]>([]);
  const [pluginEnabled, setPluginEnabled] = useState<Record<string, boolean>>({});
  const [pluginSaving, setPluginSaving] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  // Out-of-order guard: only the fetch for the CURRENT agent may write
  // state — a slow response for the previously viewed agent used to
  // clobber the fresh view (cross-agent data bleed).
  const fetchSeqRef = useRef(0);
  // Mirrors agentId for in-flight toggle completions (see handleToggle).
  const agentIdRef = useRef(agentId);
  agentIdRef.current = agentId;

  const fetchAll = useCallback(async () => {
    if (!agentId) {
      // Don't strand the page on the skeleton when the id is missing.
      setLoading(false);
      return;
    }
    const seq = ++fetchSeqRef.current;
    setLoading(true);
    try {
      // listHookPlugins() collapses every failure to [] — indistinguishable
      // from "no plugins installed". Read the endpoint directly so HTTP
      // errors (403/5xx) surface as an explicit failed state with a retry,
      // not a fake empty install.
      const [agentRec, hooksRes] = await Promise.all([
        getAgent(agentId).catch(() => null),
        apiFetch("/api/plugins/hook")
          .then(async (res) => {
            const body = await res.json().catch(() => null);
            if (!res.ok) {
              return {
                ok: false as const,
                error: (body as { error?: string } | null)?.error || `HTTP ${res.status}`,
              };
            }
            return {
              ok: true as const,
              hooks: (Array.isArray(body) ? body : (body?.plugins ?? [])) as HookPlugin[],
            };
          })
          .catch((e) => ({ ok: false as const, error: e instanceof Error ? e.message : String(e) })),
      ]);
      if (seq !== fetchSeqRef.current) return; // superseded by a newer fetch
      setPluginEnabled(boolMap(agentRec?.plugins));
      setHookPlugins(hooksRes.ok ? hooksRes.hooks : []);
      setLoadError(hooksRes.ok ? null : hooksRes.error);
    } catch (e) {
      if (seq !== fetchSeqRef.current) return;
      setLoadError(e instanceof Error ? e.message : String(e));
    } finally {
      if (seq === fetchSeqRef.current) setLoading(false);
    }
  }, [agentId]);

  useEffect(() => {
    fetchAll();
  }, [fetchAll]);

  // Per-plugin toggle. Patch-semantic so flipping one doesn't clobber
  // overrides for sibling plugins. Optimistic update with rollback — the
  // rollback must also fire when the server answers 4xx/5xx: apiFetch
  // resolves those without throwing and updateAgent returns the parsed
  // body unchecked, so the old catch-only path never ran for real HTTP
  // failures and the switch stuck in the refused state.
  const handleToggle = async (pluginID: string, next: boolean) => {
    const prev = pluginEnabled[pluginID] === true;
    const callAgentId = agentId;
    setPluginEnabled((m) => ({ ...m, [pluginID]: next }));
    setPluginSaving((m) => ({ ...m, [pluginID]: true }));
    try {
      const res = await updateAgent(callAgentId, { plugins: { [pluginID]: next } });
      if (res && (res.ok === false || res.error)) {
        if (agentIdRef.current === callAgentId) {
          setPluginEnabled((m) => ({ ...m, [pluginID]: prev }));
        }
        window.alert(`Failed to update plugins: ${res.error || "unknown error"}`);
        return;
      }
      // Adopt the server's authoritative merged map so multi-tab edits and
      // server-side normalization don't leave this page drifting.
      const merged = (res as { agent?: { plugins?: unknown } } | null)?.agent?.plugins;
      if (agentIdRef.current === callAgentId && merged && typeof merged === "object") {
        setPluginEnabled(boolMap(merged));
      }
    } catch (e) {
      if (agentIdRef.current === callAgentId) {
        setPluginEnabled((m) => ({ ...m, [pluginID]: prev }));
      }
      window.alert(`Failed to update plugins: ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setPluginSaving((m) => {
        const copy = { ...m };
        delete copy[pluginID];
        return copy;
      });
    }
  };

  if (loading) {
    return (
      <div className="p-6 space-y-6 max-w-5xl mx-auto">
        <Skeleton className="h-10 w-48" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <PageHeader
        title={t("plugins.title")}
        desc={t("plugins.subtitle", { name: agentName, code: "chat.send" })}
      />

      {loadError ? (
        <div className="rounded-lg border border-destructive/30 bg-destructive/10 p-6 flex flex-col items-start gap-3">
          <p className="text-sm text-destructive">
            Failed to load plugins: {loadError}
          </p>
          <Button variant="outline" size="sm" onClick={() => fetchAll()}>
            Retry
          </Button>
        </div>
      ) : hookPlugins.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border bg-card/30 p-12">
          <div className="flex flex-col items-center justify-center">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <Plug className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">
              {t("plugins.noPlugins")}
            </p>
            <p className="text-xs text-muted-foreground/60 max-w-sm text-center">
              {t("plugins.noPluginsHint", { code1: "~/.fluctio/plugins/", code2: 'type: "hook"', code3: "plugin.json" })}
            </p>
          </div>
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {hookPlugins.map((p) => {
            const enabled = pluginEnabled[p.id] === true;
            const saving = pluginSaving[p.id] === true;
            return (
              <div
                key={p.id}
                className="group rounded-lg border border-border bg-card p-5 transition-colors hover:bg-muted/50"
              >
                <div className="flex items-start justify-between mb-3 gap-3">
                  <div className="flex items-center gap-2.5 min-w-0">
                    <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-primary/10 shrink-0">
                      <Plug className="h-4 w-4 text-primary" />
                    </div>
                    <div className="min-w-0">
                      <p className="text-sm font-medium truncate">
                        {p.name || p.id}
                      </p>
                      {p.version && (
                        <Badge variant="outline" className="mt-1 text-2xs">
                          v{p.version}
                        </Badge>
                      )}
                    </div>
                  </div>
                  <Switch
                    checked={enabled}
                    onCheckedChange={(v) => handleToggle(p.id, v)}
                    disabled={saving}
                    aria-label={t("plugins.enable", { name: p.id })}
                  />
                </div>
                {p.description && (
                  <p className="text-xs text-muted-foreground line-clamp-3">
                    {p.description}
                  </p>
                )}
                <code className="text-2xs text-muted-foreground/70 mt-3 block truncate">
                  {p.id}
                </code>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
