package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Store is the atomic persistence contract used by Manager. Implementations
// receive the opaque ID but must not use it directly as an external storage
// key. Touch returns the current session and atomically renews its idle lease
// only when minInterval has elapsed. Rotate installs replacement and removes
// oldID as one operation, preventing two concurrent rotations from succeeding.
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
