"use client";
import { KnowledgePage, ArticleView } from "@/components/knowledge-views";

// /agents/<id>/knowledge/ — 文章（chunked sources, two-pane）。原 KB 顶部
// tab bar 已拆成独立 sidebar 入口（见 nav-knowledge），本页只承载文章视图。
export default function Page() {
  return (
    <KnowledgePage>{(notify) => <ArticleView notify={notify} />}</KnowledgePage>
  );
}
