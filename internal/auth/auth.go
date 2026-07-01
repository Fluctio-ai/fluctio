// Package auth resolves an HTTP request to a user identity. It supports
// two credential types:
//   - cookie session ("fastclaw_session"): set by /api/login, validated
//     against the web_sessions table; used by the web UI
//   - Bearer apikey: validated against the apikeys table; used by API
//     consumers and CLI clients
//
// Both paths funnel into the same Identity struct stamped onto ctx via
// config.WithUserID. There is no anonymous "local" fallback — requests
// without valid credentials get 401.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// SessionCookieName is the cookie that backs the web UI's login state.
const SessionCookieName = "fastclaw_session"

// SessionTTL is how long a freshly-issued login cookie is valid.
const SessionTTL = 30 * 24 * time.Hour

// Identity is the resolved caller for one request.
type Identity struct {
	UserID string
	Role   string

	// AuthMethod is "session" or "apikey".
	AuthMethod string

	// APIKeyID is set when AuthMethod=="apikey".
	APIKeyID string

}

// EffectiveUserID is the caller. Single-user mode has no impersonation,
// so this is always UserID.
func (i Identity) EffectiveUserID() string {
	return i.UserID
}

// CanAccessAgent answers "is this caller authorized for agentID?"
// Single-user mode: the owner owns every agent, so any authenticated
// caller is authorized. (Per-agent ownership is still enforced by the
// caller via UserSpace lookup.)
func (i Identity) CanAccessAgent(agentID string) bool {
	return true
}

// CanAdminPlatform answers "may this caller hit /api/admin/* and other
// platform-wide mutating endpoints?" Only super_admin sessions and
// type=admin apikeys qualify. Distinct from CanAccessAgent because a
// super_admin's type=user/agent apikey deliberately downgrades them to
// the narrower scope they signed it for.
func (i Identity) CanAdminPlatform() bool {
	// Single-user mode: the owner (super_admin session or any apikey) is
	// the platform admin.
	return i.Role == users.RoleSuperAdmin || i.AuthMethod == "apikey"
}

// CanCreateAgent answers "may this caller create new agents?"
// Single-user mode: the owner can always create agents.
func (i Identity) CanCreateAgent() bool {
	return true
}

type identityKey struct{}

// WithIdentity stamps the resolved identity onto ctx so handlers can read
// it without re-validating credentials.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	ctx = context.WithValue(ctx, identityKey{}, id)
	if uid := id.EffectiveUserID(); uid != "" {
		ctx = config.WithUserID(ctx, uid)
	}
	return ctx
}

// FromContext returns the resolved identity stamped by Middleware. The
// bool is false if no auth has run, which means a route is misconfigured
// (every API route must go through Middleware first).
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	v, ok := ctx.Value(identityKey{}).(Identity)
	return v, ok
}

// Resolver loads accounts, apikeys, and web sessions from the store.
type Resolver struct {
	store    store.Store
	apikeys  *users.APIKeys
	accounts *users.Accounts
}

// NewResolver returns a resolver bound to the platform store.
func NewResolver(st store.Store) (*Resolver, error) {
	if st == nil {
		return nil, errors.New("auth.NewResolver: store is required")
	}
	ak, err := users.NewAPIKeys(st)
	if err != nil {
		return nil, err
	}
	ac, err := users.NewAccounts(st)
	if err != nil {
		return nil, err
	}
	return &Resolver{store: st, apikeys: ak, accounts: ac}, nil
}

// IssueSession creates a web session for userID and returns the cookie.
// Caller writes the cookie to the response.
func (r *Resolver) IssueSession(ctx context.Context, userID string) (*http.Cookie, error) {
	sid, err := newSID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &store.WebSessionRecord{
		SID:       sid,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(SessionTTL),
	}
	if err := r.store.CreateWebSession(ctx, rec); err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  rec.ExpiresAt,
	}, nil
}

// RevokeSession drops a session from the store.
func (r *Resolver) RevokeSession(ctx context.Context, sid string) error {
	return r.store.DeleteWebSession(ctx, sid)
}

// ResolveSession turns a cookie SID into an Identity.
func (r *Resolver) ResolveSession(ctx context.Context, sid string) (Identity, error) {
	if sid == "" {
		return Identity{}, ErrUnauthorized
	}
	sess, err := r.store.GetWebSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Identity{}, ErrUnauthorized
		}
		return Identity{}, err
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		_ = r.store.DeleteWebSession(ctx, sid)
		return Identity{}, ErrUnauthorized
	}
	user, err := r.accounts.Get(ctx, sess.UserID)
	if err != nil {
		return Identity{}, ErrUnauthorized
	}
	if user.Status != users.StatusActive {
		return Identity{}, ErrUnauthorized
	}
	return Identity{
		UserID:     user.ID,
		Role:       user.Role,
		AuthMethod: "session",
	}, nil
}

