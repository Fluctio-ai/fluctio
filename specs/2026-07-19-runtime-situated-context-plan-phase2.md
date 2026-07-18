# Phase 2 实现计划：失败分类 + skill 条件可见性 + MCP 副作用声明

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Checkbox (`- [ ]`) syntax for tracking.

**Goal:** 三件事——(A) 工具失败时附分类 + recovery 提示，让 LLM 看到"出路"；(B) skill frontmatter 声明依赖，不满足时 hide/on_missing fallback（治 article-to-image 在 Windows 撞 bash）；(C) MCP 工具声明副作用，补全 Phase 1 的 annotateReachability 覆盖（take_screenshot 等）。

**Architecture:** 复用 Phase 1 的两个先例模式——`annotateReachability`（纯函数 + 两处 loop 出口接入）和 `MetaSandboxPrefix`（带内标记）。skill 侧**复用已存在的 `checkGating` 引擎**（skills.go:717），只加策略层（hide/on_missing）+ 统一 load_skill 的重复解析器。MCP 侧加 `RegisterFromWithEffect` 重载 + config per-server/per-tool effect。

**Tech Stack:** Go（`os/exec`/`runtime`/`filepath`/`encoding/json`/`yaml`），fluctio `internal/agent`（loop/skills）、`internal/agent/tools`（registry/load_skill）、`internal/config`、`internal/mcp`。

## Global Constraints

- 平台：Windows 开发，代码跨平台（`filepath.*`/`runtime.GOOS`，不硬编码分隔符）；`exec.LookPath` 做命令存在性检查。
- 最小改动：复用现有 `checkGating` 引擎，不重造；不重构无关代码。
- 失败分类 + skill gating 是**行为变化**（LLM 不再看到 gated skill 的原内容；失败结果多一行 hint）——这是 spec 阶段 2 既定，用户审 plan 时确认。
- TDD red-green；每任务 `go build ./...` + 相关包 `go test` 通过才 commit；失败 `git checkout .` 回退（max 3 attempts）。一任务一 commit（commit = 保存点，不用 stash）。
- commit 前缀：`feat(agent):` / `feat(agent/tools):` / `feat(config):` / `fix(skills):`。
- 时区 UTC+8。
- 不跑第二个 gateway 实例（验证复用 18953）。
- Phase 1 已落地（SideEffect/RegisterWithEffect/ReachabilityVerdict/deliver_file/annotateReachability@loop.go:2698,3434/modRuntimeContext），本 plan 在其之上。

## File Structure

- **Modify** `internal/agent/loop.go` — `classifyToolError` 纯函数 + 两处出口接入（主:2697-2759 / 流式:3431-3448）；MCP 注册改 `RegisterFromWithEffect`（:319-326）。
- **Modify** `internal/agent/tools/registry.go` — `RegisterFromWithEffect(name, desc, params, fn, source, effect)` 重载。
- **Modify** `internal/config/config.go` — `MCPServerConfig` 加 `Effect` + `ToolEffects` 字段（:56-64）。
- **Modify** `internal/agent/skills.go` — `OpenClawMeta.OnMissing` 字段；`Skill.OnMissing`；`BuildSkillsSummary` hide/fallback 策略（:312-325）；导出 `CheckGating`。
- **Modify** `internal/agent/tools/load_skill.go` — 删重复解析器（`unavailableReason` 等），改用 `skills.CheckGating` + `OnMissing`。
- **Modify** `C:/Users/mumu/.fluctio/agents/agt_becb3eeedca60527b7b5/agent/skills/openclaw-article-to-image/SKILL.md` — 补 frontmatter `os`/`requires.bins`/`on_missing`。
- 各 `*_test.go`。

---

## Task 1: 失败分类 classifyToolError

**Files:**
- Modify: `internal/agent/loop.go`（新增纯函数 + 主路径 :2728 isFailedToolResult 分支后 + 流式 :3434 后）
- Test: `internal/agent/loop_error_classify_test.go`

