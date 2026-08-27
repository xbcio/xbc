package xbc

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

func TestStartupHooksObserveExecuteCancellation(t *testing.T) {
	tests := []struct {
		name      string
		hook      string
		migration bool
		configure func(*hooked, func(*plugin.Context) error)
		wantStops int32
	}{
		{
			name:      "Init",
			hook:      "Init",
			configure: func(p *hooked, fn func(*plugin.Context) error) { p.onInit = fn },
			// Init returned an error, so ownership never transferred to core.
			wantStops: 0,
		},
		{
			name:      "Migrate",
			hook:      "Migrate",
			migration: true,
			configure: func(p *hooked, fn func(*plugin.Context) error) { p.onMigrate = fn },
			wantStops: 1,
		},
		{
			name:      "Start",
			hook:      "Start",
			configure: func(p *hooked, fn func(*plugin.Context) error) { p.onStart = fn },
			wantStops: 1,
		},
		{
			name:      "OpenTraffic",
			hook:      "OpenTraffic",
			configure: func(p *hooked, fn func(*plugin.Context) error) { p.onOpen = fn },
			wantStops: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entered := make(chan struct{})
			observedErr := make(chan error, 1)
			var stops atomic.Int32

			app := newTestApp(t, def("cancel-aware", func() plugin.Plugin {
				p := &hooked{onStop: func(context.Context) error {
					stops.Add(1)
					return nil
				}}
				tt.configure(p, func(ctx *plugin.Context) error {
					close(entered)
					<-ctx.Done()
					observedErr <- ctx.Err()
					return ctx.Err()
				})
				return p
			}))

			executionCtx, cancel := context.WithCancel(context.Background())
			args := quietConfig(t, "")
			if tt.migration {
				args = append([]string{"--migrate"}, args...)
			}
			result := make(chan runResult, 1)
			go func() {
				code, err := app.Execute(executionCtx, args)
				result <- runResult{code: code, err: err}
			}()

			select {
			case <-entered:
			case <-time.After(testTimeout):
				t.Fatalf("%s 未进入，无法验证取消传播", tt.hook)
			}
			cancel()

			res := awaitResult(t, result)
			require.Error(t, res.err)
			assert.Equal(t, 1, res.code, "启动 hook 被取消必须以 code 1 退出")
			assert.ErrorIs(t, res.err, context.Canceled)
			assert.ErrorIs(t, <-observedErr, context.Canceled,
				"plugin.Context.Err 必须反映 Execute caller context 的取消")
			assert.Equal(t, tt.wantStops, stops.Load())
			assert.Equal(t, stopReasonContext, app.stopReason,
				"随后进入 abort(startup-failed) 不能覆盖最先到达的 context stop reason")
		})
	}
}

func TestExecuteLifecycleContextForwardsCallerDeadlineAndValue(t *testing.T) {
	type valueKey struct{}
	type observation struct {
		deadline time.Time
		hasLimit bool
		value    any
	}

	deadline := time.Now().Add(time.Minute)
	parent, cancel := context.WithDeadline(
		context.WithValue(context.Background(), valueKey{}, "caller-value"),
		deadline,
	)
	defer cancel()

	observed := make(chan observation, 1)
	app := newTestApp(t, def("caller-scope", func() plugin.Plugin {
		return &hooked{onInit: func(ctx *plugin.Context) error {
			gotDeadline, hasLimit := ctx.Deadline()
			observed <- observation{
				deadline: gotDeadline,
				hasLimit: hasLimit,
				value:    ctx.Value(valueKey{}),
			}
			<-ctx.Done()
			return ctx.Err()
		}}
	}))

	result := make(chan runResult, 1)
	go func() {
		code, err := app.Execute(parent, quietConfig(t, ""))
		result <- runResult{code: code, err: err}
	}()

	select {
	case got := <-observed:
		require.True(t, got.hasLimit)
		assert.WithinDuration(t, deadline, got.deadline, time.Millisecond)
		assert.Equal(t, "caller-value", got.value)
	case <-time.After(testTimeout):
		t.Fatal("Init 未收到 Execute 级 lifecycle context")
	}

	cancel()
	res := awaitResult(t, result)
	assert.Equal(t, 1, res.code)
	assert.ErrorIs(t, res.err, context.Canceled)
}

