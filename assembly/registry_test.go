package assembly

import (
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// This file ports the old root package's registry_test.go, using
// plugin.NotFoundError/plugin.AmbiguousError directly (the assembly package's own
// valueRegistry.lookup constructs and returns exactly these exported types,
// not local duplicates -- see registry.go's doc comment).

// registryCounter is a small two-method interface, exercising the
// interface-assignability matching path. Named to avoid colliding with
// resolve_test.go's resolveCounter, which has a different method set.
type registryCounter interface {
	Incr(key string) int64
	Expire(key string, seconds int) error
}

type fakeDB struct{ name string }

type counterA struct{ hits int64 }

func (c *counterA) Incr(key string) int64                { c.hits++; return c.hits }
func (c *counterA) Expire(key string, seconds int) error { return nil }

type counterB struct{}

func (c *counterB) Incr(key string) int64                { return 1 }
func (c *counterB) Expire(key string, seconds int) error { return nil }

// halfCounter implements only Incr, for testing the "zero hits, report the
// closest candidate's missing method" diagnostic path.
type halfCounter struct{}

func (c *halfCounter) Incr(key string) int64 { return 0 }

// onlyIncr and onlyExpire each implement exactly one of registryCounter's two
// methods, so against want=registryCounter they score a tie (1 method hit, 1
// missing). Registering them in this order exercises the "ties keep the
// earliest registered candidate" rule in closestMatch.
type onlyIncr struct{}

func (c *onlyIncr) Incr(key string) int64 { return 0 }

type onlyExpire struct{}

func (c *onlyExpire) Expire(key string, seconds int) error { return nil }

func TestRegistryConcreteHitAndMiss(t *testing.T) {
	r := newValueRegistry()
	db := &fakeDB{name: "primary"}
	dbType := reflect.TypeOf(db)
	r.put(dbType, "default", db)

	got, err := r.lookup(dbType, "default")
	require.NoError(t, err, "Registered concrete type should match exactly")
	require.Same(t, db, got)

	_, err = r.lookup(dbType, "readonly")
	require.Error(t, err, "Same type with different instance name should not match")
	var nfe *plugin.NotFoundError
	require.ErrorAs(t, err, &nfe)
	require.Nil(t, nfe.Closest, "Concrete type not matched has no concept of closest, Closest must be nil")
}

func TestRegistryInstanceIsolation(t *testing.T) {
	r := newValueRegistry()
	def := &fakeDB{name: "default"}
	ro := &fakeDB{name: "readonly"}
	r.put(reflect.TypeOf(def), "default", def)
	r.put(reflect.TypeOf(ro), "readonly", ro)

	got, err := r.lookup(reflect.TypeOf(def), "default")
	require.NoError(t, err)
	require.Same(t, def, got, "default instance should get default, not mix with readonly")

	got, err = r.lookup(reflect.TypeOf(ro), "readonly")
	require.NoError(t, err)
	require.Same(t, ro, got)
}

func TestRegistryLookupNormalizesInstance(t *testing.T) {
	// The registry normalizes the instance argument itself (in put and
	// lookup), so "" and "default" refer to the same key no matter which
	// spelling a caller used on either side -- resolve() and
	// validateManualProvides both call put/lookup directly, bypassing the
	// plugin.Provide/Get/GetNamed facade, so this cannot be left to the
	// facade layer alone.
	r := newValueRegistry()
	db := &fakeDB{name: "x"}
	r.put(reflect.TypeOf(db), "", db)

	got, err := r.lookup(reflect.TypeOf(db), "default")
	require.NoError(t, err, "put with empty string registered, lookup with default should match same key")
	require.Same(t, db, got)

	db2 := &fakeDB{name: "y"}
	r.put(reflect.TypeOf(db2), "default", db2)

	got2, err := r.lookup(reflect.TypeOf(db2), "")
	require.NoError(t, err, "put with default registered, lookup with empty string should match same key")
	require.Same(t, db2, got2)
}

func TestRegistryInterfaceUniqueHit(t *testing.T) {
	r := newValueRegistry()
	a := &counterA{}
	r.put(reflect.TypeOf(a), "default", a)

	want := reflect.TypeOf((*registryCounter)(nil)).Elem()
	got, err := r.lookup(want, "default")
	require.NoError(t, err, "Only concrete type implementing registryCounter should be matched")
	require.Same(t, a, got)
}

func TestRegistryInterfaceZeroHitReportsClosest(t *testing.T) {
	r := newValueRegistry()
	half := &halfCounter{}
	r.put(reflect.TypeOf(half), "default", half)

	want := reflect.TypeOf((*registryCounter)(nil)).Elem()
	_, err := r.lookup(want, "default")
	require.Error(t, err)
	var nfe *plugin.NotFoundError
	require.ErrorAs(t, err, &nfe)
	require.Equal(t, reflect.TypeOf(half), nfe.Closest, "the only candidate is the closest candidate")
	require.Equal(t, []string{"Expire"}, nfe.Missing, "halfCounter is missing only Expire method")
}

func TestRegistryClosestMatchTiesKeepEarliestRegistered(t *testing.T) {
	// onlyIncr and onlyExpire both score exactly 1 method hit against
	// want=registryCounter (2 methods total) -- a genuine tie. Ties must
	// keep the earliest-registered candidate; this pins that down, since a
	// suite with only ever one candidate registered (as in the zero-hit test
	// above) can never distinguish ">" from ">=" in closestMatch.
	r := newValueRegistry()
	first := &onlyIncr{}
	second := &onlyExpire{}
	r.put(reflect.TypeOf(first), "default", first)
	r.put(reflect.TypeOf(second), "default", second)

	want := reflect.TypeOf((*registryCounter)(nil)).Elem()
	_, err := r.lookup(want, "default")
	require.Error(t, err)
	var nfe *plugin.NotFoundError
	require.ErrorAs(t, err, &nfe)
	require.Equal(t, reflect.TypeOf(first), nfe.Closest,
		"When two candidates have same number of matching methods, keep the one registered first")
	require.Equal(t, []string{"Expire"}, nfe.Missing, "onlyIncr registered first is missing Expire")
}

func TestRegistryConcreteTypesReturnsOwnInstanceInOrder(t *testing.T) {
	r := newValueRegistry()
	a := &fakeDB{name: "a"}
	other := &counterA{}
	b := &halfCounter{}
	r.put(reflect.TypeOf(a), "default", a)
	r.put(reflect.TypeOf(other), "other", other)
	r.put(reflect.TypeOf(b), "default", b)

	got := r.concreteTypes("default")
	require.Equal(t, []reflect.Type{reflect.TypeOf(a), reflect.TypeOf(b)}, got,
		"concreteTypes should return only types from this instance, in registration order, not include other instances")
}

func TestRegistryInterfaceMultiHitReportsCandidatesInOrder(t *testing.T) {
	r := newValueRegistry()
	a := &counterA{}
	b := &counterB{}
	r.put(reflect.TypeOf(a), "default", a)
	r.put(reflect.TypeOf(b), "default", b)

	want := reflect.TypeOf((*registryCounter)(nil)).Elem()
	_, err := r.lookup(want, "default")
	require.Error(t, err)
	var ae *plugin.AmbiguousError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, []reflect.Type{reflect.TypeOf(a), reflect.TypeOf(b)}, ae.Candidates,
		"Candidate order must match registration order, otherwise error messages vary between runs and users can't reproduce")
}

func TestRegistryConcurrentPutAndLookupDoesNotRace(t *testing.T) {
	r := newValueRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.put(reflect.TypeOf(&fakeDB{}), "default", &fakeDB{name: "x"})
		}()
		go func() {
			defer wg.Done()
			_, _ = r.lookup(reflect.TypeOf(&fakeDB{}), "default")
		}()
	}
	wg.Wait()
}

// TestMustGetPanicsWithFullDiagnostics and TestProvideGetRoundTrip /
// TestProvideStoresInstanceFromCtx (the old registry_test.go's
// Provide/Get/GetNamed/MustGetNamed facade tests) are ported into
// container_test.go instead: they need a live *plugin.Context wired to a
// *Container through a RuntimeHost, which is Container-level machinery this file's
// bare *valueRegistry fixtures deliberately do not build.
