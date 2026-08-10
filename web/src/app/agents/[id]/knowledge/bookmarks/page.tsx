"use client";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";
import { KnowledgePage, BookmarkView } from "@/components/knowledge-views";

// /agents/<id>/knowledge/bookmarks/ — 收藏的网页书签（链接 + 标题/备注 + 抓取正文）。
export default function Page() {
  const t = useT();
  usePageHeader(<h1 className="text-sm font-semibold">{t("knowledge.bookmarks")}</h1>, []);
  return (
    <KnowledgePage>{(notify) => <BookmarkView notify={notify} />}</KnowledgePage>
  );
}