func TestLifecycleContextDoesNotLeakCancellationAcrossApps(t *testing.T) {
	firstSeen := make(chan *plugin.Context, 1)
	first := newTestApp(t, def("fresh-context", func() plugin.Plugin {
		return &hooked{onInit: func(ctx *plugin.Context) error {
			firstSeen <- ctx
			<-ctx.Done()
			return ctx.Err()
		}}
	}))
	firstParent, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan runResult, 1)
	go func() {
		code, err := first.Execute(firstParent, quietConfig(t, ""))
		firstResult <- runResult{code: code, err: err}
	}()
	var firstCtx *plugin.Context
	select {
	case firstCtx = <-firstSeen:
	case <-time.After(testTimeout):
		t.Fatal("第一次 Execute 未进入 Init")
	}
	cancelFirst()
	firstRes := awaitResult(t, firstResult)
	require.Equal(t, 1, firstRes.code)
	require.ErrorIs(t, firstRes.err, context.Canceled)
	require.ErrorIs(t, firstCtx.Err(), context.Canceled)

	secondSeen := make(chan *plugin.Context, 1)
	second := newTestApp(t, def("fresh-context", func() plugin.Plugin {
		return &hooked{onInit: func(ctx *plugin.Context) error {
			secondSeen <- ctx
			return nil
		}}
	}))
	secondResult := runAsync(second, quietConfig(t, "")...)
	awaitReady(t, second)

	var secondCtx *plugin.Context
	select {
	case secondCtx = <-secondSeen:
	case <-time.After(testTimeout):
		t.Fatal("第二次 Execute 未进入 Init")
	}
	require.NotSame(t, firstCtx, secondCtx)
	assert.NoError(t, secondCtx.Err(), "新 App 的 Execute 不能继承前一次取消状态")
	select {
	case <-secondCtx.Done():
		t.Fatal("新 App 的 lifecycle context 不应已取消")
	default:
	}

	second.requestStop(stopReasonSignal)
	secondRes := awaitResult(t, secondResult)
	require.NoError(t, secondRes.err)
	assert.Equal(t, 0, secondRes.code, "完全启动后的 stop 仍应保持 code 0")
}

func TestRequestStopCancelsHookBeforeUnwind(t *testing.T) {
	entered := make(chan struct{})
	// The hook reports whether Stop had already begun instead of asserting
	// through testing.T from Execute's goroutine.
	stopStartedBeforeHookReturn := make(chan bool, 1)
	var stopStarted atomic.Bool

	app := newTestApp(t, def("ordered-cancel", func() plugin.Plugin {
		return &hooked{
			onStart: func(ctx *plugin.Context) error {
				close(entered)
				<-ctx.Done()
				stopStartedBeforeHookReturn <- stopStarted.Load()
				return nil
			},
			onStop: func(context.Context) error {
				stopStarted.Store(true)
				return nil
			},
		}
	}))

	result := runAsync(app, quietConfig(t, "")...)
	select {
	case <-entered:
	case <-time.After(testTimeout):
		t.Fatal("Start 未进入")
	}
	app.requestStop(stopReasonSignal)
	select {
	case started := <-stopStartedBeforeHookReturn:
		assert.False(t, started,
			"同步 Start 尚未返回时，框架绝不能并发调用 Stop")
	case <-time.After(testTimeout):
		t.Fatal("requestStop 未先关闭 plugin.Context.Done")
	}

	res := awaitResult(t, result)
	require.Error(t, res.err, "Start 返回后仍处于启动期，停止请求必须中止启动")
	assert.Equal(t, 1, res.code)
	assert.True(t, stopStarted.Load(), "Start 返回后必须进入统一 unwind")
	assert.Equal(t, stopReasonSignal, app.stopReason,
		"abort(startup-failed) 不能覆盖最先到达的 signal stop reason")
}

func TestStartupFailureCancelsLifecycleContextBeforeStopAndPreservesRootCause(t *testing.T) {
	startupErr := errors.New("startup hook failed")
	cleanupErr := errors.New("cleanup also failed")

	tests := []struct {
		name       string
		returned   error
		panicValue string
		stopErr    error
	}{
		{
			name:     "returned error",
			returned: startupErr,
			stopErr:  cleanupErr,
		},
		{
			name:       "panic",
			panicValue: "startup hook panic",
		},
	}

	type stopObservation struct {
		doneClosed   bool
		err          error
		hookReturned bool
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lifecycleCtx *plugin.Context
			var hookReturned atomic.Bool
			observed := make(chan stopObservation, 1)

			app := newTestApp(t, def("startup-failure-cancel", func() plugin.Plugin {
				return &hooked{
					onStart: func(ctx *plugin.Context) error {
						lifecycleCtx = ctx
						defer hookReturned.Store(true)
						if tt.panicValue != "" {
							panic(tt.panicValue)
						}
						return tt.returned
					},
					onStop: func(context.Context) error {
						observation := stopObservation{
							err:          lifecycleCtx.Err(),
							hookReturned: hookReturned.Load(),
						}
						select {
						case <-lifecycleCtx.Done():
							observation.doneClosed = true
						default:
						}
						observed <- observation
						return tt.stopErr
					},
				}
			}))

			code, err := app.Execute(context.Background(), quietConfig(t, ""))

			assert.Equal(t, 1, code)
			require.Error(t, err)
			if tt.returned != nil {
				assert.ErrorIs(t, err, tt.returned,
					"清理错误不能覆盖启动失败的 root cause")
				assert.False(t, errors.Is(err, cleanupErr),
					"Stop 的次生错误只应记录日志，不能替换或混入启动 root cause")
			} else {
				assert.Contains(t, err.Error(), tt.panicValue)
				assert.Contains(t, err.Error(), "panic")
			}

			var observation stopObservation
			select {
			case observation = <-observed:
			case <-time.After(testTimeout):
				t.Fatal("startup failure unwind 未调用已初始化插件的 Stop")
			}
			assert.True(t, observation.hookReturned,
				"同步 startup hook 返回或 panic 被 recover 后，框架才能调用 Stop")
			assert.True(t, observation.doneClosed,
				"任何 startup-failed unwind 的 Stop 开始前 plugin.Context.Done 都必须关闭")
			assert.ErrorIs(t, observation.err, context.Canceled)
			assert.Equal(t, stopReasonStartupFailed, app.stopReason)
		})
	}
}

