package agent

import "fmt"

// slashDefaultLang is the fallback UI language for slash-command replies
// when bus.InboundMessage.Lang is empty (IM channels, cron, legacy callers
// that don't forward the web client's i18n locale). Chinese because this
// is a single-user install whose operator is a Chinese speaker; when in
// doubt, slash replies render in Chinese.
const slashDefaultLang = "zh-CN"

// slashEntry holds the English + Simplified-Chinese copies of one
// user-facing slash-command reply string.
type slashEntry struct {
	en   string
	zhCN string
}

// slashTexts is the catalog of every string a slash command can emit to
// the chatter. Add a key here when you add a new reply, then reference it
// from slash.go via slashT / slashTf. Placeholders (%s %d %v %t %.1f) must
// match 1:1 between the two languages — slashTf runs fmt.Sprintf on the
// localized template.
var slashTexts = map[string]slashEntry{
	// ── access control ──
	"admin.denied": {
		"🔒 `%s` is owner/admin only. Ask the owner to add your platform user ID to the `admins.%s` list in agent.json (use `/whoami` to find your ID).",
		"🔒 `%s` 只有 agent owner / admin 能用。让 owner 把你的 platform 用户 ID 加进 agent.json 的 `admins.%s` 里（用 `/whoami` 查自己的 ID）。",
	},

	// ── conversation ──
	"start.greeting": {
		"👋 Hi! I'm %s, your AI assistant.\n\nJust send me a message to chat. Use /help to see available commands.",
		"👋 你好！我是 %s，你的 AI 助手。\n\n直接发消息就能和我聊天。输入 /help 查看可用命令。",
	},
	"new.done": {
		"🔄 New session started. Previous conversation kept as history.",
		"🔄 已开启新会话。之前的对话保留在历史记录里。",
	},
	"retry.empty": {
		"No previous message to retry.",
		"没有可重试的上一条消息。",
	},
	"retry.doing": {
		"🔁 Retrying: *%s*",
		"🔁 重试：*%s*",
	},
	"undo.nothing": {
		"Nothing to undo.",
		"没有可撤销的内容。",
	},
	"undo.done_turn": {
		"↩️ Undid last turn.",
		"↩️ 已撤销上一轮对话。",
	},
	"undo.done_action": {
		"↩️ Undid last action.",
		"↩️ 已撤销上一步操作。",
	},
	"compact.empty": {
		"No messages to compact.",
		"没有可压缩的消息。",
	},
	"compact.error": {
		"Compaction error: %v",
		"压缩出错：%v",
	},
	// ── runtime errors ──
	// Returned to the chatter when the LLM call itself fails after all
	// retries — the agent has no model output to relay, so this string IS
	// the user-visible reply. Localized so IM chatters (who don't pick the
	// language per-message) see it in the agent's configured language
	// instead of the hardcoded English fallback that used to ship.
	"error.processing_failed": {
		"Sorry, I ran into a problem handling this request. Please try again in a moment.",
		"抱歉，处理这条请求时遇到了问题，请稍后重试。",
	},
	"error.plan_failed": {
		"Sorry, I couldn't draft the plan — the model call failed.",
		"抱歉，起草计划时失败了——模型调用出错。",
	},
	"compact.done": {
		"✅ Compacted: %d → %d messages.",
		"✅ 已压缩：%d → %d 条消息。",
	},
	"compact.skip": {
		"Session is within limits, no compaction needed.",
		"会话在限制范围内，无需压缩。",
	},

	// ── auth (/yes /no) ──
	"auth.no_session": {
		"⚠️ No active session found.",
		"⚠️ 找不到当前会话。",
	},
	"auth.nothing_pending_yes": {
		"⚠️ No operation is awaiting authorization.",
		"⚠️ 当前没有等待授权的操作。",
	},
	"auth.nothing_pending_no": {
		"🚫 Denied — no operation was waiting, but your refusal is noted.",
		"🚫 已拒绝，当前没有待执行的操作。",
	},
	"auth.approved": {
		"✅ Approved %d operation(s), executing now.",
		"✅ 已授权 %d 个操作，立即执行…",
	},
	"auth.denied": {
		"🚫 Denied %d operation(s).",
		"🚫 已拒绝 %d 个操作。",
	},

	// ── auth mode (/auto /yolo /ask) ──
	"authmode.set": {
		"🔧 Auth mode set to `%s`.\n%s",
		"🔧 授权模式已切到 `%s`。\n%s",
	},
	"authmode.desc.ask": {
		"outside-workspace writes will prompt you (/yes to approve, /no to deny)",
		"workspace 外的写操作会先问你（/yes 授权，/no 拒绝）",
	},
	"authmode.desc.auto": {
		"outside-workspace writes are auto-denied (no prompt)",
		"workspace 外的写操作自动拒绝（不询问）",
	},
	"authmode.desc.yolo": {
		"everything is allowed (use with caution)",
		"全部放行（注意风险）",
	},

	// ── model & info ──
	"model.current": {
		"Current model: `%s`\n\nUsage: /model <model-name>\nExample: /model gpt-4o-mini",
		"当前模型：`%s`\n\n用法：/model <模型名>\n示例：/model gpt-4o-mini",
	},
	"model.switched": {
		"🤖 Model switched: `%s` → `%s`",
		"🤖 模型已切换：`%s` → `%s`",
	},
	"version": {
		"⚡ Fluctio\nAgent: %s\nModel: %s",
		"⚡ Fluctio\nAgent：%s\n模型：%s",
	},
	"whoami": {
		"Channel: `%s`\nYour user ID: `%s`\nSender name: `%s`\n\n(Add this ID to `admins.%s` in the agent config to grant write-slash access.)",
		"渠道：`%s`\n你的用户 ID：`%s`\n发送者名称：`%s`\n\n（把这个 ID 加到 agent 配置的 `admins.%s` 里，即可获得写权限 slash 命令的使用权。）",
	},

	// ── bookmark (/bookmark) ──
	"bookmark.no_store": {
		"⚠️ Bookmark storage isn't configured for this agent.",
		"⚠️ 本 agent 未启用书签存储。",
	},
	"bookmark.usage": {
		"🔖 Usage:\n  /bookmark <url> [note…] — save a URL (fetches the page body)\n  /bookmark list — list recent bookmarks",
		"🔖 用法：\n  /bookmark <链接> [备注…] — 保存链接（自动抓取正文，防失效）\n  /bookmark list — 列出最近的书签",
	},
	"bookmark.saved": {
		"🔖 Saved: %s\nTitle: %s%s\nid: %s",
		"🔖 已收藏：%s\n标题：%s%s\nid：%s",
	},
	"bookmark.body_ok": {
		"\nBody: %d chars fetched",
		"\n正文：已抓取 %d 字符",
	},
	"bookmark.body_skip": {
		"\nBody: not fetched (link saved as-is)",
		"\n正文：未抓取（仅保存链接）",
	},
	"bookmark.error": {
		"⚠️ Bookmark error: %v",
		"⚠️ 书签出错：%v",
	},
	"bookmark.list_empty": {
		"No bookmarks yet.",
		"还没有书签。",
	},
	"bookmark.list_header": {
		"🔖 %d bookmark(s):\n",
		"🔖 %d 条书签：\n",
	},

	// ── status ──
	"status": {
		"⚡ Fluctio Status\n" +
			"─────────────────\n" +
			"Agent:       %s\n" +
			"Model:       %s\n" +
			"Personality: %s\n" +
			"Max Tokens:  %d\n" +
			"Temperature: %.1f\n" +
			"Max Iter:    %d\n" +
			"Session Msgs:%d\n" +
			"Memory:      %d lines\n" +
			"Workspace:   %s",
		"⚡ Fluctio 状态\n" +
			"─────────────────\n" +
			"Agent：        %s\n" +
			"模型：         %s\n" +
			"人格：         %s\n" +
			"最大 Token：   %d\n" +
			"温度：         %.1f\n" +
			"最大迭代：     %d\n" +
			"会话消息数：   %d\n" +
			"记忆：         %d 行\n" +
			"工作区：       %s",
	},

	// ── usage ──
	"usage.session": {
		"📊 Session Usage\n" +
			"User turns:      %d\n" +
			"Assistant turns: %d\n" +
			"Tool calls:      %d\n" +
			"Total messages:  %d",
		"📊 会话用量\n" +
			"用户轮次：     %d\n" +
			"助手轮次：     %d\n" +
			"工具调用：     %d\n" +
			"总消息数：     %d",
	},
	"usage.billing.quota": {
		"💳 Billing Usage\n" +
			"Billing user:   %s\n" +
			"Tokens:         %d / %s\n" +
			"Requests:       %d / %s\n" +
			"Remaining:      %s tokens, %s requests\n" +
			"Allowed:        %t\n" +
			"Resets at:      %s",
		"💳 用量计费\n" +
			"计费用户：    %s\n" +
			"Token：       %d / %s\n" +
			"请求数：      %d / %s\n" +
			"剩余：        %s token，%s 次请求\n" +
			"是否允许：    %t\n" +
			"重置时间：    %s",
	},
	"usage.billing.unlimited": {
		"💳 Billing Usage\n" +
			"Billing user:   %s\n" +
			"Tokens:         %d used in last 30 days\n" +
			"Requests:       %d in last 30 days\n" +
			"Quota:          unlimited / not configured",
		"💳 用量计费\n" +
			"计费用户：    %s\n" +
			"Token：       过去 30 天用了 %d\n" +
			"请求数：      过去 30 天 %d 次\n" +
			"配额：        无限 / 未配置",
	},
	"usage.cost": {
		"─────────────────\n" +
			"Cost:            %s\n" +
			"Input tokens:    %v\n" +
			"Output tokens:   %v\n" +
			"API duration:    %vms\n" +
			"Tool duration:   %vms",
		"─────────────────\n" +
			"花费：          %s\n" +
			"输入 token：    %v\n" +
			"输出 token：    %v\n" +
			"API 耗时：      %vms\n" +
			"工具耗时：      %vms",
	},
	"usage.unlimited": {"unlimited", "无限"},

	// ── insights ──
	"insights": {
		"🔍 Insights (last %d days)\n" +
			"─────────────────────────\n" +
			"Log files:       %d total, %d recent\n" +
			"Memory file:     %s\n" +
			"Workspace:       %s\n\n" +
			"Tip: Use /status for session info, /usage for token stats.",
		"🔍 洞察（过去 %d 天）\n" +
			"─────────────────────────\n" +
			"日志文件：     共 %d 个，%d 个近期\n" +
			"记忆文件：     %s\n" +
			"工作区：       %s\n\n" +
			"提示：用 /status 查看会话信息，/usage 查看 token 统计。",
	},
	"insights.memfile.notfound": {"not found", "未找到"},
	"insights.memfile.fmt": {
		"%.1f KB, updated %s",
		"%.1f KB，更新于 %s",
	},

	// ── personality ──
	"personality.empty": {
		"No personality presets found.\n\nCreate files named SOUL-<name>.md in your workspace to add presets.\nExample: SOUL-assistant.md, SOUL-dev.md",
		"未找到人格预设。\n\n在工作区里创建 SOUL-<名称>.md 文件即可添加预设。\n示例：SOUL-assistant.md、SOUL-dev.md",
	},
	"personality.list_header": {
		"🎭 Personalities\n─────────────────\n",
		"🎭 人格列表\n─────────────────\n",
	},
	"personality.current_mark": {" ← current", " ← 当前"},
	"personality.usage_line": {
		"\nUsage: /personality <name>",
		"\n用法：/personality <名称>",
	},
	"personality.not_found": {
		"Personality '%s' not found.\nExpected: %s",
		"找不到人格 '%s'。\n预期路径：%s",
	},
	"personality.read_err": {
		"Error reading personality: %v",
		"读取人格出错：%v",
	},
	"personality.write_err": {
		"Error applying personality: %v",
		"应用人格出错：%v",
	},
	"personality.set_done": {
		"🎭 Personality set to: **%s**\nSOUL.md updated. Takes effect on the next message.",
		"🎭 人格已切换为：**%s**\nSOUL.md 已更新，下一条消息生效。",
	},

	// ── plan ──
	"plan.usage": {
		"Usage: `/plan <task>`",
		"用法：`/plan <任务>`",
	},
	"plan.bus_full": {
		"Bus full, try again.",
		"消息队列已满，请重试。",
	},
}

