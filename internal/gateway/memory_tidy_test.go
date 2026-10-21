package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// tidyStubProvider fakes provider.Provider: Chat returns a canned body.
type tidyStubProvider struct{ resp string }

func (p *tidyStubProvider) Chat(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	return &provider.Response{Content: p.resp}, nil
}

func (p *tidyStubProvider) ChatStream(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.StreamReader, error) {
	return nil, nil
}

func openTidyStore(t *testing.T) *store.DBStore {
	t.Helper()
	dbs, err := store.NewDBStore("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := dbs.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { dbs.Close() })
	return dbs
}

func tidyFile(t *testing.T, dbs *store.DBStore, memory, resp string) string {
	t.Helper()
	ctx := context.Background()
	if err := dbs.SaveAgentFile(ctx, "agt_tidy", "MEMORY.md", []byte(memory)); err != nil {
		t.Fatalf("seed MEMORY.md: %v", err)
	}
	tidyMemoryWithProvider(ctx, dbs, "agt_tidy", &tidyStubProvider{resp: resp}, "stub", []byte(memory))
	got, err := dbs.GetAgentFile(ctx, "agt_tidy", "MEMORY.md")
	if err != nil {
		t.Fatalf("read MEMORY.md: %v", err)
	}
	return string(got)
}

// TestTidyAppliesReplaceAndRemove walks the happy path: a dated duplicate
// is merged via replace, a stale one-off entry is removed whole (no "- "
// husk), and both old texts land verbatim in MEMORY.archive.md.
func TestTidyAppliesReplaceAndRemove(t *testing.T) {
	dbs := openTidyStore(t)
	memory := `# Memory

## Preferences
- [2026-09-20] User uses Vim as their editor
- [2026-10-01] User uses Neovim with a Lua config
- [2026-08-15] Installed tree-sitter plugin for the old Vim setup

## Facts
- User lives in Shanghai
`
	resp := `{"ops":[
	{"op":"replace","target":"[2026-09-20] User uses Vim as their editor","new":"[2026-10-01] User uses Neovim with a Lua config"},
	{"op":"remove","target":"Installed tree-sitter plugin for the old Vim setup"}
]}`
	got := tidyFile(t, dbs, memory, resp)

	if n := strings.Count(got, "Neovim with a Lua config"); n != 2 {
		t.Fatalf("Neovim count=%d want 2 (kept + merged-in):\n%s", n, got)
	}
	if strings.Contains(got, "2026-09-20") || strings.Contains(got, "tree-sitter") {
		t.Fatalf("stale entries must be gone:\n%s", got)
	}
	if strings.Contains(got, "- \n") || strings.Contains(got, "-\n") {
		t.Fatalf("remove left a bullet husk:\n%s", got)
	}
	if !strings.Contains(got, "## Facts\n- User lives in Shanghai") {
		t.Fatalf("untouched entries must survive verbatim:\n%s", got)
	}

	arch, err := dbs.GetAgentFile(context.Background(), "agt_tidy", memoryArchiveFile)
	if err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	for _, want := range []string{
		"## Archived ",
		"- [2026-09-20] User uses Vim as their editor",
		"- Installed tree-sitter plugin for the old Vim setup",
	} {
		if !strings.Contains(string(arch), want) {
			t.Fatalf("archive missing %q:\n%s", want, arch)
		}
	}
}

// TestTidySkipsAmbiguousAndShortTargets: an ambiguous target (matches two
// entries), a too-short target, and an unknown op kind all get skipped —
// no ops apply, so the file and archive stay untouched.
func TestTidySkipsAmbiguousAndShortTargets(t *testing.T) {
	dbs := openTidyStore(t)
	memory := "- same fact repeated here\n- same fact repeated here\n- User lives in Shanghai\n"
	resp := `{"ops":[
	{"op":"remove","target":"same fact repeated here"},
	{"op":"remove","target":"Shanghai"},
	{"op":"append","target":"User lives in Shanghai and works on Go"}
]}`
	got := tidyFile(t, dbs, memory, resp)

	if got != memory {
		t.Fatalf("file must be unchanged:\ngot:  %q\nwant: %q", got, memory)
	}
	if _, err := dbs.GetAgentFile(context.Background(), "agt_tidy", memoryArchiveFile); err == nil {
		t.Fatalf("archive must not exist when no op applied")
	}
}

// TestTidyNoOpsIsNoop: an empty ops list is a normal success — no write.
func TestTidyNoOpsIsNoop(t *testing.T) {
	dbs := openTidyStore(t)
	memory := "- User lives in Shanghai\n- User works on Go projects\n"
	got := tidyFile(t, dbs, memory, `{"ops":[]}`)
	if got != memory {
		t.Fatalf("file must be unchanged:\ngot:  %q\nwant: %q", got, memory)
	}
}

// TestTidyCapsOps: more ops than maxTidyOpsPerRun only apply the first
// maxTidyOpsPerRun.
func TestTidyCapsOps(t *testing.T) {
	dbs := openTidyStore(t)
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot",
		"golf", "hotel", "india", "juliet", "kilo", "lima", "mike", "november", "oscar"}
	var sb strings.Builder
	var ops strings.Builder
	for _, w := range words {
		sb.WriteString("- stale entry about " + w + " project\n")
		ops.WriteString(`{"op":"remove","target":"stale entry about ` + w + ` project"},`)
	}
	memory := sb.String()
	resp := `{"ops":[` + strings.TrimSuffix(ops.String(), ",") + `]}`
	got := tidyFile(t, dbs, memory, resp)

	if n := strings.Count(got, "stale entry about"); n != 5 {
		t.Fatalf("remaining stale entries=%d want 5 (15 - cap 10):\n%s", n, got)
	}
	arch, err := dbs.GetAgentFile(context.Background(), "agt_tidy", memoryArchiveFile)
	if err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if n := strings.Count(string(arch), "- stale entry about"); n != 10 {
		t.Fatalf("archived entries=%d want 10", n)
	}
}

// TestTidyRefusesToEmptyFile: ops that would wipe the whole file are
// rejected — the write is skipped and nothing is archived.
func TestTidyRefusesToEmptyFile(t *testing.T) {
	dbs := openTidyStore(t)
	memory := "- the only entry this agent ever recorded\n"
	resp := `{"ops":[{"op":"remove","target":"the only entry this agent ever recorded"}]}`
	got := tidyFile(t, dbs, memory, resp)

	if got != memory {
		t.Fatalf("file must survive a wipe attempt:\ngot:  %q\nwant: %q", got, memory)
	}
	if _, err := dbs.GetAgentFile(context.Background(), "agt_tidy", memoryArchiveFile); err == nil {
		t.Fatalf("archive must not exist when the write was skipped")
	}
}
