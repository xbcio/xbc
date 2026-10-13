package kafka

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// fakeResolver is the ConnectionResolver test double. Every call is recorded,
// so a test can assert how often resolution happened, what the instance asked
// for, and whether the call ran under a deadline. In blocking mode the call
// parks until the context or the release channel ends it, which is what the
// deadline test leans on to avoid a hung mutant.
type fakeResolver struct {
	mu sync.Mutex

	calls       int
	requests    []ConnectionRequest
	deadlineSet bool
	ctxErr      error

	conn Connection
	err  error

	blocking bool
	release  <-chan struct{}
}

func (r *fakeResolver) ResolveConnection(ctx context.Context, request ConnectionRequest) (Connection, error) {
	r.mu.Lock()
	r.calls++
	r.requests = append(r.requests, request)
	if _, ok := ctx.Deadline(); ok {
		r.deadlineSet = true
	}
	blocking, release := r.blocking, r.release
	conn, err := r.conn, r.err
	r.mu.Unlock()
	if !blocking {
		return conn, err
	}
	select {
	case <-ctx.Done():
		r.mu.Lock()
		r.ctxErr = ctx.Err()
		r.mu.Unlock()
		return Connection{}, ctx.Err()
	case <-release:
		return conn, err
	}
}

func (r *fakeResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *fakeResolver) lastRequest() ConnectionRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return ConnectionRequest{}
	}
	return r.requests[len(r.requests)-1]
}

func (r *fakeResolver) sawDeadline() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadlineSet
}

func (r *fakeResolver) observedContextError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctxErr
}

// TestResolveConnectionDisabledNeverConsultsTheResolver pins the default-off
// contract: an instance that does not enable resolve_connection uses its
// configured broker list as-is, even when the resolver input is selected and a
// resolver is available, and preparation never reaches one either.
func TestResolveConnectionDisabledNeverConsultsTheResolver(t *testing.T) {
	resolver := &fakeResolver{conn: Connection{
		Brokers: []string{"resolved.internal:9093"},
		SASL:    SASLConfig{Mechanism: SASLSCRAMSHA512, Username: "resolved", Password: "resolved-secret"},
	}}
	factory := &fakeFactory{writer: &fakeWriter{}}
	client, err := newManagedClient(validConfig(), factory, resolver, "")
	if err != nil {
		t.Fatalf("newManagedClient() error = %v", err)
	}
	factory.mu.Lock()
	brokers := append([]string(nil), factory.writerConfig.Brokers...)
	sasl := factory.writerConfig.SASL
	factory.mu.Unlock()
	if !reflect.DeepEqual(brokers, []string{"127.0.0.1:9092"}) || sasl != (SASLConfig{}) {
		t.Fatalf("writer connection = %v, %+v, want the configured values", brokers, sasl)
	}
	if _, err := prepareConfig(validConfig()); err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	if calls := resolver.callCount(); calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 with resolve_connection off", calls)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestPreparationAcceptsOpaqueBrokerReferencesOnlyWhenEnabled pins the
// preparation split: with resolve_connection enabled the brokers value is a
// resolver reference, so only emptiness is checked; without it the value is
// still a broker list and must be host:port.
func TestPreparationAcceptsOpaqueBrokerReferencesOnlyWhenEnabled(t *testing.T) {
	const reference = "https://indatahub.example.com:8443/cluster/42"
	enabled := validConfig()
	enabled.Brokers = []string{reference}
	enabled.ResolveConnection = true
	prepared, err := prepareConfig(enabled)
	if err != nil {
		t.Fatalf("prepareConfig(enabled) error = %v", err)
	}
	if !reflect.DeepEqual(prepared.Brokers, []string{reference}) {
		t.Fatalf("prepared brokers = %v, want the reference unchanged", prepared.Brokers)
	}

	disabled := validConfig()
	disabled.Brokers = []string{reference}
	if _, err := prepareConfig(disabled); err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("prepareConfig(disabled) error = %v, want a host:port rejection", err)
	}

	empty := enabled
	empty.Brokers = []string{"  "}
	if _, err := prepareConfig(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("prepareConfig(empty reference) error = %v, want an emptiness rejection", err)
	}
}

// TestDoctorPreparationNeverReachesTheResolver pins the doctor path: it runs
// Prepare only, which takes no resolver and performs no network call. An
// enabled instance with an opaque reference prepares successfully while every
// other rule still applies, and a resolver in scope stays untouched.
func TestDoctorPreparationNeverReachesTheResolver(t *testing.T) {
	resolver := &fakeResolver{conn: Connection{Brokers: []string{"resolved.internal:9093"}}}
	cfg := validConfig()
	cfg.Brokers = []string{"https://indatahub.example.com:8443/cluster/42"}
	cfg.ResolveConnection = true
	if _, err := prepareConfig(cfg); err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	broken := cfg
	broken.Consumers = map[string]ConsumerConfig{"jobs": {}}
	if _, err := prepareConfig(broken); err == nil || !strings.Contains(err.Error(), "group_id") {
		t.Fatalf("prepareConfig(invalid consumer) error = %v, want a group_id rejection", err)
	}
	if calls := resolver.callCount(); calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 on the doctor path", calls)
	}
}