// slashHelpText is the /help body. Kept out of the slashTexts map because
// it's a large multi-line block; slashT special-cases the "help" key.
var slashHelpText = slashEntry{
	en: `⚡ Fluctio Commands

Conversation
  /new, /reset    — Clear session history
  /retry          — Re-run last message
  /undo           — Undo last turn

Context
  /compact        — Compress context window
  /status         — Agent status & memory info
  /usage          — Session token/turn stats
  /insights [N]   — Activity insights (last N days, default 7)
  /bookmark <url> — Save a URL as a bookmark (fetches the page body)
  /bookmark list  — List recent bookmarks

Personality & Model
  /personality        — List available personalities
  /personality <name> — Switch personality (SOUL-<name>.md)
  /model <name>       — Switch LLM model

Goal (persistent multi-turn objective)
  /goal <objective> — Create a goal; agent self-continues until done
  /goal             — Show current goal status
  /goal pause       — Pause continuation
  /goal resume      — Resume a paused goal
  /goal clear       — Delete the goal

Plan
  /plan <task>      — Run <task> in plan mode: emit a numbered plan, no tool calls

Info
  /help           — Show this help
  /version        — Show version
  /whoami         — Show your platform user ID

🔒 Agent-wide write commands (/undo /retry /compact /model /personality)
   and group-chat /new or /reset are restricted to the agent owner + admins
   listed in agent.json's "admins" field. Private-chat /new and /reset are
   available to the chatter. Use /whoami to find your ID.`,
	zhCN: `⚡ Fluctio 命令

对话
  /new, /reset    — 清空会话历史
  /retry          — 重跑上一条消息
  /undo           — 撤销上一轮

上下文
  /compact        — 压缩上下文窗口
  /status         — Agent 状态与记忆信息
  /usage          — 会话 token / 轮次统计
  /insights [N]   — 活动洞察（过去 N 天，默认 7）
  /bookmark <链接> — 收藏链接（自动抓取正文，防失效）
  /bookmark list  — 列出最近的书签

人格与模型
  /personality        — 列出可用人格
  /personality <名称> — 切换人格（SOUL-<名称>.md）
  /model <名称>       — 切换 LLM 模型

目标（持续多轮的目标）
  /goal <目标>     — 创建目标；agent 会自动续跑直到完成
  /goal            — 查看当前目标状态
  /goal pause      — 暂停续跑
  /goal resume     — 恢复已暂停的目标
  /goal clear      — 删除目标

计划
  /plan <任务>     — 在计划模式下运行 <任务>：输出编号计划，不调用工具

信息
  /help           — 显示本帮助
  /version        — 显示版本
  /whoami         — 显示你的 platform 用户 ID

🔒 Agent 级写命令（/undo /retry /compact /model /personality）以及群聊里的
   /new 或 /reset 仅限 agent owner + agent.json "admins" 里列出的管理员使用。
   私聊里的 /new 和 /reset 对当前用户开放。用 /whoami 查你的 ID。`,
}

// slashT returns the localized string for key. lang is bus.InboundMessage.Lang
// ("en" selects English; anything else — including "" and "zh-CN" — selects
// Chinese, the default for this install). An unknown key returns the key
// itself so a missing translation surfaces visibly rather than silently
// rendering empty.
func slashT(lang, key string) string {
	if lang == "" {
		lang = slashDefaultLang
	}
	if key == "help" {
		if lang == "en" {
			return slashHelpText.en
		}
		return slashHelpText.zhCN
	}
	e, ok := slashTexts[key]
	if !ok {
		return key
	}
	if lang == "en" {
		return e.en
	}
	return e.zhCN
}

// slashTf is slashT + fmt.Sprintf on the localized template.
func slashTf(lang, key string, args ...any) string {
	return fmt.Sprintf(slashT(lang, key), args...)
}
