package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
)

// tools_bookmarks.go exposes the rest of the bookmark (收藏链接) surface to
// the harness. Save lives in tools.go (registerKBBookmark); these five tools
// cover list/read/update/delete/promote so the agent can work over saved
// bookmarks (查书签 / 读正文 / 改备注 / 删 / 升级为文章) instead of only adding
// them. Vector recall already covers discovery loosely (searchBookmarksByVector
// feeds knowledgebase_search), but it needs embeddings + may miss — the
// explicit listing is the reliable discovery path. Harness visibility: the
// when-to-use lives in the tool descriptions (per tool-guidance-placement A).
func registerKBBookmarkTools(r *tools.Registry, store *KBStore, agentID string) {
	registerKBListBookmarks(r, store, agentID)
	registerKBReadBookmark(r, store, agentID)
	registerKBUpdateBookmark(r, store, agentID)
	registerKBDeleteBookmark(r, store, agentID)
	registerKBPromoteBookmark(r, store, agentID)
}

// registerKBListBookmarks adds knowledgebase_list_bookmarks — the agent's view
// of the user's saved bookmarks (收藏链接): full bookmark_id + title + URL +
// summary, newest first. The fetched page BODY is deliberately omitted (it can
// be huge); knowledgebase_read_bookmark pulls one body on demand.
func registerKBListBookmarks(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_list_bookmarks", "List the user's saved bookmarks (收藏链接) — newest first, each with its full bookmark_id, title, URL, and summary note. The saved page body is NOT included here (use knowledgebase_read_bookmark on one bookmark_id). Use when the user asks what they have bookmarked (我收藏了哪些链接 / 看看书签), to check a URL was already saved before saving it again, or to find the bookmark_id before reading/updating/promoting one.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of bookmarks to return (default 20)",
			},
		},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Limit int `json:"limit,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}
		// Metadata-only query — the body is projected out in SQL (see
		// ListBookmarkMetas); this tool never renders it.
		bookmarks, err := store.ListBookmarkMetas(ctx, agentID, limit, 0)
		if err != nil {
			return "", err
		}
		if len(bookmarks) == 0 {
			return "No bookmarks yet.", nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "%d bookmark(s):\n", len(bookmarks))
		for _, b := range bookmarks {
			title := b.Title
			if title == "" {
				title = b.URL
			}
			fmt.Fprintf(&sb, "- %s\n  url: %s\n  bookmark_id: %s", title, b.URL, b.ID)
			if s := strings.TrimSpace(b.Summary); s != "" {
				if utf8.RuneCountInString(s) > 80 {
					s = string([]rune(s)[:80]) + "…"
				}
				fmt.Fprintf(&sb, "\n  note: %s", s)
			}
			if b.PromotedTo != "" {
				fmt.Fprintf(&sb, "\n  (promoted to article %s)", b.PromotedTo)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\nTo read one body: knowledgebase_read_bookmark. To promote one into a full article: knowledgebase_promote_bookmark.")
		return sb.String(), nil
	})
}

// registerKBReadBookmark adds knowledgebase_read_bookmark — one bookmark's
// full saved page body by bookmark_id. This is the link-rot-proof copy fetched
// at save time; no live re-fetch happens.
func registerKBReadBookmark(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_read_bookmark", "Read one bookmark's (收藏链接) full saved page body by bookmark_id (from knowledgebase_list_bookmarks). The body was fetched at save time so it survives link rot — reading does NOT hit the network. Returns the metadata header plus the stored text; when the save-time fetch failed the body is empty (the listing already shows which, via a missing body). Use when the user wants the content of a bookmarked page now (看看之前存的这篇文章 / 那个链接讲了什么).", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bookmark_id": map[string]interface{}{
				"type":        "string",
				"description": "The bookmark_id of the bookmark to read (from knowledgebase_list_bookmarks)",
			},
		},
		"required": []string{"bookmark_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			BookmarkID string `json:"bookmark_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.BookmarkID == "" {
			return "", fmt.Errorf("bookmark_id is required")
		}
		b, err := store.GetBookmark(ctx, agentID, args.BookmarkID)
		if errors.Is(err, ErrBookmarkNotFound) {
			return fmt.Sprintf("找不到 bookmark_id=%s 的书签（可能不属于本 agent）。请先 knowledgebase_list_bookmarks 确认。", args.BookmarkID), nil
		}
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "title: %s\nurl: %s\nsaved: %s\n", b.Title, b.URL, b.CreatedAt.Format("2006-01-02 15:04"))
		if s := strings.TrimSpace(b.Summary); s != "" {
			sb.WriteString("note: " + s + "\n")
		}
		sb.WriteString("\n")
		body := strings.TrimSpace(b.Content)
		if body == "" {
			body = "(The page body was never fetched — the save-time fetch failed, e.g. paywall or timeout. Only the URL and this note exist. Ask the user whether to re-save, or fetch the live page another way.)"
		}
		sb.WriteString(body)
		return sb.String(), nil
	})
}

