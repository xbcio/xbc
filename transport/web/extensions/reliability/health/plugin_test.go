package health

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

type fakeProber func(context.Context, corehealth.Kind) corehealth.Report

func (f fakeProber) Check(ctx context.Context, kind corehealth.Kind) corehealth.Report {
	return f(ctx, kind)
}

func upProber() fakeProber {
	return func(_ context.Context, kind corehealth.Kind) corehealth.Report {
		return corehealth.Report{Kind: kind, Status: corehealth.Up}
	}
}

func TestDefinitionIsCanonical(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
}

// Bundle composition is asserted end to end by
// examples.TestPublicAPIReadinessIsObservableDuringWebPreDrain, which composes
// only this package's Bundle and still gets a working probe: proberInput is a
// RefTo, so a composition missing the neutral capability fails to plan.

func TestNewPluginRequiresAnAggregator(t *testing.T) {
	_, err := newPlugin(DefaultConfig(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aggregator")
}

func TestNewPluginRejectsInvalidConfiguration(t *testing.T) {
	invalid := DefaultConfig()
	invalid.LivenessPath = "healthz"
	_, err := newPlugin(invalid, upProber())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "liveness_path")
}

func TestConfigValidation(t *testing.T) {
	require.NoError(t, DefaultConfig().validate())

	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{name: "liveness path", edit: func(c *Config) { c.LivenessPath = "live" }, want: "liveness_path"},
		{name: "query", edit: func(c *Config) { c.ReadinessPath = "/ready?full=1" }, want: "readiness_path"},
		{name: "duplicate", edit: func(c *Config) { c.ReadinessPath = c.LivenessPath }, want: "must differ"},
		{name: "detail policy", edit: func(c *Config) { c.DetailPolicy = "debug" }, want: "detail_policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := DefaultConfig()
			tt.edit(&candidate)
			err := candidate.validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