func TestStartupHookPanicsBecomeErrorsAndRollbackInitializedPlugins(t *testing.T) {
	tests := []struct {
		hook                 string
		migration            bool
		configure            func(*hooked, func(*plugin.Context) error)
		wantPanicPluginStops int32
	}{
		{
			hook:                 "Init",
			configure:            func(p *hooked, fn func(*plugin.Context) error) { p.onInit = fn },
			wantPanicPluginStops: 0,
		},
		{
			hook:                 "Migrate",
			migration:            true,
			configure:            func(p *hooked, fn func(*plugin.Context) error) { p.onMigrate = fn },
			wantPanicPluginStops: 1,
		},
		{
			hook:                 "Start",
			configure:            func(p *hooked, fn func(*plugin.Context) error) { p.onStart = fn },
			wantPanicPluginStops: 1,
		},
		{
			hook:                 "OpenTraffic",
			configure:            func(p *hooked, fn func(*plugin.Context) error) { p.onOpen = fn },
			wantPanicPluginStops: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.hook, func(t *testing.T) {
			var priorStops, panicPluginStops atomic.Int32
			panicText := "boom-" + tt.hook

			app := newTestApp(t,
				def("a-prior", func() plugin.Plugin {
					return &hooked{onStop: func(context.Context) error {
						priorStops.Add(1)
						return nil
					}}
				}),
				def("b-panicker", func() plugin.Plugin {
					p := &hooked{onStop: func(context.Context) error {
						panicPluginStops.Add(1)
						return nil
					}}
					tt.configure(p, func(*plugin.Context) error { panic(panicText) })
					return p
				}),
			)

			args := quietConfig(t, "")
			if tt.migration {
				args = append([]string{"--migrate"}, args...)
			}
			code, err := app.Execute(context.Background(), args)

			assert.Equal(t, 1, code)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "b-panicker")
			assert.Contains(t, err.Error(), `key="b-panicker"`)
			assert.Contains(t, err.Error(), `instance="default"`)
			assert.Contains(t, err.Error(), tt.hook)
			assert.Contains(t, err.Error(), "panic")
			assert.Contains(t, err.Error(), panicText)
			assert.Contains(t, err.Error(), "lifecycle_reliability_test.go",
				"panic 错误必须携带 debug stack，而不只是 panic 值")
			assert.Equal(t, int32(1), priorStops.Load(),
				"panic 之前已初始化的插件必须走统一反向清理")
			assert.Equal(t, tt.wantPanicPluginStops, panicPluginStops.Load())
		})
	}
}

type unharvestedResource struct{}

type successfulInitWithBrokenHarvest struct {
	plugin.Base
	Resource *unharvestedResource `xbc:"provide"`
	stops    *atomic.Int32
}

func (*successfulInitWithBrokenHarvest) Init(*plugin.Context) error { return nil }
func (p *successfulInitWithBrokenHarvest) Stop(context.Context) error {
	p.stops.Add(1)
	return nil
}

func TestSuccessfulInitTransfersOwnershipBeforeHarvest(t *testing.T) {
	var stops atomic.Int32
	app := newTestApp(t, def("broken-harvest", func() plugin.Plugin {
		return &successfulInitWithBrokenHarvest{stops: &stops}
	}))

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Init 后该字段仍为 nil")
	assert.Equal(t, int32(1), stops.Load(),
		"Init 返回 nil 后资源已由框架接管；紧随其后的 Harvest 失败也必须调用 Stop")
}

func TestInitErrorRetainsPluginOwnershipUntilSuccessfulReturn(t *testing.T) {
	initErr := errors.New("partial initialization failed")
	var ownedStops, failedStops atomic.Int32

	app := newTestApp(t,
		def("a-owned", func() plugin.Plugin {
			return &hooked{onStop: func(context.Context) error {
				ownedStops.Add(1)
				return nil
			}}
		}),
		def("b-failed-init", func() plugin.Plugin {
			return &hooked{
				onInit: func(*plugin.Context) error { return initErr },
				onStop: func(context.Context) error {
					failedStops.Add(1)
					return nil
				},
			}
		}),
	)

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	assert.Equal(t, 1, code)
	require.ErrorIs(t, err, initErr)
	assert.Equal(t, int32(1), ownedStops.Load(),
		"Init 已成功返回的插件资源由框架接管，后续失败必须调用 Stop")
	assert.Zero(t, failedStops.Load(),
		fmt.Sprintf("Init 返回错误的实例仍由插件自行回滚，框架不应调用其 Stop（%v）", initErr))
}
