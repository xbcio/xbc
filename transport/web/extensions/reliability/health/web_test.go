package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
	transportweb "github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func TestRenderReportHidesErrorsUnlessExplicitlyAllowed(t *testing.T) {
	report := corehealth.Report{Kind: corehealth.Readiness, Status: corehealth.Down, Checks: []corehealth.Result{{
		Name: "database", Status: corehealth.Down, Error: errors.New("dial tcp 10.0.0.2:3306"),
	}}}

	hidden, err := json.Marshal(renderReport(report, corehealth.DetailNever))
	require.NoError(t, err)
	assert.NotContains(t, string(hidden), "10.0.0.2")
	assert.NotContains(t, string(hidden), `"error"`)

	exposed, err := json.Marshal(renderReport(report, corehealth.DetailAlways))
	require.NoError(t, err)
	assert.Contains(t, string(exposed), "10.0.0.2")
	assert.Contains(t, string(exposed), `"error"`)
}

func TestWebProbesReturnStatusCodesAndPreserveAggregatorOrdering(t *testing.T) {
	aggregator := fakeProber(func(_ context.Context, kind corehealth.Kind) corehealth.Report {
		if kind == corehealth.Liveness {
			return corehealth.Report{Kind: kind, Status: corehealth.Up, Checks: []corehealth.Result{{
				Name: "dependency/process", Kind: kind, Status: corehealth.Up, Latency: 1500 * time.Nanosecond,
			}}}
		}
		return corehealth.Report{Kind: kind, Status: corehealth.Down, Checks: []corehealth.Result{
			{Name: "dependency/a-database", Kind: kind, Status: corehealth.Down, Error: errors.New("dial tcp 10.0.0.2:3306: connection refused")},
			{Name: "dependency/z-cache", Kind: kind, Status: corehealth.Up},
		}}
	})
	p, err := newPlugin(DefaultConfig(), aggregator)
	require.NoError(t, err)

	liveCode, liveBody := invokeProbe(t, p, corehealth.Liveness, "/healthz")
	assert.Equal(t, http.StatusOK, liveCode)
	assert.Equal(t, corehealth.Up, liveBody.Status)
	assert.Equal(t, corehealth.Liveness, liveBody.Probe)
	require.Len(t, liveBody.Checks, 1)
	assert.Equal(t, "dependency/process", liveBody.Checks[0].Name)
	assert.Equal(t, "2µs", liveBody.Checks[0].Latency, "latency is rounded, never raw nanoseconds")

	readyCode, readyBody := invokeProbe(t, p, corehealth.Readiness, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, readyCode)
	assert.Equal(t, corehealth.Down, readyBody.Status)
	assert.Equal(t, corehealth.Readiness, readyBody.Probe)
	require.Len(t, readyBody.Checks, 2)
	assert.Equal(t, "dependency/a-database", readyBody.Checks[0].Name)
	assert.Equal(t, "dependency/z-cache", readyBody.Checks[1].Name)
	assert.Empty(t, readyBody.Checks[0].Error, "error details must be absent by default")
}

func TestWebProbeHandlerHonorsConfiguredDetailPolicy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DetailPolicy = corehealth.DetailAlways
	aggregator := fakeProber(func(_ context.Context, kind corehealth.Kind) corehealth.Report {
		return corehealth.Report{Kind: kind, Status: corehealth.Down, Checks: []corehealth.Result{{
			Name: "dependency", Kind: kind, Status: corehealth.Down, Error: errors.New("explicit diagnostic"),
		}}}
	})
	p, err := newPlugin(cfg, aggregator)
	require.NoError(t, err)

	code, body := invokeProbe(t, p, corehealth.Readiness, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	require.Len(t, body.Checks, 1)
	assert.Contains(t, body.Checks[0].Error, "explicit diagnostic")
}

func invokeProbe(t *testing.T, p *Plugin, kind corehealth.Kind, target string) (int, webReport) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	transportweb.Handle(p.probeHandler(kind))(c)

	var body webReport
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&body))
	return recorder.Code, body
}
