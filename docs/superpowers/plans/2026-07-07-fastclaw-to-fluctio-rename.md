# FastClaw → Fluctio 改名实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 fastclaw 全量改名为 fluctio——Go module path、所有 .go import/标识符/prompt、二进制名、入口目录、前端 UI、文档、用户数据目录、环境变量、Docker/Helm/k8s 基建、skills 前缀、插件 demo。

**Architecture:** 分 5 个 Task 逐层切片。每个 Task = 字符串批量替换（4 个大小写变体，复用 PowerShell 函数 `Invoke-Rename`，UTF-8 无 BOM）+ 目录 `git mv` + 编译/测试验证 + 独立 commit。Task 1 把所有 Go 源码 + `go.mod` 一次改完（吸收 spec 的 L1 module path + L4 运行时标识——字符串替换原子不可分），随后 Task 2-5 处理非 Go 文件与目录重命名。失败按 CLAUDE.md 回滚。

**Tech Stack:** Go 1.25.0、PowerShell（`.NET [System.IO.File]` API 保 UTF-8 无 BOM）、Make、pnpm（前端）、Helm、Docker。

## Global Constraints

（所有 Task 隐含遵守，抄自 spec §7 + CLAUDE.md）

- **CLAUDE.md 验证流**：每 Task 改动前 `git stash`（save point；工作区干净时输出 "No local changes to save" 属正常）；改后依次 `go build ./...` → `go test ./...`（→ smoke：仅 Task 5 收尾做一次启动验证；其余 Task 因改名不涉运行时逻辑，build+test 过即视为完成）；全过则 `git stash drop 2>$null`（若创建了 stash）+ `git add -A` + `git commit`；任一失败立即 `git checkout .` 回滚，分析后换策略重试，**每 Task ≤3 次**，仍失败则停下汇报。
- **环境**：Windows + PowerShell。所有替换命令为 PowerShell 语法。
- **UTF-8 无 BOM**：用 `[System.IO.File]::ReadAllText` / `WriteAllText` + `UTF8Encoding($false)`；**不要**用 `Set-Content`（PS 5.x 会加 BOM / 改编码，破坏 .go 文件）。
- **4 替换顺序**（每文件内，严格按序）：① `github.com/fastclaw-ai/fastclaw` → `github.com/fluctio-ai/fluctio` ② `FASTCLAW` → `FLUCTIO` ③ `FastClaw` → `Fluctio` ④ `fastclaw` → `fluctio`。先长后短、先大写后小写，避免短串误伤已替换的长串。
- **排除生成物**（替换与 grep 都排除）：`node_modules`、`internal/setup/web`（Task 3 `make build-web` 重建）、`internal/agent/bundled_skills`（Task 5 `make bundle-skills` 重建）、`.git`。
- **baseline**：`internal/agent/tools` 下有 4 个 pre-existing Windows-path 测试失败（非改名引入）。**Task 1 开始前**先记录基线：
  ```powershell
  go test ./internal/agent/tools/... 2>&1 | Tee-Object baseline-pre.txt
  ```
  后续 `go test ./...` 失败集与之对照，**新增**失败才算回归。
- **module path 目标**：`github.com/fluctio-ai/fluctio`（org `Fluctio-ai` 已建，URL 大小写不敏感）。

## 通用替换函数（每 Task 复用）

每个 Task 替换前，在当前 PowerShell 会话定义一次（会话内持久）：

