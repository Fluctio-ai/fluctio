package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/httpclient"
)

func RegisterKBTools(r *tools.Registry, store *KBStore, agentID string, sourceRatioFn func() float64, thresholdFn func() float64, insightInvoker InsightInvoker, insightModel string, insightMaxTokens int) {
	registerKBSearch(r, store, agentID, sourceRatioFn, thresholdFn)
	registerKBSearchRaw(r, store, agentID)
	registerKBAdd(r, store, agentID)
	registerKBIngestURL(r, store, agentID)
	registerKBList(r, store, agentID)
	registerKBDelete(r, store, agentID)
	registerKBFlash(r, store, agentID)
	registerKBSaveTodo(r, store, agentID)
	registerKBUpdateTodo(r, store, agentID)
	registerKBListTodos(r, store, agentID)
	// The deep-reading tool needs an LLM invoker; when none is wired (e.g. an
	// agent without a provider) the tool is simply unavailable rather than
	// registered-but-broken.
	if insightInvoker != nil {
		registerKBGenerateInsights(r, store, agentID, insightInvoker, insightModel, insightMaxTokens)
	}
}

func registerKBSearch(r *tools.Registry, store *KBStore, agentID string, sourceRatioFn func() float64, thresholdFn func() float64) {
	r.Register("knowledgebase_search", "Search the agent's knowledge base for relevant information. Returns matching text chunks with source references. Use this when the user's question might be answered from previously stored knowledge.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The search query to find relevant knowledge base entries",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of results to return (default 5)",
			},
		},
		"required": []string{"query"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Query string `json:"query"`
			Limit int    `json:"limit,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 5
		}
		results, err := store.Search(ctx, agentID, args.Query, limit, 0, resolveRatio(sourceRatioFn), resolveThreshold(thresholdFn))
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return "No matching entries found in the knowledge base.", nil
		}
		deduped, ids := numberAndAccumulate(ctx, results)
		if len(deduped) == 0 {
			return "All matching sources were already cited earlier; no new results.", nil
		}
		return formatResults(deduped, args.Query, ids), nil
	})
}

// numberAndAccumulate dedups results by title against the ctx accumulator: a
// source already cited earlier this turn (by auto-query or a prior tool call)
// is dropped — NOT re-fed to the LLM — so wiki/raw overlap doesn't bloat the
// context with repeated content. Fresh sources get continuing [K#] ids
// (len(acc)+1) and are appended. Returns the deduped results + their ids.
func numberAndAccumulate(ctx context.Context, results []KBResult) ([]KBResult, []string) {
	acc := SourcesFromCtx(ctx)
	cited := map[string]bool{}
	if acc != nil {
		for _, s := range *acc {
			cited[s.File] = true
		}
	}
	nextID := 1
	if acc != nil {
		nextID = len(*acc) + 1
	}
	var deduped []KBResult
	var ids []string
	seen := map[string]bool{} // dedup within this batch too
	for _, r := range results {
		if seen[r.SourceTitle] || cited[r.SourceTitle] {
			continue
		}
		seen[r.SourceTitle] = true
		id := fmt.Sprintf("K%d", nextID)
		nextID++
		deduped = append(deduped, r)
		ids = append(ids, id)
		if acc != nil {
			*acc = append(*acc, KnowledgeSource{
				ID: id, File: r.SourceTitle, Kind: r.SourceKind, PageType: r.PageType, Chunk: r.ChunkIndex,
			})
		}
	}
	return deduped, ids
}

// resolveRatio reads the agent's configured source ratio, defaulting to 0.5.
func resolveRatio(fn func() float64) float64 {
	if fn != nil {
		if r := fn(); r >= 0 && r <= 1 {
			return r
		}
	}
	return 0.5
}

// resolveThreshold reads the agent's configured relevance threshold,
// defaulting to 0.45.
func resolveThreshold(fn func() float64) float64 {
	if fn != nil {
		if t := fn(); t >= 0 && t <= 1 {
			return t
		}
	}
	return 0.45
}

func registerKBSearchRaw(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_search_raw", "Fetch the RAW original text chunks of specific knowledge-base sources (verbatim, from kb_entries). Call this AFTER knowledgebase_search, passing the source_id values it returned, when the wiki-based summary is not detailed enough and you need the exact original wording/passages. Optionally narrow with a query phrase. Returns verbatim source chunks with [K#] citation ids.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"source_ids": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "source_id values from a prior knowledgebase_search result — the sources whose raw chunks you want. Required.",
			},
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Optional phrase to narrow within the selected sources (LIKE match on chunk text). Omit to pull all chunks of the given sources.",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of raw chunks to return (default 5)",
			},
		},
		"required": []string{"source_ids"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			SourceIDs []string `json:"source_ids"`
			Query     string   `json:"query,omitempty"`
			Limit     int      `json:"limit,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if len(args.SourceIDs) == 0 {
			return "", fmt.Errorf("source_ids is required — call knowledgebase_search first and pass the source_id values it returned")
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 5
		}
		results, err := store.SearchRawKB(ctx, agentID, args.Query, args.SourceIDs, limit)
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return "No matching raw entries found in the knowledge base.", nil
		}
		deduped, ids := numberAndAccumulate(ctx, results)
		if len(deduped) == 0 {
			return "All matching sources were already cited earlier; no new results.", nil
		}
		return formatResults(deduped, args.Query, ids), nil
	})
}

