# Phase 2: 动态压缩阈值（档位） Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 用模型 ContextWindow 动态算压缩阈值（保守/平衡/激进三档），替代硬编码 80000；用户可在 context 页选档或自定义。

**Architecture:** `CompactMessages` 加 `threshold` 参数；`Agent` 加 `contextWindow`/`compactionMode`/`compactionThreshold` 字段（manager 从 ResolvedAgent 注入）；loop 每 turn 按档位算 threshold（margin = ContextWindow × 档位%）；context 页三档单选 + 自定义；新 endpoint 给档位预览值。

**Tech Stack:** Go（internal/agent/compaction.go + loop.go + manager.go）、Next.js context 页。

**Spec:** `docs/superpowers/specs/2026-07-14-model-aware-compaction-threshold-design.md`（Phase 2）

**依赖：** Phase 1 完成（`ResolvedAgent.ContextWindow` 已注入）。

## Global Constraints
- 同 Phase 1 Global Constraints。
- `DefaultTokenThreshold` 保留为兜底常量（ContextWindow 未知时用 80000）。
- 不动 prune/compress 算法本身，只改触发阈值来源。
- 改前端后 build-web + 重启。

---

## File Structure

| 文件 | 职责 |
|------|------|
| `internal/agent/compaction.go`（改） | `CompactMessages` 加 threshold 参数；`CompactResult` 加统计；`modeMarginPct` |
| `internal/agent/loop.go`（改） | `Agent` 加字段；`compactionThresholdNow`；两处调用传 threshold |
| `internal/agent/manager.go`（改） | build agent 时注入 contextWindow/mode/threshold |
| `internal/agent/slash.go`（改） | `/compact` 命令的 CompactMessages 调用同步改签名 |
| `internal/agent/compaction_test.go`（改） | 现有测试改签名 + 新增档位测试 |
| `internal/config/config.go`（改） | `AgentFileCfg` 加 `CompactionMode`/`CompactionThreshold`；resolve 传递 |
| `internal/setup/handlers.go`（改） | `GET /api/agents/{id}/compaction/preview` |
| `web/src/app/agents/[id]/context/page.tsx`（改） | 档位单选 + 自定义 |

---

### Task 1: CompactMessages 加 threshold 参数 + CompactResult 统计字段

**Files:**
- Modify: `internal/agent/compaction.go`
- Modify: `internal/agent/compaction_test.go`（改现有 4 个测试签名）

**Interfaces:**
- Produces: `func CompactMessages(messages, workspace, prov, model string, threshold int) (*CompactResult, error)`
- Produces: `CompactResult{Messages, Pruned, LogFile, TokensBefore, TokensAfter}`

- [ ] **Step 1: 改 CompactMessages 签名 + 内部用 threshold**

`compaction.go:48`：

```go
func CompactMessages(messages []provider.Message, workspace string, prov provider.Provider, model string, threshold int) (*CompactResult, error) {
	tokens := EstimateTokens(messages)
	if threshold <= 0 {
		threshold = DefaultTokenThreshold // 防御：调用方传 0
	}
	if tokens < threshold {
		return &CompactResult{Messages: messages, TokensBefore: tokens, TokensAfter: tokens}, nil
	}
	slog.Info("context compaction triggered", "tokens", tokens, "threshold", threshold, "message_count", len(messages))
	logFile, err := writeHistoryLog(messages, workspace)
	if err != nil { slog.Warn("failed to write history log", "error", err) }
	pruned := pruneOldToolResults(messages)
	prunedTokens := EstimateTokens(pruned)
	slog.Info("after pruning", "tokens_before", tokens, "tokens_after", prunedTokens)
	if prunedTokens < threshold {
		return &CompactResult{Messages: pruned, Pruned: true, LogFile: logFile, TokensBefore: tokens, TokensAfter: prunedTokens}, nil
	}
	compressed, err := compressOlderMessages(pruned, prov, model)
	if err != nil {
		slog.Warn("compression failed, using pruned messages", "error", err)
		return &CompactResult{Messages: pruned, Pruned: true, LogFile: logFile, TokensBefore: tokens, TokensAfter: prunedTokens}, nil
	}
	slog.Info("after compression", "tokens_before", prunedTokens, "tokens_after", EstimateTokens(compressed))
	return &CompactResult{Messages: compressed, Pruned: true, LogFile: logFile, TokensBefore: tokens, TokensAfter: EstimateTokens(compressed)}, nil
}
```

