package xbc

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	kyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/xbcio/xbc/log"
)

// ---- lifecycle tracker: records method call order, so assertions about which stage was reached need no real middleware ----

type lifecycleTracker struct {
	mu    sync.Mutex
	calls []string
}

func (t *lifecycleTracker) record(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, s)
}

func (t *lifecycleTracker) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.calls...)
}

type trackedPlugin struct {
	Base
	tracker *lifecycleTracker
}

func newTrackedPlugin(tr *lifecycleTracker) *trackedPlugin { return &trackedPlugin{tracker: tr} }

func (p *trackedPlugin) Name() string { return "tracked" }
func (p *trackedPlugin) Init(ctx *Context) error {
	p.tracker.record("init")
	return nil
}
func (p *trackedPlugin) Migrate(ctx *Context) error {
	p.tracker.record("migrate")
	return nil
}
func (p *trackedPlugin) Start(ctx *Context) error {
	p.tracker.record("start")
	return nil
}
func (p *trackedPlugin) RegisterRoutes(r *Router) {
	p.tracker.record("registerRoutes")
}
func (p *trackedPlugin) Stop(ctx context.Context) error {
	p.tracker.record("stop")
	return nil
}

// newRunnableTestApp prepares an App that can reach serve() and be stopped
// early from outside: a :0 listener plus ready/criticalCh channels are
// pre-wired, so once a test receives ready it can close criticalCh to make
// serve() return immediately, with no need to guess timing via time.Sleep.
func newRunnableTestApp(t *testing.T, p Plugin) *App {
	t.Helper()
	a := &App{}
	a.ready = make(chan struct{})
	a.criticalCh = make(chan struct{})
	a.cancel = func() {}
	a.wg = &sync.WaitGroup{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a.listener = ln
	a.Register(p)
	return a
}

func TestRunNormalBootReachesServeAndShutsDownOnGoCritical(t *testing.T) {
	track := &lifecycleTracker{}
	a := newRunnableTestApp(t, newTrackedPlugin(track))

	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := a.run(nil)
		done <- struct {
			code int
			err  error
		}{code, err}
	}()

	<-a.ready
	close(a.criticalCh)
	result := <-done

	require.NoError(t, result.err)
	assert.Equal(t, 1, result.code, "GoCritical 触发的关闭退出码固定为 1")
	assert.Equal(t, []string{"init", "registerRoutes", "start", "stop"}, track.snapshot(),
		"正常启动跑阶段 1~5,7~9，不该出现 migrate；stage 7 (assembleHTTP/RegisterRoutes) 必须先于 stage 8 (startRunners/Start)")
}

func TestRunWithMigrateFlagAlsoRunsStageSix(t *testing.T) {
	track := &lifecycleTracker{}
	a := newRunnableTestApp(t, newTrackedPlugin(track))

	done := make(chan int, 1)
	go func() {
		code, _ := a.run([]string{"--migrate"})
		done <- code
	}()
	<-a.ready
	close(a.criticalCh)
	<-done

	assert.Equal(t, []string{"init", "migrate", "registerRoutes", "start", "stop"}, track.snapshot(),
		"--migrate 补跑阶段 6，且仍要保持 registerRoutes 先于 start 的阶段顺序")
}

func TestRunSubcommandsReachExpectedStagesAndExit(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantCalls []string
	}{
		{
			name:      "doctor 只到阶段 4，不调用任何 Init",
			args:      []string{"doctor"},
			wantCalls: nil,
		},
		{
			name:      "migrate 子命令跑到阶段 6 然后退出，不起 HTTP",
			args:      []string{"migrate"},
			wantCalls: []string{"init", "migrate", "stop"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			track := &lifecycleTracker{}
			a := &App{}
			a.Register(newTrackedPlugin(track))

			code, err := a.run(tc.args)
			require.NoError(t, err)
			assert.Equal(t, 0, code)
			assert.Equal(t, tc.wantCalls, track.snapshot(), tc.name)
		})
	}
}

