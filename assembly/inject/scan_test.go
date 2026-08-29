package inject

import (
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBase stands in for xbc.Base without importing the root package --
// assembly/inject must never know that type exists. It is anonymous and
// carries no xbc tag, so Scan must let it fall through the "no tag" branch
// exactly like the real Base does.
type fakeBase struct{ name string }

type fourFormsPlugin struct {
	fakeBase
	Default  *int `xbc:"inject"`
	Named    *int `xbc:"inject,name=readonly"`
	Optional *int `xbc:"inject,optional"`
	Both     *int `xbc:"inject,name=ro,optional"`
	BothRev  *int `xbc:"inject,optional,name=ro"`
	Untagged *int
	Skipped  *int `xbc:"-"`
}

func TestScanFourInjectForms(t *testing.T) {
	specs, err := Scan(&fourFormsPlugin{})
	require.NoError(t, err)

	byName := make(map[string]FieldSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}

	require.Contains(t, byName, "Default")
	assert.Equal(t, KindInject, byName["Default"].Kind)
	assert.Equal(t, "", byName["Default"].Instance, "the instance name of bare inject is empty string, representing default")
	assert.False(t, byName["Default"].Optional)

	require.Contains(t, byName, "Named")
	assert.Equal(t, "readonly", byName["Named"].Instance)

	require.Contains(t, byName, "Optional")
	assert.True(t, byName["Optional"].Optional)

	require.Contains(t, byName, "Both")
	assert.Equal(t, "ro", byName["Both"].Instance)
	assert.True(t, byName["Both"].Optional)

	require.Contains(t, byName, "BothRev")
	assert.Equal(t, "ro", byName["BothRev"].Instance, "the order of options should not affect parsing result")
	assert.True(t, byName["BothRev"].Optional)

	assert.NotContains(t, byName, "Untagged", "no tag field must be skipped")
	assert.NotContains(t, byName, "Skipped", `xbc:"-" must be skipped`)
	assert.NotContains(t, byName, "fakeBase", "embedded anonymous field without xbc tag must be skipped")
}

type provideOnlyPlugin struct {
	fakeBase
	Svc *int `xbc:"provide"`
}

func TestScanProvide(t *testing.T) {
	specs, err := Scan(&provideOnlyPlugin{})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, KindProvide, specs[0].Kind)
	assert.Equal(t, "", specs[0].Instance, "a provided value's instance name is always empty in the field specification; it comes from the plugin instance, not the tag")
}

type mixedPlugin struct {
	fakeBase
	DB  *int `xbc:"inject"`
	Svc *int `xbc:"provide"`
}

func TestScanMixedInjectAndProvide(t *testing.T) {
	specs, err := Scan(&mixedPlugin{})
	require.NoError(t, err)
	require.Len(t, specs, 2)

	kinds := make(map[string]Kind, 2)
	for _, s := range specs {
		kinds[s.Name] = s.Kind
	}
	assert.Equal(t, KindInject, kinds["DB"])
	assert.Equal(t, KindProvide, kinds["Svc"])
}

func TestScanIndexAddressesFieldDirectly(t *testing.T) {
	p := &mixedPlugin{}
	specs, err := Scan(p)
	require.NoError(t, err)

	rt := reflect.TypeOf(p).Elem()
	for _, s := range specs {
		assert.Equal(t, s.Name, rt.Field(s.Index).Name, "Index must directly locate the same field, no secondary lookup needed")
	}
}

type provideNamedPlugin struct {
	X *int `xbc:"provide,name=x"`
}