// TestEnabledResolutionWithoutAResolverFailsConstruction pins the failure mode
// of an enabled instance whose composition selected no
// kafka.ConnectionResolver: construction stops before any backend is built and
// the error names the instance and the contract.
func TestEnabledResolutionWithoutAResolverFailsConstruction(t *testing.T) {
	cfg := validConfig()
	cfg.Brokers = []string{"https://indatahub.example.com:8443/cluster/42"}
	cfg.ResolveConnection = true
	factory := &fakeFactory{writer: &fakeWriter{}}
	if _, err := newManagedClient(cfg, factory, nil, "events"); err == nil ||
		!strings.Contains(err.Error(), "events") || !strings.Contains(err.Error(), "ConnectionResolver") {
		t.Fatalf("newManagedClient() error = %v, want the instance name and the contract name", err)
	}
	if _, err := newManagedClient(cfg, factory, nil, ""); err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("newManagedClient(default) error = %v, want the default instance label", err)
	}
	factory.mu.Lock()
	built := len(factory.writerConfig.Brokers)
	factory.mu.Unlock()
	if built != 0 {
		t.Fatal("a backend was built before the instance's connection was resolved")
	}
}

// TestResolvedConnectionReplacesBrokersAndSASLForEveryBackend pins the
// replacement contract: the resolver is called exactly once with the
// configured instance, reference, and SASL block; the producer, every consumer
// reader, and the readiness probe that pings the producer are then built from
// the resolved brokers and SASL; and TLS stays as configured.
func TestResolvedConnectionReplacesBrokersAndSASLForEveryBackend(t *testing.T) {
	const (
		configuredPassword = "acl-account-secret"
		resolvedPassword   = "scram-resolved-secret"
	)
	resolvedBrokers := []string{"broker-1.internal:9092", "broker-2.internal:9092"}
	resolvedSASL := SASLConfig{Mechanism: SASLSCRAMSHA512, Username: "resolved-user", Password: resolvedPassword}
	cfg := validConfig()
	cfg.Brokers = []string{"https://indatahub.example.com:8443/cluster/42"}
	cfg.ResolveConnection = true
	cfg.TLS.Enabled = true
	cfg.SASL = SASLConfig{Mechanism: SASLPlain, Username: "acl-account", Password: configuredPassword}
	cfg.Consumers = map[string]ConsumerConfig{"jobs": consumerConfig(ErrorPolicyStop)}
	resolver := &fakeResolver{conn: Connection{Brokers: resolvedBrokers, SASL: resolvedSASL}}
	factory := &fakeFactory{
		writer:    &fakeWriter{},
		readers:   map[string]messageReader{"jobs": newFakeReader()},
		readerErr: map[string]error{},
	}
	client, err := newManagedClient(cfg, factory, resolver, "events")
	if err != nil {
		t.Fatalf("newManagedClient() error = %v", err)
	}
	if calls := resolver.callCount(); calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1", calls)
	}
	request := resolver.lastRequest()
	if request.Instance != "events" || !reflect.DeepEqual(request.Brokers, cfg.Brokers) || request.SASL != cfg.SASL {
		t.Fatalf("resolver request = %+v, want the configured instance, reference, and SASL", request)
	}

	factory.mu.Lock()
	writerConfig := factory.writerConfig
	factory.mu.Unlock()
	if !reflect.DeepEqual(writerConfig.Brokers, resolvedBrokers) || writerConfig.SASL != resolvedSASL || !writerConfig.TLS.Enabled {
		t.Fatalf("writer connection = %v, %+v, tls=%v, want the resolved values with the configured TLS",
			writerConfig.Brokers, writerConfig.SASL, writerConfig.TLS.Enabled)
	}
	// The readiness probe pings the writer the client owns, so the effective
	// configuration asserted here is also the one the probe reaches the cluster
	// with.
	if !reflect.DeepEqual(client.normalized.Brokers, resolvedBrokers) || client.normalized.SASL != resolvedSASL {
		t.Fatalf("client connection = %v, %+v", client.normalized.Brokers, client.normalized.SASL)
	}

	host := newFakeHost()
	defer host.close()
	if err := client.RegisterHandler("jobs", HandlerFunc(func(context.Context, Message) error { return nil })); err != nil {
		t.Fatal(err)
	}
	if err := client.start(runtimeContext(host, "events")); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	factory.mu.Lock()
	readerConfig, ok := factory.readerConfigs["jobs"]
	factory.mu.Unlock()
	if !ok || !reflect.DeepEqual(readerConfig.Brokers, resolvedBrokers) || readerConfig.SASL != resolvedSASL {
		t.Fatalf("reader connection = %v, %+v, want the resolved values", readerConfig.Brokers, readerConfig.SASL)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestResolverOutputRunsThroughFullNormalization pins that a resolved
// connection is re-validated exactly like configuration -- the same broker and
// SASL rules apply -- that a resolver's own error keeps the instance identity,
// and that no variant of the construction failure renders a password.
func TestResolverOutputRunsThroughFullNormalization(t *testing.T) {
	const (
		configuredPassword = "acl-account-secret"
		resolvedPassword   = "scram-resolved-secret"
	)
	sentinel := errors.New("control plane unavailable")
	enabled := func() Config {
		cfg := validConfig()
		cfg.Brokers = []string{"https://indatahub.example.com:8443/cluster/42"}
		cfg.ResolveConnection = true
		cfg.SASL = SASLConfig{Mechanism: SASLSCRAMSHA256, Username: "acl-account", Password: configuredPassword}
		return cfg
	}
	tests := []struct {
		name    string
		resolve func(*fakeResolver)
		want    string
		isErr   error
	}{
		{
			name:    "resolved brokers must be host:port",
			resolve: func(r *fakeResolver) { r.conn = Connection{Brokers: []string{"broker.internal"}} },
			want:    "host:port",
		},
		{
			name: "resolved plain sasl still requires tls",
			resolve: func(r *fakeResolver) {
				r.conn = Connection{
					Brokers: []string{"broker.internal:9092"},
					SASL:    SASLConfig{Mechanism: SASLPlain, Username: "resolved", Password: resolvedPassword},
				}
			},
			want: "requires tls",
		},
		{
			name:    "resolver errors keep the instance identity",
			resolve: func(r *fakeResolver) { r.err = sentinel },
			want:    "events",
			isErr:   sentinel,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeResolver{}
			test.resolve(resolver)
			factory := &fakeFactory{writer: &fakeWriter{}}
			_, err := newManagedClient(enabled(), factory, resolver, "events")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newManagedClient() error = %v, want substring %q", err, test.want)
			}
			if test.isErr != nil && !errors.Is(err, test.isErr) {
				t.Fatalf("newManagedClient() error = %v, want the resolver's error in the chain", err)
			}
			if strings.Contains(err.Error(), configuredPassword) || strings.Contains(err.Error(), resolvedPassword) {
				t.Fatalf("construction error leaked a SASL password: %v", err)
			}
			factory.mu.Lock()
			built := len(factory.writerConfig.Brokers)
			factory.mu.Unlock()
			if built != 0 {
				t.Fatal("a backend was built from an invalid resolved connection")
			}
		})
	}
}

