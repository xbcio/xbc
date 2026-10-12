package httpclient

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigErrorsNeverEchoCredentialOrQueryValues greps the literal secrets
// an operator would have written into base_url: a rejected URL is exactly
// where credentials live, so no error may quote the value it rejected.
func TestConfigErrorsNeverEchoCredentialOrQueryValues(t *testing.T) {
	for _, raw := range []string{
		"https://app:supersecret@gateway.example/callback?token=topsecretquery",
		"ftp://gateway.example/callback?token=topsecretquery",
		"gateway.example/callback?token=topsecretquery",
		"https://gateway.example/call\x7fback?token=topsecretquery",
	} {
		err := Config{BaseURL: raw}.Validate()
		require.Error(t, err, "base_url %q must be rejected", raw)
		assert.NotContains(t, err.Error(), "supersecret", "error for %q echoed the password", raw)
		assert.NotContains(t, err.Error(), "topsecretquery", "error for %q echoed the query", raw)
		assert.True(t, strings.HasPrefix(err.Error(), "http-client:"), "error for %q lacks the module prefix: %v", raw, err)
	}
}

// TestTheEndpointReportShowsSchemeAndHostOnly pins the reporting rule with a
// secret planted in every position a naive report would have printed: the
// query, the path, and, through the start adapter, the same line a boot
// writes.
func TestTheEndpointReportShowsSchemeAndHostOnly(t *testing.T) {
	logger := &recordingLogger{}
	client := newTestClient(t, Config{
		BaseURL: "https://gateway.example/secret-path?token=topsecretquery",
	}, nil, nil)
	require.NoError(t, reportOnStart(client, newTestContext(logger, "gateway")))

	text := logger.text()
	assert.Contains(t, text, "https://gateway.example")
	assert.NotContains(t, text, "topsecretquery")
	assert.NotContains(t, text, "secret-path")
}

// TestTheCallersErrorChainQuotesNoQueryValues pins the boundary the package
// doc describes. net/http wraps every transport failure in its own *url.Error
// and that wrapper quotes the request URL -- query included -- by design; what
// the plugin controls is the inner error it hands net/http, reachable through
// errors.As, and that value must never repeat the query. Both failure paths
// are driven with a planted secret: a connection failure that ends the call,
// and a connection failure whose retry is skipped because the body cannot be
// replayed.
func TestTheCallersErrorChainQuotesNoQueryValues(t *testing.T) {
	const planted = "topsecretquery"
	transportErr := errors.New("connection refused")

	for _, tc := range []struct {
		name    string
		body    io.Reader
		getBody func() (io.ReadCloser, error)
		// wantInner, when set, must appear in the inner error: the reason a
		// retry was skipped is part of what the caller is told.
		wantInner string
	}{
		{
			name: "connection failure",
		},
		{
			name: "retry skipped because the body cannot be replayed",
			body: strings.NewReader("payload"),
			getBody: func() (io.ReadCloser, error) {
				return nil, errors.New("body replay refused")
			},
			wantInner: "body replay refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := newSyntheticTransport(syntheticOutcome{err: transportErr})
			client := newSyntheticClient(t, retryConfig(3), transport)

			request := mustRequest(t, http.MethodGet, "http://downstream.test/resource?token="+planted, tc.body)
			request.GetBody = tc.getBody
			_, err := client.Client().Do(request)
			require.Error(t, err)
			var urlErr *url.Error
			require.ErrorAs(t, err, &urlErr, "net/http wraps every transport failure in *url.Error")
			assert.NotContains(t, urlErr.Err.Error(), planted,
				"the error this package hands net/http must not quote the request query")
			assert.ErrorIs(t, urlErr.Err, transportErr, "the attempt's own failure stays in the chain")
			if tc.wantInner != "" {
				assert.Contains(t, urlErr.Err.Error(), tc.wantInner,
					"why the retry was skipped reaches the caller")
			}
		})
	}
}

// TestRequestErrorsAndLogsStayFreeOfHeaderValues drives the failure path with
// a credential in a header and pins that nothing this package emits contains
// it. The net/http url.Error reachable through Do is documented as the
// boundary: it quotes the request URL -- query included -- because that error
// is produced by net/http itself, and rewriting its text would break the
// errors.As chain callers use to classify transport failures. What the
// plugin controls -- its own errors and its own logs -- is asserted here.
func TestRequestErrorsAndLogsStayFreeOfHeaderValues(t *testing.T) {
	logger := &recordingLogger{}
	transport := newSyntheticTransport(syntheticOutcome{err: errors.New("connection refused")})
	client := newSyntheticClient(t, retryConfig(1), transport)

	request := mustRequest(t, "GET", "http://downstream.test/resource", nil)
	request.Header.Set("Authorization", "Bearer supersecretheader")
	_, err := client.Client().Do(request)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "supersecretheader",
		"no error this package produces may copy a request header value")
	assert.Empty(t, logger.text(), "a request path logs nothing by itself")
}
