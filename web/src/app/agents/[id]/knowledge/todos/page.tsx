"use client";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";
import { KnowledgePage, TodoView } from "@/components/knowledge-views";

// /agents/<id>/knowledge/todos/ — 待办（看板 / 列表）。
export default function Page() {
  const t = useT();
  usePageHeader(<h1 className="text-sm font-semibold">{t("knowledge.todos")}</h1>, []);
  return (
    <KnowledgePage>{(notify) => <TodoView notify={notify} />}</KnowledgePage>
  );
}
