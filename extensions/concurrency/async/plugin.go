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
		return newPool(cfg, nil), nil
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

// initPool opens admission and installs the process-global Spawner. Admission
// opens here, at Init, rather than Start, so other plugins' Init and Start
// hooks -- and not only later request handlers -- can already Spawn.
func initPool(pool *Pool, ctx *plugin.Context) error {
	if pool == nil {
		return errors.New("async: Init requires a non-nil pool")
	}
	if ctx == nil {
		return errors.New("async: Init requires a non-nil plugin context")
	}
	pool.log = ctx.Log()
	pool.open()
	bindGlobal(pool, ctx.Log())
	return nil
}