// TestResolveDeadlineBoundsAResolverCall pins that the resolver always runs
// under a deadline derived from resolve_timeout: a resolver that never returns
// on its own is cut off, the construction failure carries the deadline, and no
// backend is built.
func TestResolveDeadlineBoundsAResolverCall(t *testing.T) {
	release := make(chan struct{})
	resolver := &fakeResolver{
		conn:     Connection{Brokers: []string{"broker.internal:9092"}},
		blocking: true,
		release:  release,
	}
	cfg := validConfig()
	cfg.Brokers = []string{"https://indatahub.example.com:8443/cluster/42"}
	cfg.ResolveConnection = true
	cfg.ResolveTimeout = 50 * time.Millisecond
	factory := &fakeFactory{writer: &fakeWriter{}}
	done := make(chan error, 1)
	go func() {
		_, err := newManagedClient(cfg, factory, resolver, "events")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("newManagedClient() error = %v, want a deadline while the resolver blocks", err)
		}
		if !resolver.sawDeadline() {
			t.Fatal("resolver context carried no deadline")
		}
		if observed := resolver.observedContextError(); !errors.Is(observed, context.DeadlineExceeded) {
			t.Fatalf("resolver observed %v, want deadline exceeded", observed)
		}
	case <-time.After(2 * time.Second):
		// Release the resolver so a mutant without a deadline can fail the
		// test instead of hanging it, and so the goroutine does not leak.
		close(release)
		<-done
		t.Fatal("resolution did not return within its configured bound")
	}
	factory.mu.Lock()
	built := len(factory.writerConfig.Brokers)
	factory.mu.Unlock()
	if built != 0 {
		t.Fatal("a backend was built after the resolution deadline expired")
	}
}

