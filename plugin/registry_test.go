package plugin

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Counter is a small two-method interface, exercising the interface-typed
// registry facade path without pulling in any real infrastructure type.
type Counter interface {
	Incr(key string) int64
	Expire(key string, seconds int) error
}

type fakeDB struct{ name string }

func newRegistryTestContext(host RuntimeHost, instance string) *Context {
	return NewRuntimeContext(host, Identity{Plugin: "test", Instance: instance}, nil, nil)
}

func TestNotFoundErrorWithoutClosestReportsNothingRegistered(t *testing.T) {
	err := &NotFoundError{Want: typeOf[*fakeDB](), Instance: "default"}
	assert.Contains(t, err.Error(), "该实例下未登记任何类型")
}

func TestNotFoundErrorWithClosestReportsMissingMethods(t *testing.T) {
	err := &NotFoundError{
		Want:     typeOf[Counter](),
		Instance: "default",
		Closest:  reflect.TypeOf(&fakeDB{}),
		Missing:  []string{"Expire"},
	}
	msg := err.Error()
	assert.Contains(t, msg, "最接近的是")
	assert.Contains(t, msg, "Expire", "panic/错误文案要带上缺失的方法名，否则排查者两眼一抹黑")
}

func TestAmbiguousErrorListsCandidatesInGivenOrder(t *testing.T) {
	err := &AmbiguousError{
		Want:       typeOf[Counter](),
		Instance:   "default",
		Candidates: []reflect.Type{reflect.TypeOf(&fakeDB{}), typeOf[*bytesBufferLike]()},
	}
	msg := err.Error()
	assert.Contains(t, msg, "匹配到 2 个候选")
}

type bytesBufferLike struct{}

func TestProvideGetRoundTrip(t *testing.T) {
	host := newFakeHost()
	ctx := newRegistryTestContext(host, "default")
	db := &fakeDB{name: "primary"}
	Provide(ctx, db)

	got, ok := Get[*fakeDB](ctx)
	require.True(t, ok)
	require.Same(t, db, got)
}

func TestProvideStoresUnderCallingContextInstance(t *testing.T) {
	host := newFakeHost()
	roCtx := newRegistryTestContext(host, "readonly")
	db := &fakeDB{name: "ro"}
	Provide(roCtx, db)

	defCtx := newRegistryTestContext(host, "default")
	_, ok := Get[*fakeDB](defCtx)
	require.False(t, ok, "Provide 落在 readonly 实例名下，default 实例看不到它")

	got, ok := GetNamed[*fakeDB](defCtx, "readonly")
	require.True(t, ok)
	require.Same(t, db, got)
}

func TestGetNamedEmptyStringMeansDefaultInstance(t *testing.T) {
	host := newFakeHost()
	ctx := newRegistryTestContext(host, "default")
	db := &fakeDB{name: "x"}
	Provide(ctx, db)

	got, ok := GetNamed[*fakeDB](ctx, "")
	require.True(t, ok, "GetNamed 的空实例名经归一化后应等价于 default")
	require.Same(t, db, got)
}

func TestGetReturnsFalseWhenHostReportsNotFound(t *testing.T) {
	host := newFakeHost()
	ctx := newRegistryTestContext(host, "default")
	_, ok := Get[*fakeDB](ctx)
	assert.False(t, ok, "宿主未登记任何值时 Get 必须返回 false，而不是 panic")
}

func TestMustGetPanicsWithHostDiagnostic(t *testing.T) {
	host := newFakeHost()
	ctx := newRegistryTestContext(host, "default")

	defer func() {
		r := recover()
		require.NotNil(t, r, "MustGet 在未命中时必须 panic，不能返回零值让调用者带着 nil 指针继续跑")
		msg, ok := r.(string)
		require.True(t, ok)
		require.Contains(t, msg, "未找到类型")
	}()
	MustGet[*fakeDB](ctx)
}

// TestMustGetNamedPanicsOnTypeAssertionFailure exercises the branch where the
// host's LookupValue succeeds but the stored value cannot be asserted to T
// -- reachable only when something bypasses the Provide facade and stores a
// mismatched dynamic value directly through RuntimeHost.ProvideValue.
func TestMustGetNamedPanicsOnTypeAssertionFailure(t *testing.T) {
	host := newFakeHost()
	// Store an *int under the reflect.Type key for *fakeDB -- something
	// Provide[T] itself could never produce (it always keys by the value's
	// own concrete type), simulating a RuntimeHost implementation bug.
	n := 42
	host.ProvideValue(typeOf[*fakeDB](), "default", &n)

	ctx := newRegistryTestContext(host, "default")
	defer func() {
		r := recover()
		require.NotNil(t, r)
		msg, ok := r.(string)
		require.True(t, ok)
		assert.Contains(t, msg, "类型断言失败")
	}()
	MustGet[*fakeDB](ctx)
}
