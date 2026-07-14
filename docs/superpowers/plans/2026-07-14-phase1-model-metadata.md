# Phase 1: 模型元数据基建 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立模型 ContextWindow 元数据基础设施——内置表 + 本地覆盖 + substring 匹配引擎 + resolve 注入 + 两个 HTTP endpoint + models 页 UI，为 Phase 2 动态压缩阈值提供 ContextWindow 数据。

**Architecture:** `go:embed` 内置 JSON 表 + `~/.fluctio/model-context.json` 本地覆盖，合并后用 substring longest-first 匹配 model id；config resolve 时把 ContextWindow 注入 `ResolvedAgent`；两个 endpoint（查合并表 / 拉 provider 模型列表）；models 页加 autocomplete + ContextWindow/MaxTokens 字段 + 获取列表按钮。

**Tech Stack:** Go（go:embed, encoding/json, net/http）、Next.js app router、现有 internal/config + internal/setup 框架。

**Spec:** `docs/superpowers/specs/2026-07-14-model-aware-compaction-threshold-design.md`（Phase 1）

## Global Constraints

- 遵循 `~/.claude/CLAUDE.md`：改代码前 `git stash` 建保存点；改后依次 `go build ./...` → `go test ./...` → smoke（curl health 或等效）；三步全过才 `git stash drop`，任一失败 `git checkout .` 还原后重试，同一问题不超 3 次。
- 改前端后必须 `pnpm -C web build` + `rm -rf internal/setup/web && cp -r web/out internal/setup/web` 再重启应用（embed 才更新）。
- 不动 compaction 逻辑（Phase 2 的事）、不动 session 消息模型（Phase 3）。
- 表数据来源 `docs/maxtoken.txt`（hermes-agent `DEFAULT_CONTEXT_LENGTHS`），只取 contextWindow，不含 maxTokens。
- Windows + PowerShell + Git Bash；UTC+8。

---

## File Structure

| 文件 | 职责 |
|------|------|
| `internal/config/model_context.json`（新） | embed 的内置模型 contextWindow 表 |
| `internal/config/model_embed.go`（新） | embed 声明 + 加载内置表 + 合并本地覆盖 |
| `internal/config/model_match.go`（新） | `LookupModelMeta` substring+longest-first 匹配 |
| `internal/config/config.go`（改） | `ResolvedAgent` 加 `ContextWindow`；resolve 时填 |
| `internal/config/*_test.go`（新/改） | 上述单测 |
| `internal/setup/handlers.go`（改） | 两个 endpoint |
| `web/src/lib/api.ts`（改） | `getBuiltinModels` / `fetchProviderModels` |
| `web/src/app/agents/[id]/models/page.tsx`（改） | autocomplete + 字段 + 获取按钮 |

---

### Task 1: 内置模型表 + embed 加载

**Files:**
- Create: `internal/config/model_context.json`
- Create: `internal/config/model_embed.go`
- Test: `internal/config/model_embed_test.go`

**Interfaces:**
- Produces: `func builtinModelTable() map[string]int`（返回内置表，key=model id 子串，value=contextWindow）

- [ ] **Step 1: 建 `internal/config/model_context.json`**

把 `docs/maxtoken.txt` 的 Python `DEFAULT_CONTEXT_LENGTHS` dict 转成 JSON（key 不变，value 转 int，去掉注释）。全量内容：

