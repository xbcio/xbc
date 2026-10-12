package grpc

import (
	"fmt"

	"google.golang.org/grpc"

	"github.com/xbcio/xbc/plugin"
)

// Phase is a coarse, ordered anchor for interceptor placement. It is a hard
// boundary: an interceptor in an earlier Phase always runs outside (before)
// every interceptor in a later Phase, regardless of any Before or After
// constraint.
//
// The phases carry the Web transport's names on purpose -- an operator reads
// "observe" or "auth" the same way on either stack -- but they are this
// package's own type. The two stacks are modelled separately, so neither
// imports the other's ordering vocabulary.
type Phase int

// Normal extension phases are spaced 100 apart. PhaseRecover is the floor, not a
// convention: the Server assembles its panic boundary there, and a contributed
// interceptor declaring a lower phase is rejected at startup, so nothing can run
// where a panic escapes every ordered stage.
const (
	PhaseRecover  Phase = 0   // outermost: panic boundary, assembled by the Server
	PhaseObserve  Phase = 100 // tracing, metrics, access log
	PhaseError    Phase = 150 // status mapping
	PhaseSecurity Phase = 200 // rate limiting, admission policy
	PhaseAuth     Phase = 300 // authentication and authorization
	PhaseBusiness Phase = 400 // business interceptors
)

// String renders the phase name used in startup diagnostics.
func (p Phase) String() string {
	switch p {
	case PhaseRecover:
		return "recover"
	case PhaseObserve:
		return "observe"
	case PhaseError:
		return "error"
	case PhaseSecurity:
		return "security"
	case PhaseAuth:
		return "auth"
	case PhaseBusiness:
		return "business"
	default:
		return fmt.Sprintf("phase(%d)", int(p))
	}
}

// UnaryInterceptor is the consumer-owned contract for one independently ordered
// unary server interceptor. Its identity is the Identity of the plugin.Entry
// that carries it.
type UnaryInterceptor interface {
	UnaryInterceptor() grpc.UnaryServerInterceptor
	Order() Order
}

// StreamInterceptor is the consumer-owned contract for one independently
// ordered stream server interceptor. Unary and stream chains are separate
// because the two call shapes are: a plugin that intercepts both contributes
// both contracts, and each chain is sorted on its own.
type StreamInterceptor interface {
	StreamInterceptor() grpc.StreamServerInterceptor
	Order() Order
}

// Order declares an interceptor's execution order. Phase is a hard boundary.
// Before and After constrain entries within or consistently across that
// boundary; they never create plugin construction dependencies.
type Order struct {
	Phase  Phase
	After  []OrderRef
	Before []OrderRef
}

// OrderRef identifies interceptors by their producing Definition key and,
// optionally, one configured instance. Its fields are private so callers use
// typed key values through Prefer, Require, and their instance variants.
type OrderRef struct {
	key      plugin.Key
	instance string
	required bool
}

// Prefer creates a soft reference to every enabled instance of key. If no
// instance of key contributes an interceptor to the chain being sorted, the
// sorter reports a miss and keeps the otherwise valid chain.
func Prefer(key plugin.Key) OrderRef {
	return OrderRef{key: key}
}

// PreferInstance creates a soft reference to exactly one instance of key. An
// empty instance explicitly selects plugin.DefaultInstance.
func PreferInstance(key plugin.Key, instance string) OrderRef {
	return OrderRef{key: key, instance: plugin.NormalizeInstance(instance)}
}

// Require creates a required reference to every enabled instance of key. If
// key has no interceptor in the chain being sorted, sorting fails.
func Require(key plugin.Key) OrderRef {
	return OrderRef{key: key, required: true}
}

// RequireInstance creates a required reference to exactly one instance of key.
// An empty instance explicitly selects plugin.DefaultInstance.
func RequireInstance(key plugin.Key, instance string) OrderRef {
	return OrderRef{key: key, instance: plugin.NormalizeInstance(instance), required: true}
}

// Key returns the referenced Definition key.
func (r OrderRef) Key() plugin.Key { return r.key }

// InstanceName returns the narrowed instance, or "" when the reference targets
// every enabled instance of the Definition.
func (r OrderRef) InstanceName() string { return r.instance }

// Required reports whether an absent target must fail sorting.
func (r OrderRef) Required() bool { return r.required }

func (r OrderRef) String() string {
	if r.instance == "" {
		return r.key.String()
	}
	return fmt.Sprintf("%s[%s]", r.key, r.instance)
}

func (r OrderRef) matches(identity plugin.Identity) bool {
	if identity.Plugin != r.key {
		return false
	}
	return r.instance == "" || plugin.NormalizeInstance(identity.Instance) == r.instance
}
