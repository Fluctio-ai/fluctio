"use client";
import { KnowledgePage } from "@/components/knowledge-views";
import { DiaryView } from "@/components/diary-view";

// /agents/<id>/knowledge/diary/ — 每日日记（月历 + 当天消息）。
export default function Page() {
  return (
    <KnowledgePage>{(notify) => <DiaryView notify={notify} />}</KnowledgePage>
  );
}
