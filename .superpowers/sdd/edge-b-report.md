# Edge B — Frontend `skill_pending` Handler

## Status
DONE

## Commit
`feat(web): surface skill_pending notice in chat-screen` (SHA to fill after commit)

## Recon
- Backend emitter (already shipped in P4 Task 5 / 9fccd67):
  - `internal/agent/loop.go:3030-3042` — `runPostTurn` calls
    `pendingSkillNames(a.homePath)` and emits
    `ChatEvent{Type:"skill_pending", Data:{"count":N, "names":[...]}}`
    when `allowedContinuationSources[msg.Source]` is true.
- Frontend stack: Next.js 16 (TS + React 19), source at `web/src/`,
  built via `pnpm build` → `web/out/` → `cp -r web/out internal/setup/web`
  (Makefile `build-web` target). `internal/setup/web/` is gitignored —
  rebuilt at release time, embedded via `//go:embed all:web` in
  `internal/setup/embed.go`.
- Event dispatcher: `web/src/components/chat-screen.tsx` has two
  switches over event.type — one in the EventSource catch-up handler
  (~L864) and one in the live sendChatStream callback (~L1700).
- Existing non-blocking notice precedent: `compaction_notice` renders
  as a centered muted pill via `role:"notice"` + `kind:"compaction_notice"`
  (chat-screen.tsx:2216-2225). `auth_prompt` renders as an agent bubble
  with auth-option buttons (chat-screen.tsx:1751). Compaction_notice's
  pill pattern is the closer match for a non-blocking ack.

## Change

### 1. `web/src/lib/api.ts` — extend `ChatStreamEvent`
- Added `"skill_pending"` to the `type` union.
- Added `count?: number; names?: string[]` to the `data` shape with a
  comment cross-referencing loop.go runPostTurn emit.

### 2. `web/src/components/chat-screen.tsx`
- Updated `ChatMessage.kind` doc comment to list `skill_pending`.
- Extended the catch-up SSE data shape (`data.data`) with
  `count?: number; names?: string[]` — the catch-up stream has its own
  inline type (not `ChatStreamEvent`) so it needed widening too.
- Added `case "skill_pending"` to BOTH switches (live + catch-up).
  Each handler:
  1. Reads `count`/`names` defensively (defaulting to `0`/`[]`).
  2. Skips when `count === 0` (defensive — backend also gates).
  3. Builds the message via `t("chat.skillPending", {count, names: names.join(", ")})`.
  4. Replaces any prior `skill_pending` notice in `messages` before
     pushing the new one — `prev.filter((m) => m.kind !== "skill_pending")`
     — so consecutive turns don't stack pills.
  5. Uses the SAME render path as `compaction_notice` (centered muted
     pill via `role:"notice"`) — no new widget, no new CSS.

### 3. `web/src/lib/locales/en.ts` + `zh-CN.ts`
- New key `chat.skillPending`:
  - en: `"📝 {count} skill(s) pending approval: {names}. Run \`fluctio skill approve <name>\` to activate."`
  - zh-CN: `"📝 {count} 个技能待审批：{names}。运行 \`fluctio skill approve <名称>\` 以激活。"`

## Build
- Edited source, ran `cd web && pnpm build` (Next.js compiled in ~8s).
- Synced bundle: `rm -rf internal/setup/web && cp -r web/out internal/setup/web`.
- `go build ./...` clean.
- `go test ./internal/setup/` green (embed integrity check).
- `go test ./...` shows 4 FAILs in `internal/agent/tools` — all
  pre-existing Windows-path tests documented in MEMORY.md
  (`fastclaw-build-quirks`); also reproduce on clean `git stash` run.
  Unrelated to this frontend change.

## Self-Review
- Non-blocking: yes — pill bubble, same as compaction_notice.
- Matches existing UI: yes — reuses `role:"notice"` render path.
- Clears on update: yes — `filter((m) => m.kind !== "skill_pending")`
  before push.
- i18n: both en and zh-CN covered.
- Defensive: type widening keeps both live + catch-up type-safe.

## Concerns
- `internal/setup/web/` is gitignored — the rebuilt bundle is NOT in
  the commit. Anyone building from source must run `make build-web`
  (or the manual `pnpm build` + `cp` equivalent) before `go build`.
  This matches the existing release flow (Makefile `build` target).
