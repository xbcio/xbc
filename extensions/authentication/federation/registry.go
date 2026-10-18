package federation

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

var (
	// ErrNilRegistry reports a method call on a nil *Registry.
	ErrNilRegistry = errors.New("federation: nil registry")
	// ErrRegistryValidator reports a registry constructed without a validator.
	// A registry without static validation would accept any configuration, so
	// Put refuses to store anything.
	ErrRegistryValidator = errors.New("federation: registry validator is required")
	// ErrInvalidProviderName reports a provider name outside
	// ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$.
	ErrInvalidProviderName = errors.New("federation: invalid provider name")
)

// MaxProviderNameLength is the maximum length of a provider name.
const MaxProviderNameLength = 64

// Entry is one provider snapshot row returned by Registry.List.
//
// Value is the value that was passed to Put. List copies the slice and the
// map it was built from, but not the internals of T: a provider must store a
// fresh, immutable value instead of mutating one it has already published.
type Entry[T any] struct {
	Name  string
	Value T
}

// Registry is a concurrency-safe collection of provider configurations keyed
// by a stable application-chosen name. Each protocol module instantiates it
// with its own configuration type and static validator.
//
// The zero value is not usable: it carries no validator, so Put reports
// ErrRegistryValidatorRequired. Construct a registry with NewRegistry. A nil
// validator passed to NewRegistry has the same effect.
//
// Put validates before taking the lock and replaces the stored value
// atomically, so a rejected configuration leaves the previous one in effect.
// Remove affects only subsequent lookups: sessions created before a provider
// is removed remain valid, and revoking them is the application's job.
type Registry[T any] struct {
	// validate is captured at construction and never mutated, so reading it
	// without the lock is safe.
	validate func(name string, value T) error

	mu      sync.RWMutex
	entries map[string]T
}

// NewRegistry returns an empty registry that validates every configuration
// through validate before storing it.
//
// validate must perform static checks only -- required fields, URL shape,
// algorithm allowlists -- and never network I/O: dependency discovery is a
// separate, explicit call made by the protocol module. It must return errors
// safe for internal diagnostics; it must never include client secrets, tokens,
// or assertion material.
func NewRegistry[T any](validate func(name string, value T) error) *Registry[T] {
	return &Registry[T]{validate: validate}
}

// Put validates value and stores it under name, replacing any previous value
// atomically. A name outside the provider-name rule or a value the validator
// rejects leaves the existing configuration untouched.
func (r *Registry[T]) Put(name string, value T) error {
	if r == nil {
		return ErrNilRegistry
	}
	if r.validate == nil {
		return ErrRegistryValidator
	}
	if !validProviderName(name) {
		return ErrInvalidProviderName
	}
	if err := r.validate(name, value); err != nil {
		return fmt.Errorf("federation: provider %q rejected: %w", name, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]T)
	}
	r.entries[name] = value
	return nil
}

// Remove deletes the provider stored under name and reports whether it was
// present. Removing an unknown or malformed name is a no-op.
func (r *Registry[T]) Remove(name string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[name]; !exists {
		return false
	}
	delete(r.entries, name)
	return true
}

// Get returns the provider stored under name and whether it was present.
func (r *Registry[T]) Get(name string) (T, bool) {
	var zero T
	if r == nil {
		return zero, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, exists := r.entries[name]
	if !exists {
		return zero, false
	}
	return value, true
}

// List returns every provider ordered by name. The returned slice and the
// map it was built from are copies.
func (r *Registry[T]) List() []Entry[T] {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	entries := make([]Entry[T], 0, len(r.entries))
	for name, value := range r.entries {
		entries = append(entries, Entry[T]{Name: name, Value: value})
	}
	r.mu.RUnlock()
	slices.SortFunc(entries, func(left, right Entry[T]) int {
		return strings.Compare(left.Name, right.Name)
	})
	return entries
}

// validProviderName enforces ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$. The rule is
// ASCII-only on purpose: a provider name is a configuration key, not a
// display name, and it must never need normalization or escaping.
func validProviderName(name string) bool {
	if len(name) == 0 || len(name) > MaxProviderNameLength {
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
