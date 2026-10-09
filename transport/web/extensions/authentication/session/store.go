package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// IDDigest returns the fixed-width SHA-256 digest built-in stores use as their
// storage key component. It is safe to log only when application policy allows
// correlating requests; it cannot be used as a session credential.
func IDDigest(id string) string {
	digest := sha256.Sum256([]byte(id))
	return hex.EncodeToString(digest[:])
}
