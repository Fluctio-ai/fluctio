# 模型感知的上下文压缩阈值 — 设计

日期：2026-07-14
状态：已与用户确认设计，待实现

## 背景与问题

当前上下文压缩（`CompactMessages`，`internal/agent/compaction.go`）使用硬编码常量 `DefaultTokenThreshold = 80000` 触发，存在两个问题：

1. **不适应不同模型的上下文窗口**：市面模型窗口从 8K 到 1M 不等，80000 对小窗口模型会撑爆，对大窗口模型又过早压缩浪费上下文。
2. **估算偏差未留足余量**：`EstimateTokens` 用 `chars/4` 估算，对中文低估约 30-50%；且**不含 system prompt**（skills + SOUL + chatter profile + 工具定义，通常 1-3 万 token）。这两项导致实际 LLM 输入比估算值大很多，曾经引发长 session 每轮返回空响应（empty response）。

模型配置里已有 `ModelEntry.ContextWindow` 字段（`internal/config/config.go:399`），但压缩逻辑没有读取它。

## 目标

- 压缩阈值按模型 `ContextWindow` 动态计算，自动适配不同模型。
- 内置主流模型的 ContextWindow / MaxTokens 表，自动匹配，用户可覆盖。
- models 页支持填写 ContextWindow / MaxTokens、从 provider 拉取模型列表、输入时自动补全。
- 压缩触发时，在 web 端对话流和 IM 渠道都展示带统计的提示消息。
- 用户可在 agent 级手动覆盖阈值。

## 非目标

- 不做在线模型元数据 API（用内置表 + 本地覆盖文件，离线可用）。
- 不改压缩的 prune/compress 算法本身（本轮只改触发阈值与提示）。
- 不改 `EstimateTokens` 的估算方式（动态阈值已通过 margin 吸收偏差）。

## 总体架构

三个相对独立的子系统，分阶段实现，各自可测可交付：

- **Phase 1 · 模型元数据基建**：内置表 + 本地覆盖 + 匹配引擎 + ContextWindow/MaxTokens 注入 resolve + models 页（字段 + autocomplete + 获取列表 proxy）。
- **Phase 2 · 动态压缩阈值**：动态公式 + 三级优先级 + `CompactMessages` 签名改造 + context 页阈值字段。（依赖 Phase 1 的 ContextWindow。）
- **Phase 3 · 压缩提示消息**：持久化 notice 消息 + `compaction_notice` chat event + web notice 气泡 + IM 文本广播。（相对独立。）

---

## Phase 1 · 模型元数据基建

### 1.1 内置模型表

新文件 `internal/config/model_context.json`，通过 `go:embed` 编译进二进制。**只含 contextWindow**（输入上限）；maxTokens（输出上限）走 agent 配置（ModelEntry.MaxTokens，默认 8192），不进表——输出上限不影响"是否超过输入窗口"的判断，且各模型输出上限数据不全。

数据来源：`docs/maxtoken.txt`（hermes-agent 的 `DEFAULT_CONTEXT_LENGTHS`，2026 年准确值）。格式（key = 模型 id 子串，value = contextWindow token 数），示例子集：

```json
{
  "claude-fable-5": 1000000,
  "claude-opus-4-8": 1000000,
  "claude-opus-4-6": 1000000,
  "claude-sonnet-4-6": 1000000,
  "claude": 200000,
  "gpt-5.6-luna": 1050000,
  "gpt-5.5": 1050000,
  "gpt-5.4": 1050000,
  "gpt-5": 400000,
  "gpt-4.1": 1047576,
  "gpt-4": 128000,
  "gemini": 1048576,
  "deepseek-v4-pro": 1000000,
  "deepseek-chat": 1000000,
  "deepseek": 128000,
  "glm-5.2": 1048576,
  "glm": 202752,
  "grok-4-fast": 2000000,
  "grok-4.5": 500000,
  "grok": 131072,
  "qwen3-coder-plus": 1000000,
  "qwen": 131072,
  "minimax-m3": 1000000,
  "kimi": 262144,
  "llama": 131072
}
```

完整列表（含 grok-4.20、gemma、nemotron、hy3 等）实现时从 `docs/maxtoken.txt` 全量转换。key 用 substring 匹配（见 1.3），含 catch-all 短 key（`claude`、`gpt-5`、`grok` 等）兜底未知变体。

### 1.2 本地覆盖

`~/.fluctio/model-context.json`（可选），格式与内置表相同。合并规则：**本地 > 内置**（同 key 本地覆盖）。用户可即时补充新模型，不用等发版。

