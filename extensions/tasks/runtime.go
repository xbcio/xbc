package tasks

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
)

// Local is the executor interface a local executor plugin installs: the
// goroutine-pool plugin behind a same-process Submit, the sink of tasks.Go,
// and one of the two places Run looks a method binding up.
type Local interface {
	// Go runs fn as a fire-and-forget unit of background work.
	Go(ctx context.Context, name string, fn func(context.Context)) error
	// Submit accepts an encoded task for local execution.
	Submit(ctx context.Context, name string, payload []byte) error
	// Lookup returns the binding of the named task this executor holds, if
	// any.
	Lookup(name string) (Binding, bool)
}

// Remote is the executor interface a remote executor plugin installs: the
// queue-backed plugin that accepts submissions for execution elsewhere. It is
// consulted first by Submit, and after the local executor by Run.
type Remote interface {
	// Submit accepts an encoded task for remote execution.
	Submit(ctx context.Context, name string, payload []byte) error
	// Lookup returns the binding of the named task this executor holds, if
	// any.
	Lookup(name string) (Binding, bool)
}

// localSlot and remoteSlot are the two process-wide executor bindings,
// installed by their plugins' Init and cleared by Stop. The zero value (nil)
// means no such executor is installed. The slots are independent: an
// application may install either, both, or neither.
var (
	localSlot  atomic.Pointer[Local]
	remoteSlot atomic.Pointer[Remote]
)

// InstallLocal installs local as the process-wide local executor, first
// come first served. When another local executor is already installed --
// another App in the same process -- it leaves the existing binding untouched
// and reports installed=false with a no-op uninstall, so the caller can warn
// without having to undo anything. When installed, the returned uninstall
// clears the slot, and only while local is still the installed executor, so
// it never removes a successor's binding; calling it more than once is safe.
func InstallLocal(local Local) (uninstall func(), installed bool) {
	if local == nil {
		return func() {}, false
	}
	if !localSlot.CompareAndSwap(nil, &local) {
		return func() {}, false
	}
	return func() { localSlot.CompareAndSwap(&local, nil) }, true
}

// InstallRemote installs remote as the process-wide remote executor, with the
// same first-come-first-served, no-op-on-conflict, and uninstall-only-own
// semantics as InstallLocal.
func InstallRemote(remote Remote) (uninstall func(), installed bool) {
	if remote == nil {
		return func() {}, false
	}
	if !remoteSlot.CompareAndSwap(nil, &remote) {
		return func() {}, false
	}
	return func() { remoteSlot.CompareAndSwap(&remote, nil) }, true
}

// Go runs fn as fire-and-forget background work in the local executor. It is
// the form for work defined nowhere: the caller supplies the function instead
// of a task handle, and Go chooses the name, or Named overrides it.
//
// Go never goes remote: a function cannot be delivered over a queue, so an
// application with only a remote executor gets ErrNotInstalled.
func Go(ctx context.Context, fn func(context.Context), opts ...GoOption) error {
	if fn == nil {
		panic("tasks: Go requires a function")
	}
	settings := goSettings{name: functionName(fn)}
	for _, option := range opts {
		if option != nil {
			option(&settings)
		}
	}
	local := localSlot.Load()
	if local == nil {
		return ErrNotInstalled
	}
	return (*local).Go(ctx, settings.name, fn)
}

// GoOption customizes one tasks.Go call.
type GoOption func(*goSettings)

// goSettings collects the options of one tasks.Go call.
type goSettings struct {
	name string
}

// Named sets the name a tasks.Go function runs under, replacing the symbol
// name Go derives from the function itself. The name labels the pool's logs
// and pprof profiles, so it follows the definition rules: it must be
// non-empty and free of surrounding whitespace, and Named panics otherwise.
func Named(name string) GoOption {
	if name == "" {
		panic("tasks: Go name must not be empty")
	}
	if strings.TrimSpace(name) != name {
		panic("tasks: Go name must not have leading or trailing whitespace")
	}
	return func(settings *goSettings) { settings.name = name }
}

// submit routes an encoded submission: the remote executor when one is
// installed, otherwise the local one, otherwise ErrNotInstalled. An installed
// remote executor's failure surfaces as its own error, wrapped by the
// executor; submit does not quietly fall back to the local executor, because
// the running application chose where its submissions go. Installed is read
// per call: an executor its Stop has uninstalled no longer receives
// submissions, and a later call routes to whichever executor remains
// installed.
func submit(ctx context.Context, name string, payload []byte) error {
	if remote := remoteSlot.Load(); remote != nil {
		return (*remote).Submit(ctx, name, payload)
	}
	if local := localSlot.Load(); local != nil {
		return (*local).Submit(ctx, name, payload)
	}
	return ErrNotInstalled
}

// lookup finds the binding Run needs for a method task: the local executor's
// first, then the remote executor's. An executor uninstalled between Load and
// the call answers from its own tables with closed behavior, which is its
// concern, not this routing's.
func lookup(name string) (Binding, bool) {
	if local := localSlot.Load(); local != nil {
		if binding, ok := (*local).Lookup(name); ok {
			return binding, true
		}
	}
	if remote := remoteSlot.Load(); remote != nil {
		if binding, ok := (*remote).Lookup(name); ok {
			return binding, true
		}
	}
	return Binding{}, false
}

// functionName returns the symbol name of fn, the default label a tasks.Go
// function runs under.
func functionName(fn func(context.Context)) string {
	if function := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()); function != nil {
		return function.Name()
	}
	return "anonymous function"
}
