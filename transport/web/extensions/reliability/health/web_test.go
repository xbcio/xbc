package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

func TestRenderReportHidesErrorsUnlessExplicitlyAllowed(t *testing.T) {
	report := Report{Kind: Readiness, Status: Down, Checks: []Result{{
		Name: "database", Status: Down, Error: errors.New("dial tcp 10.0.0.2:3306"),
	}}}

	hidden, err := json.Marshal(renderReport(report, DetailNever))
	require.NoError(t, err)
	assert.NotContains(t, string(hidden), "10.0.0.2")
	assert.NotContains(t, string(hidden), `"error"`)

	exposed, err := json.Marshal(renderReport(report, DetailAlways))
	require.NoError(t, err)
	assert.Contains(t, string(exposed), "10.0.0.2")
	assert.Contains(t, string(exposed), `"error"`)
}

func TestWebProbesReturnStatusCodesStableJSONAndHideDetailsByDefault(t *testing.T) {
	entries := []plugin.Entry[Contributor]{
		{
			Identity: plugin.Identity{Plugin: "dependency", Instance: plugin.DefaultInstance},
			Value: contributorFunc(func() []NamedChecker {
				return []NamedChecker{
					{Name: "process", Kind: Liveness, Checker: CheckFunc(func(context.Context) error { return nil })},
					{Name: "z-cache", Kind: Readiness, Checker: CheckFunc(func(context.Context) error { return nil })},
					{Name: "a-database", Kind: Readiness, Checker: CheckFunc(func(context.Context) error {
						return errors.New("dial tcp 10.0.0.2:3306: connection refused")
					})},
				}
			}),
		},
	}
	p, err := newPlugin(DefaultConfig(), entries)
	require.NoError(t, err)

	liveCode, liveBody := invokeProbe(t, p, Liveness, "/healthz")
	assert.Equal(t, http.StatusOK, liveCode)
	assert.Equal(t, Up, liveBody.Status)
	assert.Equal(t, Liveness, liveBody.Probe)
	require.Len(t, liveBody.Checks, 1)
	assert.Equal(t, "dependency/process", liveBody.Checks[0].Name)

	readyCode, readyBody := invokeProbe(t, p, Readiness, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, readyCode)
	assert.Equal(t, Down, readyBody.Status)
	assert.Equal(t, Readiness, readyBody.Probe)
	require.Len(t, readyBody.Checks, 2)
	assert.Equal(t, "dependency/a-database", readyBody.Checks[0].Name)
	assert.Equal(t, "dependency/z-cache", readyBody.Checks[1].Name)
	assert.Empty(t, readyBody.Checks[0].Error, "error details must be absent by default")
}

func TestWebProbeHandlerHonorsConfiguredDetailPolicy(t *testing.T) {
	healthConfig := DefaultConfig()
	healthConfig.DetailPolicy = DetailAlways
	entries := []plugin.Entry[Contributor]{
		{
			Identity: plugin.Identity{Plugin: "dependency", Instance: plugin.DefaultInstance},
			Value: contributorFunc(func() []NamedChecker {
				return []NamedChecker{{Kind: Readiness, Checker: CheckFunc(func(context.Context) error {
					return errors.New("explicit diagnostic")
				})}}
			}),
		},
	}
	p, err := newPlugin(healthConfig, entries)
	require.NoError(t, err)

	code, body := invokeProbe(t, p, Readiness, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	require.Len(t, body.Checks, 1)
	assert.Contains(t, body.Checks[0].Error, "explicit diagnostic")
}

func invokeProbe(t *testing.T, p *Plugin, kind Kind, target string) (int, webReport) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(recorder)
	gc.Request = httptest.NewRequest(http.MethodGet, target, nil)
	p.probeHandler(kind)(gc)

	var body webReport
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&body))
	return recorder.Code, body
}
