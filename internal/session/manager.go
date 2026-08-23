package session

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/provider"
)

// Session holds the message history for one conversation thread within
// a (channel, accountID, chatID) triple. session_key is the per-session
// opaque id; the triple identifies "where" the conversation lives.
type Session struct {
	mu               sync.Mutex
	Messages         []provider.Message
	LastConsolidated int // index of last consolidated message
	filePath         string
	snapshot         []provider.Message // undo snapshot
	store            SessionStore
	userID           string
	agentID          string
	sessionKey       string
	channel          string
	accountID        string
	chatID           string
	// projectID, when non-empty, is stamped on every SaveSession write
	// for this session. Set on the FIRST turn of a brand-new chat that
	// arrived with a project hint (URL `?project=<pid>`); for existing
	// rows it's read back via Manager.Get and late-bound here so the
	// next save preserves it.
	projectID string
	// provider and model are stamped onto assistant messages by
	// Append so session_messages rows record which LLM produced them.
	// Set per-turn by the agent loop via SetProviderModel.
	provider string
	model    string

	// parentSessionKey / parentForkSeq mark this session as a fork: it
	// inherits the parent's session_messages archive[0..parentForkSeq]
	// as a read-only prefix merged into LLM context + history at read
	// time (never copied). Empty parentSessionKey = a normal non-fork
	// session. Set once at creation (OpenForkSession) and reloaded by
	// getByKey; guard with mu alongside the other persisted fields.
	parentSessionKey string
	parentForkSeq    int

	// Steering: turnDepth counts in-flight HandleMessage turns for this
	// session (a counter, not a bool, so re-entrant/overlapping turns
	// don't strand the active flag). steerBuf holds user messages that
	// arrived mid-turn; the running ReAct loop drains them between tool
	// iterations. Both are guarded by mu. getByKey never touches these,
	// so a Manager.Get reload (which overwrites Messages) can't clobber a
	// pending steer.
	turnDepth int
	steerBuf  []provider.Message

	// Authorization pending: tool_calls parked awaiting the user's /yes
	// (ask mode intercepted them as outside-workspace/dangerous), plus
	// the user-approved batch drainApprovedPending runs at the top of the
	// next turn. Per-session — each conversation thread authorizes
	// independently. Guarded by mu alongside the other transient fields.
	pendingCalls    []provider.ToolCall
	approvedPending []provider.ToolCall
	pendingDesc     string
}

// SessionKey returns the opaque session_key this Session is bound to.
// Exposed so per-turn plumbing (e.g. the tool registry binding for
// goal-scoped tools) can address the right row without re-resolving
// the (channel, account, chat) quadruple every time.
func (s *Session) SessionKey() string { return s.sessionKey }

// ParentSessionKey returns the parent session_key for a forked session
// (empty for a normal non-fork session). The agent loop + history loader
// merge the parent's archive[0..ParentForkSeq] as a read-only prefix.
func (s *Session) ParentSessionKey() string { return s.parentSessionKey }

// ParentForkSeq is the inclusive fork point seq inside the parent's
// session_messages archive. Only meaningful when ParentSessionKey != "".
func (s *Session) ParentForkSeq() int { return s.parentForkSeq }

// PushPendingCalls parks intercepted tool_calls awaiting user /yes.
// A round's waiting calls are a batch; one /yes executes them all. desc
// is the human-readable reason surfaced in the authorization prompt.
func (s *Session) PushPendingCalls(calls []provider.ToolCall, desc string) {
	s.mu.Lock()
	s.pendingCalls = append(s.pendingCalls, calls...)
	if desc != "" {
		s.pendingDesc = desc
	}
	s.mu.Unlock()
}

// PopPendingCalls returns and clears the waiting tool_calls + desc.
func (s *Session) PopPendingCalls() []provider.ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.pendingCalls
	s.pendingCalls = nil
	s.pendingDesc = ""
	return calls
}

// PendingDesc returns the reason for the current pending authorization
// request (empty when nothing is pending).
func (s *Session) PendingDesc() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingDesc
}

// SetApprovedPending marks the given calls as user-authorized; the agent
// loop drains them at the top of the next turn and executes immediately.
func (s *Session) SetApprovedPending(calls []provider.ToolCall) {
	s.mu.Lock()
	s.approvedPending = calls
	s.mu.Unlock()
}

// DrainApprovedPending returns and clears the user-authorized calls.
func (s *Session) DrainApprovedPending() []provider.ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.approvedPending
	s.approvedPending = nil
	return c
}

// ClearPendingCalls drops any waiting authorization (used on /no).
func (s *Session) ClearPendingCalls() {
	s.mu.Lock()
	s.pendingCalls = nil
	s.pendingDesc = ""
	s.mu.Unlock()
}

// ctx returns a context tagged with this Session's user so the store layer
// can scope SQL by user_id. Falls back to context.Background() when no
// user is set; the store will then default to config.DefaultUserID.
//
// Also embeds the per-turn chatter (when set) so DBStore session writes
// (sessions.chatter_user_id / session_messages.chatter_user_id /
// session_events.chatter_user_id) can record the actual conversation
// participant. user_id stays = UserSpace owner; chatter is the
// additional dimension. Both tags are independent — empty chatter
// just leaves the column ”.
func (s *Session) ctx() context.Context {
	ctx := context.Background()
	if s.userID != "" {
		ctx = config.WithUserID(ctx, s.userID)
	}
	return ctx
}



