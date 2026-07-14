# Phase 3: 压缩提示消息 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]` syntax.

**Goal:** 自动压缩触发时，web 端在对话中间插入持久化提示气泡、IM 渠道发一条文本，都带压缩前后 token 统计。

**Architecture:** 方案 B——notice 存为 `role="assistant"` + `metadata.compactionNotice={before,after,retained_turns}`；content 存可读文本（IM 直接用，web 也显示）。loop 压缩后存 notice + emit `compaction_notice` SSE event；`withMessageTimestampsForChatter` 过滤 notice 不发给 LLM；`WebChatHistory` 返回 notice；chat-screen 渲染居中淡色气泡；IM dispatcher 发 content 文本。

**Tech Stack:** Go（internal/agent/loop.go + session）、Next.js chat-screen、现有 messageBus dispatcher。

**Spec:** `docs/superpowers/specs/2026-07-14-model-aware-compaction-threshold-design.md`（Phase 3，方案 B）

**依赖：** Phase 2（`CompactResult.TokensBefore/TokensAfter` 已有）。Phase 3 可独立于 Phase 2 的动态阈值（只要 CompactResult 有统计即可——Phase 2 Task 1 提供）。

## Global Constraints
- 同 Phase 1 Global Constraints。
- notice **不能进 LLM**（它是 UI 提示）——发送前过滤。
- notice 要持久化（历史回看/刷新可见）。
- 改前端后 build-web + 重启。

---

## File Structure

| 文件 | 职责 |
|------|------|
| `internal/agent/compaction.go`（改） | `buildCompactionNotice` 文案 helper |
| `internal/agent/loop.go`（改） | 两处压缩后存 notice + emit event；`withMessageTimestampsForChatter` 过滤；`WebChatHistory` 返回 notice |
| `internal/agent/loop.go`（改） | IM 文本广播（PostTurn 前发） |
| `web/src/components/chat-screen.tsx`（改） | `compaction_notice` event 处理 + notice 气泡渲染 |

---

### Task 1: 文案 helper + 存 notice（loop.go 两处）

**Files:**
- Modify: `internal/agent/compaction.go`
- Modify: `internal/agent/loop.go`

**Interfaces:**
- Produces: `func buildCompactionNotice(r *CompactResult) (text string, meta map[string]any)`

- [ ] **Step 1: 写 buildCompactionNotice 失败测试**

追加 `compaction_test.go`：

```go
func TestBuildCompactionNotice(t *testing.T) {
	r := &CompactResult{TokensBefore: 123456, TokensAfter: 4111}
	text, meta := buildCompactionNotice(r)
	if !strings.Contains(text, "压缩") || !strings.Contains(text, "20") {
		t.Errorf("notice text missing compaction + retained turns: %q", text)
	}
	if meta["before"] != 123456 || meta["after"] != 4111 {
		t.Errorf("meta stats wrong: %v", meta)
	}
	if !strings.Contains(text, "12.3万") {
		t.Errorf("before should be formatted as 万: %q", text)
	}
}
```

- [ ] **Step 2: 验证失败 → 实现**

`compaction.go` 加：

```go
import "strconv"

// buildCompactionNotice 生成压缩提示的可读文本 + 统计 metadata。
// text: IM/web 都显示的可读文案；meta: {before, after, retained_turns} 供 web 气泡。
func buildCompactionNotice(r *CompactResult) (string, map[string]any) {
	meta := map[string]any{
		"before":         r.TokensBefore,
		"after":          r.TokensAfter,
		"retained_turns": PruneTurnAge,
	}
	text := fmt.Sprintf("📝 上下文已自动压缩（%s → %s tokens，保留最近 %d 轮）",
		formatTokenCount(r.TokensBefore), formatTokenCount(r.TokensAfter), PruneTurnAge)
	return text, meta
}

// formatTokenCount 中文友好显示：>=10000 用"万"，否则原数。
func formatTokenCount(n int) string {
	if n >= 10000 {
		return strconv.FormatFloat(float64(n)/10000, 'f', 1, 64) + "万"
	}
	return strconv.Itoa(n)
}
```

- [ ] **Step 3: 验证通过**

Run: `go test ./internal/agent/ -run TestBuildCompactionNotice -v`
Expected: PASS

- [ ] **Step 4: loop.go 两处压缩后存 notice + emit event**

