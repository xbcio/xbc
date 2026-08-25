// registry_test.go
package xbc

import (
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---- everything in this file uses local fake types; no importing gorm/redis or any real middleware ----

// Counter is a small two-method interface, exercising the interface-assignability matching path.
type Counter interface {
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

// onlyIncr and onlyExpire each implement exactly one of Counter's two
// methods, so against want=Counter they score a tie (1 method hit, 1
// missing). Registering them in this order exercises the "ties keep the
// earliest registered candidate" rule in closestMatch.
type onlyIncr struct{}

func (c *onlyIncr) Incr(key string) int64 { return 0 }

type onlyExpire struct{}

func (c *onlyExpire) Expire(key string, seconds int) error { return nil }

func newTestApp() *App {
	return &App{registry: newRegistry()}
}

// newTestContext takes a shared app rather than building a fresh registry each
// time -- otherwise tests for "can one instance see what another Provide'd"
// would not exercise the real behavior at all.
func newTestContext(app *App, instance string) *Context {
	return &Context{app: app, name: "test", instance: instance}
}

func TestRegistryConcreteHitAndMiss(t *testing.T) {
	r := newRegistry()
	db := &fakeDB{name: "primary"}
	dbType := reflect.TypeOf(db)
	r.put(dbType, "default", db)

	got, err := r.lookup(dbType, "default")
	require.NoError(t, err, "已登记的具体类型应当能精确命中")
	require.Same(t, db, got)

	_, err = r.lookup(dbType, "readonly")
	require.Error(t, err, "同类型换一个实例名应当未命中")
	var nfe *NotFoundError
	require.ErrorAs(t, err, &nfe)
	require.Nil(t, nfe.Closest, "具体类型未命中没有\"最接近\"这个概念，Closest 必须是 nil")
}

func TestRegistryInstanceIsolation(t *testing.T) {
	r := newRegistry()
	def := &fakeDB{name: "default"}
	ro := &fakeDB{name: "readonly"}
	r.put(reflect.TypeOf(def), "default", def)
	r.put(reflect.TypeOf(ro), "readonly", ro)

	got, err := r.lookup(reflect.TypeOf(def), "default")
	require.NoError(t, err)
	require.Same(t, def, got, "default 实例下应取到 default 那份，不能串到 readonly")

	got, err = r.lookup(reflect.TypeOf(ro), "readonly")
	require.NoError(t, err)
	require.Same(t, ro, got)
}

func TestRegistryLookupNormalizesInstance(t *testing.T) {
	// registry normalizes the instance argument itself (in put and lookup),
	// so "" and "default" refer to the same key no matter which spelling a
	// caller used on either side. This matters because Task 10's resolve and
	// Task 13's provides check call put/lookup directly, bypassing the
	// Provide/Get/GetNamed facade -- if registry didn't normalize on its
	// own, a call site that forgot normInstance would silently miss a
	// registration made through the other spelling, instead of erroring.
	r := newRegistry()
	db := &fakeDB{name: "x"}
	r.put(reflect.TypeOf(db), "", db)

	got, err := r.lookup(reflect.TypeOf(db), "default")
	require.NoError(t, err, "put 用空串登记，lookup 用 default 查找应当命中同一个 key")
	require.Same(t, db, got)

	db2 := &fakeDB{name: "y"}
	r.put(reflect.TypeOf(db2), "default", db2)

	got2, err := r.lookup(reflect.TypeOf(db2), "")
	require.NoError(t, err, "put 用 default 登记，lookup 用空串查找应当命中同一个 key")
	require.Same(t, db2, got2)
}

func TestEmptyInstanceEqualsDefaultThroughFacade(t *testing.T) {
	app := newTestApp()
	ctx := newTestContext(app, "default")
	db := &fakeDB{name: "x"}
	Provide(ctx, db)

	got, ok := GetNamed[*fakeDB](ctx, "")
	require.True(t, ok, "GetNamed 的空实例名经归一化后应等价于 default")
	require.Same(t, db, got)

	got2, ok2 := GetNamed[*fakeDB](ctx, "default")
	require.True(t, ok2)
	require.Same(t, db, got2)
}

func TestRegistryInterfaceUniqueHit(t *testing.T) {
	r := newRegistry()
	a := &counterA{}
	r.put(reflect.TypeOf(a), "default", a)

	want := reflect.TypeOf((*Counter)(nil)).Elem()
	got, err := r.lookup(want, "default")
	require.NoError(t, err, "唯一实现了 Counter 的具体类型应当被命中")
	require.Same(t, a, got)
}

func TestRegistryInterfaceZeroHitReportsClosest(t *testing.T) {
	r := newRegistry()
	half := &halfCounter{}
	r.put(reflect.TypeOf(half), "default", half)

	want := reflect.TypeOf((*Counter)(nil)).Elem()
	_, err := r.lookup(want, "default")
	require.Error(t, err)
	var nfe *NotFoundError
	require.ErrorAs(t, err, &nfe)
	require.Equal(t, reflect.TypeOf(half), nfe.Closest, "唯一候选即最接近的候选")
	require.Equal(t, []string{"Expire"}, nfe.Missing, "halfCounter 只缺 Expire 这一个方法")
}

func TestRegistryClosestMatchTiesKeepEarliestRegistered(t *testing.T) {
	// onlyIncr and onlyExpire both score exactly 1 method hit against
	// want=Counter (2 methods total) -- a genuine tie. §5 requires ties to
	// keep the earliest-registered candidate; this pins that down, since a
	// suite with only ever one candidate registered (as in the zero-hit test
	// above) can never distinguish ">" from ">=" in closestMatch.
	r := newRegistry()
	first := &onlyIncr{}
	second := &onlyExpire{}
	r.put(reflect.TypeOf(first), "default", first)
	r.put(reflect.TypeOf(second), "default", second)

	want := reflect.TypeOf((*Counter)(nil)).Elem()
	_, err := r.lookup(want, "default")
	require.Error(t, err)
	var nfe *NotFoundError
	require.ErrorAs(t, err, &nfe)
	require.Equal(t, reflect.TypeOf(first), nfe.Closest,
		"两个候选命中方法数并列时，必须保留先登记的那个")
	require.Equal(t, []string{"Expire"}, nfe.Missing, "先登记的 onlyIncr 缺的是 Expire")
}

func TestRegistryConcreteTypesReturnsOwnInstanceInOrder(t *testing.T) {
	r := newRegistry()
	a := &fakeDB{name: "a"}
	other := &counterA{}
	b := &halfCounter{}
	r.put(reflect.TypeOf(a), "default", a)
	r.put(reflect.TypeOf(other), "other", other)
	r.put(reflect.TypeOf(b), "default", b)

	got := r.concreteTypes("default")
	require.Equal(t, []reflect.Type{reflect.TypeOf(a), reflect.TypeOf(b)}, got,
		"concreteTypes 只应返回本 instance 下的类型，且顺序与登记顺序一致，不能混入 other 实例的类型")
}

func TestRegistryInterfaceMultiHitReportsCandidatesInOrder(t *testing.T) {
	r := newRegistry()
	a := &counterA{}
	b := &counterB{}
	r.put(reflect.TypeOf(a), "default", a)
	r.put(reflect.TypeOf(b), "default", b)

	want := reflect.TypeOf((*Counter)(nil)).Elem()
	_, err := r.lookup(want, "default")
	require.Error(t, err)
	var ae *AmbiguousError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, []reflect.Type{reflect.TypeOf(a), reflect.TypeOf(b)}, ae.Candidates,
		"候选顺序必须与登记顺序一致，否则报错文案在不同运行间会飘，用户没法拿着文案去复现")
}

func TestMustGetPanicsWithFullDiagnostics(t *testing.T) {
	app := newTestApp()
	ctx := newTestContext(app, "default")
	half := &halfCounter{}
	Provide(ctx, half)

	defer func() {
		r := recover()
		require.NotNil(t, r, "MustGetNamed 在未命中时必须 panic，不能返回零值让调用者带着 nil 指针继续跑")
		msg, ok := r.(string)
		require.True(t, ok)
		require.Contains(t, msg, "Expire", "panic 文案要带上缺失的方法名，否则排查者两眼一抹黑")
	}()
	MustGetNamed[Counter](ctx, "default")
}

func TestProvideGetRoundTrip(t *testing.T) {
	app := newTestApp()
	ctx := newTestContext(app, "default")
	db := &fakeDB{name: "primary"}
	Provide(ctx, db)

	got, ok := Get[*fakeDB](ctx)
	require.True(t, ok)
	require.Same(t, db, got)
}

func TestProvideStoresInstanceFromCtx(t *testing.T) {
	app := newTestApp()
	roCtx := newTestContext(app, "readonly")
	db := &fakeDB{name: "ro"}
	Provide(roCtx, db)

	defCtx := newTestContext(app, "default")
	_, ok := Get[*fakeDB](defCtx)
	require.False(t, ok, "Provide 落在 readonly 实例名下，default 实例看不到它")

	got, ok := GetNamed[*fakeDB](defCtx, "readonly")
	require.True(t, ok)
	require.Same(t, db, got)
}

func TestRegistryConcurrentPutAndLookupDoesNotRace(t *testing.T) {
	r := newRegistry()
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
