package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/apikey"
	jwtplugin "github.com/xbcio/xbc/transport/web/extensions/authentication/jwt"
)

// startWait bounds every wait in this file. It is generous on purpose: the
// first assertion a failure should produce is the one naming what did not
// happen, not a timeout on a shared machine.
const startWait = 15 * time.Second

// TestShippedConfigurationValidates runs the validator against the
// application.yml this directory ships and the composition main.go ships, so a
// key that no plugin owns, a section that no longer exists, or an
// authentication policy that names an unregistered scheme fails here rather
// than in a deployment.
//
// The signing secret and the database location come from the environment
// because the shipped file deliberately carries neither: the secret has no
// default anywhere, and validate must not create a database beside the source
// tree.
func TestShippedConfigurationValidates(t *testing.T) {
	t.Setenv("XBC_PLUGINS_JWT_SECRET", strings.Repeat("v", 48))
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_DSN", sqliteDSN(t))

	app, err := xbc.New(options()...)
	if err != nil {
		t.Fatalf("xbc.New() error = %v", err)
	}

	code, err := app.Execute(context.Background(), []string{"validate", "--config", "application.yml"})
	if err != nil || code != 0 {
		t.Fatalf("validate application.yml = (code %d, error %v), want (0, nil)", code, err)
	}
}

// TestShippedConfigurationRequiresASigningSecret pins the other half of the
// file's design. The secret is deliberately absent from application.yml and
// plugins.jwt declares no default for it, so a deployment that forgets to
// supply one gets a startup failure naming the field rather than a process
// running on a guessable key.
func TestShippedConfigurationRequiresASigningSecret(t *testing.T) {
	// An empty value neutralizes a secret the surrounding environment may
	// carry, so the assertion stays about the file rather than about the shell
	// this test happens to run in.
	t.Setenv("XBC_PLUGINS_JWT_SECRET", "")
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_DSN", sqliteDSN(t))

	app, err := xbc.New(options()...)
	if err != nil {
		t.Fatalf("xbc.New() error = %v", err)
	}

	code, err := app.Execute(context.Background(), []string{"validate", "--config", "application.yml"})
	if err == nil && code == 0 {
		t.Fatal("validate accepted the shipped configuration with no signing secret")
	}
}