func registerKBAdd(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_add", "Add text content to the agent's knowledge base. The content will be automatically chunked and indexed for future retrieval. Use when the user explicitly asks to save or remember something in the knowledge base.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"title": map[string]interface{}{
				"type":        "string",
				"description": "A descriptive title for this knowledge entry",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The text content to add to the knowledge base",
			},
		},
		"required": []string{"title", "content"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Content == "" {
			return "", fmt.Errorf("content is required")
		}
		title := args.Title
		if title == "" {
			title = "Untitled"
		}
		id, err := store.IngestText(ctx, agentID, title, args.Content, "text", "manual")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Added to knowledge base: source_id=%s (%d chars)", id, len(args.Content)), nil
	})
}

func registerKBIngestURL(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_ingest_url", "Fetch a URL and add its text content to the knowledge base. The page will be extracted, chunked, and indexed for future retrieval.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "The URL to fetch and ingest into the knowledge base",
			},
			"title": map[string]interface{}{
				"type":        "string",
				"description": "Optional title override (defaults to page title)",
			},
		},
		"required": []string{"url"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			URL   string `json:"url"`
			Title string `json:"title,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.URL == "" {
			return "", fmt.Errorf("url is required")
		}
		u, err := url.Parse(args.URL)
		if err != nil {
			return "", fmt.Errorf("invalid url: %w", err)
		}
		if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
			return "", fmt.Errorf("scheme %q not allowed; use http or https", u.Scheme)
		}
		title, content, err := FetchURLContent(ctx, args.URL)
		if err != nil {
			return "", fmt.Errorf("fetch url: %w", err)
		}
		if args.Title != "" {
			title = args.Title
		}
		id, err := store.IngestText(ctx, agentID, title, content, "url", args.URL)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Ingested URL into knowledge base: source_id=%s (%d chars, title=%q)", id, len(content), title), nil
	})
}

func registerKBList(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_list", "List all sources in the agent's knowledge base.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of sources to return (default 20)",
			},
		},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Limit int `json:"limit,omitempty"`
		}
		json.Unmarshal(rawArgs, &args)
		limit := args.Limit
		if limit <= 0 {
			limit = 20
		}
		sources, err := store.ListSources(ctx, agentID, limit, 0)
		if err != nil {
			return "", err
		}
		if len(sources) == 0 {
			return "Knowledge base is empty.", nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Knowledge base (%d sources):\n\n", len(sources)))
		for _, s := range sources {
			sb.WriteString(fmt.Sprintf("- %s (id: %s, type: %s, entries: %d, chars: %d)\n",
				s.Title, s.ID[:12], s.SourceType, s.EntryCount, s.TotalChars))
		}
		return sb.String(), nil
	})
}

func registerKBDelete(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_delete", "Delete a source and all its entries from the knowledge base.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"source_id": map[string]interface{}{
				"type":        "string",
				"description": "The ID of the source to delete",
			},
		},
		"required": []string{"source_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			SourceID string `json:"source_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.SourceID == "" {
			return "", fmt.Errorf("source_id is required")
		}
		if err := store.DeleteSource(ctx, agentID, args.SourceID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted source %s from knowledge base.", args.SourceID), nil
	})
}

