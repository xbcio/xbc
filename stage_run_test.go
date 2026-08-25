package xbc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/graph"
)

func init() { gin.SetMode(gin.TestMode) }

// ---- plugin fixtures ----

type migratorPlugin struct {
	Base
	name     string
	migrated *bool
}

func (p *migratorPlugin) Name() string { return p.name }
func (p *migratorPlugin) Migrate(ctx *Context) error {
	*p.migrated = true
	return nil
}

// failingMigratorPlugin implements both Initializer (so it can be "already
// Init'd successfully" ahead of the failure) and Migrator (whose Migrate
// always fails). See TestMigrateAllRollsBackAlreadyInitedPluginsOnFailure --
// without this fixture, migrateAll's rollback call is untestable, since
// rollback is a no-op unless some other instance in the same batch actually
// has inst.inited == true and implements Closer.
type failingMigratorPlugin struct {
	Base
	name string
	err  error
}

func (p *failingMigratorPlugin) Name() string        { return p.name }
func (p *failingMigratorPlugin) Init(*Context) error { return nil }
func (p *failingMigratorPlugin) Migrate(*Context) error {
	return p.err
}

type mwPlugin struct {
	Base
	name  string
	order *[]string
	phase Phase
	after []string
}

func (p *mwPlugin) Name() string { return p.name }
func (p *mwPlugin) Middlewares() []Middleware {
	return []Middleware{{
		Name:  p.name,
		Phase: p.phase,
		After: p.after,
		Handler: func(c *gin.Context) {
			*p.order = append(*p.order, p.name)
			c.Next()
		},
	}}
}

type routePlugin struct {
	Base
	handler gin.HandlerFunc
}

func (p *routePlugin) Name() string { return "demo" }
func (p *routePlugin) RegisterRoutes(r *Router) {
	r.GET("/ping", p.handler)
}

// multiRoutePlugin registers a caller-controlled number of routes and can be
// flagged multi-instance, so a test can tell "keyed by inst.id()" apart from
// "keyed by inst.name" (both instances share the same plugin name) and from
// "keyed by inst.label()" (the default instance of a multi-instance plugin
// renders as "multi[default]" under label(), but as plain "multi" under id()).
type multiRoutePlugin struct {
	Base
	paths []string
}

func (p *multiRoutePlugin) Name() string        { return "multi" }
func (p *multiRoutePlugin) MultiInstance() bool { return true }
func (p *multiRoutePlugin) RegisterRoutes(r *Router) {
	for _, path := range p.paths {
		r.GET(path, func(c *gin.Context) {})
	}
}

type postRouterPlugin struct {
	Base
	seen *[]RouteInfo
}

func (p *postRouterPlugin) Name() string { return "swagger" }
func (p *postRouterPlugin) PostRoutes(ctx *Context) error {
	*p.seen = append(*p.seen, ctx.Routes()...)
	return nil
}

// failingPostRouterPlugin implements Initializer (Init trivially succeeds,
// so this instance can sit in a batch alongside "an Init'd Closer") and
// PostRouter (whose PostRoutes always fails). See
// TestAssembleHTTPRollsBackAlreadyInitedPluginsOnPostRoutesFailure.
type failingPostRouterPlugin struct {
	Base
	name string
	err  error
}

func (p *failingPostRouterPlugin) Name() string        { return p.name }
func (p *failingPostRouterPlugin) Init(*Context) error { return nil }
func (p *failingPostRouterPlugin) PostRoutes(*Context) error {
	return p.err
}

// lateRoutePlugin implements both RouteProvider and PostRouter. RegisterRoutes
// stashes the *Router handed to it; PostRoutes then tries to register one
// more route through that same *Router. That call must panic, because
// freeze() runs between RegisterRoutes and PostRoutes in assembleHTTP -- this
// is the only way to observe that the route table is frozen *during*
// PostRoutes execution, as opposed to merely "test code can't add routes
// after assembleHTTP has already returned" (TestRouterHandleAfterFreezePanics
// covers that, but does not pin freeze()'s position in the sequence).
type lateRoutePlugin struct {
	Base
	router *Router
}