// SetProviderModel binds the current LLM provider and model to this
// Session so Append stamps them onto assistant messages. Called by the
// agent loop alongside SetChatter.
func (s *Session) SetProviderModel(prov, mdl string) {
	s.mu.Lock()
	s.provider = prov
	s.model = mdl
	s.mu.Unlock()
}

// Manager manages sessions for one (user, agent). Sessions are keyed
// internally by an opaque session_key; the (channel, accountID, chatID)
// triple is what callers use to address "the conversation thread the
// user is in right now". The active session for that triple is the
// most recently updated row — `/new` mints a fresh one to start over.
//
// SessionStore is the optional persistence interface (DB-backed in
// production; nil in file-only mode for single-binary dev installs).
//
// Two parallel persistence shapes:
//   - GetSession / SaveSession operate on the LLM-facing working set
//     (post-compaction). This is what the agent loop reads/writes every
//     turn.
//   - AppendMessage / ListMessages operate on the append-only per-turn
//     archive (session_messages table). Compaction never touches it, so
//     UI history / audit reads see the original conversation regardless
//     of how many times the working set has been pruned/summarized.
type SessionStore interface {
	GetSession(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error)
	SaveSession(ctx context.Context, agentID, sessionKey, channel, accountID, chatID, projectID string, messages []provider.Message) error
	AppendMessage(ctx context.Context, agentID, sessionKey string, msg provider.Message) error
	// AppendMessageHidden writes one message to the session_messages
	// archive with llm_visible=0 so it stays out of the LLM working set,
	// summary, and recall — while remaining visible in web history,
	// which reads the archive directly. Used by regex-hook turns with
	// FeedToLLM=false. The in-memory Messages slice is untouched.
	AppendMessageHidden(ctx context.Context, agentID, sessionKey string, msg provider.Message) error
	ListMessages(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error)
	ListWebSessions(ctx context.Context, agentID string) ([]WebSession, error)
	DeleteSession(ctx context.Context, agentID, sessionKey string) error
	RenameSession(ctx context.Context, agentID, sessionKey, title string) error
	// MoveSession reassigns a session to a different project (or
	// detaches when projectID is ""). Used by the sidebar drag-and-drop
	// affordance; workspace file migration is the caller's job.
	MoveSession(ctx context.Context, agentID, sessionKey, projectID string) error
	// ResolveActiveSessionKey returns the most recent session_key for the
	// (channel, accountID, chatID) triple, or empty string if none.
	ResolveActiveSessionKey(ctx context.Context, agentID, channel, accountID, chatID string) (string, error)
	// LookupSessionTriple is the inverse — given a session_key, return
	// the conversation it belongs to. Returns ("","","",nil) when the
	// session doesn't exist (manager treats that as "not yet stored").
	LookupSessionTriple(ctx context.Context, agentID, sessionKey string) (channel, accountID, chatID string, err error)
	// LookupSessionProject returns the project_id stamped on the session
	// row, or "" for loose chats. Used by the agent runtime to thread
	// project context onto inbound messages so the workspace store and
	// sandbox both route to projects/<pid>/.
	LookupSessionProject(ctx context.Context, agentID, sessionKey string) (string, error)
	// CreateForkSession persists a brand-new session row carrying the
	// parent linkage. Called once at fork creation; subsequent
	// SaveSession calls preserve the parent (DB ON CONFLICT does not
	// touch parent_session_key/parent_fork_seq). Empty parentKey = a
	// normal non-fork session (forkSeq ignored).
	CreateForkSession(ctx context.Context, agentID, sessionKey, channel, accountID, chatID, projectID, parentKey string, forkSeq int) error
	// SessionParent returns the (parent_session_key, parent_fork_seq)
	// stamped on the session row — the read path powering LLM-context
	// and history merging for forked chats. ("",0,nil) for a normal
	// non-fork session or when the store has no row yet.
	SessionParent(ctx context.Context, agentID, sessionKey string) (parentKey string, forkSeq int, err error)
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	dataDir  string
	store    SessionStore
	userID   string
	agentID  string
}

func NewManager(dataDir string) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		dataDir:  dataDir,
	}
}

// NewManagerWithStoreForUser is the user-scoped constructor. Caller MUST
// supply a real user_id resolved from auth. We log and keep the Manager
// alive on empty input so a bad request cannot crash the whole gateway;
// downstream store calls will fail closed under the empty owner.
func NewManagerWithStoreForUser(dataDir string, st SessionStore, userID, agentID string) *Manager {
	if userID == "" {
		fmt.Fprintf(os.Stderr, "session.NewManagerWithStoreForUser: empty userID for agent %q\n", agentID)
	}
	return &Manager{
		sessions: make(map[string]*Session),
		dataDir:  dataDir,
		store:    st,
		userID:   userID,
		agentID:  agentID,
	}
}

// ctx returns a context tagged with this Manager's user for store calls.
func (m *Manager) ctx() context.Context {
	if m.userID == "" {
		return context.Background()
	}
	return config.WithUserID(context.Background(), m.userID)
}

