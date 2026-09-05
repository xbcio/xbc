package web

import (
	"fmt"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
)

// Phase is a coarse, ordered anchor for middleware placement. It is a hard
// boundary: middleware in an earlier Phase always runs outside (before) all
// middleware in a later Phase, regardless of any Before or After constraint.
type Phase int

// Normal extension phases are spaced 100 apart. PhaseError is the centralized
// error boundary between observation and request policy. A middleware that
// genuinely needs to sit outside PhaseRecover can use PhaseRecover-1, but then
// runs outside panic recovery.
const (
	PhaseRecover  Phase = 0   // outermost: panic backstop
	PhaseObserve  Phase = 100 // tracing, metrics, access log
	PhaseError    Phase = 150 // error boundary and focused error mapping
	PhaseSecurity Phase = 200 // cors, rate limiting, replay defense
	PhaseAuth     Phase = 300 // authentication and authorization
	PhaseBusiness Phase = 400 // business middleware
)

// Middleware is the consumer-owned contract for one independently ordered Gin
// middleware. Its identity is the Identity of the plugin.Entry that carries it;
// the contract deliberately has no third name or identity component.
type Middleware interface {
	Handler() gin.HandlerFunc
	Order() Order
}

// Order declares a middleware's Web-domain execution order. Phase is a hard
// boundary. Before and After constrain entries within or consistently across
// that boundary; they never create plugin construction dependencies.
type Order struct {
	Phase  Phase
	After  []OrderRef
	Before []OrderRef
}

// OrderRef identifies middleware by its producing Definition key and,
// optionally, one configured instance. Its fields are private so callers use
// typed key values through Prefer, Require, and their instance variants rather
// than building stringly-typed references.
type OrderRef struct {
	key      plugin.Key
	instance string
	required bool
}

// Prefer creates a soft reference to every enabled instance of key. If no
// instance of key contributes middleware, the sorter reports a miss and keeps
// the otherwise valid chain.
func Prefer(key plugin.Key) OrderRef {
	return OrderRef{key: key}
}

// PreferInstance creates a soft reference to exactly one instance of key. An
// empty instance explicitly selects plugin.DefaultInstance.
func PreferInstance(key plugin.Key, instance string) OrderRef {
	return OrderRef{key: key, instance: plugin.NormalizeInstance(instance)}
}

// Require creates a required reference to every enabled instance of key. If
// key has no middleware entry, sorting fails.
func Require(key plugin.Key) OrderRef {
	return OrderRef{key: key, required: true}
}

// RequireInstance creates a required reference to exactly one instance of key.
// An empty instance explicitly selects plugin.DefaultInstance.
func RequireInstance(key plugin.Key, instance string) OrderRef {
	return OrderRef{
		key:      key,
		instance: plugin.NormalizeInstance(instance),
		required: true,
	}
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

func (r OrderRef) asRequired() OrderRef {
	r.required = true
	return r
}

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
