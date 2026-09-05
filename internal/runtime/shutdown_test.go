package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/plugin"
)

// ownUnderCapture bootstraps app without installing a real logger, constructs
// its plan, and hands back the recorder. unwind can then be driven directly on
// the calling goroutine, so the log assertions need no synchronisation with a
// running application.
func ownUnderCapture(t *testing.T, app *App) *captureLogger {
	t.Helper()
	plan, capture := planUnderCapture(t, app, "")
	owned, err := assembly.Construct(plan, assembly.ConstructOptions{
		ShutdownTimeout: app.settings.ShutdownTimeout,
	})
	require.NoError(t, err)
	app.owned = owned
	return capture
}

// fields flattens one captured entry's key/value pairs.
func (e captureEntry) fields() map[string]any {
	values := make(map[string]any, len(e.kv)/2)
	for index := 0; index+1 < len(e.kv); index += 2 {
		key, ok := e.kv[index].(string)
		if !ok {
			continue
		}
		values[key] = e.kv[index+1]
	}
	return values
}

// TestShutdownWarningNamesEveryPluginTheBudgetSkipped pins the only signal an
// operator gets that cleanup was skipped. The §6.2 ruling makes one plugin
// that ignores its deadline cascade into every plugin below it being skipped
// outright; without this line the process simply exits and that cascade is
// invisible.
//
// The assertion discriminates on identities, not on the message: a warning
// that stopped naming the skipped plugins — or that folded "abandoned" and
// "not attempted" into one field — would still log something, and a
// message-only assertion would pass. The silent half is asserted too, because
// a warning that fired on every clean shutdown would train operators to
// ignore it.
func TestShutdownWarningNamesEveryPluginTheBudgetSkipped(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	skipped := plugin.Define("a-skipped", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Stop: func(*runtimeTestValue, context.Context) error { return nil },
	}})
	stuck := plugin.Define("z-stuck", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		Stop: func(*runtimeTestValue, context.Context) error {
			<-release
			return nil
		},
	}})

	app := newRuntimeTestApp(skipped, stuck)
	capture := ownUnderCapture(t, app)
	require.Error(t, app.unwind(stopReasonSignal))

	require.Len(t, capture.entries, 1, "the budget's casualties must be reported exactly once")
	entry := capture.entries[0]
	assert.Equal(t, "warn", entry.level)
	assert.Equal(t, "xbc: shutdown budget expired before the reverse unwind finished", entry.msg)

	fields := entry.fields()
	assert.Equal(t, stopReasonSignal, fields["reason"])
	assert.Equal(t, app.settings.ShutdownTimeout.String(), fields["budget"])
	assert.Equal(t, []string{"z-stuck"}, fields["abandoned"],
		"the plugin that ignored its deadline must be named as abandoned")
	assert.Equal(t, []string{"a-skipped"}, fields["not_attempted"],
		"every plugin the spent budget skipped must be named, not merely counted")
}

// TestShutdownReportsNothingWhenTheReverseUnwindCompletes is the other half of
// the same contract: a clean unwind must stay silent.
func TestShutdownReportsNothingWhenTheReverseUnwindCompletes(t *testing.T) {
	clean := func(key plugin.Key) plugin.Definition {
		return plugin.Define(key, func(plugin.BuildContext) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
			Stop: func(*runtimeTestValue, context.Context) error { return nil },
		}})
	}

	app := newRuntimeTestApp(clean("first"), clean("second"))
	capture := ownUnderCapture(t, app)
	started := time.Now()
	require.NoError(t, app.unwind(stopReasonSignal))

	assert.Empty(t, capture.entries, "a clean reverse unwind must not warn")
	assert.Less(t, time.Since(started), app.settings.ShutdownTimeout,
		"a clean unwind must not wait out the budget")
}
