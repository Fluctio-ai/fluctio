package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
)

func bookmarkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bookmark",
		Short: "Manage saved web bookmarks (quick-save a URL without an LLM)",
	}
	cmd.AddCommand(bookmarkAddCmd())
	cmd.AddCommand(bookmarkListCmd())
	cmd.AddCommand(bookmarkDeleteCmd())
	return cmd
}

// openKBStore opens the shared DB (running migrations via AutoMigrate) and
// returns a KBStore plus the underlying store for agent lookup. The caller
// closes st. This path must work standalone — no running gateway required —
// so regex hooks can point their `cli` at `fluctio bookmark add`.
func openKBStore() (*kb.KBStore, store.Store, error) {
	st, err := openStoreFromEnv()
	if err != nil {
		return nil, nil, err
	}
	dbs, ok := st.(*store.DBStore)
	if !ok {
		st.Close()
		return nil, nil, fmt.Errorf("store backend does not expose a *DBStore (cannot open KB store)")
	}
	return kb.NewKBStore(dbs.DB(), dbs.Dialect()), st, nil
}

// resolveAgentID picks the agent a bookmark is scoped to. --agent wins;
// otherwise the single agent in the DB is auto-selected; with zero or
// multiple agents the user must pass --agent.
func resolveAgentID(st store.Store, agentFlag string) (string, error) {
	if agentFlag != "" {
		return agentFlag, nil
	}
	all, err := st.ListAllAgents(context.Background())
	if err != nil {
		return "", err
	}
	if len(all) == 1 {
		return all[0].ID, nil
	}
	if len(all) == 0 {
		return "", fmt.Errorf("no agents found; create one first")
	}
	names := make([]string, 0, len(all))
	for _, a := range all {
		names = append(names, a.ID+" ("+a.Name+")")
	}
	return "", fmt.Errorf("multiple agents found, pass --agent to choose: %s", strings.Join(names, ", "))
}

func bookmarkAddCmd() *cobra.Command {
	var title, summary, agent string
	var noFetch bool
	cmd := &cobra.Command{
		Use:   "add <url>",
		Short: "Save a URL as a bookmark (fetches the page body so it survives link rot)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kbs, st, err := openKBStore()
			if err != nil {
				return err
			}
			defer st.Close()
			agentID, err := resolveAgentID(st, agent)
			if err != nil {
				return err
			}
			rawURL := args[0]
			content := ""
			fetchedTitle := ""
			if !noFetch {
				t, body, ferr := kb.FetchURLContent(context.Background(), rawURL)
				if ferr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not fetch page body (%v); saved URL only\n", ferr)
				} else {
					content = body
					fetchedTitle = t
				}
			}
			if title == "" {
				title = fetchedTitle
			}
			id, err := kbs.SaveBookmark(context.Background(), agentID, rawURL, title, summary, content, "cli")
			if err != nil {
				return err
			}
			fmt.Printf("saved bookmark id=%s\n", id)
			fmt.Printf("  url: %s\n", kb.NormalizeBookmarkURL(rawURL))
			if title != "" {
				fmt.Printf("  title: %s\n", title)
			}
			if summary != "" {
				fmt.Printf("  summary: %s\n", summary)
			}
			if content != "" {
				fmt.Printf("  body: %d chars fetched\n", len(content))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "bookmark title (defaults to the page <title>)")
	cmd.Flags().StringVar(&summary, "summary", "", "short note / summary")
	cmd.Flags().StringVar(&agent, "agent", "", "agent ID to scope the bookmark to (defaults to the single agent in the DB)")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "skip fetching the page body")
	return cmd
}

func bookmarkListCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List saved bookmarks",
		RunE: func(cmd *cobra.Command, args []string) error {
			kbs, st, err := openKBStore()
			if err != nil {
				return err
			}
			defer st.Close()
			agentID, err := resolveAgentID(st, agent)
			if err != nil {
				return err
			}
			bookmarks, err := kbs.ListBookmarks(context.Background(), agentID, 200, 0)
			if err != nil {
				return err
			}
			if len(bookmarks) == 0 {
				fmt.Println("no bookmarks")
				return nil
			}
			fmt.Printf("%d bookmark(s):\n", len(bookmarks))
			for _, b := range bookmarks {
				title := b.Title
				if title == "" {
					title = "(untitled)"
				}
				id := b.ID
				if len(id) > 12 {
					id = id[:12]
				}
				body := ""
				if b.Content != "" {
					body = fmt.Sprintf(", %d chars saved", len(b.Content))
				}
				fmt.Printf("- %s\n    id: %s\n    url: %s%s\n", title, id, b.URL, body)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "agent ID (defaults to the single agent in the DB)")
	return cmd
}

func bookmarkDeleteCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a bookmark by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kbs, st, err := openKBStore()
			if err != nil {
				return err
			}
			defer st.Close()
			agentID, err := resolveAgentID(st, agent)
			if err != nil {
				return err
			}
			if err := kbs.DeleteBookmark(context.Background(), agentID, args[0]); err != nil {
				return err
			}
			fmt.Printf("deleted bookmark %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "agent ID (defaults to the single agent in the DB)")
	return cmd
}