// generateSessionKey mints an opaque session_key for a fresh
// conversation thread. The same generator is used regardless of channel
// — `s-<unix_ms>-<rand>`. The (channel, accountID, chatID) triple is
// stored alongside in dedicated columns; the literal session_key string
// no longer encodes channel info, so a `/new` command in IM can mint a
// second key under the same triple without colliding.
func generateSessionKey() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	var rand6 [6]byte
	if _, err := cryptorand.Read(rand6[:]); err != nil {
		// fall back to time-derived bytes — collision is extremely
		// unlikely once the timestamp prefix is in play
		now := time.Now().UnixNano()
		for i := range rand6 {
			rand6[i] = byte(now >> (i * 8))
		}
	}
	suffix := make([]byte, len(rand6))
	for i, b := range rand6 {
		suffix[i] = alphabet[int(b)%len(alphabet)]
	}
	return fmt.Sprintf("s-%d-%s", time.Now().UnixMilli(), suffix)
}

// resolveOrMintKey picks the active session_key for (channel,
// accountID, chatID) from the store, or mints a fresh one when nothing
// exists yet (the very first message in a conversation). Pre-existing
// rows from before the channel-triple migration may carry a key like
// `web_<sid>` or `wechat_<openid>` — they're matched by the backfilled
// triple, not by parsing the key, so the legacy format keeps working.
//
// New-row mint policy:
//   - web: session_key == chatID. Web's chatID *is* the per-conversation
//     identifier (the frontend generates one per "+New chat") so making
//     it equal the session_key keeps the URL `?session=` token stable
//     across reloads — no "URL changed after first message" surprises.
//   - everywhere else: mint an opaque `s-<unix_ms>-<rand>`. IM channels
//     reuse one chatID (the user's openid / chat_id) across many
//     sessions, so the session_key has to be independent for `/new` to
//     produce a sibling row.
func (m *Manager) resolveOrMintKey(channel, accountID, chatID string) string {
	if m.store != nil {
		if k, err := m.store.ResolveActiveSessionKey(m.ctx(), m.agentID, channel, accountID, chatID); err == nil && k != "" {
			return k
		}
	}
	if channel == "web" && chatID != "" {
		return chatID
	}
	return generateSessionKey()
}

// Get returns or creates the active session for the (channel, accountID,
// chatID) triple. The session_key is resolved server-side rather than
// derived from the inputs — see resolveOrMintKey.
//
// projectID is the "this chat belongs to project X" hint from the chat
// request (URL `?project=<pid>`). It only matters on first save: if the
// session row already has project_id stored, that wins; if the row is
// brand new, this hint is what gets persisted.
//
// In multi-replica deployments (store-backed mode), every Get() reloads
// Messages from the store so a request served by pod B sees writes made
// by pod A. Without this, each pod's in-memory cache drifts away from
// Postgres: the first refresh after a cross-pod write returns whichever
// pod-local snapshot happened to be warm. We deliberately overwrite
// Messages on the cached Session rather than re-creating the struct so
// transient fields (snapshot, LastConsolidated) survive.
//
// File-backed mode stays cache-first since there's only one process.
func (m *Manager) Get(channel, accountID, chatID, projectID string) *Session {
	key := m.resolveOrMintKey(channel, accountID, chatID)
	return m.getByKey(key, channel, accountID, chatID, projectID)
}

// GetByKey loads a specific session by its session_key. Used when the
// caller already has a key in hand (e.g. web history fetch from a URL
// `?session=…`) and wants to bypass the active-session lookup.
func (m *Manager) GetByKey(sessionKey string) *Session {
	return m.getByKey(sessionKey, "", "", "", "")
}

// LookupSessionProject returns the project_id of a session row (or ""
// if loose / not yet stored). Used by the agent runtime to populate
// InboundMessage.ProjectID so workspace IO routes to projects/<pid>/.
func (m *Manager) LookupSessionProject(sessionKey string) string {
	if m.store == nil || sessionKey == "" {
		return ""
	}
	pid, err := m.store.LookupSessionProject(m.ctx(), m.agentID, sessionKey)
	if err != nil {
		return ""
	}
	return pid
}

// LookupSessionTriple forwards to the store's session_key → triple
// lookup. Returns ("","","",nil) when the row doesn't exist, mirroring
// the SessionStore implementation. Callers should use SessionExists
// first if they need to distinguish "no row" from "row with empty
// triple" (e.g. file-backed dev mode where the store is nil).
func (m *Manager) LookupSessionTriple(sessionKey string) (channel, accountID, chatID string, err error) {
	if m.store == nil {
		return "", "", "", nil
	}
	return m.store.LookupSessionTriple(m.ctx(), m.agentID, sessionKey)
}

// SessionExists reports whether a session row already exists under the
// given session_key. Used by agent-side URL resolvers: a `?session=…`
// token can be either a canonical session_key or a legacy web chat_id,
// and the lookup needs a cheap way to tell which.
func (m *Manager) SessionExists(sessionKey string) bool {
	if m.store == nil {
		// File-backed mode has no negative-lookup primitive — assume
		// yes so the legacy chat_id fallback isn't preferred over the
		// caller's intent. The follow-up GetByKey will load whatever's
		// on disk (empty file → empty Session, harmless).
		return true
	}
	msgs, err := m.store.GetSession(m.ctx(), m.agentID, sessionKey)
	return err == nil && msgs != nil
}

