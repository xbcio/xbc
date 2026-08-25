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

// orderedEventPlugin's Stop records into a mutex-guarded, shared event log
// instead of just its own name -- see TestShutdownStopsHTTPServerBeforePlugins,
// which needs to interleave this with an in-flight HTTP handler's own event
// on the very same log to pin down the relative order of the two.
type orderedEventPlugin struct {
	Base
	name   string
	mu     *sync.Mutex
	events *[]string
}

func (p *orderedEventPlugin) Name() string        { return p.name }
func (p *orderedEventPlugin) Init(*Context) error { return nil }
func (p *orderedEventPlugin) Stop(context.Context) error {
	p.mu.Lock()
	*p.events = append(*p.events, "stopped:"+p.name)
	p.mu.Unlock()
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
// 10's two halves: the HTTP server must fully drain (every in-flight
// request finished) before any plugin's Stop runs. Both the handler and the
// plugin's Stop append to the same mutex-guarded log, so once shutdown and
// the request have both signalled completion, the log's order is a
// deterministic fact, not a race -- there is no sleep-and-hope step here.
func TestShutdownStopsHTTPServerBeforePlugins(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})

	var mu sync.Mutex
	var events []string

	route := &routePlugin{handler: func(c *gin.Context) {
		close(reached)
		<-release
		mu.Lock()
		events = append(events, "response")
		mu.Unlock()
		c.Status(http.StatusOK)
	}}
	closer := &orderedEventPlugin{name: "gorm", mu: &mu, events: &events}

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

	close(release)

	assert.Equal(t, http.StatusOK, <-reqDone)
	require.NoError(t, <-shutdownDone)
	require.NoError(t, <-serveDone)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"response", "stopped:gorm"}, events,
		"阶段 10 必须先让 HTTP 服务器排干在飞请求，再去 Stop 插件，不能反过来或并发抢跑")
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
