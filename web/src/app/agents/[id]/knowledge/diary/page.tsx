"use client";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";
import { KnowledgePage } from "@/components/knowledge-views";
import { DiaryView } from "@/components/diary-view";

// /agents/<id>/knowledge/diary/ — 每日日记（月历 + 当天消息）。
export default function Page() {
  const t = useT();
  usePageHeader(<h1 className="text-sm font-semibold">{t("knowledge.diary")}</h1>, []);
  return (
    <KnowledgePage>{(notify) => <DiaryView notify={notify} />}</KnowledgePage>
  );
}
