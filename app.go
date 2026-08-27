package xbc

import (
	"sync"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/internal/container"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin/catalog"
)

// App is one assembled application: one frozen definition set, one
// configuration environment, one container of live plugin instances, one
// managed task group.
//
// Runtime state owned by the framework lives on this struct: plugin instances,
// value registry, Contexts, and managed task groups are separate for each App.
// Process facilities such as environment variables and the configured global
// logger remain shared; use separate processes when those also need isolation.
//
// This single type is both the public handle and the runtime state. An earlier
// layout split it in two -- an exported facade in this package wrapping an
// internal/runtime.App -- and that split paid for nothing measurable: the two
// packages had byte-identical dependency closures, so it bought no dependency
// isolation, and the only property it did enforce (a capitalized identifier
// added to the runtime cannot leak into the public API) is enforced here
// instead by the exported-symbol allowlist in tests/architecture. What the
// split cost was concrete: a pass-through wrapper type whose Execute forwarded
// verbatim, four exported symbols that existed solely to cross the boundary,
// and two types named App in one codebase. Every method below except Execute
// is unexported and therefore invisible to callers outside this package.
type App struct {
	// snapshot is the immutable definition set this App assembles. Frozen
	// before the App existed, never mutated after -- sharing it between Apps
	// is safe precisely because a Definition carries a Factory, not an
	// instance.
	snapshot catalog.Snapshot

	env      *config.Environment
	settings settings
	logger   log.Logger

	container *container.Container
	tasks     *taskRuntime

	executeMu sync.Mutex
	executed  bool

	// stopCh is closed exactly once, by requestStop. Every startup operation
	// polls it between steps and the run loop blocks on it, so caller
	// cancellation, a process signal, and a critical failure reach the same
	// single unwind path no matter when they arrive.
	stopOnce   sync.Once
	stopCh     chan struct{}
	stopReason string

	// unwindOnce makes stopping idempotent by construction rather than by
	// call-site discipline. Both the startup-failure path and the run-loop
	// path lead here, and a Closer such as a database pool cannot survive
	// being Stopped twice.
	unwindOnce sync.Once
	unwindErr  error

	// ready is closed immediately before the run loop starts blocking. Tests
	// set it to observe "the app is now fully started" without polling or
	// sleeping; production runs leave it nil.
	ready chan struct{}
}

// Option configures New. Options are created by this package so new internal
// settings can be added without expanding a public configuration struct.
type Option func(*appOptions) error

type appOptions struct {
	snapshot    catalog.Snapshot
	hasSnapshot bool
}

// WithDefinitions builds the App from an explicitly frozen Snapshot instead
// of the process-wide default catalog. It is useful for tests and for hosts
// that need an isolated set of plugin definitions.
func WithDefinitions(snapshot catalog.Snapshot) Option {
	return func(options *appOptions) error {
		options.snapshot = snapshot
		options.hasSnapshot = true
		return nil
	}
}

// New creates a single-use App. It freezes the selected definition catalog but
// performs no I/O and starts no plugin; those operations belong to Execute.
func New(options ...Option) (*App, error) {
	var resolved appOptions
	for _, option := range options {
		if err := option(&resolved); err != nil {
			return nil, err
		}
	}

	snapshot := resolved.snapshot
	if !resolved.hasSnapshot {
		frozen, err := catalog.Freeze()
		if err != nil {
			return nil, err
		}
		snapshot = frozen
	}

	return newApp(snapshot), nil
}

// newApp creates a single-use runtime from an already frozen definition
// snapshot. It performs no I/O and starts no plugin; those operations belong
// to Execute.
//
// It stays separate from New so white-box tests can assemble an App from a
// private catalog.New() snapshot without going through option resolution or
// touching the process-wide default catalog.
func newApp(snapshot catalog.Snapshot) *App {
	return &App{
		snapshot: snapshot,
		stopCh:   make(chan struct{}),
	}
}
