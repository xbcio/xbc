package xbc

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/xbcio/xbc/log"
)

// Plugin is the only interface a plugin must implement.
type Plugin interface {
	Name() string
}

// Dep, Ref and Deps are declared here -- ahead of deps.go (Task 2) -- purely
// because the Declarer and Provider interfaces immediately below reference
// them: an interface naming an undefined type fails to compile the moment
// this file lands, and plugin.go must build on its own by the end of this
// task. Task 2 adds the constructors (Need, NeedNamed, Opt, Offer, RefOf,
// Ref.Instance) and the String methods that operate on these same types --
// splitting "what shape is a dependency" from "how do you build one" across
// two commits without ever leaving the package non-building in between.
type Dep struct {
	Type     reflect.Type
	Instance string // "" means default
	Optional bool
}

type Ref struct {
	typ      reflect.Type
	instance string // "" means any instance
}

type Deps struct {
	Types   []Dep    // hard: I need an instance of this type
	Plugins []Ref    // hard: this plugin must be present
	After   []string // soft ordering preference
	Before  []string
}

// ── Optional capability interfaces: implement one, get its stage ──────────
type Configurable interface{ ConfigPtr() any }
type MultiInstancer interface{ MultiInstance() bool }
type Declarer interface{ Dependencies() Deps }
type Provider interface{ Provides() []Dep }
type Initializer interface{ Init(ctx *Context) error }
type Migrator interface{ Migrate(ctx *Context) error }
type MiddlewareProvider interface{ Middlewares() []Middleware }
type RouteProvider interface{ RegisterRoutes(r *Router) }
type PostRouter interface{ PostRoutes(ctx *Context) error }
type Runner interface{ Start(ctx *Context) error }
type Closer interface {
	Stop(ctx context.Context) error
}
type HealthChecker interface {
	Health(ctx context.Context) error
}

// Base is embedded by a plugin to get three things: convenience accessors
// (Ctx, Log), automatic Name() derivation, and the anchor bindBase uses to
// hand the plugin its Context. Embedding it is optional -- a plugin that
// writes Init(ctx) itself already has everything Base would give it, via the
// ctx parameter directly.
type Base struct {
	ctx  *Context
	name string
}

// Ctx returns the Context bound to this plugin. It is nil before stage 5
// (Init): Base itself gets a name bound earlier, at registration, purely to
// resolve Name(); the Context that carries the registry and config isn't
// built until Init.
func (b *Base) Ctx() *Context { return b.ctx }

// Log is a shortcut for Ctx().Log(). Safe to call before stage 5: it falls
// back to the global logger instead of dereferencing a nil Context.
func (b *Base) Log() log.Logger {
	if b.ctx == nil {
		return log.L()
	}
	return b.ctx.Log()
}

// Name returns the framework-derived (or later explicitly bound) name. A
// plugin that writes its own Name() method shadows this outright -- Go's
// method resolution never even reaches this one in that case.
func (b *Base) Name() string { return b.name }

// base is the unexported anchor bindBase uses to reach into an embedded Base
// without knowing the plugin's own concrete type. It stays unexported so it
// can only be satisfied by actually embedding Base, never by a plugin that
// happens to define its own base() method.
func (b *Base) base() *Base { return b }

// baseAnchor is implemented by *Base and, through Go's method promotion, by
// any plugin that embeds Base. bindBase type-asserts against it to find out
// whether there is a Base to wire up at all.
type baseAnchor interface{ base() *Base }

// deriveName reflects a plugin value's concrete package path and returns its
// last segment, skipping a trailing major-version element ("/v2").
//
// It requires p to be a pointer to a named struct type: a value receiver
// can't have its Base bound (bindBase needs a pointer to mutate), and an
// anonymous type has no meaningful package-path segment to take the name
// from. Both are rejected with an error that tells the plugin author to
// just implement Name() themselves.
func deriveName(p Plugin) (string, error) {
	t := reflect.TypeOf(p)
	if t == nil {
		return "", fmt.Errorf("xbc: 无法从 nil 值推导插件名")
	}
	if t.Kind() != reflect.Pointer {
		return "", fmt.Errorf("xbc: 插件类型 %s 不是指针，无法自动推导名字，请自行实现 Name()", t)
	}
	elem := t.Elem()
	if elem.Kind() != reflect.Struct {
		return "", fmt.Errorf("xbc: 插件类型 %s 指向的不是结构体，无法自动推导名字，请自行实现 Name()", t)
	}
	if elem.Name() == "" || elem.PkgPath() == "" {
		return "", fmt.Errorf("xbc: 插件类型 %s 是匿名类型，无法自动推导名字，请自行实现 Name()", t)
	}

	segs := strings.Split(elem.PkgPath(), "/")
	last := segs[len(segs)-1]
	if len(segs) >= 2 && isMajorVersionSegment(last) {
		last = segs[len(segs)-2]
	}
	return last, nil
}

// isMajorVersionSegment reports whether s looks like a Go major-version path
// element ("v2", "v10", ...). deriveName skips it and falls back to the
// preceding path segment, so github.com/x/foo/v2 still derives "foo".
func isMajorVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// bindBase wires ctx and name into an embedded Base. Returns false when the
// plugin does not embed Base -- legal, just means no convenience accessors.
func bindBase(p Plugin, ctx *Context, name string) bool {
	a, ok := p.(baseAnchor)
	if !ok {
		return false
	}
	b := a.base()
	b.ctx = ctx
	b.name = name
	return true
}

// validateName reports whether s contains a character the framework
// reserves, returning an error describing the first one found.
//
// Reserved: '.' (middleware qualification), '[' ']' (instance display),
// whitespace, and any character outside [a-z0-9_-]. This intentionally does
// NOT lowercase s first: an uppercase letter is rejected outright rather than
// silently folded, so "Gorm" and "gorm" can never collide by accident.
func validateName(s string) error {
	if s == "" {
		return fmt.Errorf("xbc: 插件名不能为空")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return fmt.Errorf("xbc: 插件名 %q 含非法字符 %q，只允许小写字母、数字、下划线、连字符", s, r)
		}
	}
	return nil
}
