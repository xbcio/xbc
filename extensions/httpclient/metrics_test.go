package httpclient

import (
	"errors"
	"net/http"
	"sort"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/observability/metrics"
)

// gatheredByName indexes a registry's families the way a scrape would.
func gatheredByName(t *testing.T, registerer *fakeRegisterer) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := registerer.inner.Gather()
	require.NoError(t, err)
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}
	return byName
}

func familyNames(byName map[string]*dto.MetricFamily) []string {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// labelNamesOf collects every distinct label name a family's samples carry.
func labelNamesOf(family *dto.MetricFamily) map[string]bool {
	names := map[string]bool{}
	for _, metric := range family.GetMetric() {
		for _, label := range metric.GetLabel() {
			names[label.GetName()] = true
		}
	}
	return names
}

func labelValue(metric *dto.Metric, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}

// newSyntheticClientWithRegistry builds the synthetic client with the metric
// collectors a real construction would register.
func newSyntheticClientWithRegistry(t *testing.T, config Config, transport http.RoundTripper, registerer metrics.Registerer) *Client {
	t.Helper()
	normalized := mustNormalize(t, config)
	collectors, err := newClientMetrics("test", registerer)
	if err != nil {
		t.Fatalf("newClientMetrics error = %v", err)
	}
	client := &Client{instance: "test", config: normalized}
	engine := &retryTransport{config: normalized, next: transport, metrics: collectors, stopped: &client.stopped}
	client.client = &http.Client{Transport: engine, Timeout: normalized.TotalTimeout}
	t.Cleanup(func() { _ = client.stop(t.Context()) })
	return client
}

func TestMetricsPublishThreeFamiliesWithTheDocumentedLabels(t *testing.T) {
	registerer := newFakeRegisterer()
	server := newScriptedServer(t, http.StatusServiceUnavailable, http.StatusOK)
	client := newTestClient(t, retryConfig(2), nil, registerer)

	response, err := client.Client().Get(server.URL)
	require.NoError(t, err)
	response.Body.Close()

	assert.Equal(t, 1, registerer.batchCount(), "the three collectors are registered in one atomic call")
	assert.Equal(t, 3, registerer.lastBatchSize())

	byName := gatheredByName(t, registerer)

	requests := byName["http_client_requests_total"]
	require.NotNil(t, requests, "requests family missing; got %v", familyNames(byName))
	require.Len(t, requests.GetMetric(), 2, "one sample per attempt outcome")
	assert.Equal(t, map[string]bool{"client": true, "method": true, "code": true},
		labelNamesOf(requests), "requests labels are exactly client, method, code")
	codes := map[string]float64{}
	for _, metric := range requests.GetMetric() {
		assert.Equal(t, "test", labelValue(metric, "client"))
		assert.Equal(t, "GET", labelValue(metric, "method"))
		codes[labelValue(metric, "code")] = metric.GetCounter().GetValue()
	}
	assert.Equal(t, map[string]float64{"503": 1, "200": 1}, codes)

	durations := byName["http_client_request_duration_seconds"]
	require.NotNil(t, durations, "duration family missing; got %v", familyNames(byName))
	assert.Equal(t, map[string]bool{"client": true, "method": true},
		labelNamesOf(durations), "duration labels are client and method")
	assert.Len(t, durations.GetMetric()[0].GetHistogram().GetBucket(), 11,
		"the histogram carries its documented buckets")

	retries := byName["http_client_retries_total"]
	require.NotNil(t, retries, "retries family missing; got %v", familyNames(byName))
	require.Len(t, retries.GetMetric(), 1)
	assert.Equal(t, map[string]bool{"client": true, "reason": true}, labelNamesOf(retries))
	assert.Equal(t, "status", labelValue(retries.GetMetric()[0], "reason"))
	assert.Equal(t, float64(1), retries.GetMetric()[0].GetCounter().GetValue())

	for name, family := range byName {
		for label := range labelNamesOf(family) {
			assert.NotContains(t, []string{"path", "url", "host", "query", "target"}, label,
				"family %s must not label requests by their destination", name)
		}
	}
}

func TestFailedAttemptsCountWithTheErrorCode(t *testing.T) {
	registerer := newFakeRegisterer()
	transport := newSyntheticTransport(syntheticOutcome{err: errors.New("connection refused")})
	client := newSyntheticClientWithRegistry(t, Config{}, transport, registerer)

	response, err := client.Client().Get("http://downstream.test/resource")
	require.Error(t, err)
	assert.Nil(t, response)

	requests := gatheredByName(t, registerer)["http_client_requests_total"]
	require.NotNil(t, requests)
	require.Len(t, requests.GetMetric(), 1)
	assert.Equal(t, "error", labelValue(requests.GetMetric()[0], "code"),
		"an attempt with no status line counts under the error code")
}

func TestNoMetricsSelectionRegistersNothing(t *testing.T) {
	collectors, err := newClientMetrics("test", nil)
	require.NoError(t, err)
	assert.Nil(t, collectors, "without the metrics plugin there is nothing to register")

	// And a client built that way runs requests with no metrics at all.
	server := newScriptedServer(t, http.StatusOK)
	client := newTestClient(t, Config{}, nil, nil)
	response, err := client.Client().Get(server.URL)
	require.NoError(t, err)
	response.Body.Close()
}

func TestSeveralInstancesShareOneRegistryUnderDistinctClientLabels(t *testing.T) {
	registerer := newFakeRegisterer()
	first, err := newClient("gateway", mustNormalize(t, Config{}), nil, registerer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.stop(t.Context()) })
	second, err := newClient("ledger", mustNormalize(t, Config{}), nil, registerer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.stop(t.Context()) })

	first.client.Transport.(*retryTransport).metrics.observe("GET", "200", 0)
	second.client.Transport.(*retryTransport).metrics.observe("GET", "200", 0)

	requests := gatheredByName(t, registerer)["http_client_requests_total"]
	require.NotNil(t, requests)
	require.Len(t, requests.GetMetric(), 2, "two instances, one family, two series")
	clients := map[string]bool{}
	for _, metric := range requests.GetMetric() {
		clients[labelValue(metric, "client")] = true
	}
	assert.Equal(t, map[string]bool{"gateway": true, "ledger": true}, clients)
}

func TestRegistrationFailureFailsConstruction(t *testing.T) {
	registerer := newFakeRegisterer()
	first, err := newClientMetrics("gateway", registerer)
	require.NoError(t, err)
	require.NotNil(t, first)
	// A second registration under the same instance name duplicates the
	// family with identical constant labels, which the registry rejects;
	// construction must fail with it, not at scrape time.
	_, err = newClient("gateway", mustNormalize(t, Config{}), nil, registerer)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http-client: register metrics")
}
