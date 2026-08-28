package store

import (
	"context"
	"fmt"
)

// dailyRunTables whitelists the (agent_id, date)-keyed marker tables the
// daily-run helpers below may touch — the table name reaches SQL, so an
// open-ended parameter would be an injection foot-gun.
var dailyRunTables = map[string]bool{
	"kb_card_gen_runs":  true,
	"kb_card_push_runs": true,
}

// HasDailyRun reports whether the (agent, date) marker row exists in one
// of the whitelisted daily-run tables — the cards autogen sweep's skip
// condition and the digest push's once-a-day gate.
func (d *DBStore) HasDailyRun(ctx context.Context, table, agentID, date string) bool {
	if !dailyRunTables[table] {
		return false
	}
	var one int
	err := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT 1 FROM %s WHERE agent_id = %s AND date = %s`,
			table, d.ph(1), d.ph(2)),
		agentID, date).Scan(&one)
	return err == nil
}

// StampCardGenRun upserts the nightly generation marker (created count +
// model). A manual re-run overwrites the stamp — regenerate is explicit.
// Idempotency for the cards autogen sweep.
func (d *DBStore) StampCardGenRun(ctx context.Context, agentID, date string, created int, model string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_card_gen_runs (agent_id, date, created, model, created_at)
			VALUES (%s,%s,%s,%s,CURRENT_TIMESTAMP)
			ON CONFLICT(agent_id, date) DO UPDATE SET created=excluded.created, model=excluded.model, created_at=CURRENT_TIMESTAMP`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		agentID, date, created, model)
	return err
}

// StampCardPushRun upserts the once-a-day digest marker (pushed count +
// channel). A failed push leaves no stamp so the next tick retries; a
// delivered one is never double-sent.
func (d *DBStore) StampCardPushRun(ctx context.Context, agentID, date string, count int, channel string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_card_push_runs (agent_id, date, pushed_count, channel, pushed_at)
			VALUES (%s,%s,%s,%s,CURRENT_TIMESTAMP)
			ON CONFLICT(agent_id, date) DO UPDATE SET pushed_count=excluded.pushed_count,
				channel=excluded.channel, pushed_at=CURRENT_TIMESTAMP`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		agentID, date, count, channel)
	return err
}