```powershell
function Invoke-Rename {
    param([string[]]$Paths, [string[]]$Include = @('*'))
    $utf8NoBom = New-Object System.Text.UTF8Encoding $false
    $exclude = '\\(node_modules|internal[\\/]setup[\\/]web|internal[\\/]agent[\\/]bundled_skills|\.git)[\\/]'
    $files = @()
    foreach ($p in $Paths) {
        if (Test-Path $p -PathType Leaf) {
            $files += (Get-Item $p)
        } else {
            $files += (Get-ChildItem -Path $p -Recurse -Include $Include -File -ErrorAction SilentlyContinue)
        }
    }
    $files | Sort-Object FullName -Unique | Where-Object { $_.FullName -notmatch $exclude } | ForEach-Object {
        $t = [System.IO.File]::ReadAllText($_.FullName)
        $n = $t
        $n = $n -replace 'github\.com/fastclaw-ai/fastclaw', 'github.com/fluctio-ai/fluctio'
        $n = $n -replace 'FASTCLAW', 'FLUCTIO'
        $n = $n -replace 'FastClaw', 'Fluctio'
        $n = $n -replace 'fastclaw', 'fluctio'
        if ($n -ne $t) { [System.IO.File]::WriteAllText($_.FullName, $n, $utf8NoBom); Write-Host "updated: $($_.FullName)" }
    }
}
```

---

## Task 1: Go 源码 + go.mod 全量替换（spec L1 + L4）

**Depends on:** 无（首个 Task）
**Files:**
- Modify: `go.mod`（module 行）
- Modify: 仓库内所有 `*.go`（排除生成物，见 Global Constraints）
**覆盖的 spec 项:** module path、所有 import、buildinfo import、内部标识符/变量名/注释、prompt 内 `FastClaw`、数据目录 `~/.fastclaw`→`~/.fluctio`、`fastclaw.db`→`fluctio.db`、`FASTCLAW_*`→`FLUCTIO_*`。

- [ ] **Step 1: 记录测试 baseline**

```powershell
go test ./internal/agent/tools/... 2>&1 | Tee-Object baseline-pre.txt
```
预期：4 个 Windows-path 测试失败（保留 `baseline-pre.txt` 供后续对照）。

- [ ] **Step 2: 创建 save point**

```powershell
git stash
```
（工作区干净时输出 "No local changes to save"，正常。）

- [ ] **Step 3: 在会话定义 `Invoke-Rename`（见上节）**

- [ ] **Step 4: 替换所有 .go + go.mod**

```powershell
Invoke-Rename -Paths '.','go.mod' -Include '*.go'
```
预期：打印大量 `updated: ...`（约 250+ 个 .go 文件 + go.mod）。

- [ ] **Step 5: 编译验证**

```powershell
go build ./...
```
预期：无错。**若出现"undefined: fastClawXxx"等**：说明存在 `fastClaw`（小驼峰）等其他大小写变体；在 `Invoke-Rename` 中加 `$n = $n -replace 'fastClaw', 'fluctio'` 后对报错文件重跑（`Invoke-Rename -Paths '<报错文件路径>' -Include '*'`），再 build。此为该 Task 重试策略之一（≤3 次）。

- [ ] **Step 6: 测试验证（对照 baseline）**

```powershell
go test ./... 2>&1 | Tee-Object test-task1.txt
```
预期：失败集 ⊆ `baseline-pre.txt` 的 4 个 pre-existing 失败；**无新增**。

- [ ] **Step 7: 提交**

```powershell
git stash drop 2>$null
git add -A
git commit -m "refactor: rename fastclaw->fluctio in Go sources + go.mod"
```

---

## Task 2: 入口目录 + 二进制/构建产物名（spec L2）

**Depends on:** Task 1
**Files:**
- Rename: `cmd/fastclaw/` → `cmd/fluctio/`
- Modify: `Makefile`、`.goreleaser.yaml`、`install.sh`

- [ ] **Step 1: save point**

```powershell
git stash
```

- [ ] **Step 2: 定义 `Invoke-Rename`（若新会话，见通用函数节）**

- [ ] **Step 3: 替换构建文件内容**（含 `cmd/fastclaw`→`cmd/fluctio`、`bin/fastclaw`→`bin/fluctio`、`project_name`、`binary`、ldflags buildinfo path 等）