`grep -n "compactResult.Pruned" internal/agent/loop.go`（2108、2957 两处）。在现有 `if compactResult != nil && compactResult.Pruned { ... }` 块内，`a.maybeExtractSummary` 后加：

```go
		// 持久化压缩提示（方案 B：role=assistant + metadata.compactionNotice）
		if compactResult.TokensBefore > 0 {
			text, meta := buildCompactionNotice(compactResult)
			sess.Append(provider.Message{
				Role:      "assistant",
				Content:   text,
				Metadata:  map[string]any{"compactionNotice": meta},
				Timestamp: time.Now().UnixMilli(),
			})
			emitEvent(ctx, ChatEvent{Type: "compaction_notice", Data: meta})
		}
```

两处都加。`ChatEvent` 的 Type 字段是 string，Data 是 map——确认现有 ChatEvent 结构（`grep -n "type ChatEvent" internal/agent/`）。

- [ ] **Step 5: build + test**

```bash
go build ./... && go test ./internal/agent/
```
Expected: 全过。

- [ ] **Step 6: commit**

```bash
git add internal/agent/compaction.go internal/agent/compaction_test.go internal/agent/loop.go
git commit -m "feat(agent): persist compaction notice + emit SSE event"
```

---

### Task 2: 过滤 notice 不发给 LLM

**Files:**
- Modify: `internal/agent/loop.go`（`withMessageTimestampsForChatter`）

- [ ] **Step 1: 定位 + 加过滤**

`grep -n "func.*withMessageTimestampsForChatter" internal/agent/loop.go`。在该函数构建返回 []provider.Message 的循环里，跳过 compactionNotice：

```go
for _, m := range msgs {
	// 压缩提示是 UI-only，不发给 LLM
	if _, ok := m.Metadata["compactionNotice"]; ok {
		continue
	}
	// ... 现有 timestamp 注入逻辑 ...
	out = append(out, m)
}
```

（确切变量名看函数体。`m.Metadata` 是 `map[string]any`——确认 Message.Metadata 类型。）

- [ ] **Step 2: 加测试（如有 withMessageTimestampsForChatter 测试）**

`grep -rn "withMessageTimestampsForChatter" internal/agent/*_test.go`。若无测试，手动验证：压缩触发后，下一 turn 的 LLM 请求（`FLUCTIO_DUMP_LLM=1` 看日志）不含 notice 文本。

- [ ] **Step 3: build + smoke**

```bash
go build ./...
# 重启，FLUCTIO_DUMP_LLM=1，触发压缩，检查 ~/.fluctio/logs/llm-dump.log 不含 "上下文已自动压缩"
```

- [ ] **Step 4: commit**

```bash
git add internal/agent/loop.go
git commit -m "fix(agent): exclude compaction notice from LLM-bound messages"
```

---

### Task 3: WebChatHistory 返回 notice

**Files:**
- Modify: `internal/agent/loop.go`（`WebChatHistory`，约 1051 行）

- [ ] **Step 1: 在 assistant case 里识别 compactionNotice**

`WebChatHistory` 的 `case "assistant":` 分支，在 `history = append(...)` 前判断：

```go
case "assistant":
	// 压缩提示单独渲染成 notice（前端识别 entry.kind="compaction_notice"）
	if _, ok := m.Metadata["compactionNotice"]; ok {
		meta, _ := m.Metadata["compactionNotice"].(map[string]any)
		entry := map[string]any{
			"role":      "assistant",
			"kind":      "compaction_notice",
			"content":   m.Content,
			"timestamp": m.Timestamp,
		}
		for k, v := range meta { entry[k] = v } // before/after/retained_turns
		history = append(history, entry)
		continue
	}
	// ... 现有 assistant 处理 ...
```

- [ ] **Step 2: build + 重启 + smoke**

```bash
go build ./...  # 重启
```
触发压缩后刷新页面，`GET /api/agents/{id}/chats/{sid}/history` 返回的 messages 含 `kind:"compaction_notice"` 项。

- [ ] **Step 3: commit**

```bash
git add internal/agent/loop.go
git commit -m "feat(agent): WebChatHistory surfaces compaction notice entries"
```

---

### Task 4: chat-screen 渲染 notice 气泡 + 处理 SSE event

**Files:**
- Modify: `web/src/components/chat-screen.tsx`

- [ ] **Step 1: 处理 compaction_notice SSE event**