// ResolveSessionKey turns a URL token (`?session=…`) into the
// canonical session_key. Accepts either:
//   - a session_key directly (the ID surfaced by ListWebSessions)
//   - a legacy web chat_id (older URLs and the frontend's freshly-
//     generated id on the *first* turn of a "+New chat")
//
// Returns the input unchanged when nothing matches — callers' downstream
// load/save will then create the row, which is correct for brand-new
// web chats where the URL token is the about-to-exist session_key.
func (m *Manager) ResolveSessionKey(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if m.SessionExists(sessionID) {
		return sessionID
	}
	if m.store != nil {
		if k, err := m.store.ResolveActiveSessionKey(m.ctx(), m.agentID, "web", "", sessionID); err == nil && k != "" {
			return k
		}
	}
	return sessionID
}

// OpenNewSession mints a brand new session under the same (channel,
// accountID, chatID) triple and returns its session_key. The next Get
// for that triple will pick it up (it has the freshest updated_at).
// Used by IM `/new` / `/reset` commands and any future "start new
// conversation" UI affordance.
func (m *Manager) OpenNewSession(channel, accountID, chatID string) string {
	key := generateSessionKey()
	if m.store != nil {
		// Persist an empty row immediately so the active-session lookup
		// for the next inbound message resolves to this key, not the
		// previous (still-newer-than-not-existing) row. IM `/new` is
		// always a loose chat (project_id=""); project chats are
		// minted lazily by the chat handler on first message.
		_ = m.store.SaveSession(m.ctx(), m.agentID, key, channel, accountID, chatID, "", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Session{
		filePath:   filepath.Join(m.dataDir, key+".jsonl"),
		store:      m.store,
		userID:     m.userID,
		agentID:    m.agentID,
		sessionKey: key,
		channel:    channel,
		accountID:  accountID,
		chatID:     chatID,
	}
	m.sessions[key] = s
	return key
}

// OpenForkSession creates a brand-new session B that inherits parent A's
// archive[0..forkSeq] as a read-only prefix. B's own archive and working
// set start empty; the prefix is merged in at read time (LLM context +
// history), never copied. B copies A's project_id so the fork clusters
// beside its parent in the sidebar. For web chats the chatID is forced to
// B's own session_key (the resolveOrMintKey invariant) so B resolves
// independently of A instead of being shadowed by A's active-key row.
//
// Returns B's session_key. The parent linkage is written via
// CreateForkSession (a one-shot insert); later SaveSession calls keep
// the parent columns because DB ON CONFLICT does not touch them.
func (m *Manager) OpenForkSession(channel, accountID, chatID, projectID, parentKey string, forkSeq int) string {
	key := generateSessionKey()
	if channel == "web" {
		chatID = key
	}
	if m.store != nil {
		_ = m.store.CreateForkSession(m.ctx(), m.agentID, key, channel, accountID, chatID, projectID, parentKey, forkSeq)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Session{
		filePath:         filepath.Join(m.dataDir, key+".jsonl"),
		store:            m.store,
		userID:           m.userID,
		agentID:          m.agentID,
		sessionKey:       key,
		channel:          channel,
		accountID:        accountID,
		chatID:           chatID,
		projectID:        projectID,
		parentSessionKey: parentKey,
		parentForkSeq:    forkSeq,
	}
	m.sessions[key] = s
	return key
}

func (m *Manager) getByKey(key, channel, accountID, chatID, projectID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[key]; ok {
		if m.store != nil {
			if msgs, err := m.store.GetSession(m.ctx(), m.agentID, key); err == nil {
				s.mu.Lock()
				s.Messages = msgs
				s.mu.Unlock()
			}
			// Reload parent linkage alongside messages so a forked
			// session cached before its row was written picks up the
			// parent prefix once the row lands.
			if pk, fs, err := m.store.SessionParent(m.ctx(), m.agentID, key); err == nil {
				s.mu.Lock()
				s.parentSessionKey = pk
				s.parentForkSeq = fs
				s.mu.Unlock()
			}
		}
		// Late-bind the triple + project on cached entries created via
		// GetByKey or earlier hint-less paths. Once stamped, project_id
		// on the persisted row is authoritative — we only ever fill in
		// the empty case so a hint mismatch can't overwrite the truth.
		if channel != "" || projectID != "" {
			s.mu.Lock()
			if s.channel == "" && channel != "" {
				s.channel, s.accountID, s.chatID = channel, accountID, chatID
			}
			if s.projectID == "" && projectID != "" {
				s.projectID = projectID
			}
			s.mu.Unlock()
		}
		return s
	}

	filePath := filepath.Join(m.dataDir, key+".jsonl")

	s := &Session{
		filePath:   filePath,
		store:      m.store,
		userID:     m.userID,
		agentID:    m.agentID,
		sessionKey: key,
		channel:    channel,
		accountID:  accountID,
		chatID:     chatID,
		projectID:  projectID,
	}

	// Load from store (DB) if available, otherwise from file
	if m.store != nil {
		msgs, err := m.store.GetSession(m.ctx(), m.agentID, key)
		if err == nil && len(msgs) > 0 {
			s.Messages = msgs
		}
		if pk, fs, err := m.store.SessionParent(m.ctx(), m.agentID, key); err == nil {
			s.parentSessionKey = pk
			s.parentForkSeq = fs
		}
	} else {
		s.load()
	}

	// Repair a working set left dangling by a mid-turn daemon death —
	// see healInterruptedTurn. Fresh loads only: a restart leaves no turn
	// in flight, so appending here can never race a live one (the cached
	// reload above is deliberately left alone for exactly that reason).
	s.healInterruptedTurn()

	m.sessions[key] = s
	return s
}

// Append adds a message to the session and persists it.
//
// Store-backed mode writes to TWO places:
//   - SaveSession overwrites the LLM-facing working set in the sessions
//     table (the array the agent loop reads next turn);
//   - AppendMessage inserts the new turn into session_messages, the
//     append-only archive that survives compaction.
//
// The archive write is best-effort (logged on failure but not surfaced)
// — losing one archive row is recoverable from the working set, and we
// don't want history to silently drop chat replies if the audit table
// hiccups.
// Key returns the opaque session_key this Session is bound to.
// Exposed so callers that need to tag external records by session
// (e.g. usage metering's per-session token rollup) don't have to
// reach into the struct.
func (s *Session) Key() string { return s.sessionKey }

// AgentID returns the owning agent's ID — the key under which this
// session's archive rows are stored. Used by the rollback path to
// delete the orphan user row from session_messages via the store.
func (s *Session) AgentID() string { return s.agentID }

func (s *Session) Append(msg provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Auto-set timestamp if not provided
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	// Stamp provider/model on assistant messages so the archive
	// records which LLM produced each response.
	if msg.Role == "assistant" && msg.Provider == "" && s.provider != "" {
		msg.Provider = s.provider
		msg.Model = s.model
	}

	s.Messages = append(s.Messages, msg)

	if s.store != nil {
		s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages)
		if err := s.store.AppendMessage(s.ctx(), s.agentID, s.sessionKey, msg); err != nil {
			fmt.Fprintf(os.Stderr, "session archive append error: %v\n", err)
		}
	} else {
		s.appendToFile(msg)
	}
}

// RollbackLastUser removes the trailing message iff it is an unanswered
// user turn — the last message in the working set AND role=user (the
// agent never replied, typically because the LLM call failed). It
// persists the trimmed working set via SaveSession but does NOT touch
// the session_messages archive; the caller deletes the matching orphan
// row so history-reload stays consistent. Returns false (no-op) if the
// last message isn't a user turn. Used by the web chat's "resend a
// failed message" flow.
func (s *Session) RollbackLastUser() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Messages) == 0 {
		return false
	}
	if s.Messages[len(s.Messages)-1].Role != "user" {
		return false
	}
	s.Messages = s.Messages[:len(s.Messages)-1]
	if s.store != nil {
		s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages)
	} else {
		s.rewriteFile()
	}
	return true
}

