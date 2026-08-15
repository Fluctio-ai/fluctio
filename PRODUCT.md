# Product

## Register

product

## Users

单用户部署者本人（owner）：懂技术的开发者，本地/自有服务器跑一个单二进制 AI Agent 运行时，每天用 Web 控制台与 agent 对话、管理模型/技能/通道/知识库/工作流。主用桌面浏览器，同时要求移动端可用（已做 master-detail 适配）。界面语言以中文为主（完整 i18n）。

## Product Purpose

Fluctio 是「Agent 工厂」：创建、管理、运行 AI agent。Web 控制台是它的驾驶舱——chat 是最高频入口，其次是 agent 管理、KB/Wiki/记忆、工作流、cron、诊断与设置。成功标准：常用路径零摩擦，owner 一眼能找到正在运行的东西和刚发生的事。

## Brand Personality

克制、安静、工具感。Geist 血统：纯中性灰阶 + 单一波蓝 `#1890FF` 色轴，三档圆角家族（6/12/16px），信息密度优先，动效少而快。

## Anti-references

- AI 紫渐变风、生成式落地页审美
- SaaS 营销化大卡片、渐变 hero、玻璃拟态
- 花哨动效、装饰性颜色（颜色只作为信号，不作装饰）
- 过度留白稀释信息密度

## Design Principles

1. **密度优先于留白**：这是工具不是杂志；紧凑但有节奏，成组元素贴紧、组间留白充分。
2. **单一蓝色轴**：波蓝只用于主操作/焦点/品牌；状态色仅表状态。
3. **Geist 家族一致性**：中性灰阶表面分层、三档圆距、hairline 边框，不发明新家族。
4. **常用路径零摩擦**：chat 与日常管理路径上的每次点击、每层嵌套都要挣得自己的位置。
5. **中文排版清晰**：中文为先的字号/行高/标点处理，CJK 换行不破版。

## Accessibility & Inclusion

正文对比度 ≥4.5:1；尊重 `prefers-reduced-motion`；键盘可达性依托 shadcn/Base UI 原语；触控目标 ≥44px（移动端）。
