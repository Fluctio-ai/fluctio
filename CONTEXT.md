# Context — 术语表

本文件只记录"一个词在 FastClaw 里指什么"，不记录实现细节、规格或决策理由（决策另见 `docs/adr/`）。随设计推进逐条增补。

## Workflow

预先编排好的、固定的多步骤执行流程：每个步骤（见 Node）承接上一步输出、执行固定动作、产出确定结果，流程按预定路径推进，并可在节点处分支。与 agent loop 的根本区别在于——**路径是编排出来的，不依赖 LLM 临场规划每一步**。设计目标首先是可控与可复现，token 节约为副产品；可被定时任务、LLM 或用户手动触发。

_Avoid_: 把它等同于 agent loop；等同于 CI pipeline。

## Node

Workflow 中的一个步骤：承接上一步输出，执行一个固定动作，产出结果传给下一步。

_Avoid_: "tool"——tool 指 agent 工具系统中的能力单元；Node 是 Workflow 内的步骤，可能内部调用一个 tool，也可能不是，两者不等同。

## Workflow Definition

一个 workflow 的编排模板，以 YAML（唯一真源）描述节点、边、input schema，带不可变 version。改定义 = 发新版本；运行时由 runner 按某版本加载执行。

_Avoid_: 把它和一次执行实例混为一谈——"workflow"一词既可指模板（定义）也可指一次执行，需区分。

## Workflow Run

一次 workflow 执行的实例：给定一个 Workflow Definition 的某版本 + input，由 runner 跑出一条执行轨迹，产出 ExecutionResult；节点输出与状态持久化在 SQLite（`workflow_runs` / `workflow_node_outputs`）。失败后从失败点续跑（run_id 不变，追加节点尝试）。三种触发源（定时/LLM/手动）各产生 run；run 可选归属某 session 与 owner。

_Avoid_: 把它和 Workflow Definition（模板）混为一谈。
