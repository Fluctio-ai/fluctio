package diag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// LLMCompleter is a one-shot text-generation backend (agent.Complete
// satisfies it). Kept as an interface so this package doesn't import agent.
type LLMCompleter interface {
	Complete(ctx context.Context, messages []provider.Message, maxTokens int) (string, error)
}

// ReportOptions controls failure-report generation.
type ReportOptions struct {
	Since     time.Time // failures at or after this time
	AgentID   string    // "" = all agents
	TopCases  int       // representative cases to detail (default 8)
	MaxTokens int       // LLM output budget (default 2048)
	OutDir    string    // output dir (default ~/.fluctio/diag-reports/)
}

// GenerateReport collects recent failures, attributes each representative
// case, hands the overview + cases to the LLM to write a structured Markdown
// report, writes it to OutDir, and returns the file path.
//
// No-PII by construction: it uses only llm_call_diag + session_events
// metadata (status / http code / tool names / failure classes / token
// counts) — never session_messages conversation content.
func GenerateReport(ctx context.Context, st *store.DBStore, llm LLMCompleter, opts ReportOptions) (string, error) {
	if opts.TopCases <= 0 {
		opts.TopCases = 8
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 2048
	}
	if opts.OutDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		opts.OutDir = filepath.Join(home, ".fluctio", "diag-reports")
	}

	// Pull recent failures (more than TopCases so we can dedup by session).
	failed, err := st.ListFailedLLMCalls(ctx, opts.Since, opts.AgentID, opts.TopCases*3)
	if err != nil {
		return "", fmt.Errorf("list failed calls: %w", err)
	}

	overview := buildOverview(failed, opts)
	cases := pickCases(ctx, st, failed, opts.TopCases)

	prompt := buildReportPrompt(overview, cases)
	msgs := []provider.Message{
		{Role: "system", Content: reportSystemPrompt},
		{Role: "user", Content: prompt},
	}
	content, err := llm.Complete(ctx, msgs, opts.MaxTokens)
	if err != nil {
		return "", fmt.Errorf("llm generate report: %w", err)
	}

	body := reportHeader(opts, overview) + "\n\n" + strings.TrimSpace(content) + "\n"
	return writeReport(opts.OutDir, body)
}

// failureOverview aggregates the failure distribution for the report.
type failureOverview struct {
	Total    int
	ByStatus map[string]int
	ByHTTP   map[int]int
	ByAgent  map[string]int
	Since    time.Time
	AgentID  string
}

func buildOverview(rows []store.LLMCallDiagRow, opts ReportOptions) failureOverview {
	ov := failureOverview{
		Total: len(rows), ByStatus: map[string]int{}, ByHTTP: map[int]int{}, ByAgent: map[string]int{},
		Since: opts.Since, AgentID: opts.AgentID,
	}
	for _, r := range rows {
		ov.ByStatus[r.Status]++
		ov.ByHTTP[r.HTTPStatus]++
		ov.ByAgent[r.AgentID]++
	}
	return ov
}

type caseReport struct {
	AgentID   string
	SessionKey string
	Time      time.Time
	RootCause RootCause
	HasCause  bool
	Timeline  []TimelineEntry
}

// pickCases selects up to n distinct (agent, session) pairs and builds each
// one's attributed timeline. ListFailedLLMCalls is already newest-first, so
// dedup preserves recency.
func pickCases(ctx context.Context, st *store.DBStore, failed []store.LLMCallDiagRow, n int) []caseReport {
	seen := map[string]bool{}
	var cases []caseReport
	for _, f := range failed {
		key := f.AgentID + "|" + f.SessionKey
		if seen[key] {
			continue
		}
		seen[key] = true
		llm, _ := st.ListLLMCallDiagBySession(ctx, f.SessionKey)
		events, _ := st.ListSessionEventsSince(ctx, f.AgentID, f.SessionKey, -1)
		tl := BuildTimeline(llm, events)
		rc, ok := Attribute(tl)
		cases = append(cases, caseReport{
			AgentID: f.AgentID, SessionKey: f.SessionKey, Time: f.CreatedAt,
			RootCause: rc, HasCause: ok, Timeline: tl,
		})
		if len(cases) >= n {
			break
		}
	}
	return cases
}

