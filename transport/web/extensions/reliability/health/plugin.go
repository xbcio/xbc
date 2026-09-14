package health

import (
	"context"
	"errors"
	"fmt"

	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and runtime identity of the health plugin.
const Key plugin.Key = "health"

// Plugin aggregates the contributors injected by its Definition and exposes
// their probes through the Web route-contributor contract.
type Plugin struct {
	cfg          Config
	contributors []plugin.Entry[Contributor]
	runtimeDone  <-chan struct{}
}

var _ plugin.Initializer = (*Plugin)(nil)

const runtimeCheckName = "runtime"

var errRuntimeStopping = errors.New("health: application is shutting down")

var contributorInput = plugin.Collect[Contributor]()

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		return &Plugin{
			cfg:          cfg,
			contributors: append([]plugin.Entry[Contributor](nil), contributorInput.Get(ctx)...),
		}, nil
	},
	plugin.Options[*Plugin]{
		Inputs:  plugin.Inputs(contributorInput),
		Exports: routeContracts(),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable health aggregator with production-safe
// defaults and no contributors. Application assembly should normally use
// Definition so contributor identities are injected with the values.
func New() *Plugin {
	value, _ := newPlugin(DefaultConfig(), nil)
	return value
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config, contributors []plugin.Entry[Contributor]) (*Plugin, error) {
	prepared, err := prepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Plugin{
		cfg:          prepared,
		contributors: append([]plugin.Entry[Contributor](nil), contributors...),
	}, nil
}

// Init captures the runtime cancellation signal. A canceled execution context
// means shutdown has begun, so readiness must stop accepting new production
// traffic even while the Web listener drains existing requests.
func (p *Plugin) Init(ctx *plugin.Context) error {
	if p == nil {
		return errors.New("health: Init requires a non-nil plugin")
	}
	if ctx == nil {
		return errors.New("health: Init requires a non-nil plugin context")
	}
	p.runtimeDone = ctx.Done()
	return nil
}

// Definition returns health's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns health's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// Check asks each pre-bound Contributor for its checks and runs the requested
// probe. Contributor panics become down results so both direct callers and
// protocol adapters receive a health answer instead of a panic. Once runtime
// cancellation begins, readiness instead returns immediately without calling a
// contributor or a dependency checker: the response must not wait for a
// dependency that is itself draining.
func (p *Plugin) Check(ctx context.Context, kind Kind) Report {
	if kind == Readiness && p.shuttingDown() {
		return failedReport(Readiness, runtimeCheckName, errRuntimeStopping)
	}

	checkers := make([]NamedChecker, 0, len(p.contributors))
	for _, entry := range p.contributors {
		owner := entry.Identity.String()
		provided, panicErr := contributorChecks(entry.Value)
		if panicErr != nil {
			err := panicErr
			checkers = append(checkers, NamedChecker{
				Name:    owner,
				Kind:    kind,
				Checker: CheckFunc(func(context.Context) error { return err }),
			})
			continue
		}
		for _, candidate := range provided {
			candidate.Name = qualifyCheckName(owner, candidate.Name)
			checkers = append(checkers, candidate)
		}
	}
	return Check(ctx, kind, checkers, p.cfg.Timeout)
}

func (p *Plugin) shuttingDown() bool {
	if p == nil || p.runtimeDone == nil {
		return false
	}
	select {
	case <-p.runtimeDone:
		return true
	default:
		return false
	}
}

func contributorChecks(contributor Contributor) (checks []NamedChecker, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("health: contributor panicked while listing checks: %v", recovered)
			checks = nil
		}
	}()
	return contributor.HealthChecks(), nil
}

func qualifyCheckName(owner, local string) string {
	if local == "" || local == owner {
		return owner
	}
	return owner + "/" + local
}
