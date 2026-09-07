package runtime

import (
	"fmt"
	"reflect"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin/assembly"
)

// bootstrap loads process-independent configuration and runtime settings.
// It creates no Plugin-owned resources and invokes no Definition factory.
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
	a.env = env
	a.settings = settings
	a.tasks = newTaskRuntime(a.logger, a.onCritical)
	return nil
}

// configUniverse declares every top-level configuration owner: the framework's
// own roots plus one section per selected Definition.
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
