package tts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// captureServer fakes /v1/audio/speech and records the last request body.
func captureServer(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF"))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestOpenAIVoicePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		args      map[string]any
		options   map[string]string
		wantVoice string
	}{
		{"option overrides default alloy", map[string]any{"text": "hi"}, map[string]string{"voice": "speaker-girl-01"}, "speaker-girl-01"},
		{"tool arg beats option", map[string]any{"text": "hi", "voice": "narrator"}, map[string]string{"voice": "speaker-girl-01"}, "narrator"},
		{"alloy when nothing set", map[string]any{"text": "hi"}, nil, "alloy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := captureServer(t)
			p := OpenAI{}
			_, err := p.Execute(context.Background(), toolproviders.Request{
				Args:   tc.args,
				Config: toolproviders.ProviderConfig{APIKey: "sk-test", Endpoint: srv.URL, Options: tc.options},
			})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if v, _ := (*got)["voice"].(string); v != tc.wantVoice {
				t.Errorf("voice = %q, want %q", v, tc.wantVoice)
			}
			if v, _ := (*got)["response_format"].(string); v != "wav" {
				t.Errorf("response_format = %q, want wav", v)
			}
		})
	}
}
