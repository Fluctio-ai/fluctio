// Package diary generates per-agent daily-diary entries from the day's
// actual conversation messages (session_messages rows whose created_at
// falls on that UTC+8 day), NOT from conversation_summaries. Summaries
// can't be sliced by day: incremental compaction rewrites a whole
// session's summary rows under one fresh created_at, so a long IM
// session's summary mixes several days under a single timestamp —
// useless for a per-day diary.
//
// Each entry distills one day's messages into themed groups (each
// carrying clickable #seq-N references back into the source turns,
// scoped to the session they came from) plus a "you might have missed"
// blindspot section — the core value the operator can't get from
// scrolling the raw message list.
package diary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/llmjson"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// CST is UTC+8 — the day boundary the diary groups by. It is the single
// source of truth for the product's CST day cut: kb cards, cardsgen, the
// gateway sweeps, and backups all alias this value (their local vars
// exist for call-site readability, not as second definitions).
var CST = time.FixedZone("CST", 8*3600)

// thinkingOff reports whether a pass should run with thinking disabled,
// per AgentDiaryCfg.ThinkingMode.
//
//	"" / "blindspots": theme aggregation = off, blindspot pass = on
//	"off":             both off (fastest, shallow blindspots)
//	"deep":            both on (slowest, deepest)
func thinkingOff(mode string, blindspotPass bool) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off":
		return true
	case "deep":
		return false
	default: // "" / "blindspots"
		return !blindspotPass
	}
}

// idxRef maps one LLM source_index (its handle for a transcript line)
// back to the (session, seq) that line came from, so themes carry
// faithful clickable segments without the LLM ever touching seq values.
type idxRef struct {
	Session string
	Seq     int
}

type llmTheme struct {
	Title         string   `json:"title"`
	Summary       string   `json:"summary"`
	Points        []string `json:"points"`
	SourceIndices []int    `json:"source_indices"`
}

type llmBlindspot struct {
	Point  string `json:"point"`
	Reason string `json:"reason"`
}