`CompactResult` 加字段：

```go
type CompactResult struct {
	Messages     []provider.Message
	Pruned       bool
	LogFile      string
	TokensBefore int  // 新增
	TokensAfter  int  // 新增
}
```

- [ ] **Step 2: 改 compactResult.Pruned 分支（loop.go / slash.go 引用 LogFile 不变）**

无需改——现有引用 `compactResult.Messages`/`.Pruned`/`.LogFile` 仍有效。

- [ ] **Step 3: 更新现有测试调用签名**

`compaction_test.go` 的 `TestCompactMessagesHandlesOversizedToolResult`：

```go
out, err := CompactMessages(msgs, t.TempDir(), &fakeSummarizer{}, "", DefaultTokenThreshold)
```
（加最后一个参数 `DefaultTokenThreshold`。）

加新测试验证 threshold 控制触发：

```go
func TestCompactMessagesThresholdControlsTrigger(t *testing.T) {
	msgs := make([]provider.Message, PruneTurnAge+5)
	for i := range msgs { msgs[i] = provider.Message{Role: "user", Content: strings.Repeat("x", 1000)} }
	// 高 threshold → 不触发
	out, _ := CompactMessages(msgs, t.TempDir(), &fakeSummarizer{}, "", 10_000_000)
	if out.Pruned { t.Error("high threshold should not trigger") }
	if out.TokensBefore == 0 { t.Error("TokensBefore should be populated") }
	// 低 threshold → 触发
	out2, _ := CompactMessages(msgs, t.TempDir(), &fakeSummarizer{}, "", 1000)
	if !out2.Pruned { t.Error("low threshold should trigger") }
}
```

- [ ] **Step 4: 改 slash.go 的 /compact 调用**

`grep -n "CompactMessages" internal/agent/slash.go`（约 361 行），加 threshold 参数。/compact 用什么 threshold？用 agent 当前的（Phase 2 Task 3 的 `a.compactionThresholdNow`）。但 slash.go 在 Task 3 之前——**本 Task 先传 `DefaultTokenThreshold`**，Task 4 再改成动态。

```go
result, err := CompactMessages(sessionMsgs, a.homePath, a.provider, a.model, DefaultTokenThreshold)
```

- [ ] **Step 5: 改 loop.go 两处调用（先传 DefaultTokenThreshold，Task 4 再改动态）**

`grep -n "CompactMessages" internal/agent/loop.go`（2104、2953），两处加 `, DefaultTokenThreshold`。

- [ ] **Step 6: build + test**

```bash
go build ./... && go test ./internal/agent/ -run "Compact|Prune|Compress|SafeCompaction"
```
Expected: 全过（现有测试改签名 + 新增通过）。

- [ ] **Step 7: commit**

```bash
git add internal/agent/compaction.go internal/agent/compaction_test.go internal/agent/loop.go internal/agent/slash.go
git commit -m "refactor(agent): CompactMessages takes threshold param + result stats"
```

---

### Task 2: modeMarginPct + Agent 字段

**Files:**
- Modify: `internal/agent/loop.go`（Agent struct 加字段）
- Create: `internal/agent/compaction.go`（加 modeMarginPct，或 loop.go）
- Test: `internal/agent/compaction_test.go`

**Interfaces:**
- Produces: `func modeMarginPct(mode string) int`（conservative 30 / balanced 15 / aggressive 10）
- Produces: `Agent` 字段 `contextWindow int`, `compactionThreshold int`, `compactionMode string`

- [ ] **Step 1: 写 modeMarginPct 失败测试**

追加 `compaction_test.go`：

```go
func TestModeMarginPct(t *testing.T) {
	cases := map[string]int{
		"conservative": 30,
		"balanced":     15,
		"aggressive":   10,
		"":             15, // 默认 balanced
		"unknown":      15,
	}
	for mode, want := range cases {
		if got := modeMarginPct(mode); got != want {
			t.Errorf("modeMarginPct(%q) = %d, want %d", mode, got, want)
		}
	}
}
```

- [ ] **Step 2: 验证失败**

Run: `go test ./internal/agent/ -run TestModeMarginPct -v`
Expected: FAIL（undefined）

- [ ] **Step 3: 加 modeMarginPct 到 compaction.go**

