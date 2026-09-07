package elasticsearch

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientDoValidatesRequestAndHandlesBackendBoundaries(t *testing.T) {
	client := newClientWithBackend(&stubBackend{}, time.Second)
	invalid := []Request{
		{Path: "/"},
		{Method: http.MethodGet, Path: "relative"},
		{Method: http.MethodGet, Path: "//other-host/path"},
	}
	for _, request := range invalid {
		if _, err := client.Do(context.Background(), request); err == nil {
			t.Fatalf("Do(%+v) unexpectedly succeeded", request)
		}
	}
	if _, err := client.Do(nil, Request{Method: http.MethodGet, Path: "/"}); err == nil {
		t.Fatal("Do(nil, ...) unexpectedly succeeded")
	}

	nilResponse := newClientWithBackend(&stubBackend{performFn: func(*http.Request) (*http.Response, error) {
		return nil, nil
	}}, time.Second)
	if _, err := nilResponse.Do(context.Background(), Request{Method: http.MethodGet, Path: "/"}); err == nil {
		t.Fatal("Do() accepted a nil backend response")
	}
}

func TestClientDoKeepsDeadlineUntilResponseBodyClosesAndClonesHeaders(t *testing.T) {
	var requestContext context.Context
	backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
		requestContext = request.Context()
		if got := request.Header.Get("X-Test"); got != "original" {
			t.Fatalf("request header = %q", got)
		}
		request.Header.Set("X-Test", "backend")
		return response(http.StatusOK, "ok"), nil
	}}
	client := newClientWithBackend(backend, time.Second)
	headers := http.Header{"X-Test": []string{"original"}}
	result, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/_cluster/health", Header: headers})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if requestContext == nil {
		t.Fatal("backend did not receive request context")
	}
	select {
	case <-requestContext.Done():
		t.Fatal("request context was canceled before response body ownership ended")
	default:
	}
	if got := headers.Get("X-Test"); got != "original" {
		t.Fatalf("caller's headers were mutated to %q", got)
	}
	if err := result.Body.Close(); err != nil {
		t.Fatalf("Body.Close() error = %v", err)
	}
	select {
	case <-requestContext.Done():
	case <-time.After(time.Second):
		t.Fatal("closing response body did not release request context")
	}
}

func TestClientDoSupportsNilHeadersAndEnforcesTimeout(t *testing.T) {
	backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
		if request.Header == nil {
			t.Fatal("Do supplied a nil Header map")
		}
		<-request.Context().Done()
		return nil, request.Context().Err()
	}}
	client := newClientWithBackend(backend, 15*time.Millisecond)
	_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/slow"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do() error = %v, want deadline exceeded", err)
	}
}

func TestClientCloseIsConcurrentSafeAndRejectsLaterRequests(t *testing.T) {
	backend := &stubBackend{}
	client := newClientWithBackend(backend, time.Second)
	const callers = 16
	results := make(chan error, callers)
	for range callers {
		go func() { results <- client.Close(context.Background()) }()
	}
	for range callers {
		if err := <-results; err != nil {
			t.Fatalf("close() error = %v", err)
		}
	}
	if backend.closes() != 1 {
		t.Fatalf("backend closes = %d, want 1", backend.closes())
	}
	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/"}); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Do() after close error = %v, want ErrClientClosed", err)
	}
}

func TestClientHealthChecksHTTPStatus(t *testing.T) {
	for _, test := range []struct {
		status  int
		wantErr bool
	}{{http.StatusOK, false}, {http.StatusServiceUnavailable, true}} {
		client := newClientWithBackend(&stubBackend{performFn: func(*http.Request) (*http.Response, error) {
			return response(test.status, strings.Repeat("x", 8<<10)), nil
		}}, time.Second)
		err := client.Health(context.Background())
		if (err != nil) != test.wantErr {
			t.Fatalf("Health() status %d error = %v", test.status, err)
		}
	}
}

func TestElasticFactoryAppliesAuthentication(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*Config)
		wantHeader string
	}{
		{name: "api key", configure: func(cfg *Config) { cfg.APIKey = "encoded-key" }, wantHeader: "APIKey encoded-key"},
		{name: "basic", configure: func(cfg *Config) { cfg.Username, cfg.Password = "service", "secret" }, wantHeader: "Basic c2VydmljZTpzZWNyZXQ="},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan *http.Request, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests <- request.Clone(context.Background())
				writer.Header().Set("X-Elastic-Product", "Elasticsearch")
				_, _ = io.WriteString(writer, `{}`)
			}))
			defer server.Close()
			cfg := validConfig(server.URL)
			test.configure(&cfg)
			normalized, err := normalizeConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			client, err := (elasticFactory{}).New(normalized)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer client.Close(context.Background())
			result, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/_cluster/health"})
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			_ = result.Body.Close()
			request := <-requests
			if got := request.Header.Get("Authorization"); got != test.wantHeader {
				t.Fatalf("Authorization = %q, want %q", got, test.wantHeader)
			}
		})
	}
}

func TestElasticFactoryUsesConfiguredCertificateAuthority(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Elastic-Product", "Elasticsearch")
		_, _ = io.WriteString(writer, `{}`)
	}))
	defer server.Close()

	certificate := server.Certificate()
	if _, err := x509.ParseCertificate(certificate.Raw); err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	contents := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if err := os.WriteFile(caPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := validConfig(server.URL)
	cfg.TLS.CAFile = caPath
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client, err := (elasticFactory{}).New(normalized)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer client.Close(context.Background())
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() with configured CA error = %v", err)
	}

	cfg.TLS.CAFile = filepath.Join(t.TempDir(), "missing.pem")
	normalized, err = normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (elasticFactory{}).New(normalized); err == nil {
		t.Fatal("New() with missing CA file unexpectedly succeeded")
	}
}
