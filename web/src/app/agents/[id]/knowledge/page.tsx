"use client";
import { useT } from "@/lib/i18n";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
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
  getAgentConfig,
  updateAgent,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

export default function AgentKnowledgePage() {
  const t = useT();
  const agentId = useAgentIdFromURL();

  const [sources, setSources] = useState<KBSource[]>([]);
  const [stats, setStats] = useState<KBStats | null>(null);
  const [loading, setLoading] = useState(true);

  const [kbEnabled, setKbEnabled] = useState(false);
  const [autoMode, setAutoMode] = useState("always");
  const [keywords, setKeywords] = useState("");
  const [maxResults, setMaxResults] = useState(5);
  const [searchMode, setSearchMode] = useState("augment");
  const [emptyAction, setEmptyAction] = useState("llm");
  const [showIndicator, setShowIndicator] = useState(true);
  const [indicatorFound, setIndicatorFound] = useState("");
  const [indicatorNotFound, setIndicatorNotFound] = useState("");
  const [configLoaded, setConfigLoaded] = useState(false);
  const [saving, setSaving] = useState(false);

  const [textDialogOpen, setTextDialogOpen] = useState(false);
  const [urlDialogOpen, setUrlDialogOpen] = useState(false);
  const [previewOpen, setPreviewOpen] = useState(false);
  const [previewSource, setPreviewSource] = useState<KBSource | null>(null);
  const [previewEntries, setPreviewEntries] = useState<KBEntry[]>([]);
  const [previewLoading, setPreviewLoading] = useState(false);
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

  useEffect(() => {
    if (!agentId) return;
    getAgentConfig(agentId)
      .then((cfg) => {
        const kb = cfg.kb;
        if (kb) {
          setKbEnabled(kb.enabled ?? false);
          setAutoMode(kb.autoMode ?? "always");
          setKeywords((kb.keywords ?? []).join(", "));
          setMaxResults(kb.maxResults || 5);
          setSearchMode(kb.searchMode ?? "augment");
          setEmptyAction(kb.emptyAction ?? "llm");
          setShowIndicator(kb.showIndicator ?? true);
          setIndicatorFound(kb.indicatorFound ?? "");
          setIndicatorNotFound(kb.indicatorNotFound ?? "");
        }
        setConfigLoaded(true);
      })
      .catch(() => {});
  }, [agentId]);

  const handleSaveConfig = useCallback(async () => {
    if (!agentId) return;
    setSaving(true);
    try {
      await updateAgent(agentId, {
        kb: {
          enabled: kbEnabled,
          autoMode,
          keywords: keywords.split(/[,\n]/).map((s) => s.trim()).filter(Boolean),
          maxResults,
          searchMode,
          emptyAction,
          showIndicator,
          indicatorFound: indicatorFound || undefined,
          indicatorNotFound: indicatorNotFound || undefined,
        },
      } as any);
    } catch {}
    setSaving(false);
  }, [agentId, kbEnabled, autoMode, keywords, maxResults, searchMode, emptyAction, showIndicator, indicatorFound, indicatorNotFound]);

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
      if ("error" in res) alert(res.error); else loadData();
    } catch {}
  }, [agentId, loadData]);

  const handlePreviewSource = useCallback(async (src: KBSource) => {
    if (!agentId) return;
    setPreviewSource(src);
    setPreviewOpen(true);
    setPreviewLoading(true);
    try {
      const entries = await listKBEntries(agentId, src.id);
      setPreviewEntries(entries);
    } catch { setPreviewEntries([]); }
    setPreviewLoading(false);
  }, [agentId]);

  return (
    <div className="p-6 max-w-3xl mx-auto space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">{t("knowledge.title")}</h2>
          <p className="text-sm text-muted-foreground mt-1">
            {t("knowledge.subtitle")}
          </p>
        </div>
        <Button size="sm" onClick={handleSaveConfig} disabled={saving || !configLoaded}>
          {saving ? t("common.saving") : t("common.save")}
        </Button>
      </div>

      {/* Auto-Query Card */}
      <div className="space-y-3 rounded-lg border border-border bg-card p-5">
        <div className="flex items-center justify-between">
          <div className="space-y-1">
            <Label className="text-sm font-medium">{t("knowledge.autoQuery")}</Label>
            <p className="text-xs text-muted-foreground">
              {t("knowledge.autoQueryDesc")}
            </p>
          </div>
          <Switch checked={kbEnabled} onCheckedChange={setKbEnabled} disabled={!configLoaded} />
        </div>

        {kbEnabled && (
          <div className="space-y-3 pt-1">
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label className="text-xs">{t("knowledge.triggerMode")}</Label>
                <Select value={autoMode} onValueChange={(v) => v && setAutoMode(v)}>
                  <SelectTrigger className="h-8 text-xs">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="always">{t("knowledge.modeAlways")}</SelectItem>
                    <SelectItem value="keyword">{t("knowledge.modeKeyword")}</SelectItem>
                    <SelectItem value="disabled">{t("knowledge.modeDisabled")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label className="text-xs">{t("knowledge.maxResults")}</Label>
                <Input
                  type="number" min={1} max={20}
                  value={maxResults}
                  onChange={(e) => setMaxResults(Number(e.target.value))}
                  className="h-8 text-xs"
                />
              </div>
            </div>

            {autoMode === "keyword" && (
              <div className="space-y-1.5">
                <Label className="text-xs">{t("knowledge.keywords")}</Label>
                <Input
                  value={keywords}
                  onChange={(e) => setKeywords(e.target.value)}
                  placeholder={t("knowledge.keywordsPlaceholder")}
                  className="h-8 text-xs"
                />
              </div>
            )}

            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label className="text-xs">{t("knowledge.searchMode")}</Label>
                <Select value={searchMode} onValueChange={(v) => v && setSearchMode(v)}>
                  <SelectTrigger className="h-8 text-xs">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="augment">{t("knowledge.searchAugment")}</SelectItem>
                    <SelectItem value="strict">{t("knowledge.searchStrict")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label className="text-xs">{t("knowledge.noResultAction")}</Label>
                <Select value={emptyAction} onValueChange={(v) => v && setEmptyAction(v)}>
                  <SelectTrigger className="h-8 text-xs">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="llm">{t("knowledge.actionLLM")}</SelectItem>
                    <SelectItem value="stop">{t("knowledge.actionStop")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="flex items-center justify-between pt-1">
              <Label className="text-xs">{t("knowledge.showIndicator")}</Label>
              <Switch checked={showIndicator} onCheckedChange={setShowIndicator} />
            </div>

            {showIndicator && (
              <div className="space-y-2 pt-1">
                <div className="space-y-1.5">
                  <Label className="text-xs">{t("knowledge.foundIndicator")}</Label>
                  <Input
                    value={indicatorFound}
                    onChange={(e) => setIndicatorFound(e.target.value)}
                    placeholder='[KB] {kbCount} 条知识库, {wikiCount} 条百科'
                    className="h-8 text-xs"
                  />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-xs">{t("knowledge.notFoundIndicator")}</Label>
                  <Input
                    value={indicatorNotFound}
                    onChange={(e) => setIndicatorNotFound(e.target.value)}
                    placeholder='[KB] 知识库中未找到相关信息'
                    className="h-8 text-xs"
                  />
                </div>
                <p className="text-[10px] text-muted-foreground">
                  {"{count}"} = total, {"{kbCount}"} = KB count, {"{wikiCount}"} = Wiki count, {"{query}"} = query
                </p>
              </div>
            )}
          </div>
        )}
      </div>

      {/* Data Sources Card */}
      <div className="rounded-lg border border-border bg-card p-5 space-y-3">
        <div className="flex items-center justify-between">
          <div className="space-y-1">
            <Label className="text-sm font-medium">{t("knowledge.dataSources")}</Label>
            {stats && (
              <p className="text-xs text-muted-foreground">
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

        {loading ? (
          <p className="text-xs text-muted-foreground">{t("common.loading")}</p>
        ) : sources.length === 0 ? (
          <p className="text-xs text-muted-foreground py-1">
            {t("knowledge.noSources")}
          </p>
        ) : (
          <div className="space-y-1">
            {sources.map((src) => (
              <div
                key={src.id}
                className="flex items-center gap-2 rounded-md border px-3 py-1.5 cursor-pointer hover:bg-accent/50 transition-colors"
                onClick={() => handlePreviewSource(src)}
              >
                <div className="flex-1 min-w-0">
                  <p className="text-sm truncate">{src.title}</p>
                  <p className="text-xs text-muted-foreground">
                    {src.entry_count} entries · {(src.total_chars / 1024).toFixed(1)} KB
                    {src.source_type && (
                      <Badge variant="outline" className="text-[10px] px-1 py-0 ml-1.5">
                        {src.source_type}
                      </Badge>
                    )}
                  </p>
                </div>
                <Button variant="ghost" size="icon" className="h-6 w-6 shrink-0" onClick={() => handleDeleteSource(src.id)}>
                  <TrashIcon className="h-3 w-3" />
                </Button>
              </div>
            ))}
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

      {/* Preview Dialog */}
      <Dialog open={previewOpen} onOpenChange={setPreviewOpen}>
        <DialogContent className="max-w-2xl max-h-[80vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{previewSource?.title || t("knowledge.sourcePreview")}</DialogTitle>
            <DialogDescription>
              {previewSource?.entry_count} {t("knowledge.entries")} · {((previewSource?.total_chars ?? 0) / 1024).toFixed(1)} KB
              {previewSource?.source_type && ` · ${previewSource.source_type}`}
            </DialogDescription>
          </DialogHeader>
          {previewLoading ? (
            <p className="text-sm text-muted-foreground py-4">{t("common.loading")}</p>
          ) : previewEntries.length === 0 ? (
            <p className="text-sm text-muted-foreground py-4">{t("knowledge.noEntries")}</p>
          ) : (
            <div className="space-y-3">
              {previewEntries.map((entry) => (
                <div key={entry.id} className="rounded-md border bg-muted/30 p-3">
                  <p className="text-[10px] text-muted-foreground mb-1">{t("knowledge.chunk")} {entry.chunk_index}</p>
                  <pre className="text-sm whitespace-pre-wrap font-sans">{entry.content}</pre>
                </div>
              ))}
            </div>
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}