const reportSystemPrompt = `You are a diagnostic report generator for the Fluctio agent platform. You receive structured failure data (a failure distribution + attributed representative cases) and write a concise Markdown report a developer can hand to a coding assistant to fix the underlying code.

Output exactly these sections (in Chinese, Markdown):
## 概览
One line: time window, total failures, the dominant status / http code.
## 常见模式
The 1-3 most frequent failure shapes, each as a bullet with frequency hint.
## 代表 case 分析
For each provided case: a sub-heading "### Case N: <agent> @ <time>", then:
- **Root cause**: the rule + confidence + one-line summary (use the provided attribution; don't second-guess it).
- **Timeline**: a compact one-line-per-step list (time, kind, status, key detail).
- **Code leads**: concrete file/package hints — e.g. a failed tool "web_search" → internal/toolproviders/websearch/; an LLM http 500 → provider/internal/openai.go or anthropic.go + check the model's upstream. If the rule is empty-response → prompt/context issue, point at the agent's system prompt config.
## 建议
2-4 prioritized fix directions, each naming the likely code location.

Rules:
- Use ONLY the provided data. Don't invent statuses, codes, or steps.
- If a case has no root cause (HasCause=false), say "无明显根因" and skip Code leads for it.
- Keep the whole report under ~500 words. No preamble before "## 概览".`

func buildReportPrompt(ov failureOverview, cases []caseReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TIME WINDOW: since %s\n", ov.Since.UTC().Format(time.RFC3339))
	if ov.AgentID != "" {
		fmt.Fprintf(&b, "AGENT FILTER: %s\n", ov.AgentID)
	}
	fmt.Fprintf(&b, "TOTAL FAILURES: %d\n", ov.Total)
	fmt.Fprintf(&b, "BY STATUS: %s\n", mapSummary(ov.ByStatus))
	fmt.Fprintf(&b, "BY HTTP CODE: %s\n", intMapSummary(ov.ByHTTP))
	fmt.Fprintf(&b, "BY AGENT: %s\n", mapSummary(ov.ByAgent))
	fmt.Fprintf(&b, "\nCASES (%d, newest first):\n", len(cases))
	for i, c := range cases {
		fmt.Fprintf(&b, "\n--- CASE %d: agent=%s session=%s @ %s ---\n",
			i+1, c.AgentID, c.SessionKey, c.Time.UTC().Format(time.RFC3339))
		if c.HasCause {
			fmt.Fprintf(&b, "root_cause: [rule=%s confidence=%s] %s\n",
				c.RootCause.Rule, c.RootCause.Confidence, c.RootCause.Summary)
			for _, ev := range c.RootCause.Evidence {
				fmt.Fprintf(&b, "  evidence: %s\n", ev)
			}
		} else {
			fmt.Fprintf(&b, "root_cause: (none — no rule fired)\n")
		}
		fmt.Fprintln(&b, "timeline:")
		for _, e := range c.Timeline {
			fmt.Fprintf(&b, "  %s %-11s %-12s", e.Time.Format("15:04:05"), e.Kind, e.Status)
			if e.Failed {
				fmt.Fprintf(&b, " FAILED")
				if e.FailKind != "" {
					fmt.Fprintf(&b, "[%s]", e.FailKind)
				}
			}
			if e.HTTPStatus != 0 {
				fmt.Fprintf(&b, " http=%d", e.HTTPStatus)
			}
			if e.ToolCallCount > 0 {
				fmt.Fprintf(&b, " tool_calls=%d", e.ToolCallCount)
			}
			if e.ResponseChars > 0 {
				fmt.Fprintf(&b, " chars=%d", e.ResponseChars)
			}
			if e.Detail != "" {
				fmt.Fprintf(&b, " %s", truncate(e.Detail, 60))
			}
			fmt.Fprintln(&b)
		}
	}
	return b.String()
}

func reportHeader(opts ReportOptions, ov failureOverview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Fluctio 错误报告\n\n")
	fmt.Fprintf(&b, "> 生成时间: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "> 时间窗: since %s", opts.Since.UTC().Format(time.RFC3339))
	if opts.AgentID != "" {
		fmt.Fprintf(&b, " | agent: %s", opts.AgentID)
	}
	fmt.Fprintf(&b, "\n> 失败调用: %d | 代表 case: %d\n", ov.Total, opts.TopCases)
	fmt.Fprintf(&b, "> (本报告由 llm_call_diag + session_events 元数据生成，不含对话内容)\n")
	return b.String()
}

func writeReport(dir, body string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir diag-reports: %w", err)
	}
	name := "report-" + time.Now().UTC().Format("20060102T150405Z") + ".md"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	return path, nil
}

func mapSummary(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			k = "(empty)"
		}
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func intMapSummary(m map[int]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
