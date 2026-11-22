package elasticx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	elastic "github.com/elastic/go-elasticsearch/v8"
)

var ErrClientClosed = errors.New("elasticsearch: client is closed")

type clientBackend interface {
	Perform(*http.Request) (*http.Response, error)
	Close(context.Context) error
}

// Client is XBC's timeout-aware Elasticsearch client. Do is the stable primary
// API; Raw is an explicit escape hatch to go-elasticsearch's full untyped
// generated API, and Typed is the same escape hatch to its typed API.
type Client struct {
	raw     *elastic.Client
	typed   *elastic.TypedClient
	backend clientBackend
	timeout time.Duration

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error

	lifecycleMu  sync.Mutex
	bulkConfig   BulkConfig
	healthProbe  bool
	probePolicy  string
	bulkObserver BulkObserver
	bulk         *asyncBulkIndexer
	stopDone     chan struct{}
	stopErr      error

	// draining is set by drainClient and keeps a later Start from creating a
	// fresh bulk worker.
	//
	// bulkClaimedBy records which side first observed the bulk worker's
	// final Close result and is therefore responsible for the failure,
	// decided in one critical section so a drainClient and a finishClose
	// that wake up together cannot both claim it:
	//   - bulkClaimOwnerDrain: a drainClient call observed completion first.
	//     Every drainClient call, before and after, replays the same cached
	//     bulkErr, so Drain is idempotent including its result; Stop never
	//     reports it.
	//   - bulkClaimOwnerStop: no drainClient call observed completion
	//     (Drain never ran, or every Drain call timed out first), so
	//     finishClose (Stop) is the sole owner. Stop reports bulkErr exactly
	//     once via stopErr; a drainClient call made after Stop has claimed it
	//     deliberately returns nil for it instead of re-reporting a failure
	//     Stop already surfaced.
	draining      bool
	bulkErr       error
	bulkClaimedBy bulkClaimOwner
}

// bulkClaimOwner records which side of shutdown is responsible for
// reporting the bulk worker's final Close failure, decided once that result
// is first observed.
type bulkClaimOwner int

const (
	bulkClaimOwnerNone bulkClaimOwner = iota
	bulkClaimOwnerDrain
	bulkClaimOwnerStop
)

// Request describes one cluster-relative HTTP request. Path must begin with a
// single slash, which prevents bypassing the configured cluster endpoints.
type Request struct {
	Method string
	Path   string
	Body   io.Reader
	Header http.Header
}

func newClient(raw *elastic.Client, typed *elastic.TypedClient, timeout time.Duration) *Client {
	return &Client{raw: raw, typed: typed, backend: raw, timeout: timeout}
}

func newClientWithBackend(backend clientBackend, timeout time.Duration) *Client {
	return &Client{backend: backend, timeout: timeout}
}

// Raw returns the vendor untyped client. Calls made through Raw do not receive
// the timeout applied by Do, so callers must provide bounded contexts
// themselves.
func (c *Client) Raw() *elastic.Client { return c.raw }

// Typed returns the vendor typed client. It is built during construction from
// the same addresses, authentication, TLS, and transport configuration as Raw,
// so its configuration errors surface there and closing the Client closes it.
// Typed returns nil when the Client was built over a caller-supplied backend,
// which only tests do.
func (c *Client) Typed() *elastic.TypedClient {
	if c == nil {
		return nil
	}
	return c.typed
}

// Do performs a request against the configured cluster. Callers own and must
// close a successful response body.
func (c *Client) Do(ctx context.Context, request Request) (*http.Response, error) {
	if ctx == nil {
		return nil, errors.New("elasticsearch: request requires a non-nil context")
	}
	if c == nil || c.backend == nil || c.closed.Load() {
		return nil, ErrClientClosed
	}
	method := strings.TrimSpace(request.Method)
	if method == "" {
		return nil, errors.New("elasticsearch: request method is required")
	}
	if !strings.HasPrefix(request.Path, "/") || strings.HasPrefix(request.Path, "//") {
		return nil, errors.New("elasticsearch: request path must begin with one slash")
	}
	parsed, err := url.Parse("http://" + request.Path)
	if err != nil || parsed.Host != "" {
		return nil, errors.New("elasticsearch: invalid cluster-relative request path")
	}
	bounded, cancel := context.WithTimeout(ctx, c.timeout)
	httpRequest, err := http.NewRequestWithContext(bounded, method, parsed.String(), request.Body)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("elasticsearch: build request: %w", err)
	}
	if request.Header != nil {
		httpRequest.Header = request.Header.Clone()
	}
	response, err := c.backend.Perform(httpRequest)
	if err != nil {
		cancel()
		if errors.Is(err, elastic.ErrClosed) {
			return nil, ErrClientClosed
		}
		return nil, fmt.Errorf("elasticsearch: perform request: %w", err)
	}
	if response == nil {
		cancel()
		return nil, errors.New("elasticsearch: perform request returned a nil response")
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	if err != nil {
		r.once.Do(r.cancel)
	}
	return n, err
}

