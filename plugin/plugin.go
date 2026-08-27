package plugin

import "github.com/xbcio/xbc/log"

// Plugin is a marker for a live plugin value. Any non-nil Go value is valid,
// including a non-pointer struct such as struct{}{}. Identity deliberately is
// not a method on this interface: the owning Definition.Key is the sole source
// of identity, so the same implementation type may safely back several
// distinct definitions without creating ambiguous dependency edges.
//
// Field declarations using xbc tags are an optional, stricter capability:
// a plugin that declares inject or provide fields must be a non-nil struct
// pointer so the runtime can safely mutate and harvest those fields.
type Plugin interface{}

// Base is optional convenience plumbing for a plugin implementation. It gives
// access to the Context and to the Definition key bound by the runtime.
type Base struct {
	ctx *Context
	key Key
}

// Ctx returns the Context bound during expansion, or nil before expansion.
func (b *Base) Ctx() *Context { return b.ctx }

// Log returns the instance logger, falling back to the package logger before
// a Context has been bound.
func (b *Base) Log() log.Logger {
	if b.ctx == nil {
		return log.L()
	}
	return b.ctx.Log()
}

// Key returns the owning Definition's stable key.
func (b *Base) Key() Key { return b.key }

// Name returns Key as text for logging and display convenience.
func (b *Base) Name() string { return b.key.String() }

func (b *Base) base() *Base { return b }

type baseAnchor interface{ base() *Base }

func bindBase(p Plugin, ctx *Context, key Key) bool {
	a, ok := p.(baseAnchor)
	if !ok {
		return false
	}
	b := a.base()
	if b == nil {
		// Embedding *Base is legal Go, and its promoted base method makes the
		// plugin satisfy baseAnchor even when that pointer is nil. Base is only
		// optional convenience plumbing, so an unusable nil embedding must not
		// turn an otherwise valid marker Plugin into an assembly panic.
		return false
	}
	b.ctx = ctx
	b.key = key
	return true
}