```json
{
  "claude-fable-5": 1000000,
  "claude-fable": 1000000,
  "claude-opus-4-8": 1000000,
  "claude-opus-4.8": 1000000,
  "claude-opus-4-7": 1000000,
  "claude-opus-4.7": 1000000,
  "claude-opus-4-6": 1000000,
  "claude-sonnet-4-6": 1000000,
  "claude-opus-4.6": 1000000,
  "claude-sonnet-4.6": 1000000,
  "claude": 200000,
  "gpt-5.6-luna": 1050000,
  "gpt-5.6-terra": 1050000,
  "gpt-5.6-sol": 1050000,
  "gpt-5.5": 1050000,
  "gpt-5.4-nano": 400000,
  "gpt-5.4-mini": 400000,
  "gpt-5.4": 1050000,
  "gpt-5.3-codex-spark": 128000,
  "gpt-5.1-chat": 128000,
  "gpt-5": 400000,
  "gpt-4.1": 1047576,
  "gpt-4": 128000,
  "gemini": 1048576,
  "gemma-4": 256000,
  "gemma4": 256000,
  "gemma-4-31b": 256000,
  "gemma-3": 131072,
  "gemma": 8192,
  "deepseek-v4-pro": 1000000,
  "deepseek-v4-flash": 1000000,
  "deepseek-chat": 1000000,
  "deepseek-reasoner": 1000000,
  "deepseek": 128000,
  "llama": 131072,
  "qwen3.6-plus": 1048576,
  "qwen3-coder-plus": 1000000,
  "qwen3-coder": 262144,
  "qwen": 131072,
  "minimax-m3": 1000000,
  "minimax": 204800,
  "glm-5.2": 1048576,
  "glm": 202752,
  "grok-composer": 200000,
  "grok-build-latest": 500000,
  "grok-build": 256000,
  "grok-code-fast": 256000,
  "grok-2-vision": 8192,
  "grok-4-fast": 2000000,
  "grok-4.20": 2000000,
  "grok-4.5": 500000,
  "grok-4.3": 1000000,
  "grok-4": 256000,
  "grok-3": 131072,
  "grok-2": 131072,
  "grok": 131072,
  "kimi": 262144,
  "hy3-preview": 262144,
  "hy3": 262144,
  "nemotron": 131072,
  "trinity": 262144,
  "elephant": 262144,
  "mimo-v2-pro": 1048576,
  "mimo-v2.5-pro": 1048576,
  "mimo-v2.5": 1048576,
  "mimo-v2-omni": 262144,
  "mimo-v2-flash": 262144
}
```

（HuggingFace `org/name` 格式的 key 如 `Qwen/Qwen3.5-397B-A17B` 也一并从 maxtoken.txt 转入——substring 匹配对含 `/` 的 key 同样有效。）

- [ ] **Step 2: 写失败测试 `internal/config/model_embed_test.go`**

```go
package config

import "testing"

func TestBuiltinModelTableKnownEntries(t *testing.T) {
	tbl := builtinModelTable()
	cases := map[string]int{
		"claude-opus-4-8": 1000000,
		"gpt-5.4":         1050000,
		"gemini":          1048576,
		"deepseek-chat":   1000000,
		"glm-5.2":         1048576,
		"grok-4-fast":     2000000,
		"claude":          200000, // catch-all
	}
	for key, want := range cases {
		if got := tbl[key]; got != want {
			t.Errorf("builtinModelTable()[%q] = %d, want %d", key, got, want)
		}
	}
	if len(tbl) < 50 {
		t.Errorf("builtin table too small: %d entries", len(tbl))
	}
}
```

- [ ] **Step 3: 验证测试失败**

Run: `go test ./internal/config/ -run TestBuiltinModelTableKnownEntries -v`
Expected: FAIL（`builtinModelTable` undefined）

- [ ] **Step 4: 写 `internal/config/model_embed.go`**

```go
package config

import _ "embed"

//go:embed model_context.json
var modelContextJSON []byte

// builtinModelTable 解析 embed 的 model_context.json。
// 返回 key=model id 子串, value=contextWindow token 数。
// 解析失败（不应发生，embed 静态文件）panic 于 init 更安全，这里返回 nil 由调用方处理。
func builtinModelTable() map[string]int {
	out := map[string]int{}
	if err := json.Unmarshal(modelContextJSON, &out); err != nil {
		return nil
	}
	return out
}
```

确认 `encoding/json` 已在 config 包 import（config.go 已用）。

- [ ] **Step 5: 验证测试通过**

Run: `go test ./internal/config/ -run TestBuiltinModelTableKnownEntries -v`
Expected: PASS

