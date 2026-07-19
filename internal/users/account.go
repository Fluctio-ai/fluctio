// Package users owns the platform's identity layer: real user accounts
// (Account) and the programmatic tokens they issue (APIKey). Both types are
// thin facades over store.Store so a single SQL backend remains the source
// of truth across pods.
//
// The legacy "apikey == user" model is gone. An account is what owns
// agents/sessions/cron jobs; an apikey is just a scoped credential pointing
// at one account, with an explicit list of agents it may operate on.
package users

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// Roles. super_admin can manage every user/agent/provider on the platform;
// user can only touch their own resources. app_user is provisioned by an
// api_key on behalf of a downstream application — these accounts have no
// password and cannot log in via dashboard or password endpoints; they
// exist purely to give external end-users a stable fluctio user_id so
// sessions / agent_files / scope=user configs partition cleanly per
// end-user. There is intentionally no fine-grained scheme — anything
// more complex lives in the apikey ACL layer.
const (
	RoleSuperAdmin  = "super_admin"
	RoleChannelUser = "channel_user"
)

// Statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// ErrInvalidCredentials masks "no such user" and "wrong password" so the
// login handler can't be used as an email-existence oracle.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Account is the public representation of a user row. PasswordHash never
// leaves the package — we read it during Authenticate and zero it out
// before returning to callers.
type Account struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName,omitempty"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	APIKeyID    string    `json:"apikeyId,omitempty"`
	ExternalID  string    `json:"externalId,omitempty"`
	AvatarURL   string    `json:"avatarUrl,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Accounts is the registry for user accounts.
type Accounts struct {
	store store.Store
}

// NewAccounts returns an account registry backed by st. Refuses nil — the
// platform has no in-memory mode.
func NewAccounts(st store.Store) (*Accounts, error) {
	if st == nil {
		return nil, errors.New("users.NewAccounts: store is required")
	}
	return &Accounts{store: st}, nil
}

// Count returns the number of users on the platform. Onboarding gates on
// `Count(ctx) == 0` to decide whether to show the wizard.
func (a *Accounts) Count(ctx context.Context) (int, error) {
	return a.store.CountUsers(ctx)
}

// CreateInput is the bag of fields Create writes onto a new user row.
// Required: Username, Email, Password. Role defaults to RoleSuperAdmin.
//
// APIKeyID + ExternalID are the upstream-provisioning idempotency pair.
// Set APIKeyID to the apikey that's minting this row (handler reads it
// from auth.Identity, never from the request body) so the row is
// auditable back to the provisioning key. Set ExternalID to the calling
// app's own user identifier; the partial UNIQUE index on (apikey_id,
// external_id) — see migrateUsersAppUserCols — means the same pair
// always resolves to the same fluctio user_id, so retries are safe.
//
// AvatarURL must be empty or a `data:image/*` URL ≤256KB; the handler
// caller is responsible for that validation.
type CreateInput struct {
	Username    string
	Email       string
	Password    string
	DisplayName string
	Role        string
	AvatarURL   string
	APIKeyID    string
	ExternalID  string
}

// Create writes a new account. Password is hashed with bcrypt; plaintext
// is never persisted. ID is always auto-generated.
//
// Idempotent on (APIKeyID, ExternalID): when both are non-empty, a repeat
// call returns the already-provisioned row instead of erroring on the
// partial UNIQUE index. Upstream apps can re-issue the same provisioning
// call without tracking whether they've called us before. username/email
// UNIQUE collisions across *different* identities still surface as errors —
// silently returning a stranger's row would hide a real conflict.
func (a *Accounts) Create(ctx context.Context, in CreateInput) (*Account, error) {
	apikeyID := strings.TrimSpace(in.APIKeyID)
	externalID := strings.TrimSpace(in.ExternalID)
	// Fast path — already provisioned for this (apikey, external_id) pair.
	if apikeyID != "" && externalID != "" {
		if rec, err := a.store.GetUserByExternal(ctx, apikeyID, externalID); err == nil {
			return toAccount(rec), nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	username := strings.TrimSpace(in.Username)
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if username == "" || email == "" || in.Password == "" {
		return nil, errors.New("users.Create: username, email, password are required")
	}
	role := in.Role
	if role == "" {
		role = RoleSuperAdmin
	}
	if role != RoleSuperAdmin {
		return nil, errors.New("users.Create: invalid role")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	id, err := newID("u_")
	if err != nil {
		return nil, err
	}
	rec := &store.UserRecord{
		ID:           id,
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		DisplayName:  in.DisplayName,
		Role:         role,
		Status:       StatusActive,
		APIKeyID:     apikeyID,
		ExternalID:   externalID,
		AvatarURL:    in.AvatarURL,
	}
	if err := a.store.CreateUser(ctx, rec); err != nil {
		// Race: another concurrent request minted the same
		// (apikey_id, external_id) pair between our fast-path miss
		// above and the INSERT. Re-read and return that row so the
		// caller sees the same idempotent contract regardless of
		// timing. username/email collisions across different
		// identities still bubble — see EnsureAppUser for the same
		// pattern.
		if apikeyID != "" && externalID != "" {
			if again, qerr := a.store.GetUserByExternal(ctx, apikeyID, externalID); qerr == nil {
				return toAccount(again), nil
			}
		}
		return nil, err
	}
	return toAccount(rec), nil
}

// Authenticate validates a username-or-email + password pair. Returns the
// account on success, ErrInvalidCredentials on every failure mode (missing
// user, wrong password, disabled account) so callers can't distinguish.
func (a *Accounts) Authenticate(ctx context.Context, login, password string) (*Account, error) {
	login = strings.TrimSpace(login)
	if login == "" || password == "" {
		return nil, ErrInvalidCredentials
	}
	if strings.Contains(login, "@") {
		login = strings.ToLower(login)
	}
	rec, err := a.store.GetUserByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if rec.Status != StatusActive {
		return nil, ErrInvalidCredentials
	}
	// app_user accounts (and any other row provisioned without a real
	// password) carry an empty hash. bcrypt.CompareHashAndPassword
	// would still fail-closed, but checking explicitly keeps the
	// failure mode unambiguous and avoids burning bcrypt cycles on
	// every probe.
	if rec.PasswordHash == "" || rec.Role == RoleChannelUser {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return toAccount(rec), nil
}

// Get returns the account for id, or store.ErrNotFound.
func (a *Accounts) Get(ctx context.Context, id string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// List returns all accounts. Super-admin endpoints only.
func (a *Accounts) List(ctx context.Context) ([]*Account, error) {
	recs, err := a.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(recs))
	for i := range recs {
		out = append(out, toAccount(&recs[i]))
	}
	return out, nil
}

// Update applies non-credential changes (display name, role, status). Use
// SetPassword for password rotation.
func (a *Accounts) Update(ctx context.Context, id, displayName, role, status string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if displayName != "" {
		rec.DisplayName = displayName
	}
	if role != "" {
		if role != RoleSuperAdmin {
			return nil, errors.New("users.Update: invalid role")
		}
		rec.Role = role
	}
	if status != "" {
		if status != StatusActive && status != StatusDisabled {
			return nil, errors.New("users.Update: invalid status")
		}
		rec.Status = status
	}
	if err := a.store.UpdateUser(ctx, rec); err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// UpdateProfile applies self-service edits — display name and avatar
// only. Role/status changes go through Update (admin-only). avatarURL
// is stored verbatim; the handler is responsible for shape and size
// validation. Pass an explicit empty string to clear the avatar.
func (a *Accounts) UpdateProfile(ctx context.Context, id, displayName, avatarURL string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	rec.DisplayName = displayName
	rec.AvatarURL = avatarURL
	if err := a.store.UpdateUser(ctx, rec); err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// VerifyPassword checks a plaintext password against the stored hash for
// id. Returns ErrInvalidCredentials on mismatch (or for accounts with no
// password, e.g. app_user). Used by /api/me/password to gate self-service
// password change behind the current password.
func (a *Accounts) VerifyPassword(ctx context.Context, id, password string) error {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return ErrInvalidCredentials
	}
	if rec.PasswordHash == "" {
		return ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// SetPassword rotates an account's password. Caller is responsible for
// permission checks (self vs. super_admin).
func (a *Accounts) SetPassword(ctx context.Context, id, newPassword string) error {
	if newPassword == "" {
		return errors.New("users.SetPassword: empty password")
	}
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	rec.PasswordHash = string(hash)
	return a.store.UpdateUser(ctx, rec)
}


func toAccount(r *store.UserRecord) *Account {
	if r == nil {
		return nil
	}
	return &Account{
		ID:          r.ID,
		Username:    r.Username,
		Email:       r.Email,
		DisplayName: r.DisplayName,
		Role:        r.Role,
		Status:      r.Status,
		APIKeyID:    r.APIKeyID,
		ExternalID:  r.ExternalID,
		AvatarURL:   r.AvatarURL,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// newID returns a short unique id with the given prefix. ~80 bits of
// entropy — collisions vanishingly unlikely at platform scale.
func newID(prefix string) (string, error) {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}