// registerKBFlash adds knowledgebase_save_flash — a 灵感闪记 (inspiration
// flash): a short idea/insight/note worth recalling later, stored verbatim as
// one un-chunked source of type 'flash'. The description is written to make
// the agent capture proactively (harness visibility: the when-to-use lives in
// the tool description per tool-guidance-placement A, no prompt_modules entry).
func registerKBFlash(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_save_flash", "Save or update a short inspiration flash (灵感闪记). TWO modes: (1) NEW — omit source_id, record a brand-new idea the user EXPLICITLY asks to capture; (2) UPDATE — pass source_id to overwrite an existing flash with the FULL evolved text, used when the user iterates / refines / clarifies / adds to an idea they already recorded (so one idea stays as ONE complete iterated flash instead of fragmenting into many partial duplicates). Always write the complete current version of the idea, never just the delta. Use ONLY on explicit user intent (我有一个想法 / 帮我记一下 / 更新刚才那个想法 / 补充一下). Never proactive, and never capture content the user did not ask to record.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The full idea / insight / note text. When updating (source_id set), write the COMPLETE evolved version, not just the new part.",
			},
			"source_id": map[string]interface{}{
				"type":        "string",
				"description": "Optional. Pass the source_id of an existing flash to UPDATE it (when the user iterates / refines a previously recorded idea). Omit to create a new flash.",
			},
		},
		"required": []string{"content"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Content  string `json:"content"`
			SourceID string `json:"source_id,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Content == "" {
			return "", fmt.Errorf("content is required")
		}
		if args.SourceID == "" {
			id, err := store.SaveFlash(ctx, agentID, args.Content)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Saved inspiration flash (source_id=%s).", id), nil
		}
		if err := store.UpdateFlash(ctx, agentID, args.SourceID, args.Content); err != nil {
			if errors.Is(err, ErrFlashNotFound) {
				return fmt.Sprintf("更新灵感闪记失败：找不到 id=%s 的闪记（可能不属于本 agent 或不是闪记）。可改用不带 source_id 的方式新建，或先 knowledgebase_list 确认 id。", args.SourceID), nil
			}
			return "", err
		}
		return fmt.Sprintf("Updated inspiration flash (source_id=%s) — 已更新为完整迭代后的版本。", args.SourceID), nil
	})
}

// registerKBSaveTodo adds knowledgebase_save_todo — a task item stored as a
// type='todo' source with status + optional RFC3339 start/end times.
func registerKBSaveTodo(r *tools.Registry, store *KBStore, agentID string) {
	statusProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{
			"type":        "string",
			"enum":        []string{"pending", "in_progress", "done", "cancelled"},
			"description": desc,
		}
	}
	timeProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{
			"type":        "string",
			"format":      "date-time",
			"description": desc,
		}
	}
	r.Register("knowledgebase_save_todo", "Save a todo item to the knowledge base — something that needs doing. status defaults to pending; set it explicitly to record an item that is already in progress or done. Provide start_at and/or end_at as RFC 3339 timestamps (e.g. 2026-08-02T09:00:00Z) when timing is mentioned or implied. Use it ONLY when the user explicitly commits to an action item / follow-up / task (e.g. 帮我记个待办 / 提醒我去做X / 这是个待办 / 我得去...). Do NOT proactively turn casual mentions or vague intentions into todos.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": "What needs to be done",
			},
			"status":   statusProp("Lifecycle state (defaults to pending if omitted)"),
			"start_at": timeProp("Optional RFC 3339 start time"),
			"end_at":   timeProp("Optional RFC 3339 due/deadline time — the reminders sweep keys off this"),
		},
		"required": []string{"content"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Content string `json:"content"`
			Status  string `json:"status,omitempty"`
			StartAt string `json:"start_at,omitempty"`
			EndAt   string `json:"end_at,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		id, err := store.SaveTodo(ctx, agentID, args.Content, args.Status, args.StartAt, args.EndAt)
		if err != nil {
			return "", err
		}
		status := args.Status
		if status == "" {
			status = "pending"
		}
		return fmt.Sprintf("Saved todo (source_id=%s, status=%s).", id, status), nil
	})
}

