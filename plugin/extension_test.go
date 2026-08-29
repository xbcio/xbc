package plugin

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Middler is a small capability interface used only by this test file to
// exercise Extensions[T] without depending on any protocol-specific type.
type Middler interface{ Handle() string }

type middlerA struct{ tag string }

func (m *middlerA) Handle() string { return m.tag }

// notAMiddler implements a different capability, so it must never show up
// in an Extensions[Middler] result.
type notAMiddler struct{}

func (notAMiddler) OtherCapability() {}

func TestExtensionsReturnsOnlyMatchingCapabilityInGivenOrder(t *testing.T) {
	host := newFakeHost()
	a := &middlerA{tag: "a"}
	b := &middlerA{tag: "b"}
	host.initialized = []Extension[any]{
		{Identity: Identity{Plugin: "a"}, Value: a},
		{Identity: Identity{Plugin: "irrelevant"}, Value: notAMiddler{}},
		{Identity: Identity{Plugin: "b"}, Value: b},
	}
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer"}, nil, nil)

	got, err := Extensions[Middler](ctx)
	require.NoError(t, err)
	require.Len(t, got, 2, "Instances that are not Middler must be filtered out")
	assert.Equal(t, Key("a"), got[0].Identity.Plugin)
	assert.Same(t, a, got[0].Value.(*middlerA))
	assert.Equal(t, Key("b"), got[1].Identity.Plugin)
	assert.Same(t, b, got[1].Value.(*middlerA))
}

func TestExtensionsPreservesHostOrderExactly(t *testing.T) {
	// The host's InitializedPlugins order IS the core dependency-topological
	// order (package-layout design §5.6) -- Extensions must never re-sort
	// it, e.g. alphabetically by Identity, which would silently break that
	// contract.
	host := newFakeHost()
	host.initialized = []Extension[any]{
		{Identity: Identity{Plugin: "zeta"}, Value: &middlerA{tag: "zeta"}},
		{Identity: Identity{Plugin: "alpha"}, Value: &middlerA{tag: "alpha"}},
	}
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer"}, nil, nil)

	got, err := Extensions[Middler](ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, Key("zeta"), got[0].Identity.Plugin, "Order must match the host's return, cannot be reordered")
	assert.Equal(t, Key("alpha"), got[1].Identity.Plugin)
}

func TestExtensionsReturnsEmptySliceWhenNothingMatches(t *testing.T) {
	host := newFakeHost()
	host.initialized = []Extension[any]{{Identity: Identity{Plugin: "x"}, Value: notAMiddler{}}}
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer"}, nil, nil)

	got, err := Extensions[Middler](ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestExtensionsResultIsDefensiveCopy(t *testing.T) {
	host := newFakeHost()
	host.initialized = []Extension[any]{
		{Identity: Identity{Plugin: "a"}, Value: &middlerA{tag: "a"}},
	}
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer"}, nil, nil)

	first, err := Extensions[Middler](ctx)
	require.NoError(t, err)
	first[0].Identity.Plugin = "mutated"

	second, err := Extensions[Middler](ctx)
	require.NoError(t, err)
	assert.Equal(t, Key("a"), second[0].Identity.Plugin,
		"Modifying the slice returned by the previous call must not affect the result of the next call")
}

// TestExtensionsRejectsConcreteType pins the §5.6 rule that Extensions only
// ever queries a capability (interface type) -- passing a concrete type is
// a caller bug (they meant Get[T]) and must be reported, not silently
// return zero matches.
func TestExtensionsRejectsConcreteType(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer"}, nil, nil)

	_, err := Extensions[*middlerA](ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: ")
	assert.Contains(t, err.Error(), "Get[T]", "Error should point to the specific type, use Get[T] for that")
}

// TestExtensionsAcceptsStdlibInterfaceType is a second, independent case for
// the interface-only rule using io.Reader, to make sure the check is really
// "is T an interface" and not something narrower like "is T one of our own
// capability interfaces".
func TestExtensionsAcceptsStdlibInterfaceType(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer"}, nil, nil)

	got, err := Extensions[io.Reader](ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestExtensionsPropagatesHostErrorUnchanged pins that an error from
// InitializedPlugins (e.g. "Init still in progress") is returned as-is, and
// that Extensions never falls back to handing out a partial snapshot when
// the host refuses to give one.
func TestExtensionsPropagatesHostErrorUnchanged(t *testing.T) {
	host := newFakeHost()
	sentinel := errors.New("xbc: Plugin has not completed Init yet")
	host.initializedErr = sentinel
	host.initialized = []Extension[any]{{Identity: Identity{Plugin: "a"}, Value: &middlerA{}}}
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer"}, nil, nil)

	got, err := Extensions[Middler](ctx)
	assert.Nil(t, got, "Cannot return an incomplete snapshot when the host errors")
	assert.Same(t, sentinel, err)
}
