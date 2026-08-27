// lifecycle.go
package plugin

import "context"

// ── Optional capability interfaces: implement one, get its stage ──────────
//
// A plugin implements zero or more of these; the container type-asserts for
// each one rather than requiring a fat interface, so a plugin only pays (in
// implementation surface) for the stages it actually participates in.
//
// HTTP-shaped capabilities (route/middleware registration) deliberately do
// NOT live here: this package is protocol-agnostic, and a capability that
// only a Gin-backed web application could ever implement belongs to the
// `web` module instead. HealthChecker is likewise not part of this set --
// health aggregation is an optional operations concern with no consumer in
// core, and will live in a future `management/health` package once one
// exists to define the contract.
type Configurable interface{ ConfigPtr() any }
type Declarer interface{ Dependencies() Deps }
type Provider interface{ Provides() []Dep }

// Initializer acquires an instance's runtime resources. Init is called once,
// synchronously, in dependency order. If Init returns an error or panics, the
// instance is not considered initialized and core will not call its Stop: Init
// must release everything it acquired before returning an error. Returning nil
// transfers ownership to the framework; from that point, an implemented Stop
// is guaranteed to be called during a later startup rollback or shutdown.
//
// Context implements context.Context. Potentially blocking work must observe
// ctx.Done() (or pass ctx to a context-aware dependency) so Execute caller
// cancellation and process signals can interrupt startup cooperatively. Core
// does not put Init in a detached goroutine and will not run Stop concurrently
// with an in-flight Init.
type Initializer interface{ Init(ctx *Context) error }

// Migrator performs optional migration work after every enabled instance has
// initialized. It is synchronous and must observe ctx.Done() while blocking.
type Migrator interface{ Migrate(ctx *Context) error }

// Runner is readiness phase 1: acquire resources and bind endpoints. Start
// is called by core, serially, in dependency-topological order, and must
// return quickly -- long-lived loops go through Context.GoCritical instead
// of blocking inside Start. After Start returns, the plugin must be fully
// prepared but must NOT yet accept or produce traffic; that is
// TrafficOpener's job, one full barrier later. Potentially blocking setup must
// observe ctx.Done(); core keeps Start synchronous so Stop cannot race it.
type Runner interface{ Start(ctx *Context) error }

// TrafficOpener is readiness phase 2: begin accepting traffic. Core calls it
// only after every Runner in the application has completed Start
// successfully, so a plugin that opens here can rely on the whole
// application being prepared -- not just its own dependencies. Like Start,
// OpenTraffic must return quickly; long-lived serve loops belong in
// Context.GoCritical. Potentially blocking work must observe ctx.Done(); core
// keeps OpenTraffic synchronous so Stop cannot race it.
type TrafficOpener interface{ OpenTraffic(ctx *Context) error }

type Closer interface {
	Stop(ctx context.Context) error
}
