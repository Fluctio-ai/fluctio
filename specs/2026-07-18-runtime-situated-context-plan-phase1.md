# 阶段 1 PoC 实现计划：运行时处境层（Runtime Situated Context）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 LLM 看见自己的操作处境与产物可见性，遇到产物落在用户可见域外时自主调用 `deliver_file` 投递，无需用户催促。

**Architecture:** 三件套——(1) Registry 上加副作用声明 + 公开可见性裁决；(2) agent loop 工具结果统一出口插 `annotateReachability`，对落盘工具产物判定可见性，不可见则追加裁决行；(3) 新 prompt 模块 `modRuntimeContext` 把可见域/能力/deliver_file 用法讲清楚。配套 `deliver_file` 工具作为 LLM 的迁移手段。

**Tech Stack:** Go（标准库 `os`/`io`/`filepath`/`runtime`/`encoding/json`），fluctio 现有 `internal/agent/tools`、`internal/agent`（loop/context/prompt_modules）、`internal/provider`。

## Global Constraints

- **平台**：Windows 优先开发（PowerShell），代码须跨平台——用 `filepath.Join`/`filepath.Separator`，绝不硬编码 `/` 或 `\`；`runtime.GOOS` 分支处理 OS 差异。
- **最小改动**：不重构无关代码；现有 ~24 个 `Register`/`RegisterFrom`/`RegisterSerial` 调用点**零改动**（新增的 `effect` 字段默认 `SidePure`）。
- **TDD 纪律**：每任务先写失败测试→验证失败→最小实现→验证通过→commit。
- **验证门槛**：每个任务结束 `go build ./...` 通过；涉及 tools 包的任务额外 `go test ./internal/agent/tools/...`。
- **commit 纪律**：每任务一个 commit，conventional commits 前缀（`feat(agent): …`）。
- **时区**：UTC+8。
- **不开新 git 实例**：PoC 验证（Task 6）若需跑服务，复用已在跑的实例，勿启动第二个（DB+lease 竞争会 crash 原实例）。

## File Structure

- **Modify** `internal/agent/tools/registry.go` — `SideEffect` 类型 + `registeredTool.effect` 字段 + `RegisterWithEffect` + `SideEffectOf` getter + `ReachabilityVerdict` 公开方法。
- **Create** `internal/agent/tools/deliver.go` — `deliver_file` 工具 handler + `RegisterDeliverTools` 聚合注册。
- **Create** `internal/agent/tools/deliver_test.go` — deliver_file 集成测试。
- **Create** `internal/agent/tools/registry_effect_test.go` — SideEffect + ReachabilityVerdict 测试。
- **Modify** `internal/agent/loop.go` — `annotateReachability` helper + 主路径(:2669)/流式路径(:3404) 两处接入；`bindSession`(:280) 传入 MCP 摘要到 ctxBuilder。
- **Create** `internal/agent/loop_reachability_test.go` — annotateReachability 纯函数测试。
- **Modify** `internal/agent/context.go` — `ContextBuilder` 加 `mcpServerSummary string` 字段 + setter。
- **Modify** `internal/agent/prompt_modules.go` — `modRuntimeContext` 模块 + 插入 `agentModules`/`chatbotModules`。

---

## Task 1: SideEffect 类型 + 注册扩展

**Files:**
- Modify: `internal/agent/tools/registry.go`（`registeredTool` 结构体 :729 附近；`ToolSource` 常量块之后）
- Test: `internal/agent/tools/registry_effect_test.go`

**Interfaces:**
- Produces: `type SideEffect int` 及常量 `SidePure/SideWritesFile/SideEmitsInline/SideExternal`；`(*Registry).RegisterWithEffect(name, desc string, params interface{}, fn ToolFunc, effect SideEffect)`；`(*Registry).SideEffectOf(name string) SideEffect`。后续 Task 3/4 依赖。

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/tools/registry_effect_test.go`：
```go
package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSideEffectRegistration(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	fn := func(ctx context.Context, args json.RawMessage) (string, error) {
		return "ok", nil
	}
	r.RegisterWithEffect("dummy_writes", "desc", map[string]interface{}{"type": "object"}, fn, SideWritesFile)
	r.Register("dummy_pure", "desc", map[string]interface{}{"type": "object"}, fn)

	if got := r.SideEffectOf("dummy_writes"); got != SideWritesFile {
		t.Fatalf("dummy_writes effect = %v, want SideWritesFile", got)
	}
	if got := r.SideEffectOf("dummy_pure"); got != SidePure {
		t.Fatalf("dummy_pure effect = %v, want SidePure (default)", got)
	}
	if got := r.SideEffectOf("nonexistent"); got != SidePure {
		t.Fatalf("nonexistent effect = %v, want SidePure", got)
	}
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/agent/tools/ -run TestSideEffectRegistration -v`
Expected: FAIL — `undefined: SideEffect` / `r.RegisterWithEffect undefined` / `r.SideEffectOf undefined`。

