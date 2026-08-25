package xbc

import "github.com/gin-gonic/gin"

// Phase is a coarse, ordered anchor for middleware placement. It is a hard
// boundary: middleware in an earlier Phase always runs outside (before) all
// middleware in a later Phase, regardless of any After/Before preference.
type Phase int

// The five phases are spaced 100 apart, not a plain iota run, so a plugin
// that genuinely needs to sit outside PhaseRecover can write
// xbc.PhaseRecover-1 without colliding with a future inserted phase. This is
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
// Task 11 (mwchain.go) adds the sorter that consumes this; this task only
// needs the shape to exist so MiddlewareProvider compiles.
type Middleware struct {
	Name    string // unique identifier other middleware can reference in After/Before
	Phase   Phase
	After   []string // soft ordering preference within the same Phase
	Before  []string
	Handler gin.HandlerFunc
}
