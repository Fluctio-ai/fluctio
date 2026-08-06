"use client";
import { KnowledgePage, FlashView } from "@/components/knowledge-views";

// /agents/<id>/knowledge/flashes/ — 灵感闪记（masonry + 记一笔）。
export default function Page() {
  return (
    <KnowledgePage>{(notify) => <FlashView notify={notify} />}</KnowledgePage>
  );
}
