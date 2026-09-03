// Package cardsgen generates spaced-repetition Q&A cards from one day's
// knowledge input: the agent's daily diary plus the wiki pages not yet
// distilled into cards. One LLM pass (no-thinking + JSON mode, per the background-call
// convention) distills the material into question/answer pairs, each
// pinned back to its source (diary date or wiki page id) via a
// source_index map so the card can deep-link its origin. Candidates are
// deduped against existing cards (keyword + vector legs, see
// kb.KBStore.CheckCardDuplicatesBatch) before insert.
package cardsgen

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/diary"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/llmjson"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/wiki"
)

// cst is UTC+8 — the day boundary generation windows are cut on, matching
// the diary generator (an alias of diary.CST, the single source of truth).
var cst = diary.CST

// DefaultDailyLimit caps cards per nightly run when cfg.DailyLimit is 0.
const DefaultDailyLimit = 10

// maxWikiPagesPerRun bounds the wiki leg of the prompt so one busy day
// can't flood the LLM input. Pages are newest-updated first, so the cap
// keeps the freshest material.
const maxWikiPagesPerRun = 20

// maxMaterialChars clips each material block so the prompt stays bounded
// even when a wiki summary runs long.
const maxMaterialChars = 1200

// cardSource is one addressable material block: kind ("diary"|"wiki"),
// ref (diary date / wiki page id), and the text the LLM sees.
type cardSource struct {
	kind string
	ref  string
	text string
}

// llmCard is one candidate as returned by the LLM.
type llmCard struct {
	Question    string `json:"question"`
	Answer      string `json:"answer"`
	SourceIndex int    `json:"source_index"`
	Excerpt     string `json:"excerpt"`
}

// Run generates cards for one agent and one date. Material = the day's
// diary entry plus up to maxWikiPagesPerRun wiki pages never yet
// distilled (carded_at IS NULL, newest-updated first) — the page leg is
// consumption-tracked, not day-windowed, so sporadic wiki autogen runs
// and the pre-enablement backlog both drain over successive passes.
// Returns the number of cards created (0 with nil error when the day had
// no material or everything deduped out). The run is stamped into
// kb_card_gen_runs — the nightly sweep's idempotency marker; a manual
// re-run overwrites the stamp (regenerate is explicit).
func Run(
	ctx context.Context,
	dbs *store.DBStore,
	ks *kb.KBStore,
	ws *wiki.WikiStore,
	agentID, date string,
	prov provider.Provider,
	model string,
	dailyLimit int,
) (int, error) {
	if dailyLimit <= 0 {
		dailyLimit = DefaultDailyLimit
	}
	if _, err := time.ParseInLocation("2006-01-02", date, cst); err != nil {
		return 0, fmt.Errorf("cardsgen: parse date %q: %w", date, err)
	}

	sources, err := collectMaterial(ctx, dbs, ws, agentID, date)
	if err != nil {
		return 0, err
	}
	if len(sources) == 0 {
		slog.Info("cardsgen: no material for day", "agent", agentID, "date", date)
		return 0, dbs.StampCardGenRun(ctx, agentID, date, 0, model)
	}

	cards, err := callLLM(ctx, prov, model, sources, dailyLimit)
	if err != nil {
		return 0, fmt.Errorf("cardsgen: llm pass: %w", err)
	}

	// Consume the fed wiki pages on a successful LLM pass — even when the
	// output dedups to zero or the daily cap truncates it, re-feeding the
	// same pages nightly would only burn tokens. A failed pass leaves them
	// uncarded for the next run to retry.
	wikiIDs := make([]string, 0, len(sources))
	for _, s := range sources {
		if s.kind == "wiki" {
			wikiIDs = append(wikiIDs, s.ref)
		}
	}
	if err := ws.MarkPagesCarded(ctx, agentID, wikiIDs); err != nil {
		slog.Warn("cardsgen: mark wiki pages carded failed", "agent", agentID, "error", err)
	}

	// Batch the dedup checks: one embedding call and one embedding-table
	// scan cover the whole candidate set (CheckCardDuplicatesBatch) instead
	// of a round trip per card. Only candidates that survive the empty-q/a
	// filter are checked; same-batch repeats still fall to `seen`.
	type cand struct {
		idx int
		q   string
	}
	var cands []cand
	for i, c := range cards {
		if q := strings.TrimSpace(c.Question); q != "" && strings.TrimSpace(c.Answer) != "" {
			cands = append(cands, cand{idx: i, q: q})
		}
	}
	dup := make(map[int]bool, len(cands))
	if len(cands) > 0 {
		qs := make([]string, len(cands))
		for j, cd := range cands {
			qs[j] = cd.q
		}
		res := ks.CheckCardDuplicatesBatch(ctx, agentID, qs)
		for j, cd := range cands {
			dup[cd.idx] = res[j]
		}
	}

	created, skipped := 0, 0
	seen := map[string]bool{}
	for i, c := range cards {
		q := strings.TrimSpace(c.Question)
		a := strings.TrimSpace(c.Answer)
		if q == "" || a == "" {
			continue
		}
		if created >= dailyLimit {
			break
		}
		// Dedup against existing cards AND within this batch.
		key := strings.ToLower(q)
		if seen[key] || dup[i] {
			skipped++
			continue
		}
		src := clampSource(c.SourceIndex, sources)
		if _, err := ks.SaveCard(ctx, agentID, q, a, src.kind, src.ref, clip(c.Excerpt, 200)); err != nil {
			slog.Warn("cardsgen: save card failed", "agent", agentID, "error", err)
			continue
		}
		seen[key] = true
		created++
	}
	if err := dbs.StampCardGenRun(ctx, agentID, date, created, model); err != nil {
		slog.Warn("cardsgen: stamp run failed", "agent", agentID, "date", date, "error", err)
	}
	slog.Info("cardsgen: done", "agent", agentID, "date", date,
		"material_blocks", len(sources), "candidates", len(cards), "created", created, "deduped", skipped)
	return created, nil
}