**Interfaces:**
- Consumes: `isFailedToolResult`（loop.go:2859）；error content 已含 `[Analyze the error above...]` 后缀（sdkbridge.go:193）。
- Produces: `classifyToolError(content string) (category, hint string)`——纯函数，前缀/关键词匹配 5 类。

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/loop_error_classify_test.go`：
```go
package agent

import "testing"

func TestClassifyToolError(t *testing.T) {
	cases := []struct{ in, wantCat, wantHintSub string }{
		{"exec: bash: executable file not found in %PATH%\n[Analyze...]", "env_missing", "替代"},
		{"open /x/y: no such file or directory", "env_missing", "替代"},
		{"'/bin/sh: foo: command not found'", "env_missing", "替代"},
		{"open /etc/shadow: permission denied", "permission", ""},
		{"createfile access is denied", "permission", ""},
		{"upstream_error: 503 Service busy", "external", "重试"},
		{"HTTP 500 internal server error", "external", "重试"},
		{"context deadline exceeded (timeout)", "external", "重试"},
		{"invalid argument: missing required field", "logic", "参数"},
		{"just some normal success result text", "", ""}, // 非失败 → 不分类
	}
	for _, c := range cases {
		gotCat, gotHint := classifyToolError(c.in)
		if gotCat != c.wantCat {
			t.Errorf("classifyToolError(%q) category = %q, want %q", c.in, gotCat, c.wantCat)
		}
		if c.wantHintSub != "" && gotHint == "" {
			t.Errorf("classifyToolError(%q) hint empty, want containing %q", c.in, c.wantHintSub)
		}
	}
}
```

- [ ] **Step 2: 运行验证失败**

Run: `go test ./internal/agent/ -run TestClassifyToolError -v`
Expected: FAIL — `undefined: classifyToolError`。

- [ ] **Step 3: 实现纯函数**

在 `internal/agent/loop.go` 的 `annotateReachability` 附近新增：
```go
// classifyToolError 对失败的工具结果文本做分类 + recovery 提示。
// 非失败文本（无 error 信号）返回 ("", "")。纯函数，便于测试。
func classifyToolError(content string) (category, hint string) {
	c := strings.ToLower(content)
	switch {
	case strings.Contains(c, "command not found") ||
		strings.Contains(c, "no such file or directory") ||
		strings.Contains(c, "executable file not found") ||
		strings.Contains(c, "not recognized as an internal or external command") ||
		strings.Contains(c, "no such file"):
		return "env_missing", "依赖的命令/文件在当前环境缺失；可换替代命令（如 Windows 用 powershell 替代 bash）、跳过该步并告知用户，或调 deliver_file 投递已有产物"
	case strings.Contains(c, "permission denied") ||
		strings.Contains(c, "access is denied") ||
		strings.Contains(c, "access denied"):
		return "permission", "权限不足；确认路径/权限，或换可见域内路径"
	case strings.Contains(c, "service unavailable") ||
		strings.Contains(c, "503") || strings.Contains(c, "500 internal") ||
		strings.Contains(c, "upstream_error") || strings.Contains(c, "timeout") ||
		strings.Contains(c, "context deadline exceeded") ||
		strings.Contains(c, "http 5") || strings.Contains(c, "http 4"):
		return "external", "外部服务错误；可退避重试、换备用服务，或告知用户稍后再试"
	case strings.Contains(c, "invalid argument") ||
		strings.Contains(c, "missing required") ||
		strings.Contains(c, "parse args") || strings.Contains(c, "bad request"):
		return "logic", "参数/逻辑错误；检查参数格式、路径合法性，或换实现方式"
	default:
		return "", ""
	}
}
```

- [ ] **Step 4: 接入两处出口**

主路径（loop.go `isFailedToolResult` 分支，:2728 附近，`RecordToolFailure` 之后、`toolMsg` 构造之前）：
```go
if thisFailed {
    ...
    a.registry.RecordToolFailure(...)
    if cat, hint := classifyToolError(resultContent); cat != "" {
        resultContent = resultContent + "\n[失败类别: " + cat + "] [可恢复: " + hint + "]"
    }
}
```

流式路径（loop.go :3434 `annotateReachability` 之后）——流式没有 `isFailedToolResult` 分支，但失败结果含 `[Analyze the error above...]`：
```go
resultContent = annotateReachability(r.toolName, resultContent, a.registry)
if cat, hint := classifyToolError(resultContent); cat != "" {
    resultContent = resultContent + "\n[失败类别: " + cat + "] [可恢复: " + hint + "]"
}
```
（流式用 classifyToolError 自身的"非失败返回空"判断代替 isFailedToolResult。）

- [ ] **Step 5: 验证通过 + commit**

Run: `go test ./internal/agent/ -run TestClassifyToolError -v` → PASS；`go build ./...`。
```bash
git add internal/agent/loop.go internal/agent/loop_error_classify_test.go
git commit -m "feat(agent): classify tool failures into categories with recovery hints"
```

---

## Task 2: MCP 工具副作用声明

**Files:**
- Modify: `internal/agent/tools/registry.go`（`RegisterFromWithEffect` 重载）
- Modify: `internal/config/config.go`（`MCPServerConfig` 加字段，:56-64）
- Modify: `internal/agent/loop.go`（MCP 注册 :319-326 改用 effect）
- Test: `internal/agent/tools/registry_effect_test.go`（追加）

**Interfaces:**
- Consumes: Phase 1 `RegisterWithEffect`/`SideEffect`/`SideWritesFile`/`SideExternal`/`SidePure`；`SourceMCP`（registry.go ToolSource 常量）。
- Produces: `RegisterFromWithEffect(name, desc, params, fn, source, effect)`；MCP 工具按 config/启发式声明 effect。

- [ ] **Step 1: 写失败测试**

追加到 `internal/agent/tools/registry_effect_test.go`：
```go
func TestRegisterFromWithEffect(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	fn := func(ctx context.Context, args json.RawMessage) (string, error) { return "ok", nil }
	r.RegisterFromWithEffect("mcp_x_screenshot", "d", map[string]interface{}{"type": "object"}, fn, SourceMCP, SideWritesFile)
	if got := r.SideEffectOf("mcp_x_screenshot"); got != SideWritesFile {
		t.Fatalf("effect = %v, want SideWritesFile", got)
	}
	// source 也应是 SourceMCP（验证没误用 RegisterWithEffect 的 SourceBuiltin）
	if t2, ok := r.tools["mcp_x_screenshot"]; !ok || t2.source != SourceMCP {
		t.Fatalf("source not SourceMCP")
	}
}
```

- [ ] **Step 2: 验证失败**

Run: `go test ./internal/agent/tools/ -run TestRegisterFromWithEffect -v` → FAIL `undefined: RegisterFromWithEffect`。

- [ ] **Step 3: registry.go 加重载**

在 `RegisterWithEffect`（:817）之后新增：
```go
// RegisterFromWithEffect 注册工具并声明副作用类型 + 来源（供 MCP/Plugin 工具用，避免 RegisterWithEffect 硬编码 SourceBuiltin）。
func (r *Registry) RegisterFromWithEffect(name, description string, parameters interface{}, fn ToolFunc, source ToolSource, effect SideEffect) {
	r.RegisterFrom(name, description, parameters, fn, source)
	if t, ok := r.tools[name]; ok {
		t.effect = effect
		r.tools[name] = t
	}
}
```

- [ ] **Step 4: config.go 加字段**

`internal/config/config.go` 的 `MCPServerConfig`（:56-64）加：
```go
type MCPServerConfig struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Effect 是该 server 所有工具的默认副作用声明（writes_file/emits_inline/external/pure）。
	Effect string `json:"effect,omitempty"`
	// ToolEffects 按"原始工具名"（不含 mcp_ 前缀）覆盖单工具的 effect。
	ToolEffects map[string]string `json:"tool_effects,omitempty"`
}
```
确认 ResolvedAgent 合并（:916/:974 附近）透传新字段（json 反序列化自动）。

- [ ] **Step 5: loop.go MCP 注册用 effect**

`internal/agent/loop.go:319-326` 的 MCP 注册循环改成：
```go
for _, td := range a.mcpMgr.ToolDefs() {
	toolName := td.Name
	effect := mcpDefaultEffect(srvName, td.Name, a.mcpServers[srvName]) // srvName 是当前 server 名；从 server config 读
	a.registry.RegisterFromWithEffect(toolName, td.Description, td.InputSchema,
		func(ctx context.Context, args json.RawMessage) (string, error) {
			return a.mcpMgr.CallTool(ctx, toolName, args)
		}, SourceMCP, effect)
}
```
（若循环里没有 srvName，需从 `a.mcpServers` 遍历适配——按实际循环结构调整为外层 server 循环 + 内层 tool 循环。若 ToolDefs() 是全 server 扁平列表，则按 toolName 前缀 `mcp_<server>_` 反解 server 名取 config。）

加辅助函数 `mcpDefaultEffect`：
```go
// mcpDefaultEffect 决定一个 MCP 工具的副作用：config per-tool > config per-server > 启发式（screenshot/browser_take_screenshot→writes_file，else external）。
func mcpDefaultEffect(serverName, toolName string, cfg config.MCPServerConfig) tools.SideEffect {
	// 原始工具名 = 去掉 "mcp_<server>_" 前缀
	raw := strings.TrimPrefix(toolName, "mcp_"+safeName(serverName)+"_")
	if e, ok := cfg.ToolEffects[raw]; ok {
		if se := parseSideEffect(e); se != tools.SidePure || e == "pure" {
			return se
		}
	}
	if cfg.Effect != "" {
		return parseSideEffect(cfg.Effect)
	}
	low := strings.ToLower(toolName)
	if strings.Contains(low, "screenshot") || strings.Contains(low, "browser_take") {
		return tools.SideWritesFile
	}
	return tools.SideExternal
}
func parseSideEffect(s string) tools.SideEffect {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "writes_file": return tools.SideWritesFile
	case "emits_inline": return tools.SideEmitsInline
	case "external": return tools.SideExternal
	default: return tools.SidePure
	}
}
```
（`safeName` 复用 mcp 包的同名逻辑——若未导出，在 agent 包内重新实现或用 `strings.ToLower` 简化前缀匹配。实现时确认。）

- [ ] **Step 6: 验证 + commit**

Run: `go test ./internal/agent/tools/ -run TestRegisterFromWithEffect -v` → PASS；`go build ./...`；`go test ./internal/agent/tools/`（确认无回归）。
```bash
git add internal/agent/tools/registry.go internal/config/config.go internal/agent/loop.go internal/agent/tools/registry_effect_test.go
git commit -m "feat(agent): declare MCP tool side-effects so annotateReachability covers them"
```

---

## Task 3: skill hide + on_missing fallback

**Files:**
- Modify: `internal/agent/skills.go`（`OpenClawMeta.OnMissing`；`Skill.OnMissing`；`BuildSkillsSummary` 策略；`discoverSkillsEnhanced` 传 OnMissing）
- Test: `internal/agent/skills_gating_test.go`

**Interfaces:**
- Consumes: 现有 `checkGating`（:717，已检查 OS/Bins/Env）；`Skill.Gated`/`GateReason`（:29-30）。
- Produces: frontmatter 字段 `metadata.fluctio.on_missing`（或 `metadata.openclaw.on_missing`）；gated skill 在 BuildSkillsSummary 按 on_missing 决定 hide/fallback。

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/skills_gating_test.go`：
```go
package agent

import "testing"

func TestBuildSkillsSummaryGatedHideAndFallback(t *testing.T) {
	skills := map[string]Skill{
		"available-skill": {Name: "available-skill", Description: "d1", Gated: false},
		"gated-no-fallback": {Name: "gated-no-fallback", Description: "d2", Gated: true, GateReason: "requires bash"},
		"gated-with-fallback": {Name: "gated-with-fallback", Description: "d3", Gated: true, GateReason: "requires bash", OnMissing: "Windows 下跳过后处理，直接交付"},
	}
	got := BuildSkillsSummary(skills, "")
	// available 列出
	if !strings.Contains(got, "available-skill") { t.Fatalf("available skill should be listed:\n%s", got) }
	// gated 无 fallback → hide
	if strings.Contains(got, "gated-no-fallback") { t.Fatalf("gated skill without on_missing should be hidden:\n%s", got) }
	// gated 有 fallback → 列出 fallback
	if !strings.Contains(got, "gated-with-fallback") || !strings.Contains(got, "Windows 下跳过后处理") {
		t.Fatalf("gated skill with on_missing should show fallback:\n%s", got)
	}
}
```

