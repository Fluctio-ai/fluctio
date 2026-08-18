// Package cardsgen generates spaced-repetition Q&A cards from one day's
// knowledge input: the agent's daily diary plus the wiki pages touched
// that day. One LLM pass (no-thinking + JSON mode, per the background-call
// convention) distills the material into question/answer pairs, each
// pinned back to its source (diary date or wiki page id) via a
// source_index map so the card can deep-link its origin. Candidates are
// deduped against existing cards (keyword + vector legs, see
// kb.KBStore.CheckCardDuplicate) before insert.
package cardsgen

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/wiki"
)

// cst is UTC+8 — the day boundary generation windows are cut on, matching
// the diary generator.
var cst = time.FixedZone("CST", 8*3600)

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
// diary entry plus wiki pages whose updated_at falls in [day, day+1).
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
	day, err := time.ParseInLocation("2006-01-02", date, cst)
	if err != nil {
		return 0, fmt.Errorf("cardsgen: parse date %q: %w", date, err)
	}
	from := day
	to := day.AddDate(0, 0, 1)

	sources, err := collectMaterial(ctx, dbs, ws, agentID, date, from, to)
	if err != nil {
		return 0, err
	}
	if len(sources) == 0 {
		slog.Info("cardsgen: no material for day", "agent", agentID, "date", date)
		return 0, stampRun(ctx, dbs, agentID, date, 0, model)
	}

	cards, err := callLLM(ctx, prov, model, sources, dailyLimit)
	if err != nil {
		return 0, fmt.Errorf("cardsgen: llm pass: %w", err)
	}

	created, skipped := 0, 0
	seen := map[string]bool{}
	for _, c := range cards {
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
		if seen[key] || ks.CheckCardDuplicate(ctx, agentID, q) != nil {
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
	if err := stampRun(ctx, dbs, agentID, date, created, model); err != nil {
		slog.Warn("cardsgen: stamp run failed", "agent", agentID, "date", date, "error", err)
	}
	slog.Info("cardsgen: done", "agent", agentID, "date", date,
		"material_blocks", len(sources), "candidates", len(cards), "created", created, "deduped", skipped)
	return created, nil
}

// collectMaterial assembles the addressable source blocks: yesterday's
// diary (themes + blindspots) then the wiki pages touched in the window,
// newest-updated first, capped at maxWikiPagesPerRun.
func collectMaterial(ctx context.Context, dbs *store.DBStore, ws *wiki.WikiStore, agentID, date string, from, to time.Time) ([]cardSource, error) {
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
	pages, _, err := ws.ListPages(ctx, agentID, "", 500, 0)
	if err != nil {
		return sources, fmt.Errorf("cardsgen: list wiki pages: %w", err)
	}
	n := 0
	for _, p := range pages { // newest-updated first (ListPages order)
		if p.UpdatedAt.Before(from) || !p.UpdatedAt.Before(to) {
			continue
		}
		text := strings.TrimSpace(p.Title + "\n" + p.Summary)
		if text == "" {
			continue
		}
		sources = append(sources, cardSource{kind: "wiki", ref: p.ID, text: clip(text, maxMaterialChars)})
		n++
		if n >= maxWikiPagesPerRun {
			break
		}
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
	prompt := fmt.Sprintf(`你是知识卡片整理助手。下面是某用户一天的知识输入材料（日记与 Wiki 页面），每块带 idx 编号。请从中筛选"值得长期记住的有价值知识点"，生成问答复习卡片。

材料:
%s

输出严格 JSON（不要 markdown 代码块）:
{"cards":[{"question":"问题（卡片正面，一句话提问）","answer":"答案（释义/知识点/用法，1-3 句）","source_index":0,"excerpt":"原文中最关键的一句话（不超过 50 字）"}]}

要求:
- 只挑值得记住的：概念定义、方法与用法、重要决策、容易遗忘的细节
- 排除：琐事、闲聊、情绪、系统运维等无长期价值的内容
- question 必须自包含：不看材料也能明白在问什么
- answer 简明准确，必须基于材料真实内容，不要臆造
- 最多 %d 张，按记忆价值排序；若没有值得出卡的内容返回空数组`, mat.String(), limit)

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
	if err := parseJSONLoose(resp.Content, &parsed); err != nil {
		return nil, fmt.Errorf("parse cards: %w", err)
	}
	return parsed.Cards, nil
}

// stampRun upserts the (agent, date) generation marker — idempotency for
// the nightly sweep plus a creation log.
func stampRun(ctx context.Context, dbs *store.DBStore, agentID, date string, created int, model string) error {
	ph := func(n int) string {
		if dbs.Dialect() == "postgres" {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	q := `INSERT INTO kb_card_gen_runs (agent_id, date, created, model, created_at)
		VALUES (` + ph(1) + `,` + ph(2) + `,` + ph(3) + `,` + ph(4) + `,CURRENT_TIMESTAMP)
		ON CONFLICT(agent_id, date) DO UPDATE SET created=excluded.created, model=excluded.model, created_at=CURRENT_TIMESTAMP`
	_, err := dbs.DB().ExecContext(ctx, q, agentID, date, created, model)
	return err
}

// HasRunFor reports whether a generation pass already stamped (agent,
// date) — the nightly sweep's skip condition.
func HasRunFor(ctx context.Context, dbs *store.DBStore, agentID, date string) bool {
	ph := func(n int) string {
		if dbs.Dialect() == "postgres" {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	var one int
	err := dbs.DB().QueryRowContext(ctx,
		`SELECT 1 FROM kb_card_gen_runs WHERE agent_id = `+ph(1)+` AND date = `+ph(2),
		agentID, date).Scan(&one)
	return err == nil
}

// parseJSONLoose strips a ```json fence if present then unmarshals.
func parseJSONLoose(s string, v any) error {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	return json.Unmarshal([]byte(s), v)
}

// clip truncates s to at most max runes on a rune boundary.
func clip(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
