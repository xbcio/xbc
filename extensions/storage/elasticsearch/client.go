package elasticsearch

import (
	"context"
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
// API; Raw is an explicit escape hatch to go-elasticsearch's full generated API.
type Client struct {
	raw     *elastic.Client
	backend clientBackend
	timeout time.Duration

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error

	lifecycleMu  sync.Mutex
	bulkConfig   BulkConfig
	healthProbe  bool
	bulkObserver BulkObserver
	bulk         *asyncBulkIndexer
	stopDone     chan struct{}
	stopErr      error
}

// Request describes one cluster-relative HTTP request. Path must begin with a
// single slash, which prevents bypassing the configured cluster endpoints.
type Request struct {
	Method string
	Path   string
	Body   io.Reader
	Header http.Header
}

func newClient(raw *elastic.Client, timeout time.Duration) *Client {
	return &Client{raw: raw, backend: raw, timeout: timeout}
}

func newClientWithBackend(backend clientBackend, timeout time.Duration) *Client {
	return &Client{backend: backend, timeout: timeout}
}

// Raw returns the vendor client. Calls made through Raw do not receive the
// timeout applied by Do, so callers must provide bounded contexts themselves.
func (c *Client) Raw() *elastic.Client { return c.raw }

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
		if c.backend != nil {
			c.closeErr = c.backend.Close(ctx)
		}
	})
	return c.closeErr
}

type clientFactory interface {
	New(normalizedConfig) (*Client, error)
}

type elasticFactory struct{}

func (elasticFactory) New(cfg normalizedConfig) (*Client, error) {
	var ca []byte
	if cfg.TLS.CAFile != "" {
		contents, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("elasticsearch: read tls ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(contents) {
			return nil, errors.New("elasticsearch: tls ca_file contains no certificates")
		}
		ca = contents
	}
	raw, err := elastic.NewClient(elastic.Config{
		Addresses: cfg.Addresses,
		CloudID:   cfg.CloudID,
		APIKey:    cfg.APIKey,
		Username:  cfg.Username,
		Password:  cfg.Password,
		CACert:    ca,
	})
	if err != nil {
		return nil, fmt.Errorf("elasticsearch: create client: %w", err)
	}
	return newClient(raw, cfg.Timeout), nil
}
