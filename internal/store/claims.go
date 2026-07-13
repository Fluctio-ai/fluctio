package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// IMClaimMaxAttempts caps how many failed redeems a code tolerates before
// it's voided. 6 digits = 1e6 space; 5 attempts / 10 min window makes
// brute force negligible.
const IMClaimMaxAttempts = 5

// IMClaimTTL is how long a code stays redeemable after creation.
const IMClaimTTL = 10 * time.Minute

// IMClaim intent values.
const (
	IMClaimIntentFirst   = "first"
	IMClaimIntentAdd     = "add"
	IMClaimIntentReplace = "replace"
)

// IMClaimRecord is a one-time verification-code row that lets the agent
// owner bind their IM platform ID via `/claim <code>`.
type IMClaimRecord struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agentId"`
	Channel   string    `json:"channel"`
	OwnerUUID string    `json:"ownerUuid"` // web-side authenticated owner UUID
	Code      string    `json:"-"`         // 6-digit one-time code (never serialized)
	Intent    string    `json:"intent"`    // "first" | "add" | "replace"
	ExpiresAt time.Time `json:"expiresAt"`
	Used      bool      `json:"used"`
	Attempts  int       `json:"attempts"` // >= IMClaimMaxAttempts → voided
	CreatedAt time.Time `json:"createdAt"`
}

// CreateIMClaim mints a one-time verification code for (agentID, channel),
// invalidating any prior unused code for the same pair first. The caller is
// the web-side authenticated owner (ownerUUID). Returns the 6-digit code.
func (d *DBStore) CreateIMClaim(ctx context.Context, agentID, channel, ownerUUID, intent string) (string, error) {
	if agentID == "" || channel == "" || ownerUUID == "" {
		return "", errors.New("store: create im claim requires agentID, channel, ownerUUID")
	}
	if intent == "" {
		intent = IMClaimIntentAdd
	}
	// Void any prior unused code for the same (agent, channel) so only the
	// latest minted code is redeemable — stale codes don't pile up.
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE im_claims SET used = TRUE WHERE agent_id = %s AND channel = %s AND used = FALSE`, d.ph(1), d.ph(2)),
		agentID, channel); err != nil {
		return "", err
	}
	code, err := randomClaimCode()
	if err != nil {
		return "", err
	}
	id, err := randomClaimID()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO im_claims (id, agent_id, channel, owner_uuid, code, intent, expires_at, used, attempts, created_at)
			VALUES (%s, %s, %s, %s, %s, %s, %s, FALSE, 0, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)),
		id, agentID, channel, ownerUUID, code, intent, now.Add(IMClaimTTL), now); err != nil {
		return "", err
	}
	return code, nil
}

// RedeemIMClaim verifies + retires the code for (agentID, channel).
// Returns true only when a valid, unused, unexpired, under-attempt-limit
// code matched and was atomically marked used. A failed redeem against an
// existing unused code increments its attempt counter (voiding at
// IMClaimMaxAttempts); a nonexistent or already-used code leaves state
// unchanged and returns false (no existence disclosure).
func (d *DBStore) RedeemIMClaim(ctx context.Context, agentID, channel, code string) (bool, error) {
	if agentID == "" || channel == "" || code == "" {
		return false, nil
	}
	now := time.Now().UTC()
	res, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE im_claims SET used = TRUE
			WHERE agent_id = %s AND channel = %s AND code = %s
			  AND used = FALSE AND attempts < %s AND expires_at > %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		agentID, channel, code, IMClaimMaxAttempts, now)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	_, _ = d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE im_claims SET attempts = attempts + 1
			WHERE agent_id = %s AND channel = %s AND code = %s AND used = FALSE`,
			d.ph(1), d.ph(2), d.ph(3)),
		agentID, channel, code)
	return false, nil
}

// GetActiveIMClaim returns the latest live (unused, unexpired) claim for
// (agentID, channel), or ErrNotFound. The web UI uses this to show the
// pending code + countdown.
func (d *DBStore) GetActiveIMClaim(ctx context.Context, agentID, channel string) (*IMClaimRecord, error) {
	now := time.Now().UTC()
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, agent_id, channel, owner_uuid, code, intent, expires_at, used, attempts, created_at
			FROM im_claims
			WHERE agent_id = %s AND channel = %s AND used = FALSE AND expires_at > %s
			ORDER BY created_at DESC LIMIT 1`,
			d.ph(1), d.ph(2), d.ph(3)),
		agentID, channel, now)
	var c IMClaimRecord
	if err := row.Scan(&c.ID, &c.AgentID, &c.Channel, &c.OwnerUUID, &c.Code, &c.Intent, &c.ExpiresAt, &c.Used, &c.Attempts, &c.CreatedAt); err != nil {
		return nil, scanErr(err)
	}
	return &c, nil
}

// CleanupExpiredIMClaims removes used + expired rows. Intended for boot.
func (d *DBStore) CleanupExpiredIMClaims(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	res, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM im_claims WHERE used = TRUE OR expires_at < %s`, d.ph(1)),
		now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func randomClaimCode() (string, error) {
	const digits = "0123456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	out := make([]byte, 6)
	for i, v := range b {
		out[i] = digits[int(v)%len(digits)]
	}
	return string(out), nil
}

func randomClaimID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("imclaim_%x", b[:]), nil
}