- [ ] **Step 2: 验证失败**

Run: `go test ./internal/agent/ -run TestBuildSkillsSummaryGatedHideAndFallback -v` → FAIL（当前 BuildSkillsSummary 对 gated 只 annotate 不 hide，且无 OnMissing 字段）。

- [ ] **Step 3: 加 OnMissing 字段 + 传递**

`internal/agent/skills.go`：
- `OpenClawMeta`（:64-79）加 `OnMissing string \`yaml:"on_missing"\``。
- `Skill` struct（:24-32 附近）加 `OnMissing string`。
- `discoverSkillsEnhanced`（:548-565）把 `oc.OnMissing` 存进 `Skill.OnMissing`。

- [ ] **Step 4: BuildSkillsSummary hide/fallback 策略**

`BuildSkillsSummary`（:312-325）改：
```go
for _, skill := range catalog { // 或现有遍历方式
	if skill.Gated {
		if skill.OnMissing != "" {
			fmt.Fprintf(&sb, "- %s — %s (当前环境不可用: %s) → 替代: %s\n", skill.Name, desc, skill.GateReason, skill.OnMissing)
		}
		// OnMissing 空 → 不列出（hide）
		continue
	}
	fmt.Fprintf(&sb, "- %s — %s\n", skill.Name, desc)
}
```
（对照现有 :317-322 的遍历变量名调整；保留 `skillAlwaysLoads` 的 `!Gated` 守卫。）