func (p *lateRoutePlugin) Name() string { return "late" }
func (p *lateRoutePlugin) RegisterRoutes(r *Router) {
	p.router = r
}
func (p *lateRoutePlugin) PostRoutes(*Context) error {
	p.router.GET("/late", func(c *gin.Context) {})
	return nil
}

// routeInfoPlugin registers one route with a trailing slash and one without,
// each capturing what ctx.Route(gc) reports for the request that hit it. See
// TestContextRouteMatchesTrailingSlashRoutes.
type routeInfoPlugin struct {
	Base
	withSlash    **RouteInfo
	withoutSlash **RouteInfo
}

func (p *routeInfoPlugin) Name() string { return "demo" }
func (p *routeInfoPlugin) RegisterRoutes(r *Router) {
	ctx := p.Ctx()
	r.GET("/health/", func(c *gin.Context) {
		*p.withSlash = ctx.Route(c)
		c.Status(http.StatusOK)
	})
	r.GET("/status", func(c *gin.Context) {
		*p.withoutSlash = ctx.Route(c)
		c.Status(http.StatusOK)
	})
}

type runnerPlugin struct {
	Base
	name     string
	startErr error
	stopped  *[]string
}

func (p *runnerPlugin) Name() string             { return p.name }
func (p *runnerPlugin) Init(*Context) error      { return nil }
func (p *runnerPlugin) Start(ctx *Context) error { return p.startErr }
func (p *runnerPlugin) Stop(context.Context) error {
	*p.stopped = append(*p.stopped, p.name)
	return nil
}

type closerPlugin struct {
	Base
	name    string
	stopped *[]string
	delay   time.Duration
}

func (p *closerPlugin) Name() string        { return p.name }
func (p *closerPlugin) Init(*Context) error { return nil }
func (p *closerPlugin) Stop(ctx context.Context) error {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	*p.stopped = append(*p.stopped, p.name)
	return nil
}

// signalStopPlugin's Stop closes stopCalled instead of recording into a
// shared log -- see TestShutdownStopsHTTPServerBeforePlugins, which needs to
// prove a *negative* ("Stop has not run yet") at a specific instant, and a
// closed-channel check is the one primitive that lets a bounded wait be a
// deterministic assertion instead of a sleep-and-hope guess: Stop itself
// does no I/O, so if it had already run, closing stopCalled happens
// essentially instantly, well inside any reasonable bound.
type signalStopPlugin struct {
	Base
	name       string
	stopCalled chan struct{}
}

func (p *signalStopPlugin) Name() string        { return p.name }
func (p *signalStopPlugin) Init(*Context) error { return nil }
func (p *signalStopPlugin) Stop(context.Context) error {
	close(p.stopCalled)
	return nil
}

// ---- assembly helpers ----

func newAssembleTestApp(t *testing.T) *App {
	t.Helper()
	a := newInitTestApp(t)
	a.cfg.Server.BasePath = "/"
	return a
}

// newServeTestApp finishes wiring an App already carrying its instances
// (built via mustInstance against this same a, so inst.ctx is bound to it)
// for serve()/shutdown() -- everything a test needs to drive stage 9/10
// without touching time.Sleep for synchronization.
func newServeTestApp(t *testing.T, a *App, insts []*instance) *App {
	t.Helper()
	a.cfg.Server.ShutdownTimeout = 2 * time.Second
	a.cancel = func() {}
	a.wg = &sync.WaitGroup{}
	a.criticalCh = make(chan struct{})
	a.ready = make(chan struct{})
	a.order = insts

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a.listener = ln

	require.NoError(t, a.initAll(insts))
	require.NoError(t, a.assembleHTTP(insts))
	return a
}

// ---- stage 6: Migrate ----

func TestMigrateAllDefaultDoesNotRun(t *testing.T) {
	a := newInitTestApp(t)
	var ran bool
	inst := mustInstance(t, a, &migratorPlugin{name: "user", migrated: &ran}, "user", "default")
	require.NoError(t, a.migrateAll([]*instance{inst}))
	assert.False(t, ran, "默认不迁移")
}

