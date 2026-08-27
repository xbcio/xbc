// middleware.go
package web

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

// Phase is a coarse, ordered anchor for middleware placement. It is a hard
// boundary: middleware in an earlier Phase always runs outside (before) all
// middleware in a later Phase, regardless of any After/Before preference.
type Phase int

// The five phases are spaced 100 apart, not a plain iota run, so a plugin
// that genuinely needs to sit outside PhaseRecover can write
// web.PhaseRecover-1 without colliding with a future inserted phase. This is
// documented as an escape hatch, not a named constant, deliberately: a
// middleware placed there runs outside panic recovery, and naming it would
// make it look like a normal, supported option.
const (
	PhaseRecover  Phase = iota * 100 // 0,   outermost: panic backstop
	PhaseObserve                     // 100, tracing, access log
	PhaseSecurity                    // 200, cors / ratelimit / replay defense
	PhaseAuth                        // 300, authentication and authorization
	PhaseBusiness                    // 400, business middleware
)

// Middleware is one entry a plugin contributes to the HTTP middleware chain.
// order.go's orderMiddlewares is the sorter that consumes this.
type Middleware struct {
	Name    string // unique identifier other middleware can reference in After/Before
	Phase   Phase
	After   []string // soft ordering preference within the same Phase
	Before  []string
	Handler gin.HandlerFunc
}

// String renders the phase name used in startup logs and error copy. An
// out-of-range value -- reached only via the PhaseRecover-1 escape hatch
// described in the design doc, or a stray literal -- falls back to
// "phase(N)" instead of an empty string, so it is always safe to print.
func (p Phase) String() string {
	switch p {
	case PhaseRecover:
		return "recover"
	case PhaseObserve:
		return "observe"
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

// qualify applies ruling R5 (carried over from the pre-split kernel design).
// A name that already contains "." is assumed to be pre-qualified and passes
// through unchanged; a name identical to its owning Definition key is left
// bare (so key "cors" contributing middleware "cors" does not render as
// "cors.cors"); everything else gets the plugin key prefixed so names stay
// unique across plugins. Callers pass Extension.Identity.Plugin as pluginKey
// -- see extension.go -- rather than deriving identity from an implementation
// type or identity method.
func qualify(pluginKey, name string) string {
	if strings.Contains(name, ".") {
		return name
	}
	if name == pluginKey {
		return pluginKey
	}
	return pluginKey + "." + name
}
