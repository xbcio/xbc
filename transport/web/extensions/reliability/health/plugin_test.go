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

func TestConfigValidation(t *testing.T) {
	valid := Config{Timeout: time.Second, LivenessPath: "/live", ReadinessPath: "/ready", DetailPolicy: DetailNever}
	require.NoError(t, valid.validate())

	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{name: "timeout", edit: func(c *Config) { c.Timeout = 0 }, want: "timeout"},
		{name: "liveness path", edit: func(c *Config) { c.LivenessPath = "live" }, want: "liveness_path"},
		{name: "query", edit: func(c *Config) { c.ReadinessPath = "/ready?full=1" }, want: "readiness_path"},
		{name: "duplicate", edit: func(c *Config) { c.ReadinessPath = c.LivenessPath }, want: "must differ"},
		{name: "detail policy", edit: func(c *Config) { c.DetailPolicy = "debug" }, want: "detail_policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := valid
			tt.edit(&candidate)
			err := candidate.validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
