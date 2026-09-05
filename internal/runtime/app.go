// Package runtime owns application planning, construction, lifecycle,
// managed tasks, shutdown, and process adaptation behind package xbc.
package runtime

import (
	"context"
	"io"
	"sync"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// App is one single-use application. Definition and Bundle handles are
// immutable; every mutable plan, value, lifecycle context, and task scope is
// owned by this App.
type App struct {
	bundles []plugin.Bundle

	env      *config.Environment
	settings settings
	logger   log.Logger
	plan     *assembly.Plan
	owned    *assembly.Constructed
	tasks    *taskRuntime

	// out receives read-only command output. Tests set it; otherwise the
	// process adapter's stream is used.
	out io.Writer

	executeMu sync.Mutex
	executed  bool

	stateMu           sync.Mutex
	stopRequestedFlag bool
	stopReason        string
	stopCh            chan struct{}
	trafficGate       chan struct{}
	trafficOpen       bool
	executionCtx      context.Context
	cancelExec        context.CancelCauseFunc

	unwindOnce sync.Once
	unwindErr  error
	// shutdownReport is written inside unwindOnce, so any goroutine that has
	// returned from unwind observes it.
	shutdownReport assembly.ShutdownReport

	// Tests may set ready to observe the post-gate run-loop boundary.
	ready chan struct{}
}

// Option configures New without exposing runtime-owned mutable settings.
type Option func(*appOptions) error

type appOptions struct {
	bundles    []plugin.Bundle
	hasBundles bool
}

// WithBundles composes an App explicitly from side-effect-free Bundles.
// Calling it more than once appends to the same composition; repeated
// canonical Definition handles are collapsed during planning.
func WithBundles(bundles ...plugin.Bundle) Option {
	copied := append([]plugin.Bundle(nil), bundles...)
	return func(options *appOptions) error {
		options.hasBundles = true
		options.bundles = append(options.bundles, copied...)
		return nil
	}
}

// New creates a single-use App without loading configuration, planning,
// constructing resources, or starting lifecycle hooks.
func New(options ...Option) (*App, error) {
	var resolved appOptions
	for index, option := range options {
		if option == nil {
			return nil, optionError(index, "is nil")
		}
		if err := option(&resolved); err != nil {
			return nil, err
		}
	}
	bundles := append([]plugin.Bundle(nil), resolved.bundles...)
	if !resolved.hasBundles {
		bundles = []plugin.Bundle{autoload.Freeze()}
	}
	return newApp(bundles), nil
}

func optionError(index int, detail string) error {
	return &runtimeOptionError{index: index, detail: detail}
}

type runtimeOptionError struct {
	index  int
	detail string
}

func (e *runtimeOptionError) Error() string {
	return "xbc: option " + itoa(e.index) + " " + e.detail
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}

func newApp(bundles []plugin.Bundle) *App {
	return &App{
		bundles:     append([]plugin.Bundle(nil), bundles...),
		stopCh:      make(chan struct{}),
		trafficGate: make(chan struct{}),
	}
}