// ResolveBearer turns a Bearer token into an Identity.
func (r *Resolver) ResolveBearer(ctx context.Context, token string) (Identity, error) {
	res, err := r.apikeys.LookupByToken(ctx, token)
	if err != nil {
		if errors.Is(err, users.ErrInvalidCredentials) {
			return Identity{}, ErrUnauthorized
		}
		return Identity{}, err
	}
	return Identity{
		UserID:     res.Account.ID,
		Role:       res.Account.Role,
		AuthMethod: "apikey",
		APIKeyID:   res.APIKey.ID,
	}, nil
}

// ErrUnauthorized is returned when no valid credential is present.
var ErrUnauthorized = errors.New("unauthorized")

// extract returns the bearer token (if any) and session cookie SID (if
// any) from a request. A `?token=` query param is also accepted, but
// ONLY on the narrow set of paths that legitimately need it — file
// downloads (which the browser can't add an Authorization header to
// when rendered via <img> / <a download>) and the chat SSE
// subscription (EventSource has no header API). Everywhere else the
// query-param fallback is denied: tokens in URLs leak via Referer,
// browser history, reverse-proxy access logs, and observability
// pipelines. Header-only enforcement on /v1/* and the rest of /api/*
// closes that leak surface; CLI scripts that previously built
// `?token=` URLs for those endpoints must switch to
// `Authorization: Bearer <token>` (every HTTP client supports it).
func extract(r *http.Request) (bearer, sid string) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		sid = c.Value
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if t := strings.TrimPrefix(h, "Bearer "); t != h {
			bearer = t
		}
	} else if t := r.URL.Query().Get("token"); t != "" && queryTokenAllowed(r) {
		bearer = t
	}
	return bearer, sid
}

// queryTokenAllowed gates the `?token=` bearer fallback to a
// narrow allowlist of paths whose clients have no other way to
// attach an Authorization header.
//
// Allowed:
//   - GET /api/agents/<id>/files/...        — workspace file download
//   - GET /api/agents/<id>/files.zip        — workspace archive
//   - GET /api/agents/<id>/system-files/<n> — identity-file fetch (rare)
//   - GET /api/chat/subscribe               — EventSource SSE stream
//
// Everything else (/v1/*, /api/chat, /api/agents/<id> JSON, …) must
// use the Authorization header. Deliberately *not* a prefix match
// on /api/agents/<id>/files since some workspace endpoints under
// that prefix accept POST/PUT bodies — limit to GET so a write
// path can never authenticate via a logged URL.
func queryTokenAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/agents/") && strings.Contains(p, "/files"):
		return true
	case strings.HasPrefix(p, "/api/agents/") && strings.Contains(p, "/system-files/"):
		return true
	case p == "/api/chat/subscribe":
		return true
	}
	return false
}

// Middleware enforces auth on every wrapped route. 401 on no/invalid
// credentials. Resolves ?actAs= for super_admins.
func (r *Resolver) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, err := r.resolve(req)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		req = req.WithContext(WithIdentity(req.Context(), ident))
		next(w, req)
	}
}

// Optional resolves credentials when present but lets unauthenticated
// requests through. Used for /api/status during onboarding so the
// onboarding UI can probe the install before any user exists.
func (r *Resolver) Optional(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, err := r.resolve(req)
		if err == nil {
			req = req.WithContext(WithIdentity(req.Context(), ident))
		}
		next(w, req)
	}
}

func (r *Resolver) resolve(req *http.Request) (Identity, error) {
	bearer, sid := extract(req)

	var ident Identity
	var err error
	if sid != "" {
		ident, err = r.ResolveSession(req.Context(), sid)
		if err == nil {
			goto done
		}
	}
	if bearer != "" {
		ident, err = r.ResolveBearer(req.Context(), bearer)
		if err == nil {
			goto done
		}
	}
	return Identity{}, ErrUnauthorized

done:
	return ident, nil
}

// RequireSuperAdmin returns a middleware that 403s any non-super-admin
// caller. Wraps another middleware (typically the auth Middleware).
//
// This is the strictest gate: it requires the live caller's identity to
// be super_admin regardless of how they authenticated. A super_admin
// using a type=user apikey is rejected — that's the deliberate downgrade
// the user signed up for when they issued the narrower key. For routes
// that should accept either path (admin session OR type=admin apikey),
// use RequirePlatformAdmin instead.
func RequireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, ok := FromContext(req.Context())
		if !ok || ident.Role != users.RoleSuperAdmin {
			writeForbidden(w, "super_admin required")
			return
		}
		next(w, req)
	}
}

// RequirePlatformAdmin gates handlers that should accept any platform
// admin — session super_admin OR type=admin apikey. Same authority as
// RequireSuperAdmin in terms of what's allowed; just doesn't require the
// session path.
func RequirePlatformAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, ok := FromContext(req.Context())
		if !ok || !ident.CanAdminPlatform() {
			writeForbidden(w, "platform admin required")
			return
		}
		next(w, req)
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
}

func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"ok":false,"error":"` + msg + `"}`))
}

func newSID() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