// registerKBUpdateTodo adds knowledgebase_update_todo — mutate status/timing of
// an existing todo. Only non-empty fields are applied.
func registerKBUpdateTodo(r *tools.Registry, store *KBStore, agentID string) {
	statusProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{
			"type":        "string",
			"enum":        []string{"pending", "in_progress", "done", "cancelled"},
			"description": desc,
		}
	}
	timeProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{
			"type":        "string",
			"format":      "date-time",
			"description": desc,
		}
	}
	r.Register("knowledgebase_update_todo", "Update an existing todo's status and/or timing. Pass source_id (from knowledgebase_list_todos or knowledgebase_save_todo) plus any of status/start_at/end_at to change; omit a field to leave it unchanged. Use it as a todo moves forward (pending→in_progress→done, or cancelled) or when its timing changes.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"source_id": map[string]interface{}{
				"type":        "string",
				"description": "The source_id of the todo to update",
			},
			"status":   statusProp("New lifecycle state"),
			"start_at": timeProp("New RFC 3339 start time (omit to leave unchanged)"),
			"end_at":   timeProp("New RFC 3339 due/deadline time (omit to leave unchanged)"),
		},
		"required": []string{"source_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			SourceID string `json:"source_id"`
			Status   string `json:"status,omitempty"`
			StartAt  string `json:"start_at,omitempty"`
			EndAt    string `json:"end_at,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.SourceID == "" {
			return "", fmt.Errorf("source_id is required")
		}
		if err := store.UpdateTodo(ctx, agentID, args.SourceID, args.Status, args.StartAt, args.EndAt); err != nil {
			if errors.Is(err, ErrTodoNotFound) {
				return "", fmt.Errorf("todo %s not found (it may belong to another agent or not be a todo; check via knowledgebase_list_todos)", args.SourceID)
			}
			return "", err
		}
		return fmt.Sprintf("Updated todo %s.", args.SourceID), nil
	})
}

// registerKBListTodos adds knowledgebase_list_todos — the agent's view of the
// todo board, including the due-soon/overdue filter the reminders sweep uses.
func registerKBListTodos(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_list_todos", "List the user's todos. status: omit to list every status, 'active' for pending+in_progress (the working set), or a specific status. due_within_hours: when set, only todos whose end_at is set and at or before now+that many hours (due soon or overdue). Use it at the start of a conversation or when the user may have pending work, and proactively mention any item that is due, overdue, or relevant to the current topic.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"status": map[string]interface{}{
				"type":        "string",
				"description": "Filter: omit for all, 'active' for pending+in_progress, or one of pending/in_progress/done/cancelled",
			},
			"due_within_hours": map[string]interface{}{
				"type":        "integer",
				"description": "Only return todos due at or before now+this many hours (due soon / overdue). Omit to ignore due time.",
			},
		},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Status         string `json:"status,omitempty"`
			DueWithinHours int    `json:"due_within_hours,omitempty"`
		}
		json.Unmarshal(rawArgs, &args)
		todos, err := store.ListTodos(ctx, agentID, args.Status, args.DueWithinHours)
		if err != nil {
			return "", err
		}
		if len(todos) == 0 {
			label := "all"
			if args.Status != "" {
				label = args.Status
			}
			return fmt.Sprintf("No %s todos.", label), nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "%d todo(s)", len(todos))
		if args.Status != "" {
			fmt.Fprintf(&sb, " [%s]", args.Status)
		}
		sb.WriteString(":\n")
		for _, t := range todos {
			end := "no due date"
			if t.EndAt != nil {
				end = "due " + t.EndAt.UTC().Format("2006-01-02 15:04Z")
			}
			fmt.Fprintf(&sb, "- [%s] %s (id: %s, %s)\n", t.Status, t.Title, t.ID, end)
		}
		return sb.String(), nil
	})
}

// registerKBGenerateInsights adds knowledgebase_generate_insights — the
// deep-reading tool that runs an LLM pass over one article's full text and
// stores summary / quotes / actions / sprouts on it. Expensive (one LLM call
// over the whole article), so the description tells the agent to run it only
// on explicit request. Harness visibility: the when-to-use lives in the tool
// description (per tool-guidance-placement A).
func registerKBGenerateInsights(r *tools.Registry, store *KBStore, agentID string, invoker InsightInvoker, model string, maxTokens int) {
	r.Register("knowledgebase_generate_insights", "Generate a deep reading (深度解读) of one knowledge-base ARTICLE: a structured summary (core + thematic topic blocks + chapter outlines), curated verbatim quotes, action items, and knowledge-extension 'sprouts'. The result is stored ON the article by this tool itself and rendered in the article's detail page — you do NOT need any other tool to save it. Pass the source_id of an article (from knowledgebase_list / knowledgebase_search). EXPENSIVE (one LLM call over the full text), so run ONLY when the user explicitly asks to deeply analyze / unpack / '生根发芽' / '解读' a specific article; never proactively. If the call FAILS, report the error to the user verbatim and STOP — do NOT manually compose the reading yourself, and do NOT store anything via knowledgebase_add / knowledgebase_save_flash / other tools; this tool is the only correct path.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"source_id": map[string]interface{}{
				"type":        "string",
				"description": "The source_id of the article to deeply analyze (from knowledgebase_list / knowledgebase_search). Must be an article, not a flash/todo.",
			},
		},
		"required": []string{"source_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			SourceID string `json:"source_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.SourceID == "" {
			return "", fmt.Errorf("source_id is required")
		}
		ins, err := store.GenerateInsights(ctx, agentID, args.SourceID, invoker, model, maxTokens)
		if err != nil {
			// 业务失败（LLM 错误 / source 不存在 / 非文章）走"成功"路径：返回
			// 明确文案 + nil err。否则工具 registry 会在 tool error 上追加
			// "[Analyze the error above and try a different approach.]" 后缀
			// (registry.go 的工具结果包装)，诱导 agent 改走"自己手写解读 +
			// 用 knowledgebase_add / save_flash 存自创内容"的歧路 ——
			// s-1785659282356 里 agent 正是这么把自创解读塞进了知识库。这里
			// 强约束 agent 把失败转告用户并停手，不接管、不代写、不另存。
			return fmt.Sprintf("深度解读生成失败，原因：%v。\n请把这句失败原因如实转告用户，然后停止。不要自己手动编写解读内容，也不要改用 knowledgebase_add / knowledgebase_save_flash 等任何其他工具把自创解读存入知识库——深度解读只能由 knowledgebase_generate_insights 产出。是否重试由用户决定。", err), nil
		}
		core := ins.Summary.Core
		if len(core) > 200 {
			core = clipUTF8(core, 200) + "…"
		}
		return fmt.Sprintf("已生成深度解读并保存到文章 (source_id=%s)。核心总结：%s；金句 %d 条；待办 %d 条；发芽 %d 个。完整内容见文章详情页。",
			args.SourceID, core, len(ins.Quotes), len(ins.Actions), len(ins.Sprouts.Items)), nil
	})
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

var kbFetchClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: httpclient.Wrap(&http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
		ResponseHeaderTimeout: 20 * time.Second,
	}),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

func FetchURLContent(ctx context.Context, rawURL string) (title, body string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", httpclient.UserAgent())

	resp, err := kbFetchClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", "", err
	}

	html := string(data)

	// Extract title
	titleRe := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	if m := titleRe.FindStringSubmatch(html); len(m) > 1 {
		title = strings.TrimSpace(htmlTagRe.ReplaceAllString(m[1], ""))
	}
	if title == "" {
		title = rawURL
	}

	// Strip script/style, then tags
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")
	text := htmlTagRe.ReplaceAllString(html, " ")

	// Decode common entities
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&nbsp;", " ")

	// Collapse whitespace
	spaceRe := regexp.MustCompile(`[ \t]+`)
	text = spaceRe.ReplaceAllString(text, " ")
	nlRe := regexp.MustCompile(`\n{3,}`)
	text = nlRe.ReplaceAllString(text, "\n\n")
	body = strings.TrimSpace(text)

	return title, body, nil
}