// registerKBUpdateBookmark adds knowledgebase_update_bookmark — overwrite a
// bookmark's title and/or summary note. URL and saved body are immutable
// (UpdateBookmark); re-fetching is a re-save, not an update.
func registerKBUpdateBookmark(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_update_bookmark", "Update one bookmark's (收藏链接) editable metadata — title and/or the summary note — by bookmark_id. The URL and the saved page body are immutable. Use when the user renames a bookmark or refines why they saved it (给这个书签改个名 / 补一句备注). Empty fields are left unchanged.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bookmark_id": map[string]interface{}{
				"type":        "string",
				"description": "The bookmark to update (bookmark_id from knowledgebase_list_bookmarks)",
			},
			"title": map[string]interface{}{
				"type":        "string",
				"description": "New title (omit to keep the current one)",
			},
			"summary": map[string]interface{}{
				"type":        "string",
				"description": "New summary note (omit to keep the current one)",
			},
		},
		"required": []string{"bookmark_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			BookmarkID string `json:"bookmark_id"`
			Title      string `json:"title,omitempty"`
			Summary    string `json:"summary,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.BookmarkID == "" {
			return "", fmt.Errorf("bookmark_id is required")
		}
		if args.Title == "" && args.Summary == "" {
			return "", fmt.Errorf("nothing to update: pass title and/or summary")
		}
		if err := store.UpdateBookmark(ctx, agentID, args.BookmarkID, args.Title, args.Summary); err != nil {
			if errors.Is(err, ErrBookmarkNotFound) {
				return fmt.Sprintf("找不到 bookmark_id=%s 的书签（可能不属于本 agent）。请先 knowledgebase_list_bookmarks 确认。", args.BookmarkID), nil
			}
			return "", err
		}
		return fmt.Sprintf("Updated bookmark %s.", args.BookmarkID), nil
	})
}

// registerKBDeleteBookmark adds knowledgebase_delete_bookmark — remove one
// bookmark (and its embedding) by bookmark_id. Destructive, so the
// description gates it on explicit user intent, mirroring the save tool's
// "only on explicit intent" stance.
func registerKBDeleteBookmark(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_delete_bookmark", "Delete one bookmark (收藏链接) by bookmark_id — removes the saved body and its embedding. DESTRUCTIVE and irreversible: use ONLY when the user explicitly asks to remove a bookmark (删掉这个书签 / 不要这个链接了), never as cleanup. Confirm which bookmark the user means first (knowledgebase_list_bookmarks) when the intent is ambiguous.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bookmark_id": map[string]interface{}{
				"type":        "string",
				"description": "The bookmark to delete (bookmark_id from knowledgebase_list_bookmarks)",
			},
		},
		"required": []string{"bookmark_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			BookmarkID string `json:"bookmark_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.BookmarkID == "" {
			return "", fmt.Errorf("bookmark_id is required")
		}
		if err := store.DeleteBookmark(ctx, agentID, args.BookmarkID); err != nil {
			if errors.Is(err, ErrBookmarkNotFound) {
				return fmt.Sprintf("找不到 bookmark_id=%s 的书签（可能不属于本 agent）。请先 knowledgebase_list_bookmarks 确认。", args.BookmarkID), nil
			}
			return "", err
		}
		return fmt.Sprintf("Deleted bookmark %s.", args.BookmarkID), nil
	})
}

// registerKBPromoteBookmark adds knowledgebase_promote_bookmark — turn a
// read-later bookmark into a full KB article so it enters the wiki-generation
// pipeline. Mirrors the web UI's 升级为文章 button; idempotent.
func registerKBPromoteBookmark(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_promote_bookmark", "Promote one bookmark (收藏链接) into a full knowledge-base ARTICLE by bookmark_id — the saved page body becomes an article (chunked, embedded, and eligible for wiki generation), turning a read-later link into retrievable knowledge. Idempotent: an already-promoted bookmark returns its existing article id. The saved body is used verbatim; if empty the URL is re-fetched (fails when the page is gone). Use when the user decides a saved link is worth keeping as knowledge (把之前收藏的那篇升级成文章 / 这个链接值得进知识库). Routing: just saving a link for later → knowledgebase_save_bookmark; making it searchable knowledge → this tool.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bookmark_id": map[string]interface{}{
				"type":        "string",
				"description": "The bookmark to promote (bookmark_id from knowledgebase_list_bookmarks)",
			},
		},
		"required": []string{"bookmark_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			BookmarkID string `json:"bookmark_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.BookmarkID == "" {
			return "", fmt.Errorf("bookmark_id is required")
		}
		articleID, err := store.PromoteBookmarkToArticle(ctx, agentID, args.BookmarkID)
		if errors.Is(err, ErrBookmarkNotFound) {
			return fmt.Sprintf("找不到 bookmark_id=%s 的书签（可能不属于本 agent）。请先 knowledgebase_list_bookmarks 确认。", args.BookmarkID), nil
		}
		if err != nil {
			return fmt.Sprintf("升级失败（bookmark_id=%s）：%v", args.BookmarkID, err), nil
		}
		return fmt.Sprintf("Promoted bookmark %s into article (source_id=%s). It is now chunked/embedded and searchable via knowledgebase_search; wiki generation will pick it up on the next run.", args.BookmarkID, articleID), nil
	})
}