func TestMigrateAllRunsWhenRequested(t *testing.T) {
	a := newInitTestApp(t)
	a.migrate = true
	var ran bool
	inst := mustInstance(t, a, &migratorPlugin{name: "user", migrated: &ran}, "user", "default")
	require.NoError(t, a.migrateAll([]*instance{inst}))
	assert.True(t, ran, "--migrate / migrate 子命令 / server.auto_migrate 命中任一个都要跑迁移")
}

// The fact that "the migrate subcommand exits as soon as it finishes
// (stages 1-6, then exit, no HTTP)" is cli.go's subcommand dispatch logic,
// verified end-to-end in Task 15's cli_test.go; this test only covers
// migrateAll's own on/off behavior.

// TestMigrateAllRollsBackAlreadyInitedPluginsOnFailure pins spec §140: a
// stage 6 failure must roll back every instance that already completed Init
// successfully, the same as stage 5's own failure path. gorm sits ahead of
// the failing migrator and is a Closer, so it is the only fixture element
// that can tell "rollback runs" apart from "rollback call was silently
// dropped" -- neither the failing migrator itself (not a Closer) nor an
// instance after it in insts (Migrate never even reaches it) can.
func TestMigrateAllRollsBackAlreadyInitedPluginsOnFailure(t *testing.T) {
	a := newInitTestApp(t)
	a.migrate = true
	var stopped []string
	closer := &closerPlugin{name: "gorm", stopped: &stopped}
	failing := &failingMigratorPlugin{name: "broken", err: errors.New("模拟迁移失败")}

	closerInst := mustInstance(t, a, closer, "gorm", "default")
	failInst := mustInstance(t, a, failing, "broken", "default")
	insts := []*instance{closerInst, failInst}
	require.NoError(t, a.initAll(insts))

	err := a.migrateAll(insts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "模拟迁移失败")
	assert.Equal(t, []string{"gorm"}, stopped,
		"迁移失败必须回滚已经 Init 成功的插件，否则它已经打开的连接会在进程退出时泄漏")
}

// ---- stage 7: AssembleHTTP ----

func TestAssembleHTTPMiddlewareRunsInPhaseOrderOnARealRequest(t *testing.T) {
	a := newAssembleTestApp(t)
	var order []string

	recoverMW := &mwPlugin{name: "recover", order: &order, phase: PhaseRecover}
	observeMW := &mwPlugin{name: "observe", order: &order, phase: PhaseObserve}
	businessMW := &mwPlugin{name: "audit", order: &order, phase: PhaseBusiness}
	route := &routePlugin{handler: func(c *gin.Context) { c.Status(http.StatusOK) }}

	// Registration order is deliberately shuffled; the assertion checks that sorting only looks at Phase, not registration order.
	insts := []*instance{
		mustInstance(t, a, businessMW, "audit", "default"),
		mustInstance(t, a, route, "demo", "default"),
		mustInstance(t, a, recoverMW, "recover", "default"),
		mustInstance(t, a, observeMW, "observe", "default"),
	}

	require.NoError(t, a.assembleHTTP(insts))

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	a.router.engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"recover", "observe", "audit"}, order,
		"中间件必须按 Phase 顺序套上，与插件注册顺序无关")
}

func TestRouterHandleAfterFreezePanics(t *testing.T) {
	a := newAssembleTestApp(t)
	require.NoError(t, a.assembleHTTP(nil))

	assert.PanicsWithValue(t, "xbc: 路由表已在阶段 7 冻结，PostRoutes 里不能再加路由", func() {
		a.router.GET("/late", func(c *gin.Context) {})
	})
}

