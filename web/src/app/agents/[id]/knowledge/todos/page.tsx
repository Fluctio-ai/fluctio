"use client";
import { KnowledgePage, TodoView } from "@/components/knowledge-views";

// /agents/<id>/knowledge/todos/ — 待办（看板 / 列表）。
export default function Page() {
  return (
    <KnowledgePage>{(notify) => <TodoView notify={notify} />}</KnowledgePage>
  );
}
