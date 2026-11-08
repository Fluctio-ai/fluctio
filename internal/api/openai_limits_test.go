package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// The attachment count cap fires before auth resolution, so the handler
// can be exercised directly: >32 attachments → 400 with an explicit
// message, ≤32 passes the gate (and proceeds to fail auth, which is out
// of scope here).
func TestChatCompletionsAttachmentCap(t *testing.T) {
	s := &Server{}

	mk := func(n int) *httptest.ResponseRecorder {
		atts := make([]map[string]any, n)
		for i := range atts {
			atts[i] = map[string]any{"url": "https://example.com/f.png", "name": "f.png"}
		}
		body, _ := json.Marshal(map[string]any{
			"model":       "m",
			"messages":    []map[string]any{{"role": "user", "content": "hi"}},
			"attachments": atts,
		})
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		s.HandleChatCompletions(w, r)
		return w
	}

	w := mk(maxChatCompletionsAttachments + 1)
	if w.Code != 400 {
		t.Fatalf("over-cap: status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "too many attachments") {
		t.Fatalf("over-cap: body = %q", w.Body.String())
	}

	w = mk(maxChatCompletionsAttachments)
	if w.Code == 400 && strings.Contains(w.Body.String(), "too many attachments") {
		t.Fatalf("at-cap rejected: %d %s", w.Code, w.Body.String())
	}
}

// Bodies beyond the 64 MiB ceiling are refused (the JSON decode fails on
// the truncated stream — either way the caller gets 400, not OOM).
func TestChatCompletionsBodyLimit(t *testing.T) {
	s := &Server{}
	huge := strings.Repeat("a", int(maxChatCompletionsBody)+1024)
	body := `{"model":"m","messages":[{"role":"user","content":"` + huge + `"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	s.HandleChatCompletions(w, r)
	if w.Code != 400 {
		t.Fatalf("oversized body: status = %d, want 400", w.Code)
	}
}
