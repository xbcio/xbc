package inject

import (
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBase stands in for xbc.Base without importing the root package --
// internal/inject must never know that type exists. It is anonymous and
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
	assert.Equal(t, "", byName["Default"].Instance, "裸 inject 的实例名是空串，代表 default")
	assert.False(t, byName["Default"].Optional)

	require.Contains(t, byName, "Named")
	assert.Equal(t, "readonly", byName["Named"].Instance)

	require.Contains(t, byName, "Optional")
	assert.True(t, byName["Optional"].Optional)

	require.Contains(t, byName, "Both")
	assert.Equal(t, "ro", byName["Both"].Instance)
	assert.True(t, byName["Both"].Optional)

	require.Contains(t, byName, "BothRev")
	assert.Equal(t, "ro", byName["BothRev"].Instance, "选项顺序不应影响解析结果")
	assert.True(t, byName["BothRev"].Optional)

	assert.NotContains(t, byName, "Untagged", "无 tag 字段必须被跳过")
	assert.NotContains(t, byName, "Skipped", `xbc:"-" 必须被跳过`)
	assert.NotContains(t, byName, "fakeBase", "嵌入的匿名字段没有 xbc tag，必须被跳过")
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
	assert.Equal(t, "", specs[0].Instance, "产物的实例名恒为空，由插件自己的实例名决定，不能来自 tag")
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
		assert.Equal(t, s.Name, rt.Field(s.Index).Name, "Index 必须能直接定位到同一个字段，不需要二次查找")
	}
}

type provideNamedPlugin struct {
	X *int `xbc:"provide,name=x"`
}

func TestScanProvideWithNameErrors(t *testing.T) {
	_, err := Scan(&provideNamedPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不能指定 name", "产物实例名来自插件自己，不能由 tag 指定")
}

type provideOptionalPlugin struct {
	X *int `xbc:"provide,optional"`
}

func TestScanProvideWithOptionalErrors(t *testing.T) {
	_, err := Scan(&provideOptionalPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "没有可选一说")
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
	require.Error(t, err, "未导出字段带 xbc tag 必须报错——反射设不进去，静默跳过等于埋雷")
	assert.Contains(t, err.Error(), "未导出")
}

func TestScanRejectsNonPointer(t *testing.T) {
	_, err := Scan(mixedPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "指针")
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
	assert.Contains(t, err.Error(), "结构体")
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

	err := Set(p, spec, "不是 *int")
	require.Error(t, err, "类型不匹配必须报错而不是 panic")
	assert.Contains(t, err.Error(), "类型不匹配")
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
		assert.True(t, zero, "字段 %s 初始必须是零值", s.Name)
	}

	n := 1
	p.Ptr = &n
	p.Iface = errors.New("x")
	p.Slice = []string{"a"}
	p.Struct.A = 1

	for _, s := range specs {
		zero, err := IsZero(p, s)
		require.NoError(t, err)
		assert.False(t, zero, "字段 %s 赋值后不应仍是零值", s.Name)
	}
}

func TestValueReturnsCurrentValue(t *testing.T) {
	p := &setPlugin{Name: "cors"}
	spec := FieldSpec{Index: 1, Name: "Name", Type: reflect.TypeOf("")}

	v, err := Value(p, spec)
	require.NoError(t, err)
	assert.Equal(t, "cors", v)
}
