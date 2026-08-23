package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

func writeSessionFile(t *testing.T, path string, msgs []provider.Message) {
	t.Helper()
	var b strings.Builder
	for _, m := range msgs {
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
}

func countFileLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	n := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		n++
	}
	return n
}

// TestHealInterruptedTurnPadsOrphansAndMarksBoundary simulates the crash
// failure mode this repair targets: the daemon died mid-turn, so the
// persisted history ends with an assistant tool_calls batch where only the
// first call got its result. A fresh Manager (restart) must pad the
// dangling call and append the turn-aborted marker — both in memory and on
// disk — so the next LLM request carries a valid assistant/tool sequence.
func TestHealInterruptedTurnPadsOrphansAndMarksBoundary(t *testing.T) {
	dir := t.TempDir()
	key := "chat-crash-1"
	writeSessionFile(t, filepath.Join(dir, key+".jsonl"), []provider.Message{
		{Role: "user", Content: "run the build"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{
			{ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: "read_file", Arguments: "{}"}},
			{ID: "call_2", Type: "function", Function: provider.FunctionCall{Name: "exec", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "call_1", Name: "read_file", Content: "ok"},
	})

	// web channel: session_key == chatID, so the fresh Manager resolves
	// the same file — exactly the daemon-restart scenario.
	s := NewManager(dir).Get("web", "", key, "")
	got := s.GetMessages()

	if len(got) != 5 {
		t.Fatalf("want 5 messages after heal (3 original + 1 pad + 1 marker), got %d", len(got))
	}
	pad := got[3]
	if pad.Role != "tool" || pad.ToolCallID != "call_2" || pad.Content != toolResultCrashNote {
		t.Errorf("pad wrong: role=%q toolCallID=%q content=%q", pad.Role, pad.ToolCallID, pad.Content)
	}
	marker := got[4]
	if marker.Role != "user" || marker.Metadata["turnAborted"] != true {
		t.Errorf("marker wrong: role=%q metadata=%v", marker.Role, marker.Metadata)
	}
	if !strings.Contains(marker.Content, "中断") {
		t.Errorf("marker content should explain the interruption: %q", marker.Content)
	}

	// Repair must persist: the file on disk now holds the healed history.
	if n := countFileLines(t, filepath.Join(dir, key+".jsonl")); n != 5 {
		t.Errorf("want 5 lines persisted, got %d", n)
	}

	// Idempotent: a second restart reloads the already-healed history and
	// must not pad or mark again.
	got2 := NewManager(dir).Get("web", "", key, "").GetMessages()
	if len(got2) != 5 {
		t.Errorf("second restart re-healed: want 5 messages, got %d", len(got2))
	}
}

// TestHealInterruptedTurnNoOpOnCleanHistory: histories that end on a
// answered turn (or a plain text reply) must pass through untouched.
func TestHealInterruptedTurnNoOpOnCleanHistory(t *testing.T) {
	dir := t.TempDir()
	key := "chat-clean-1"
	writeSessionFile(t, filepath.Join(dir, key+".jsonl"), []provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	})

	s := NewManager(dir).Get("web", "", key, "")
	got := s.GetMessages()
	if len(got) != 2 {
		t.Fatalf("clean history modified: want 2 messages, got %d", len(got))
	}
	if n := countFileLines(t, filepath.Join(dir, key+".jsonl")); n != 2 {
		t.Errorf("file rewritten on no-op: want 2 lines, got %d", n)
	}
}

// TestPadOrphanToolResultsStopNote covers the raw pad primitive the
// turn-exit path used before markers existed: pads only, no marker.
func TestPadOrphanToolResultsStopNote(t *testing.T) {
	dir := t.TempDir()
	s := NewManager(dir).Get("web", "", "chat-stop-1", "")
	s.Append(provider.Message{Role: "user", Content: "go"})
	s.Append(provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{
		{ID: "call_9", Type: "function", Function: provider.FunctionCall{Name: "web_fetch", Arguments: "{}"}},
	}})

	if !s.PadOrphanToolResults(ToolResultStoppedNote) {
		t.Fatal("expected pad to fire for the dangling call")
	}
	got := s.GetMessages()
	if len(got) != 3 {
		t.Fatalf("want 3 messages, got %d", len(got))
	}
	if got[2].Content != ToolResultStoppedNote || got[2].ToolCallID != "call_9" {
		t.Errorf("pad wrong: %+v", got[2])
	}
	// Second run over the same history is a no-op.
	if s.PadOrphanToolResults(ToolResultStoppedNote) {
		t.Error("second pad should be a no-op")
	}
}

// TestPadOrphanToolResultsAndMarkAborted covers the turn-exit defer
// variant: pads the dangling call AND appends the stop-flavored
// turn-aborted marker (user role, metadata) so the model sees an explicit
// boundary; a clean history gets neither.
func TestPadOrphanToolResultsAndMarkAborted(t *testing.T) {
	dir := t.TempDir()
	s := NewManager(dir).Get("web", "", "chat-stop-2", "")
	s.Append(provider.Message{Role: "user", Content: "go"})
	s.Append(provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{
		{ID: "call_7", Type: "function", Function: provider.FunctionCall{Name: "exec", Arguments: "{}"}},
	}})

	s.PadOrphanToolResultsAndMarkAborted(ToolResultStoppedNote)
	got := s.GetMessages()
	if len(got) != 4 {
		t.Fatalf("want 4 messages (pad + marker), got %d", len(got))
	}
	if got[2].Content != ToolResultStoppedNote || got[2].ToolCallID != "call_7" {
		t.Errorf("pad wrong: %+v", got[2])
	}
	marker := got[3]
	if marker.Role != "user" || marker.Metadata["turnAborted"] != true {
		t.Errorf("marker wrong: role=%q metadata=%v", marker.Role, marker.Metadata)
	}
	if marker.Content != turnAbortedStopNote {
		t.Errorf("marker should use the stop-flavored note: %q", marker.Content)
	}

	// Idempotent: nothing left dangling, so a second call changes nothing.
	s.PadOrphanToolResultsAndMarkAborted(ToolResultStoppedNote)
	if got := s.GetMessages(); len(got) != 4 {
		t.Errorf("second call appended again: want 4 messages, got %d", len(got))
	}
}
