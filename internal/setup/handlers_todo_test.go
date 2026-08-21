package setup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/session"
)

// TestParseTodoMarkdown locks the checkbox grammar the chat UI depends on:
// [ ] pending, [x]/[X] done, and the newer [-]/[~] cancelled. A regression
// here either hides the progress panel or leaves cancelled steps pinned as
// pending forever.
func TestParseTodoMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []map[string]any
	}{
		{
			name: "pending and done",
			in:   "- [ ] alpha\n- [x] beta\n",
			want: []map[string]any{
				{"text": "alpha", "done": false},
				{"text": "beta", "done": true},
			},
		},
		{
			name: "cancelled markers",
			in:   "- [-] dropped\n- [~] also dropped\n- [ ] live\n",
			want: []map[string]any{
				{"text": "dropped", "done": false, "cancelled": true},
				{"text": "also dropped", "done": false, "cancelled": true},
				{"text": "live", "done": false},
			},
		},
		{
			name: "duplicate text merges, done and cancelled are sticky",
			in:   "- [ ] step\n- [x] step\n- [ ] other\n- [-] other\n",
			want: []map[string]any{
				{"text": "step", "done": true},
				{"text": "other", "done": false, "cancelled": true},
			},
		},
		{
			name: "asterisk bullets, non-checkbox lines ignored",
			in:   "# Plan\nsome prose\n* [x] built\n* not a checkbox\n",
			want: []map[string]any{
				{"text": "built", "done": true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTodoMarkdown(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				for k, wv := range w {
					gv, ok := got[i][k]
					if !ok {
						t.Errorf("item %d missing key %q; got %v", i, k, got[i])
						continue
					}
					if gv != wv {
						t.Errorf("item %d key %q = %v, want %v", i, k, gv, wv)
					}
				}
			}
		})
	}
}

// fakeTodoScope maps session keys to (chatID, projectID); keys absent from
// the map mimic a failed triple lookup (scope "" → skip).
type fakeTodoScope map[string][2]string

func (f fakeTodoScope) resolve(sessionKey string) (string, string) {
	pair, ok := f[sessionKey]
	if !ok {
		return "", ""
	}
	return pair[0], pair[1]
}

// TestPickLatestTodo covers the dashboard "latest active todo" selection:
// newest session with a non-empty checklist wins, unreadable/empty/failed
// scopes are skipped, and nothing anywhere yields ok=false.
func TestPickLatestTodo(t *testing.T) {
	newest := session.WebSession{ID: "sk_new", Title: "newest chat", Channel: "web", UpdatedAt: 300}
	mid := session.WebSession{ID: "sk_mid", Title: "mid chat", Channel: "qq", UpdatedAt: 200}
	oldest := session.WebSession{ID: "sk_old", Title: "old chat", Channel: "web", UpdatedAt: 100}

	scope := fakeTodoScope{
		"sk_new": {"chat_new", ""},
		"sk_mid": {"chat_mid", "proj1"},
		"sk_old": {"chat_old", ""},
	}
	read := func(relPath string) ([]byte, error) {
		switch relPath {
		case "sessions/chat_new/todo.md":
			return []byte("- [ ] fresh step\n"), nil // newest wins
		case "projects/proj1/chat_mid/todo.md":
			return []byte("- [x] done long ago\n"), nil
		case "sessions/chat_old/todo.md":
			return []byte("# plan\nno checkboxes\n"), nil // parses empty → skip
		}
		return nil, fmt.Errorf("no such file")
	}

	out, ok := pickLatestTodo([]session.WebSession{mid, oldest, newest}, scope.resolve, read)
	if !ok {
		t.Fatal("expected a hit")
	}
	if out["sessionId"] != "sk_new" {
		t.Fatalf("sessionId = %v, want sk_new (newest non-empty wins over unsorted input)", out["sessionId"])
	}
	if out["title"] != "newest chat" || out["channel"] != "web" || out["updatedAt"] != int64(300) {
		t.Fatalf("session metadata not carried: %v", out)
	}
	items, _ := out["items"].([]map[string]any)
	if len(items) != 1 || items[0]["text"] != "fresh step" {
		t.Fatalf("items = %v, want the parsed checklist", out["items"])
	}
	if out["raw"] != "- [ ] fresh step\n" {
		t.Fatalf("raw = %q, want the file body", out["raw"])
	}

	t.Run("newest sessions empty, falls through to older", func(t *testing.T) {
		out, ok := pickLatestTodo([]session.WebSession{newest, oldest}, scope.resolve, func(relPath string) ([]byte, error) {
			if relPath == "sessions/chat_old/todo.md" {
				return []byte("- [ ] legacy step\n"), nil
			}
			return nil, fmt.Errorf("missing")
		})
		if !ok || out["sessionId"] != "sk_old" {
			t.Fatalf("out=%v ok=%v, want sk_old", out, ok)
		}
	})

	t.Run("scope failure skips session", func(t *testing.T) {
		emptyScope := fakeTodoScope{}
		if _, ok := pickLatestTodo([]session.WebSession{newest}, emptyScope.resolve, read); ok {
			t.Fatal("unscoped session must be skipped")
		}
	})

	t.Run("nothing anywhere → ok=false", func(t *testing.T) {
		if _, ok := pickLatestTodo(nil, scope.resolve, read); ok {
			t.Fatal("empty session list must miss")
		}
		if _, ok := pickLatestTodo([]session.WebSession{oldest}, scope.resolve, read); ok {
			t.Fatal("empty-checklist-only account must miss")
		}
	})
}

// TestChatTodoLatestAgentNotFound locks the HTTP glue: unknown agent →
// 404 JSON, unauthenticated → 401 (newAuthTestServer has no userResolver
// wired, so every agent lookup 404s — exactly the branch under test).
func TestChatTodoLatestAgentNotFound(t *testing.T) {
	ctx := context.Background()
	s, resolver, _, regularUser := newAuthTestServer(t, ctx)
	handler := s.authMiddleware(s.handleChatTodoLatest)

	t.Run("unauthenticated request is rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodGet, "/api/chat/todo/latest?agentId=nope", nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("unknown agent is 404", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler(rr, authTestRequest(t, ctx, resolver, http.MethodGet, "/api/chat/todo/latest?agentId=nope", regularUser.ID))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
		}
	})
}