// Generate produces one day's diary for an agent. `date` is YYYY-MM-DD
// (UTC+8). Returns the persisted entry, or (nil, nil) when the day had
// no messages worth a diary (the caller then leaves that day absent so
// the UI shows "no diary" rather than an empty one).
func Generate(
	ctx context.Context,
	dbs *store.DBStore,
	agentID, date string,
	prov provider.Provider,
	model, thinkingMode string,
) (*store.DailyDiary, error) {
	day, err := time.ParseInLocation("2006-01-02", date, CST)
	if err != nil {
		return nil, fmt.Errorf("diary: parse date %q: %w", date, err)
	}
	// Pad the window ±30min so a topic that straddles midnight (asked
	// 23:50, answered 00:10) isn't cut in half — both turns land in the
	// transcript. A uniform time pad beats per-session seq padding: one
	// query, no second pass, and the buffer is the same across sessions.
	from := day.Add(-30 * time.Minute)
	to := day.AddDate(0, 0, 1).Add(30 * time.Minute)

	msgs, err := dbs.ListSessionMessagesByAgentAndTimeRange(ctx, agentID, from, to)
	if err != nil {
		return nil, fmt.Errorf("diary: list messages: %w", err)
	}
	slog.Info("diary: loaded", "agent", agentID, "date", date, "raw", len(msgs))
	// Keep only genuine conversation: user questions and the assistant's
	// text replies. Drop tool-result rows (role=tool — raw tool output is
	// noise that distorts theme extraction), system rows, runtime-injected
	// turns (origin!=""), hidden regex-hook turns (llm_visible=false), and
	// empty/tool-call-only assistant turns. Thinking lives in a separate
	// field that is never read here, so reasoning traces never leak in.
	visible := msgs[:0]
	for i := range msgs {
		m := msgs[i]
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if !m.LLMVisible || m.Origin != "" {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		visible = append(visible, m)
	}
	msgs = visible
	slog.Info("diary: filtered", "agent", agentID, "date", date, "kept", len(msgs))
	if len(msgs) == 0 {
		// No conversations that day. Persist a row with no themes/
		// blindspots so the calendar can mark it "tried, empty" (red框)
		// — distinct from "never generated" (grey). The list filters
		// these out; they exist only as calendar markers.
		entry := store.DailyDiary{
			AgentID:     agentID,
			Date:        date,
			Overview:    "该日无对话内容",
			Themes:      []store.DiaryTheme{},
			Blindspots:  []store.DiaryBlindspot{},
			Model:       model,
			GeneratedAt: time.Now(),
		}
		if err := dbs.InsertDailyDiary(ctx, entry); err != nil {
			return nil, fmt.Errorf("diary: insert empty marker: %w", err)
		}
		return &entry, nil
	}

	// Build a per-session-grouped transcript (each conversation as a
	// contiguous block) plus the idx→(session,seq) map. Grouping keeps
	// each session's context intact instead of interleaving turns across
	// sessions on the raw global-timeline order from the query.
	refs, transcript := buildTranscript(msgs)

	// Pass 1 — theme aggregation.
	themes, err := aggregateThemes(ctx, prov, model, transcript, thinkingMode)
	if err != nil {
		return nil, fmt.Errorf("diary: aggregate themes: %w", err)
	}

	// Map LLM themes back to real (session, seq) segments via source_indices.
	mapped := mapThemes(themes, refs)

	// Pass 2 — blindspots (the core value).
	blindspots, err := findBlindspots(ctx, prov, model, transcript, themes, thinkingMode)
	if err != nil {
		slog.Warn("diary: blindspot pass failed, continuing without", "agent", agentID, "date", date, "error", err)
		blindspots = nil
	}

	overview := buildOverview(mapped)

	entry := store.DailyDiary{
		AgentID:     agentID,
		Date:        date,
		Overview:    overview,
		Themes:      mapped,
		Blindspots:  blindspots,
		Model:       model,
		GeneratedAt: time.Now(),
	}
	if err := dbs.InsertDailyDiary(ctx, entry); err != nil {
		return nil, fmt.Errorf("diary: insert: %w", err)
	}
	return &entry, nil
}

// buildTranscript arranges messages into per-session blocks — turns time-
// ordered within each block, blocks ordered by the session's earliest turn
// — and emits a transcript with a global idx on every line plus an idxRef
// slice mapping each idx back to (session, seq). Blocking keeps each
// conversation contiguous for the LLM instead of interleaving turns across
// sessions on a global timeline. The query already returns rows visible &
// role-filtered by the caller; buildTranscript only reorders and formats.
func buildTranscript(msgs []store.SessionMessage) ([]idxRef, string) {
	type group struct {
		session string
		first   time.Time
		msgs    []store.SessionMessage
	}
	groups := map[string]*group{}
	for i := range msgs {
		m := msgs[i]
		g, ok := groups[m.SessionKey]
		if !ok {
			g = &group{session: m.SessionKey, first: m.Timestamp}
			groups[m.SessionKey] = g
		}
		g.msgs = append(g.msgs, m)
	}
	var gs []*group
	for _, g := range groups {
		sort.SliceStable(g.msgs, func(i, j int) bool {
			return g.msgs[i].Timestamp.Before(g.msgs[j].Timestamp)
		})
		gs = append(gs, g)
	}
	sort.SliceStable(gs, func(i, j int) bool {
		return gs[i].first.Before(gs[j].first)
	})
	var b strings.Builder
	refs := make([]idxRef, 0, len(msgs))
	idx := 0
	for gi, g := range gs {
		fmt.Fprintf(&b, "=== 会话 %s ===\n", shortKey(g.session))
		for _, m := range g.msgs {
			content := m.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			fmt.Fprintf(&b, "[idx=%d seq=%d role=%s] %s\n", idx, m.Seq, m.Role, content)
			refs = append(refs, idxRef{Session: m.SessionKey, Seq: m.Seq})
			idx++
		}
		if gi < len(gs)-1 {
			b.WriteString("\n")
		}
	}
	return refs, b.String()
}

// shortKey shortens a session key for the transcript so the model isn't
// flooded with long uuids — the full key still lives in refs for segments.
func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12]
	}
	return k
}

// mapThemes converts LLM themes (which carry source_indices into the
// transcript) into DiaryThemes with faithful (session, seq) segments.
// Each source index becomes one segment ref pinned to its own turn; the
// frontend groups them per session when rendering #seq-N deep links.
func mapThemes(themes []llmTheme, refs []idxRef) []store.DiaryTheme {
	out := make([]store.DiaryTheme, 0, len(themes))
	for _, lt := range themes {
		if strings.TrimSpace(lt.Title) == "" && strings.TrimSpace(lt.Summary) == "" {
			continue
		}
		theme := store.DiaryTheme{
			Title:   strings.TrimSpace(lt.Title),
			Summary: strings.TrimSpace(lt.Summary),
			Points:  lt.Points,
		}
		seen := map[string]bool{}
		for _, idx := range lt.SourceIndices {
			if idx < 0 || idx >= len(refs) {
				continue
			}
			r := refs[idx]
			key := r.Session + "|" + strconv.Itoa(r.Seq)
			if !seen[key] {
				theme.Segments = append(theme.Segments, store.DiarySegRef{
					Session: r.Session, Start: r.Seq, End: r.Seq,
				})
				seen[key] = true
			}
			if theme.Session == "" {
				theme.Session = r.Session
			}
		}
		out = append(out, theme)
	}
	return out
}