```go
// modeMarginPct 把档位映射成 margin 占 ContextWindow 的百分比。
// 保守 30 / 平衡 15 / 激进 10。激进档仍留 10% 垫——chars/4 对中文低估 30-50%，
// margin 再小会重现 empty response。
func modeMarginPct(mode string) int {
	switch mode {
	case "conservative":
		return 30
	case "aggressive":
		return 10
	default:
		return 15 // balanced（默认）
	}
}
```

- [ ] **Step 4: 验证通过**

Run: `go test ./internal/agent/ -run TestModeMarginPct -v`
Expected: PASS

- [ ] **Step 5: Agent struct 加字段**

`loop.go` 的 `Agent` struct（约 37 行）加：

```go
type Agent struct {
	// ... 现有字段 ...
	contextWindow       int  // 新增；Phase 1 ResolvedAgent 注入；0=未知
	compactionThreshold int  // 新增；用户手填；0=走档位动态
	compactionMode      string  // 新增；""/balanced=默认
}
```

- [ ] **Step 6: build（确认无语法错）**

Run: `go build ./internal/agent/`
Expected: OK

- [ ] **Step 7: commit**

```bash
git add internal/agent/compaction.go internal/agent/compaction_test.go internal/agent/loop.go
git commit -m "feat(agent): modeMarginPct + Agent context-window/mode fields"
```

---

### Task 3: compactionThresholdNow + manager 注入

**Files:**
- Modify: `internal/agent/loop.go`（加方法）
- Modify: `internal/agent/manager.go`（build 时注入）
- Modify: `internal/config/config.go`（AgentFileCfg 加字段 + resolve 传递）
- Test: `internal/agent/compaction_test.go`

**Interfaces:**
- Produces: `func (a *Agent) compactionThresholdNow(systemPrompt string) int`

- [ ] **Step 1: 写 compactionThresholdNow 测试**

需要一个不依赖完整 Agent 的测试入口。把核心逻辑抽成纯函数：

```go
// computeThreshold 纯函数版（测试入口）。
// priority: manual > dynamic(by mode) > fallback
func computeThreshold(manual, contextWindow, sysTokens, maxTokens int, mode string) int {
	if manual > 0 {
		upper := contextWindow - maxTokens
		if upper > 0 && manual > upper { return upper }
		return manual
	}
	if contextWindow > 0 {
		margin := contextWindow * modeMarginPct(mode) / 100
		t := contextWindow - sysTokens - maxTokens - margin
		if t < 1000 { t = 1000 }
		return t
	}
	return DefaultTokenThreshold
}
```

测试：

```go
func TestComputeThreshold(t *testing.T) {
	// manual 优先
	if got := computeThreshold(50000, 200000, 20000, 8192, "balanced"); got != 50000 {
		t.Errorf("manual should win: %d", got)
	}
	// manual clamp
	if got := computeThreshold(300000, 200000, 20000, 8192, "balanced"); got != 200000-8192 {
		t.Errorf("manual should clamp to cw-maxTokens: %d", got)
	}
	// 动态 balanced: 200000 - 20000 - 8192 - 30000 = 141808
	if got := computeThreshold(0, 200000, 20000, 8192, "balanced"); got != 141808 {
		t.Errorf("balanced dynamic: %d", got)
	}
	// conservative margin 30%: 200000-20000-8192-60000=111808
	if got := computeThreshold(0, 200000, 20000, 8192, "conservative"); got != 111808 {
		t.Errorf("conservative: %d", got)
	}
	// aggressive margin 10%: 200000-20000-8192-20000=151808
	if got := computeThreshold(0, 200000, 20000, 8192, "aggressive"); got != 151808 {
		t.Errorf("aggressive: %d", got)
	}
	// ContextWindow 未知 → 80000
	if got := computeThreshold(0, 0, 20000, 8192, "balanced"); got != DefaultTokenThreshold {
		t.Errorf("unknown cw fallback: %d", got)
	}
	// 下限保护
	if got := computeThreshold(0, 30000, 25000, 8192, "balanced"); got != 1000 {
		t.Errorf("floor 1000: %d", got)
	}
}
```

- [ ] **Step 2: 验证失败 → 实现 computeThreshold**

加到 `compaction.go`（上面的纯函数）。验证测试通过。

- [ ] **Step 3: Agent 方法 compactionThresholdNow**

`loop.go` 加：

