package apptest

import (
	"fmt"
	"time"

	"github.com/xbcio/xbc/plugin"
)

const (
	// defaultStartTimeout bounds how long New waits for the probe route before
	// declaring the boot failed. A local process that is going to serve answers
	// well inside this; the budget is generous only because a CI machine under
	// load can be slow to schedule the goroutine.
	defaultStartTimeout = 10 * time.Second
	// probeInterval is how often the readiness route is polled. It is short
	// because a successful boot is usually one or two intervals away, and the
	// poll is a loopback request the server answers from an empty handler.
	probeInterval = 20 * time.Millisecond
	// probeTimeout bounds one readiness request, so a connection that is
	// accepted but never answered cannot consume the whole start budget.
	probeTimeout = time.Second
	// defaultClientTimeout is the request timeout of App.Client. A test that
	// asserts a slow response passes its own client.
	defaultClientTimeout = 10 * time.Second
	// shutdownBudget bounds how long cleanup waits for Execute to return after
	// its context is canceled. The generated configuration gives the runtime
	// 5s to unwind; this is the outer guard for a runtime that ignores it.
	shutdownBudget = 30 * time.Second
	// maxStartAttempts is the number of boots tried before New fails. Only the
	// port can be lost to a racing process, but every boot failure is retried:
	// the harness cannot tell a lost port from another transient failure, and a
	// retry costs milliseconds.
	maxStartAttempts = 3
)

// Option configures New before the application starts.
type Option func(*options)

// options is the resolved configuration handed to launch.
type options struct {
	bundles      []plugin.Bundle
	yamlOverlay  string
	startTimeout time.Duration
}

// WithBundles adds Bundles to the composed application. The Web server, the
// Gin engine, and the harness probe are always composed; these are appended
// after them, and a Bundle the harness already selected is collapsed by
// assembly rather than rejected.
func WithBundles(bundles ...plugin.Bundle) Option {
	return func(o *options) {
		o.bundles = append(o.bundles, bundles...)
	}
}

// WithYAML supplies a configuration overlay merged above the generated file.
// The overlay must be legal YAML for the composed plugins, and it may override
// the generated keys -- including web.addr, which turns the overlay into a way
// to test a boot that cannot start.
func WithYAML(overlay string) Option {
	return func(o *options) {
		o.yamlOverlay = overlay
	}
}

// WithStartTimeout replaces the readiness budget. It bounds how long New waits
// for the probe route, not how long the application itself may run.
func WithStartTimeout(d time.Duration) Option {
	return func(o *options) {
		o.startTimeout = d
	}
}

func newOptions(opts ...Option) options {
	resolved := options{startTimeout: defaultStartTimeout}
	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}
	return resolved
}

// validate rejects an option value New cannot honour. It reports the option
// rather than silently substituting the default, because a test that asked for
// an impossible budget has a bug the harness must not hide.
func (o options) validate() error {
	if o.startTimeout <= 0 {
		return fmt.Errorf("start timeout must be positive, got %s", o.startTimeout)
	}
	return nil
}
