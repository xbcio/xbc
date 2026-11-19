package session

import (
	"context"
	"fmt"
	"reflect"

	"github.com/xbcio/xbc/log"
)

// Observer receives one Event after a session operation has finished. It is
// the audit and metrics hook of this package: Observe is called
// synchronously, on the caller's goroutine, after the operation already
// succeeded or failed, so every implementation must return quickly and must
// not block on I/O. An Observer that panics is contained: the panic is
// recovered, recorded through the log facade without session data, and the
// remaining observers are still notified.
//
// An Event never carries a session ID, attribute value, or credential. Digest
// is IDDigest(id), a fixed-width handle that names a session for correlation
// but cannot authenticate as it. Subject is the session's subject, which the
// Event contract requires so an auditor can answer "whose sessions changed".
type Observer interface {
	Observe(ctx context.Context, e Event)
}

// ObserverContributor is the typed contribution contract another plugin's
// Definition exports to supply session observers. Observers is called once
// during construction; implementations must return a stable snapshot, must
// not acquire lifecycle-owned resources, and must not rely on their own
// lifecycle having started. Silence is the safe failure mode: a nil
// contributor, a nil observer, or a panicking Observers callback is skipped
// and recorded through the log facade rather than failing construction.
type ObserverContributor interface {
	Observers() []Observer
}

// EventKind names the operation an Event reports.
type EventKind int

const (
	// EventCreated reports a session created by Manager.Create.
	EventCreated EventKind = iota
	// EventRotated reports a session whose ID was replaced by Manager.Rotate.
	// Digest names the replacement ID.
	EventRotated
	// EventRevoked reports a Manager.Revoke call that completed, or a session the
	// max_sessions policy evicted before creating another one. Revoke is
	// idempotent, so the Event does not distinguish a live session from one that
	// was already gone; an eviction reports the evicted session's digest and
	// Count 1 when the store deleted a live session.
	EventRevoked
	// EventRevokedBySubject reports Manager.RevokeBySubject. Count is the
	// number of live sessions it deleted.
	EventRevokedBySubject
	// EventRevokedByDigest reports Manager.RevokeByDigest. Count is 1 when it
	// deleted a live session and 0 otherwise.
	EventRevokedByDigest
	// EventAttributesUpdated reports AttributeManager.UpdateAttributes.
	EventAttributesUpdated
	// EventAttributesPatched reports AttributeManager.PatchAttributes. Count is
	// the number of distinct attribute keys the patch named (set plus unset),
	// never a key name or a value.
	EventAttributesPatched
	// EventVerified reports an AttributeManager.Verify (or Authenticate) that
	// found a live session and renewed its idle lease.
	EventVerified
	// EventRejected reports an AttributeManager.Verify (or Authenticate) that
	// refused a credential. Verdict distinguishes Malformed, NotFound, and
	// StoreError for internal auditing only; Err is set for StoreError.
	EventRejected
	// EventExpired reports a session that the MemoryStore cleanup sweep removed
	// because it was past its absolute lifetime or its idle lease. Digest names
	// the pruned session and Subject is the subject it belonged to. Only the
	// memory backend's sweeper emits it: the Redis backend never does, because
	// reliable expiry notifications need keyspace-notification configuration a
	// shared Redis deployment cannot guarantee. Lazy pruning (Get, Touch, List,
	// RevokeBySubject) does not emit it either; only the periodic sweep does.
	EventExpired
)

func (k EventKind) String() string {
	switch k {
	case EventCreated:
		return "created"
	case EventRotated:
		return "rotated"
	case EventRevoked:
		return "revoked"
	case EventRevokedBySubject:
		return "revoked_by_subject"
	case EventRevokedByDigest:
		return "revoked_by_digest"
	case EventAttributesUpdated:
		return "attributes_updated"
	case EventAttributesPatched:
		return "attributes_patched"
	case EventVerified:
		return "verified"
	case EventRejected:
		return "rejected"
	case EventExpired:
		return "expired"
	default:
		return fmt.Sprintf("EventKind(%d)", int(k))
	}
}

// Event is the data-free record an Observer receives. It deliberately has no
// field for a session ID, attribute map, or credential: the session ID is the
// bearer credential, and attribute values routinely carry identity data, so
// neither may reach an observer (or, through it, a log line or metric label).
// Digest is IDDigest of the session ID; Subject is the session's subject when
// the operation knows it (a rejection of a never-resolved ID, and a Revoke,
// leave it empty); Err carries a store failure for StoreError only. Count is
// set on the RevokedBySubject, RevokedByDigest, AttributesPatched, and Expired
// kinds and is zero elsewhere: the number of live sessions a subject revoke
// deleted, whether a digest revoke deleted one, the number of distinct keys a
// patch named, and 1 for one swept session. Verdict is meaningful on Verified
// and Rejected, is VerdictValid on the write kinds, and is VerdictValid on
// Expired because the session was live until its deadline passed.
type Event struct {
	Kind    EventKind
	Subject string
	Digest  string
	Verdict Verdict
	Count   int
	Err     error
}

// notifyObservers delivers event to every observer in order, on the caller's
// goroutine. Each delivery is isolated: a panicking observer is recovered and
// recorded through the log facade with only the event kind, because the log
// must not gain session data the event itself withholds.
func notifyObservers(ctx context.Context, observers []Observer, event Event) {
	for _, observer := range observers {
		notifyObserver(ctx, observer, event)
	}
}

func notifyObserver(ctx context.Context, observer Observer, event Event) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Ctx(ctx).Error("session: session observer panicked", "event", event.Kind.String())
		}
	}()
	observer.Observe(ctx, event)
}

// isNilObserver reports whether value carries no observer, including the typed
// nil a caller can store in the interface.
func isNilObserver(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
