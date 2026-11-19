package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// Manager is the protocol-neutral session API. Create and Rotate return the
// value whose opaque ID a transport adapter hands to the client (the Web
// adapter installs it as a cookie). Rotate preserves the original absolute
// lifetime and revokes the previous ID; RedisStore.Rotate documents the window
// in which both IDs remain valid while a rotation is in flight.
//
// When max_sessions is positive, Create enforces it through the subject index:
// OnExceedRevokeOldest revokes the subject's earliest-issued live session until
// there is room, and OnExceedRejectNew reports ErrTooManySessions. The limit is
// best effort under concurrency -- parallel Creates can briefly exceed it by up
// to their own number before the next Create converges -- and it needs an
// IndexedStore: without one Create reports ErrNotSupported instead of silently
// allowing the session, and construction fails when the store is already known.
//
// ListBySubject, RevokeBySubject, and RevokeByDigest work on a subject's
// sessions without holding their IDs: the activity list behind a "my devices"
// page, signing a user out everywhere, and ending one listed session. They need
// a Store that implements IndexedStore, as the built-in memory and Redis stores
// do, and return ErrNotSupported otherwise. SessionInfo.Digest, like IDDigest,
// is a handle that names a session and cannot authenticate as it. Changing a
// subject's credentials and then calling RevokeBySubject in that order is what
// ends their sessions; a session created while the revocation runs may survive
// it. Sessions created before a deployment adopted the index are not indexed
// and expire on their own schedule. Each method returns ErrStopped once the
// plugin stopped and a parameter error for an empty subject.
//
// Observers supplied with WithObserver or contributed through
// ObserverContributor are notified synchronously after every operation.
type Manager interface {
	Create(ctx context.Context, subject string, attributes map[string]any) (Session, error)
	Rotate(ctx context.Context, id string) (Session, error)
	Revoke(ctx context.Context, id string) error
	// ListBySubject returns the live sessions of subject, in no particular order.
	ListBySubject(ctx context.Context, subject string) ([]SessionInfo, error)
	// RevokeBySubject deletes every live session of subject and returns how many
	// it deleted; it is idempotent.
	RevokeBySubject(ctx context.Context, subject string) (int, error)
	// RevokeByDigest deletes the session named by digest only if it belongs to
	// subject, and reports whether it deleted one. A malformed digest is an
	// error, distinct from an unknown or foreign one, which report false.
	RevokeByDigest(ctx context.Context, subject, digest string) (bool, error)
}

// AttributeManager is the extended session API: Manager plus in-place
// attribute maintenance and the shared verification check behind Authenticate.
// It is a separate contract so Manager keeps its published method set and an
// application or test double that implements only the lifecycle keeps
// compiling; only a consumer that needs the methods below depends on
// AttributeManager. The built-in Plugin implements both, and its Definition
// exports both.
//
// UpdateAttributes replaces a live session's attributes in place: use it for a
// profile refresh, never for a privilege change. A privilege change must go
// through Rotate (new ID, same absolute lifetime) or Revoke followed by a new
// Create, so a credential minted under the old authority cannot keep the new
// one. UpdateAttributes does not extend the absolute lifetime, reset the idle
// lease, or revive a session that expired, was revoked, or never existed
// (ErrNotFound). It needs a Store that implements AttributeStore, as the
// built-in memory and Redis stores do, and returns ErrNotSupported otherwise.
//
// PatchAttributes applies a delta to a live session's attributes: it removes
// every key in unset and assigns every key in set, so concurrent patches of
// different attributes do not overwrite each other the way whole-map updates
// do. It has the same lifetime and revival rules as UpdateAttributes, needs a
// Store that implements PatchStore, and reports ErrNotSupported otherwise. A
// key named in both set and unset is a parameter error; an empty patch is a
// lookup that still satisfies the lifetime rule. The Redis implementation
// merges optimistically and reports ErrConflict when its bounded retries are
// exhausted.
//
// Verify is the single session check behind Authenticate. It renews the idle
// lease of a live session (throttled by touch_interval, exactly as
// Authenticate always did) and classifies every other outcome as a Verdict.
// Verdicts are for the host's own auditing and metrics; a transport must never
// render one to a client, because distinguishing "malformed" from "unknown"
// would let a caller probe which IDs exist.
type AttributeManager interface {
	Manager
	// UpdateAttributes replaces the attributes of the live session id. It
	// preserves ID, Subject, IssuedAt, ExpiresAt, and the idle lease, and
	// reports ErrNotFound for an expired, revoked, or unknown session.
	UpdateAttributes(ctx context.Context, id string, attributes map[string]any) (Session, error)
	// PatchAttributes removes every attribute key in unset and then assigns
	// every key in set, preserving ID, Subject, IssuedAt, ExpiresAt, and the
	// idle lease, and reporting ErrNotFound for an expired, revoked, or unknown
	// session. A key named in both set and unset is a parameter error; an empty
	// patch returns the current session.
	PatchAttributes(ctx context.Context, id string, set map[string]any, unset []string) (Session, error)
	// Verify checks a presented session ID, renewing the idle lease of a live
	// session, and reports the internal Verdict. Only VerdictValid is success;
	// a StoreError also returns the underlying error. A stopped manager and an
	// unusable context are StoreError with ErrStopped or the context error.
	Verify(ctx context.Context, id string) (Session, Verdict, error)
}

