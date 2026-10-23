package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// failingBody fails every read.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("read failure") }
func (failingBody) Close() error             { return nil }

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	target, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, err)
	}
	return target
}

func TestNewClientRejectsInvalidPolicy(t *testing.T) {
	for name, policy := range map[string]Policy{
		"negative dial timeout":      {DialTimeout: -time.Second},
		"negative handshake timeout": {TLSHandshakeTimeout: -time.Second},
		"negative total timeout":     {TotalTimeout: -time.Second},
		"negative body limit":        {MaxBodyBytes: -1},
	} {
		if _, err := NewClient(policy); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: NewClient() error = %v, want ErrInvalidPolicy", name, err)
		}
	}
}

func TestNewClientDefaults(t *testing.T) {
	client, err := NewClient(Policy{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client.Timeout != 30*time.Second {
		t.Fatalf("Timeout = %v, want 30s", client.Timeout)
	}
	if client.CheckRedirect == nil {
		t.Fatal("CheckRedirect = nil, want the policy redirect check")
	}
	transport, ok := client.Transport.(policyTransport)
	if !ok {
		t.Fatalf("Transport type = %T, want policyTransport", client.Transport)
	}
	if transport.policy.MaxRedirects != 5 {
		t.Fatalf("transport policy MaxRedirects = %d, want 5", transport.policy.MaxRedirects)
	}
}

func TestClientRejectsPlainHTTPByDefault(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	defer server.Close()

	// AllowPrivate is on so the scheme is the only rule that can reject the
	// request; the server is never reached.
	client, err := NewClient(Policy{AllowPrivate: true})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	response, err := client.Get(server.URL)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrURLRejected) {
		t.Fatalf("Get() error = %v, want ErrURLRejected", err)
	}
	if hits != 0 {
		t.Fatalf("server hits = %d, want 0", hits)
	}
}

func TestClientReachesDevelopmentTargetsWhenConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	client, err := NewClient(Policy{AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer response.Body.Close()
	body, err := ReadBody(response, 0)
	if err != nil {
		t.Fatalf("ReadBody() error = %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}

func TestClientStripsCredentialsAcrossOrigins(t *testing.T) {
	var authorization, cookie string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		cookie = request.Header.Get("Cookie")
		_, _ = io.WriteString(w, "target")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, target.URL+"/final", http.StatusFound)
	}))
	defer source.Close()

	client, err := NewClient(Policy{AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer top-secret")
	request.Header.Set("Cookie", "session=top-secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer response.Body.Close()
	if authorization != "" || cookie != "" {
		t.Fatalf("redirect target saw Authorization=%q Cookie=%q, want both stripped", authorization, cookie)
	}
}

func TestClientKeepsCredentialsOnSameOriginRedirect(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/start" {
			http.Redirect(w, request, "/final", http.StatusFound)
			return
		}
		authorization = request.Header.Get("Authorization")
		_, _ = io.WriteString(w, "done")
	}))
	defer server.Close()

	client, err := NewClient(Policy{AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer keep")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer response.Body.Close()
	if authorization != "Bearer keep" {
		t.Fatalf("same-origin Authorization = %q, want it preserved", authorization)
	}
}

func TestClientRejectsTooManyRedirects(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, server.URL+"/loop", http.StatusFound)
	}))
	defer server.Close()

	client, err := NewClient(Policy{AllowHTTP: true, AllowPrivate: true, MaxRedirects: 2})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	response, err := client.Get(server.URL)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("Get() error = %v, want ErrTooManyRedirects", err)
	}
}

func TestClientRejectsEveryRedirectWhenDisabled(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, "/next", http.StatusFound)
	}))
	defer source.Close()

	client, err := NewClient(Policy{AllowHTTP: true, AllowPrivate: true, MaxRedirects: NoRedirects})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	response, err := client.Get(source.URL)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("Get() error = %v, want ErrTooManyRedirects", err)
	}
}