// TestFreezeHappensBeforePostRoutesRuns proves the route table is frozen
// *during* PostRoutes execution, not merely "frozen once assembleHTTP has
// returned" (which TestRouterHandleAfterFreezePanics already covers but
// does not pin the ordering within the function). late stashes the *Router
// from RegisterRoutes and tries to register through it again from inside
// PostRoutes -- freeze() must have already run by then, so this panics.
func TestFreezeHappensBeforePostRoutesRuns(t *testing.T) {
	a := newAssembleTestApp(t)
	late := &lateRoutePlugin{}
	inst := mustInstance(t, a, late, "late", "default")

	assert.Panics(t, func() {
		_ = a.assembleHTTP([]*instance{inst})
	}, "freeze() 必须在 PostRoutes 执行之前完成，PostRoutes 里加路由必须 panic")
}

// TestAssembleHTTPRollsBackAlreadyInitedPluginsOnMiddlewareOrderError covers
// the orderMiddlewares failure path: two mwPlugin fixtures deliberately
// share the same qualified middleware name ("dup" == both plugin name and
// Middleware.Name, see qualify), which orderMiddlewares rejects as a
// duplicate. gorm is a Closer and already Init'd, so it is what makes the
// missing rollback call observable.
func TestAssembleHTTPRollsBackAlreadyInitedPluginsOnMiddlewareOrderError(t *testing.T) {
	a := newAssembleTestApp(t)
	var stopped []string
	closer := &closerPlugin{name: "gorm", stopped: &stopped}
	dup1 := &mwPlugin{name: "dup", order: new([]string), phase: PhaseObserve}
	dup2 := &mwPlugin{name: "dup", order: new([]string), phase: PhaseObserve}

	insts := []*instance{
		mustInstance(t, a, closer, "gorm", "default"),
		mustInstance(t, a, dup1, "dup", "default"),
		mustInstance(t, a, dup2, "dup", "readonly"), // same qname "dup" as dup1: qualify() ignores instance name
	}
	require.NoError(t, a.initAll(insts))

	err := a.assembleHTTP(insts)
	require.Error(t, err, "重复的中间件名必须报错")
	assert.Equal(t, []string{"gorm"}, stopped,
		"排序失败也必须回滚已经 Init 成功的插件，不能只在 PostRoutes 失败时才回滚")
}

// TestAssembleHTTPRollsBackAlreadyInitedPluginsOnPostRoutesFailure covers the
// PostRoutes failure path with the same gorm-Closer fixture element.
func TestAssembleHTTPRollsBackAlreadyInitedPluginsOnPostRoutesFailure(t *testing.T) {
	a := newAssembleTestApp(t)
	var stopped []string
	closer := &closerPlugin{name: "gorm", stopped: &stopped}
	failing := &failingPostRouterPlugin{name: "swagger", err: errors.New("模拟 PostRoutes 失败")}

	insts := []*instance{
		mustInstance(t, a, closer, "gorm", "default"),
		mustInstance(t, a, failing, "swagger", "default"),
	}
	require.NoError(t, a.initAll(insts))

	err := a.assembleHTTP(insts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "模拟 PostRoutes 失败")
	assert.Equal(t, []string{"gorm"}, stopped,
		"PostRoutes 失败必须回滚已经 Init 成功的插件，否则它已经打开的连接会在进程退出时泄漏")
}

// TestContextRouteMatchesTrailingSlashRoutes pins spec §827's "c.FullPath()
// 与 RouteInfo.Path 天然对齐" claim for a route registered with a trailing
// slash, which gin's own joinPaths keeps but a bare path.Join silently
// drops. /status is the control: it must keep matching too.
func TestContextRouteMatchesTrailingSlashRoutes(t *testing.T) {
	a := newAssembleTestApp(t)
	var withSlash, withoutSlash *RouteInfo
	p := &routeInfoPlugin{withSlash: &withSlash, withoutSlash: &withoutSlash}
	insts := []*instance{mustInstance(t, a, p, "demo", "default")}
	require.NoError(t, a.assembleHTTP(insts))

	reqSlash := httptest.NewRequest(http.MethodGet, "/health/", nil)
	a.router.engine.ServeHTTP(httptest.NewRecorder(), reqSlash)
	require.NotNil(t, withSlash, "带尾斜杠注册的路由，ctx.Route 不能返回 nil")
	assert.Equal(t, "/health/", withSlash.Path)

	reqNoSlash := httptest.NewRequest(http.MethodGet, "/status", nil)
	a.router.engine.ServeHTTP(httptest.NewRecorder(), reqNoSlash)
	require.NotNil(t, withoutSlash, "不带尾斜杠的路由作为对照，也必须能命中")
	assert.Equal(t, "/status", withoutSlash.Path)
}