- [ ] **Step 5: 验证 + commit**

Run: `go test ./internal/agent/ -run TestBuildSkillsSummaryGatedHideAndFallback -v` → PASS；`go build ./...`；`go test ./internal/agent/`（确认现有 skill 测试无回归）。
```bash
git add internal/agent/skills.go internal/agent/skills_gating_test.go
git commit -m "feat(agent): hide gated skills without on_missing; show fallback when declared"
```

---

## Task 4: 统一 load_skill 用 CheckGating + OnMissing

**Files:**
- Modify: `internal/agent/skills.go`（导出 `CheckGating`）
- Modify: `internal/agent/tools/load_skill.go`（删 `unavailableReason`/`loadSkillFrontmatter`/`loadSkillMetadata`/`loadSkillOpenClawMeta`/`loadSkillRequires`，改用 `skills.CheckGating` + OnMissing）
- Test: `internal/agent/tools/load_skill_test.go`（追加 on_missing 场景）

**Interfaces:**
- Consumes: Task 3 的 `OnMissing`；`skills.CheckGating`。
- Produces: load_skill 加载 gated skill 时前置 `[SKILL FALLBACK: <on_missing>]` banner（而非旧的 unavailable banner）。

- [ ] **Step 1: 写失败测试**

追加到 `internal/agent/tools/load_skill_test.go`（参考现有 :28 模式）：
```go
func TestLoadSkillOnMissingFallback(t *testing.T) {
	// 造一个 skill 目录，frontmatter 声明 requires.bins: [bash-不存在] + on_missing
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "fake-skill", "SKILL.md"), []byte(
		"---\nname: fake-skill\ndescription: d\nmetadata:\n  fluctio:\n    requires:\n      bins: [bash-totally-nonexistent-xyz]\n    on_missing: 用 powershell 替代\n---\n# body\n"), 0o644)
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkill(r, []string{dir})
	got, err := r.Execute(context.Background(), "load_skill", `{"name":"fake-skill"}`)
	if err != nil { t.Fatal(err) }
	if !strings.Contains(got, "SKILL FALLBACK") || !strings.Contains(got, "用 powershell 替代") {
		t.Fatalf("expected on_missing fallback banner, got:\n%s", got)
	}
}
```