// AppendArchivedHidden writes msgs to the session_messages archive with
// llm_visible=0, WITHOUT adding them to the in-memory Messages slice and
// WITHOUT calling SaveSession. The LLM working set (and its sessions-table
// JSON mirror) therefore never holds these rows, so a GetSession reload
// can't pull them back either; but web history, which reads the archive,
// still shows them. Used by regex-hook turns whose hook has FeedToLLM=false.
func (s *Session) AppendArchivedHidden(msgs []provider.Message) {
	s.mu.Lock()
	store := s.store
	agentID := s.agentID
	sessionKey := s.sessionKey
	s.mu.Unlock()
	if store == nil {
		return
	}
	for _, m := range msgs {
		if m.Timestamp == 0 {
			m.Timestamp = time.Now().UnixMilli()
		}
		if err := store.AppendMessageHidden(s.ctx(), agentID, sessionKey, m); err != nil {
			fmt.Fprintf(os.Stderr, "session archive hidden append error: %v\n", err)
		}
	}
}

// ArchivedMessages returns the full append-only history for this session.
// Falls back to the in-memory working set when no store is configured or
// the archive is empty (e.g. file-backed mode, or a session created
// before the archive table existed).
func (s *Session) ArchivedMessages() []provider.Message {
	s.mu.Lock()
	store := s.store
	agentID := s.agentID
	sessionKey := s.sessionKey
	s.mu.Unlock()
	if store == nil {
		return s.GetMessages()
	}
	msgs, err := store.ListMessages(s.ctx(), agentID, sessionKey)
	if err != nil || len(msgs) == 0 {
		return s.GetMessages()
	}
	return msgs
}

// ParentPrefixMessages returns the parent's archived messages [0..forkSeq]
// (inclusive) for a forked session — the read-only conversation prefix
// merged into LLM context + web history at read time, never physically
// copied into B's archive. Returns nil for a normal non-fork session.
//
// Reads the parent's append-only archive (session_messages) so the prefix
// is a frozen snapshot: later compaction or new turns in the parent do
// not change what B sees.
func (s *Session) ParentPrefixMessages() []provider.Message {
	s.mu.Lock()
	pk := s.parentSessionKey
	fs := s.parentForkSeq
	store := s.store
	agentID := s.agentID
	s.mu.Unlock()
	if pk == "" || store == nil {
		return nil
	}
	archive, err := store.ListMessages(s.ctx(), agentID, pk)
	if err != nil || len(archive) == 0 {
		return nil
	}
	if fs < 0 {
		fs = 0
	}
	if fs >= len(archive) {
		fs = len(archive) - 1
	}
	return archive[:fs+1]
}

// GetMessages returns a copy of all messages.
func (s *Session) GetMessages() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	msgs := make([]provider.Message, len(s.Messages))
	copy(msgs, s.Messages)
	return msgs
}