// TestProductionServiceServesItsDocumentedPolicy boots the shipped composition
// against a generated configuration and asserts the behavior the example
// documents: a public route anyone may call, routes that require a credential,
// a policy that distinguishes two authenticated subjects, an owner-scoped
// lookup that hides another subject's order, a scrape endpoint that accepts
// only the service credential, and a clean shutdown.
func TestProductionServiceServesItsDocumentedPolicy(t *testing.T) {
	const (
		issuer = "xbc-production-test"
		secret = "production-test-signing-secret-0123456789"
	)
	apiKey := "production-test-api-key-" + strings.Repeat("k", 24)

	address := freeAddress(t)
	configPath := writeTestConfig(t, address, secret, issuer, apiKey)

	app, err := xbc.New(options()...)
	if err != nil {
		t.Fatalf("xbc.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := app.Execute(ctx, []string{"--config", configPath, "--migrate"})
		done <- err
	}()
	waitForReadiness(t, "http://"+address+"/api/v1/readyz", done)

	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + address + "/api/v1"
	alice := mintToken(t, secret, issuer, "alice")
	bob := mintToken(t, secret, issuer, "bob")

	// The public route answers without any credential at all.
	if status, body := call(t, client, http.MethodGet, base+"/version", "", ""); status != http.StatusOK {
		t.Fatalf("GET /version without credentials = %d (%s), want 200", status, body)
	} else if !strings.Contains(string(body), "xbc-production-example") {
		t.Fatalf("GET /version body = %s, want the configured service name", body)
	}

	// An undeclared route is covered by web.security.default (deny), so it
	// requires authentication rather than being reachable anonymously.
	for _, path := range []string{"/orders", "/metrics"} {
		if status, body := call(t, client, http.MethodGet, base+path, "", ""); status != http.StatusUnauthorized {
			t.Fatalf("GET %s without credentials = %d (%s), want 401", path, status, body)
		}
	}

	// Two subjects, one policy: bob authenticates and is still refused the
	// write, which is the difference between authentication and authorization.
	if status, body := call(t, client, http.MethodGet, base+"/orders", bob, ""); status != http.StatusOK {
		t.Fatalf("GET /orders as bob = %d (%s), want 200", status, body)
	}
	if status, body := call(t, client, http.MethodPost, base+"/orders", bob, `{"item":"widget","quantity":1}`); status != http.StatusForbidden {
		t.Fatalf("POST /orders as bob = %d (%s), want 403", status, body)
	}

	createdStatus, createdBody := call(t, client, http.MethodPost, base+"/orders", alice, `{"item":"widget","quantity":3}`)
	if createdStatus != http.StatusCreated {
		t.Fatalf("POST /orders as alice = %d (%s), want 201", createdStatus, createdBody)
	}
	// The 201 is also the proof that the outbox write committed: the handler
	// runs the order insert and the event insert inside one transaction and
	// returns the database error if either fails, so a status here means both
	// rows are durable, not just the one this response describes.
	var order struct {
		ID       string `json:"id"`
		Item     string `json:"item"`
		Quantity int    `json:"quantity"`
	}
	if err := json.Unmarshal(createdBody, &order); err != nil {
		t.Fatalf("decode created order %s: %v", createdBody, err)
	}
	if order.ID == "" || order.Item != "widget" || order.Quantity != 3 {
		t.Fatalf("created order = %+v, want the submitted item and quantity with a generated id", order)
	}
	// The identifier is what a client can enumerate, so it must not be a
	// sequence: a small integer would let any caller walk the table.
	if len(order.ID) != 32 {
		t.Fatalf("order id %q has length %d, want a 128-bit hex identifier", order.ID, len(order.ID))
	}

	// The owner is part of the lookup, so another authenticated subject cannot
	// distinguish this order from one that does not exist.
	if status, body := call(t, client, http.MethodGet, base+"/orders/"+order.ID, bob, ""); status != http.StatusNotFound {
		t.Fatalf("GET /orders/%s as bob = %d (%s), want 404", order.ID, status, body)
	}
	if status, body := call(t, client, http.MethodGet, base+"/orders/"+order.ID, alice, ""); status != http.StatusOK {
		t.Fatalf("GET /orders/%s as alice = %d (%s), want 200", order.ID, status, body)
	}

	// The administrative route is narrowed to the token scheme by a tier-1
	// web.security rule, and then authorized by the policy.
	if status, body := call(t, client, http.MethodGet, base+"/admin/orders/stats", bob, ""); status != http.StatusForbidden {
		t.Fatalf("GET /admin/orders/stats as bob = %d (%s), want 403", status, body)
	}
	statsStatus, statsBody := call(t, client, http.MethodGet, base+"/admin/orders/stats", alice, "")
	if statsStatus != http.StatusOK {
		t.Fatalf("GET /admin/orders/stats as alice = %d (%s), want 200", statsStatus, statsBody)
	}
	if !strings.Contains(string(statsBody), `"orders":1`) {
		t.Fatalf("stats = %s, want one stored order", statsBody)
	}

	// The scrape endpoint takes the service credential and refuses the user
	// token: both are authenticated, but the policy says which one belongs
	// here.
	if status, body := call(t, client, http.MethodGet, base+"/metrics", alice, ""); status != http.StatusUnauthorized {
		t.Fatalf("GET /metrics with a JWT = %d (%s), want 401 (the rule selects apikey)", status, body)
	}
	if status, body := callWithAPIKey(t, client, http.MethodGet, base+"/metrics", apiKey); status != http.StatusOK {
		t.Fatalf("GET /metrics with an API key = %d (%s), want 200", status, body)
	}

	// Cancelling the context is the same path SIGTERM takes: readiness flips,
	// traffic stops, and Execute returns once the reverse-order shutdown
	// completes.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned error %v, want a clean exit", err)
		}
	case <-time.After(startWait):
		t.Fatal("timed out waiting for a clean shutdown")
	}
}