// TestAssembleHTTPAccumulatesSoftMissesInsteadOfOverwriting pins the "="
// vs "= append(...)" distinction on a.softMisses: stage 4's plugin sort may
// already have contributed entries before assembleHTTP ever runs, and stage
// 7's own middleware sort must add to that list, not replace it.
func TestAssembleHTTPAccumulatesSoftMissesInsteadOfOverwriting(t *testing.T) {
	a := newAssembleTestApp(t)
	a.softMisses = []graph.Miss{{Node: "stage4-node", Ref: "stage4-ref", Dir: "after"}}

	mw := &mwPlugin{name: "mw", order: new([]string), phase: PhaseObserve, after: []string{"missing-ref"}}
	insts := []*instance{mustInstance(t, a, mw, "mw", "default")}

	require.NoError(t, a.assembleHTTP(insts))

	require.Len(t, a.softMisses, 2, "阶段 7 必须累加进阶段 4 已经收集的 softMisses，不能覆盖掉")
	assert.Equal(t, "stage4-node", a.softMisses[0].Node, "阶段 4 的记录必须保留")
	assert.Equal(t, "mw", a.softMisses[1].Node, "阶段 7 自己产生的记录必须追加在后面")
	assert.Equal(t, "missing-ref", a.softMisses[1].Ref)
}

func TestPostRoutesSeesFullFrozenRouteTable(t *testing.T) {
	a := newAssembleTestApp(t)
	var seen []RouteInfo
	route := &routePlugin{handler: func(c *gin.Context) {}}
	post := &postRouterPlugin{seen: &seen}

	insts := []*instance{
		mustInstance(t, a, route, "demo", "default"),
		mustInstance(t, a, post, "swagger", "default"),
	}

	require.NoError(t, a.assembleHTTP(insts))
	require.Len(t, seen, 1, "PostRoutes 执行时路由表必须已经装满")
	assert.Equal(t, "/ping", seen[0].Path)
}

// TestAssembleHTTPRouteCountsKeyedByInstanceID pins routeCounts' map key to
// inst.id(), not inst.name (both instances below share the plugin name
// "multi", so a name-keyed map would collapse them into one entry) and not
// inst.label() (the default instance of a multi-instance plugin renders as
// "multi[default]" under label(), never under id()).
func TestAssembleHTTPRouteCountsKeyedByInstanceID(t *testing.T) {
	a := newAssembleTestApp(t)
	def := &multiRoutePlugin{paths: []string{"/a"}}
	ro := &multiRoutePlugin{paths: []string{"/b", "/c"}}
	insts := []*instance{
		mustInstance(t, a, def, "multi", "default"),
		mustInstance(t, a, ro, "multi", "readonly"),
	}

	require.NoError(t, a.assembleHTTP(insts))

	assert.Equal(t, 2, len(a.routeCounts), "两个实例的路由计数不能被同名 name 键覆盖成一份")
	assert.Equal(t, 1, a.routeCounts["multi"],
		"default 实例的路由数必须挂在 id() 形式的 key（\"multi\"）上，不是 label() 的 \"multi[default]\"")
	assert.Equal(t, 2, a.routeCounts["multi[readonly]"],
		"readonly 实例的路由数必须挂在 \"multi[readonly]\" 这个 key 上")
}

// ---- stage 8: Start ----

