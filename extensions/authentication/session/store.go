package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Store is the persistence contract used by Manager. Implementations receive
// the opaque ID but must not use it directly as an external storage key. Touch
// returns the current session and atomically renews its idle lease only when
// minInterval has elapsed. Rotate installs replacement and retires oldID: a nil
// return guarantees the old ID no longer resolves, ErrNotFound reports an old
// ID that is gone or past its lifetime, and ErrAlreadyExists reports a
// replacement ID another session holds. A store over a distributed backend may
// run the rotation as a sequence of single-key steps rather than one atomic
// operation, so a rotation in flight leaves both IDs briefly valid, and two
// concurrent rotations with different replacements may both report success,
// each receiving only the replacement it passed in.
type Store interface {
	Create(ctx context.Context, value Session, idleTTL time.Duration) error
	Get(ctx context.Context, id string) (Session, bool, error)
	Touch(ctx context.Context, id string, idleTTL, minInterval time.Duration) (Session, bool, error)
	Rotate(ctx context.Context, oldID string, replacement Session, idleTTL time.Duration) error
	Delete(ctx context.Context, id string) error
}

// SessionInfo is a read-only view of one live session. Digest is the hex
// IDDigest of the session ID: a storage handle that names the session for
// IndexedStore.RevokeByDigest and for correlation, never a credential. The
// struct carries no session ID, so holding a SessionInfo does not allow
// impersonating the session. Attributes is a defensive copy. LastSeenAt is the
// time the session's idle lease was last renewed, which Touch throttles to
// touch_interval, so it can trail the latest request by up to that interval;
// a store that cannot report it leaves it zero.
type SessionInfo struct {
	Digest     string
	Subject    string
	Attributes map[string]any
	IssuedAt   time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// IndexedStore is the optional capability of a Store that indexes its sessions
// by subject. MemoryStore and RedisStore implement it; a custom Store injected
// with WithStore need not, and Manager then reports ErrNotSupported from
// ListBySubject, RevokeBySubject, and RevokeByDigest.
//
// An implementation keeps one invariant: every live session is indexed under
// its subject. The index may also hold stale members -- digests whose session
// is gone -- which are harmless and are pruned lazily by the operations below.
// To keep the invariant, Create indexes before it stores the session and fails
// without storing when indexing fails, Rotate indexes the replacement before
// installing it, and Delete removes the session before its index entry. Touch
// never changes the index.
//
// Every method rejects an empty subject. ListBySubject returns the live
// sessions of the subject in no particular order, each with its own Attributes
// copy. RevokeBySubject deletes every live session of the subject, removes
// only the members it read (a session created concurrently stays indexed), and
// returns the number of live sessions it deleted; calling it again returns 0.
// A distributed implementation cannot make this atomic: a session created while
// the revocation runs may survive it, so callers change the credential first
// and revoke second. RevokeByDigest deletes the session named by digest only
// when it is indexed under subject: another subject's digest, an unknown
// digest, and a dead session all report false without touching any other
// session, and a digest that is not exactly 64 lowercase hex characters is a
// parameter error, distinguishable from "not found".
type IndexedStore interface {
	Store
	ListBySubject(ctx context.Context, subject string) ([]SessionInfo, error)
	RevokeBySubject(ctx context.Context, subject string) (int, error)
	RevokeByDigest(ctx context.Context, subject, digest string) (bool, error)
}

// IDDigest returns the fixed-width SHA-256 digest built-in stores use as their
// storage key component. It is a handle, not a credential: it is safe to log
// only when application policy allows correlating requests, and it cannot be
// used as a session credential. SessionInfo.Digest carries the same value.
func IDDigest(id string) string {
	digest := sha256.Sum256([]byte(id))
	return hex.EncodeToString(digest[:])
}

// validDigest reports whether digest has exactly the form IDDigest produces:
// 64 lowercase hexadecimal characters. Stores build keys from it, so anything
// else is rejected before it can name a key.
func validDigest(digest string) bool {
	if len(digest) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for i := range len(digest) {
		ch := digest[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// normalizeSubject applies the subject rule Manager.Create uses.
func normalizeSubject(subject string) (string, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", errors.New("session: subject cannot be empty")
	}
	return subject, nil
}