### 1.3 匹配引擎

新文件 `internal/config/model_match.go`：

```go
type ModelMeta struct {
    ContextWindow int
}

// LookupModelMeta 按 modelID 查 contextWindow。
// substring + longest-first 匹配（对齐 hermes-agent）：在所有 key 中找
// strings.Contains(modelID, key) 命中的，取最长 key。合并内置表 + 本地覆盖。
// 未中返回 matched=false。
func LookupModelMeta(modelID string) (m ModelMeta, matched bool)
```

匹配规则（**substring + longest-first**）：
1. 遍历所有 key，`strings.Contains(modelID, key)` 为 true 的入候选
2. 取候选里**最长**的 key（避免 `gpt-5` 误匹配 `gpt-5.4-mini`——后者更长且值不同）
3. 无候选 → `matched=false`

substring 而非 prefix：处理 provider 前缀（`anthropic/claude-sonnet-4-6` 包含 `claude-sonnet-4-6`）和版本后缀。catch-all 短 key（`claude`、`grok`）兜底未知变体。

### 1.4 ContextWindow / MaxTokens 注入 resolve

`ResolvedAgent`（`internal/config/config.go:705`）新增字段：

```go
type ResolvedAgent struct {
    ...
    Model                string
    MaxTokens            int
    ContextWindow        int  // 新增；0 = 未知
    CompactionThreshold  int  // 新增；0 = 走档位动态计算（Phase 2）
    CompactionMode       string  // 新增；"" / "balanced" = 默认档位（Phase 2）
}
```

resolve 时（`ResolveConfig` / agent build 路径），按优先级填 `ContextWindow` 和 `MaxTokens`：

```
ModelEntry.ContextWindow (用户在 models 页填的，非空)
  → 本地覆盖文件
  → 内置表（LookupModelMeta）
  → 0 (未知)
```

MaxTokens 同理（ModelEntry.MaxTokens 非空优先 → 表 → 现有默认 8192）。

### 1.5 后端 endpoints

新增两个 HTTP endpoint（`internal/setup/handlers.go` 或 model 相关 handler）：

- **`GET /api/models/builtin`**：返回内置表 + 本地覆盖合并后的列表（前端 autocomplete 用）。无需 agent 范围（全局）。
- **`POST /api/agents/{agentID}/models/fetch`**：按该 agent 绑定的 provider（apiType + apiBase + apiKey）调上游 list models，返回 `[{id, contextWindow}]`。每项的 contextWindow 用 `LookupModelMeta` 补（表只含 contextWindow）；maxTokens（输出）不补，由用户在 models 页填。
  - OpenAI 兼容（apiType=openai）：`GET {apiBase}/models`，Header `Authorization: Bearer {key}`
  - Gemini（apiType=gemini）：`GET {apiBase}/models?key={key}`（或 `x-goog-api-key`）
  - Anthropic（apiType=anthropic）：`GET {apiBase}/v1/models`，Header `x-api-key`、`anthropic-version`（实现时确认当前 API 可用性；不可用则该 provider 返回 501 + 提示用内置表）
  - 其它 apiType：返回 501

### 1.6 前端 models 页

`web/src/app/agents/[id]/models/page.tsx`（路径实现时确认）：

- **model id 输入框**：debounce 后调 `GET /api/models/builtin` 做前缀 autocomplete，下拉建议。
- **ContextWindow 字段**：选中 model id（从 autocomplete 或 fetch 列表）时自动填入；可手改；可清空（清空 = 用表动态查）。
- **MaxTokens 字段**（输出上限）：用户手填，默认 8192；不从表填（表只含 contextWindow）。写入 agent.json 的 ModelEntry.MaxTokens。
- **"获取模型列表"按钮**：调 `POST /api/agents/{id}/models/fetch`，弹出候选列表（每项显示 id + 自动补的 contextWindow/maxTokens），点选即填两个字段。

---

## Phase 2 · 动态压缩阈值

用户不需要填具体 token 数——选**档位**即可。档位决定 margin 占 ContextWindow 的比例：

| 档位 | margin | 取舍 |
|------|--------|------|
| 保守（conservative） | 30% | 最安全，早压缩，丢上下文最多 |
| 平衡（balanced，默认） | 15% | 大多数 agent 合适 |
| 激进（aggressive） | 10% | 最晚压缩；仍留 10% 垫，避免 `chars/4` 估算偏差重现 empty response |

### 2.1 CompactMessages 签名改造

`internal/agent/compaction.go`：