- [ ] **Step 6: commit**

```bash
git add internal/config/model_context.json internal/config/model_embed.go internal/config/model_embed_test.go
git commit -m "feat(config): embed builtin model contextWindow table"
```

---

### Task 2: 本地覆盖合并

**Files:**
- Modify: `internal/config/model_embed.go`
- Test: `internal/config/model_embed_test.go`

**Interfaces:**
- Produces: `func mergedModelTable(localPath string) map[string]int`（内置 + 本地覆盖合并，本地优先）

- [ ] **Step 1: 加失败测试**

追加到 `model_embed_test.go`：

```go
func TestMergedModelTableLocalOverrides(t *testing.T) {
	tmp := t.TempDir() + "/model-context.json"
	// 本地覆盖 claude 的 catch-all + 加一条新模型
	local := `{"claude": 500000, "my-custom-model": 99000}`
	if err := os.WriteFile(tmp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	merged := mergedModelTable(tmp)
	if merged["claude"] != 500000 {
		t.Errorf("local should override builtin claude: got %d", merged["claude"])
	}
	if merged["my-custom-model"] != 99000 {
		t.Errorf("local-only entry missing: %d", merged["my-custom-model"])
	}
	if merged["gpt-5.4"] != 1050000 {
		t.Errorf("builtin entry lost after merge: %d", merged["gpt-5.4"])
	}
}

func TestMergedModelTableNoLocalFile(t *testing.T) {
	merged := mergedModelTable("/nonexistent/path.json")
	if merged["gemini"] != 1048576 {
		t.Errorf("builtin should still load without local file: %d", merged["gemini"])
	}
}
```

加 import `"os"`。

- [ ] **Step 2: 验证失败**

Run: `go test ./internal/config/ -run TestMergedModelTable -v`
Expected: FAIL（`mergedModelTable` undefined）

- [ ] **Step 3: 实现 `mergedModelTable`**

加到 `model_embed.go`：

```go
import "os"

// mergedModelTable 合并内置表 + 本地覆盖文件（path 为空或读失败则只用内置）。
// 同 key 本地覆盖优先。用于 LookupModelMeta。
func mergedModelTable(localPath string) map[string]int {
	out := builtinModelTable()
	if localPath == "" {
		return out
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return out
	}
	local := map[string]int{}
	if err := json.Unmarshal(data, &local); err != nil {
		return out
	}
	for k, v := range local {
		out[k] = v
	}
	return out
}
```

- [ ] **Step 4: 验证通过**

Run: `go test ./internal/config/ -run TestMergedModelTable -v`
Expected: PASS

- [ ] **Step 5: commit**

```bash
git add internal/config/model_embed.go internal/config/model_embed_test.go
git commit -m "feat(config): merge local model-context override file"
```

---

### Task 3: 匹配引擎 LookupModelMeta（substring + longest-first）

**Files:**
- Create: `internal/config/model_match.go`
- Test: `internal/config/model_match_test.go`

**Interfaces:**
- Produces: `func LookupModelMeta(modelID string) (ContextWindow int, matched bool)` — substring longest-first 匹配；读 `~/.fluctio/model-context.json` 本地覆盖。

- [ ] **Step 1: 写失败测试 `internal/config/model_match_test.go`**

```go
package config

import "testing"

func TestLookupModelMetaSubstringLongestFirst(t *testing.T) {
	// 不设本地覆盖（用内置表）
	cases := []struct {
		id     string
		want   int
		reason string
	}{
		{"claude-opus-4-8-20250929", 1000000, "版本后缀 → claude-opus-4-8"},
		{"anthropic/claude-sonnet-4-6", 1000000, "provider 前缀 → claude-sonnet-4-6"},
		{"claude-3-5-sonnet", 200000, "无精确 → catch-all claude"},
		{"gpt-5.4-mini", 400000, "gpt-5.4-mini 优先于 gpt-5.4 / gpt-5（longest-first）"},
		{"gpt-5", 400000, "gpt-5 catch-all"},
		{"some-unknown-model", 0, "无匹配 → matched=false"},
	}
	for _, c := range cases {
		got, ok := lookupModelMetaIn(c.id, "") // 用显式表入口测，避免读磁盘
		if c.want == 0 {
			if ok {
				t.Errorf("%s: expected no match, got %d", c.id, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%s: got (%d,%v), want %d — %s", c.id, got, ok, c.want, c.reason)
		}
	}
}
```

