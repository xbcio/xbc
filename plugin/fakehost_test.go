// fakehost_test.go
package plugin

import (
	"context"
	"reflect"
	"sync"
)

// fakeHost is a minimal, in-memory RuntimeHost used only by this package's own
// tests. plugin cannot import the root package's real hostAdapter (that
// would be an import cycle -- the root package is the thing that imports
// plugin), so exercising Context/registry/Extensions behavior needs a local
// stand-in that implements the four RuntimeHost methods directly.
type fakeHost struct {
	mu    sync.Mutex
	store map[fakeHostKey]any

	initialized    []Extension[any]
	initializedErr error

	goCalls []fakeGoCall
}

type fakeHostKey struct {
	typ      reflect.Type
	instance string
}

type fakeGoCall struct {
	id       Identity
	critical bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{store: make(map[fakeHostKey]any)}
}

func (h *fakeHost) ProvideValue(typ reflect.Type, instance string, v any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.store[fakeHostKey{typ, instance}] = v
}

func (h *fakeHost) LookupValue(typ reflect.Type, instance string) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.store[fakeHostKey{typ, instance}]
	if !ok {
		return nil, &NotFoundError{Want: typ, Instance: instance}
	}
	return v, nil
}

func (h *fakeHost) InitializedPlugins() ([]Extension[any], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.initializedErr != nil {
		return nil, h.initializedErr
	}
	out := make([]Extension[any], len(h.initialized))
	copy(out, h.initialized)
	return out, nil
}

func (h *fakeHost) GoManaged(id Identity, fn func(context.Context), critical bool) {
	h.mu.Lock()
	h.goCalls = append(h.goCalls, fakeGoCall{id: id, critical: critical})
	h.mu.Unlock()
	fn(context.Background())
}

func (h *fakeHost) calls() []fakeGoCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]fakeGoCall, len(h.goCalls))
	copy(out, h.goCalls)
	return out
}

// admissionHost wraps a *fakeHost and additionally implements the optional
// taskAdmissionReporter probe (context.go), so tests can exercise both
// branches of Context.TasksAccepted: a host that answers the probe, and a
// bare *fakeHost that (deliberately) does not implement it at all.
type admissionHost struct {
	*fakeHost
	accepted bool
}

func (h *admissionHost) TasksAccepted() bool { return h.accepted }