// BeginTurn marks a HandleMessage turn as in-flight for this session.
// Paired with EndTurn. Steering messages are only accepted while at
// least one turn is active.
func (s *Session) BeginTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnDepth++
}

// EndTurn marks a turn as finished. When the last in-flight turn ends it
// returns any steer messages still buffered (the end-of-turn race: a
// message pushed after the loop's final drain). Callers redispatch the
// leftovers as a fresh turn.
func (s *Session) EndTurn() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnDepth > 0 {
		s.turnDepth--
	}
	if s.turnDepth > 0 || len(s.steerBuf) == 0 {
		return nil
	}
	leftover := s.steerBuf
	s.steerBuf = nil
	return leftover
}

// PushSteerIfActive buffers a steering message iff a turn is currently
// in-flight. Returns false when no turn is active, so the caller can
// fall back to dispatching the message as a normal new turn. The return
// value is the single source of truth — there is deliberately no
// separate "is running" probe to race against.
func (s *Session) PushSteerIfActive(msg provider.Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnDepth == 0 {
		return false
	}
	s.steerBuf = append(s.steerBuf, msg)
	return true
}

// DrainSteer atomically returns and clears the buffered steer messages.
// The running loop calls this between tool iterations.
func (s *Session) DrainSteer() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steerBuf) == 0 {
		return nil
	}
	drained := s.steerBuf
	s.steerBuf = nil
	return drained
}

// UnconsolidatedCount returns the number of messages since last consolidation.
func (s *Session) UnconsolidatedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Messages) - s.LastConsolidated
}

// MarkConsolidated updates the consolidation pointer.
func (s *Session) MarkConsolidated(index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastConsolidated = index
}

// ReplaceMessages replaces all session messages with the given list.
// This is used after context compaction to trim the session.
func (s *Session) ReplaceMessages(msgs []provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Messages = make([]provider.Message, len(msgs))
	copy(s.Messages, msgs)
	s.LastConsolidated = 0

	if s.store != nil {
		s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages)
	} else {
		s.rewriteFile()
	}
}

// Clear resets the session messages.
func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = nil
	s.LastConsolidated = 0
	if s.store != nil {
		s.store.DeleteSession(s.ctx(), s.agentID, s.sessionKey)
	} else {
		os.Remove(s.filePath)
	}
}

// ToolResultStoppedNote pads a tool result that never arrived because the
// turn was stopped (user abort / turn exit) before the tool returned.
const ToolResultStoppedNote = "(stopped — execution was interrupted before the tool returned)"

// toolResultCrashNote pads a tool result that never arrived because the
// daemon died mid-turn: the pad may understate reality — the tool could
// have run to completion in the outside world without the result ever
// being recorded.
const toolResultCrashNote = "(interrupted — the service restarted before this tool returned; it may have partially executed)"

// turnAbortedCrashNote / turnAbortedStopNote are appended (user role,
// metadata.turnAborted) when a turn ends without answering its tool_calls,
// so the model resumes across an explicit boundary instead of
// hallucinating continuity over the gap. Crash flavor: the daemon died
// mid-turn (load-time heal). Stop flavor: turn-exit defer — the run was
// stopped or errored out, and the defer can't tell which, so the wording
// stays neutral.
const turnAbortedCrashNote = "[system] 上一轮执行被中断(服务重启):被中止的工具可能已部分执行,后台进程可能仍在运行。请基于当前实际状态核实后再继续,不要假设中断前的操作已完成。"
const turnAbortedStopNote = "[system] 上一轮执行未正常结束(被停止或出错退出):被中止的工具可能已部分执行,后台进程可能仍在运行。继续时请基于当前实际状态核实,不要假设此前的操作已完成。"

// PadOrphanToolResults pads the latest assistant message's unanswered
// tool_calls with padText. History whose assistant tool_calls lack results
// is rejected by OpenAI-compatible providers and semantically ambiguous to
// every model. Runs on turn exit (deferred in the agent loop) and after a
// crash-restart load. Returns true when at least one pad was appended.
func (s *Session) PadOrphanToolResults(padText string) bool {
	msgs := s.GetMessages()
	// Walk back to the latest assistant message; if it has no tool_calls
	// or all tool_calls already have results after it, nothing to do.
	lastAssistantIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			lastAssistantIdx = i
			break
		}
	}
	if lastAssistantIdx < 0 {
		return false
	}
	resolved := make(map[string]bool)
	for _, m := range msgs[lastAssistantIdx+1:] {
		if m.Role == "tool" && m.ToolCallID != "" {
			resolved[m.ToolCallID] = true
		}
	}
	padded := false
	for _, tc := range msgs[lastAssistantIdx].ToolCalls {
		if resolved[tc.ID] {
			continue
		}
		slog.Warn("padding orphan tool_use with stopped result",
			"session", s.sessionKey, "toolCallID", tc.ID, "tool", tc.Function.Name)
		s.Append(provider.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    padText,
		})
		padded = true
	}
	return padded
}

// healInterruptedTurn repairs a cold-loaded working set after a daemon
// crash. When the persisted history ends with unanswered tool_calls, the
// deferred PadOrphanToolResults never ran (process death skips defers),
// and the next LLM request would carry an invalid assistant/tool sequence
// — the "restart kills a long task forever" failure mode. Pads the
// dangling calls, then appends a user-role turn-aborted marker so the
// model knows the boundary it is resuming across. No-op on clean
// histories, so it is safe to run on every fresh load.
func (s *Session) healInterruptedTurn() {
	if !s.PadOrphanToolResults(toolResultCrashNote) {
		return
	}
	s.Append(provider.Message{
		Role:     "user",
		Content:  turnAbortedCrashNote,
		Metadata: map[string]any{"turnAborted": true},
	})
}

