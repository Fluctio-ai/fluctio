"use client";
import { useT } from "@/lib/i18n";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog";
import {
  FileTextIcon,
  GlobeIcon,
  TrashIcon,
} from "lucide-react";
import {
  type KBSource,
  type KBStats,
  type KBEntry,
  listKBSources,
  listKBEntries,
  kbIngestText,
  kbIngestURL,
  deleteKBSource,
  getKBStats,
  generateWiki,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { cn } from "@/lib/utils";

// AgentKnowledgePage is the two-pane data-source browser at
// /agents/<id>/knowledge/ (sidebar "Knowledge" → "Data Sources"). Left
// pane lists sources; clicking one loads its chunks into the right pane
// (replaces the old preview dialog). Ingest (text/URL) lives in dialogs.
// KB *settings* are in the Settings dialog's Knowledge tab.
export default function AgentKnowledgePage() {
  const t = useT();
  const agentId = useAgentIdFromURL();

  const [sources, setSources] = useState<KBSource[]>([]);
  const [stats, setStats] = useState<KBStats | null>(null);
  const [loading, setLoading] = useState(true);

  // Right pane: chunks of the selected source.
  const [selectedSource, setSelectedSource] = useState<KBSource | null>(null);
  const [entries, setEntries] = useState<KBEntry[]>([]);
  const [entriesLoading, setEntriesLoading] = useState(false);

  const [textDialogOpen, setTextDialogOpen] = useState(false);
  const [urlDialogOpen, setUrlDialogOpen] = useState(false);
  const [textTitle, setTextTitle] = useState("");
  const [textContent, setTextContent] = useState("");
  const [urlValue, setUrlValue] = useState("");
  const [urlTitle, setUrlTitle] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const loadData = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    try {
      const [srcs, st] = await Promise.all([
        listKBSources(agentId),
        getKBStats(agentId),
      ]);
      setSources(srcs);
      setStats(st);
    } catch {}
    setLoading(false);
  }, [agentId]);

  useEffect(() => { loadData(); }, [loadData]);

  // Re-selecting the active "Data Sources" sidebar item clears the right
  // pane (the URL is unchanged, so navigateOnce would otherwise no-op).
  useEffect(() => {
    const onReselect = (e: Event) => {
      const url = (e as CustomEvent<{ url?: string }>).detail?.url ?? "";
      if (url.includes("/knowledge/")) {
        setSelectedSource(null);
        setEntries([]);
      }
    };
    window.addEventListener("fluctio:nav-reselect", onReselect);
    return () => window.removeEventListener("fluctio:nav-reselect", onReselect);
  }, []);

  const handleSelectSource = useCallback(async (src: KBSource) => {
    if (!agentId) return;
    setSelectedSource(src);
    setEntries([]);
    setEntriesLoading(true);
    try {
      const es = await listKBEntries(agentId, src.id);
      setEntries(es);
    } catch { setEntries([]); }
    setEntriesLoading(false);
  }, [agentId]);

  const handleIngestText = useCallback(async () => {
    if (!agentId || !textContent.trim()) return;
    setSubmitting(true);
    try {
      const res = await kbIngestText(agentId, textTitle || t("knowledge.untitled"), textContent);
      if ("error" in res) { alert(res.error); } else {
        setTextDialogOpen(false); setTextTitle(""); setTextContent("");
        await loadData();
        if ("source_id" in res) generateWiki(agentId, [res.source_id!]).catch(() => {});
      }
    } catch { alert(t("knowledge.failedAddText")); }
    setSubmitting(false);
  }, [agentId, textTitle, textContent, loadData, t]);

  const handleIngestURL = useCallback(async () => {
    if (!agentId || !urlValue.trim()) return;
    setSubmitting(true);
    try {
      const res = await kbIngestURL(agentId, urlValue, urlTitle || undefined);
      if ("error" in res) { alert(res.error); } else {
        setUrlDialogOpen(false); setUrlValue(""); setUrlTitle("");
        await loadData();
        if ("source_id" in res) generateWiki(agentId, [res.source_id!]).catch(() => {});
      }
    } catch { alert(t("knowledge.failedFetchURL")); }
    setSubmitting(false);
  }, [agentId, urlValue, urlTitle, loadData, t]);

  const handleDeleteSource = useCallback(async (sourceId: string) => {
    if (!agentId) return;
    try {
      const res = await deleteKBSource(agentId, sourceId);
      if ("error" in res) alert(res.error); else {
        if (selectedSource?.id === sourceId) {
          setSelectedSource(null);
          setEntries([]);
        }
        loadData();
      }
    } catch {}
  }, [agentId, loadData, selectedSource]);

  return (
    <div className="flex h-[calc(100vh-3.5rem)]">
      {/* Left: source list */}
      <div className="w-80 shrink-0 border-r bg-muted/30 flex flex-col">
        <div className="p-3 border-b space-y-2">
          <div>
            <h3 className="text-sm font-semibold">{t("knowledge.dataSources")}</h3>
            {stats && (
              <p className="text-xs text-muted-foreground mt-0.5">
                {stats.source_count} {t("knowledge.sources")} · {stats.entry_count} {t("knowledge.entries")} · {(stats.total_chars / 1024).toFixed(1)} KB
              </p>
            )}
          </div>
          <div className="flex gap-1.5">
            <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => setTextDialogOpen(true)}>
              <FileTextIcon className="h-3 w-3 mr-1" /> {t("knowledge.text")}
            </Button>
            <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => setUrlDialogOpen(true)}>
              <GlobeIcon className="h-3 w-3 mr-1" /> {t("knowledge.url")}
            </Button>
          </div>
        </div>
        <ScrollArea className="flex-1">
          <div className="p-2">
            {loading ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">{t("common.loading")}</p>
            ) : sources.length === 0 ? (
              <p className="text-xs text-muted-foreground px-2 py-1.5">{t("knowledge.noSources")}</p>
            ) : (
              sources.map((src) => (
                <div
                  key={src.id}
                  role="button"
                  tabIndex={0}
                  className={cn(
                    "group w-full text-left px-3 py-1.5 text-sm rounded-md hover:bg-accent flex items-center gap-2 cursor-pointer",
                    selectedSource?.id === src.id && "bg-accent",
                  )}
                  onClick={() => handleSelectSource(src)}
                >
                  <div className="flex-1 min-w-0">
                    <p className="truncate">{src.title}</p>
                    <p className="text-xs text-muted-foreground">
                      {src.entry_count} entries · {(src.total_chars / 1024).toFixed(1)} KB
                      {src.source_type && (
                        <Badge variant="outline" className="text-[10px] px-1 py-0 ml-1.5">
                          {src.source_type}
                        </Badge>
                      )}
                    </p>
                  </div>
                  <button
                    type="button"
                    className="opacity-0 group-hover:opacity-100 text-muted-foreground hover:text-destructive shrink-0"
                    onClick={(e) => {
                      e.stopPropagation();
                      handleDeleteSource(src.id);
                    }}
                    aria-label={t("common.delete")}
                  >
                    <TrashIcon className="h-3.5 w-3.5" />
                  </button>
                </div>
              ))
            )}
          </div>
        </ScrollArea>
      </div>

      {/* Right: chunks of the selected source */}
      <div className="flex-1 flex flex-col min-w-0">
        {selectedSource ? (
          <>
            <div className="p-4 border-b">
              <div className="flex items-center gap-2">
                <h1 className="text-xl font-bold truncate">{selectedSource.title}</h1>
                {selectedSource.source_type && (
                  <Badge variant="outline" className="text-[10px]">{selectedSource.source_type}</Badge>
                )}
              </div>
              <p className="text-xs text-muted-foreground mt-1">
                {selectedSource.entry_count} {t("knowledge.entries")} · {((selectedSource.total_chars ?? 0) / 1024).toFixed(1)} KB
              </p>
            </div>
            <ScrollArea className="flex-1">
              <div className="p-4 space-y-3 max-w-4xl">
                {entriesLoading ? (
                  <p className="text-sm text-muted-foreground">{t("common.loading")}</p>
                ) : entries.length === 0 ? (
                  <p className="text-sm text-muted-foreground">{t("knowledge.noEntries")}</p>
                ) : (
                  entries.map((entry) => (
                    <div key={entry.id} className="rounded-md border bg-muted/30 p-3">
                      <p className="text-[10px] text-muted-foreground mb-1">{t("knowledge.chunk")} {entry.chunk_index}</p>
                      <pre className="text-sm whitespace-pre-wrap font-sans">{entry.content}</pre>
                    </div>
                  ))
                )}
              </div>
            </ScrollArea>
          </>
        ) : (
          <div className="flex-1 flex items-center justify-center text-sm text-muted-foreground p-4 text-center">
            {t("knowledge.selectSourcePrompt")}
          </div>
        )}
      </div>

      {/* Text Ingest Dialog */}
      <Dialog open={textDialogOpen} onOpenChange={setTextDialogOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.addText")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("knowledge.titleLabel")}</Label>
              <Input value={textTitle} onChange={(e) => setTextTitle(e.target.value)} placeholder={t("knowledge.sourceTitlePlaceholder")} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("knowledge.contentLabel")}</Label>
              <textarea
                className="flex min-h-[200px] w-full rounded-md border bg-transparent px-3 py-2 text-sm"
                value={textContent} onChange={(e) => setTextContent(e.target.value)}
                placeholder={t("knowledge.contentPlaceholder")}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setTextDialogOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleIngestText} disabled={submitting || !textContent.trim()}>
              {submitting ? t("knowledge.adding") : t("knowledge.add")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* URL Ingest Dialog */}
      <Dialog open={urlDialogOpen} onOpenChange={setUrlDialogOpen}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("knowledge.addFromURL")}</DialogTitle></DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t("knowledge.urlLabel")}</Label>
              <Input value={urlValue} onChange={(e) => setUrlValue(e.target.value)} placeholder={t("knowledge.urlPlaceholder")} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("knowledge.titleOptional")}</Label>
              <Input value={urlTitle} onChange={(e) => setUrlTitle(e.target.value)} placeholder={t("knowledge.customTitlePlaceholder")} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setUrlDialogOpen(false)}>{t("common.cancel")}</Button>
            <Button onClick={handleIngestURL} disabled={submitting || !urlValue.trim()}>
              {submitting ? t("knowledge.fetching") : t("knowledge.add")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
