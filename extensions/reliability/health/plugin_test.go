package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type contributorFunc func() []NamedChecker

func (f contributorFunc) HealthChecks() []NamedChecker { return f() }

type healthTestHost struct {
	execution context.Context
}

var _ plugin.RuntimeHost = healthTestHost{}

func (h healthTestHost) ExecutionContext() context.Context { return h.execution }
func (healthTestHost) Logger() corelog.Logger              { return corelog.Nop() }
func (healthTestHost) TrafficGate() <-chan struct{}        { return nil }
func (healthTestHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool {
	return false
}
func (healthTestHost) RequestShutdown(plugin.Identity, string) bool { return false }

func TestNewUsesSafeDefaults(t *testing.T) {
	p := New()
	require.NotNil(t, p)
	assert.Equal(t, DefaultConfig(), p.cfg)
	require.NoError(t, p.cfg.validate())
}

func TestDefinitionIsCanonical(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
}

func TestPluginAggregatesNamedContributorsFromTypedEntries(t *testing.T) {
	entries := []plugin.Entry[Contributor]{
		{
			Identity: plugin.Identity{Plugin: "redis", Instance: plugin.DefaultInstance},
			Value: contributorFunc(func() []NamedChecker {
				return []NamedChecker{{Kind: Readiness, Checker: CheckFunc(func(context.Context) error { return nil })}}
			}),
		},
		{
			Identity: plugin.Identity{Plugin: "gorm", Instance: "readonly"},
			Value: contributorFunc(func() []NamedChecker {
				return []NamedChecker{{Name: "replica", Kind: Readiness, Checker: CheckFunc(func(context.Context) error { return errors.New("offline") })}}
			}),
		},
	}
	p, err := newPlugin(DefaultConfig(), entries)
	require.NoError(t, err)

	report := p.Check(context.Background(), Readiness)
	assert.Equal(t, Down, report.Status)
	require.Len(t, report.Checks, 2)
	assert.Equal(t, "gorm[readonly]/replica", report.Checks[0].Name)
	assert.Equal(t, "redis", report.Checks[1].Name)
}

func TestPluginTurnsContributorPanicIntoDown(t *testing.T) {
	entries := []plugin.Entry[Contributor]{
		{
			Identity: plugin.Identity{Plugin: "bad", Instance: plugin.DefaultInstance},
			Value: contributorFunc(func() []NamedChecker {
				panic("list boom")
			}),
		},
	}
	p, err := newPlugin(DefaultConfig(), entries)
	require.NoError(t, err)

	report := p.Check(context.Background(), Readiness)
	assert.Equal(t, Down, report.Status)
	require.Len(t, report.Checks, 1)
	assert.Contains(t, report.Checks[0].Error.Error(), "list boom")
}

func TestReadinessTurnsDownImmediatelyWhenRuntimeShutdownBegins(t *testing.T) {
	execution, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	contributorCalls := make(chan struct{}, 2)
	p, err := newPlugin(DefaultConfig(), []plugin.Entry[Contributor]{{
		Identity: plugin.Identity{Plugin: "dependency", Instance: plugin.DefaultInstance},
		Value: contributorFunc(func() []NamedChecker {
			contributorCalls <- struct{}{}
			return []NamedChecker{{
				Kind: Readiness,
				Checker: CheckFunc(func(context.Context) error {
					return errors.New("this check must not run during shutdown")
				}),
			}}
		}),
	}})
	require.NoError(t, err)
	require.NoError(t, p.Init(plugin.NewRuntimeContext(healthTestHost{execution: execution}, plugin.Identity{Plugin: Key})))

	before := p.Check(context.Background(), Readiness)
	assert.Equal(t, Down, before.Status, "the normal readiness contributor is down before shutdown")
	require.Len(t, before.Checks, 1)
	assert.Equal(t, "dependency", before.Checks[0].Name)
	select {
	case <-contributorCalls:
	case <-time.After(time.Second):
		t.Fatal("the normal readiness probe did not call its contributor")
	}

	cancel()

	during := p.Check(context.Background(), Readiness)
	assert.Equal(t, Down, during.Status)
	require.Len(t, during.Checks, 1)
	assert.Equal(t, runtimeCheckName, during.Checks[0].Name)
	assert.ErrorIs(t, during.Checks[0].Error, errRuntimeStopping)
	select {
	case <-contributorCalls:
		t.Fatal("shutdown readiness must short-circuit before it calls contributors")
	default:
	}

	live := p.Check(context.Background(), Liveness)
	assert.Equal(t, Up, live.Status, "liveness stays up while the process is draining")
	assert.Empty(t, live.Checks, "the contributor offers only readiness")
}

func TestConfigValidationRejectsNonPositiveTimeout(t *testing.T) {
	require.NoError(t, Config{Timeout: time.Second}.validate())

	err := Config{Timeout: 0}.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}
