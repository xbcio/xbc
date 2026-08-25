package xbc

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// depsFixturePlugin exists purely so RefOf has a concrete Plugin type to
// capture; its behavior is never exercised.
type depsFixturePlugin struct{ Base }

func (p *depsFixturePlugin) Name() string { return "deps-fixture" }

// TestTypeOfDiffersFromNaiveReflectTypeOf locks in exactly why typeOf exists:
// calling reflect.TypeOf directly on an interface-typed value gives back the
// concrete type stored inside it, not the interface type itself.
func TestTypeOfDiffersFromNaiveReflectTypeOf(t *testing.T) {
	var r io.Reader = bytes.NewBufferString("x")
	naive := reflect.TypeOf(r)
	assert.NotEqual(t, reflect.Interface, naive.Kind(),
		"对接口值直接调用 reflect.TypeOf 拿到的是背后的具体类型，不是接口类型本身")
	assert.Equal(t, reflect.Interface, typeOf[io.Reader]().Kind(),
		"typeOf 绕开这个陷阱，稳定拿到接口类型")
}

func TestTypeOfCapturesInterfaceType(t *testing.T) {
	got := typeOf[io.Reader]()
	assert.Equal(t, reflect.Interface, got.Kind(), "io.Reader 是接口类型")
	assert.Equal(t, "io.Reader", got.String())
}

func TestTypeOfCapturesAnotherInterfaceType(t *testing.T) {
	got := typeOf[fmt.Stringer]()
	assert.Equal(t, reflect.Interface, got.Kind(), "fmt.Stringer 是接口类型")
	assert.Equal(t, "fmt.Stringer", got.String())
}

func TestTypeOfCapturesConcretePointerType(t *testing.T) {
	got := typeOf[*bytes.Buffer]()
	assert.Equal(t, reflect.Pointer, got.Kind(), "*bytes.Buffer 是具体类型")
	assert.Equal(t, "*bytes.Buffer", got.String())
}

func TestNormInstanceMapsEmptyToDefault(t *testing.T) {
	assert.Equal(t, "default", normInstance(""), `空串归一为 "default"`)
}

func TestNormInstanceLeavesDefaultUnchanged(t *testing.T) {
	assert.Equal(t, "default", normInstance("default"))
}

func TestNormInstanceLeavesNamedInstanceUnchanged(t *testing.T) {
	assert.Equal(t, "readonly", normInstance("readonly"))
}

func TestNeedProducesDefaultInstanceHardDep(t *testing.T) {
	d := Need[io.Reader]()
	assert.Equal(t, typeOf[io.Reader](), d.Type)
	assert.Equal(t, "", d.Instance)
	assert.False(t, d.Optional, "Need 产生的依赖不是可选的")
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
	assert.True(t, d.Optional, "Opt 产生的依赖必须是可选的")
}

func TestOfferProducesNonOptionalDep(t *testing.T) {
	d := Offer[fmt.Stringer]()
	assert.Equal(t, typeOf[fmt.Stringer](), d.Type)
	assert.False(t, d.Optional, "Offer 描述的是产出，不存在可选一说")
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
		`显式写 "default" 与不写等价，字符串表示不应带方括号`)
}

func TestRefOfCapturesConcretePluginType(t *testing.T) {
	r := RefOf[*depsFixturePlugin]()
	assert.Equal(t, typeOf[*depsFixturePlugin](), r.typ)
	assert.Equal(t, "", r.instance, `未调用 Instance 时默认为 "任意实例"`)
}

func TestRefInstanceSetsInstance(t *testing.T) {
	r := RefOf[*depsFixturePlugin]().Instance("readonly")
	assert.Equal(t, "readonly", r.instance)
}

func TestRefStringWithoutInstance(t *testing.T) {
	r := RefOf[*depsFixturePlugin]()
	assert.Equal(t, "*xbc.depsFixturePlugin", r.String())
}

func TestRefStringWithInstance(t *testing.T) {
	r := RefOf[*depsFixturePlugin]().Instance("readonly")
	assert.Equal(t, "*xbc.depsFixturePlugin[readonly]", r.String())
}
