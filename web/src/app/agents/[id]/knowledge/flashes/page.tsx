"use client";
import { useT } from "@/lib/i18n";
import { usePageHeader } from "@/components/sidebar";
import { KnowledgePage, FlashView } from "@/components/knowledge-views";

// /agents/<id>/knowledge/flashes/ — 灵感闪记（masonry + 记一笔）。
export default function Page() {
  const t = useT();
  usePageHeader(<h1 className="text-sm font-semibold">{t("knowledge.flashes")}</h1>, []);
  return (
    <KnowledgePage>{(notify) => <FlashView notify={notify} />}</KnowledgePage>
  );
}
