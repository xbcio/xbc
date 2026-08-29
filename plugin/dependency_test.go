package plugin

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTypeOfDiffersFromNaiveReflectTypeOf locks in exactly why typeOf exists:
// calling reflect.TypeOf directly on an interface-typed value gives back the
// concrete type stored inside it, not the interface type itself.
func TestTypeOfDiffersFromNaiveReflectTypeOf(t *testing.T) {
	var r io.Reader = bytes.NewBufferString("x")
	naive := reflect.TypeOf(r)
	assert.NotEqual(t, reflect.Interface, naive.Kind(),
		"calling reflect.TypeOf on interface values directly returns the underlying concrete type, not the interface type itself")
	assert.Equal(t, reflect.Interface, typeOf[io.Reader]().Kind(),
		"typeOf bypasses this trap, reliably getting the interface type")
}

func TestTypeOfCapturesInterfaceType(t *testing.T) {
	got := typeOf[io.Reader]()
	assert.Equal(t, reflect.Interface, got.Kind(), "io.Reader is an interface type")
	assert.Equal(t, "io.Reader", got.String())
}

func TestTypeOfCapturesAnotherInterfaceType(t *testing.T) {
	got := typeOf[fmt.Stringer]()
	assert.Equal(t, reflect.Interface, got.Kind(), "fmt.Stringer is an interface type")
	assert.Equal(t, "fmt.Stringer", got.String())
}

func TestTypeOfCapturesConcretePointerType(t *testing.T) {
	got := typeOf[*bytes.Buffer]()
	assert.Equal(t, reflect.Pointer, got.Kind(), "*bytes.Buffer is a concrete type")
	assert.Equal(t, "*bytes.Buffer", got.String())
}

func TestNormalizeInstanceMapsEmptyToDefault(t *testing.T) {
	assert.Equal(t, "default", NormalizeInstance(""), `empty string is normalized to "default"`)
}

func TestNormalizeInstanceLeavesDefaultUnchanged(t *testing.T) {
	assert.Equal(t, "default", NormalizeInstance("default"))
}

func TestNormalizeInstanceLeavesNamedInstanceUnchanged(t *testing.T) {
	assert.Equal(t, "readonly", NormalizeInstance("readonly"))
}

func TestNeedProducesDefaultInstanceHardDep(t *testing.T) {
	d := Need[io.Reader]()
	assert.Equal(t, typeOf[io.Reader](), d.Type)
	assert.Equal(t, "", d.Instance)
	assert.False(t, d.Optional, "Dependency produced by Need is not optional")
}

func TestNeedNamedSetsInstance(t *testing.T) {
	d := NeedNamed[io.Reader]("readonly")
	assert.Equal(t, typeOf[io.Reader](), d.Type)
	assert.Equal(t, "readonly", d.Instance)
	assert.False(t, d.Optional)
}

func TestOptSetsOptionalTrue(t *testing.T) {
	d := Opt[io.Reader]()
	assert.Equal(t, "", d.Instance)
	assert.True(t, d.Optional, "Dependency produced by Opt must be optional")
}

func TestOfferProducesNonOptionalDep(t *testing.T) {
	d := Offer[fmt.Stringer]()
	assert.Equal(t, typeOf[fmt.Stringer](), d.Type)
	assert.False(t, d.Optional, "Offer describes output, there is no concept of optional")
}

func TestDepStringWithoutInstance(t *testing.T) {
	d := Need[*bytes.Buffer]()
	assert.Equal(t, "*bytes.Buffer", d.String())
}

func TestDepStringWithNamedInstance(t *testing.T) {
	d := NeedNamed[*bytes.Buffer]("readonly")
	assert.Equal(t, "*bytes.Buffer[readonly]", d.String())
}

func TestDepStringTreatsExplicitDefaultAsUnqualified(t *testing.T) {
	d := NeedNamed[*bytes.Buffer]("default")
	assert.Equal(t, "*bytes.Buffer", d.String(),
		`Explicitly writing "default" is equivalent to omitting it, string representation should not include brackets`)
}

func TestDepStringHandlesNilTypeWithoutPanicking(t *testing.T) {
	assert.Equal(t, "<nil>", (Dep{}).String())
}

func TestRefToCapturesStableKey(t *testing.T) {
	r := RefTo("cache")
	assert.Equal(t, Key("cache"), r.Key())
	assert.Equal(t, "", r.InstanceName(), `When Instance is not called, all enabled instances depending on this Definition are considered`)
}

func TestKeyRefIsEquivalentToRefTo(t *testing.T) {
	assert.Equal(t, RefTo("cache"), Key("cache").Ref())
}

func TestRefInstanceSetsInstance(t *testing.T) {
	r := RefTo("cache").Instance("readonly")
	assert.Equal(t, "readonly", r.InstanceName())
}

func TestRefStringWithoutInstance(t *testing.T) {
	assert.Equal(t, "cache", RefTo("cache").String())
}

func TestRefStringWithInstance(t *testing.T) {
	assert.Equal(t, "cache[readonly]", RefTo("cache").Instance("readonly").String())
}

func TestRefInstanceNameDefaultsToEmptyMeaningAllEnabledInstances(t *testing.T) {
	assert.Equal(t, "", RefTo("cache").InstanceName(), `When Instance is not called, InstanceName must be empty string, representing all enabled instances`)
}

func TestRefInstanceNameReflectsNarrowing(t *testing.T) {
	assert.Equal(t, "readonly", RefTo("cache").Instance("readonly").InstanceName())
}
