package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/session"
	"github.com/fluctio-ai/fluctio/internal/workspace"
)

// ──────────────────────────────────────────────────────────────────
// PROJECT.md — the shared per-project card
//
// ONE file per project, stored at the project-shared layer of the
// workspace store — the (projectID, sessionID="") tuple, i.e.
// projects/<pid>/PROJECT.md on the local backend. Every chat in the
// project shares it; there are deliberately NO per-session copies.
// This code addresses the file only through the store tuple (never a
// hand-built OS path), so the existing workspace scope isolation keeps
// a chat from touching another project's card; agent-side reads and
// writes go through the file tools' "../PROJECT.md" scoping (see
// Registry.wsScope), which admits exactly the project-shared layer and
// nothing wider. The narrative sections (关于 / 决策与结论) belong to
// the agent and the user; the 会话索引 section is system-managed.
// ──────────────────────────────────────────────────────────────────

const (
	projectCardFilename = "PROJECT.md"
	// projectCardIndexHeader marks the system-managed session registry
	// section. Skeleton rows are appended at its end on session create.
	projectCardIndexHeader = "## 会话索引"
	// Snapshot budget: lines kept from the sections above the index
	// header, and the newest index rows kept, on first-turn injection.
	projectCardMaxHeadLines = 25
	projectCardMaxIndexRows = 15
)

// projectCardTemplate seeds the card on first touch.
const projectCardTemplate = `# PROJECT.md（项目卡）

本项目所有会话共用这一份项目卡。每次新建会话时，系统会在「会话索引」自动追加一行；其余内容由 agent 与用户共同维护。

## 关于

（项目是什么、约定、当前目标。）

## 决策与结论

（跨会话沉淀的关键结论，让新会话不必重问。新条目加在最上面，宁缺毋滥。）

## 会话索引

（系统自动追加，每次新建会话一行；到达重要节点后回填一句话结论。不要删除系统行。）
`

func projectCardMarker(sessionKey string) string {
	return fmt.Sprintf("<!-- sid:%s -->", sessionKey)
}

// projectCardPreview shortens the first user message into the skeleton
// row's display text.
func projectCardPreview(text string) string {
	t := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if t == "" {
		return "新会话"
	}
	r := []rune(t)
	if len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return t
}

// ensureProjectCardEntry registers this session's skeleton row in the
// shared PROJECT.md. Idempotent: the <!-- sid:... --> marker is the
// dedup key, so re-running on a later turn of the same session is a
// no-op read. Returns the card content after the update so the caller
// can snapshot it without a second read.
func ensureProjectCardEntry(ctx context.Context, ws workspace.Store, agentID, projectID, sessionKey, preview string, now time.Time) (string, error) {
	var content string
	if rc, err := ws.Get(ctx, agentID, projectID, "", projectCardFilename); err == nil {
		b, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return "", readErr
		}
		content = string(b)
	} else if !errors.Is(err, workspace.ErrNotFound) {
		return "", err
	}
	if content != "" && strings.Contains(content, projectCardMarker(sessionKey)) {
		return content, nil
	}
	row := fmt.Sprintf("- [%s] %s · 进行中 %s", now.Format("2006-01-02"), preview, projectCardMarker(sessionKey))
	if content == "" {
		content = strings.TrimRight(projectCardTemplate, "\n") + "\n\n" + row + "\n"
	} else {
		content = appendProjectCardRow(content, row)
	}
	if err := ws.Put(ctx, agentID, projectID, "", projectCardFilename, strings.NewReader(content), int64(len(content)), "text/markdown; charset=utf-8"); err != nil {
		return "", err
	}
	return content, nil
}