// collectMaterial assembles the addressable source blocks: the day's
// diary (themes + blindspots) then the wiki pages never yet distilled,
// newest-updated first, capped at maxWikiPagesPerRun.
func collectMaterial(ctx context.Context, dbs *store.DBStore, ws *wiki.WikiStore, agentID, date string) ([]cardSource, error) {
	var sources []cardSource
	if dia, _ := dbs.GetDailyDiary(ctx, agentID, date); dia != nil {
		var b strings.Builder
		if dia.Overview != "" {
			fmt.Fprintf(&b, "总览: %s\n", dia.Overview)
		}
		for _, th := range dia.Themes {
			fmt.Fprintf(&b, "主题: %s\n%s\n", th.Title, th.Summary)
			for _, p := range th.Points {
				fmt.Fprintf(&b, "- %s\n", p)
			}
		}
		for _, bl := range dia.Blindspots {
			fmt.Fprintf(&b, "可能遗漏: %s（%s）\n", bl.Point, bl.Reason)
		}
		if text := strings.TrimSpace(b.String()); text != "" {
			sources = append(sources, cardSource{kind: "diary", ref: date, text: clip(text, maxMaterialChars)})
		}
	}
	pages, err := ws.ListUncardedPages(ctx, agentID, maxWikiPagesPerRun)
	if err != nil {
		return sources, fmt.Errorf("cardsgen: list uncarded wiki pages: %w", err)
	}
	for _, p := range pages { // newest-updated first (ListUncardedPages order)
		text := strings.TrimSpace(p.Title + "\n" + p.Summary)
		if text == "" {
			continue
		}
		sources = append(sources, cardSource{kind: "wiki", ref: p.ID, text: clip(text, maxMaterialChars)})
	}
	return sources, nil
}

// clampSource maps an LLM source_index back to its block, defensively
// falling back to the first block on out-of-range indices (the card still
// lands, just with an approximate origin) or an empty block when there is
// nothing to point at.
func clampSource(idx int, sources []cardSource) cardSource {
	if len(sources) == 0 {
		return cardSource{kind: "manual"}
	}
	if idx < 0 || idx >= len(sources) {
		idx = 0
	}
	return sources[idx]
}

