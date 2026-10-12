package httpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientHandsOutAWorkingHTTPClient(t *testing.T) {
	server := newScriptedServer(t, http.StatusOK)
	client := newTestClient(t, Config{}, nil, nil)

	response, err := client.Client().Get(server.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, 1, server.count())
}

func TestStopRefusesLaterRequestsAndIsIdempotent(t *testing.T) {
	server := newScriptedServer(t, http.StatusOK)
	client := newTestClient(t, Config{}, nil, nil)
	require.NoError(t, client.stop(context.Background()))

	_, err := client.Client().Get(server.URL)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStopped)
	assert.Equal(t, 0, server.count(), "a stopped client never reaches the network")

	// Repeating Stop stays correct and success-valued, and a client stopped
	// before it ever started stops the same way.
	require.NoError(t, client.stop(context.Background()))
	fresh := newTestClient(t, Config{}, nil, nil)
	require.NoError(t, fresh.stop(context.Background()))
	_, err = fresh.Client().Get(server.URL)
	assert.ErrorIs(t, err, ErrStopped)
}

func TestStopIsSafeOnAZeroValuedClient(t *testing.T) {
	var client Client
	require.NoError(t, client.stop(context.Background()))
	assert.Nil(t, client.Client(), "a zero Client has nothing to hand out")
}

// TestStopClosesIdleConnections pins the pool half of Stop against the one
// witness that can see it: the peer. net/http's Client.CloseIdleConnections
// only reaches a transport that implements the method, so this test fails
// both when Stop stops forwarding and when the retry engine stops relaying
// the call to the *http.Transport beneath it.
func TestStopClosesIdleConnections(t *testing.T) {
	states := newObservedConnStates()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.Config.ConnState = states.record
	server.Start()
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{}, nil, nil)
	response, err := client.Client().Get(server.URL)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())

	require.Eventually(t, func() bool { return states.saw(http.StateIdle) }, time.Second, time.Millisecond,
		"the keep-alive connection must be idle before Stop is asked to close it")
	require.NoError(t, client.stop(t.Context()))
	require.Eventually(t, func() bool { return states.saw(http.StateClosed) }, 2*time.Second, time.Millisecond,
		"Stop must close the idle connection of the transport beneath the retry engine")
}

func TestStartAndPreflightReportTheEndpointWithoutSecrets(t *testing.T) {
	logger := &recordingLogger{}
	client := newTestClient(t, Config{BaseURL: "https://gateway.example/private?token=supersecret"}, nil, nil)

	ctx := newTestContext(logger, "gateway")
	require.NoError(t, reportOnStart(client, ctx))
	require.NoError(t, reportOnValidate(client, ctx))

	text := logger.text()
	assert.Contains(t, text, "xbc: http-client endpoint")
	assert.Contains(t, text, "https://gateway.example")
	assert.NotContains(t, text, "supersecret", "a query secret must not reach the log")
	assert.NotContains(t, text, "/private", "the path is not part of the report")
	assert.NotContains(t, text, "token")
}

func TestEndpointReportIsSilentWithoutABaseURL(t *testing.T) {
	logger := &recordingLogger{}
	client := newTestClient(t, Config{}, nil, nil)
	require.NoError(t, reportOnStart(client, newTestContext(logger, "bare")))
	assert.Empty(t, logger.text(), "an instance with no base_url prints nothing")
}

func TestReportAdaptersRejectAMissingContext(t *testing.T) {
	client := newTestClient(t, Config{}, nil, nil)
	assert.Error(t, reportOnStart(client, nil))
	assert.Error(t, reportOnValidate(client, nil))
}