func buildOverview(themes []store.DiaryTheme) string {
	if len(themes) == 0 {
		return ""
	}
	var parts []string
	max := 4
	for i, t := range themes {
		if i >= max {
			break
		}
		if t.Title != "" {
			parts = append(parts, t.Title)
		}
	}
	if len(parts) == 0 {
		return themes[0].Summary
	}
	return strings.Join(parts, " · ")
}

func aggregateThemes(ctx context.Context, prov provider.Provider, model, transcript, thinkingMode string) ([]llmTheme, error) {
	prompt := fmt.Sprintf(`你是日记整理助手。下面是某用户一天内跨多个对话（session）的原始聊天记录，每条带 idx（唯一编号）/session/seq/角色。请将它们归并成当天的若干"主题"。

聊天记录:
%s

输出严格 JSON（不要 markdown 代码块）:
{"themes":[{"title":"主题标题","summary":"2-3句概述","points":["要点1","要点2"],"source_indices":[0,3]}]}

要求:
- source_indices 填该主题涉及的聊天记录 idx
- 同一主题跨多个 session 的片段要合并
- 排除: 播报/天气/TTS/开门/纯闲聊/系统运维/cron 等无价值内容（直接不纳入）
- 保留: 深度讨论/灵感想法/资料分享/重要决策
- 3-8 个主题，按重要性排序
- points 每条简短（一句话）`, transcript)

	resp, err := callLLM(ctx, prov, model, prompt, 6144, thinkingOff(thinkingMode, false))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Themes []llmTheme `json:"themes"`
	}
	if err := llmjson.UnmarshalLLM(resp, &parsed); err != nil {
		return nil, fmt.Errorf("parse themes: %w", err)
	}
	return parsed.Themes, nil
}

func findBlindspots(ctx context.Context, prov provider.Provider, model, transcript string, themes []llmTheme, thinkingMode string) ([]store.DiaryBlindspot, error) {
	if len(transcript) == 0 {
		return nil, nil
	}
	themesJSON, _ := json.Marshal(themes)
	prompt := fmt.Sprintf(`下面是某用户今天的原始聊天记录，以及已归纳的主题。请找出"用户可能忽略的重点"——记录里确实出现、有价值，但用户没有深入跟进/没列待办/没进一步行动的点。

聊天记录:
%s

已归纳主题:
%s

输出严格 JSON（不要 markdown 代码块）:
{"blindspots":[{"point":"被忽略的要点","reason":"为什么重要/为何容易被忽略"}]}

要求:
- 每条必须基于聊天记录里真实出现的内容，不要臆造
- 聚焦真正重要的；3-6 条；若确实没有遗漏可返回空数组
- point 简明，reason 一句话`, transcript, themesJSON)

	resp, err := callLLM(ctx, prov, model, prompt, 3072, thinkingOff(thinkingMode, true))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Blindspots []llmBlindspot `json:"blindspots"`
	}
	if err := llmjson.UnmarshalLLM(resp, &parsed); err != nil {
		return nil, fmt.Errorf("parse blindspots: %w", err)
	}
	out := make([]store.DiaryBlindspot, 0, len(parsed.Blindspots))
	for _, b := range parsed.Blindspots {
		if strings.TrimSpace(b.Point) == "" {
			continue
		}
		out = append(out, store.DiaryBlindspot{Point: b.Point, Reason: b.Reason})
	}
	return out, nil
}

// callLLM wraps a single Chat call with JSON mode, a timeout, and
// thinking gated by `thinkOff` (true = disable thinking).
func callLLM(ctx context.Context, prov provider.Provider, model, prompt string, maxTokens int, thinkOff bool) (string, error) {
	c, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	c = provider.WithJSONMode(c)
	if thinkOff {
		c = provider.WithNoThinking(c)
	}
	resp, err := prov.Chat(c, []provider.Message{{Role: "user", Content: prompt}}, nil, model, maxTokens, 0.3)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

