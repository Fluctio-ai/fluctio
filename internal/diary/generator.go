// Package diary generates per-agent daily-diary entries from the
// conversation_summaries the agent already produces. Each entry distills
// one UTC+8 day's summaries into themed groups (each carrying clickable
// #seq-N references back into the source turns) plus a "you might have
// missed" blindspot section — the core value the operator can't get from
// reading the raw summary list.
package diary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// cst is UTC+8 — the day boundary the diary groups by.
var cst = time.FixedZone("CST", 8*3600)

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

// summaryInput is the JSON shape fed to the LLM: one entry per
// conversation_summaries row, each tagged with its list index so the
// model references it by source_indices instead of touching seq values
// directly (seq stays faithful — the Go side maps indices back to real
// segments).
type summaryInput struct {
	Index      int      `json:"index"`
	Topic      string   `json:"topic"`
	Summary    string   `json:"summary"`
	Keywords   []string `json:"keywords"`
	Importance int      `json:"importance"`
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
// no summaries worth a diary (the caller then leaves that day absent so
// the UI shows "no diary" rather than an empty one).
func Generate(
	ctx context.Context,
	dbs *store.DBStore,
	agentID, date string,
	prov provider.Provider,
	model, thinkingMode string,
) (*store.DailyDiary, error) {
	day, err := time.ParseInLocation("2006-01-02", date, cst)
	if err != nil {
		return nil, fmt.Errorf("diary: parse date %q: %w", date, err)
	}
	from := day
	to := day.AddDate(0, 0, 1)

	summaries, err := dbs.ListConversationSummariesByDateRange(ctx, agentID, from, to)
	if err != nil {
		return nil, fmt.Errorf("diary: list summaries: %w", err)
	}
	if len(summaries) == 0 {
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

	// Build the LLM input, pre-sorting by importance (desc) then time so
	// the model sees the most salient topics first.
	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].Importance != summaries[j].Importance {
			return summaries[i].Importance > summaries[j].Importance
		}
		return summaries[i].CreatedAt.Before(summaries[j].CreatedAt)
	})
	inputs := make([]summaryInput, 0, len(summaries))
	for i, s := range summaries {
		inputs = append(inputs, summaryInput{
			Index:      i,
			Topic:      s.Topic,
			Summary:    s.Summary,
			Keywords:   s.Keywords,
			Importance: s.Importance,
		})
	}

	// Pass 1 — theme aggregation.
	themes, err := aggregateThemes(ctx, prov, model, inputs, thinkingMode)
	if err != nil {
		return nil, fmt.Errorf("diary: aggregate themes: %w", err)
	}

	// Map LLM themes back to real seq segments via source_indices.
	mapped := mapThemes(themes, summaries)

	// Pass 2 — blindspots (the core value).
	blindspots, err := findBlindspots(ctx, prov, model, inputs, themes, thinkingMode)
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

// mapThemes converts LLM themes (which carry source_indices) into
// DiaryThemes with faithful seq segments pulled from the source rows.
func mapThemes(themes []llmTheme, summaries []store.ConversationSummary) []store.DiaryTheme {
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
			if idx < 0 || idx >= len(summaries) {
				continue
			}
			s := summaries[idx]
			if s.Topic != "" && !seen["t:"+s.Topic] {
				theme.Topics = append(theme.Topics, s.Topic)
				seen["t:"+s.Topic] = true
			}
			if theme.Session == "" {
				theme.Session = s.SessionKey
			}
			if len(s.Segments) > 0 {
				for _, seg := range s.Segments {
					theme.Segments = append(theme.Segments, store.DiarySegRef{
						Session: s.SessionKey, Start: seg[0], End: seg[1],
					})
				}
			} else if s.SeqStart >= 0 {
				theme.Segments = append(theme.Segments, store.DiarySegRef{
					Session: s.SessionKey, Start: s.SeqStart, End: s.SeqEnd,
				})
			}
		}
		if theme.Session == "" && len(lt.SourceIndices) == 0 && len(summaries) > 0 {
			theme.Session = summaries[0].SessionKey
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

func aggregateThemes(ctx context.Context, prov provider.Provider, model string, inputs []summaryInput, thinkingMode string) ([]llmTheme, error) {
	inputsJSON, _ := json.Marshal(inputs)
	prompt := fmt.Sprintf(`你是日记整理助手。下面是某用户一天内多个对话的主题摘要（系统已分段提取，每条带 index/topic/摘要/关键词/重要性）。请将它们归并成当天的若干"主题"。

输入摘要:
%s

输出严格 JSON（不要 markdown 代码块）:
{"themes":[{"title":"主题标题","summary":"2-3句概述","points":["要点1","要点2"],"source_indices":[0,3]}]}

要求:
- 合并相近 topic 为一个主题；source_indices 填该主题涉及的摘要 index
- 排除: 播报/天气/TTS/开门/纯闲聊/系统运维/cron 等无价值内容（直接不纳入）
- 保留: 深度讨论/灵感想法/资料分享/重要决策
- 3-8 个主题，按重要性排序
- points 每条简短（一句话）`, inputsJSON)

	resp, err := callLLM(ctx, prov, model, prompt, 6144, thinkingOff(thinkingMode, false))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Themes []llmTheme `json:"themes"`
	}
	if err := parseJSONLoose(resp, &parsed); err != nil {
		return nil, fmt.Errorf("parse themes: %w", err)
	}
	return parsed.Themes, nil
}

func findBlindspots(ctx context.Context, prov provider.Provider, model string, inputs []summaryInput, themes []llmTheme, thinkingMode string) ([]store.DiaryBlindspot, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	inputsJSON, _ := json.Marshal(inputs)
	themesJSON, _ := json.Marshal(themes)
	prompt := fmt.Sprintf(`下面是某用户今天的对话主题摘要，以及已归纳的主题。请找出"用户可能忽略的重点"——摘要里确实出现、有价值，但用户没有深入跟进/没列待办/没进一步行动的点。

主题摘要:
%s

已归纳主题:
%s

输出严格 JSON（不要 markdown 代码块）:
{"blindspots":[{"point":"被忽略的要点","reason":"为什么重要/为何容易被忽略"}]}

要求:
- 每条必须基于摘要里真实出现的内容，不要臆造
- 聚焦真正重要的；3-6 条；若确实没有遗漏可返回空数组
- point 简明，reason 一句话`, inputsJSON, themesJSON)

	resp, err := callLLM(ctx, prov, model, prompt, 3072, thinkingOff(thinkingMode, true))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Blindspots []llmBlindspot `json:"blindspots"`
	}
	if err := parseJSONLoose(resp, &parsed); err != nil {
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

// parseJSONLoose strips a ```json fence if present then unmarshals.
func parseJSONLoose(s string, v any) error {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	return json.Unmarshal([]byte(s), v)
}