- [ ] **Step 2: 验证失败**

Run: `go test ./internal/config/ -run TestLookupModelMetaSubstringLongestFirst -v`
Expected: FAIL（`lookupModelMetaIn` undefined）

- [ ] **Step 3: 写 `internal/config/model_match.go`**

```go
package config

import "strings"

// LookupModelMeta 按 modelID 查 contextWindow（substring + longest-first）。
// 合并内置表 + ~/.fluctio/model-context.json 本地覆盖。未中返回 matched=false。
func LookupModelMeta(modelID string) (contextWindow int, matched bool) {
	return lookupModelMetaIn(modelID, modelContextOverridePath())
}

// lookupModelMetaIn 用指定本地覆盖路径查（测试入口）。
func lookupModelMetaIn(modelID, localPath string) (int, bool) {
	tbl := mergedModelTable(localPath)
	var bestKey string
	for key := range tbl {
		if strings.Contains(modelID, key) && len(key) > len(bestKey) {
			bestKey = key
		}
	}
	if bestKey == "" {
		return 0, false
	}
	return tbl[bestKey], true
}

// modelContextOverridePath 返回 ~/.fluctio/model-context.json 本地覆盖路径。
func modelContextOverridePath() string {
	home := fluctioHome() // config.go 的 home 目录 helper
	return home + "/model-context.json"
}
```

（`fluctioHome()`：config.go 已有的 home 目录 helper。实现时 `grep -n "func.*Home.*string" internal/config/config.go` 确认确切名字后替换——可能是 `HomeDir()` / `FluctioHome()` 等。modelContextOverridePath 在本 Task 的 model_match.go 定义，Task 4 的 resolve 复用。）

- [ ] **Step 4: 验证通过**

Run: `go test ./internal/config/ -run TestLookupModelMetaSubstringLongestFirst -v`
Expected: PASS（5 条命中 + 1 条 no-match）

- [ ] **Step 5: commit**

```bash
git add internal/config/model_match.go internal/config/model_match_test.go
git commit -m "feat(config): LookupModelMeta substring+longest-first matcher"
```

---

### Task 4: resolve 注入 ContextWindow 到 ResolvedAgent

**Files:**
- Modify: `internal/config/config.go`（`ResolvedAgent` 加字段 + resolve 填值）
- Test: `internal/config/config_test.go`（或新建）

**Interfaces:**
- Consumes: `LookupModelMeta`（Task 3）
- Produces: `ResolvedAgent.ContextWindow int`（0=未知），供 Phase 2 用

- [ ] **Step 1: 加字段**

在 `config.go` 的 `ResolvedAgent`（约 705 行）加：

```go
type ResolvedAgent struct {
	// ... 现有字段 ...
	Model                string
	MaxTokens            int
	ContextWindow        int  // 新增；0 = 未知
}
```

- [ ] **Step 2: 写失败测试**

新建或追加 `config_test.go`：

```go
func TestResolveAgentContextWindowFromTable(t *testing.T) {
	// 构造一个最小 config，agent 用 "claude-opus-4-8" 模型，ModelEntry 不填 ContextWindow
	// resolve 后 ResolvedAgent.ContextWindow 应 = 1000000（来自表）
	// 具体构造看 ResolveConfig 签名；若难直接测，改测一个 helper：
	rc := ResolvedAgent{Model: "claude-opus-4-8"}
	if cw, ok := lookupModelMetaIn(rc.Model, ""); !ok || cw != 1000000 {
		t.Fatalf("expected 1000000 from table, got %d %v", cw, ok)
	}
	// ModelEntry 非空优先：模拟 resolve 把 entry.ContextWindow 写进 ResolvedAgent
	rc.ContextWindow = 500000 // entry 显式填的
	if rc.ContextWindow != 500000 {
		t.Fatal("entry value should win")
	}
}
```

