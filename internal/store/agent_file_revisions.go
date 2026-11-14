package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Agent file revision history: every agent_files write that actually
// changed the content lands one row here, tagged with the writer and a
// short reason — the "commit message". Together with the diffable list /
// detail API this is the attributable history the write-only
// MEMORY.archive.md never provided (the archive still records what the
// tidy sweep pruned; revisions record every writer's changes).
//
// Recording happens inside SaveAgentFile, so no writer can bypass it;
// CompareAndSwapAgentFile forwards through SaveAgentFile and inherits it.

// AgentFileRevision is one recorded version of an (agent_id, filename)
// row. Content is deliberately not part of the list shape — only
// GetAgentFileRevision returns bytes.
type AgentFileRevision struct {
	ID        int64     `json:"id"`
	AgentID   string    `json:"agent_id"`
	Filename  string    `json:"filename"`
	Writer    string    `json:"writer"`
	Reason    string    `json:"reason"`
	Size      int       `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentFileRevisionDetail is a revision plus the content of the
// immediately older version of the same file, so callers can render a
// diff without a second round-trip. PrevContent is "" for the first
// revision.
type AgentFileRevisionDetail struct {
	AgentFileRevision
	Content     string `json:"content"`
	PrevContent string `json:"prev_content"`
}

// fileWriteOpts carries attribution for one agent_files write.
type fileWriteOpts struct {
	writer string
	reason string
}

// FileWriteOpt tags a SaveAgentFile / CompareAndSwapAgentFile call with
// who is writing and why. Writers in use: "agent" (in-chat tools),
// "auto-persist", "tidy", "ui", "cli", "system".
type FileWriteOpt func(*fileWriteOpts)

// WithFileWriter attributes a write: writer identifies the caller,
// reason is a short commit-message-style summary (may be empty).
func WithFileWriter(writer, reason string) FileWriteOpt {
	return func(o *fileWriteOpts) { o.writer = writer; o.reason = reason }
}

func applyFileWriteOpts(opts []FileWriteOpt) fileWriteOpts {
	out := fileWriteOpts{writer: "system"}
	for _, o := range opts {
		if o != nil {
			o(&out)
		}
	}
	return out
}

// DefaultAgentFileRevisionKeep bounds history per (agent, filename).
// The gateway reads FLUCTIO_AGENT_FILE_REVISIONS_KEEP (<=0 disables
// recording entirely) and wires the value via SetAgentFileRevisionKeep.
const DefaultAgentFileRevisionKeep = 100

// SetAgentFileRevisionKeep overrides how many revisions to keep per
// (agent, filename); keep<=0 turns recording off (existing rows stay
// until pruned by a later positive setting).
func (d *DBStore) SetAgentFileRevisionKeep(keep int) { d.agentFileRevisionKeep = keep }

// AgentFileRevisionKeep returns the effective per-file revision cap.
// The gateway's consolidation sweep prunes with the same value it
// installed at boot.
func (d *DBStore) AgentFileRevisionKeep() int { return d.agentFileRevisionKeep }

func (d *DBStore) migrateAgentFileRevisions(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, d.adaptDDL(`CREATE TABLE IF NOT EXISTS agent_file_revisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		agent_id TEXT NOT NULL,
		filename TEXT NOT NULL,
		writer TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL,
		size INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)); err != nil {
		return fmt.Errorf("migrate agent_file_revisions: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_agent_file_revisions
		ON agent_file_revisions (agent_id, filename, id)`); err != nil {
		return fmt.Errorf("migrate agent_file_revisions (index): %w", err)
	}
	return nil
}

// recordAgentFileRevision inserts one revision row. Runs in the caller's
// transaction (SaveAgentFile's) so content and history commit or roll
// back together — a save that succeeded while its history row failed
// would be silent history loss, the exact problem this table exists to
// fix.
func (d *DBStore) recordAgentFileRevision(ctx context.Context, tx *sql.Tx, agentID, filename string, data []byte, o fileWriteOpts) error {
	if d.agentFileRevisionKeep <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO agent_file_revisions
			(agent_id, filename, writer, reason, content, size, created_at)
			VALUES (%s, %s, %s, %s, %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		agentID, filename, o.writer, o.reason, string(data), len(data), time.Now().UTC())
	return err
}

const agentFileRevisionCols = `id, agent_id, filename, writer, reason, size, created_at`

func scanAgentFileRevision(row interface{ Scan(...any) error }) (AgentFileRevision, error) {
	var r AgentFileRevision
	err := row.Scan(&r.ID, &r.AgentID, &r.Filename, &r.Writer, &r.Reason, &r.Size, &r.CreatedAt)
	return r, err
}

// ListAgentFileRevisions returns the newest-first revision metadata for
// one agent file, capped at limit (<=0 gets the default keep window).
func (d *DBStore) ListAgentFileRevisions(ctx context.Context, agentID, filename string, limit int) ([]AgentFileRevision, error) {
	if limit <= 0 {
		limit = d.agentFileRevisionKeep
		if limit <= 0 {
			limit = DefaultAgentFileRevisionKeep
		}
	}
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM agent_file_revisions
			WHERE agent_id = %s AND filename = %s ORDER BY id DESC LIMIT %d`,
			agentFileRevisionCols, d.ph(1), d.ph(2), limit),
		agentID, filename)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentFileRevision
	for rows.Next() {
		r, err := scanAgentFileRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAgentFileRevision returns one revision's full content plus the
// previous version's content for diffing.
func (d *DBStore) GetAgentFileRevision(ctx context.Context, agentID, filename string, id int64) (*AgentFileRevisionDetail, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s, content FROM agent_file_revisions
			WHERE id = %s AND agent_id = %s AND filename = %s`,
			agentFileRevisionCols, d.ph(1), d.ph(2), d.ph(3)),
		id, agentID, filename)
	var detail AgentFileRevisionDetail
	err := row.Scan(&detail.ID, &detail.AgentID, &detail.Filename, &detail.Writer, &detail.Reason,
		&detail.Size, &detail.CreatedAt, &detail.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Previous version of the same file, for the diff view. Missing
	// (first revision) leaves PrevContent empty.
	_ = d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT content FROM agent_file_revisions
			WHERE agent_id = %s AND filename = %s AND id < %s ORDER BY id DESC LIMIT 1`,
			d.ph(1), d.ph(2), d.ph(3)),
		agentID, filename, id).Scan(&detail.PrevContent)
	return &detail, nil
}

// PruneAgentFileRevisions keeps the newest `keep` revisions per
// (agent_id, filename) and deletes the rest. keep<=0 is a no-op — a
// disabled history is frozen, not nuked, so re-enabling finds its rows.
func (d *DBStore) PruneAgentFileRevisions(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM agent_file_revisions WHERE id IN (
			SELECT r.id FROM agent_file_revisions r
			WHERE (SELECT COUNT(*) FROM agent_file_revisions r2
				WHERE r2.agent_id = r.agent_id AND r2.filename = r.filename AND r2.id > r.id) >= %d)`, keep))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