func TestStartRunnersFailureTriggersRollbackInReverseOrder(t *testing.T) {
	a := newInitTestApp(t)
	a.cancel = func() {}
	a.wg = &sync.WaitGroup{}
	var stopped []string

	good := &runnerPlugin{name: "cron", stopped: &stopped}
	bad := &runnerPlugin{name: "consumer", startErr: errors.New("模拟启动失败"), stopped: &stopped}

	goodInst := mustInstance(t, a, good, "cron", "default")
	badInst := mustInstance(t, a, bad, "consumer", "default")
	insts := []*instance{goodInst, badInst}
	require.NoError(t, a.initAll(insts))

	err := a.startRunners(insts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "模拟启动失败")
	assert.Equal(t, []string{"consumer", "cron"}, stopped,
		"启动失败要回滚全部已初始化的插件，按 insts 的逆序：consumer 排在后面先被 Stop")
}

// ---- stage 9/10: Serve / Shutdown ----

func TestShutdownStopsInReverseTopologicalOrder(t *testing.T) {
	a := newInitTestApp(t)
	a.cancel = func() {}
	a.wg = &sync.WaitGroup{}

	var stopped []string
	gormPlugin := &closerPlugin{name: "gorm", stopped: &stopped}
	userPlugin := &closerPlugin{name: "user", stopped: &stopped} // user depends on gorm -> sorts after it
	insts := []*instance{
		mustInstance(t, a, gormPlugin, "gorm", "default"),
		mustInstance(t, a, userPlugin, "user", "default"),
	}
	require.NoError(t, a.initAll(insts))
	a.order = insts

	require.NoError(t, a.shutdown("test"))
	assert.Equal(t, []string{"user", "gorm"}, stopped, "user 依赖 gorm，关闭必须先停 user 再停 gorm")
	assert.Equal(t, 0, a.exitCode, "非 critical 原因触发的关闭不能把退出码设为 1")
}

func TestInFlightRequestIsDrainedBeforeShutdownCompletes(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	route := &routePlugin{handler: func(c *gin.Context) {
		close(reached)
		<-release
		c.Status(http.StatusOK)
	}}
	a := newAssembleTestApp(t)
	insts := []*instance{mustInstance(t, a, route, "demo", "default")}
	a = newServeTestApp(t, a, insts)

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serve() }()
	<-a.ready

	addr := a.listener.Addr().String()
	reqDone := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/ping")
		require.NoError(t, err)
		reqDone <- resp.StatusCode
	}()
	<-reached // the request has already entered the handler and is stuck there

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- a.shutdown("test") }()

	// Instead of sleeping to wait for "shutdown should be waiting on the
	// in-flight request", release the handler directly and let channel ordering
	// converge: the request must finish first, and only then should shutdown
	// return.
	close(release)

	assert.Equal(t, http.StatusOK, <-reqDone, "in-flight 请求必须正常跑完，不能被 shutdown 打断")
	require.NoError(t, <-shutdownDone)
	require.NoError(t, <-serveDone)
}

func TestShutdownTimeoutForceKillsStuckConnection(t *testing.T) {
	block := make(chan struct{})
	reached := make(chan struct{})
	route := &routePlugin{handler: func(c *gin.Context) {
		close(reached)
		<-block // deliberately never released, simulating a connection that ignores the shutdown signal
	}}
	a := newAssembleTestApp(t)
	insts := []*instance{mustInstance(t, a, route, "demo", "default")}
	a = newServeTestApp(t, a, insts)
	a.cfg.Server.ShutdownTimeout = 200 * time.Millisecond

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serve() }()
	<-a.ready

	addr := a.listener.Addr().String()
	go func() { _, _ = http.Get("http://" + addr + "/ping") }()
	<-reached

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- a.shutdown("test") }()

	select {
	case err := <-shutdownErr:
		assert.NoError(t, err, "超时后应该强杀退出，不能无限期等一个不肯放手的连接")
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown 应该在 shutdown_timeout 后强杀，不应该卡住")
	}
	close(block)
	<-serveDone
}

