package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
)

func TestAnnotateReachability(t *testing.T) {
	root := t.TempDir()
	reg := tools.NewRegistry(root, root)
	reg.SetUserRoot(root)
	// 注册一个 dummy writes_file 工具
	reg.RegisterWithEffect("write_file", "d", map[string]interface{}{"type": "object"},
		func(ctx context.Context, args json.RawMessage) (string, error) { return "", nil },
		tools.SideWritesFile)

	// 产物在可见域外（绝对路径）→ 追加裁决
	got := annotateReachability("write_file",
		"Written 1234 bytes to D:/outside/x.png", reg)
	if !strings.Contains(got, "不在用户可见域") || !strings.Contains(got, "deliver_file") {
		t.Fatalf("expected reachability verdict appended, got:\n%s", got)
	}

	// 产物在可见域内（相对路径）→ 不追加
	got2 := annotateReachability("write_file",
		"Written 1234 bytes to notes/x.txt", reg)
	if strings.Contains(got2, "deliver_file") {
		t.Fatalf("visible artifact should not trigger verdict, got:\n%s", got2)
	}

	// 非 writes_file 工具 → 不处理
	got3 := annotateReachability("read_file", "file contents...", reg)
	if got3 != "file contents..." {
		t.Fatalf("non-writes_file result should pass through, got: %s", got3)
	}
}