func TestScanProvideWithNameErrors(t *testing.T) {
	_, err := Scan(&provideNamedPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify name", "a provided value's instance name comes from the plugin itself and cannot be specified by the tag")
}

type provideOptionalPlugin struct {
	X *int `xbc:"provide,optional"`
}

func TestScanProvideWithOptionalErrors(t *testing.T) {
	_, err := Scan(&provideOptionalPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be optional")
}

type unknownActionPlugin struct {
	X *int `xbc:"injct"`
}

func TestScanUnknownActionErrorsAndListsValidOnes(t *testing.T) {
	_, err := Scan(&unknownActionPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"injct"`)
	assert.Contains(t, err.Error(), "inject")
	assert.Contains(t, err.Error(), "provide")
}

type unknownOptionPlugin struct {
	X *int `xbc:"inject,nmae=x"`
}

func TestScanUnknownOptionErrors(t *testing.T) {
	_, err := Scan(&unknownOptionPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nmae")
}

type unexportedTaggedPlugin struct {
	x int `xbc:"inject"` //nolint:unused // deliberately an unexported field, to test the error-reporting branch
}

func TestScanUnexportedFieldWithTagErrors(t *testing.T) {
	_, err := Scan(&unexportedTaggedPlugin{})
	require.Error(t, err, "unexported field with xbc tag must report error — reflection cannot set it, silently skipping equals planting a landmine")
	assert.Contains(t, err.Error(), "unexported")
}

func TestScanRejectsNonPointer(t *testing.T) {
	_, err := Scan(mixedPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pointer")
}

func TestScanRejectsNilPointer(t *testing.T) {
	var p *mixedPlugin
	_, err := Scan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestScanRejectsPointerToNonStruct(t *testing.T) {
	n := 1
	_, err := Scan(&n)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "struct")
}

type setPlugin struct {
	DB   *int
	Name string
}

func TestSetAssignsCompatibleValue(t *testing.T) {
	p := &setPlugin{}
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}
	n := 42

	require.NoError(t, Set(p, spec, &n))
	assert.Equal(t, &n, p.DB)
}

func TestSetRejectsIncompatibleType(t *testing.T) {
	p := &setPlugin{}
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}

	err := Set(p, spec, "Is not *int")
	require.Error(t, err, "Type mismatch must error rather than panic")
	assert.Contains(t, err.Error(), "type mismatch")
}

func TestSetAcceptsNilForNilableKinds(t *testing.T) {
	p := &setPlugin{DB: new(int)}
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}

	require.NoError(t, Set(p, spec, nil))
	assert.Nil(t, p.DB)
}

type zeroCheckPlugin struct {
	Ptr    *int
	Iface  error
	Slice  []string
	Struct struct{ A int }
}

func TestIsZeroCoversFourKinds(t *testing.T) {
	p := &zeroCheckPlugin{}
	specs := []FieldSpec{
		{Index: 0, Name: "Ptr", Type: reflect.TypeOf((*int)(nil))},
		{Index: 1, Name: "Iface", Type: reflect.TypeOf((*error)(nil)).Elem()},
		{Index: 2, Name: "Slice", Type: reflect.TypeOf([]string(nil))},
		{Index: 3, Name: "Struct", Type: reflect.TypeOf(struct{ A int }{})},
	}

	for _, s := range specs {
		zero, err := IsZero(p, s)
		require.NoError(t, err)
		assert.True(t, zero, "Field %s initial value must be zero value", s.Name)
	}

	n := 1
	p.Ptr = &n
	p.Iface = errors.New("x")
	p.Slice = []string{"a"}
	p.Struct.A = 1

	for _, s := range specs {
		zero, err := IsZero(p, s)
		require.NoError(t, err)
		assert.False(t, zero, "Field %s should not remain zero value after assignment", s.Name)
	}
}

func TestValueReturnsCurrentValue(t *testing.T) {
	p := &setPlugin{Name: "cors"}
	spec := FieldSpec{Index: 1, Name: "Name", Type: reflect.TypeOf("")}

	v, err := Value(p, spec)
	require.NoError(t, err)
	assert.Equal(t, "cors", v)
}

// assertFieldValueGuardErrors exercises all three fieldValue callers (Set,
// IsZero, Value) with the same (v, spec) pair and asserts each one returns an
// error containing want -- instead of panicking. Set/IsZero/Value can be
// called with a hand-built FieldSpec independently of Scan, so these guard
// branches are part of the exported contract, not internal fallback code.
func assertFieldValueGuardErrors(t *testing.T, v any, spec FieldSpec, want string) {
	t.Helper()

	err := Set(v, spec, nil)
	require.Error(t, err, "Set must error rather than panic")
	assert.Contains(t, err.Error(), want)

	_, err = IsZero(v, spec)
	require.Error(t, err, "IsZero must error rather than panic")
	assert.Contains(t, err.Error(), want)

	_, err = Value(v, spec)
	require.Error(t, err, "Value must error rather than panic")
	assert.Contains(t, err.Error(), want)
}

func TestFieldValueRejectsNonPointerReceiver(t *testing.T) {
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}
	assertFieldValueGuardErrors(t, setPlugin{}, spec, "pointer")
}

func TestFieldValueRejectsNilPointerReceiver(t *testing.T) {
	var p *setPlugin
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}
	assertFieldValueGuardErrors(t, p, spec, "pointer")
}

func TestFieldValueRejectsPointerToNonStruct(t *testing.T) {
	n := 1
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}
	assertFieldValueGuardErrors(t, &n, spec, "struct")
}

func TestFieldValueRejectsOutOfRangeIndex(t *testing.T) {
	p := &setPlugin{}
	// Derive the field count instead of hardcoding it, so adding a field to
	// setPlugin later cannot silently drop this test's coverage of the
	// boundary case (Index == NumField(), the off-by-one a "<" vs "<=" or
	// ">" vs ">=" typo would miss).
	numFields := reflect.TypeOf(*p).NumField()

	t.Run("Index equals number of fields", func(t *testing.T) {
		spec := FieldSpec{Index: numFields, Name: "Out of bounds"}
		assertFieldValueGuardErrors(t, p, spec, "out of range")
	})

	t.Run("Negative index", func(t *testing.T) {
		spec := FieldSpec{Index: -1, Name: "Out of bounds"}
		assertFieldValueGuardErrors(t, p, spec, "out of range")
	})
}