// TestShutdownStopsHTTPServerBeforePlugins pins the ordering between stage
// 10's two halves: the HTTP server must fully drain (every in-flight request
// finished) before any plugin's Stop runs. The check is deliberately a
// negative one, taken *while the request is still deliberately stuck in its
// handler*: at that instant, a correct shutdown() cannot possibly have
// reached the plugin-stop step yet, because http.Server.Shutdown blocks
// synchronously on that very connection -- so stopCalled provably cannot be
// closed yet, no matter how slow or fast the test machine is. A regression
// that reorders the two steps calls Stop with no I/O in between, so it closes
// stopCalled essentially instantly; the bounded wait below only needs to be
// long enough to reliably observe that, not to "guess" anything.
func TestShutdownStopsHTTPServerBeforePlugins(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	stopCalled := make(chan struct{})

	route := &routePlugin{handler: func(c *gin.Context) {
		close(reached)
		<-release
		c.Status(http.StatusOK)
	}}
	closer := &signalStopPlugin{name: "gorm", stopCalled: stopCalled}

	a := newAssembleTestApp(t)
	insts := []*instance{
		mustInstance(t, a, route, "demo", "default"),
		mustInstance(t, a, closer, "gorm", "default"),
	}
	a = newServeTestApp(t, a, insts)

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serve() }()
	<-a.ready

	addr := a.listener.Addr().String()
	reqDone := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/ping")
		require.NoError(t, err)
		reqDone <- resp.StatusCode
	}()
	<-reached

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- a.shutdown("test") }()

	select {
	case <-stopCalled:
		t.Fatal("阶段 10 必须先让 HTTP 服务器排干在飞请求，插件的 Stop 不能在请求还卡着的时候被调用")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	assert.Equal(t, http.StatusOK, <-reqDone, "in-flight 请求必须正常跑完，不能被 shutdown 打断")
	mustClosed(t, stopCalled, "HTTP 服务器排干后插件必须被 Stop")
	require.NoError(t, <-shutdownDone)
	require.NoError(t, <-serveDone)
}

func TestGoCriticalTriggersFullShutdownAndExitCodeOne(t *testing.T) {
	var stopped []string
	closer := &closerPlugin{name: "gorm", stopped: &stopped}
	a := newAssembleTestApp(t)
	insts := []*instance{mustInstance(t, a, closer, "gorm", "default")}
	a = newServeTestApp(t, a, insts)

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serve() }()
	<-a.ready

	close(a.criticalCh) // simulates a GoCritical trigger

	require.NoError(t, <-serveDone)
	assert.Equal(t, []string{"gorm"}, stopped, "GoCritical 触发的关闭仍要走完整阶段 10，其他插件照样被 Stop")
	assert.Equal(t, 1, a.exitCode, "GoCritical 触发的退出码必须是 1")
}

// TestServeErrorStillRunsShutdown covers the third arm of serve()'s select:
// Serve returning on its own for an unplanned reason (here: someone closes
// a.listener directly, not through httpServer.Shutdown/Close, so Serve's
// Accept loop dies with an ordinary "closed" error instead of the deliberate
// http.ErrServerClosed sentinel). That must still walk through shutdown()
// -- gorm's Stop is the only fixture element that can tell "shutdown ran"
// apart from "the error was just returned, no stage 10 at all".
func TestServeErrorStillRunsShutdown(t *testing.T) {
	var stopped []string
	closer := &closerPlugin{name: "gorm", stopped: &stopped}
	a := newAssembleTestApp(t)
	insts := []*instance{mustInstance(t, a, closer, "gorm", "default")}
	a = newServeTestApp(t, a, insts)

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serve() }()
	<-a.ready

	require.NoError(t, a.listener.Close())

	err := <-serveDone
	require.Error(t, err, "非计划内的 Serve 错误必须原样返回")
	assert.False(t, errors.Is(err, http.ErrServerClosed),
		"这条路径必须是意料之外的错误，不是优雅关闭的 http.ErrServerClosed")
	assert.Equal(t, []string{"gorm"}, stopped,
		"Serve 异常退出也必须走完阶段 10，把已经 Init 的插件 Stop 掉，不能绕过优雅关闭")
	assert.Equal(t, 1, a.exitCode, "非计划内的 Serve 错误退出码必须是 1")
}
