# FastClaw → Fluctio 改名设计

- 日期：2026-07-07
- 状态：已确认，待生成实现计划
- 范围决策：方案 B（全量改名，不做向后兼容）

## 1. 背景与目标

把项目 `fastclaw` 全量改名为 `fluctio`。当前 `fastclaw` 共 1032 处、分布在 285 个文件中（module path、二进制、目录、UI、文档、数据目录、环境变量、基建、skills 前缀等）。

GitHub org `Fluctio-ai` 已创建（URL 大小写不敏感，module path 用全小写 `fluctio-ai` 即可解析）。org 暂不上传代码，module path 仅作本地标识符，将来上传零改动对得上。

## 2. 范围与非目标

**范围**：全量替换所有 `fastclaw`/`FastClaw`/`FASTCLAW` 出现。

**非目标（YAGNI，明确不做）**：
- 向后兼容层、旧名回退
- `fastclaw` 命令别名/symlink
- 数据目录自动迁移或启动迁移提示（用户手动处理 `~/.fastclaw` → `~/.fluctio`）
- 旧环境变量回退（`FASTCLAW_*` 直接改，不留兼容）

## 3. 新旧命名映射表

| 项 | 旧 | 新 |
|---|---|---|
| Go module path | `github.com/fastclaw-ai/fastclaw` | `github.com/fluctio-ai/fluctio` |
| ldflags buildinfo 注入路径 | `…/fastclaw/internal/buildinfo` | `…/fluctio/internal/buildinfo` |
| 二进制 / CLI 命令 | `fastclaw` | `fluctio` |
| 入口目录 | `cmd/fastclaw/` | `cmd/fluctio/` |
| 产品展示名（README/UI/alt/prompt） | `FastClaw` | `Fluctio` |
| 用户数据目录 | `~/.fastclaw/` | `~/.fluctio/` |
| 数据库文件 | `fastclaw.db` | `fluctio.db` |
| 环境变量前缀 | `FASTCLAW_*` | `FLUCTIO_*` |
| 内置 skills 前缀 | `fastclaw-api-integration`、`fastclaw-skill-learner`、`fastclaw-skill-guide` | `fluctio-api-integration`、`fluctio-skill-learner`、`fluctio-skill-guide` |
| 插件 demo 目录 | `plugins/fastclaw-plugin-demo/` | `plugins/fluctio-plugin-demo/` |
| Helm chart 目录 | `deploy/helm/fastclaw/` | `deploy/helm/fluctio/` |
| k8s 部署文件 | `deploy/k8s/fastclaw.yaml` | `deploy/k8s/fluctio.yaml` |
| Docker 镜像/容器名 | `fastclaw` | `fluctio` |
| GitHub owner（占位） | `fastclaw-ai` | `fluctio-ai` |

大小写规则：二进制、目录、module path、环境变量一律小写 `fluctio`（Go/Unix 惯例）；产品展示名 `Fluctio`（仅首字母大写）。

## 4. 执行策略：分层切片

按编译依赖关系分 6 层，每层独立成一个 `git stash` → 改 → `go build ./...` → `go test ./...` → smoke → `git stash drop` 周期，并独立 commit。失败立即 `git checkout .` 回滚，换策略重试（每层最多 3 次尝试，遵循 CLAUDE.md）。

理由：符合用户的小步验证+回滚规矩；出错可定位到层；module path 层虽内部是大改，但整层一起验证，编译不过就整体回滚。

## 5. 分层清单

### L1 · Go module path（原子层，必须一次改完）

- `go.mod` 的 `module` 行
- 所有 `.go` 文件中 `github.com/fastclaw-ai/fastclaw/...` import 路径
- `Makefile` 中 `BUILDINFO` 变量路径
- `.goreleaser.yaml` 中 ldflags 的 buildinfo 路径

**验证**：`go build ./...` + `go test ./...`（对照 baseline，见 §7）

### L2 · 二进制名 + 入口目录

- 目录重命名 `cmd/fastclaw/` → `cmd/fluctio/`
- `Makefile`：`bin/fastclaw`→`bin/fluctio`、`install` target、`release-local` 各平台产物路径与归档名
- `.goreleaser.yaml`：`project_name`、`builds.id`、`builds.main`、`builds.binary`、`release.github` owner/name
- `install.sh`

**验证**：`go build ./cmd/fluctio` + `make build`

### L3 · 产品展示名（UI + 文档 + prompt）