// TestResolveTimeoutFollowsTheEffectiveDialTimeout pins the defaulting rule:
// an unset resolve_timeout derives from the effective dial_timeout, an
// explicit value wins, and a non-positive value is rejected.
func TestResolveTimeoutFollowsTheEffectiveDialTimeout(t *testing.T) {
	prepared, err := prepareConfig(validConfig())
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	if prepared.ResolveTimeout != 10*time.Second || prepared.ResolveTimeout != prepared.DialTimeout {
		t.Fatalf("resolve_timeout = %s, dial_timeout = %s, want the derived dial bound", prepared.ResolveTimeout, prepared.DialTimeout)
	}
	custom := validConfig()
	custom.DialTimeout = 3 * time.Second
	prepared, err = prepareConfig(custom)
	if err != nil {
		t.Fatalf("prepareConfig(custom) error = %v", err)
	}
	if prepared.ResolveTimeout != 3*time.Second {
		t.Fatalf("resolve_timeout = %s, want the effective dial_timeout", prepared.ResolveTimeout)
	}
	explicit := custom
	explicit.ResolveTimeout = time.Second
	prepared, err = prepareConfig(explicit)
	if err != nil {
		t.Fatalf("prepareConfig(explicit) error = %v", err)
	}
	if prepared.ResolveTimeout != time.Second {
		t.Fatalf("resolve_timeout = %s, want the explicit value", prepared.ResolveTimeout)
	}
	negative := validConfig()
	negative.ResolveTimeout = -time.Second
	if _, err := prepareConfig(negative); err == nil || !strings.Contains(err.Error(), "resolve_timeout") {
		t.Fatalf("prepareConfig(negative) error = %v, want a resolve_timeout rejection", err)
	}
}

// ── assembly-level wiring ──────────────────────────────────

// resolverProbe is the application-side plugin this file exports as a
// ConnectionResolver, the way the ACL control plane's own plugin does in the
// SAS deployment. It records every request and answers with resolved values.
type resolverProbe struct {
	mu       sync.Mutex
	requests []ConnectionRequest
	conn     Connection
}

func (p *resolverProbe) ResolveConnection(_ context.Context, request ConnectionRequest) (Connection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	return p.conn, nil
}

func (p *resolverProbe) recorded() []ConnectionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ConnectionRequest(nil), p.requests...)
}

var resolverProbeDefinition = plugin.Define(
	"resolver-probe",
	func(plugin.BuildContext) (*resolverProbe, error) {
		return &resolverProbe{conn: Connection{
			Brokers: []string{"resolved-1.internal:9092"},
			SASL:    SASLConfig{Mechanism: SASLSCRAMSHA512, Username: "resolved", Password: "resolved-secret"},
		}}, nil
	},
	plugin.Options[*resolverProbe]{
		Exports: plugin.Contracts(plugin.ExportAs(func(p *resolverProbe) ConnectionResolver { return p })),
	},
)

// TestAssemblyResolvesTheConfiguredReferenceForANamedInstance guards the
// Definition's own wiring, which the direct construction tests cannot reach:
// the factory must hand the configured instance name and the selected
// resolver to newManagedClient. The configured value is an opaque URL and the
// resolver another plugin's export, exactly the SAS shape; the resolved
// values must be the ones the client is actually built from.
func TestAssemblyResolvesTheConfiguredReferenceForANamedInstance(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"kafka": map[string]any{
				"events": map[string]any{
					"brokers":            []string{"https://indatahub.example.com:8443/cluster/42"},
					"resolve_connection": true,
				},
			},
		},
	}, "XBC_KAFKA_RESOLVE_TEST_")
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle(), plugin.BundleOf(resolverProbeDefinition)},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: "events"})
	if !ok {
		t.Fatal("kafka instance events was not constructed")
	}
	client, ok := instance.Primary().(*Client)
	if !ok {
		t.Fatalf("kafka instance primary = %T", instance.Primary())
	}
	probeInstance, ok := constructed.Instance(plugin.Identity{Plugin: "resolver-probe"})
	if !ok {
		t.Fatal("resolver probe was not constructed")
	}
	probe, ok := probeInstance.Primary().(*resolverProbe)
	if !ok {
		t.Fatalf("resolver probe primary = %T", probeInstance.Primary())
	}

	requests := probe.recorded()
	if len(requests) != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1", len(requests))
	}
	if requests[0].Instance != "events" {
		t.Fatalf("resolver request instance = %q, want the configured instance name", requests[0].Instance)
	}
	if !reflect.DeepEqual(requests[0].Brokers, []string{"https://indatahub.example.com:8443/cluster/42"}) {
		t.Fatalf("resolver request brokers = %v, want the configured reference", requests[0].Brokers)
	}
	if !reflect.DeepEqual(client.normalized.Brokers, []string{"resolved-1.internal:9092"}) ||
		client.normalized.SASL.Mechanism != SASLSCRAMSHA512 {
		t.Fatalf("client connection = %v, %+v, want the resolved values", client.normalized.Brokers, client.normalized.SASL)
	}
}