```go
// compactionThresholdNow 算当前 turn 的压缩阈值。
func (a *Agent) compactionThresholdNow(systemPrompt string) int {
	sysTokens := EstimateTokens([]provider.Message{{Role: "system", Content: systemPrompt}})
	return computeThreshold(a.compactionThreshold, a.contextWindow, sysTokens, a.maxTokens, a.compactionMode)
}
```

- [ ] **Step 4: config.go AgentFileCfg 加字段 + resolve 传递**

`grep -n "type AgentFileCfg\|CompactionThreshold" internal/config/config.go`。在 AgentFileCfg 加：

```go
CompactionMode       string `json:"compactionMode,omitempty"`       // ""/balanced/conservative/aggressive
CompactionThreshold  int    `json:"compactionThreshold,omitempty"`  // 0=走档位动态
```

resolve 时（ResolvedAgent 也加这两个字段，同 Phase 1 Task 4 的 ContextWindow 模式）传递。

- [ ] **Step 5: manager.go build agent 时注入**

`grep -n "NewAgentWithSkillsCfg\|ag\.maxTokens\|rc\.MaxTokens" internal/agent/manager.go`。在 build agent 后（NewAgentWithSkillsCfg 调用后）加：

```go
ag.contextWindow = rc.ContextWindow
ag.compactionMode = rc.CompactionMode
ag.compactionThreshold = rc.CompactionThreshold
```

（`rc` 是 ResolvedAgent；确认变量名。）

- [ ] **Step 6: build + test**

```bash
go build ./... && go test ./internal/agent/ ./internal/config/
```
Expected: 全过。

- [ ] **Step 7: commit**

```bash
git add internal/agent/loop.go internal/agent/compaction.go internal/agent/compaction_test.go internal/agent/manager.go internal/config/config.go
git commit -m "feat(agent): compactionThresholdNow + mode/threshold injection"
```

---

### Task 4: loop.go 两处用动态 threshold

**Files:**
- Modify: `internal/agent/loop.go`（2104、2953 两处）

- [ ] **Step 1: 非流式路径（约 2104）**

当前（Phase 2 Task 1 改过）：
```go
compactResult, err := CompactMessages(sessionMsgs, a.homePath, a.provider, a.model, DefaultTokenThreshold)
```
改为：
```go
threshold := a.compactionThresholdNow(systemPrompt)
compactResult, err := CompactMessages(sessionMsgs, a.homePath, a.provider, a.model, threshold)
```
（`systemPrompt` 变量在该处上方 `a.ctxBuilder.BuildSystemPromptAs(...)` 已赋值——确认变量名。）

- [ ] **Step 2: 流式路径（约 2953）**

同样改为 `threshold := a.compactionThresholdNow(systemPrompt)` + 传 threshold。

- [ ] **Step 3: slash.go /compact 也用动态**

```go
threshold := a.compactionThresholdNow(/* 需要 system prompt */)
```
/compact 路径如果没现成的 systemPrompt，用空串（sysTokens=0）或临时 build。看 slash.go 上下文——若有 systemPrompt 用它，否则传 `""`（compactionThresholdNow 对空 systemPrompt 给 sysTokens=0，threshold 偏大但仍合理）。

- [ ] **Step 4: build + test + smoke**

```bash
go build ./... && go test ./internal/agent/
# 重启应用，发消息触发压缩，看日志 "context compaction triggered threshold=X"
```
Expected: threshold 不再固定 80000，而是按 agent model 算（如 claude → ~14 万 balanced）。

- [ ] **Step 5: commit**

```bash
git add internal/agent/loop.go internal/agent/slash.go
git commit -m "feat(agent): use dynamic compaction threshold in both loop paths"
```

---

### Task 5: GET /api/agents/{id}/compaction/preview

**Files:**
- Modify: `internal/setup/handlers.go`

**Interfaces:**
- Produces: `GET /api/agents/{id}/compaction/preview` → `{contextWindow, maxTokens, systemPromptTokens, modes:{conservative,balanced,aggressive}}`

- [ ] **Step 1: 加 handler**

```go
func (s *Server) handleCompactionPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.GET {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentID := r.PathValue("id")
	ag := s.gateway.LocalAgentManager().Get(agentID) // 确认取 agent 的方法名
	if ag == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	resp := ag.CompactionPreview() // Step 2
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
```

- [ ] **Step 2: Agent.CompactionPreview 方法**