```powershell
Invoke-Rename -Paths 'Makefile','.goreleaser.yaml','install.sh' -Include '*'
```

- [ ] **Step 4: 重命名入口目录**

```powershell
git mv cmd/fastclaw cmd/fluctio
```
预期：无输出（成功）。

- [ ] **Step 5: 编译验证（新入口路径）**

```powershell
go build ./cmd/fluctio
```
预期：无错（Task 1 已改 import，此处验证目录重命名后路径解析）。

- [ ] **Step 6: 全量编译**

```powershell
go build ./...
```
预期：无错。

- [ ] **Step 7: 提交**

```powershell
git stash drop 2>$null
git add -A
git commit -m "refactor: rename binary/cmd dir fastclaw->fluctio (Makefile, goreleaser, install.sh)"
```

---

## Task 3: 前端 UI + 文档（spec L3）

**Depends on:** Task 1（前端文件里的 `fastclaw` 字符串如 API 路径需与 Task 1 改过的后端一致）
**Files:**
- Modify: `web/src/**`、`docs/**`、`README.md`

- [ ] **Step 1: save point**

```powershell
git stash
```

- [ ] **Step 2: 定义 `Invoke-Rename`**

- [ ] **Step 3: 替换前端 + 文档**

```powershell
Invoke-Rename -Paths 'web/src','docs','README.md' -Include '*.ts','*.tsx','*.md','*.js','*.jsx','*.json'
```
预期：打印 `updated: ...`（i18n locales、components、pages、docs/*.md、README.md）。

- [ ] **Step 4: 重建前端 embed**

```powershell
make build-web
```
预期：pnpm build 成功，`internal/setup/web` 被覆盖（这是 memory 记的坑——改前端后必须 `make build-web` + 重编 exe）。

- [ ] **Step 5: 编译验证（embed 一致性）**

```powershell
go build ./...
```
预期：无错。

- [ ] **Step 6: 提交**

```powershell
git stash drop 2>$null
git add -A
git commit -m "refactor: rename FastClaw->Fluctio in web UI + docs"
```

---

## Task 4: 基建（spec L5）

**Depends on:** Task 2
**Files:**
- Rename: `deploy/helm/fastclaw/` → `deploy/helm/fluctio/`、`deploy/k8s/fastclaw.yaml` → `deploy/k8s/fluctio.yaml`
- Modify: `Dockerfile`、`deploy/**`、`scripts/**`、`.github/**`、`.gitignore`

- [ ] **Step 1: save point**

```powershell
git stash
```

- [ ] **Step 2: 定义 `Invoke-Rename`**

- [ ] **Step 3: 重命名基建目录/文件**

```powershell
git mv deploy/helm/fastclaw deploy/helm/fluctio
git mv deploy/k8s/fastclaw.yaml deploy/k8s/fluctio.yaml
```

- [ ] **Step 4: 替换基建文件内容**

```powershell
Invoke-Rename -Paths 'Dockerfile','deploy','scripts','.github','.gitignore' -Include '*'
```

- [ ] **Step 5: Helm 语法验证（若装了 helm）**

```powershell
helm lint deploy/helm/fluctio
```
预期：`[INFO] Chart.yaml file is valid` + 无 error。未装 helm 则跳过，靠 Step 6。

- [ ] **Step 6: 编译验证（基建不影响 Go，确认即可）**

```powershell
go build ./...
```
预期：无错。

- [ ] **Step 7: 提交**

```powershell
git stash drop 2>$null
git add -A
git commit -m "refactor: rename fastclaw->fluctio in infra (Docker, Helm, k8s, scripts, CI)"
```

---

## Task 5: skills + 插件 + 杂项 + 收尾全量验证（spec L6）

**Depends on:** Task 3、Task 4
**Files:**
- Rename: `skills/fastclaw-api-integration`/`fastclaw-skill-learner`/`fastclaw-skill-guide` → `fluctio-*`、`plugins/fastclaw-plugin-demo` → `fluctio-plugin-demo`
- Modify: `skills/**`、`plugins/**`、`tools/openclaw-plugin-bridge/**`

- [ ] **Step 1: save point**

```powershell
git stash
```

- [ ] **Step 2: 定义 `Invoke-Rename`**

- [ ] **Step 3: 重命名 skills + 插件目录**

```powershell
git mv skills/fastclaw-api-integration skills/fluctio-api-integration
git mv skills/fastclaw-skill-learner skills/fluctio-skill-learner
git mv skills/fastclaw-skill-guide skills/fluctio-skill-guide
git mv plugins/fastclaw-plugin-demo plugins/fluctio-plugin-demo
```

- [ ] **Step 4: 替换 skills + 插件 + bridge 内容**

```powershell
Invoke-Rename -Paths 'skills','plugins','tools/openclaw-plugin-bridge' -Include '*'
```

- [ ] **Step 5: 重新同步 bundled_skills**

```powershell
make bundle-skills
```
预期：`==> bundled skills synced`（把改名后的 `skills/skill-creator`、`skills/find-skills` 拷贝到 `internal/agent/bundled_skills/`；这两个目录名不含 fastclaw，但内容若有残留会被同步覆盖）。

- [ ] **Step 6: 全量编译**

```powershell
go build ./...
```
预期：无错。

- [ ] **Step 7: 全量测试（对照 baseline）**

```powershell
go test ./... 2>&1 | Tee-Object test-task5.txt
```
预期：失败集 ⊆ baseline 4 个 pre-existing，无新增。

- [ ] **Step 8: 完整构建（产物名校验）**

```powershell
make build
```
预期：成功，产出 `bin/fluctio`（不再是 `bin/fastclaw`）。

- [ ] **Step 8.5: 启动 smoke**（CLAUDE.md 要求，收尾验证一次）

```powershell
# 后台启动刚构建的二进制；监听端口从 stdout/启动日志读取，按现有运行配置替换 <port>
Start-Process -NoNewWindow .\bin\fluctio
Start-Sleep -Seconds 3
curl.exe -s http://localhost:<port>/health
# 验证后用 Stop-Process 或任务管理器停掉测试进程
```
预期：`/health` 返回 200 / `ok`。改名不改运行时逻辑，此步仅确认二进制可正常加载配置与启动。

- [ ] **Step 9: 残留扫描**（用 Grep 工具，非 shell）

用 Grep 工具搜索 `fastclaw`（大小写不敏感，`output_mode: files_with_matches`），结果应**仅**含：
- `baseline-pre.txt`、`test-task1.txt`、`test-task5.txt`（本计划产生的日志，可删）
- `.git/` 内（git 历史，忽略）
- `internal/setup/web`（若 `make build-web` 未清干净，重跑 `make build-web`）
- `internal/agent/bundled_skills`（若残留，重跑 `make bundle-skills`）
- `restart*.log`（本地日志，非项目文件）

仓库源码中应**零残留**。若有源码残留，记下文件回到 Step 4 补替换。

- [ ] **Step 10: 提交**

```powershell
git stash drop 2>$null
git add -A
git commit -m "refactor: rename fastclaw->fluctio skills/plugins + final verification"
```

- [ ] **Step 11: 清理本地日志产物**（可选）

```powershell
Remove-Item baseline-pre.txt, test-task1.txt, test-task5.txt -ErrorAction SilentlyContinue
```

---

## 完成标准

- 5 个 commit 全部落地，`go build ./...` + `go test ./...`（失败集 = baseline 4 个）+ `make build`（产物 `bin/fluctio`）全过。
- Grep 残留扫描：源码零 `fastclaw`。
- `~/.fluctio/` + `fluctio.db` 由用户在首次运行后手动确认（用户自行从 `~/.fastclaw` 迁移数据，spec 决策 2）。
