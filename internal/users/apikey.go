package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// APIKeyTypeAdmin is the sole key tier in single-user mode — every key
// is owner-level. The string is stored on the apikeys.type column for
// display continuity.
const APIKeyTypeAdmin = "admin"

// APIKey is the public representation of an apikey row. Key holds the
// masked display string ("fc_xxxx****") on list responses, and the freshly
// issued plaintext token on create/rotate. The hash is never returned.
type APIKey struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	Name      string    `json:"name,omitempty"`
	Key       string    `json:"key"`
	Type      string    `json:"type"`
	AgentIDs  []string  `json:"agentIds,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Resolved is what the auth middleware needs to authorize a request: the
// apikey, its owning user, and the agents this key may operate on. Fetched
// in one shot from LookupByToken so the hot path stays a single round-trip.
//
// For type=user keys, Agents is populated with every agent owned by the
// apikey owner at resolve time (a fresh agent created mid-request won't
// appear until the next request). For type=agent it's the explicit ACL
// list. For type=admin it's empty — the auth gate short-circuits on type.
type Resolved struct {
	APIKey  APIKey
	Account Account
	Agents  []string
}

// APIKeys is the registry for programmatic credentials.
type APIKeys struct {
	store store.Store
}

// NewAPIKeys returns an apikey registry backed by st.
func NewAPIKeys(st store.Store) (*APIKeys, error) {
	if st == nil {
		return nil, errors.New("users.NewAPIKeys: store is required")
	}
	return &APIKeys{store: st}, nil
}

// Create issues a new apikey for userID. The plaintext token is returned
// once and never recoverable.
//
// In single-user mode keyType is always APIKeyTypeAdmin (owner-level).
// agentIDs, when non-empty, scopes the key to those agents (enforced at
// request time by auth.Identity.CanAccessAgent). Caller is responsible
// for validating that every agentID resolves to an agent the caller may
// bind (handlers_admin.go does this via validateAgentScope).
func (k *APIKeys) Create(ctx context.Context, userID, name, keyType string, agentIDs []string) (*APIKey, string, error) {
	if userID == "" {
		return nil, "", errors.New("users.APIKeys.Create: userID is required")
	}
	id, err := newID("k_")
	if err != nil {
		return nil, "", err
	}
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}
	rec := &store.APIKeyRecord{
		ID:        id,
		UserID:    userID,
		Name:      name,
		KeyHash:   hashToken(token),
		KeyPrefix: keyPrefix(token),
		Type:      keyType,
		AgentIDs:  agentIDs,
		CreatedAt: time.Now().UTC(),
	}
	if err := k.store.CreateAPIKey(ctx, rec); err != nil {
		return nil, "", err
	}
	out := toAPIKey(rec)
	out.Key = token
	return out, token, nil
}

// Rotate replaces the apikey's token. Old token stops working immediately.
func (k *APIKeys) Rotate(ctx context.Context, id string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := k.store.RotateAPIKey(ctx, id, hashToken(token), keyPrefix(token)); err != nil {
		return "", err
	}
	return token, nil
}

func (k *APIKeys) Delete(ctx context.Context, id string) error {
	return k.store.DeleteAPIKey(ctx, id)
}

func (k *APIKeys) Get(ctx context.Context, id string) (*APIKey, error) {
	rec, err := k.store.GetAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	return toAPIKey(rec), nil
}

// List returns every apikey owned by userID, with masked Key fields.
func (k *APIKeys) List(ctx context.Context, userID string) ([]*APIKey, error) {
	recs, err := k.store.ListAPIKeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*APIKey, 0, len(recs))
	for i := range recs {
		out = append(out, toAPIKey(&recs[i]))
	}
	return out, nil
}

// LookupByToken is the auth hot path. SHA256(token) → (apikey, account).
//
// Single-user mode: every key is owner-level; there is no per-agent ACL
// to resolve.
func (k *APIKeys) LookupByToken(ctx context.Context, token string) (*Resolved, error) {
	if token == "" {
		return nil, ErrInvalidCredentials
	}
	rec, err := k.store.LookupAPIKeyByHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	user, err := k.store.GetUser(ctx, rec.UserID)
	if err != nil {
		// Orphaned apikey (user deleted but apikey lingered). Treat as
		// invalid — the cascade should have caught this.
		return nil, ErrInvalidCredentials
	}
	if user.Status != StatusActive {
		return nil, ErrInvalidCredentials
	}
	// Single-user mode: every key is owner-level; no per-agent ACL.
	return &Resolved{
		APIKey:  *toAPIKey(rec),
		Account: *toAccount(user),
		Agents:  rec.AgentIDs,
	}, nil
}

func toAPIKey(rec *store.APIKeyRecord) *APIKey {
	if rec == nil {
		return nil
	}
	masked := rec.KeyPrefix
	if masked == "" {
		masked = "fc_********"
	} else {
		masked = masked + "****"
	}
	return &APIKey{
		ID:        rec.ID,
		UserID:    rec.UserID,
		Name:      rec.Name,
		Key:       masked,
		Type:      rec.Type,
		AgentIDs:  rec.AgentIDs,
		CreatedAt: rec.CreatedAt,
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// keyPrefix keeps a recognizable slice of the plaintext for UI display.
// 10 chars is enough to spot "your" key in a list while staying far below
// brute-force feasibility.
func keyPrefix(token string) string {
	if len(token) <= 10 {
		return token
	}
	return token[:10]
}

func newToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "fc_" + hex.EncodeToString(buf[:]), nil
}