`loop.go` 加（或 compaction.go）：

```go
type CompactionPreview struct {
	ContextWindow       int            `json:"contextWindow"`
	MaxTokens           int            `json:"maxTokens"`
	SystemPromptTokens  int            `json:"systemPromptTokens"`
	Modes               map[string]int `json:"modes"` // conservative/balanced/aggressive → threshold
	ManualThreshold     int            `json:"manualThreshold,omitempty"`
}

// CompactionPreview 给 context 页展示各档位的实际 threshold 估算。
// systemPrompt 用空串（v1：不实算 system prompt token，sysTokens=0，展示的是"最大可能 threshold"）。
func (a *Agent) CompactionPreview() CompactionPreview {
	sysTokens := 0 // v1 简化；如要精确，build 一次 system prompt 再 EstimateTokens
	modes := map[string]int{}
	for _, m := range []string{"conservative", "balanced", "aggressive"} {
		margin := a.contextWindow * modeMarginPct(m) / 100
		t := a.contextWindow - sysTokens - a.maxTokens - margin
		if t < 1000 { t = 1000 }
		modes[m] = t
	}
	return CompactionPreview{
		ContextWindow: a.contextWindow,
		MaxTokens:     a.maxTokens,
		SystemPromptTokens: sysTokens,
		Modes:         modes,
		ManualThreshold: a.compactionThreshold,
	}
}
```

注：`ag` 是 `*Agent`——确认 `LocalAgentManager().Get` 返回 `*Agent`（或通过 expose 方法）。若 Agent 不便暴露，在 manager 加 proxy 方法。

- [ ] **Step 3: 注册路由 + build + smoke**

```go
mux.HandleFunc("/api/agents/{id}/compaction/preview", s.handleCompactionPreview)
```
```bash
go build ./... # 重启
curl -s http://localhost:18953/api/agents/agt_xxx/compaction/preview
```
Expected: JSON 含三档 threshold。

- [ ] **Step 4: commit**

```bash
git add internal/setup/handlers.go internal/agent/loop.go
git commit -m "feat(api): GET compaction/preview returns per-mode threshold estimates"
```

---

### Task 6: context 页档位 UI

**Files:**
- Modify: `web/src/app/agents/[id]/context/page.tsx`（git status 里这个文件在改，已存在）
- Modify: `web/src/lib/api.ts`

- [ ] **Step 1: api.ts 加 preview 函数**

```ts
export interface CompactionPreview {
  contextWindow: number;
  maxTokens: number;
  systemPromptTokens: number;
  modes: { conservative: number; balanced: number; aggressive: number };
  manualThreshold?: number;
}
export async function getCompactionPreview(agentId: string): Promise<CompactionPreview> {
  const r = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/compaction/preview`);
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return r.json();
}
```

- [ ] **Step 2: context 页加档位单选**

打开 context page。加：
- `useEffect` 加载 `getCompactionPreview(agentId)` 存 state。
- 三档 radio（保守/平衡/激进），每档旁显示 `modes[mode]` 估算值（如"平衡 → 约 141K"）。token 数用 `formatK(n)`（`n>=1000 ? (n/1000).toFixed(0)+"K" : n`）。
- "自定义"radio 选中后展开 `<input type="number">`，保存 manualThreshold。
- 保存调现有 agent config save endpoint（写 compactionMode / compactionThreshold 到 agent.json）——参照该页现有保存模式。

- [ ] **Step 3: build-web + 重启 + 浏览器验证**

```bash
pnpm -C web build && rm -rf internal/setup/web && cp -r web/out internal/setup/web
# 重启
```
打开 context 页：三档可选，每档显示 threshold 估算；选自定义能填数字；保存后重启应用阈值生效（看压缩日志）。

- [ ] **Step 4: commit**

```bash
git add web/src/app/agents/[id]/context/page.tsx web/src/lib/api.ts
git commit -m "feat(web): context page compaction mode selector + custom threshold"
```

---

## Phase 2 完成标准

- `go build ./...` + `go test ./internal/agent/ ./internal/config/` 全过。
- 压缩日志 threshold 按 agent model 算（非固定 80000）。
- context 页三档可选 + 自定义，保存生效。
- `compaction/preview` endpoint 返回三档估算。
- ContextWindow 未知时回退 80000（不破坏无表模型 agent）。

Phase 3（压缩提示）独立，可在此之后做。