func TestClientRejectsRedirectToMetadataAddress(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer source.Close()

	client, err := NewClient(Policy{AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	response, err := client.Get(source.URL)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrAddressRejected) {
		t.Fatalf("Get() error = %v, want ErrAddressRejected", err)
	}
}

func TestClientRevalidatesRedirectHostAtDialTime(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, "http://internal.invalid/secret", http.StatusFound)
	}))
	defer source.Close()

	resolver := &staticResolver{addresses: map[string][]netip.Addr{
		"internal.invalid": {netip.MustParseAddr("169.254.169.254")},
	}}
	var requested []string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		requested = append(requested, address)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	client, err := newClient(Policy{AllowHTTP: true, AllowPrivate: true}, resolver, dial)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}
	response, err := client.Get(source.URL)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrAddressRejected) {
		t.Fatalf("Get() error = %v, want ErrAddressRejected", err)
	}
	for _, address := range requested {
		if strings.Contains(address, "internal.invalid") {
			t.Fatalf("dialed the unresolved host name: %q", address)
		}
	}
	if len(resolver.hosts) == 0 || resolver.hosts[len(resolver.hosts)-1] != "internal.invalid" {
		t.Fatalf("resolver hosts = %v, want the redirect host to be resolved", resolver.hosts)
	}
}

func TestClientRejectsRedirectedPost(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits++
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client, err := NewClient(Policy{AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	response, err := client.Post(source.URL, "application/x-www-form-urlencoded", strings.NewReader("client_secret=top-secret"))
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrRedirectRejected) {
		t.Fatalf("Post() error = %v, want ErrRedirectRejected", err)
	}
	if targetHits != 0 {
		t.Fatalf("redirect target hits = %d, want 0", targetHits)
	}
}

func TestCheckRedirectRejectsInvalidChainAndDowngrade(t *testing.T) {
	policy, err := (Policy{AllowHTTP: true}).normalized()
	if err != nil {
		t.Fatalf("normalized() error = %v", err)
	}
	if err := policy.checkRedirect(&http.Request{URL: mustParseURL(t, "https://idp.example.test/")}, nil); !errors.Is(err, ErrRedirectRejected) {
		t.Fatalf("empty chain error = %v, want ErrRedirectRejected", err)
	}

	original := &http.Request{Method: http.MethodGet, URL: mustParseURL(t, "https://idp.example.test/start")}
	after := &http.Request{
		Method: http.MethodGet,
		URL:    mustParseURL(t, "http://idp.example.test/next"),
		Header: make(http.Header),
	}
	if err := policy.checkRedirect(after, []*http.Request{original}); !errors.Is(err, ErrInsecureRedirect) {
		t.Fatalf("downgrade error = %v, want ErrInsecureRedirect", err)
	}
}

func TestCheckRedirectStripsSensitiveHeaders(t *testing.T) {
	policy := DefaultPolicy()
	original := &http.Request{Method: http.MethodGet, URL: mustParseURL(t, "https://idp.example.test/start")}
	after := &http.Request{
		Method: http.MethodGet,
		URL:    mustParseURL(t, "https://other.example.test/next"),
		Header: http.Header{
			"Authorization":       []string{"Bearer top-secret"},
			"Proxy-Authorization": []string{"Basic top-secret"},
			"Cookie":              []string{"session=top-secret"},
			"Cookie2":             []string{"session=top-secret"},
			"Referer":             []string{"https://idp.example.test/start"},
			"Origin":              []string{"https://idp.example.test"},
			"Accept":              []string{"application/json"},
		},
	}
	if err := policy.checkRedirect(after, []*http.Request{original}); err != nil {
		t.Fatalf("checkRedirect() error = %v", err)
	}
	for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Cookie2", "Referer", "Origin"} {
		if value := after.Header.Get(header); value != "" {
			t.Fatalf("%s = %q, want stripped", header, value)
		}
	}
	if after.Header.Get("Accept") != "application/json" {
		t.Fatal("Accept header was stripped, want it preserved")
	}
}

