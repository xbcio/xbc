package runtime

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

func TestExecuteCancellationStopsWithoutOwningProcess(t *testing.T) {
	var stopped bool
	app := newTestApp(t,
		def("execute-cancel", func() plugin.Plugin {
			return &hooked{onStop: func(context.Context) error { stopped = true; return nil }}
		}),
		liveness("execute-live"),
	)
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan runResult, 1)
	go func() {
		code, err := app.Execute(ctx, quietConfig(t, ""))
		result <- runResult{code: code, err: err}
	}()

	awaitReady(t, app)
	cancel()
	res := awaitResult(t, result)

	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)
	assert.Equal(t, stopReasonContext, app.stopReason)
	assert.True(t, stopped)
}

func TestExecuteUsesExplicitArgsAndNeverExitsProcess(t *testing.T) {
	// A process-owned implementation would accidentally parse this invalid
	// ambient flag instead of the explicit doctor args below.
	previousArgs := os.Args
	os.Args = []string{"xbc-test", "--ambient-invalid-flag"}
	t.Cleanup(func() { os.Args = previousArgs })

	exitCode := withFakeExit(t)
	app := newTestApp(t)
	code, err := app.Execute(context.Background(), append([]string{"doctor"}, quietConfig(t, "")...))

	require.NoError(t, err)
	assert.Equal(t, 0, code)
	_, exited := exitCode()
	assert.False(t, exited, "Execute must return its code and must never call os.Exit")
}

func TestExecuteRejectsNilContextAndSecondExecution(t *testing.T) {
	app := newTestApp(t)

	code, err := app.Execute(nil, nil)
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context")

	code, err = app.Execute(context.Background(), append([]string{"doctor"}, quietConfig(t, "")...))
	require.NoError(t, err, "a rejected nil context must not consume the App")
	assert.Equal(t, 0, code)

	code, err = app.Execute(context.Background(), nil)
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "只能调用一次")
}
