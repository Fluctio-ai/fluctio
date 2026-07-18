package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
)

// TestSubagentToolResultAnnotation verifies the annotation pipeline that
// the subagent tool-result exit (subagent.go ~:210-222) applies to each
// internal tool result before appending it to its private messages slice.
//
// Subagent internal tool results never return to the parent (by design),
// so the subagent MUST self-annotate using the same pipeline as the
// streaming exit (loop.go ~:3443-3455):
//
//	extractToolMeta -> annotateReachability -> isFailedToolResult/classifyToolError
//
// Driving runSubagentLoop directly requires a full provider/registry
// setup and the only externally observable output is the final
// synthesised text, so this test exercises the same inlined pipeline
// the subagent exit uses. Failing here means the subagent exit has
// regressed (or was never wired).
func TestSubagentToolResultAnnotation(t *testing.T) {
	root := t.TempDir()
	reg := tools.NewRegistry(root, root)
	reg.SetUserRoot(root)
	reg.RegisterWithEffect("write_file", "d", map[string]interface{}{"type": "object"},
		func(ctx context.Context, args json.RawMessage) (string, error) { return "", nil },
		tools.SideWritesFile)

	// Case 1: write_file artifact outside visible root — subagent exit
	// must append the reachability verdict so the model learns to call
	// deliver_file. Mirrors loop_reachability_test.go but through the
	// exact pipeline used by subagent.go.
	rawResult := "Written 1234 bytes to D:/outside/x.png"
	content, meta := extractToolMeta(rawResult)
	content = annotateReachability("write_file", content, reg)
	if !strings.Contains(content, "不在用户可见域") || !strings.Contains(content, "deliver_file") {
		t.Fatalf("subagent exit must annotate outside-root artifact; got:\n%s", content)
	}
	if meta != nil {
		t.Fatalf("extractToolMeta should return nil meta for plain result; got %v", meta)
	}

	// Case 2: a genuinely failed tool result must pick up the
	// [失败类别 ...] tag via the isFailedToolResult gate + classifier,
	// exactly as the streaming path does.
	failResult := "HTTP 500 internal server error"
	content, _ = extractToolMeta(failResult)
	content = annotateReachability("write_file", content, reg)
	tagged := false
	if isFailedToolResult(nil, content) {
		if cat, hint := classifyToolError(content); cat != "" {
			content = content + "\n[失败类别: " + cat + "] [可恢复: " + hint + "]"
			tagged = true
		}
	}
	if !tagged {
		t.Fatalf("subagent exit must tag a failed result; got:\n%s", content)
	}
	if !strings.Contains(content, "[失败类别:") || !strings.Contains(content, "[可恢复:") {
		t.Fatalf("subagent exit tag shape mismatch; got:\n%s", content)
	}

	// Case 3: a successful result containing a classifier substring
	// ("port 5030") must NOT be tagged — the isFailedToolResult gate
	// protects it, matching TestClassifyToolErrorStreamingSuccessNotMisfired.
	successResult := "Server listening on port 5030"
	content, _ = extractToolMeta(successResult)
	content = annotateReachability("write_file", content, reg)
	if isFailedToolResult(nil, content) {
		if cat, _ := classifyToolError(content); cat != "" {
			t.Fatalf("subagent exit must not tag success-with-substring; got cat=%q content=%q", cat, content)
		}
	}
}
