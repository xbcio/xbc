package runtime

import (
	"fmt"
	"reflect"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin/assembly"
)

// bootstrap loads process-independent configuration and runtime settings, and
// installs the process-level resource knobs the configuration asks for. It
// creates no Plugin-owned resources and invokes no Definition factory.
//
// The configuration Universe is built first, from the very Bundles this App
// was composed from. That ordering is what lets environment variables enter
// the merged view at all: the loader can only resolve XBC_PLUGINS_X_Y once it
// knows which sections exist and what shape they have. It also keeps the rule
// that configuration never activates uncompiled code -- a variable naming a
// Definition the composition root did not select has no section to land in.
func (a *App) bootstrap(cmd command) error {
	universe, err := a.configUniverse()
	if err != nil {
		return err
	}
	env, err := config.Load(config.Options{
		File:      cmd.config,
		Profile:   cmd.profile,
		EnvPrefix: config.DefaultEnvPrefix,
		Universe:  universe,
	})
	if err != nil {
		return err
	}
	logging, err := bindLogging(env)
	if err != nil {
		return err
	}
	// doctor validates the log section like every other section but must not
	// install a logger: initialising one mutates process-global state, opens
	// files and starts a flusher, none of which a read-only command may do.
	if cmd.subcommand == doctorSubcommand {
		a.logger = log.Nop()
	} else {
		if err := log.Init(logging); err != nil {
			return fmt.Errorf("xbc: failed to initialize logging: %w", err)
		}
		a.logger = log.L()
	}
	settings, err := loadSettings(env)
	if err != nil {
		return err
	}
	// The process-level knobs are resolved for every command, so an
	// unsatisfiable xbc.runtime section fails the same way whether the process
	// was about to start or was only being diagnosed, but they are installed
	// only for a command that goes on to run an application: doctor exists to
	// report a process configuration, and a diagnostic that changes the
	// GOMAXPROCS and memory limits of the process it is diagnosing is not one.
	knobs, err := resolveRuntimeKnobs(settings.Runtime, cgroupRoot)
	if err != nil {
		return err
	}
	if cmd.subcommand != doctorSubcommand {
		applyRuntimeKnobs(knobs)
	}
	a.logger.Info("xbc: runtime knobs " + knobs.describe())
	// The task budgets are configuration and are read here, where the
	// configuration is; the identity-to-workload attribution they are charged
	// against is a property of the frozen graph and cannot exist yet. The
	// closure below resolves it per submission instead, which is why the two
	// halves of one budget are assembled from two different moments.
	limits, err := workloadTaskLimits(a.bundles, env)
	if err != nil {
		return err
	}
	a.env = env
	a.settings = settings
	a.tasks = newTaskRuntime(a.logger, a.onCritical)
	a.tasks.configureWorkloadBudget(limits, a.workloadOf)
	return nil
}

// configUniverse declares every top-level configuration owner: the framework's
// own roots plus one section per selected Definition and per declared workload.
func (a *App) configUniverse() (*config.Universe, error) {
	sections := []config.Section{
		{
			Path:   settingsSection,
			Owner:  "the xbc runtime",
			Kind:   config.SectionTyped,
			Schema: reflect.TypeOf(settings{}),
		},
		{
			Path:   loggingSection,
			Owner:  "the log package",
			Kind:   config.SectionTyped,
			Schema: reflect.TypeOf(log.Config{}),
		},
		{
			Path:  applicationSection,
			Owner: "the application author",
			Kind:  config.SectionFreeform,
		},
	}
	// The plugin and workload sections both come from assembly, which owns both
	// roots. It emits the "workloads" namespace together with one typed child
	// per declared workload, so a workload's "enabled" and its own settings bind
	// through the same strict path a plugin section does -- and a key under
	// "workloads" that no declared workload answers for is reported as unowned
	// rather than silently ignored.
	pluginSections, err := assembly.ConfigSections(a.bundles)
	if err != nil {
		return nil, err
	}
	return config.NewUniverse(append(sections, pluginSections...)...)
}

func bindLogging(env *config.Environment) (log.Config, error) {
	cfg := log.DefaultConfig()
	if err := env.Bind(loggingSection, &cfg); err != nil {
		return cfg, fmt.Errorf("xbc: failed to bind log configuration: %w", err)
	}
	return cfg, nil
}
