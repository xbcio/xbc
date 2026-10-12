package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryConfig is the base for tests that only care about attempt counts: two
// extra attempts with waits short enough to keep the suite fast.
func retryConfig(attempts int) Config {
	return Config{Retry: RetryConfig{
		MaxAttempts:    attempts,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}}
}

func TestPostIsNotRetriedOn503(t *testing.T) {
	server := newScriptedServer(t, http.StatusServiceUnavailable)
	client := newTestClient(t, retryConfig(3), nil, nil)

	request := mustRequest(t, http.MethodPost, server.URL, strings.NewReader("payload"))
	response, err := client.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(t, 1, server.count(), "a POST is never retried without the Idempotent mark")
}

func TestGetRetriesServiceUnavailableUntilMaxAttempts(t *testing.T) {
	server := newScriptedServer(t, http.StatusServiceUnavailable)
	client := newTestClient(t, retryConfig(3), nil, nil)

	response, err := client.Client().Get(server.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(t, 3, server.count(), "the last attempt's response is what the caller sees")
}

func TestGatewayTimeoutsRetryButClientErrorsAndServerErrorsDoNot(t *testing.T) {
	for _, tc := range []struct {
		status      int
		wantRetries bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, true},
		{http.StatusGatewayTimeout, true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := newScriptedServer(t, tc.status)
			client := newTestClient(t, retryConfig(3), nil, nil)
			response, err := client.Client().Get(server.URL)
			require.NoError(t, err)
			defer response.Body.Close()
			assert.Equal(t, tc.status, response.StatusCode)
			want := 1
			if tc.wantRetries {
				want = 3
			}
			assert.Equal(t, want, server.count())
		})
	}
}

func TestTooManyRequestsWithRetryAfterHonoursTheCap(t *testing.T) {
	server := newScriptedServer(t, http.StatusTooManyRequests, http.StatusOK)
	server.setResponseHeader(0, "Retry-After", "2")
	config := retryConfig(2)
	config.Retry.MaxRetryAfter = 200 * time.Millisecond
	client := newTestClient(t, config, nil, nil)

	started := time.Now()
	response, err := client.Client().Get(server.URL)
	elapsed := time.Since(started)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, 2, server.count())
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond,
		"the capped Retry-After wait is honoured")
	assert.Less(t, elapsed, 1500*time.Millisecond,
		"the header asked for two seconds; max_retry_after caps the wait")
}

func TestTooManyRequestsWithoutRetryAfterFallsBackToBackoff(t *testing.T) {
	server := newScriptedServer(t, http.StatusTooManyRequests, http.StatusOK)
	client := newTestClient(t, retryConfig(2), nil, nil)

	response, err := client.Client().Get(server.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, 2, server.count(), "429 is retryable with or without the header")
}

func TestIdempotentContextMarkLetsPostRetry(t *testing.T) {
	server := newScriptedServer(t, http.StatusServiceUnavailable, http.StatusOK)
	client := newTestClient(t, retryConfig(2), nil, nil)

	request := mustRequest(t, http.MethodPost, server.URL, strings.NewReader("payload"))
	request = request.WithContext(Idempotent(request.Context()))
	response, err := client.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, 2, server.count())
	assert.Equal(t, "payload", server.body(0))
	assert.Equal(t, "payload", server.body(1), "the retried attempt carries the whole body again")
}

// TestUnreplayableBodyIsNotRetried pins the GetBody gate: a GET whose body
// is a one-shot stream is idempotent by method but cannot be produced again,
// so it is sent exactly once.
func TestUnreplayableBodyIsNotRetried(t *testing.T) {
	server := newScriptedServer(t, http.StatusServiceUnavailable)
	client := newTestClient(t, retryConfig(3), nil, nil)

	body := io.LimitReader(strings.NewReader("payload"), int64(len("payload")))
	request := mustRequest(t, http.MethodGet, server.URL, body)
	require.Nil(t, request.GetBody, "the test needs a body with no replay function")

	response, err := client.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(t, 1, server.count())
}

func TestConnectionErrorIsRetried(t *testing.T) {
	transport := newSyntheticTransport(
		syntheticOutcome{err: errors.New("connection refused")},
		statusOutcome(http.StatusOK),
	)
	client := newSyntheticClient(t, retryConfig(2), transport)

	response, err := client.Client().Get("http://downstream.test/resource")
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, 2, transport.attemptCount())
}

func TestConnectionErrorKeepsFailingWithoutIdempotentMarkForPost(t *testing.T) {
	transport := newSyntheticTransport(syntheticOutcome{err: errors.New("connection refused")})
	client := newSyntheticClient(t, retryConfig(3), transport)

	_, err := client.Client().Post("http://downstream.test/resource", "application/json", strings.NewReader("{}"))
	require.Error(t, err)
	assert.Equal(t, 1, transport.attemptCount())
}