```go
// 新签名：threshold 由调用方算好传入（替代读全局 DefaultTokenThreshold）
func CompactMessages(messages []provider.Message, workspace string, prov provider.Provider, model string, threshold int) (*CompactResult, error)
```

- `DefaultTokenThreshold` 保留为**兜底常量**（ContextWindow 未知时用）。
- 函数内部所有 `tokens < DefaultTokenThreshold` 判断改为 `tokens < threshold`。
- `CompactResult` 新增统计字段（Phase 3 用）：
  ```go
  type CompactResult struct {
      Messages    []provider.Message
      Pruned      bool
      LogFile     string
      TokensBefore int  // 新增
      TokensAfter  int  // 新增
  }
  ```

### 2.2 阈值三级优先级（loop.go）

每个 turn 开头，build system prompt 之后、`CompactMessages` 之前（`internal/agent/loop.go:2104` 非流式 / `:2953` 流式），算 threshold：

```go
func (a *Agent) compactionThresholdNow(systemPrompt string) int {
    // 1. 用户手填（agent.compactionThreshold 非空）→ clamp 到上限
    if a.compactionThreshold > 0 {
        upper := a.contextWindow - a.maxTokens
        if upper > 0 && a.compactionThreshold > upper {
            return upper  // clamp
        }
        return a.compactionThreshold
    }
    // 2. 动态计算
    if a.contextWindow > 0 {
        sysTokens := EstimateTokens([]provider.Message{{Role: "system", Content: systemPrompt}})
        margin := a.contextWindow * modeMarginPct(a.compactionMode) / 100
        t := a.contextWindow - sysTokens - a.maxTokens - margin
        if t < 1000 { t = 1000 }  // 防负/防零；小窗口模型本就该低阈值早压缩
        return t
    }
    // 3. 回退
    return DefaultTokenThreshold
}

// modeMarginPct 把档位映射成 margin 占 ContextWindow 的百分比。
// 保守 30 / 平衡 15 / 激进 10。激进档仍留 10% 垫——chars/4 对中文低估 30-50%，
// margin 再小会重现 empty response。
func modeMarginPct(mode string) int {
    switch mode {
    case "conservative": return 30
    case "aggressive":   return 10
    default:             return 15 // balanced（默认）
    }
}
```

`Agent` 结构新增字段 `contextWindow int`、`compactionThreshold int`、`compactionMode string`（从 `ResolvedAgent` 注入，manager build agent 时填；compactionMode 默认 "balanced"）。

### 2.3 context 页字段

`web/src/app/agents/[id]/context/page.tsx`：

- **压缩档位单选**（默认"平衡"）：保守 / 平衡 / 激进 三选一，每档旁边显示该 model 下算出的实际 threshold 估算值（如"平衡 → 约 85K"），让用户直观看到差异。保存到 agent.json `compactionMode`。
- **"自定义"展开**（高级）：切到自定义后出现 threshold 数字输入框，保存到 agent.json `compactionThreshold`（优先级高于档位）。校验 ≤ `ContextWindow − maxTokens`，超限前端警告 + 后端 clamp。
- 旁边显示当前 model 的 ContextWindow 作参考上限。
- 档位对应的 threshold 估算值：v1 前端按公式自算（需 system prompt token 预估），或后端给 `GET /api/agents/{id}/compaction/preview`（开放问题 3）。
- 后端校验同 2.2 的 clamp。

---

## Phase 3 · 压缩提示消息

### 3.1 触发点

`loop.go` 两处 `compactResult.Pruned` 为 true 时（已有分支，`internal/agent/loop.go:2108` / `:2957`），在现有日志后追加提示发射。

### 3.2 持久化 notice 消息

在 session 历史里插入一条 notice 消息（使其在历史回看 / 刷新后仍可见，位置在触发 turn 的 user 消息和 assistant 回复之间）。

表示方式（实现时二选一，倾向方案 A）：
- **方案 A**：`session_messages` 加 `role="notice"`，content 存 JSON 统计 `{before, after, retained_turns}`。
- **方案 B**：`role="assistant"` + `metadata.compactionNotice` 标记。

需要确认 DBStore / 文件 session 对 role="notice" 的读写兼容（`WebChatHistory` 渲染、`ArchivedMessages`、provider 序列化——notice 不发给 LLM，发送前过滤掉）。

### 3.3 Web 端

