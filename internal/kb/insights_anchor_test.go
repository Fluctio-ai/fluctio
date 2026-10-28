package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// TestGenerateInsightsDropsUnverifiedQuotes: the LLM returns one verbatim
// quote and one confabulation; only the verbatim quote may persist.
func TestGenerateInsightsDropsUnverifiedQuotes(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()
	ks := NewKBStore(db, "sqlite")

	content := "alpha beta gamma sentence one.\n\nsecond paragraph about oak trees and rivers."
	sid, err := ks.IngestText(ctx, "a1", "Anchored Article", content, "text", "manual")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	invoker := func(ctx context.Context, msgs []provider.Message) (string, error) {
		return `{
			"summary": {"core": "core", "topics": [], "chapters": []},
			"quotes": [
				{"text": "alpha beta gamma sentence one", "tag": "real"},
				{"text": "completely fabricated line that appears nowhere", "tag": "fake"},
				{"text": "", "tag": "empty"}
			],
			"actions": [],
			"sprouts": {}
		}`, nil
	}
	ins, err := ks.GenerateInsights(ctx, "a1", sid, invoker, "mock", 0)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(ins.Quotes) != 1 {
		t.Fatalf("quotes = %+v, want only the verbatim one", ins.Quotes)
	}
	if !ins.Quotes[0].Verified || ins.Quotes[0].Tag != "real" {
		t.Errorf("surviving quote wrong: %+v", ins.Quotes[0])
	}
	// Persisted view matches.
	stored, err := ks.GetInsights(ctx, "a1", sid)
	if err != nil {
		t.Fatalf("get insights: %v", err)
	}
	if len(stored.Quotes) != 1 || !strings.Contains(stored.Quotes[0].Text, "alpha beta") {
		t.Errorf("persisted quotes wrong: %+v", stored.Quotes)
	}
}
