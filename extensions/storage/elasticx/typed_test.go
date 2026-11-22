package elasticx

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	elastic "github.com/elastic/go-elasticsearch/v8"
)

type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// newTypedTestServer records requests and answers with a valid, empty search
// response. X-Elastic-Product is what the vendor client's product check
// requires on the first successful answer.
func newTypedTestServer(t *testing.T) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reader := io.Reader(request.Body)
		if request.Header.Get("Content-Encoding") == "gzip" {
			gzipReader, err := gzip.NewReader(request.Body)
			if err != nil {
				t.Errorf("gzip.NewReader() error = %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			defer gzipReader.Close()
			reader = gzipReader
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		requests <- capturedRequest{
			Method: request.Method,
			Path:   request.URL.Path,
			Header: request.Header.Clone(),
			Body:   body,
		}
		writer.Header().Set("X-Elastic-Product", "Elasticsearch")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"took":1,"timed_out":false,"_shards":{"total":1,"successful":1,"skipped":0,"failed":0},"hits":{"total":{"value":0,"relation":"eq"},"max_score":null,"hits":[]}}`)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func TestNewTypedClientSharesAuthenticationCompressionAndTransport(t *testing.T) {
	server, requests := newTypedTestServer(t)
	cfg := validConfig(server.URL)
	cfg.Username = "service"
	cfg.Password = "secret"
	cfg.CompressRequestBody = true

	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	typed := client.Typed()
	if typed == nil {
		t.Fatal("Typed() = nil, want the typed client built by New")
	}

	const query = `{"query":{"match_all":{}}}`
	if _, err := typed.Core.Search().Index("events").Raw(strings.NewReader(query)).Do(context.Background()); err != nil {
		t.Fatalf("typed Search() error = %v", err)
	}

	captured := <-requests
	if captured.Method != http.MethodPost || captured.Path != "/events/_search" {
		t.Fatalf("request = %s %s, want POST /events/_search", captured.Method, captured.Path)
	}
	wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("service:secret"))
	if got := captured.Header.Get("Authorization"); got != wantAuthorization {
		t.Fatalf("Authorization = %q, want %q", got, wantAuthorization)
	}
	if got := captured.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip when compress_request_body is set", got)
	}
	if got := string(captured.Body); got != query {
		t.Fatalf("decompressed body = %q, want %q", got, query)
	}
}

func TestNewAcceptsEmptyPasswordOnlyWhenExplicitlyAllowed(t *testing.T) {
	_, err := New(Config{
		Addresses:   []string{"http://127.0.0.1:1"},
		Username:    "service",
		HealthProbe: false,
	})
	if err == nil || !strings.Contains(err.Error(), "allow_empty_password") {
		t.Fatalf("New() error = %v, want the pair rule and its explicit escape hatch", err)
	}

	server, requests := newTypedTestServer(t)
	cfg := validConfig(server.URL)
	cfg.Username = "service"
	cfg.AllowEmptyPassword = true

	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New() with allow_empty_password error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if _, err := client.Typed().Core.Info().Do(context.Background()); err != nil {
		t.Fatalf("typed Info() error = %v", err)
	}

	captured := <-requests
	// go-elasticsearch deliberately sends Basic authentication only when both
	// fields are non-empty; SAS's current client has the same behavior. The
	// switch only relaxes validation, it does not invent a credential.
	if got := captured.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none for a username with an empty password", got)
	}
}

func TestTypedClientIsClosedWithItsClient(t *testing.T) {
	server, requests := newTypedTestServer(t)
	cfg := validConfig(server.URL)

	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	typed := client.Typed()
	if typed == nil {
		t.Fatal("Typed() = nil")
	}
	if _, err := typed.Core.Info().Do(context.Background()); err != nil {
		t.Fatalf("typed Info() before Close error = %v", err)
	}
	<-requests

	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := typed.Core.Info().Do(context.Background()); !errors.Is(err, elastic.ErrClosed) {
		t.Fatalf("typed Info() after Close error = %v, want ErrClosed", err)
	}
}
