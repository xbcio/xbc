package runtime

import (
	"fmt"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/internal/cli"
	"github.com/xbcio/xbc/log"
)

// bootstrap loads process-independent configuration and runtime settings.
// It creates no Plugin-owned resources and invokes no Definition factory.
func (a *App) bootstrap(cmd cli.Command) error {
	env, err := config.Load(config.Options{
		File:      cmd.Config,
		Profile:   cmd.Profile,
		EnvPrefix: config.DefaultEnvPrefix,
	})
	if err != nil {
		return err
	}
	if err := initLogging(env); err != nil {
		return err
	}
	settings, err := loadSettings(env)
	if err != nil {
		return err
	}
	a.env = env
	a.settings = settings
	a.logger = log.L()
	a.tasks = newTaskRuntime(a.logger, a.onCritical)
	return nil
}

func initLogging(env *config.Environment) error {
	cfg := log.DefaultConfig()
	if err := env.Bind("log", &cfg); err != nil {
		return fmt.Errorf("xbc: failed to bind log configuration: %w", err)
	}
	if err := log.Init(cfg); err != nil {
		return fmt.Errorf("xbc: failed to initialize logging: %w", err)
	}
	return nil
}
