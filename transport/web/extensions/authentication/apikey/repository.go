package apikey

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// KeyDigest is the only key representation passed to or retained by
// repositories.
type KeyDigest [sha256.Size]byte

// HashKey irreversibly hashes a presented key. Callers can use
// HashKey(key).String() to prepare a static configuration digest.
func HashKey(key string) KeyDigest { return sha256.Sum256([]byte(key)) }

// String renders the canonical lowercase hexadecimal configuration form.
func (d KeyDigest) String() string { return hex.EncodeToString(d[:]) }

// ParseKeyDigest parses the exact 32-byte SHA-256 hex representation.
func ParseKeyDigest(value string) (KeyDigest, error) {
	var digest KeyDigest
	decoded, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != sha256.Size {
		return digest, errors.New("apikey: SHA-256 digest must contain exactly 64 hexadecimal characters")
	}
	copy(digest[:], decoded)
	return digest, nil
}

// Credential is verified identity metadata returned by a Repository. It never
// contains the presented key or its digest.
type Credential struct {
	ID         string
	AppID      string
	Subject    string
	Attributes map[string]any
}

// Repository resolves an already-hashed key. Implementations must compare
// secret material in constant time and must honor ctx cancellation.
type Repository interface {
	Lookup(ctx context.Context, appID string, digest KeyDigest) (Credential, bool, error)
}

type storedCredential struct {
	digest     KeyDigest
	id         string
	appID      string
	subject    string
	attributes map[string]any
	disabled   bool
}

type repositorySnapshot struct {
	credentials []storedCredential
}

// StaticRepository is a lock-free read, atomically replaceable credential
// snapshot. It stores SHA-256 digests only and performs ConstantTimeCompare.
type StaticRepository struct {
	state atomic.Pointer[repositorySnapshot]
}

// NewStaticRepository validates and compiles one digest-only snapshot.
func NewStaticRepository(credentials []StaticCredential) (*StaticRepository, error) {
	r := new(StaticRepository)
	if err := r.replace(credentials); err != nil {
		return nil, err
	}
	return r, nil
}

// Replace validates and compiles a complete new snapshot before atomically
// publishing it. Readers see either the old or the new snapshot, never a mix.
func (r *StaticRepository) Replace(credentials []StaticCredential) error {
	if r == nil {
		return errors.New("apikey: replace credentials on nil static repository")
	}
	return r.replace(credentials)
}

func (r *StaticRepository) replace(credentials []StaticCredential) error {
	if len(credentials) == 0 {
		return errors.New("apikey: static credentials cannot be empty")
	}
	compiled := make([]storedCredential, 0, len(credentials))
	seenID := make(map[string]struct{}, len(credentials))
	seenDigest := make(map[KeyDigest]struct{}, len(credentials))
	for i, item := range credentials {
		subject := strings.TrimSpace(item.Subject)
		id := strings.TrimSpace(item.ID)
		appID := strings.TrimSpace(item.AppID)
		if subject == "" {
			return fmt.Errorf("apikey: static[%d].subject cannot be empty", i)
		}
		digest, err := ParseKeyDigest(item.SHA256)
		if err != nil {
			return fmt.Errorf("apikey: static[%d].sha256: %w", i, err)
		}
		if id == "" {
			id = subject
		}
		if _, duplicate := seenID[id]; duplicate {
			return fmt.Errorf("apikey: duplicate static credential id %q", id)
		}
		seenID[id] = struct{}{}
		if _, duplicate := seenDigest[digest]; duplicate {
			return fmt.Errorf("apikey: duplicate static credential key digest")
		}
		seenDigest[digest] = struct{}{}
		compiled = append(compiled, storedCredential{
			digest:     digest,
			id:         id,
			appID:      appID,
			subject:    subject,
			attributes: cloneAttributes(item.Attributes),
			disabled:   item.Disabled,
		})
	}
	r.state.Store(&repositorySnapshot{credentials: compiled})
	return nil
}

// Lookup checks every eligible digest without data-dependent early exit. A
// duplicate digest is rejected during compilation, so at most one can match.
func (r *StaticRepository) Lookup(ctx context.Context, appID string, digest KeyDigest) (Credential, bool, error) {
	if r == nil {
		return Credential{}, false, errors.New("apikey: nil static repository")
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, false, err
	}
	snapshot := r.state.Load()
	if snapshot == nil {
		return Credential{}, false, nil
	}
	appID = strings.TrimSpace(appID)
	match := -1
	for i := range snapshot.credentials {
		item := &snapshot.credentials[i]
		eligible := !item.disabled && (appID == "" || item.appID == appID)
		equal := subtle.ConstantTimeCompare(item.digest[:], digest[:]) == 1
		if eligible && equal {
			match = i
		}
	}
	if match < 0 {
		return Credential{}, false, nil
	}
	item := snapshot.credentials[match]
	return Credential{
		ID:         item.id,
		AppID:      item.appID,
		Subject:    item.subject,
		Attributes: cloneAttributes(item.attributes),
	}, true, nil
}

func cloneAttributes(attributes map[string]any) map[string]any {
	if attributes == nil {
		return nil
	}
	copy := make(map[string]any, len(attributes))
	for key, value := range attributes {
		copy[key] = value
	}
	return copy
}