func TestRunUnknownSubcommandReportsErrorAndDoesNotBoot(t *testing.T) {
	track := &lifecycleTracker{}
	a := &App{}
	a.Register(newTrackedPlugin(track))

	code, err := a.run([]string{"launch"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未知子命令")
	assert.Equal(t, 2, code, "命令行用法错误固定退出码 2")
	assert.Nil(t, track.snapshot(), "命令行都没解析成功，不该跑到任何阶段")
}

func TestRunConfigFileMissingIsFatalWhenExplicitlyNamed(t *testing.T) {
	a := &App{}
	_, err := a.run([]string{"--config", "/tmp/xbc-plan-does-not-exist-ever.yml"})
	require.Error(t, err, "裁决 R8：显式传了 --config 但文件不存在必须报错，不能静默按空配置跑")
}

func TestParseArgsProfileFlagTakesPriorityOverEnv(t *testing.T) {
	t.Setenv("XBC_PROFILE", "from-env")
	opts, err := parseArgs(nil)
	require.NoError(t, err)
	assert.Equal(t, "from-env", opts.profile, "没传 --profile 时落回 XBC_PROFILE")

	opts, err = parseArgs([]string{"--profile", "from-flag"})
	require.NoError(t, err)
	assert.Equal(t, "from-flag", opts.profile, "--profile 优先于 XBC_PROFILE")
}

func TestParseArgsUnknownFlagReportsErrorAndPrintsUsage(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	_, parseErr := parseArgs([]string{"--not-a-real-flag"})
	w.Close()
	os.Stderr = orig

	out, _ := io.ReadAll(r)
	assert.Error(t, parseErr, "未知 flag 必须报错")
	assert.Contains(t, string(out), "Usage", "未知 flag 必须连带打印 usage")
}

func TestParseArgsExtraPositionalArgIsAnError(t *testing.T) {
	_, err := parseArgs([]string{"--config", "x.yml", "extra-arg"})
	assert.Error(t, err, "flag 之后不该再有多余的位置参数")
}

func TestApplicationExampleYmlIsValidYAML(t *testing.T) {
	raw, err := os.ReadFile("application.example.yml")
	require.NoError(t, err, "样例配置文件必须存在于仓库根目录")

	// Uses koanf's own yaml parser (a dependency this plan has already locked
	// in), instead of pulling in an out-of-plan third-party package such as
	// gopkg.in/yaml.v3. This only does the generic check of "valid YAML with all
	// four sections present" -- fields under plugins other than server/log belong
	// to each Plan 4 plugin's own Config; kernel tests do not know gorm/redis/
	// jwt, and do not do schema-level validation here.
	doc, err := kyaml.Parser().Unmarshal(raw)
	require.NoError(t, err, "样例配置必须是合法 YAML")

	for _, section := range []string{"server", "log", "plugins", "app"} {
		_, ok := doc[section]
		assert.True(t, ok, "样例配置缺少 %s 段", section)
	}
}

// ---- pinning correction 2: initGoroutines() must run immediately before
// initAll, not at the top of run() and not merely somewhere before serve() ----

// goroutineFromInitPlugin calls ctx.Go from inside Init itself -- exactly the
// scenario goroutine.go's own doc comment on initGoroutines calls out: a
// plugin's Init already receives a live *Context and is free to start a
// managed goroutine right there. If initGoroutines() ran any later than
// "immediately before initAll", a.wg is still nil when ctx.Go reaches
// a.wg.Go(...), which panics on a nil *sync.WaitGroup.
type goroutineFromInitPlugin struct {
	Base
	started chan struct{}
}

func (p *goroutineFromInitPlugin) Name() string { return "goroutine-from-init" }
func (p *goroutineFromInitPlugin) Init(ctx *Context) error {
	ctx.Go(func(context.Context) { close(p.started) })
	return nil
}
func (p *goroutineFromInitPlugin) Stop(context.Context) error { return nil }

func TestRunAllowsInitToCallContextGoWithoutPanicking(t *testing.T) {
	p := &goroutineFromInitPlugin{started: make(chan struct{})}
	a := newRunnableTestApp(t, p)

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{-1, fmt.Errorf("run() panic：%v —— initGoroutines 很可能没有在 initAll 之前跑", r)}
			}
		}()
		code, err := a.run(nil)
		done <- result{code, err}
	}()

	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Init 里通过 ctx.Go 启动的托管 goroutine 从未运行")
	}

	<-a.ready
	close(a.criticalCh)
	res := <-done

	require.NoError(t, res.err)
	assert.Equal(t, 1, res.code)
}