- [ ] **Step 3: 实现**

在 `internal/agent/tools/registry.go`，找到 `type ToolSource int` 常量块（约 :154 附近，`SourceBuiltin` 等之后），新增：
```go
// SideEffect 声明工具的副作用类型，供 agent loop 判定是否需要可达性裁决。
type SideEffect int

const (
	SidePure SideEffect = iota // 默认：无副作用或纯查询（read_file/list_dir）
	SideWritesFile             // 产生落盘文件（write_file/deliver_file/apply_patch）
	SideEmitsInline            // 产生内联产物（image_gen 的 base64/url）
	SideExternal               // 外部副作用（exec/MCP 截图等）
)
```

在 `registeredTool` 结构体（:729）加字段：
```go
type registeredTool struct {
	def    provider.Tool
	fn     ToolFunc
	source ToolSource
	effect SideEffect
}
```

在 `RegisterFrom`（:762）之后新增：
```go
// RegisterWithEffect 注册工具并声明其副作用类型。
func (r *Registry) RegisterWithEffect(name, description string, parameters interface{}, fn ToolFunc, effect SideEffect) {
	r.RegisterFrom(name, description, parameters, fn, SourceBuiltin)
	if t, ok := r.tools[name]; ok {
		t.effect = effect
		r.tools[name] = t
	}
}

// SideEffectOf 返回工具的副作用声明；未注册工具返回 SidePure。
func (r *Registry) SideEffectOf(name string) SideEffect {
	if t, ok := r.tools[name]; ok {
		return t.effect
	}
	return SidePure
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test ./internal/agent/tools/ -run TestSideEffectRegistration -v`
Expected: PASS。

- [ ] **Step 5: 构建 + commit**

Run: `go build ./...`
```bash
git add internal/agent/tools/registry.go internal/agent/tools/registry_effect_test.go
git commit -m "feat(agent/tools): add SideEffect declaration + RegisterWithEffect"
```

---

## Task 2: ReachabilityVerdict 公开方法

**Files:**
- Modify: `internal/agent/tools/registry.go`（`isWorkspacePath` :156 附近）
- Test: `internal/agent/tools/registry_effect_test.go`（追加）

**Interfaces:**
- Consumes: `isWorkspacePath`（:156，未导出）、`UserRoot()`（:663）
- Produces: `(*Registry).ReachabilityVerdict(path string) (visible bool, visibleRoot string)`。Task 4 的 `annotateReachability`（agent 包）依赖此公开方法（因为 `isWorkspacePath` 未导出）。

- [ ] **Step 1: 写失败测试**

