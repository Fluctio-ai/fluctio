package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RegisterDeliverTools 注册 deliver_file 工具——把任意路径文件复制进用户可见 workspace。
// 供 agent loop 在产物落到可见域外时由 LLM 自主调用。
func RegisterDeliverTools(r *Registry) {
	r.RegisterWithEffect("deliver_file",
		"Copy a file from any path into the user-visible workspace so the user can see/download it. Use this when another tool (e.g. screenshot, exec) produced a file OUTSIDE the visible workspace. Returns the new visible path.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"src": map[string]interface{}{
					"type":        "string",
					"description": "Source file path (absolute or relative).",
				},
				"dest": map[string]interface{}{
					"type":        "string",
					"description": "Destination filename or relative path within the visible workspace. Optional; defaults to the base name of src.",
				},
			},
			"required": []string{"src"},
		},
		makeDeliverFile(r), SideWritesFile)
}

type deliverFileArgs struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

func makeDeliverFile(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args deliverFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Src == "" {
			return "", fmt.Errorf("deliver_file: src is required")
		}

		visibleRoot := r.UserRoot()
		if visibleRoot == "" {
			return "", fmt.Errorf("deliver_file: no visible workspace bound")
		}

		destName := args.Dest
		if destName == "" {
			destName = filepath.Base(args.Src)
		}
		// dest 必须是可见域内的相对路径（防止逃逸）
		if filepath.IsAbs(destName) {
			return "", fmt.Errorf("deliver_file: dest must be relative within the visible workspace")
		}
		dst := filepath.Join(visibleRoot, filepath.Clean(destName))
		// Verify dst stays inside visibleRoot: a relative dest like "../../etc/passwd"
		// is rejected by IsAbs but resolves outside visibleRoot after Join.
		rel, err := filepath.Rel(visibleRoot, dst)
		// Reject real parent traversal only (".." exactly or "../...").
		// A legitimate filename that happens to start with ".." (e.g. "..foo")
		// must NOT be flagged — filepath.Rel returns "..foo" for those, which
		// a naive HasPrefix(..) check would wrongly reject.
		if err != nil {
			return "", fmt.Errorf("deliver_file: dest must stay within the visible workspace")
		}
		r := filepath.ToSlash(rel)
		if r == ".." || strings.HasPrefix(r, "../") {
			return "", fmt.Errorf("deliver_file: dest must stay within the visible workspace")
		}

		// src 通常是 agent 从工具结果里拿到的容器内逻辑路径 /workspace/<name>。
		// 该文件已由 writeWorkspaceBytes 的 host 后端写到 visibleRoot；但
		// deliver_file 跑在宿主机 gateway 进程，没有 /workspace 顶层目录，
		// 必须把 /workspace 前缀映射回 visibleRoot 才能读到。其它绝对路径
		// （如 /www/fluctio/agents/...）维持原样直传 os.Open。
		src := args.Src
		if src == "/workspace" {
			src = visibleRoot
		} else if strings.HasPrefix(src, "/workspace/") {
			src = filepath.Join(visibleRoot, strings.TrimPrefix(src, "/workspace"))
		}
		// 若 src 映射后已等于 dst（典型：image_gen 产物由 writeWorkspaceBytes
		// 直接写到 visibleRoot，agent 拿 /workspace/<name> 调 deliver_file，映射后
		// src 与默认 dest 同路径），"复制"=自己复制自己——os.Create(dst) 先把原文件
		// 截断成 0 字节，再从同一(已空)文件读出 0 字节，原文件被毁。判同即跳过。
		if filepath.Clean(src) == filepath.Clean(dst) {
			return fmt.Sprintf("Already in visible workspace: %s", dst), nil
		}
		in, err := os.Open(src)
		if err != nil {
			return "", fmt.Errorf("deliver_file: open src: %w", err)
		}
		defer in.Close()

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", fmt.Errorf("deliver_file: mkdir: %w", err)
		}
		out, err := os.Create(dst)
		if err != nil {
			return "", fmt.Errorf("deliver_file: create dest: %w", err)
		}

		n, err := io.Copy(out, in)
		if err != nil {
			out.Close()
			return "", fmt.Errorf("deliver_file: copy: %w", err)
		}
		// Close explicitly to surface final-flush errors (disk full / write fault),
		// which a deferred Close would swallow.
		if err := out.Close(); err != nil {
			return "", fmt.Errorf("deliver_file: close: %w", err)
		}
		return fmt.Sprintf("Delivered %d bytes to %s", n, dst), nil
	}
}