- [ ] **Step 2: 验证失败**

Run: `go test ./internal/agent/tools/ -run TestLoadSkillOnMissingFallback -v` → FAIL（当前 load_skill 的 unavailableReason 只查 env，不识别 on_missing/banner 不同）。

- [ ] **Step 3: 导出 CheckGating**

`internal/agent/skills.go`：把 `checkGating`（:717）重命名为 `CheckGating`（导出）。更新内部调用点（:549）。签名：
```go
func CheckGating(meta *OpenClawMeta) (gated bool, reason string)
```
（确认参数类型——侦察说是 `checkGating(meta)`，meta 是 `*OpenClawMeta`。）

- [ ] **Step 4: load_skill 用 CheckGating + OnMissing**

`internal/agent/tools/load_skill.go`：
- 删 `unavailableReason`/`loadSkillFrontmatter`/`loadSkillMetadata`/`loadSkillOpenClawMeta`/`loadSkillRequires`（:82-133）。
- `makeLoadSkill` 读 SKILL.md 后，用 `skills` 包的 frontmatter 解析 + `skills.CheckGating`：
```go
// 读 SKILL.md bytes → skills.ParseFrontmatter（导出 parseFrontmatterFromBytes 为 ParseFrontmatter，若未导出则在 skills 加导出包装）
meta, err := skills.ParseFrontmatterForLoad(content) // 解析 metadata.fluctio/openclaw → OpenClawMeta
if err == nil {
	if gated, reason := skills.CheckGating(meta); gated {
		banner := "[SKILL CURRENTLY UNAVAILABLE: " + reason + "]"
		if meta.OnMissing != "" {
			banner = "[SKILL FALLBACK: " + meta.OnMissing + "]"
		}
		content = banner + "\n\n" + content
	}
}
```
（若 skills 包的 frontmatter 解析未导出，Task 4 含将其导出/包装。`ReplaceAll("{baseDir}", ...)` 保留。）