type manager struct {
	store     Store
	cfg       normalizedConfig
	now       func() time.Time
	random    io.Reader
	observers []Observer
	closed    atomic.Bool
}

var (
	_ Manager          = (*manager)(nil)
	_ AttributeManager = (*manager)(nil)
)

func (m *manager) clock() time.Time {
	if m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

func (m *manager) Create(ctx context.Context, subject string, attributes map[string]any) (Session, error) {
	if m.closed.Load() {
		return Session{}, ErrStopped
	}
	if err := contextError(ctx); err != nil {
		return Session{}, err
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return Session{}, errors.New("session: subject cannot be empty")
	}
	normalizedAttributes, err := normalizeAttributes(attributes)
	if err != nil {
		return Session{}, err
	}
	if err := m.enforceSessionLimit(ctx, subject); err != nil {
		return Session{}, err
	}
	now := m.clock().Truncate(time.Millisecond)
	for range 4 {
		id, err := m.newID()
		if err != nil {
			return Session{}, err
		}
		value := Session{ID: id, Subject: subject, Attributes: normalizedAttributes, IssuedAt: now, ExpiresAt: now.Add(m.cfg.ttl)}
		err = m.store.Create(ctx, value, m.cfg.idleTTL)
		if errors.Is(err, ErrAlreadyExists) {
			continue
		}
		if err != nil {
			return Session{}, err
		}
		m.notify(ctx, Event{Kind: EventCreated, Subject: value.Subject, Digest: IDDigest(id), Verdict: VerdictValid})
		return cloneSession(value), nil
	}
	return Session{}, errors.New("session: could not allocate a unique session ID")
}

func (m *manager) Rotate(ctx context.Context, id string) (Session, error) {
	if m.closed.Load() {
		return Session{}, ErrStopped
	}
	if !validID(id, m.cfg.idBytes) {
		return Session{}, ErrInvalidCookie
	}
	current, found, err := m.store.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrNotFound
	}
	for range 4 {
		newID, err := m.newID()
		if err != nil {
			return Session{}, err
		}
		replacement := cloneSession(current)
		replacement.ID = newID
		err = m.store.Rotate(ctx, id, replacement, m.cfg.idleTTL)
		if errors.Is(err, ErrAlreadyExists) {
			continue
		}
		if err != nil {
			return Session{}, err
		}
		m.notify(ctx, Event{Kind: EventRotated, Subject: replacement.Subject, Digest: IDDigest(newID), Verdict: VerdictValid})
		return replacement, nil
	}
	return Session{}, errors.New("session: could not allocate a unique session ID")
}

func (m *manager) Revoke(ctx context.Context, id string) error {
	if m.closed.Load() {
		return ErrStopped
	}
	if !validID(id, m.cfg.idBytes) {
		return ErrInvalidCookie
	}
	if err := m.store.Delete(ctx, id); err != nil {
		return err
	}
	m.notify(ctx, Event{Kind: EventRevoked, Digest: IDDigest(id), Verdict: VerdictValid})
	return nil
}

// UpdateAttributes replaces the attributes of the live session id. It refuses
// a stopped manager, a nil context, a malformed ID, and non-JSON-safe
// attributes before the store is asked anything, and reports ErrNotSupported
// when the store carries no AttributeStore capability. A session that is gone
// for any reason -- expired, revoked, evicted, or never created -- reports
// ErrNotFound, which never gives the caller a way to tell those apart.
func (m *manager) UpdateAttributes(ctx context.Context, id string, attributes map[string]any) (Session, error) {
	if m.closed.Load() {
		return Session{}, ErrStopped
	}
	if err := contextError(ctx); err != nil {
		return Session{}, err
	}
	if !validID(id, m.cfg.idBytes) {
		return Session{}, ErrInvalidCookie
	}
	normalized, err := normalizeAttributes(attributes)
	if err != nil {
		return Session{}, err
	}
	store, ok := m.store.(AttributeStore)
	if !ok {
		return Session{}, ErrNotSupported
	}
	value, found, err := store.UpdateAttributes(ctx, id, normalized)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrNotFound
	}
	m.notify(ctx, Event{Kind: EventAttributesUpdated, Subject: value.Subject, Digest: IDDigest(id), Verdict: VerdictValid})
	return cloneSession(value), nil
}