func TestReadBody(t *testing.T) {
	t.Run("reads within limit", func(t *testing.T) {
		body, err := ReadBody(&http.Response{Body: io.NopCloser(strings.NewReader("hello"))}, 5)
		if err != nil || string(body) != "hello" {
			t.Fatalf("ReadBody() = (%q, %v)", body, err)
		}
	})
	t.Run("rejects over limit", func(t *testing.T) {
		if _, err := ReadBody(&http.Response{Body: io.NopCloser(strings.NewReader("hello!"))}, 5); !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("ReadBody() error = %v, want ErrBodyTooLarge", err)
		}
	})
	t.Run("default limit applies", func(t *testing.T) {
		body, err := ReadBody(&http.Response{Body: io.NopCloser(strings.NewReader("hello"))}, 0)
		if err != nil || string(body) != "hello" {
			t.Fatalf("ReadBody() = (%q, %v)", body, err)
		}
	})
	t.Run("nil response", func(t *testing.T) {
		if _, err := ReadBody(nil, 10); !errors.Is(err, ErrNilResponse) {
			t.Fatalf("ReadBody(nil) error = %v, want ErrNilResponse", err)
		}
	})
	t.Run("nil body", func(t *testing.T) {
		if _, err := ReadBody(&http.Response{}, 10); !errors.Is(err, ErrNilResponse) {
			t.Fatalf("ReadBody(nil body) error = %v, want ErrNilResponse", err)
		}
	})
	t.Run("read failure", func(t *testing.T) {
		if _, err := ReadBody(&http.Response{Body: failingBody{}}, 10); !errors.Is(err, ErrBodyRead) {
			t.Fatalf("ReadBody(failing body) error = %v, want ErrBodyRead", err)
		}
	})
}

func TestSecretRedactsValue(t *testing.T) {
	const value = "super-secret-value"
	secret := Secret(value)
	embedded := struct{ ClientSecret Secret }{ClientSecret: secret}
	for _, rendered := range []string{
		fmt.Sprint(secret),
		fmt.Sprintf("%s", secret),
		fmt.Sprintf("%v", secret),
		fmt.Sprintf("%+v", secret),
		fmt.Sprintf("%#v", secret),
		fmt.Sprintf("%q", secret),
		fmt.Sprintf("%x", secret),
		fmt.Sprintf("%d", secret),
		fmt.Sprintf("%#v", embedded),
		fmt.Sprintf("%#v", &embedded),
		secret.String(),
	} {
		if strings.Contains(rendered, value) {
			t.Fatalf("rendered secret %q contains the raw value", rendered)
		}
		if !strings.Contains(rendered, "[REDACTED]") {
			t.Fatalf("rendered secret %q does not contain [REDACTED]", rendered)
		}
	}

	// JSON is the serialization boundary MarshalText exists for: a bare
	// Secret, a structure that holds one, and a map value all pass through
	// encoding/json, which consults TextMarshaler rather than String.
	for name, encode := range map[string]func() ([]byte, error){
		"bare":   func() ([]byte, error) { return json.Marshal(secret) },
		"struct": func() ([]byte, error) { return json.Marshal(embedded) },
		"map":    func() ([]byte, error) { return json.Marshal(map[string]any{"nested": secret}) },
	} {
		encoded, err := encode()
		if err != nil {
			t.Fatalf("json.Marshal(%s) = %v", name, err)
		}
		if strings.Contains(string(encoded), value) {
			t.Fatalf("json.Marshal(%s) rendered the raw value: %s", name, encoded)
		}
		if !strings.Contains(string(encoded), "[REDACTED]") {
			t.Fatalf("json.Marshal(%s) does not contain [REDACTED]: %s", name, encoded)
		}
	}
}
