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
	"github.com/fluctio-ai/fluctio/internal/store"
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
	// projectCardArchiveHeader receives index rows spilled past the live
	// budget. Archived rows are kept verbatim — history, not garbage;
	// the snapshot and the reconciler only look at the live section.
	projectCardArchiveHeader = "## 会话归档"
	// projectCardMaxLiveRows is the live-index budget: once 会话索引
	// holds more rows, the oldest spill into 会话归档.
	projectCardMaxLiveRows = 30
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
	return insertProjectCardRow(content, projectCardIndexHeader, row)
}

// insertProjectCardRow is appendProjectCardRow for an arbitrary section
// header (the archive section uses it too).
func insertProjectCardRow(content, header, row string) string {
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	lines := strings.Split(content, "\n")
	headerIdx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			headerIdx = i
			break
		}
	}
	if headerIdx == -1 {
		return strings.TrimRight(content, "\n") + "\n\n" + header + "\n\n" + row + "\n"
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

// projectCardSectionBounds locates a section by its exact header line
// and returns (headerIdx, endIdx, true), where endIdx is the first line
// of the next "## " section (or len(lines) when this is the last
// section). ok=false when the header is absent — callers treat that as
// "the user removed the section" and must not fabricate one.
func projectCardSectionBounds(lines []string, header string) (int, int, bool) {
	for i, l := range lines {
		if strings.TrimSpace(l) != header {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "## ") {
				end = j
				break
			}
		}
		return i, end, true
	}
	return 0, 0, false
}