追加到 `registry_effect_test.go`：
```go
func TestReachabilityVerdict(t *testing.T) {
	root := t.TempDir()
	r := NewRegistry(root, root)
	r.SetUserRoot(root)

	// 相对路径（workspace 内）→ 可见
	vis, vr := r.ReachabilityVerdict("notes/x.txt")
	if !vis {
		t.Fatalf("relative path should be visible")
	}
	if vr != root {
		t.Fatalf("visibleRoot = %q, want %q", vr, root)
	}

	// 绝对路径 → 不可见
	vis, _ = r.ReachabilityVerdict("D:/some/abs/path.png")
	if vis {
		t.Fatalf("absolute path should not be visible")
	}
}
```
注：`SetUserRoot` 已存在（`registry.go:369`）；`NewRegistry(root, root)` 把 userRoot 初始化为 root（见 `coding_scope_test.go:9` 模式）。

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/agent/tools/ -run TestReachabilityVerdict -v`
Expected: FAIL — `r.ReachabilityVerdict undefined`。

- [ ] **Step 3: 实现**

在 `internal/agent/tools/registry.go` 的 `isWorkspacePath`（:156）之后新增：
```go
// ReachabilityVerdict 判定一个产物路径是否对前端用户可见，返回是否可见及可见域根。
// 供 agent loop（agent 包）调用——isWorkspacePath 未导出，故提供此公开封装。
func (r *Registry) ReachabilityVerdict(path string) (visible bool, visibleRoot string) {
	return r.isWorkspacePath(path), r.UserRoot()
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test ./internal/agent/tools/ -run TestReachabilityVerdict -v`
Expected: PASS。

- [ ] **Step 5: 构建 + commit**

Run: `go build ./...`
```bash
git add internal/agent/tools/registry.go internal/agent/tools/registry_effect_test.go
git commit -m "feat(agent/tools): expose ReachabilityVerdict for agent-loop reachability checks"
```

---

## Task 3: deliver_file 工具

**Files:**
- Create: `internal/agent/tools/deliver.go`
- Create: `internal/agent/tools/deliver_test.go`
- Modify: `internal/agent/loop.go`（agent setup 处挂钩 `RegisterDeliverTools`，仿 `RegisterBillingTools` 调用点）

**Interfaces:**
- Consumes: Task 1 的 `RegisterWithEffect`/`SideWritesFile`；`UserRoot()`（:663）；`resolvePathSandboxed`（file.go:406）+ `effectiveSandboxRoot`（file.go:434）——若沙箱启用。
- Produces: `RegisterDeliverTools(r *Registry)`；工具名 `deliver_file`，副作用 `SideWritesFile`，返回格式 `"Delivered %d bytes to %s"`。

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/tools/deliver_test.go`：
```go
package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliverFile(t *testing.T) {
	root := t.TempDir()
	r := NewRegistry(root, root)
	r.SetUserRoot(root)
	RegisterDeliverTools(r)

	// 在可见域外造一个源文件
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "shot.png")
	if err := os.WriteFile(src, []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := r.Execute(context.Background(), "deliver_file",
		`{"src":"`+filepath.ToSlash(src)+`"}`)
	if err != nil {
		t.Fatalf("deliver_file: %v", err)
	}
	if !strings.Contains(got, "Delivered 7 bytes to") {
		t.Fatalf("unexpected output: %s", got)
	}
	// 目标落在可见域根下
	entries, _ := os.ReadDir(root)
	if len(entries) == 0 {
		t.Fatalf("nothing delivered into visible root %s", root)
	}
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/agent/tools/ -run TestDeliverFile -v`
Expected: FAIL — `undefined: RegisterDeliverTools`。

- [ ] **Step 3: 实现 deliver.go**

创建 `internal/agent/tools/deliver.go`：
```go
package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"encoding/json"
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

		in, err := os.Open(args.Src)
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
		defer out.Close()

		n, err := io.Copy(out, in)
		if err != nil {
			return "", fmt.Errorf("deliver_file: copy: %w", err)
		}
		return fmt.Sprintf("Delivered %d bytes to %s", n, dst), nil
	}
}
```

- [ ] **Step 4: 挂钩到 agent setup**

在 `internal/agent/loop.go` 找到 `RegisterBillingTools` 的调用点（grep `RegisterBillingTools`），在其附近加：
```go
tools.RegisterDeliverTools(a.registry)
```
（需 `import` 当前 loop.go 已有的 tools 包别名——保持一致。）

- [ ] **Step 5: 运行测试验证通过**

Run: `go test ./internal/agent/tools/ -run TestDeliverFile -v`
Expected: PASS。
Run: `go build ./...`
Expected: 构建成功（确认 loop.go 挂钩编译通过）。

- [ ] **Step 6: commit**

```bash
git add internal/agent/tools/deliver.go internal/agent/tools/deliver_test.go internal/agent/loop.go
git commit -m "feat(agent/tools): add deliver_file tool to relocate artifacts into visible workspace"
```

---

## Task 4: annotateReachability helper + loop 接入

**Files:**
- Modify: `internal/agent/loop.go`（新增 helper；主路径 :2669 后；流式路径 :3404 后）
- Create: `internal/agent/loop_reachability_test.go`

**Interfaces:**
- Consumes: Task 1 `SideEffectOf`/`SideWritesFile`；Task 2 `ReachabilityVerdict`/`UserRoot()`；`toolCallResult`（sdkbridge.go:126）
- Produces: `annotateReachability(toolName, resultContent string, reg *tools.Registry) string`——纯函数，对 `SideWritesFile` 工具解析结果文本里的落盘路径，不可见则追加裁决行。

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/loop_reachability_test.go`：
```go
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
```
（import 已含 context/encoding/json，无需补全。）

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/agent/ -run TestAnnotateReachability -v`
Expected: FAIL — `undefined: annotateReachability`。

- [ ] **Step 3: 实现 helper**

在 `internal/agent/loop.go` 的 `extractToolMeta`（:3524）附近新增：
```go
var writtenPathRE = regexp.MustCompile(`(?:to|→|->)\s+(\S+)`)

// annotateReachability 对落盘型工具产物判定可见性，不可见则追加裁决行。
// 纯函数，便于测试。只在 SideWritesFile 工具上生效；其他工具原样返回。
func annotateReachability(toolName, resultContent string, reg *tools.Registry) string {
	if reg.SideEffectOf(toolName) != tools.SideWritesFile {
		return resultContent
	}
	visibleRoot := reg.UserRoot()
	var notes []string
	for _, m := range writtenPathRE.FindAllStringSubmatch(resultContent, -1) {
		p := strings.TrimRight(m[1], ".,;:\"'")
		visible, _ := reg.ReachabilityVerdict(p)
		if !visible {
			notes = append(notes, fmt.Sprintf(
				"[产物 %s 不在用户可见域 %s；可调 deliver_file(src=%q) 投递到可见域供用户查看]",
				p, visibleRoot, p))
		}
	}
	if len(notes) == 0 {
		return resultContent
	}
	return resultContent + "\n" + strings.Join(notes, "\n")
}
```
（`regexp`/`strings`/`fmt` 需在 loop.go import——多数已存在，确认补齐。）

- [ ] **Step 4: 接入两处 chokepoint**

主路径（:2669 后）：原
```go
resultContent, meta := extractToolMeta(r.result)
```
之后插入一行：
```go
resultContent = annotateReachability(r.toolName, resultContent, a.registry)
```

流式路径（:3404 后）：同样在 `resultContent, meta := extractToolMeta(r.result)` 之后插入同一行。

- [ ] **Step 5: 运行测试验证通过**

Run: `go test ./internal/agent/ -run TestAnnotateReachability -v`
Expected: PASS。
Run: `go build ./...`
Expected: 构建成功。

- [ ] **Step 6: commit**

```bash
git add internal/agent/loop.go internal/agent/loop_reachability_test.go
git commit -m "feat(agent): annotate on-disk artifacts with reachability verdict at tool-result exit"
```

---

## Task 5: stable 处境层 prompt 模块

**Files:**
- Modify: `internal/agent/context.go`（`ContextBuilder` 加字段）
- Modify: `internal/agent/prompt_modules.go`（新模块 + 插入列表）
- Modify: `internal/agent/loop.go`（`bindSession` :280 传入 MCP 摘要）

**Interfaces:**
- Consumes: `runtime.GOOS`；`ContextBuilder.mcpServerSummary`（新增）；`exec.LookPath("bash")` 现场探测。
- Produces: prompt 模块 `modRuntimeContext`，输出可见域根、bash 可用性、MCP server 列表 + 其 cwd、`deliver_file` 用法提示。

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/prompt_runtime_context_test.go`：
```go
package agent

import (
	"strings"
	"testing"
)

func TestModRuntimeContext(t *testing.T) {
	cb := &ContextBuilder{
		workspace:        "/tmp/ws",
		mcpServerSummary: "playwright (cwd: /tmp/ws/.playwright-mcp)",
		sandboxEnabled:   false,
	}
	p := &promptCtx{cb: cb, mode: "agent"}
	got := modRuntimeContext(p)

	mustContain := []string{
		"可见域",          // visible workspace
		"deliver_file",   // 投递工具提示
		"playwright",     // MCP server 名
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Fatalf("modRuntimeContext missing %q:\n%s", s, got)
		}
	}
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/agent/ -run TestModRuntimeContext -v`
Expected: FAIL — `unknown field mcpServerSummary in struct literal` / `undefined: modRuntimeContext`。

- [ ] **Step 3: 加 ContextBuilder 字段**

`internal/agent/context.go` 的 `ContextBuilder` 结构体（:21-50）加：
```go
mcpServerSummary string // MCP server 名称 + cwd 摘要，供处境层告知 LLM
```

- [ ] **Step 4: 实现 modRuntimeContext**

`internal/agent/prompt_modules.go`，先加模块函数（放在 `modAgentIntro` 之后）：
```go
func modRuntimeContext(p *promptCtx) string {
	bash := "不可用"
	if _, err := exec.LookPath("bash"); err == nil {
		bash = "可用"
	}
	mcp := p.cb.mcpServerSummary
	if mcp == "" {
		mcp = "（无）"
	}
	return fmt.Sprintf(`# 运行时处境（你正在操作的真实环境）

- 操作系统：%s/%s
- bash：%s
- 用户可见域（前端能看到的文件范围）：当前 session/project 的 workspace 根
- MCP 工具服务器：%s
  注意：部分 MCP 工具（如截图）可能把产物写到可见域之外（例如自己的 cwd 子目录）。若你产出文件后用户看不到，该文件很可能落在了可见域外。
- 投递手段：调用 deliver_file(src=<产物绝对路径>) 可把任意路径文件复制进可见域供用户查看。

原则：凡是你产出文件后，先确认文件在可见域内；不在就主动 deliver_file，不要等用户催。`,
		runtime.GOOS, runtime.GOARCH, bash, mcp)
}
```
（`exec`/`runtime`/`fmt` import 确认补齐——`runtime` 已用于 modAgentIntro。`os/exec` 可能需新 import。）

在 `agentModules`（:92）的 `{"agent_intro", modAgentIntro}` 之后插入：
```go
{"runtime_context", modRuntimeContext},
```
在 `chatbotModules`（:113）做同样插入。

- [ ] **Step 5: bindSession 传入 MCP 摘要**

`internal/agent/loop.go` 的 `bindSession`（:280）中，在设置 registry scope 之后，加：
```go
a.ctxBuilder.mcpServerSummary = summarizeMCPServers(a.mcpServers, a.mcpSessionDir)
```
并在 loop.go 加辅助函数：
```go
func summarizeMCPServers(servers map[string]config.MCPServerConfig, sessionDir string) string {
	if len(servers) == 0 {
		return ""
	}
	var parts []string
	cwd := sessionDir
	if cwd == "" {
		cwd = "<unset>"
	}
	for name := range servers {
		parts = append(parts, fmt.Sprintf("%s (cwd: %s)", name, cwd))
	}
	return strings.Join(parts, ", ")
}
```

- [ ] **Step 6: 运行测试验证通过**

Run: `go test ./internal/agent/ -run TestModRuntimeContext -v`
Expected: PASS。
Run: `go build ./...`
Expected: 构建成功。

- [ ] **Step 7: commit**

```bash
git add internal/agent/context.go internal/agent/prompt_modules.go internal/agent/loop.go internal/agent/prompt_runtime_context_test.go
git commit -m "feat(agent): add runtime_context prompt module exposing visible workspace + deliver_file"
```

---

## Task 6: 端到端验证（article-to-image 重放）

**Files:** 无代码改动——验证任务。

**目标**：用 article-to-image 同任务验证 F 理念——LLM 自主投递产物、零催促。

- [ ] **Step 1: 确认服务在跑**

Run（PowerShell，用户执行）：`! curl -s http://localhost:<port>/health`
若没跑，由用户启动 `gateway --port <port>`（勿启动第二个实例）。

- [ ] **Step 2: 新建 session 跑同任务**

通过 web UI 或 API 新建 session，发送与 `s-1784355741022-nv26q9` 相同的首条消息：
> 使用文章转信息图技能，帮我把这篇文章转为可分享的图片 https://mp.weixin.qq.com/s/noFiGkvKjH1UhPYCIOLHsQ

- [ ] **Step 3: 观察行为，对照成功标准**

预期（对比原 session 的 8 轮催促）：
1. system prompt 含「运行时处境」段（可见域 + deliver_file 用法）——可从 session_messages 的 goal_context 或抓包确认。
2. take_screenshot 产物落在可见域外时，LLM **自主**调 `deliver_file` 把截图搬进可见域。
3. 用户前端直接看到图，无需追问"我没看到图片"。
4. 全程**零催促**（用户除首条外不发消息即完成）。

- [ ] **Step 4: 记录结果**

把新 session 的 id、消息数、是否零催促、产物可见情况记到 `specs/2026-07-18-runtime-situated-context-plan-phase1.md` 末尾的「验证结果」节。

- [ ] **Step 5: 若失败——回头改机制**

若 LLM 仍未自主 deliver：
- 检查 stable 层是否真的进了 system prompt（modRuntimeContext 输出）
- 检查 take_screenshot 是否被 annotateReachability 覆盖（MCP 工具副作用声明为 SideExternal，PoC 不自动判定——靠 stable 层告知 LLM 自己判断）；若 LLM 仍不关联，考虑把 take_screenshot 显式声明 SideWritesFile + 结果路径解析纳入（但 MCP 结果格式不控，需 diff——退到阶段 3）。
- 记录失败现象，回到 spec 调整。

---

## Self-Review

**Spec coverage**：
- 接入点 1a（stable 处境层）→ Task 5 ✅
- 接入点 2（落盘检测+可见性裁决）→ Task 4 ✅
- 接入点 2'（工具副作用声明）→ Task 1 ✅
- 接入点 4（deliver_file）→ Task 3 ✅
- 阶段 1 成功标准（stable 层知可见域 / 产物标不可见 / 自主 deliver / 前端见图 / 零催促）→ Task 6 ✅
- 未覆盖（属阶段 2/3，本 plan 非目标）：失败分类(接入点3)、skill 条件可见性(接入点5)、volatile 层(接入点1b)、diff 捕获。✅ 范围正确。

**Placeholder scan**：Task 4 测试 import 路径已填实际 module 名 `github.com/fluctio-ai/fluctio`（见 go.mod）。无 TBD。

**Type consistency**：
- `SideEffect`/`SideWritesFile`/`SidePure`（Task 1）在 Task 3/4 一致使用 ✅
- `RegisterWithEffect` 签名（Task 1）与 Task 3/4/测试调用一致 ✅
- `ReachabilityVerdict(path) (visible bool, visibleRoot string)`（Task 2）与 Task 4 使用一致 ✅
- `annotateReachability(toolName, resultContent, reg)`（Task 4）两处 chokepoint 与测试一致 ✅
- `ContextBuilder.mcpServerSummary`（Task 5）测试/实现/bindSession 一致 ✅

**风险标注**：
- Task 4 的 `writtenPathRE` 正则用 `(?:to|→|->)\s+(\S+)` 匹配 write_file/deliver_file 的 "to <path>"——覆盖现有格式。MCP take_screenshot 结果格式不控，PoC 不依赖正则捕获它，靠 stable 层（Task 5）告知 LLM 自行关联。
- Task 5 `modRuntimeContext` 含每轮 `exec.LookPath("bash")`——成本极低（单次系统调用），可接受。
- Task 6 依赖 MCP cwd 现状（开放问题4）；若截图已 scope 到 session workspace，则天然可见，deliver_file 对截图场景非必需但仍为其他场景保留。