- `web/src/**` 的 `FastClaw`：i18n（`locales/zh-CN.ts`、`locales/en.ts`、`i18n.tsx`）、components、pages
- `README.md`、`docs/*.md`
- agent runtime 给 LLM 的 prompt 中 `FastClaw`（如 `internal/agent/skills.go` 等，共 `internal/agent/skills.go` 27 处需逐一核对）

**验证**：`make build-web` + `go build`（`internal/setup/web` embed 必须重建）

### L4 · 运行时标识（破坏性）

- 数据目录 `~/.fastclaw` → `~/.fluctio`（`internal/config/config.go`、`env.go`、`internal/store/*` 等）
- 数据库文件名 `fastclaw.db` → `fluctio.db`
- 环境变量 `FASTCLAW_*` → `FLUCTIO_*`（`internal/config/env.go`）

**验证**：`go test ./...` + 启动二进制确认 `~/.fluctio/` 与 `fluctio.db` 生成

### L5 · 基建

- `Dockerfile`
- `deploy/docker/`（docker-compose.yml、sandbox 子目录）
- `deploy/helm/fastclaw/` → `deploy/helm/fluctio/`（Chart.yaml、values.yaml、templates/* 全套，含 `_helpers.tpl` 中的 `fastclaw` 引用）
- `deploy/k8s/fastclaw.yaml` → `fluctio.yaml`（54 处）、`deploy/k8s/postgres.yml`、`namespace.yml`
- `scripts/release.sh`、`scripts/dev-build.sh`
- `.github/workflows/release.yml`、`docker.yml`

**验证**：`helm lint deploy/helm/fluctio`（可选 `docker build`）

### L6 · skills 前缀 + 插件 + 杂项

- `skills/fastclaw-api-integration/`、`skills/fastclaw-skill-learner/`、`skills/fastclaw-skill-guide/` 目录重命名 + 其 `SKILL.md` 内引用
- `internal/agent/bundled_skills/` 下对应副本（由 `make bundle-skills` 同步，注意源在 repo-root `skills/`）
- `plugins/fastclaw-plugin-demo/` → `plugins/fluctio-plugin-demo/`（含 `plugin.json`、`plugin.py`）
- `tools/openclaw-plugin-bridge/`（`README.md`、`package.json` 中残留）
- `.gitignore`、`restart*.log`（如需保留则改内部引用，否则忽略）

**验证**：`go build ./...` + `make bundle-skills`

## 6. 决策记录

1. **环境变量**：`FASTCLAW_*` → `FLUCTIO_*`，直接改，不留旧名回退。用户本地 `.env`/启动脚本自行更新。
2. **数据目录迁移**：不做自动迁移，不加启动提示。用户手动 `~/.fastclaw` → `~/.fluctio`。
3. **命令别名**：彻底不留 `fastclaw` → `fluctio` 的 symlink/alias。
4. **git history**：目录重命名与内容修改合并在该层 commit 中，不单独拆 rename commit（机械改名不值得保 history 的额外复杂度）。

## 7. 验证策略

**每层**：`go build ./...` → `go test ./...` → smoke（启动二进制 + `curl /health`）。

**baseline（已知 pre-existing 失败，非改名引入）**：
- `internal/agent/tools` 下 4 个 Windows-path 相关测试（见 memory `fastclaw-build-quirks`）。改名后 `go test` 结果与之对照，新增失败才算回归。
- `.fastclaw1` 多副本 DB、`internal/setup/web` 旧 embed 等开发环境历史遗留，不影响代码改名。

**收尾全量验证**：L6 完成后跑一次完整 `go build ./...` + `go test ./...` + `make build`（含 `build-web` + `bundle-skills`），确认产物名为 `fluctio`。

## 8. 风险点

- **大小写变体遗漏**：替换必须同时覆盖 `FastClaw`（驼峰）/ `fastclaw`（小写）/ `FASTCLAW`（环境变量）。用大小写不敏感匹配 + 人工核对，避免漏 `FASTCLAW_`。
- **`internal/setup/web` embed**：L3 改前端文案后必须 `make build-web` + 重编 exe，否则 UI 不生效。
- **prompt 内产品名**：agent system prompt 中 `FastClaw` 改后，模型"自我认知"微变，通常无害。
- **字符串误伤**：`github.com/fastclaw-ai/fastclaw` 批量替换会同时命中注释/文档中的引用——这正是期望行为；需确认无第三方包路径恰好包含该子串。
- **Windows 替换语法**：批量替换用 PowerShell 语法（用户主环境），注意 `-LiteralPath` 与正则转义。

## 9. 后续

本 spec 确认后，转 `writing-plans` skill 生成分步实现计划（按 L1–L6 拆成可执行步骤，每步含具体命令与验证）。