// TestServiceStartsFromEnvironmentAlone boots the shipped composition with no
// configuration file anywhere: every setting below arrives as an environment
// variable, in the exact name the environment layer derives from its
// configuration path. This is the container's run mode, where the image
// supplies the settings that describe the process and the deployment supplies
// the credential, and it is what lets the same binary be configured by a file
// on a workstation and by a scheduler in a cluster.
//
// Two capabilities are deliberately absent: the API-key scheme, because a list
// of credentials is not something the environment layer can express, and the
// authorization policy, which this run does not need. The assertions below
// record that posture rather than working around it.
func TestServiceStartsFromEnvironmentAlone(t *testing.T) {
	const (
		issuer = "xbc-production-env"
		secret = "production-env-signing-secret-0123456789"
	)
	address := freeAddress(t)

	// The container's working directory holds no configuration, so this test
	// runs in one too: with nothing named application.yml beneath the working
	// directory, a setting can only have come from the environment.
	t.Chdir(t.TempDir())

	for name, value := range map[string]string{
		"XBC_WEB_ADDR":                     address,
		"XBC_WEB_BASE_PATH":                "/api/v1",
		"XBC_PLUGINS_JWT_SECRET":           secret,
		"XBC_PLUGINS_JWT_ISSUER":           issuer,
		"XBC_PLUGINS_GORM_DEFAULT_DRIVER":  "sqlite",
		"XBC_PLUGINS_GORM_DEFAULT_DSN":     sqliteDSN(t),
		"XBC_PLUGINS_OUTBOX_MIGRATE":       "true",
		"XBC_PLUGINS_METRICS_HTTP_ENABLED": "true",
		"XBC_AUTO_MIGRATE":                 "true",
		"XBC_LOG_LEVEL":                    "warn",
	} {
		t.Setenv(name, value)
	}

	app, err := xbc.New(options()...)
	if err != nil {
		t.Fatalf("xbc.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// No arguments at all: even the migration request above is a
		// configuration value, not a flag.
		_, err := app.Execute(ctx, nil)
		done <- err
	}()

	base := "http://" + address + "/api/v1"
	waitForReadiness(t, base+"/readyz", done)

	client := &http.Client{Timeout: 5 * time.Second}
	alice := mintToken(t, secret, issuer, "alice")

	if status, body := call(t, client, http.MethodGet, base+"/version", "", ""); status != http.StatusOK {
		t.Fatalf("GET /version = %d (%s), want 200", status, body)
	}
	if status, body := call(t, client, http.MethodGet, base+"/orders", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("GET /orders without credentials = %d (%s), want 401", status, body)
	}
	// The credential the scheme needs is a list, which the environment layer
	// cannot express, so apikey stayed dormant: this header is not a credential
	// in this run, and the request is anonymous.
	if status, body := callWithAPIKey(t, client, http.MethodGet, base+"/orders", "any-key-at-all"); status != http.StatusUnauthorized {
		t.Fatalf("GET /orders with an API key = %d (%s), want 401 (apikey is not configured)", status, body)
	}

	// The database and the outbox are active, and the schema came from
	// xbc.auto_migrate rather than from a flag: a write that had nowhere to go
	// would answer 500 here.
	createdStatus, createdBody := call(t, client, http.MethodPost, base+"/orders", alice, `{"item":"widget","quantity":2}`)
	if createdStatus != http.StatusCreated {
		t.Fatalf("POST /orders as alice = %d (%s), want 201", createdStatus, createdBody)
	}

	// Authorization is the one layer this run does not carry: with no
	// casbin-http section the authorization middleware is absent, so an
	// authenticated subject reaches the administrative route. A deployment that
	// needs the distinction configures it, in a file or by composing the
	// middleware's own section.
	if status, body := call(t, client, http.MethodGet, base+"/admin/orders/stats", alice, ""); status != http.StatusOK {
		t.Fatalf("GET /admin/orders/stats as alice = %d (%s), want 200 (no policy is configured)", status, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned error %v, want a clean exit", err)
		}
	case <-time.After(startWait):
		t.Fatal("timed out waiting for a clean shutdown")
	}
}

// call sends one request carrying a Bearer token when token is set, and no
// credential at all when it is empty.
func call(t *testing.T, client *http.Client, method, url, token, body string) (int, []byte) {
	t.Helper()

	request := newRequest(t, method, url, body)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return do(t, client, request)
}

// callWithAPIKey sends one request carrying the API key in the header the
// shipped configuration names, which is the other way a client reaches this
// service.
func callWithAPIKey(t *testing.T, client *http.Client, method, url, apiKey string) (int, []byte) {
	t.Helper()

	request := newRequest(t, method, url, "")
	request.Header.Set("X-API-Key", apiKey)
	return do(t, client, request)
}

func newRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func do(t *testing.T, client *http.Client, request *http.Request) (int, []byte) {
	t.Helper()

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", request.Method, request.URL, err)
	}
	return response.StatusCode, payload
}