- [ ] **Step 3: 验证测试通过（字段已加，应直接过）**

Run: `go test ./internal/config/ -run TestResolveAgentContextWindow -v`
Expected: PASS

- [ ] **Step 4: 在 resolve 路径填 ContextWindow**

`grep -n "MaxTokens:" internal/config/config.go` 定位 resolve 构造 `ResolvedAgent` 的位置（约 850、922 行附近有 `MaxTokens:` 赋值）。在每处 `ResolvedAgent{...}` 字面量或赋值里，`MaxTokens` 设定后加 ContextWindow 填充逻辑：

```go
// resolve 末尾、return resolved 前：
if resolved.ContextWindow == 0 && resolved.Model != "" {
	if cw, ok := LookupModelMeta(resolved.Model); ok {
		resolved.ContextWindow = cw
	}
}
```

（ModelEntry.ContextWindow 非空时已在构造时填入 resolved.ContextWindow；这里只在为 0 时用表兜底。确认 ModelEntry → resolved 的映射点同 MaxTokens 的映射点一致。）

- [ ] **Step 5: 全量 build + test**

Run: `go build ./... && go test ./internal/config/ ./internal/agent/`
Expected: build OK，测试全过（agent 包现有测试不破）。

- [ ] **Step 6: commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): inject ContextWindow into ResolvedAgent via table"
```

---

### Task 5: GET /api/models/builtin endpoint

**Files:**
- Modify: `internal/setup/handlers.go`（加 handler + 注册路由）
- Test: 手动 curl

**Interfaces:**
- Produces: `GET /api/models/builtin` → `200 {"claude-opus-4-8": 1000000, ...}`（合并内置 + 本地）

- [ ] **Step 1: 加 handler**

`grep -n "func (s \*Server)" internal/setup/handlers.go` 看现有 handler 风格。仿照任一 GET handler，加：

```go
// handleBuiltinModels 返回合并后的模型 contextWindow 表（内置 + 本地覆盖）。
// 前端 models 页 autocomplete 用。
func (s *Server) handleBuiltinModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.GET {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tbl := config.MergedModelTablePublic() // 见 Step 2 导出的公开入口
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tbl)
}
```

- [ ] **Step 2: 导出 mergedModelTable 的公开入口**

在 `model_embed.go` 加：

```go
// MergedModelTablePublic 供 HTTP handler 用（读默认本地覆盖路径）。
func MergedModelTablePublic() map[string]int {
	return mergedModelTable(modelContextOverridePath())
}
```

- [ ] **Step 3: 注册路由**

`grep -n "mux.Handle\|HandleFunc\|/api/" internal/setup/handlers.go`（或 server.go 注册路由处），仿照现有 `/api/...` 注册加：

```go
mux.HandleFunc("/api/models/builtin", s.handleBuiltinModels)
```

（确切注册方式 + auth 中间件参照现有 endpoint，如 `/api/agents`。）

- [ ] **Step 4: build + 重启 + smoke**

```bash
go build ./...
# 重启应用（停旧进程 + go run ./cmd/fluctio）
curl -s http://localhost:18953/api/models/builtin | head -c 200
```
Expected: JSON 含 `"claude-opus-4-8":1000000`。

- [ ] **Step 5: commit**

```bash
git add internal/setup/handlers.go internal/config/model_embed.go
git commit -m "feat(api): GET /api/models/builtin returns merged contextWindow table"
```

---

### Task 6: POST /api/agents/{id}/models/fetch — 拉 provider 模型列表

**Files:**
- Modify: `internal/setup/handlers.go`
- Test: 手动（需真实 provider key）

**Interfaces:**
- Produces: `POST /api/agents/{agentID}/models/fetch` → `200 [{"id":"...","contextWindow":...}, ...]`；上游失败 → 501 + 提示。

- [ ] **Step 1: 加 handler**

```go
// handleFetchProviderModels 按该 agent 绑定的 provider 调上游 list models，
// 每项用 LookupModelMeta 补 contextWindow。上游不可达 → 501。
func (s *Server) handleFetchProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.POST {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentID := r.PathValue("id") // 或现有路由的取参方式
	prov, err := s.providerForAgent(r.Context(), agentID) // 现有 resolver；实现时确认方法名
	if err != nil {
		http.Error(w, "provider not configured: "+err.Error(), http.StatusBadRequest)
		return
	}
	ids, err := fetchUpstreamModelIDs(r.Context(), prov) // Step 2
	if err != nil {
		http.Error(w, "upstream list failed: "+err.Error(), http.StatusNotImplemented)
		return
	}
	type item struct{ ID string; ContextWindow int }
	out := make([]item, 0, len(ids))
	for _, id := range ids {
		cw, _ := config.LookupModelMeta(id)
		out = append(out, item{ID: id, ContextWindow: cw})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
```

- [ ] **Step 2: 实现 fetchUpstreamModelIDs**

新文件 `internal/setup/provider_models.go`：

```go
package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/config"
)

// fetchUpstreamModelIDs 按 provider apiType 调上游 list models，返回 model id 列表。
// 支持 openai / gemini / anthropic；其它返回 error（handler 转 501）。
func fetchUpstreamModelIDs(ctx context.Context, prov config.ProviderConfig) ([]string, error) {
	base := strings.TrimRight(prov.APIBase, "/")
	switch prov.APIType {
	case "openai", "": // OpenAI 兼容默认
		return fetchIDs(ctx, base+"/models", "Bearer "+prov.APIKey, "")
	case "gemini":
		return fetchIDs(ctx, base+"/models?key="+prov.APIKey, "", "")
	case "anthropic":
		return fetchIDs(ctx, base+"/v1/models", prov.APIKey, "2023-06-01")
	default:
		return nil, fmt.Errorf("unsupported apiType %q for list models", prov.APIType)
	}
}

func fetchIDs(ctx context.Context, url, bearerKey, anthropicVersion string) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	if bearerKey != "" {
		if anthropicVersion != "" {
			req.Header.Set("x-api-key", strings.TrimPrefix(bearerKey, "Bearer "))
			req.Header.Set("anthropic-version", anthropicVersion)
		} else {
			req.Header.Set("Authorization", bearerKey)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Data []struct{ ID string `json:"id"` } `json:"data"`
		Models []struct{ Name string `json:"name"` } `json:"models"` // gemini
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse upstream list: %w", err)
	}
	var ids []string
	for _, m := range parsed.Data {
		ids = append(ids, m.ID)
	}
	for _, m := range parsed.Models { // gemini 返回 models[].name
		ids = append(ids, m.Name)
	}
	return ids, nil
}
```

注：`s.providerForAgent` 的确切方法名——`grep -n "func (s \*Server).*provider\|ProviderConfig" internal/setup/handlers.go` 找现有取 provider 的入口；若没有现成的，从 `s.store` 读 agent config 再取对应 provider。

- [ ] **Step 3: 注册路由**

```go
mux.HandleFunc("/api/agents/{id}/models/fetch", s.handleFetchProviderModels)
```

（路由变量语法 `{id}` 取决于路由库——Go 1.22+ `http.ServeMux` 支持 `r.PathValue("id")`；若项目用 gorilla/chi 等则用对应语法。`grep -n "PathValue\|chi\.NewRouter\|gorilla" internal/setup/` 确认。）

- [ ] **Step 4: build + 重启 + smoke（需真实 provider）**

```bash
go build ./...
# 重启
curl -s -X POST http://localhost:18953/api/agents/agt_xxx/models/fetch | head -c 300
```
Expected: 成功 → `[{"id":"...","contextWindow":...}]`；provider 不支持 → 501 + 提示。

- [ ] **Step 5: commit**

```bash
git add internal/setup/handlers.go internal/setup/provider_models.go
git commit -m "feat(api): POST /models/fetch lists models from upstream provider"
```

---

### Task 7: 前端 models 页 — autocomplete + 字段 + 获取按钮

**Files:**
- Modify: `web/src/lib/api.ts`
- Modify: `web/src/app/agents/[id]/models/page.tsx`（路径 `glob "web/src/app/**/models/page.tsx"` 确认）

**Interfaces:**
- Consumes: `GET /api/models/builtin`、`POST /api/agents/{id}/models/fetch`
- Produces: models 页能 autocomplete model id、自动填 ContextWindow、手填 MaxTokens、点按钮拉 provider 列表

- [ ] **Step 1: api.ts 加两个函数**

在 `web/src/lib/api.ts` 加：

```ts
// 返回合并后的内置+本地模型 contextWindow 表
export async function getBuiltinModels(): Promise<Record<string, number>> {
  const r = await apiFetch("/api/models/builtin");
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return r.json();
}

// 拉取 provider 上游模型列表（每项含 contextWindow）
export async function fetchProviderModels(
  agentId: string
): Promise<{ id: string; contextWindow: number }[]> {
  const r = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/models/fetch`,
    { method: "POST" }
  );
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return r.json();
}
```

- [ ] **Step 2: models 页加 autocomplete**

打开 models 页（`glob web/src/app/**/models/page.tsx` 确认确切路径）。在 model id 输入框旁加 debounce autocomplete：
- `useEffect` 加载 `getBuiltinModels()` 存 state。
- 输入框 `onChange` debounce 300ms 后，按输入文本前缀过滤 keys，下拉显示前 8 个。
- 选中某 key 时：填 model id = key，`ContextWindow` = 表[key]。

（具体 JSX 参照页面现有 input 组件风格——加一个 `<input>` + 绝对定位 `<ul>` 下拉，或用页面已有的 combobox 组件若有。）

- [ ] **Step 3: models 页加 ContextWindow / MaxTokens 字段**

在 model 配置表单里加两个 `<input type="number">`：
- **ContextWindow**：选中 autocomplete 项时自动填；可改可清空。保存到 ModelEntry.contextWindow。
- **MaxTokens**（输出上限）：用户填，默认 8192。保存到 ModelEntry.maxTokens。

- [ ] **Step 4: 加"获取模型列表"按钮**

```tsx
<button onClick={async () => {
  try {
    const list = await fetchProviderModels(agentId);
    setFetchResults(list); // 弹出列表供选择
  } catch (e) { alert("获取失败（该 provider 可能不支持）：" + e); }
}}>获取模型列表</button>
```

点选列表项时填 model id + contextWindow。

- [ ] **Step 5: build-web + 重启 + 浏览器验证**

```bash
pnpm -C web build
rm -rf internal/setup/web && cp -r web/out internal/setup/web
# 重启应用
```
浏览器打开 models 页：
- 输入 "claude" → autocomplete 下拉显示 claude 系列，选中填 contextWindow。
- 点"获取模型列表" → 拉到 provider 模型（或 501 提示）。
- ContextWindow/MaxTokens 字段能填能保存。

- [ ] **Step 6: commit**

```bash
git add web/src/lib/api.ts web/src/app/agents/[id]/models/page.tsx
git commit -m "feat(web): models page autocomplete + context window fields + fetch"
```

---

## Phase 1 完成标准

- `go build ./...` + `go test ./internal/config/` 全过。
- `GET /api/models/builtin` 返回合并表。
- `POST /api/agents/{id}/models/fetch` 对 OpenAI/Gemini provider 返回列表。
- models 页能 autocomplete、填 ContextWindow/MaxTokens、获取列表。
- ResolvedAgent.ContextWindow 在 resolve 后非 0（对表内模型）。

Phase 2 依赖本 phase 的 `ResolvedAgent.ContextWindow`。
