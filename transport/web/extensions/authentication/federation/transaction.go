package federation

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// maxMaterialLength bounds each opaque protocol value carried by a
	// transaction. The values are secrets, not user content, so a value that
	// long is a defect rather than a legitimate payload.
	maxMaterialLength = 512
	// minStateLength is the shortest state a transaction may carry. Protocol
	// modules generate state with at least 128 bits of entropy; the length
	// check only rejects material that cannot possibly meet that bar.
	minStateLength = 16
	// transactionSegment and reservationSegment keep the two key spaces below
	// one prefix disjoint, so a provider named like a reservation namespace
	// can never collide with an assertion replay key.
	transactionSegment = "txn:"
	reservationSegment = "replay:"
)

var (
	// ErrInvalidTransaction reports login-transaction material that cannot be
	// stored: an invalid provider or state, a missing browser binding, a
	// lifetime outside (0, MaxTransactionTTL], or an oversized protocol value.
	ErrInvalidTransaction = errors.New("federation: invalid login transaction")
	// ErrTransactionStore reports an operational store or backend failure.
	ErrTransactionStore = errors.New("federation: login transaction store")
	// ErrTransactionExists reports a state collision inside one provider
	// namespace. A transaction already in flight is never overwritten.
	ErrTransactionExists = errors.New("federation: login transaction already exists")
	// ErrInvalidReservation reports a malformed set-if-absent reservation.
	ErrInvalidReservation = errors.New("federation: invalid reservation")
	// ErrNilStore reports a method call on a nil store.
	ErrNilStore = errors.New("federation: nil store")
	// ErrNilContext reports a nil context passed to a store.
	ErrNilContext = errors.New("federation: nil context")
)

// Transaction is the server-side record of one login attempt. It carries the
// protocol module's opaque one-time material plus the browser binding and the
// landing decision the shared base owns. It never leaves the server except as
// the values each field is named for.
type Transaction struct {
	// Provider is the application-chosen provider identifier the login
	// started for. It namespaces the transaction.
	Provider string `json:"provider"`
	// State is the protocol's anti-forgery value and the transaction's lookup
	// key. Protocol modules must generate at least 128 bits of entropy.
	State string `json:"state"`
	// Nonce is the OIDC nonce the ID token must echo, when the protocol uses
	// one.
	Nonce string `json:"nonce,omitempty"`
	// CodeVerifier is the PKCE verifier whose S256 challenge was sent in the
	// authorization request, when the protocol uses PKCE.
	CodeVerifier string `json:"code_verifier,omitempty"`
	// RequestID is the SAML AuthnRequest ID the response must name in
	// InResponseTo, when the protocol is SAML.
	RequestID string `json:"request_id,omitempty"`
	// ReturnTo is the sanitized landing path or allowlisted absolute URL.
	ReturnTo string `json:"return_to,omitempty"`
	// Binding is the digest of the browser-binding cookie secret. It is never
	// the cookie value itself, so a store dump cannot be replayed as a
	// browser.
	Binding string `json:"binding"`
	// ExpiresAt is the instant after which the transaction is unusable.
	ExpiresAt time.Time `json:"expires_at"`
}

// Store persists login transactions and set-if-absent reservations.
//
// Implementations must make Consume a single atomic operation that deletes the
// transaction and reports whether it existed together with the record it
// deleted. A read followed by a separate delete is not a valid implementation:
// two concurrent callbacks would both observe the transaction and both
// complete the login. The built-in MemoryStore and RedisStore implement the
// contract; the Redis backend is the one a multi-replica deployment must use.
//
// Implementations must treat every value as opaque, must not log a transaction
// or any of its fields, and must not retain the caller's Transaction value.
type Store interface {
	// Create stores tx unless a transaction already exists under the same
	// provider and state. It reports ErrTransactionExists for a collision and
	// ErrInvalidTransaction for material outside the contract.
	Create(ctx context.Context, tx Transaction) error
	// Consume atomically deletes the transaction named by provider and state
	// and returns it when the digest of the presenting browser's binding
	// cookie equals tx.Binding. A missing transaction, an expired one, and a
	// different browser all report found == false; callers must not
	// distinguish them.
	Consume(ctx context.Context, provider, state, binding string) (Transaction, bool, error)
	// SetIfAbsent writes key under namespace only when it is absent and
	// reports whether this call created it. The SAML adapter uses it as the
	// assertion replay cache.
	SetIfAbsent(ctx context.Context, namespace, key string, ttl time.Duration) (bool, error)
}

