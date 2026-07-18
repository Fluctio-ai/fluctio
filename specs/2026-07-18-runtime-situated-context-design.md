# 运行时处境层（Runtime Situated Context）设计

- 日期：2026-07-18
- 状态：设计完成，待用户审阅 → writing-plans
- 触发场景：session `s-1784355741022-nv26q9`（黔西南兴义暑期亲子游信息图）
- 参考项目：Hermes Agent（Nous Research）

---

## 1. 背景与问题

### 1.1 症状
session `s-1784355741022-nv26q9`（67 条消息、8 轮用户输入，其中 6 轮在催促/纠错）暴露两个问题：

- **文件路径管理混乱**：助手在 workspace 路径（`C:\Users\mumu\.fluctio\workspaces\agt_…\`）、桌面绝对路径（`D:\backup\Desktop\`）、`.playwright-mcp\`、`~/.openclaw/` 四套路径间反复横跳，用户在前端永远看不到产物。
- **对话频繁中断**：助手反复"假完成"（贴坏图就宣布完成🎉），必须用户多次催促才推进；skill 后处理步骤（`bash ~/.openclaw/…/post-process.sh`）因 Windows 缺 bash、且 `.openclaw` 改名遗留路径不存在，无法执行，助手偏离 skill 流程。

### 1.2 根因
两个症状同源：**LLM 戴着黑布套操作**——它知道自己下了什么指令，但看不见操作真实落点、对用户的可见性、环境的隐形墙。工具结果"报喜不报忧"（写成功就返回成功，不告知"该路径前端不可见"），LLM 信息不足无法主动迁移，只能撞墙后停下等用户推。

### 1.3 设计原则
**不做点状修复**（挨个工具/场景写可见性代码——永远弄不完），而建**通用运行时处境层**：系统化把 harness 知道但 LLM 不知道的处境暴露给 LLM，让它对一整类问题自决策。截图、image_gen、未来任何产物工具都自动被机制覆盖。

---

## 2. 对 Hermes 的借鉴与超越

研究 Hermes Agent 得出三类可借鉴机制，以及 fluctio 特有的一项超越：

- **借鉴**：分层 system prompt（`stable → context → volatile`）+ prompt stability 不变量（对话内不突变，命中 prefix cache）+ 工具结果原样 error-wrapped 回流（**不做** per-result 通用注解）+ skill 条件可见性（声明依赖，不满足则隐藏/给 fallback）+ 自改进环（`skill_manage`）。
- **超越**：Hermes 是纯 agent，无"前端可见域"概念。fluctio 有 workspace scope 硬约束——产物落可见域外用户即瞎。此约束纯靠 system prompt 讲规则不可靠（session 铁证：LLM 记不住），故在 Hermes 基础上保留**一个最小增强**：对落盘型产物触发一次可见性判定。整体 **90% 学 Hermes，10% fluctio 特需**。

---

## 3. 设计

### 3.1 载体、时序与预算（方案 F：Hermes + 最小增强）

| 载体 | 形式 | 时机 |
|---|---|---|
| 分层 system prompt 处境层 | **stable**（OS/能力/scope 边界，对话不变，缓存友好）+ **volatile**（当前 skill 依赖状态/本轮产物，每轮换） | prompt 组装时 |
| 工具结果（成功） | 原样 error-wrapped 回流，**不贴通用注解** | 结果回流时 |
| 落盘可见性裁决（唯一增强） | 仅落盘型产物触发，`isWorkspacePath` 判定，不可见则该次结果追加一行裁决 | 落盘工具结果回流时 |
| 失败分类 | error-wrapping 时附带类别 + recovery options | 工具失败时 |

**与现有 prompt 的关系**：处境层是**事实层**（环境是什么、边界在哪），与 skill 指令/系统 prompt 的**策略层**（该怎么做）正交。

**砍除（YAGNI）**：产物账本（对话历史已记录 turn 内产物）；步骤级进度跟踪（学 Hermes 不做）。

### 3.2 采集机制

**A. Stable 处境**：聚合 `runtime.GOOS`/shell、能力清单（bash 可用性、各 MCP server 名+其 cwd/写入域、沙箱开关）、scope 边界（`scopeFor` 给的可见根/可写根）。把散落各处的隐式事实聚合成一段 LLM 可读清单。

**B. Skill 声明依赖 + 条件可见性（学 Hermes）**：
- frontmatter 扩展字段：`platforms`、`requires_tools`、`requires_commands`、`requires_mcp`、`on_missing`（内联 fallback 指令）。
- 加载时对照 stable 能力清单：满足 → 加载原 skill；不满足 + 有 `on_missing` → 加载 fallback 指令；不满足 + 无 `on_missing` → 隐藏 + volatile 层提示。
- 砍步骤跟踪。
- 注：fluctio 与 Hermes 同用 agentskills.io 标准（SKILL.md + YAML frontmatter），扩展天然兼容。

**C. 落盘产物检测（工具无关，方案 iii = 声明 gating + diff 捕获）**：
- 工具注册声明副作用类型枚举：`writes_file` / `emits_inline` / `external` / `pure`。
- `writes_file` 类：执行前后对可见根（`registry.UserRoot()`）做轻量 entries diff，捕获**实际**落盘路径（覆盖工具写多文件、路径与声称不符）。
- 捕获 → `isWorkspacePath` 判可见性 → 不可见 → 该次结果追加：
  `[产物 <path> 不在可见域 <visibleroot>；可调 deliver_file 投递]`
- `pure` 类（read_file/list_dir）跳过 diff 省开销。
- **新增任何 writes_file 工具零代码纳入**——这是不退化为打地鼠的根本保证。

**D. 失败分类（error-wrapping 附带，不违背 F）**：
- 分类：`env_missing` / `permission` / `reachability` / `external`(503) / `logic`。
- 每类挂一组 recovery options。
- 形式：error 文本前置一行 `[类别: env_missing] [可恢复: 用 powershell 替代 / 退避重试 / deliver_file]`。
- 成功结果原样回流（F），仅失败结果结构化。

### 3.3 接入点

| # | 接入点 | 落位 | 时机 |
|---|---|---|---|
| 1a | stable 处境层 | system prompt builder（待定位） | prompt 组装时 |
| 1b | volatile 处境层 | 同上 + `RuntimeContext` 对象 | 每轮组装 |
| 2 | 落盘检测 + 可见性裁决 | `loop.go:2666`（主路径）/ `:3398`（流式）统一出口，抽 helper 两处调；先例 `extractToolMeta`(:3524) | 工具结果回流前 |
| 2' | 工具副作用声明 | registry 注册处（`registry.go:145 ToolFunc` 附近，待精确） | 工具注册时 |
| 3 | 失败分类 | error-wrapping 处（待定位） | 工具失败时 |
| 4 | `deliver_file` 工具 | 新增 `tools/deliver.go` | LLM 调用，copy 任意路径 → 可见域 |
| 5 | skill 条件可见性 | skill loader（`load_skill` 流程，待定位） | skill 加载时 |

> 标注"待定位"的接入点在 writing-plans 阶段精确侦察；已确认：可见性判定用 `registry.isWorkspacePath`（`file.go:156`）、scope 用 `runtime.scopeFor`（`runtime.go:279`）、运行时可见根用 `registry.UserRoot()`。

---

## 4. 分阶段路线图

### 阶段 1 — PoC（验证 F 理念是否真生效）⭐
- **范围**：接入点 1a（stable 处境层）+ 2（落盘检测/可见性）+ 4（deliver_file）。
- **验证方法**：新建 session 重放 article-to-image 同任务。
- **成功标准**：
  1. LLM 从 stable 层知道可见域 + 截图落点；
  2. `take_screenshot` 结果被标 `user_visible:false`；
  3. LLM **自主**调 `deliver_file` 迁移截图到可见域；
  4. 用户前端见图；
  5. **零催促**（原 session 8 轮催促）。
- 通过则整个方向立住；不通过则回头改机制。

### 阶段 2 — 失败分类 + skill 条件可见性
- 接入点 3（error-wrapping 分类）+ 5（skill frontmatter `requires_*`/`platforms`/`on_missing` + 加载时条件可见性）。
- 给 article-to-image 补 frontmatter，**治 Windows 不兼容根因**（不再撞墙后才知道缺 bash）。

### 阶段 3 — volatile 层 + 打磨
- 接入点 1b（volatile 处境：skill 依赖状态/本轮产物）+ 缓存优化 + 边缘情况。
- 解开放问题：MCP take_screenshot 的 cwd 是否 scope 到 session workspace（影响截图是否天然可见）。

### 阶段 4（远期，单独立项）— 自改进环
- `skill_manage` 工具（create/patch/edit/delete/write_file/remove_file）+ 四触发时机（成功完成复杂任务后/撞墙找到出路后/用户纠正后/发现非平凡工作流后）+ 写审批门控（暂存 → approve）。
- 学 Hermes 最强项：撞墙经验自动沉淀为 skill 改进。

---

## 5. 非目标（YAGNI）
- 产物账本（载体 3）——对话历史已记录 turn 内产物。
- 步骤级进度跟踪——学 Hermes 不做。
- 全量 per-result 通用注解——仅落盘产物 + 失败做最小处理。
- 自改进环——远期阶段 4，不在本 spec 实现范围。

---

## 6. 开放问题（writing-plans 阶段精确侦察）
1. system prompt builder 位置（接入点 1a/1b）。
2. error-wrapping 位置（接入点 3）。
3. skill loader 位置（接入点 5）。
4. MCP `take_screenshot` 的 cwd 是否已 scope 到 session workspace——若是，截图天然可见，阶段 1 的 deliver_file 对截图场景可能非必要（但其他落 scope 外的工具仍需要）。