- [ ] **Step 5: 验证 + commit**

Run: `go test ./internal/agent/tools/ -run TestLoadSkillOnMissingFallback -v` → PASS；`go build ./...`；`go test ./internal/agent/tools/`（确认现有 load_skill 测试无回归）。
```bash
git add internal/agent/skills.go internal/agent/tools/load_skill.go internal/agent/tools/load_skill_test.go
git commit -m "refactor(agent): unify load_skill gating with skills.CheckGating + on_missing fallback"
```

---

## Task 5: 给 article-to-image 补 frontmatter requires

**Files:**
- Modify: `C:/Users/mumu/.fluctio/agents/agt_becb3eeedca60527b7b5/agent/skills/openclaw-article-to-image/SKILL.md`
- （也在 `C:/Users/mumu/.fluctio/workspaces/agt_becb3eeedca60527b7b5/skills/openclaw-article-to-image/SKILL.md` 若存在同步）

**注：** 这是用户数据文件（~/.fluctio），不在 repo。改它不入 commit；但验证依赖它。

- [ ] **Step 1: 读当前 frontmatter 确认**

确认 `.../openclaw-article-to-image/SKILL.md` frontmatter 当前只有 `name`/`description`（侦察确认）。

- [ ] **Step 2: 补 frontmatter**

在 frontmatter（`---` 块）内加：
```yaml
metadata:
  fluctio:
    os: [linux, macos]
    requires:
      bins: [bash]
    on_missing: "后处理脚本依赖 bash，当前环境不可用——可跳过 post-process.sh，直接交付已生成的 HTML + 截图（产物默认落在用户可见 workspace，前端可见）。若需后处理，用 powershell 手动运行等价命令。"
```
（`os: [linux, macos]` 让 Windows 下 gated；`requires.bins: [bash]` 双重保险；on_missing 给 LLM 替代方案。）

- [ ] **Step 3: 验证 gating 生效**

