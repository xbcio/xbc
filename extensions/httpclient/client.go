package httpclient

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/extensions/observability/tracing"
	"github.com/xbcio/xbc/log"
)

// Client is one named outbound client. It owns a standard *http.Client whose
// transport carries the configured policy; callers reach the network only
// through that value, so the retry, timeout, tracing, and metrics behavior
// applies to every call and to every redirect inside a call.
type Client struct {
	instance string
	config   Config
	client   *http.Client
	stopped  atomic.Bool
}

// newClient builds the client one factory call owns. Everything that can fail
// eagerly does: the TLS material is loaded, the optional metric collectors
// are registered, and a duplicate or invalid registration fails construction
// -- and therefore validate -- instead of surfacing at the first request.
func newClient(instance string, config Config, tracer tracing.Handle, registerer metrics.Registerer) (*Client, error) {
	collectors, err := newClientMetrics(instance, registerer)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := config.TLS.clientTLSConfig()
	if err != nil {
		return nil, err
	}
	client := &Client{instance: instance, config: config}
	// The pool knobs mirror http.DefaultTransport except where the
	// configuration names them, so an instance that configures nothing
	// behaves like a plainly constructed http.Client.
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
		IdleConnTimeout:       config.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}
	client.client = &http.Client{
		Transport: &retryTransport{
			config:   config,
			next:     transport,
			metrics:  collectors,
			tracer:   tracer,
			instance: instance,
			stopped:  &client.stopped,
		},
		// The client-level timeout is the total budget seen from outside: it
		// bounds the whole Do call -- redirects included -- and, through
		// net/http's body cancellation, the reading of the final response.
		// The transport derives its own view of the same budget to decide
		// whether another attempt still fits.
		Timeout: config.TotalTimeout,
	}
	return client, nil
}

// Client returns the underlying *http.Client. A consumer configures its
// request library with it once -- resty's NewWithClient, an SDK's own option
// -- or calls Do directly, and keeps the standard net/http surface either
// way. The value is handed out, not copied: every instance keeps exactly one
// client and one connection pool, and the plugin never replaces it.
func (c *Client) Client() *http.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// stop refuses every request that has not started yet and releases the
// instance's pooled idle connections. It is safe before Start, on repeated
// calls, and on a client whose construction was the only thing that ever ran,
// and it never waits, so no context can make it fail: its error is always
// nil and exists only to satisfy the lifecycle stage shape.
func (c *Client) stop(_ context.Context) error {
	c.stopped.Store(true)
	if c.client != nil {
		c.client.CloseIdleConnections()
	}
	return nil
}

// reportEndpoint logs what this instance is pointed at. It prints the
// scheme://host of base_url and nothing else: the path and query of a
// configured URL routinely carry ids, and userinfo is rejected at
// validation precisely because this line -- and every other place a URL is
// rendered -- must stay safe to log. An instance that configures no base_url
// prints nothing rather than an empty value.
func (c *Client) reportEndpoint(logger log.Logger) {
	endpoint := c.config.endpoint()
	if endpoint == "" {
		return
	}
	logger.Info("xbc: http-client endpoint", "base_url", endpoint)
}
