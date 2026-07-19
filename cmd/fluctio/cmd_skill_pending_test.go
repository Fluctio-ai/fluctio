package main

// cmd_skill_pending_test.go — TDD coverage for the four skill pending CLI
// commands. The cobra wrappers depend on a live store + gateway, which is
// heavy for a unit test; instead we drive the run* cores directly with a
// temp agentHome. This still exercises skills.{List,Approve,Reject}Pending
// end-to-end (the same code the CLI calls), so any regression in the
// pending layer fails these tests too.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/skills"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestRunSkillApprove_MovesPendingLive is the RED→GREEN anchor: write a
// pending skill, run approve, verify the live SKILL.md exists with the
// pending body and the pending entry is gone.
func TestRunSkillApprove_MovesPendingLive(t *testing.T) {
	home := t.TempDir()
	body := []byte("---\nname: foo\ndescription: test\n---\nbody\n")
	if err := skills.WritePending(home, "foo", body, skills.PendingMeta{
		Source:      "test",
		Description: "demo",
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}

	var buf bytes.Buffer
	if err := runSkillApprove(home, "foo", &buf); err != nil {
		t.Fatalf("runSkillApprove: %v", err)
	}

	liveFile := filepath.Join(home, "skills", "foo", "SKILL.md")
	got, err := os.ReadFile(liveFile)
	if err != nil {
		t.Fatalf("live SKILL.md missing after approve: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("live content = %q, want %q", got, body)
	}
	// Pending entry is gone.
	entries, _ := skills.ListPending(home)
	if len(entries) != 0 {
		t.Fatalf("ListPending after approve = %d, want 0", len(entries))
	}
	// .pending.json must not follow into the live tree.
	if _, err := os.Stat(filepath.Join(home, "skills", "foo", ".pending.json")); !os.IsNotExist(err) {
		t.Fatalf(".pending.json leaked into live tree (err=%v)", err)
	}
	// Output mentions activation + the live path (the skill directory, not
	// the SKILL.md file — ApprovePending returns the renamed dir).
	liveDir := filepath.Join(home, "skills", "foo")
	out := buf.String()
	if !strings.Contains(out, "activated foo") {
		t.Fatalf("output = %q, want it to mention %q", out, "activated foo")
	}
	if !strings.Contains(out, liveDir) {
		t.Fatalf("output = %q, want live dir %q", out, liveDir)
	}
}

// TestRunSkillApprove_UnknownName errors clearly so the user sees what to
// fix rather than a silent success.
func TestRunSkillApprove_UnknownName(t *testing.T) {
	home := t.TempDir()
	var buf bytes.Buffer
	err := runSkillApprove(home, "missing", &buf)
	if err == nil {
		t.Fatal("runSkillApprove on missing skill: want error, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v, want it to mention the skill name", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("output on error should be empty, got %q", buf.String())
	}
}

// TestRunSkillReject_DropsEntry covers the discard path.
func TestRunSkillReject_DropsEntry(t *testing.T) {
	home := t.TempDir()
	if err := skills.WritePending(home, "bar", []byte("x"), skills.PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	var buf bytes.Buffer
	if err := runSkillReject(home, "bar", &buf); err != nil {
		t.Fatalf("runSkillReject: %v", err)
	}
	entries, _ := skills.ListPending(home)
	if len(entries) != 0 {
		t.Fatalf("ListPending after reject = %d, want 0", len(entries))
	}
	if !strings.Contains(buf.String(), "removed pending bar") {
		t.Fatalf("output = %q, want %q", buf.String(), "removed pending bar")
	}
}

// TestRunSkillPending_ListsAndFormats covers the listing + formatting path
// and the empty case message.
func TestRunSkillPending_ListsAndFormats(t *testing.T) {
	home := t.TempDir()

	// Empty case.
	var buf bytes.Buffer
	if err := runSkillPending(home, &buf); err != nil {
		t.Fatalf("runSkillPending empty: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "No pending skills.") {
		t.Fatalf("empty output = %q, want %q", got, "No pending skills.")
	}

	// With an entry.
	buf.Reset()
	if err := skills.WritePending(home, "foo", []byte("x"), skills.PendingMeta{
		Source:      "skill_manage",
		Description: "demo edit",
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	if err := runSkillPending(home, &buf); err != nil {
		t.Fatalf("runSkillPending: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "foo") {
		t.Fatalf("output missing skill name: %q", got)
	}
	if !strings.Contains(got, "skill_manage") {
		t.Fatalf("output missing source: %q", got)
	}
	if !strings.Contains(got, "demo edit") {
		t.Fatalf("output missing description: %q", got)
	}
}

// TestRunSkillDiff_PendingVsLive covers both panes. The output is
// intentionally minimal (no diff library) — this test pins the format so
// future changes remain intentional.
func TestRunSkillDiff_PendingVsLive(t *testing.T) {
	home := t.TempDir()

	// First: no live yet → only pending body shown.
	pendingBody := "---\nname: foo\n---\nNEW\n"
	if err := skills.WritePending(home, "foo", []byte(pendingBody), skills.PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	var buf bytes.Buffer
	if err := runSkillDiff(home, "foo", &buf); err != nil {
		t.Fatalf("runSkillDiff (no live): %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "+++ pending:") {
		t.Fatalf("no-live output missing +++ pending marker: %q", got)
	}
	if strings.Contains(got, "--- live:") {
		t.Fatalf("no-live output should not contain --- live marker: %q", got)
	}

	// Now create a live skill and re-diff.
	liveDir := filepath.Join(home, "skills", "foo")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "SKILL.md"), []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := runSkillDiff(home, "foo", &buf); err != nil {
		t.Fatalf("runSkillDiff (live+pending): %v", err)
	}
	got = buf.String()
	if !strings.Contains(got, "--- live:") || !strings.Contains(got, "OLD") {
		t.Fatalf("output missing live pane: %q", got)
	}
	if !strings.Contains(got, "+++ pending:") || !strings.Contains(got, "NEW") {
		t.Fatalf("output missing pending pane: %q", got)
	}

	// Missing pending → clear error.
	buf.Reset()
	err := runSkillDiff(home, "nope", &buf)
	if err == nil {
		t.Fatal("runSkillDiff on missing pending: want error, got nil")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want it to mention the skill name", err)
	}
}

// TestNotifyGatewayReloadHTTP_NoDaemon is the contract test for the
// cross-platform HTTP reload helper: with no daemon running (the test
// environment never has one), it must return false silently and not panic
// or print spurious errors. The skill-rename already happened before the
// helper is called, so "no daemon" is a normal, expected path — the user
// just doesn't get hot-reload this time.
func TestNotifyGatewayReloadHTTP_NoDaemon(t *testing.T) {
	// No need for a real store — the helper's first guard is the daemon
	// PID check, which fails immediately in tests. Passing a nil store
	// makes the test self-contained and asserts the guard fires before
	// any store access (which would otherwise panic on nil).
	rec := &store.AgentRecord{ID: "agt_test", UserID: "u_test"}
	if notifyGatewayReloadHTTP(context.Background(), nil, rec) {
		t.Fatal("expected false when no daemon is running; got true")
	}
}

// TestNotifyGatewayReloadHTTP_NilRecord guards against the helper being
// called with an unresolved agent record (defensive nil-check).
func TestNotifyGatewayReloadHTTP_NilRecord(t *testing.T) {
	if notifyGatewayReloadHTTP(context.Background(), nil, nil) {
		t.Fatal("expected false for nil record; got true")
	}
}