// PadOrphanToolResultsAndMarkAborted is the turn-exit variant of
// PadOrphanToolResults: when repairs were made, the turn ended without
// answering its tool_calls (user Stop, premature exit), so append the
// stop-flavored turn-aborted marker alongside the pads — the model then
// knows the boundary it resumes across instead of treating the stopped
// tools as if they had never run.
func (s *Session) PadOrphanToolResultsAndMarkAborted(padText string) {
	if !s.PadOrphanToolResults(padText) {
		return
	}
	s.Append(provider.Message{
		Role:     "user",
		Content:  turnAbortedStopNote,
		Metadata: map[string]any{"turnAborted": true},
	})
}

func (s *Session) load() {
	f, err := os.Open(s.filePath)
	if err != nil {
		return // file doesn't exist yet
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var msg provider.Message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		s.Messages = append(s.Messages, msg)
	}
}

func (s *Session) rewriteFile() {
	dir := filepath.Dir(s.filePath)
	os.MkdirAll(dir, 0o755)

	f, err := os.Create(s.filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session rewrite error: %v\n", err)
		return
	}
	defer f.Close()

	for _, msg := range s.Messages {
		data, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		f.Write(data)
		f.Write([]byte("\n"))
	}
}

func (s *Session) appendToFile(msg provider.Message) {
	dir := filepath.Dir(s.filePath)
	os.MkdirAll(dir, 0o755)

	f, err := os.OpenFile(s.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session persist error: %v\n", err)
		return
	}
	defer f.Close()

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	f.Write(data)
	f.Write([]byte("\n"))
}

// WebSession holds metadata for one chat session surfaced to the
// dashboard. Despite the historical name it now spans every channel —
// the Channel field tells callers which one to render the icon for.
//
// ID is the session_key (the row's PK), not the chat_id. Older URLs
// pointing at a chat_id still resolve via the agent-side fallback
// (ResolveSessionKey) so existing bookmarks don't break.
type WebSession struct {
	ID        string `json:"id"`
	Channel   string `json:"channel,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	ChatID    string `json:"chatId,omitempty"`
	// ProjectID groups this chat under a per-(user, agent) project
	// folder. Empty = loose chat. Surfaced so the sidebar can section
	// chats by project.
	ProjectID string `json:"projectId,omitempty"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	CreatedAt int64  `json:"createdAt"` // unix ms
	UpdatedAt int64  `json:"updatedAt"` // unix ms
	// ThumbnailURL is the first image_url attached to the FIRST user
	// turn of the session, surfaced so the sidebar can show "image +
	// text" instead of just the text label for multimodal chats.
	// Empty for sessions whose opening message had no image.
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
	// ChatterUserID is the actual conversation participant. Differs
	// from user_id when an IM sender is resolved to a per-sender
	// app_user under the channel owner's UserSpace.
}

// ListWebSessions scans session files for web chat sessions and returns
// a list with id, title, preview, and timestamps.
func (m *Manager) ListWebSessions() []WebSession {
	if m.store != nil {
		sessions, err := m.store.ListWebSessions(m.ctx(), m.agentID)
		if err == nil {
			return sessions
		}
	}
	pattern := filepath.Join(m.dataDir, "web_*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}

	var sessions []WebSession
	for _, f := range files {
		base := filepath.Base(f)
		// "web_<sessionId>.jsonl" -> "<sessionId>"
		sessionId := strings.TrimPrefix(base, "web_")
		sessionId = strings.TrimSuffix(sessionId, ".jsonl")

		info, err := os.Stat(f)
		if err != nil {
			continue
		}

		// Read first user message as preview
		preview := ""
		thumb := ""
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(fh)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			// Multimodal user turns store text inside content_parts and
			// leave content empty — read both shapes so the preview
			// doesn't latch onto a later plain message and mislabel the
			// session, and pull the first image_url so the sidebar can
			// surface a thumbnail.
			var msg struct {
				Role         string                 `json:"role"`
				Content      string                 `json:"content"`
				ContentParts []provider.ContentPart `json:"content_parts"`
			}
			if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Role != "user" {
				continue
			}
			text := msg.Content
			img := ""
			if text == "" {
				var parts []string
				for _, p := range msg.ContentParts {
					if p.Type == "text" && p.Text != "" {
						parts = append(parts, p.Text)
					}
				}
				text = strings.Join(parts, "\n")
			}
			text = provider.StripAttachedPrefix(text)
			for _, p := range msg.ContentParts {
				if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
					img = p.ImageURL.URL
					break
				}
			}
			if text == "" && img == "" {
				continue
			}
			preview = text
			if preview == "" {
				preview = "[image]"
			}
			if len(preview) > 100 {
				preview = preview[:100] + "..."
			}
			thumb = img
			break
		}
		fh.Close()

		if preview == "" {
			continue // skip empty sessions
		}

		// Read title from metadata file, fallback to preview
		title := m.readSessionTitle(sessionId)
		if title == "" {
			title = preview
			if len(title) > 60 {
				title = title[:60] + "..."
			}
		}

		sessions = append(sessions, WebSession{
			ID:           sessionId,
			Title:        title,
			Preview:      preview,
			ThumbnailURL: thumb,
			CreatedAt:    info.ModTime().UnixMilli(),
			UpdatedAt:    info.ModTime().UnixMilli(),
		})
	}

	// Sort by updatedAt descending (newest first)
	for i := 0; i < len(sessions); i++ {
		for j := i + 1; j < len(sessions); j++ {
			if sessions[j].UpdatedAt > sessions[i].UpdatedAt {
				sessions[i], sessions[j] = sessions[j], sessions[i]
			}
		}
	}

	return sessions
}

