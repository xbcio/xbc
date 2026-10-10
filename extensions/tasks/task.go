package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
)

// Handler is the encoded form of a task: what an executor stores in its
// dispatch table and calls when a submission arrives. It decodes the payload
// and runs the task's function or method.
type Handler func(ctx context.Context, payload []byte) error

// Binding is one task made runnable: its name together with the two forms an
// executor needs, the encoded Handler for dispatch and a typed invocation for
// Run. Bind produces bindings for method tasks, and Funcs returns the bindings
// of function tasks; only those two construct them, so a Binding a caller holds
// always belongs to a defined task.
type Binding struct {
	name    string
	handler Handler
	invoke  func(ctx context.Context, arg any) error
}

// Name returns the task's registered name.
func (b Binding) Name() string { return b.name }

// Handler returns the encoded form of the task, the shape executors dispatch.
func (b Binding) Handler() Handler { return b.handler }

// Provider is implemented by a plugin that owns method tasks. The slice it
// returns binds each MethodTask to the plugin value, and becomes part of the
// plugin's Definition through plugin.ExportAs[tasks.Provider], so every
// executor in the application collects it.
type Provider interface {
	// Tasks returns the task bindings this plugin provides.
	Tasks() []Binding
}

// Task is a handle for a function task defined with New. Both calling forms
// derive their argument type from the defining function, so a wrong argument
// is a compile error.
type Task[A any] struct {
	name string
	fn   func(context.Context, A) error
}

// New defines a task from a function that needs nothing but its argument and
// the context. The handle is runnable immediately: Run calls fn in the
// caller's goroutine, and Submit hands an encoded argument to the installed
// executor. To let a remote executor consume submitted tasks, the application
// opts the task in by name; submitting and locally running need no
// registration beyond this definition.
//
// New panics on an empty name, a name with leading or trailing whitespace, or
// a name already defined by New or Method, so a duplicate task is a startup
// error rather than a race over which definition wins.
func New[A any](name string, fn func(context.Context, A) error) Task[A] {
	if fn == nil {
		panic(fmt.Sprintf("tasks: nil function for task %q", name))
	}
	binding := Binding{
		name: name,
		handler: func(ctx context.Context, payload []byte) error {
			var arg A
			if err := json.Unmarshal(payload, &arg); err != nil {
				return fmt.Errorf("%w: decoding task %q: %v", ErrPayload, name, err)
			}
			return fn(ctx, arg)
		},
		invoke: func(ctx context.Context, arg any) error {
			return fn(ctx, arg.(A))
		},
	}
	register(name, &binding)
	return Task[A]{name: name, fn: fn}
}

// Name returns the task's registered name.
func (t Task[A]) Name() string { return t.name }

// Run executes the task synchronously in the caller's goroutine, without
// touching an executor: fn is called with ctx and arg directly. A panic in fn
// is recovered and returned as an error.
func (t Task[A]) Run(ctx context.Context, arg A) error {
	return guard(t.name, func() error { return t.fn(ctx, arg) })
}

// Submit encodes arg and hands the task to the installed executor: the
// remote one when present, otherwise the local one. It reports acceptance
// only; whether the payload is executed somewhere else, and by which
// application, is the executor's business.
func (t Task[A]) Submit(ctx context.Context, arg A) error {
	return submitJSON(ctx, t.name, arg)
}

// MethodTask is a handle for a method task defined with Method: the handler is
// a method on a plugin, and the plugin value is supplied later, when the
// plugin binds the task through Bind.
type MethodTask[S, A any] struct {
	name   string
	method func(*S, context.Context, A) error
}

// Method defines a task whose handler is a method on a plugin. The plugin
// declares the task by binding it in Tasks: tasks.Bind(plugin, MethodTask...)
// -- and exporting tasks.Provider in its Definition. Run executes the bound
// method synchronously, consulting the installed executors for the binding;
// Submit hands an encoded argument to the installed executor.
//
// Method panics under the same conditions as New: an empty name, surrounding
// whitespace, or a name already defined.
func Method[S, A any](name string, method func(*S, context.Context, A) error) MethodTask[S, A] {
	if method == nil {
		panic(fmt.Sprintf("tasks: nil method for task %q", name))
	}
	register(name, nil)
	return MethodTask[S, A]{name: name, method: method}
}

// Name returns the task's registered name.
func (t MethodTask[S, A]) Name() string { return t.name }