func mintToken(t *testing.T, secret, issuer, subject string) string {
	t.Helper()

	issuerPlugin, err := jwtplugin.New(jwtplugin.Config{Secret: secret, Issuer: issuer})
	if err != nil {
		t.Fatalf("build token issuer: %v", err)
	}
	token, err := issuerPlugin.Sign(subject, nil)
	if err != nil {
		t.Fatalf("sign token for %s: %v", subject, err)
	}
	return token
}

func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return address
}

func sqliteDSN(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "production.db")
	return "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL"
}

// writeTestConfig writes a configuration whose every value the test chooses:
// a free port, a generated signing secret, a digest computed from the key the
// test will present, and a database inside the test's own temporary directory.
// It mirrors application.yml key for key, which is what keeps the assertions
// above a statement about the shipped configuration and not about a second
// arrangement that happens to work.
func writeTestConfig(t *testing.T, address, secret, issuer, apiKey string) string {
	t.Helper()

	contents := fmt.Sprintf(`xbc:
  shutdown_timeout: 10s

log:
  level: warn
  console:
    enabled: false

web:
  addr: %[1]q
  base_path: "/api/v1"
  shutdown:
    pre_drain_delay: 0s
  security:
    default: deny
    schemes: ["jwt", "apikey"]
    policies:
      - match: "/api/v1/admin/**"
        authenticate: [jwt]
      - match: "/api/v1/metrics"
        authenticate: [apikey]

plugins:
  jwt:
    secret: %[2]q
    issuer: %[3]q
  apikey:
    static:
      - id: "test-scraper"
        subject: "prometheus"
        sha256: %[4]q
  casbin:
    request_convention: path_method
    policy: |
      p, admin, /api/v1/orders, GET
      p, admin, /api/v1/orders, POST
      p, admin, /api/v1/orders/:id, GET
      p, admin, /api/v1/admin/orders/stats, GET
      p, prometheus, /api/v1/metrics, GET
      g, alice, admin

      p, viewer, /api/v1/orders, GET
      p, viewer, /api/v1/orders/:id, GET
      g, bob, viewer
  casbin-http:
    missing_permission: deny
  gorm:
    default:
      driver: sqlite
      dsn: %[5]q
      max_open_conn: 1
      max_idle_conn: 1
  outbox:
    migrate: true
    worker:
      enabled: false
  metrics:
    http:
      enabled: true
`, address, secret, issuer, apikey.HashKey(apiKey).String(), sqliteDSN(t))

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write test configuration: %v", err)
	}
	return path
}

func waitForReadiness(t *testing.T, url string, done <-chan error) {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("the process exited before becoming ready: %v", err)
		default:
		}
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", url)
}