// TestRunDoctorDoesNotInitializeGoroutineMachinery pins the other half of
// correction 2: doctor returns right after stage 4 (resolve) and must never
// reach initGoroutines() at all -- it never establishes a runCtx it would
// then have no matching cancel-and-wait for.
func TestRunDoctorDoesNotInitializeGoroutineMachinery(t *testing.T) {
	track := &lifecycleTracker{}
	a := &App{}
	a.Register(newTrackedPlugin(track))

	code, err := a.run([]string{"doctor"})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Nil(t, a.wg, "doctor 在阶段 4 后就返回，initGoroutines 不该被调用过")
	assert.Nil(t, a.runCtx)
	assert.Nil(t, a.cancel)
	assert.Nil(t, a.criticalCh)
}

// TestRunMigrateSubcommandInitializesGoroutineMachineryBeforeRollback pins
// that the migrate subcommand path does reach initGoroutines() (it runs
// initAll, which is stage 5, well past doctor's stage-4 exit), and that its
// shutdown-shaped tail -- cancel, then wg.Wait, then rollback -- actually
// cancels runCtx before returning (correction 3's ordering).
func TestRunMigrateSubcommandInitializesGoroutineMachineryBeforeRollback(t *testing.T) {
	track := &lifecycleTracker{}
	a := &App{}
	a.Register(newTrackedPlugin(track))

	code, err := a.run([]string{"migrate"})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	require.NotNil(t, a.wg, "migrate 跑到阶段 6，initGoroutines 必须已经执行过")
	require.NotNil(t, a.runCtx)
	require.NotNil(t, a.criticalCh)
	assert.Error(t, a.runCtx.Err(), "cancel 必须先于 rollback 调用，migrate 返回时 runCtx 应已被取消")
}

// ---- pinning correction 4: Run() must call log.Sync() before osExit ----

// sequenceRecorder records an ordered, concurrency-safe log of tagged
// events, used to assert the relative order of log.Sync() vs. osExit without
// depending on wall-clock timing.
type sequenceRecorder struct {
	mu  sync.Mutex
	log []string
}

func (r *sequenceRecorder) record(tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, tag)
}

func (r *sequenceRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

// syncSpySink is a zapcore.WriteSyncer that only records whether Sync was
// called. Inspecting log file contents cannot answer that question: the
// default file writer isn't separately buffered, so content on disk looks
// identical whether or not Sync ran. Recording the call itself is the only
// way to observe it.
type syncSpySink struct {
	seq *sequenceRecorder
}

func (s syncSpySink) Write(p []byte) (int, error) { return len(p), nil }
func (s syncSpySink) Sync() error {
	s.seq.record("sync")
	return nil
}

// zapSpyLogger is a minimal log.Logger that also implements log.ZapProvider
// -- log.Sync() only has an observable effect when the current global
// logger implements ZapProvider, so a plain capturingLogger (as used in
// startuplog_test.go) cannot be used to test this.
type zapSpyLogger struct{ zl *zap.Logger }

func (l zapSpyLogger) Debug(string, ...any)   {}
func (l zapSpyLogger) Info(string, ...any)    {}
func (l zapSpyLogger) Warn(string, ...any)    {}
func (l zapSpyLogger) Error(string, ...any)   {}
func (l zapSpyLogger) Fatal(string, ...any)   {}
func (l zapSpyLogger) With(...any) log.Logger { return l }
func (l zapSpyLogger) Enabled(log.Level) bool { return true }
func (l zapSpyLogger) Zap() *zap.Logger       { return l.zl }

func TestRunCallsLogSyncBeforeOsExit(t *testing.T) {
	seq := &sequenceRecorder{}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), syncSpySink{seq: seq}, zapcore.InfoLevel)
	log.SetLogger(zapSpyLogger{zl: zap.New(core)})
	defer log.SetLogger(log.Nop())

	origExit := osExit
	defer func() { osExit = origExit }()
	osExit = func(code int) { seq.record("exit") }

	origArgs := os.Args
	os.Args = []string{"xbc-test", "--not-a-real-flag"}
	defer func() { os.Args = origArgs }()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	origStderr := os.Stderr
	os.Stderr = w

	a := &App{}
	a.Run()

	w.Close()
	os.Stderr = origStderr
	_, _ = io.ReadAll(r) // drain and discard the usage/error noise this run intentionally produces

	assert.Equal(t, []string{"sync", "exit"}, seq.snapshot(),
		"Run() 必须先调 log.Sync() 再调 os.Exit，否则进程退出前最后一条日志可能来不及落盘")
}