// Run executes the task synchronously in the caller's goroutine through the
// binding of the plugin that provides it, located in the installed executors
// (local first, then remote). It never encodes the argument and never waits on
// a remote executor. When the process holds no binding -- the plugin is not
// part of this application -- Run reports ErrNoHandler.
func (t MethodTask[S, A]) Run(ctx context.Context, arg A) error {
	binding, ok := lookup(t.name)
	if !ok {
		return fmt.Errorf("%w: task %q", ErrNoHandler, t.name)
	}
	return guard(t.name, func() error { return binding.invoke(ctx, arg) })
}

// Submit encodes arg and hands the task to the installed executor, like
// Task.Submit. Whether this process can execute it is not part of the check:
// a submission-only application submits method tasks it does not host.
func (t MethodTask[S, A]) Submit(ctx context.Context, arg A) error {
	return submitJSON(ctx, t.name, arg)
}

// bind attaches the plugin value, producing the Binding the plugin declares.
func (t MethodTask[S, A]) bind(s *S) Binding {
	return Binding{
		name: t.name,
		handler: func(ctx context.Context, payload []byte) error {
			var arg A
			if err := json.Unmarshal(payload, &arg); err != nil {
				return fmt.Errorf("%w: decoding task %q: %v", ErrPayload, t.name, err)
			}
			return t.method(s, ctx, arg)
		},
		invoke: func(ctx context.Context, arg any) error {
			return t.method(s, ctx, arg.(A))
		},
	}
}

// MethodBinding is implemented by every MethodTask, and only inside this
// package: it exists so Bind can accept method tasks with different argument
// types from the same plugin in one call. Callers never name it.
type MethodBinding[S any] interface {
	bind(s *S) Binding
}

// Bind attaches the plugin value to one or more method tasks, producing the
// bindings the plugin declares in its Tasks method:
//
//	func (s *Service) Tasks() []tasks.Binding {
//		return tasks.Bind(s, SendConfirmation, SendReceipt)
//	}
func Bind[S any](s *S, tasks ...MethodBinding[S]) []Binding {
	bindings := make([]Binding, 0, len(tasks))
	for _, task := range tasks {
		bindings = append(bindings, task.bind(s))
	}
	return bindings
}

// Funcs returns the binding of every function task defined with New in this
// process, ordered by name. Executors are its consumers: the local executor
// takes all of them, and a remote executor consumes only the ones the
// application opted in. It returns a fresh slice each call, so a caller may
// keep or modify it freely.
func Funcs() []Binding {
	definitions.Lock()
	defer definitions.Unlock()
	names := make([]string, 0, len(definitions.funcs))
	for name := range definitions.funcs {
		names = append(names, name)
	}
	sort.Strings(names)
	bindings := make([]Binding, 0, len(names))
	for _, name := range names {
		bindings = append(bindings, definitions.funcs[name])
	}
	return bindings
}

// definitions is the process-wide task-name registry behind the definition
// panics of New and Method, and the store Funcs reads. It records claimed
// names for uniqueness and, for function tasks, their bindings. It is not a
// dispatch table: executors build their own tables from Providers and Funcs,
// and keep handler lookup out of this package.
var definitions = struct {
	sync.Mutex
	claimed map[string]struct{}
	funcs   map[string]Binding
}{
	claimed: make(map[string]struct{}),
	funcs:   make(map[string]Binding),
}

// register validates a task name and claims it for the process, panicking on
// empty or padded names and on a name New or Method already defined. A
// non-nil binding is stored for Funcs.
func register(name string, binding *Binding) {
	if name == "" {
		panic("tasks: task name must not be empty")
	}
	if strings.TrimSpace(name) != name {
		panic(fmt.Sprintf("tasks: task name %q must not have leading or trailing whitespace", name))
	}
	definitions.Lock()
	defer definitions.Unlock()
	if _, defined := definitions.claimed[name]; defined {
		panic(fmt.Sprintf("tasks: task %q is already defined", name))
	}
	definitions.claimed[name] = struct{}{}
	if binding != nil {
		definitions.funcs[name] = *binding
	}
}

// guard runs fn, turning a panic into an error. Run uses it so that a task
// body cannot take the caller's goroutine down; executors keep their own
// recovery for the Submit path.
func guard(name string, fn func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tasks: task %q panicked: %v\n%s", name, recovered, debug.Stack())
		}
	}()
	return fn()
}

// submitJSON encodes arg and routes the submission.
func submitJSON(ctx context.Context, name string, arg any) error {
	payload, err := json.Marshal(arg)
	if err != nil {
		return fmt.Errorf("%w: encoding task %q: %v", ErrPayload, name, err)
	}
	return submit(ctx, name, payload)
}
