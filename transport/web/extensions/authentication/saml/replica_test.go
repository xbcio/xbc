package saml

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

// newReplicaAdapter builds a second adapter instance on its own transaction
// store, sharing everything else the two replicas would share in a deployment:
// the provider registry, the resolver, the session backend, and the clock.
func newReplicaAdapter(t *testing.T, source *adapterHarness, store federationweb.Store) *Plugin {
	t.Helper()
	login, err := federationweb.New(
		source.federationConfig,
		source.resolver,
		source.sessions,
		testSessionCookie,
		federationweb.WithStore(store),
		federationweb.WithClock(source.clock.Now),
	)
	if err != nil {
		t.Fatalf("federation.New: %v", err)
	}
	adapter, err := New(DefaultConfig(), source.registry, login, WithLogger(source.logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return adapter
}

// TestLoginStartedOnOneReplicaCompletesOnAnother pins the property that makes
// the Redis backend the default: the start request and the assertion consumer
// service may land on different replicas. Both stores here are distinct values
// over the same Redis server, which is exactly what two processes share.
func TestLoginStartedOnOneReplicaCompletesOnAnother(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	storeA, err := federationweb.NewRedisStore(client, "test:fed:")
	if err != nil {
		t.Fatalf("NewRedisStore(a): %v", err)
	}
	storeB, err := federationweb.NewRedisStore(client, "test:fed:")
	if err != nil {
		t.Fatalf("NewRedisStore(b): %v", err)
	}

	harness := newHarness(t, func(options *harnessOptions) {
		options.store = storeA
	})
	replica := newReplicaAdapter(t, harness, storeB)

	state, binding, requestID, _ := harness.start(t, "/dashboard")
	response := buildSignedResponse(t, harness.clock.Now(), requestID, nil)
	form := url.Values{
		"SAMLResponse": {base64.StdEncoding.EncodeToString(response)},
		"RelayState":   {state},
	}

	// The callback lands on the other replica, with the same browser and the
	// RelayState the first replica issued.
	ctx, recorder := newACSRequestCtx(t, testProvider, form, binding)
	if err := replica.acs(context.Background(), ctx); err != nil {
		t.Fatalf("acs handler error = %v", err)
	}
	if status := statusOf(ctx); status != http.StatusFound {
		t.Fatalf("acs status = %d, want 302 (location %q)", status, locationOf(recorder))
	}
	if location := locationOf(recorder); location != "/dashboard" {
		t.Fatalf("acs on the other replica landed on %q, want /dashboard", location)
	}
	if got := countOperations(harness.sessions.operations(), "create"); got != 1 {
		t.Fatalf("session creates = %d, want 1", got)
	}
}