// buildProjectCardSnapshot renders the truncated first-turn snapshot:
// the 关于 / 决策与结论 sections up to a line budget, then the index
// header with the newest rows. Long cards degrade to newest-first
// instead of overflowing the context window; the model reads the full
// file via ../PROJECT.md when it needs the rest. The row scan stops at
// the next "## " section so archived rows never leak into the snapshot.
func buildProjectCardSnapshot(content string) string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	headerIdx, idxEnd, ok := projectCardSectionBounds(lines, projectCardIndexHeader)
	if !ok {
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
	for _, l := range lines[headerIdx+1 : idxEnd] {
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

// readProjectCard fetches the shared card, "" when absent or
// unreadable (callers treat that as "no card yet").
func readProjectCard(ctx context.Context, ws workspace.Store, agentID, projectID string) string {
	rc, err := ws.Get(ctx, agentID, projectID, "", projectCardFilename)
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

// ──────────────────────────────────────────────────────────────────
// Reconciler (M2)
//
// The sessions table is the source of truth; the card's 会话索引
// section converges to it. Immediate hooks on delete / rename / move
// keep the UX tight, and the gateway's hourly sweep catches everything
// those miss (auto titles, IM-side mutations, a crash between the DB
// write and the card write). Best-effort by design.
// ──────────────────────────────────────────────────────────────────

// projectCardRow is one parsed skeleton row of the 会话索引 section:
//
//	- [2026-10-06] label · 进行中 <!-- sid:s-… -->
type projectCardRow struct {
	key    string
	date   string
	label  string
	status string
}

// parseProjectCardRow extracts the row's fields; ok=false for any line
// that isn't a marker-carrying row (narrative lines inside the section
// are left alone by callers).
func parseProjectCardRow(line string) (projectCardRow, bool) {
	l := strings.TrimSpace(line)
	if !strings.HasPrefix(l, "- ") {
		return projectCardRow{}, false
	}
	rest := strings.TrimPrefix(l, "- ")
	i := strings.Index(rest, "<!-- sid:")
	if i < 0 || !strings.HasSuffix(rest, " -->") {
		return projectCardRow{}, false
	}
	key := rest[i+len("<!-- sid:") : len(rest)-len(" -->")]
	if key == "" {
		return projectCardRow{}, false
	}
	rest = strings.TrimSpace(rest[:i])
	r := projectCardRow{key: key, status: "进行中"}
	if strings.HasPrefix(rest, "[") {
		if j := strings.Index(rest, "] "); j > 0 {
			r.date = rest[1:j]
			rest = rest[j+2:]
		}
	}
	if k := strings.LastIndex(rest, " · "); k >= 0 {
		r.label = strings.TrimSpace(rest[:k])
		r.status = strings.TrimSpace(rest[k+len(" · "):])
	} else {
		r.label = strings.TrimSpace(rest)
	}
	if r.label == "" {
		r.label = "未命名会话"
	}
	return r, true
}

func (r projectCardRow) render() string {
	return fmt.Sprintf("- [%s] %s · %s %s", r.date, r.label, r.status, projectCardMarker(r.key))
}

// projectCardSessionLister is the slice of store.Store the reconciler
// needs — a one-method interface so unit tests don't have to satisfy
// the whole store.
type projectCardSessionLister interface {
	ListSessions(ctx context.Context, agentID string) ([]store.SessionMeta, error)
}

// reconcileProjectCard aligns one project's card with the sessions
// table: adds rows for DB sessions the card doesn't list yet (backfill
// for sessions predating the card, or an ensure lost to a crash),
// refreshes a row's label to the session's stored title when one exists
// (manual + auto renames converge here), drops rows whose sessions no
// longer exist (deleted or moved away), then archives live overflow.
// Skips cards whose index section the user removed — ensure recreates
// it on the next session create; the reconciler never fabricates
// structure the operator deleted. Returns true when the card changed;
// a no-op pass writes nothing.
func reconcileProjectCard(ctx context.Context, ws workspace.Store, st projectCardSessionLister, agentID, projectID string) (bool, error) {
	metas, err := st.ListSessions(ctx, agentID)
	if err != nil {
		return false, err
	}
	var live []store.SessionMeta
	for _, m := range metas {
		if m.ProjectID == projectID {
			live = append(live, m)
		}
	}
	content := readProjectCard(ctx, ws, agentID, projectID)
	if content == "" {
		if len(live) == 0 {
			return false, nil
		}
		content = strings.TrimRight(projectCardTemplate, "\n") + "\n"
	}
	lines := strings.Split(content, "\n")
	headerIdx, end, ok := projectCardSectionBounds(lines, projectCardIndexHeader)
	if !ok {
		return false, nil
	}
	orig := content
	byKey := make(map[string]store.SessionMeta, len(live))
	for _, m := range live {
		byKey[m.Key] = m
	}
	seen := map[string]bool{}
	changed := false
	var body []string
	for i := headerIdx + 1; i < end; i++ {
		r, isRow := parseProjectCardRow(lines[i])
		if !isRow {
			body = append(body, lines[i]) // narrative lines inside the section stay
			continue
		}
		seen[r.key] = true
		m, inDB := byKey[r.key]
		switch {
		case !inDB:
			changed = true // session deleted or moved away — drop the row
		case m.Title != "" && projectCardPreview(m.Title) != r.label:
			r.label = projectCardPreview(m.Title)
			body = append(body, r.render())
			changed = true
		default:
			body = append(body, lines[i])
		}
	}
	for _, m := range live {
		if seen[m.Key] {
			continue
		}
		label := "未命名会话"
		if m.Title != "" {
			label = projectCardPreview(m.Title)
		}
		body = append(body, projectCardRow{
			key:    m.Key,
			date:   m.UpdatedAt.Local().Format("2006-01-02"),
			label:  label,
			status: "进行中",
		}.render())
		changed = true
	}
	if changed {
		updated := lines[:headerIdx+1]
		updated = append(updated, body...)
		updated = append(updated, lines[end:]...)
		content = strings.Join(updated, "\n")
	}
	// The live-index budget applies even when DB alignment is a no-op —
	// a card can sit over budget with no session changes (e.g. 32
	// ensures, then the first reconcile). The final content comparison
	// decides whether anything gets written.
	content = archiveProjectCardOverflow(content, projectCardMaxLiveRows)
	if content == orig {
		return false, nil
	}
	if err := ws.Put(ctx, agentID, projectID, "", projectCardFilename, strings.NewReader(content), int64(len(content)), "text/markdown; charset=utf-8"); err != nil {
		return false, err
	}
	return true, nil
}

// archiveProjectCardOverflow moves the oldest index rows past `keep`
// into the 会话归档 section (appended at EOF when absent), verbatim and
// in their original oldest-first order.
func archiveProjectCardOverflow(content string, keep int) string {
	lines := strings.Split(content, "\n")
	headerIdx, end, ok := projectCardSectionBounds(lines, projectCardIndexHeader)
	if !ok {
		return content
	}
	var rowIdx []int
	for i := headerIdx + 1; i < end; i++ {
		if _, isRow := parseProjectCardRow(lines[i]); isRow {
			rowIdx = append(rowIdx, i)
		}
	}
	if len(rowIdx) <= keep {
		return content
	}
	overflow := rowIdx[:len(rowIdx)-keep]
	drop := make(map[int]bool, len(overflow))
	var moved []string
	for _, i := range overflow {
		drop[i] = true
		moved = append(moved, lines[i])
	}
	var out []string
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	body := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	for _, row := range moved {
		body = insertProjectCardRow(body, projectCardArchiveHeader, row)
	}
	return body
}

// reconcileProjectCardFor converges one project's card (best-effort;
// the hourly sweep retries). Called after delete / rename / move so the
// index rows track those mutations immediately instead of waiting for
// the sweep.
func (a *Agent) reconcileProjectCardFor(projectID string) {
	if projectID == "" || a.workspaceStore == nil || a.dataStore == nil {
		return
	}
	if _, err := reconcileProjectCard(context.Background(), a.workspaceStore, a.dataStore, a.agentID, projectID); err != nil {
		slog.Warn("project card: reconcile failed",
			"agent", a.agentID, "project", projectID, "error", err)
	}
}

// reconcileAgentProjectCards reconciles every project this agent has
// sessions in — the boot backfill for projects whose cards predate
// them, plus the drift safety net.
func (a *Agent) reconcileAgentProjectCards(ctx context.Context) {
	if a.workspaceStore == nil || a.dataStore == nil {
		return
	}
	metas, err := a.dataStore.ListSessions(ctx, a.agentID)
	if err != nil {
		slog.Warn("project card: list sessions failed", "agent", a.agentID, "error", err)
		return
	}
	seen := map[string]bool{}
	for _, m := range metas {
		if m.ProjectID == "" || seen[m.ProjectID] {
			continue
		}
		seen[m.ProjectID] = true
		if _, err := reconcileProjectCard(ctx, a.workspaceStore, a.dataStore, a.agentID, m.ProjectID); err != nil {
			slog.Warn("project card: reconcile failed",
				"agent", a.agentID, "project", m.ProjectID, "error", err)
		}
	}
}

// ReconcileProjectCards aligns every agent's project cards with the
// sessions table — the gateway's hourly sweep entry point.
func (m *Manager) ReconcileProjectCards(ctx context.Context) {
	for _, ag := range m.All() {
		if ctx.Err() != nil {
			return
		}
		ag.reconcileAgentProjectCards(ctx)
	}
}
