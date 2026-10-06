package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/workspace"
)

func newCardStore(t *testing.T) workspace.Store {
	t.Helper()
	return workspace.NewLocalFS(t.TempDir())
}

var cardNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestEnsureProjectCardCreatesTemplate(t *testing.T) {
	st := newCardStore(t)
	content, err := ensureProjectCardEntry(context.Background(), st, "agt", "p1", "s-1", "帮我做课程表", cardNow)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for _, want := range []string{"## 关于", "## 决策与结论", projectCardIndexHeader, projectCardMarker("s-1"), "[2026-10-06] 帮我做课程表 · 进行中"} {
		if !strings.Contains(content, want) {
			t.Errorf("card missing %q:\n%s", want, content)
		}
	}
	// The registered file must be at the project-shared layer
	// ((projectID, sessionID="")), not a per-session copy.
	if rc, err := st.Get(context.Background(), "agt", "p1", "", projectCardFilename); err != nil {
		t.Fatalf("shared-layer get: %v", err)
	} else {
		rc.Close()
	}
	if _, err := st.Get(context.Background(), "agt", "p1", "s-1", projectCardFilename); err == nil {
		t.Error("card must not land in a per-session dir")
	}
}

func TestEnsureProjectCardIdempotent(t *testing.T) {
	st := newCardStore(t)
	ctx := context.Background()
	first, err := ensureProjectCardEntry(ctx, st, "agt", "p1", "s-1", "first", cardNow)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := ensureProjectCardEntry(ctx, st, "agt", "p1", "s-1", "first", cardNow)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Errorf("second call rewrote the card:\n--first--\n%s\n--second--\n%s", first, second)
	}
}

func TestEnsureProjectCardTwoSessions(t *testing.T) {
	st := newCardStore(t)
	ctx := context.Background()
	if _, err := ensureProjectCardEntry(ctx, st, "agt", "p1", "s-1", "会话一", cardNow); err != nil {
		t.Fatalf("s-1: %v", err)
	}
	content, err := ensureProjectCardEntry(ctx, st, "agt", "p1", "s-2", "会话二", cardNow)
	if err != nil {
		t.Fatalf("s-2: %v", err)
	}
	if strings.Count(content, "· 进行中") != 2 {
		t.Errorf("want 2 index rows, card:\n%s", content)
	}
	if !strings.Contains(content, "s-2 · 进行中 <!-- sid:s-2 -->") && !strings.Contains(content, projectCardMarker("s-2")) {
		t.Errorf("s-2 row missing marker:\n%s", content)
	}
}

func TestEnsureProjectCardIsolatedPerProject(t *testing.T) {
	st := newCardStore(t)
	ctx := context.Background()
	if _, err := ensureProjectCardEntry(ctx, st, "agt", "p1", "s-1", "x", cardNow); err != nil {
		t.Fatalf("p1: %v", err)
	}
	if _, err := st.Get(ctx, "agt", "p2", "", projectCardFilename); err == nil {
		t.Error("p2 must not see p1's card — store tuple isolation")
	}
}

func TestAppendProjectCardRowHeaderMissing(t *testing.T) {
	content := appendProjectCardRow("# 改过的项目卡\n\n## 关于\n\n内容。\n", "- [2026-10-06] x · 进行中 <!-- sid:s-9 -->")
	if !strings.HasSuffix(strings.TrimSpace(content), "- [2026-10-06] x · 进行中 <!-- sid:s-9 -->") {
		t.Errorf("row should append at EOF under a fresh section:\n%s", content)
	}
	if strings.Count(content, projectCardIndexHeader) != 1 {
		t.Errorf("fresh index header should appear exactly once:\n%s", content)
	}
}

func TestAppendProjectCardRowBeforeNextSection(t *testing.T) {
	card := "# 卡\n\n## 会话索引\n\n- [2026-10-01] 旧 · 进行中 <!-- sid:s-0 -->\n\n## 归档\n\n旧东西。\n"
	got := appendProjectCardRow(card, "- [2026-10-06] 新 · 进行中 <!-- sid:s-1 -->")
	idx := strings.Index(got, projectCardIndexHeader)
	arch := strings.Index(got, "## 归档")
	rowIdx := strings.Index(got, "<!-- sid:s-1 -->")
	if idx == -1 || arch == -1 || rowIdx == -1 || !(idx < rowIdx && rowIdx < arch) {
		t.Errorf("row must land inside the index section (header < row < next section):\n%s", got)
	}
}

func TestBuildProjectCardSnapshotTruncates(t *testing.T) {
	var pre []string
	for range 40 {
		pre = append(pre, "决策行 "+strings.Repeat("x", 3))
	}
	var rows []string
	for i := range 20 {
		rows = append(rows, "- [2026-10-06] 会话"+string(rune('A'+i))+" · 进行中 <!-- sid:s-"+string(rune('0'+i))+" -->")
	}
	card := strings.Join(pre, "\n") + "\n" + projectCardIndexHeader + "\n\n" + strings.Join(rows, "\n") + "\n"
	snap := buildProjectCardSnapshot(card)
	lines := strings.Split(snap, "\n")
	// head budget + truncation note + header + note + newest rows
	if len(lines) > projectCardMaxHeadLines+1+1+1+projectCardMaxIndexRows+2 {
		t.Errorf("snapshot too long: %d lines", len(lines))
	}
	if !strings.Contains(snap, "关于/决策区过长") {
		t.Error("head truncation note missing")
	}
	// newest 15 of A..T are F..T; the oldest five must be gone
	if strings.Contains(snap, "会话A ·") || strings.Contains(snap, "会话E ·") {
		t.Error("oldest index rows should be dropped")
	}
	if !strings.Contains(snap, "会话F · 进行中") || !strings.Contains(snap, "会话T · 进行中") {
		t.Error("newest index rows should be kept")
	}
}

func TestBuildProjectCardSnapshotNoHeader(t *testing.T) {
	card := strings.Repeat("自由文本行\n", 60)
	snap := buildProjectCardSnapshot(card)
	if n := len(strings.Split(snap, "\n")); n > projectCardMaxHeadLines+1 {
		t.Errorf("headerless snapshot too long: %d lines", n)
	}
	if !strings.Contains(snap, "已截断") {
		t.Error("truncation note missing")
	}
}

func TestProjectCardPreview(t *testing.T) {
	if got := projectCardPreview(""); got != "新会话" {
		t.Errorf("empty preview = %q", got)
	}
	if got := projectCardPreview("  多个\n空白   行 "); got != "多个 空白 行" {
		t.Errorf("whitespace join = %q", got)
	}
	long := strings.Repeat("字", 50)
	if got := projectCardPreview(long); len([]rune(got)) != 41 { // 40 + ellipsis
		t.Errorf("long preview rune count = %d", len([]rune(got)))
	}
}