// TestTotalBudgetStopsNewAttempts is the deadline behavior: once the
// remaining budget cannot cover the wait and another attempt, the last
// response is returned intact instead of the call being cut mid-retry.
func TestTotalBudgetStopsNewAttempts(t *testing.T) {
	server := newScriptedServer(t, http.StatusServiceUnavailable)
	config := Config{
		TotalTimeout: 300 * time.Millisecond,
		Retry: RetryConfig{
			MaxAttempts:    7,
			InitialBackoff: 200 * time.Millisecond,
			MaxBackoff:     200 * time.Millisecond,
		},
	}
	client := newTestClient(t, config, nil, nil)

	started := time.Now()
	response, err := client.Client().Get(server.URL)
	elapsed := time.Since(started)
	require.NoError(t, err, "an exhausted budget returns the last response, not an error")
	defer response.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, "Service Unavailable", string(body), "the last response body is intact")

	assert.Less(t, server.count(), 7, "the attempt budget is not exhausted once the deadline binds")
	assert.LessOrEqual(t, server.count(), 3, "jittered waits of at least 100ms fit at most three attempts into 300ms")
	assert.LessOrEqual(t, elapsed, 550*time.Millisecond, "total time stays within total_timeout plus tolerance")
}

// TestCallerDeadlineBoundsTheWholeCall pins that the caller's context wins
// over the configured budget: with an earlier caller deadline, the first
// response comes back instead of the engine waiting past it.
func TestCallerDeadlineBoundsTheWholeCall(t *testing.T) {
	server := newScriptedServer(t, http.StatusServiceUnavailable)
	config := Config{
		TotalTimeout: 10 * time.Second,
		Retry: RetryConfig{
			MaxAttempts:    5,
			InitialBackoff: 500 * time.Millisecond,
			MaxBackoff:     500 * time.Millisecond,
		},
	}
	client := newTestClient(t, config, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	request := mustRequest(t, http.MethodGet, server.URL, nil).WithContext(ctx)
	started := time.Now()
	response, err := client.Client().Do(request)
	elapsed := time.Since(started)
	require.NoError(t, err, "an early caller deadline returns the response, not an error")
	defer response.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(t, 1, server.count(), "the jittered wait of at least 250ms cannot fit the remaining 150ms")
	assert.Less(t, elapsed, 300*time.Millisecond, "the caller's deadline is not extended by a retry wait")
}

// TestBodiesAreClosedBeforeEachRetry pins the connection hygiene: the
// previous attempt's body is drained and closed before the wait, so a retry
// never leaks the connection it is abandoning.
func TestBodiesAreClosedBeforeEachRetry(t *testing.T) {
	transport := newSyntheticTransport(
		statusOutcome(http.StatusServiceUnavailable),
		statusOutcome(http.StatusServiceUnavailable),
		statusOutcome(http.StatusOK),
	)
	client := newSyntheticClient(t, retryConfig(3), transport)

	response, err := client.Client().Get("http://downstream.test/resource")
	require.NoError(t, err)
	assert.Equal(t, 3, transport.attemptCount())
	assert.True(t, transport.body(0).closed.Load(), "the first body is closed by the time the retry runs")
	assert.True(t, transport.body(1).closed.Load(), "the second body is closed by the time the retry runs")
	assert.Greater(t, transport.body(0).reads.Load(), int64(0), "the discarded body was drained for reuse")
	response.Body.Close()
	assert.True(t, transport.body(2).closed.Load(), "the returned body is the caller's to close")
}

func TestStoppedClientRefusesDuringRetryWait(t *testing.T) {
	transport := newSyntheticTransport(statusOutcome(http.StatusServiceUnavailable))
	config := retryConfig(3)
	config.Retry.InitialBackoff = 50 * time.Millisecond
	config.Retry.MaxBackoff = 50 * time.Millisecond
	client := newSyntheticClient(t, config, transport)

	stopped := make(chan error, 1)
	go func() {
		_, err := client.Client().Get("http://downstream.test/resource")
		stopped <- err
	}()
	// Let the first attempt land, then stop while the engine is waiting.
	require.Eventually(t, func() bool { return transport.attemptCount() >= 1 }, time.Second, time.Millisecond)
	require.NoError(t, client.stop(t.Context()))

	err := <-stopped
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStopped, "Stop ends the retry sequence at its next boundary")
	assert.Equal(t, 1, transport.attemptCount())
}