- **SSE**：emit `ChatEvent{Type: "compaction_notice", Data: {before, after, retained_turns, message}}`（流内实时显示）。
- **chat 流渲染**：`chat-screen.tsx` 识别 notice（来自 SSE event 或历史 message 的 role/metadata），渲染成居中、淡色、带图标的提示气泡，位于对话中间（user 消息和 assistant 回复之间）。
- **文案**：`📝 上下文已自动压缩（{before} → {after} tokens，保留最近 {retained_turns} 轮）`，token 数用 `formatTokenCount` 友好显示（如 12.3万）。
- **历史回看**：`WebChatHistory`（`loop.go:1051`）返回 notice 消息，前端按相同方式渲染。

### 3.4 IM 渠道

- 压缩触发时，通过 `messageBus` 发一条 outbound 系统文本（与 web notice 同文案）。
- 发送时机：在 agent 真正回复**之前**（压缩在 turn 开头，notice 先发，agent 回复随后）。IM dispatcher 串行投递即可保证顺序。
- 仅对有活跃 IM 渠道的 session 发（web session 不发 IM）。

---

## 数据模型变更汇总

| 位置 | 变更 |
|------|------|
| `internal/config/model_context.json` | 新文件（embed） |
| `internal/config/model_match.go` | 新文件（LookupModelMeta） |
| `config.ResolvedAgent` | 加 `ContextWindow int`、`CompactionThreshold int`、`CompactionMode string` |
| `config.AgentFileCfg`（agent.json） | 加 `CompactionThreshold int`、`CompactionMode string` 字段 |
| `Agent` struct（loop.go） | 加 `contextWindow int`、`compactionThreshold int`、`compactionMode string` |
| `CompactMessages` 签名 | 加 `threshold int` 参数 |
| `CompactResult` | 加 `TokensBefore`、`TokensAfter` |
| `session_messages` | 支持 role="notice"（或 metadata 标记） |
| `chat_event` | 新 type `compaction_notice` |

## 文件清单（预估）

**后端**
- `internal/config/model_context.json`（新）
- `internal/config/model_match.go`（新）
- `internal/config/config.go`（ResolvedAgent + AgentFileCfg 字段 + resolve 逻辑）
- `internal/agent/compaction.go`（签名 + CompactResult）
- `internal/agent/loop.go`（阈值计算 + 调用 + notice 发射 + WebChatHistory）
- `internal/agent/manager.go`（build agent 时注入 contextWindow/threshold）
- `internal/agent/session` 相关（notice 持久化，实现时定位）
- `internal/setup/handlers.go`（两个 endpoint）

**前端**
- `web/src/app/agents/[id]/models/page.tsx`（字段 + autocomplete + fetch 按钮）
- `web/src/app/agents/[id]/context/page.tsx`（threshold 字段）
- `web/src/components/chat-screen.tsx`（notice 气泡 + compaction_notice event 处理）
- `web/src/lib/api.ts`（getBuiltinModels / fetchProviderModels / saveCompactionThreshold）

## 测试策略

- **model_match 单测**：精确匹配、前缀降级（含 `claude-sonnet-4-5-20250929` → `claude-sonnet-4`）、最长前缀优先、本地覆盖优先、未中返回 false。
- **CompactMessages 单测**：现有测试改签名（传 threshold）；新测验证不同 threshold 触发/不触发。
- **动态阈值单测**：mock contextWindow / systemPromptTokens / maxTokens，验证公式 + clamp + 下限保护 + ContextWindow 未知回退 80000；`modeMarginPct` 三档（conservative 30 / balanced 15 / aggressive 10）+ 默认值；手填 threshold 优先于档位。
- **resolve 单测**：ModelEntry 非空 > 本地 > 内置 > 0 优先级。
- **notice 持久化 + 渲染**：压缩后 session 含 notice；发送给 LLM 的 messages 过滤掉 notice；WebChatHistory 返回 notice。
- 现有 `compaction_test.go` 4 个测试改签名后应仍通过。

## 开放问题（实现时确认）

1. **Anthropic list models endpoint**：确认 `GET /v1/models` 当前可用性与 required headers；不可用则该 apiType 返回 501 + 提示用内置表。
2. **notice 在 session_messages 的表示**：新 `role="notice"` vs metadata 标记——看 DBStore schema 和 provider 序列化哪条路改动小。
3. **context 页"自动阈值"预览**：前端是否需要后端给一个 `GET /api/agents/{id}/compaction/preview` 返回当前动态估算值，还是简单显示"自动"即可（v1 可先简单）。
4. **内置表初版覆盖范围**：~30-50 条够不够，是否需要按用户实际用到的 provider 补。

## 实现顺序建议

Phase 1 → Phase 2 → Phase 3。每个 phase 独立可测、可单独 commit / 交付。Phase 1 完成后即使不做 Phase 2/3，models 页的元数据管理也有独立价值。
