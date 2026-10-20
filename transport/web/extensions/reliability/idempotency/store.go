package idempotency

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// AcquireState is the result of attempting to reserve one idempotency key.
type AcquireState uint8

const (
	Acquired AcquireState = iota + 1
	Pending
	Completed
	Conflict
)

// Response is the replay-safe subset of an HTTP response. ContentType carries
// Content-Type; Header carries the other whitelisted headers -- Location,
// Content-Language, Content-Disposition, ETag, Last-Modified and Cache-Control.
// Set-Cookie is never part of a stored response, and a Store that returns one
// makes the middleware refuse the replay.
//
// Header keys follow http.Header's canonical spelling. Implementations must
// return defensive copies of both Body and Header.
type Response struct {
	Status      int
	ContentType string
	Header      http.Header
	Body        []byte
}

// AcquireResult reports the key's current state. Response is populated only
// for Completed and implementations must return a defensive body copy.
type AcquireResult struct {
	State    AcquireState
	Response Response
}

// ErrOwnershipLost means a pending reservation expired, was taken over, or no
// longer matches its owner/fingerprint. It is safe for callers to release.
var ErrOwnershipLost = errors.New("idempotency: reservation ownership lost")

// Store is the narrow atomic state machine used by the middleware.
type Store interface {
	Acquire(ctx context.Context, key, fingerprint, owner string, pendingTTL time.Duration) (AcquireResult, error)
	Complete(ctx context.Context, key, fingerprint, owner string, response Response, ttl time.Duration) error
	Release(ctx context.Context, key, fingerprint, owner string) error
}
