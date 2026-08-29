package runtime

import (
	"fmt"

	"github.com/xbcio/xbc/assembly"
	"github.com/xbcio/xbc/cli"
	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
)

// bootstrap runs everything that must exist before the first plugin does:
// merged configuration, a configured logger, the framework's own settings,
// the managed task group, and the assembly container.
//
// The task group is built here rather than just before the Runner stage
// because a plugin's Init already receives a live Context and may call
// ctx.Go from inside Init itself. Anything that exists only from the Runner
// stage onward would be missing at exactly the moment the first plugin
// reaches for it.
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
	a.container = assembly.New(assembly.Options{
		Snapshot: a.snapshot,
		Env:      env,
		Host:     hostAdapter{app: a},
		Logger:   a.logger,
	})
	return nil
}

// initLogging binds the "log" section and brings the global logger up.
//
// Bind must run before log.Init, not after. Bind is the only step that
// applies the file/ENV/default chain; log.Init merely assembles zap cores
// from whatever Config it is handed and has no idea a "log" config section,
// environment variables or defaults exist. Calling Init first would silently
// discard every override Bind was supposed to apply.
func initLogging(env *config.Environment) error {
	cfg := log.DefaultConfig()
	if err := env.Bind("log", &cfg); err != nil {
		return fmt.Errorf("xbc: 绑定 log 配置失败：%w", err)
	}
	if err := log.Init(cfg); err != nil {
		return fmt.Errorf("xbc: 初始化日志失败：%w", err)
	}
	return nil
}
