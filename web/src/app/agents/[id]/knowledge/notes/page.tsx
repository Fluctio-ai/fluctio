"use client";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";
import { KnowledgePage } from "@/components/knowledge-views";
import { NotesView } from "@/components/notes-view";

// /agents/<id>/knowledge/notes/ — 笔记（markdown + 白板 + 附件）。
export default function Page() {
  const t = useT();
  usePageHeader(<h1 className="text-sm font-semibold">{t("knowledge.notes")}</h1>, []);
  return (
    <KnowledgePage>{(notify) => <NotesView notify={notify} />}</KnowledgePage>
  );
}