// callLLM runs the single distillation pass. JSON mode + no-thinking per
// the background-call convention; temp 0.3 mirrors the diary/wiki jobs.
func callLLM(ctx context.Context, prov provider.Provider, model string, sources []cardSource, limit int) ([]llmCard, error) {
	var mat strings.Builder
	for i, s := range sources {
		origin := s.kind
		if s.kind == "diary" {
			origin = "日记 " + s.ref
		} else if s.kind == "wiki" {
			origin = "Wiki 页面"
		}
		fmt.Fprintf(&mat, "=== [idx=%d 来源=%s] ===\n%s\n\n", i, origin, s.text)
	}
	prompt := fmt.Sprintf(`你是知识卡片整理助手。下面是某用户的知识输入材料（日记与 Wiki 页面，Wiki 可能包含更早积累的内容），每块带 idx 编号。你的任务是严格筛选：只有当一条内容是"任何人都能复用的通用知识"时才值得做成卡片。预期一次只出 0~3 张，出 0 张完全正常。

材料:
%s

输出严格 JSON（不要 markdown 代码块）:
{"cards":[{"question":"问题（卡片正面，一句话提问）","answer":"答案（释义/知识点/用法，1-3 句）","source_index":0,"excerpt":"原文中最关键的一句话（不超过 50 字）"}]}

要求:
- 只出知识点卡：概念与定义、原理与规律、方法与步骤、重要结论、关键数据或事实。判断标准：三个月后复习它仍然有收获，且换一个不认识该用户的人来看也有学习价值
- 一律排除：当天做了什么的流水账、日程与计划、情绪与闲聊、临时性或一次性的信息、人人皆知的常识、系统运维细节
- 同样排除这些"伪知识点"：某项目/某任务/某 agent 的进度或状态、某个人的经历或决定、只对该用户自己有意义的配置和偏好、纯实体名介绍（某项目叫什么、某人是谁）
- 个人学习/工作情境类内容一律不出卡：选哪本教材哪个版本及原因、预习或学习进度及其受限因素、某学校/机构/地区的中途换书或做法差异。判据：把"你"换成任何陌生人、把场景换成任何城市，答案仍然成立才是通用知识；否则它只是"某人某地的具体情况"，不是知识
- 日记中的"可能遗漏"行是给用户的元建议，不是知识，不要据此出卡
- 隐私与敏感信息一律不出卡：姓名、住址、电话、证件号、账号密码、API 密钥、财务信息、健康状况、私人关系与私密经历等；question、answer、excerpt 任何一处都不得包含此类内容
- 宁缺毋滥：拿不准就不出；整批材料没有合格知识点时返回空数组
- 输出前自查每一张：如果答案描述的是"发生了什么/做了什么/用了什么"而非"为什么/怎么理解/怎么做"，删掉它；"为什么"句式不自动合格——先确认答案解释的是客观规律，而不是某人的选择、进度或某个学校的做法
- question 必须自包含：不看材料也能明白在问什么
- answer 简明准确，必须基于材料真实内容，不要臆造
- 最多 %d 张，按记忆价值排序`, mat.String(), limit)

	c, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	c = provider.WithJSONMode(c)
	c = provider.WithNoThinking(c)
	resp, err := prov.Chat(c, []provider.Message{{Role: "user", Content: prompt}}, nil, model, 4096, 0.3)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Cards []llmCard `json:"cards"`
	}
	if err := llmjson.UnmarshalLLM(resp.Content, &parsed); err != nil {
		return nil, fmt.Errorf("parse cards: %w", err)
	}
	return parsed.Cards, nil
}

// HasRunFor reports whether a generation pass already stamped (agent,
// date) — the nightly sweep's skip condition.
func HasRunFor(ctx context.Context, dbs *store.DBStore, agentID, date string) bool {
	return dbs.HasDailyRun(ctx, "kb_card_gen_runs", agentID, date)
}

// clip truncates s to at most max runes on a rune boundary.
func clip(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