// BrowserBinding returns the digest stored in Transaction.Binding for a
// browser-binding cookie secret. The secret is the cookie value; only its
// digest is ever persisted, and comparisons are constant time.
func BrowserBinding(secret string) string {
	// The digest is computed by the caller that owns the cookie. Keeping the
	// function exported lets an embedding application build transactions
	// without reimplementing the shape the stores compare against.
	return bindingDigest(secret)
}

// validate checks the parts of tx that do not depend on a store, at instant
// now.
func (tx Transaction) validate(now time.Time) error {
	if !validProviderName(tx.Provider) {
		return fmt.Errorf("%w: provider name is invalid", ErrInvalidTransaction)
	}
	if !validState(tx.State) {
		return fmt.Errorf("%w: state is invalid", ErrInvalidTransaction)
	}
	if !validBindingDigest(tx.Binding) {
		return fmt.Errorf("%w: browser binding is invalid", ErrInvalidTransaction)
	}
	if len(tx.ReturnTo) > MaxReturnToLength || containsControl(tx.ReturnTo) {
		return fmt.Errorf("%w: return_to is invalid", ErrInvalidTransaction)
	}
	for _, material := range []string{tx.Nonce, tx.CodeVerifier, tx.RequestID} {
		if len(material) > maxMaterialLength || containsControl(material) {
			return fmt.Errorf("%w: protocol material is invalid", ErrInvalidTransaction)
		}
	}
	if !tx.ExpiresAt.After(now) {
		return fmt.Errorf("%w: transaction is already expired", ErrInvalidTransaction)
	}
	if tx.ExpiresAt.After(now.Add(MaxTransactionTTL)) {
		return fmt.Errorf("%w: lifetime exceeds %s", ErrInvalidTransaction, MaxTransactionTTL)
	}
	return nil
}

// matches reports whether a stored transaction belongs to the lookup that
// found it and was started by the presenting browser.
func (tx Transaction) matches(provider, state, binding string) bool {
	if tx.Provider != provider || tx.State != state {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tx.Binding), []byte(binding)) == 1
}

// validProviderName enforces ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$, the same rule
// the federation registry applies, so an adapter can pass a path parameter
// straight through.
func validProviderName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case index > 0 && (character == '_' || character == '-'):
		default:
			return false
		}
	}
	return true
}

// validState enforces the state shape: RFC 3986 unreserved characters only, a
// bounded length, and at least minStateLength bytes. The value is a key
// component and travels through the browser, so it must never need escaping.
func validState(state string) bool {
	if len(state) < minStateLength || len(state) > maxMaterialLength {
		return false
	}
	for index := 0; index < len(state); index++ {
		character := state[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '.', character == '_', character == '~':
		default:
			return false
		}
	}
	return true
}

// validBindingDigest reports whether value has the shape BrowserBinding
// produces.
func validBindingDigest(value string) bool {
	if len(value) != bindingDigestLength {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// reservationNamespaceRule is ^[a-z][a-z0-9_-]{0,31}$: a namespace is chosen
// by module code, never by a request, and stays ASCII.
func validReservationNamespace(namespace string) bool {
	if len(namespace) == 0 || len(namespace) > 32 {
		return false
	}
	for index := 0; index < len(namespace); index++ {
		character := namespace[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case index > 0 && (character == '_' || character == '-'):
		default:
			return false
		}
	}
	return true
}

// validReservationKey reports whether key can be stored as a reservation
// name: bounded, printable, and free of the segment separator.
func validReservationKey(key string) bool {
	if key == "" || len(key) > maxMaterialLength || containsControl(key) {
		return false
	}
	return !strings.Contains(key, ":")
}

func validReservationTTL(ttl time.Duration) bool {
	return ttl > 0 && ttl <= MaxReservationTTL
}

// transactionKey names one transaction below prefix. The provider segment is
// validated before this call, so a state can never escape its namespace.
func transactionKey(prefix, provider, state string) string {
	return prefix + transactionSegment + provider + ":" + state
}

// reservationKey names one reservation below prefix.
func reservationKey(prefix, namespace, key string) string {
	return prefix + reservationSegment + namespace + ":" + key
}

// checkContext rejects a nil context before a store touches it.
func checkContext(ctx context.Context) error {
	if ctx == nil {
		return ErrNilContext
	}
	return nil
}

// contextError reports a cancelled or expired context before a store starts
// work. It mirrors the neutral session store's pre-flight check.
func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrTransactionStore, err)
	}
	return nil
}
