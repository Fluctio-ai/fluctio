package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/kb"
)

// slashBookmark handles /bookmark — a code-only (no-LLM) quick-save of a web
// URL into the agent's bookmark table. Mirrors `fluctio bookmark add` so either
// entry point works; regex hooks can also point at the CLI. The page body is
// fetched at save time (go-readability) so the bookmark survives link rot.
// Title defaults to the page <title>; the user's trailing args become the
// summary.
//
//	/bookmark <url> [备注…]   save (fetches body)
//	/bookmark list            list recent bookmarks
//	/bookmark                 usage
//
// Not admin-gated: a bookmark is knowledge the chatter is contributing (like
// sending a message), not a mutation of the agent's runtime config.
func (a *Agent) slashBookmark(msg bus.InboundMessage, args []string) slashResult {
	if a.kbStore == nil {
		return slashResult{handled: true, reply: slashT(msg.Lang, "bookmark.no_store")}
	}
	if len(args) == 0 {
		return slashResult{handled: true, reply: slashT(msg.Lang, "bookmark.usage")}
	}
	if args[0] == "list" {
		return a.slashBookmarkList(msg)
	}
	rawURL := args[0]
	summary := ""
	if len(args) > 1 {
		summary = strings.TrimSpace(strings.Join(args[1:], " "))
	}
	content, fetchedTitle, fetchOK := fetchBookmarkBody(rawURL)
	normURL := kb.NormalizeBookmarkURL(rawURL)
	displayTitle := fetchedTitle
	if displayTitle == "" {
		displayTitle = normURL
	}
	id, err := a.kbStore.SaveBookmark(context.Background(), a.agentID, rawURL, fetchedTitle, summary, content, "slash")
	if err != nil {
		return slashResult{handled: true, reply: slashTf(msg.Lang, "bookmark.error", err)}
	}
	bodyNote := slashT(msg.Lang, "bookmark.body_skip")
	if fetchOK && content != "" {
		bodyNote = slashTf(msg.Lang, "bookmark.body_ok", len(content))
	}
	shortID := id
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return slashResult{
		handled: true,
		reply:   slashTf(msg.Lang, "bookmark.saved", normURL, displayTitle, bodyNote, shortID),
	}
}

func (a *Agent) slashBookmarkList(msg bus.InboundMessage) slashResult {
	bookmarks, err := a.kbStore.ListBookmarks(context.Background(), a.agentID, 20, 0)
	if err != nil {
		return slashResult{handled: true, reply: slashTf(msg.Lang, "bookmark.error", err)}
	}
	if len(bookmarks) == 0 {
		return slashResult{handled: true, reply: slashT(msg.Lang, "bookmark.list_empty")}
	}
	var sb strings.Builder
	sb.WriteString(slashTf(msg.Lang, "bookmark.list_header", len(bookmarks)))
	for _, b := range bookmarks {
		title := b.Title
		if title == "" {
			title = b.URL
		}
		id := b.ID
		if len(id) > 12 {
			id = id[:12]
		}
		fmt.Fprintf(&sb, "• %s\n  %s — id %s\n", title, b.URL, id)
	}
	return slashResult{handled: true, reply: sb.String()}
}

// fetchBookmarkBody wraps kb.FetchURLContent with a fail-safe return so the
// slash command can save the URL even when the page fetch fails (broken link,
// paywall, timeout). fetchOK=false means content is empty and the bookmark is
// saved with just the URL.
func fetchBookmarkBody(rawURL string) (content, title string, fetchOK bool) {
	t, body, err := kb.FetchURLContent(context.Background(), rawURL)
	if err != nil {
		return "", "", false
	}
	return body, t, true
}
