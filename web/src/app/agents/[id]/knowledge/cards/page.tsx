"use client";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";
import { KnowledgePage } from "@/components/knowledge-views";
import { CardsView } from "@/components/cards-view";

// /agents/<id>/knowledge/cards/ — 问答卡片库 + 复习流（艾宾浩斯间隔重复）。
export default function Page() {
  const t = useT();
  usePageHeader(<h1 className="text-sm font-semibold">{t("knowledge.cards")}</h1>, []);
  return (
    <KnowledgePage>{(notify) => <CardsView notify={notify} />}</KnowledgePage>
  );
}