// appendProjectCardRow inserts row at the end of the 会话索引 section,
// after the section's last non-blank line (so it lands under the newest
// row, not below trailing blanks). When the header is gone (the user
// rewrote the file), a fresh section is appended at EOF rather than
// failing — degrading to append-only keeps the registry row alive
// without ever overwriting the narrative sections.
func appendProjectCardRow(content, row string) string {
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	lines := strings.Split(content, "\n")
	headerIdx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == projectCardIndexHeader {
			headerIdx = i
			break
		}
	}
	if headerIdx == -1 {
		return strings.TrimRight(content, "\n") + "\n\n" + projectCardIndexHeader + "\n\n" + row + "\n"
	}
	end := len(lines)
	for i := headerIdx + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	insertAt := end
	for insertAt > headerIdx+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	lines = append(lines[:insertAt], append([]string{row}, lines[insertAt:]...)...)
	return strings.Join(lines, "\n")
}

// buildProjectCardSnapshot renders the truncated first-turn snapshot:
// the 关于 / 决策与结论 sections up to a line budget, then the index
// header with the newest rows. Long cards degrade to newest-first
// instead of overflowing the context window; the model reads the full
// file via ../PROJECT.md when it needs the rest.
func buildProjectCardSnapshot(content string) string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	headerIdx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == projectCardIndexHeader {
			headerIdx = i
			break
		}
	}
	if headerIdx == -1 {
		if len(lines) > projectCardMaxHeadLines {
			lines = append(lines[:projectCardMaxHeadLines:projectCardMaxHeadLines], "…（项目卡过长，已截断）")
		}
		return strings.Join(lines, "\n")
	}
	pre := lines[:headerIdx]
	if len(pre) > projectCardMaxHeadLines {
		pre = append(pre[:projectCardMaxHeadLines:projectCardMaxHeadLines], "…（关于/决策区过长，已截断）")
	}
	out := append([]string{}, pre...)
	out = append(out, lines[headerIdx])
	var rows []string
	for _, l := range lines[headerIdx+1:] {
		if strings.HasPrefix(strings.TrimSpace(l), "- ") {
			rows = append(rows, l)
		}
	}
	if len(rows) > projectCardMaxIndexRows {
		rows = append([]string{"…（更早的会话行已截断）"}, rows[len(rows)-projectCardMaxIndexRows:]...)
	}
	out = append(out, rows...)
	return strings.Join(out, "\n")
}

// injectProjectCard registers the session in the shared PROJECT.md
// and, on the session's first turn, appends a system-role snapshot so
// the model enters the project oriented (which chats exist, what was
// concluded). Called from HandleMessage / HandleMessageStream right
// before the user message is appended. The snapshot is stored in
// history once — a stable prefix that is never re-injected mid-session
// (protecting the provider prompt cache) — and tagged
// OriginProjectCard so UI history / FTS treat it as runtime context,
// not a user turn.
func (a *Agent) injectProjectCard(ctx context.Context, sess *session.Session, projectID, firstUserText string) {
	if projectID == "" || a.workspaceStore == nil || sess == nil {
		return
	}
	key := sess.SessionKey()
	if key == "" {
		return
	}
	content, err := ensureProjectCardEntry(ctx, a.workspaceStore, a.agentID, projectID, key, projectCardPreview(firstUserText), time.Now())
	if err != nil {
		slog.Warn("project card: register session failed",
			"agent", a.agentID, "project", projectID, "session", key, "error", err)
		return
	}
	if len(sess.GetMessages()) > 0 {
		return // not the first turn — the snapshot already sits in history
	}
	snapshot := buildProjectCardSnapshot(content)
	if strings.TrimSpace(snapshot) == "" {
		return
	}
	sess.Append(provider.Message{
		Role:      "system",
		Origin:    provider.OriginProjectCard,
		Timestamp: time.Now().UnixMilli(),
		Content: fmt.Sprintf("<project_card source=\"../PROJECT.md\">\n"+
			"系统在本会话开始时注入的项目卡快照（本项目所有会话共用一份，位于项目共享层；read_file(\"../PROJECT.md\") 可取最新全文）。\n\n"+
			"%s\n</project_card>", snapshot),
	})
}
