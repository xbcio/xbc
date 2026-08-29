package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// TestRunStartsAndStopsCleanly is the harness's own smoke test: one plugin
// with every lifecycle hook, started and then asked to stop, asserting the
// stages ran in the documented order and the process would have exited 0.
//
// It is deliberately the least specific test in the package. Its job is to
// fail loudly when the harness itself breaks, so that the sharply targeted
// tests in the other files fail for reasons about their own subject rather
// than about scaffolding.
func TestRunStartsAndStopsCleanly(t *testing.T) {
	var stages []string

	app := newTestApp(t, def("demo", func() plugin.Plugin {
		return &hooked{
			onInit:  func(*plugin.Context) error { stages = append(stages, "init"); return nil },
			onStart: func(*plugin.Context) error { stages = append(stages, "start"); return nil },
			onOpen:  func(*plugin.Context) error { stages = append(stages, "open"); return nil },
			onStop:  func(ctx context.Context) error { stages = append(stages, "stop"); return nil },
		}
	}))

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)

	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code, "被请求的停止是正常退出")
	assert.Equal(t, []string{"init", "start", "open", "stop"}, stages,
		"生命周期阶段顺序应为 Init -> Start -> OpenTraffic -> Stop")
}