func (r *cancelReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.cancel)
	return err
}

// Health verifies that the configured endpoint is an available Elasticsearch
// cluster. It has no dependency on a web framework or health endpoint shape.
func (c *Client) Health(ctx context.Context) error {
	response, err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/"})
	if err != nil {
		return fmt.Errorf("elasticsearch: health probe: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("elasticsearch: health probe returned HTTP %d", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	return nil
}

func (c *Client) closeTransport(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		var errs []error
		if c.backend != nil {
			errs = append(errs, c.backend.Close(ctx))
		}
		if c.typed != nil {
			errs = append(errs, c.typed.Close(ctx))
		}
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}

// claimBulkErrForDrain is called by every drainClient caller once the bulk
// worker's Close has returned. The first call to reach here claims
// bulkClaimOwnerDrain and caches err for replay; every call after that, from
// this drainClient caller or any other, replays the same cached error under
// the lock instead of re-deriving ownership, so Drain's result is idempotent
// and every drainClient caller that observes completion returns the same
// failure. If finishClose already claimed ownership first, this returns
// nil: Stop is the sole owner and Drain must not re-report what Stop already
// surfaced.
func (c *Client) claimBulkErrForDrain(err error) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.bulkClaimedBy == bulkClaimOwnerNone {
		c.bulkClaimedBy = bulkClaimOwnerDrain
		c.bulkErr = err
	}
	if c.bulkClaimedBy != bulkClaimOwnerDrain {
		return nil
	}
	return c.bulkErr
}

// claimBulkErrForStop is called by finishClose after the bulk worker's Close
// has returned. If a drainClient call already claimed ownership
// (bulkClaimOwnerDrain), Stop never reports the failure, even if that
// drainClient call is still blocked replaying it. Otherwise Stop claims
// ownership itself and reports err exactly once; because Stop's claim is
// also recorded here, a drainClient call made after Stop has claimed it sees
// bulkClaimOwnerStop and returns nil for it, deliberately not re-reporting a
// failure Stop already returned.
func (c *Client) claimBulkErrForStop(err error) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.bulkClaimedBy == bulkClaimOwnerDrain {
		return nil
	}
	if c.bulkClaimedBy == bulkClaimOwnerStop {
		return nil
	}
	c.bulkClaimedBy = bulkClaimOwnerStop
	c.bulkErr = err
	return c.bulkErr
}

type clientFactory interface {
	New(normalizedConfig) (*Client, error)
}

type elasticFactory struct{}

func (elasticFactory) New(cfg normalizedConfig) (*Client, error) {
	config, err := buildElasticConfig(cfg)
	if err != nil {
		return nil, err
	}
	raw, err := elastic.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch: create client: %w", err)
	}
	typed, err := elastic.NewTypedClient(config)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("elasticsearch: create typed client: %w", err),
			raw.Close(context.Background()),
		)
	}
	return newClient(raw, typed, cfg.Timeout), nil
}

// buildElasticConfig is the single mapping from XBC configuration to the
// vendor configuration both clients use. Keeping it in one place is what makes
// Raw and Typed share address, authentication, TLS, compression, and transport
// behavior instead of drifting into two construction paths.
func buildElasticConfig(cfg normalizedConfig) (elastic.Config, error) {
	var ca []byte
	if cfg.TLS.CAFile != "" {
		contents, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return elastic.Config{}, fmt.Errorf("elasticsearch: read tls ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(contents) {
			return elastic.Config{}, errors.New("elasticsearch: tls ca_file contains no certificates")
		}
		ca = contents
	}
	config := elastic.Config{
		Addresses:           cfg.Addresses,
		CloudID:             cfg.CloudID,
		APIKey:              cfg.APIKey,
		Username:            cfg.Username,
		Password:            cfg.Password,
		CompressRequestBody: cfg.CompressRequestBody,
	}
	if cfg.TLS.InsecureSkipVerify {
		transport, err := insecureTransport(ca)
		if err != nil {
			return elastic.Config{}, err
		}
		config.Transport = transport
	} else if ca != nil {
		config.CACert = ca
	}
	return config, nil
}

// insecureTransport builds the explicit TLS opt-in. It starts from
// http.DefaultTransport so proxy, dial, and idle-connection behavior match the
// default CACert path; the vendor client accepts this transport as-is. A
// caller-supplied CA is still installed as the root pool, although
// InsecureSkipVerify makes verification moot: loading it keeps a bad ca_file a
// construction error rather than a silently ignored setting.
func insecureTransport(ca []byte) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("elasticsearch: default HTTP transport is not an *http.Transport")
	}
	transport := base.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // explicit configuration opt-in for internal clusters
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("elasticsearch: tls ca_file contains no certificates")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	return transport, nil
}
