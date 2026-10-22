package automemlimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is this plugin's stable configuration and dependency identity. It owns
// the plugins.automemlimit configuration section.
const Key plugin.Key = "automemlimit"

const (
	envGOMEMLIMIT   = "GOMEMLIMIT"
	envAUTOMEMLIMIT = "AUTOMEMLIMIT"
)

// Plugin installs the process soft memory limit from the container's cgroup
// limit during Init and, when configured to, refreshes it from one managed
// task. It owns no resource that outlives the process: the refresh task's
// context is cancelled by the runtime, and the limit itself is deliberately
// left in place, so the plugin has no Stop or Drain stage.
type Plugin struct {
	config   normalizedConfig
	provider memlimit.Provider
}

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		normalized, err := normalizeConfig(cfg)
		if err != nil {
			return nil, err
		}
		return newPlugin(normalized), nil
	},
	plugin.Options[*Plugin]{
		// Zero Activation: selecting the Bundle is the explicit decision, and
		// the universal plugins.automemlimit.enabled=false switch still vetoes
		// it. The factory reads no cgroup and changes no process state, because
		// the composition guard assembles every Bundle with an empty
		// configuration.
		Lifecycle: plugin.Lifecycle[*Plugin]{
			Init:  (*Plugin).init,
			Start: (*Plugin).start,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns automemlimit's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns automemlimit's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

func newPlugin(cfg normalizedConfig) *Plugin {
	return &Plugin{config: cfg, provider: newProvider(cfg)}
}

// init decides whether this process already has a memory-limit owner and, when
// it does not, installs the container-derived limit exactly once.
func (p *Plugin) init(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("automemlimit: Init requires a plugin context")
	}
	logger := ctx.Log()

	// The environment decides before the conflict check does: a GOMEMLIMIT
	// already in the environment made the runtime install a limit, and
	// treating that as a competing configuration owner would fail a valid
	// deployment.
	if _, ok := os.LookupEnv(envGOMEMLIMIT); ok {
		logger.Info("automemlimit: GOMEMLIMIT is set in the environment, leaving the process limit alone",
			"reason", "environment")
		return nil
	}
	if value, ok := os.LookupEnv(envAUTOMEMLIMIT); ok && value == "off" {
		logger.Info("automemlimit: AUTOMEMLIMIT is off, leaving the process limit alone",
			"reason", "automemlimit-off")
		return nil
	}

	current, err := currentSoftLimit()
	if err != nil {
		return err
	}
	if current != math.MaxInt64 {
		return fmt.Errorf(
			"automemlimit: a soft memory limit of %d bytes is already installed by this process "+
				"(the framework installs one from xbc.runtime.memory_limit, or the application set it itself); "+
				"set xbc.runtime.memory_limit to 0 or remove plugins.automemlimit, because two owners must not compete",
			current)
	}

	return p.install(logger)
}

// start submits the optional refresh task. It submits nothing when the
// configured interval is zero, and a refused submission fails startup rather
// than silently leaving the process without the refresh the configuration
// asked for.
func (p *Plugin) start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("automemlimit: Start requires a plugin context")
	}
	if p.config.refreshInterval <= 0 {
		return nil
	}
	logger := ctx.Log()
	accepted := ctx.Go(func(taskCtx context.Context) {
		p.refresh(taskCtx, logger)
	})
	if !accepted {
		return errors.New("automemlimit: managed task submission was refused during Start")
	}
	return nil
}

// refresh re-runs only the installation step. The conflict check is
// deliberately absent here: after Init succeeded, a non-default limit is this
// plugin's own, and re-checking it would report the plugin as its own
// competitor.
func (p *Plugin) refresh(ctx context.Context, logger log.Logger) {
	ticker := time.NewTicker(p.config.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.install(logger); err != nil {
				logger.Warn("automemlimit: refresh failed, the next tick retries", "error", err)
			}
		}
	}
}

// install calls the library with the validated configuration and classifies
// the outcome. The library re-reads the environment on every call, so the
// refresh loop inherits exactly the priority the one-time installation has.
func (p *Plugin) install(logger log.Logger) error {
	limit, err := memlimit.Set(
		memlimit.WithRatio(p.config.ratio),
		memlimit.WithMin(p.config.minBytes),
		memlimit.WithProvider(p.provider),
	)
	switch {
	case err == nil && limit == math.MaxInt64:
		// The library handles ErrNoLimit by installing math.MaxInt64. The
		// conflict check already proved the process was unlimited, so this is
		// the unchanged state rather than a decision of this plugin.
		logger.Info("automemlimit: no finite memory limit", "reason", "no-limit")
		return nil
	case err == nil:
		logger.Info("automemlimit: GOMEMLIMIT set",
			"limit_bytes", limit,
			"ratio", p.config.ratio,
			"min_bytes", p.config.minBytes,
			"fallback", p.config.fallback)
		return nil
	case errors.Is(err, memlimit.ErrNoLimit):
		logger.Info("automemlimit: no finite memory limit", "reason", "no-limit")
		return nil
	case errors.Is(err, memlimit.ErrCgroupsNotSupported), errors.Is(err, memlimit.ErrNoCgroup):
		logger.Warn("automemlimit: no cgroup memory limit available, leaving the process limit alone",
			"reason", "no-cgroup")
		return nil
	default:
		return fmt.Errorf("automemlimit: install GOMEMLIMIT: %w", err)
	}
}