// PatchAttributes applies an attribute delta to the live session id. It
// refuses a stopped manager, a nil context, a malformed ID, a key named in both
// set and unset, and non-JSON-safe set values before the store is asked
// anything, and reports ErrNotSupported when the store carries no PatchStore
// capability. The store removes the unset keys, then assigns the set keys, and
// validates the merged map; an ID that is gone for any reason reports
// ErrNotFound, which never gives the caller a way to tell the reasons apart.
// The event Count is the number of distinct keys the patch named, never a key
// name or a value.
func (m *manager) PatchAttributes(ctx context.Context, id string, set map[string]any, unset []string) (Session, error) {
	if m.closed.Load() {
		return Session{}, ErrStopped
	}
	if err := contextError(ctx); err != nil {
		return Session{}, err
	}
	if !validID(id, m.cfg.idBytes) {
		return Session{}, ErrInvalidCookie
	}
	if err := validatePatchArguments(set, unset); err != nil {
		return Session{}, err
	}
	normalized, err := normalizeAttributes(set)
	if err != nil {
		return Session{}, err
	}
	store, ok := m.store.(PatchStore)
	if !ok {
		return Session{}, ErrNotSupported
	}
	value, found, err := store.PatchAttributes(ctx, id, normalized, append([]string(nil), unset...))
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrNotFound
	}
	m.notify(ctx, Event{
		Kind:    EventAttributesPatched,
		Subject: value.Subject,
		Digest:  IDDigest(id),
		Verdict: VerdictValid,
		Count:   patchKeyCount(normalized, unset),
	})
	return cloneSession(value), nil
}

// enforceSessionLimit applies max_sessions before a new session is stored. It
// needs the subject index, which is why a positive limit fails closed with
// ErrNotSupported on a Store that has none: silently allowing the session
// would defeat the configured security bound. The check is best effort under
// concurrency -- two Creates that both read a full subject can both proceed --
// and the next Create converges the count again. An eviction revokes the
// earliest-issued live session (ties broken by digest for determinism) and is
// reported as EventRevoked with the evicted digest and Count 1. A store failure
// fails the Create instead of storing the session.
func (m *manager) enforceSessionLimit(ctx context.Context, subject string) error {
	if m.cfg.maxSessions <= 0 {
		return nil
	}
	store, ok := m.store.(IndexedStore)
	if !ok {
		return ErrNotSupported
	}
	sessions, err := store.ListBySubject(ctx, subject)
	if err != nil {
		return err
	}
	for len(sessions) >= m.cfg.maxSessions {
		if m.cfg.onExceed == OnExceedRejectNew {
			return ErrTooManySessions
		}
		oldest := earliestSession(sessions)
		deleted, err := store.RevokeByDigest(ctx, subject, oldest.Digest)
		if err != nil {
			return err
		}
		count := 0
		if deleted {
			count = 1
		}
		m.notify(ctx, Event{Kind: EventRevoked, Subject: subject, Digest: oldest.Digest, Verdict: VerdictValid, Count: count})
		sessions = removeSessionInfo(sessions, oldest.Digest)
	}
	return nil
}

// earliestSession picks the session with the smallest IssuedAt, breaking ties
// by digest so repeated runs over one listing revoke the same session first.
func earliestSession(sessions []SessionInfo) SessionInfo {
	oldest := sessions[0]
	for _, candidate := range sessions[1:] {
		if candidate.IssuedAt.Before(oldest.IssuedAt) ||
			(candidate.IssuedAt.Equal(oldest.IssuedAt) && candidate.Digest < oldest.Digest) {
			oldest = candidate
		}
	}
	return oldest
}

func removeSessionInfo(sessions []SessionInfo, digest string) []SessionInfo {
	for i := range sessions {
		if sessions[i].Digest == digest {
			return append(sessions[:i], sessions[i+1:]...)
		}
	}
	return sessions
}

