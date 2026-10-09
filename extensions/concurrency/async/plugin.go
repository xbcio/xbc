package async

import (
	"errors"

	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and runtime identity of the async pool.
const Key plugin.Key = "async"

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Pool, error) {
		return newPool(cfg, nil)
	},
	plugin.Options[*Pool]{
		Exports: plugin.Contracts(
			plugin.ExportAs[Spawner](func(pool *Pool) Spawner { return pool }),
		),
		Lifecycle: plugin.Lifecycle[*Pool]{
			Init:  initPool,
			Drain: (*Pool).drain,
			Stop:  (*Pool).stop,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns async's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns async's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// ErrDrainedBeforeInit is returned by Init when the Pool's admission has
// already been closed by drain or stop: such a Pool refuses to reopen, so
// Init reports the refusal instead of binding a Pool that every later Spawn
// would only reject.
var ErrDrainedBeforeInit = errors.New("async: cannot Init after Drain or Stop")

// initPool opens admission and installs the process-global Spawner. Admission
// opens here, at Init, rather than Start, so other plugins' Init and Start
// hooks -- and not only later request handlers -- can already Spawn. A Pool
// drained or stopped before its Init refuses to (re)open admission, and Init
// reports that instead of silently succeeding.
//
// Init also resolves the workload quota every executed task charges. It is the
// pool's own: a Pool declared inside a workload spends that workload's budget,
// and a Pool belonging to no workload charges nothing. The tasks it runs may
// have been spawned by callers in several workloads, but the pool is the thing
// running them, and the quota bounds a workload's concurrent work rather than
// who asked for it.
func initPool(pool *Pool, ctx *plugin.Context) error {
	if pool == nil {
		return errors.New("async: Init requires a non-nil pool")
	}
	if ctx == nil {
		return errors.New("async: Init requires a non-nil plugin context")
	}
	pool.log = ctx.Log()
	pool.admission = ctx.Admission()
	if !pool.open() {
		return ErrDrainedBeforeInit
	}
	bindGlobal(pool, ctx.Log())
	return nil
}
