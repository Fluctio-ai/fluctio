package tts

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// The voice id goes into the request URL path verbatim and is
// LLM-controlled: crafted values ("../v1/…", "?x=", "#…") could rewrite
// the target endpoint while xi-api-key rides along. Invalid ids must be
// rejected BEFORE any network I/O.
func TestElevenLabsRejectsInvalidVoice(t *testing.T) {
	cases := []string{
		"../v1/voices",        // path traversal
		"21m00?output_format", // query injection
		"21m00#fragment",      // fragment truncation
		"voice with spaces",
	}
	for _, voice := range cases {
		p := &ElevenLabs{}
		_, err := p.Execute(context.Background(), toolproviders.Request{
			Args:   map[string]any{"text": "hello", "voice": voice},
			Config: toolproviders.ProviderConfig{APIKey: "k"},
		})
		if err == nil || !contains(err.Error(), "invalid voice id") {
			t.Fatalf("voice %q: expected invalid-voice error before any HTTP call, got %v", voice, err)
		}
	}
}

// The default voice and well-formed custom ids pass the allowlist (they
// proceed to the HTTP stage — failure there is fine for this test).
func TestElevenLabsAcceptsWellFormedVoice(t *testing.T) {
	p := &ElevenLabs{}
	_, err := p.Execute(context.Background(), toolproviders.Request{
		Args:   map[string]any{"text": "hello", "voice": "custom_voice-1"},
		Config: toolproviders.ProviderConfig{APIKey: "k"},
	})
	// Network failure is expected in tests; the voice gate must NOT be
	// the reason for the error.
	if err != nil && contains(err.Error(), "invalid voice id") {
		t.Fatalf("well-formed voice rejected: %v", err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