// Verify performs the shared session check: a stopped manager, a nil context,
// and a malformed ID are refused without a store lookup; a live session is
// returned with its idle lease renewed under the configured operation timeout
// and touch throttling; and every other outcome is classified without
// distinguishing an expired, evicted, revoked, or never-created session. A
// stopped manager and an unusable context report VerdictStoreError with their
// own error, because neither says anything about the presented ID.
func (m *manager) Verify(ctx context.Context, id string) (Session, Verdict, error) {
	if m.closed.Load() {
		return Session{}, VerdictStoreError, ErrStopped
	}
	if err := contextError(ctx); err != nil {
		return Session{}, VerdictStoreError, err
	}
	digest := IDDigest(id)
	if !validID(id, m.cfg.idBytes) {
		m.notify(ctx, Event{Kind: EventRejected, Digest: digest, Verdict: VerdictMalformed})
		return Session{}, VerdictMalformed, nil
	}
	touchCtx, cancel := context.WithTimeout(ctx, m.cfg.operationTimeout)
	value, found, err := m.store.Touch(touchCtx, id, m.cfg.idleTTL, m.cfg.touchInterval)
	cancel()
	if err != nil {
		m.notify(ctx, Event{Kind: EventRejected, Digest: digest, Verdict: VerdictStoreError, Err: err})
		return Session{}, VerdictStoreError, err
	}
	if !found {
		m.notify(ctx, Event{Kind: EventRejected, Digest: digest, Verdict: VerdictNotFound})
		return Session{}, VerdictNotFound, nil
	}
	m.notify(ctx, Event{Kind: EventVerified, Subject: value.Subject, Digest: digest, Verdict: VerdictValid})
	return cloneSession(value), VerdictValid, nil
}

// indexed returns the store's subject-index capability, after the checks every
// index method shares: a stopped manager, a nil context, and an empty subject
// are refused before the store is asked anything.
func (m *manager) indexed(ctx context.Context, subject string) (IndexedStore, string, error) {
	if m.closed.Load() {
		return nil, "", ErrStopped
	}
	if err := contextError(ctx); err != nil {
		return nil, "", err
	}
	subject, err := normalizeSubject(subject)
	if err != nil {
		return nil, "", err
	}
	store, ok := m.store.(IndexedStore)
	if !ok {
		return nil, "", ErrNotSupported
	}
	return store, subject, nil
}

func (m *manager) ListBySubject(ctx context.Context, subject string) ([]SessionInfo, error) {
	store, subject, err := m.indexed(ctx, subject)
	if err != nil {
		return nil, err
	}
	return store.ListBySubject(ctx, subject)
}

func (m *manager) RevokeBySubject(ctx context.Context, subject string) (int, error) {
	store, subject, err := m.indexed(ctx, subject)
	if err != nil {
		return 0, err
	}
	revoked, err := store.RevokeBySubject(ctx, subject)
	if err != nil {
		return revoked, err
	}
	m.notify(ctx, Event{Kind: EventRevokedBySubject, Subject: subject, Verdict: VerdictValid, Count: revoked})
	return revoked, nil
}

func (m *manager) RevokeByDigest(ctx context.Context, subject, digest string) (bool, error) {
	store, subject, err := m.indexed(ctx, subject)
	if err != nil {
		return false, err
	}
	deleted, err := store.RevokeByDigest(ctx, subject, digest)
	if err != nil {
		return false, err
	}
	count := 0
	if deleted {
		count = 1
	}
	m.notify(ctx, Event{Kind: EventRevokedByDigest, Subject: subject, Digest: digest, Verdict: VerdictValid, Count: count})
	return deleted, nil
}

// notify delivers one event to every observer in registration order, on the
// caller's goroutine, through the package's panic-contained delivery helper.
func (m *manager) notify(ctx context.Context, event Event) {
	notifyObservers(ctx, m.observers, event)
}

// lifetime reports how long value's ID may still be handed to a client: a
// stopped manager, a malformed ID, and an already expired session are all
// refused.
func (m *manager) lifetime(value Session) (time.Duration, error) {
	if m.closed.Load() {
		return 0, ErrStopped
	}
	if !validID(value.ID, m.cfg.idBytes) || !value.ExpiresAt.After(m.clock()) {
		return 0, ErrInvalidCookie
	}
	remaining := time.Until(value.ExpiresAt)
	if m.now != nil {
		remaining = value.ExpiresAt.Sub(m.clock())
	}
	if remaining <= 0 {
		return 0, ErrInvalidCookie
	}
	return remaining, nil
}

func (m *manager) newID() (string, error) {
	raw := make([]byte, m.cfg.idBytes)
	reader := m.random
	if reader == nil {
		reader = rand.Reader
	}
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", errors.New("session: generate random session ID")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (m *manager) stop() { m.closed.Store(true) }

func validID(id string, bytes int) bool {
	if id == "" || id != strings.TrimSpace(id) || strings.Contains(id, "=") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(raw) == bytes
}