`grep -n "ChatEvent\|emitEvent\|on.*content_delta\|case \"" web/src/components/chat-screen.tsx` 找 SSE event 处理处。加 `compaction_notice` case：把它转成一条 notice 气泡插入 chat 流（同 user/assistant 气泡的 append 路径，但 kind=compaction_notice）。

具体：在 SSE 事件 switch 里加：
```ts
case "compaction_notice": {
  appendNotice({
    kind: "compaction_notice",
    content: `📝 上下文已自动压缩（${formatK(ev.data.before)} → ${formatK(ev.data.after)} tokens，保留最近 ${ev.data.retained_turns} 轮）`,
    timestamp: Date.now(),
  });
  break;
}
```
`appendNotice` 往 chat messages state 推一条 notice 项；`formatK` 同后端 formatTokenCount（>=10000 用"万"）。

- [ ] **Step 2: 历史回看渲染 notice**

chat 流渲染处（`grep -n "msg.role\|history.map\|role ===" web/src/components/chat-screen.tsx`），识别 `kind === "compaction_notice"`（来自 WebChatHistory Task 3）渲染成居中淡色气泡：

```tsx
{msg.kind === "compaction_notice" ? (
  <div className="flex justify-center my-3">
    <span className="text-xs text-muted-foreground bg-muted/40 rounded-full px-3 py-1">
      {msg.content}
    </span>
  </div>
) : (
  /* 现有 user/assistant 渲染 */
)}
```

- [ ] **Step 3: build-web + 重启 + 浏览器验证**

```bash
pnpm -C web build && rm -rf internal/setup/web && cp -r web/out internal/setup/web
# 重启
```
触发压缩（发消息到长 session）：对话中间出现居中淡色 `📝 上下文已自动压缩（...）` 气泡；刷新页面后仍在。

- [ ] **Step 4: commit**

```bash
git add web/src/components/chat-screen.tsx
git commit -m "feat(web): render compaction notice bubble + handle SSE event"
```

---

### Task 5: IM 渠道文本广播

**Files:**
- Modify: `internal/agent/loop.go`（两处压缩块内）

- [ ] **Step 1: 压缩时发 IM 文本**

在 Task 1 加的 notice 块里，emit IM outbound。messageBus 发 OutboundMessage：

```go
// 仅对 IM 渠道发文本（web session 走 SSE，不发 IM）
if msg.Channel != "web" && isIMChannel(msg.Channel) && a.messageBus != nil {
	a.messageBus.Publish(bus.OutboundMessage{
		Channel:   msg.Channel,
		AccountID: msg.AccountID,
		ChatID:    msg.ChatID,
		Text:      text,
	})
}
```

`isIMChannel` 已存在（loop.go，判断 wechat/telegram/discord/slack/line/feishu）。`bus.OutboundMessage` 字段名确认（`grep -n "type OutboundMessage" internal/bus/`）。`msg` 是当前 InboundMessage，在作用域内（HandleMessage 参数）。

发送时机：在 `a.maybeExtractSummary` 后、ReAct loop 前——保证 IM notice 在 agent 回复之前到。dispatcher 串行投递保证顺序。

- [ ] **Step 2: 两处都加（流式路径 2957 块内同样）**

流式路径的 `msg` 变量名确认（HandleMessageStream 的 inbound）。

- [ ] **Step 3: build + smoke（需 IM 渠道配置）**

```bash
go build ./...
# 若有 telegram/wechat 配置，触发压缩，确认 IM 收到 "📝 上下文已自动压缩..." 且在 agent 回复前
```
（无 IM 配置时，仅确认 web 路径不误发——`msg.Channel != "web"` 守卫。）

- [ ] **Step 4: commit**

```bash
git add internal/agent/loop.go
git commit -m "feat(agent): broadcast compaction notice to IM channels"
```

---

## Phase 3 完成标准

- `go build ./...` + `go test ./internal/agent/` 全过。
- 压缩触发后：web 对话中间出现持久化 notice 气泡（刷新仍在）；IM 渠道收到文本（agent 回复前）。
- notice 不进 LLM（llm-dump.log 不含提示文本）。
- 三档统计正确（before/after 来自 CompactResult）。

---

## 三 Phase 总完成标准

Phase 1/2/3 全部完成后，端到端：
- models 页配模型 → ContextWindow 自动填（表/获取列表）
- context 页选档位 → 压缩阈值按模型动态算
- 长对话触发压缩 → web/IM 显示带统计提示
- ContextWindow 未知的老 agent → 回退 80000，不破坏