// resolveWebSessionKey maps a web sessionId (the URL `?session=` token,
// which is the conversation's chat_id) to its current session_key. New
// rows have an opaque session_key (different from chat_id); legacy rows
// still carry the `web_<sid>` form. Falls back to the legacy literal
// when no row exists yet so file-backed mode and brand-new sessions
// don't error on rename/delete.
func (m *Manager) resolveWebSessionKey(sessionId string) string {
	if m.store != nil {
		if k, err := m.store.ResolveActiveSessionKey(m.ctx(), m.agentID, "web", "", sessionId); err == nil && k != "" {
			return k
		}
	}
	return "web_" + sessionId
}

// DeleteSessionByID resolves a URL token (session_key or legacy web
// chat_id) and deletes the matching session. Channel-agnostic — used
// by the dashboard to delete any-channel chats.
func (m *Manager) DeleteSessionByID(sessionId string) error {
	key := m.ResolveSessionKey(sessionId)
	if m.store != nil {
		if err := m.store.DeleteSession(m.ctx(), m.agentID, key); err != nil {
			// Leave the cached entry intact on failure (e.g. the parent
			// still has fork children) so a rejected delete doesn't drop
			// the in-memory session a concurrent request is using.
			return err
		}
		m.mu.Lock()
		delete(m.sessions, key)
		m.mu.Unlock()
		return nil
	}
	// File-backed mode only had a "web_<sid>" filename convention; non-
	// web sessions don't reach this path in dev mode, so the legacy
	// fallback in DeleteWebSession is sufficient.
	return m.DeleteWebSession(sessionId)
}

// RenameSessionByID resolves a URL token and renames the matching
// session.
func (m *Manager) RenameSessionByID(sessionId, title string) error {
	key := m.ResolveSessionKey(sessionId)
	if m.store != nil {
		return m.store.RenameSession(m.ctx(), m.agentID, key, title)
	}
	return m.RenameWebSession(sessionId, title)
}

// MoveSessionByID reassigns a session to a different project (or
// detaches when projectID is ""). Resolves either a session_key or a
// legacy web chat_id. Drops the in-memory cache entry so the next
// Get re-loads the row with the freshly-stamped project_id — without
// this drop, an open chat would keep saving with the old project_id
// even after the sidebar shows it under a new project.
//
// File-backed mode is a no-op (no project concept) — callers that
// only run dev mode shouldn't reach this path.
func (m *Manager) MoveSessionByID(sessionId, projectID string) error {
	key := m.ResolveSessionKey(sessionId)
	m.mu.Lock()
	if s, ok := m.sessions[key]; ok {
		s.mu.Lock()
		s.projectID = projectID
		s.mu.Unlock()
	}
	m.mu.Unlock()
	if m.store != nil {
		return m.store.MoveSession(m.ctx(), m.agentID, key, projectID)
	}
	return nil
}

// DeleteWebSession removes a web chat session file and its metadata.
func (m *Manager) DeleteWebSession(sessionId string) error {
	key := m.resolveWebSessionKey(sessionId)

	// Remove from in-memory cache
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()

	if m.store != nil {
		return m.store.DeleteSession(m.ctx(), m.agentID, key)
	}

	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")
	sessionFile := filepath.Join(m.dataDir, "web_"+safeId+".jsonl")
	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	os.Remove(metaFile)
	return os.Remove(sessionFile)
}

// RenameWebSession sets a custom title for a web chat session.
func (m *Manager) RenameWebSession(sessionId, title string) error {
	if m.store != nil {
		return m.store.RenameSession(m.ctx(), m.agentID, m.resolveWebSessionKey(sessionId), title)
	}

	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")
	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	data, _ := json.Marshal(map[string]string{"title": title})
	return os.WriteFile(metaFile, data, 0o644)
}

// readSessionTitle reads the title from a session metadata file.
func (m *Manager) readSessionTitle(sessionId string) string {
	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")

	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	data, err := os.ReadFile(metaFile)
	if err != nil {
		return ""
	}
	var meta struct {
		Title string `json:"title"`
	}
	json.Unmarshal(data, &meta)
	return meta.Title
}

// Snapshot saves the current message list as a restore point (for undo).
func (s *Session) Snapshot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = make([]provider.Message, len(s.Messages))
	copy(s.snapshot, s.Messages)
}

// Undo restores the last snapshot. Returns false if no snapshot exists.
func (s *Session) Undo() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		return false
	}
	s.Messages = make([]provider.Message, len(s.snapshot))
	copy(s.Messages, s.snapshot)
	s.snapshot = nil
	s.LastConsolidated = 0
	s.rewriteFile()
	return true
}

// HasSnapshot returns true if an undo snapshot exists.
func (s *Session) HasSnapshot() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot != nil
}