重启或热加载 agent（让 skill 重扫）。确认 article-to-image 在 Windows 下：BuildSkillsSummary 里显示 fallback（on_missing）而非原条目；load_skill 调它返回 `[SKILL FALLBACK: ...]`。

（这步依赖运行时；可在 Task 6 端到端验证一起做。）

---

## Task 6: 端到端验证

**Files:** 无代码改动——验证。

- [ ] **Step 1: 失败分类**

通过 /api/chat 在 default agent 发一条会触发 exec 失败的消息（如让 LLM 跑一个不存在的命令），读 history 确认 tool 结果含 `[失败类别: env_missing] [可恢复: ...]`。

- [ ] **Step 2: skill gating（article-to-image 在 Windows）**

新 session 发"使用文章转信息图技能..."，确认 system prompt 的 Skills 段里 article-to-image 显示 fallback（on_missing）而非原描述，或 load_skill 返回 FALLBACK banner——LLM 不再盲目跑撞 bash。

- [ ] **Step 3: MCP 副作用声明**

读 history 里一次 take_screenshot 的 tool 结果：若截图落可见域外（本次 Phase 1 验证它落 workspace 可见，所以标 visible=true 不追加裁决——这是正确的）；确认 take_screenshot 已声明 SideWritesFile（DB 或日志确认 effect）。

- [ ] **Step 4: 记录结果**

把验证结果（哪些达成、哪些 gap）记到本 plan 末尾「验证结果」节。

---

## Self-Review

**Spec coverage**（spec 阶段 2 + Task 6 发现的 MCP gap）：
- 失败分类（接入点3）→ Task 1 ✅
- skill frontmatter 条件可见性（接入点5：hide + on_missing fallback）→ Task 3 ✅
- 给 article-to-image 补 frontmatter 治 Windows 缺 bash → Task 5 ✅
- MCP 工具副作用声明（Task 6 发现的 Phase 1 gap）→ Task 2 ✅
- 统一 load_skill 重复解析器 → Task 4 ✅
- 端到端验证 → Task 6 ✅

**Placeholder scan**：
- Task 2 Step 5 的 `safeName`/server 名反解——标注"实现时确认 mcp 包 safeName 是否导出"。这是实现细节，plan 给了方向。
- Task 4 Step 4 的 `skills.ParseFrontmatterForLoad`——标注"若未导出则加导出包装"。方向明确。
- Task 5 改的是 ~/.fluctio 用户文件（不入 repo），标注清楚。
无 TBD。

**Type consistency**：
- `SideEffect`/`SideWritesFile`/`SideExternal`/`SidePure`/`SourceMCP`（Phase 1）在 Task 2 一致 ✅
- `RegisterFromWithEffect(name, desc, params, fn, source, effect)` Task 2 定义 + 测试一致 ✅
- `classifyToolError(content) (cat, hint)` Task 1 定义 + 两处接入 + 测试一致 ✅
- `OpenClawMeta.OnMissing` / `Skill.OnMissing` Task 3/4 一致 ✅
- `CheckGating`（导出）Task 4 定义 + load_skill 用一致 ✅

**行为变化标注（用户审）**：
1. **gated skill 从 annotate 改 hide**（Task 3）：LLM 不再看到无 on_missing 的 gated skill。spec 既定，但这是可见的行为变化。
2. **失败结果多一行 hint**（Task 1）：LLM 看到的失败 tool 结果多了 `[失败类别/可恢复]`，略增 context。
3. **MCP 工具标 SourceMCP + effect**（Task 2）：dashboard 分类显示修正（之前误标 builtin）。

**风险**：
- Task 2 的 MCP server 名反解（toolName `mcp_<server>_<tool>` → server config）依赖 mcp 包的命名规则。若 safeName 未导出，启发式前缀匹配可能不准——plan 标注实现时确认；最坏 fallback 全标 SideExternal（保守）。
- Task 4 统一解析器涉及 skills 包导出 API 变化（CheckGating/ParseFrontmatter）——影响面 skills 包内 + load_skill。
- Task 5 改用户数据文件，不入 repo——验证依赖手动改 + 重扫。
