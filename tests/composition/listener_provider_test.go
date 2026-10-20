package composition_test

// The listener-provider composition cases live here rather than in
// transport/web because that module's architecture guard forbids its own
// packages -- external tests included -- from reverse importing the root
// facade. These cases are about the real runtime: the provider export has to
// reach web's OptionalOne input, and `validate` must never ask it for a
// listener.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// recordingListenerProvider records every address the runtime asks for and
// every listener it hands back, so a test can assert both that a validate run
// asks for none and that a boot serves the one it was given.
type recordingListenerProvider struct {
	mu     sync.Mutex
	asked  []string
	bound  []net.Listener
	listen func(address string) (net.Listener, error)
}

func (p *recordingListenerProvider) Listen(address string) (net.Listener, error) {
	p.mu.Lock()
	p.asked = append(p.asked, address)
	p.mu.Unlock()

	ln, err := p.listen(address)
	if ln != nil {
		p.mu.Lock()
		p.bound = append(p.bound, ln)
		p.mu.Unlock()
	}
	return ln, err
}

func (p *recordingListenerProvider) addresses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

func (p *recordingListenerProvider) listeners() []net.Listener {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]net.Listener(nil), p.bound...)
}

// providerTestEngine satisfies web.Engine for a validate run, which assembles
// the pipeline but never registers a route or serves a request.
type providerTestEngine struct{}

func (providerTestEngine) Handle(string, string, []web.Handler) {}
func (providerTestEngine) Mount(string, string, []web.Handler)  {}
func (providerTestEngine) NoRoute([]web.Handler)                {}
func (providerTestEngine) NoMethod([]web.Handler)               {}
func (providerTestEngine) Serve(net.Listener) error             { return nil }
func (providerTestEngine) Shutdown(context.Context) error       { return nil }

type providerTestFactory struct{}

func (providerTestFactory) NewEngine(web.Options) (web.Engine, error) {
	return providerTestEngine{}, nil
}

func providerTestEngineBundle() plugin.Bundle {
	definition := plugin.Define(
		"listener-provider-test-engine",
		func(plugin.BuildContext) (providerTestFactory, error) { return providerTestFactory{}, nil },
		plugin.Options[providerTestFactory]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.EngineFactory](func(value providerTestFactory) web.EngineFactory { return value }),
			),
		},
	)
	return plugin.BundleOf(definition)
}

// staticListenerProvider gives plugin.Define a concrete primary type while the
// exported contract stays web.ListenerProvider. It must itself satisfy that
// interface: the framework binds the constructed primary value and uses the
// ExportAs witness only as a compile-time check.
type staticListenerProvider struct{ provider web.ListenerProvider }

func (p staticListenerProvider) Listen(address string) (net.Listener, error) {
	return p.provider.Listen(address)
}

func listenerProviderTestBundle(key string, provider web.ListenerProvider) plugin.Bundle {
	value := staticListenerProvider{provider: provider}
	definition := plugin.Define(
		plugin.Key(key),
		func(plugin.BuildContext) (staticListenerProvider, error) { return value, nil },
		plugin.Options[staticListenerProvider]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.ListenerProvider](func(value staticListenerProvider) web.ListenerProvider {
					return value.provider
				}),
			),
		},
	)
	return plugin.BundleOf(definition)
}

// writeListenerProviderValidateConfig writes the smallest configuration a
// validate run needs: Web's address and a logger that writes nothing.
func writeListenerProviderValidateConfig(t *testing.T) string {
	t.Helper()

	contents := `xbc:
  shutdown_timeout: 2s
log:
  console:
    enabled: false
web:
  addr: "127.0.0.1:0"
`
	path := filepath.Join(t.TempDir(), "application.yml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// resetGlobalLogger restores the process-wide log facade the runtime installs
// during Execute, so one composition test cannot observe another's logger.
func resetGlobalLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = corelog.Close()
		corelog.SetLogger(corelog.Nop())
	})
}

// TestListenerProviderReachesTheDefinitionInput is the composition half: a real
// xbc.New + web.Bundle() + engine plugin + provider plugin validates
// successfully, and the validate run never asks the provider for a listener,
// which is what proves the export reached the OptionalOne input.
func TestListenerProviderReachesTheDefinitionInput(t *testing.T) {
	resetGlobalLogger(t)

	provider := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		return nil, errors.New("validate must not listen")
	}}
	app, err := xbc.New(xbc.WithBundles(
		web.Bundle(),
		providerTestEngineBundle(),
		listenerProviderTestBundle("listener-provider", provider),
	))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), []string{"validate", "--config", writeListenerProviderValidateConfig(t)})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Empty(t, provider.addresses(), "validate must never ask the provider for a listener")
}

// TestTwoListenerProvidersFailAssembly pins the cardinality decision: the input
// is OptionalOne, so a second exporter is an assembly error rather than a race
// between boot orders.
func TestTwoListenerProvidersFailAssembly(t *testing.T) {
	resetGlobalLogger(t)

	first := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		return nil, errors.New("assembly must not construct a provider listener")
	}}
	second := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		return nil, errors.New("assembly must not construct a provider listener")
	}}
	app, err := xbc.New(xbc.WithBundles(
		web.Bundle(),
		providerTestEngineBundle(),
		listenerProviderTestBundle("listener-provider-a", first),
		listenerProviderTestBundle("listener-provider-b", second),
	))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), []string{"validate", "--config", writeListenerProviderValidateConfig(t)})
	require.Error(t, err)
	assert.Equal(t, 1, code)
	assert.ErrorContains(t, err, "is ambiguous")
	assert.ErrorContains(t, err, "ListenerProvider")
	assert.ErrorContains(t, err, "listener-provider-a")
	assert.ErrorContains(t, err, "listener-provider-b")
	assert.Empty(t, first.addresses())
	assert.Empty(t, second.addresses())
}

// listenerProviderBootConfig is the smallest configuration the boot case
// needs: a provider binds the real socket, and shutdown must finish well
// inside the test's bounded wait.
const listenerProviderBootConfig = `xbc:
  shutdown_timeout: 2s
log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: "127.0.0.1:0"
`

// recordingBootEngine is the engine a real boot serves through in this file.
// It reports the listener Serve received, then blocks until Shutdown closes
// that listener, so a test can observe both halves of the ownership transfer.
type recordingBootEngine struct {
	served chan net.Listener
	stop   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	listener net.Listener
}

func newRecordingBootEngine() *recordingBootEngine {
	return &recordingBootEngine{served: make(chan net.Listener, 1), stop: make(chan struct{})}
}

func (*recordingBootEngine) Handle(string, string, []web.Handler) {}
func (*recordingBootEngine) Mount(string, string, []web.Handler)  {}
func (*recordingBootEngine) NoRoute([]web.Handler)                {}
func (*recordingBootEngine) NoMethod([]web.Handler)               {}

func (e *recordingBootEngine) Serve(ln net.Listener) error {
	e.mu.Lock()
	e.listener = ln
	e.mu.Unlock()
	e.served <- ln
	<-e.stop
	return nil
}

func (e *recordingBootEngine) Shutdown(context.Context) error {
	e.once.Do(func() {
		e.mu.Lock()
		ln := e.listener
		e.mu.Unlock()
		if ln != nil {
			_ = ln.Close()
		}
		close(e.stop)
	})
	return nil
}

type recordingBootFactory struct{ engine *recordingBootEngine }

func (f recordingBootFactory) NewEngine(web.Options) (web.Engine, error) {
	return f.engine, nil
}

func recordingBootEngineBundle(engine *recordingBootEngine) plugin.Bundle {
	factory := recordingBootFactory{engine: engine}
	definition := plugin.Define(
		"listener-provider-boot-engine",
		func(plugin.BuildContext) (recordingBootFactory, error) { return factory, nil },
		plugin.Options[recordingBootFactory]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.EngineFactory](func(value recordingBootFactory) web.EngineFactory { return value }),
			),
		},
	)
	return plugin.BundleOf(definition)
}

// TestListenerProviderSuppliesTheBootListener is the case a validate run
// cannot see: the Definition has to store the optional entry on the Server, the
// provider has to be called with web.addr, the engine's Serve has to receive
// exactly the listener the provider returned, and shutdown has to close it.
func TestListenerProviderSuppliesTheBootListener(t *testing.T) {
	resetGlobalLogger(t)

	provider := &recordingListenerProvider{listen: func(address string) (net.Listener, error) {
		return net.Listen("tcp", address)
	}}
	engine := newRecordingBootEngine()
	run := dualBootApp(t, listenerProviderBootConfig,
		web.Bundle(),
		recordingBootEngineBundle(engine),
		listenerProviderTestBundle("listener-provider", provider),
	)

	var served net.Listener
	dualWaitFor(t, 10*time.Second, "the provider's listener to reach the engine's Serve", func() error {
		if len(provider.addresses()) == 0 {
			return errors.New("the provider has not been called")
		}
		select {
		case served = <-engine.served:
			return nil
		default:
			return errors.New("the engine has not begun serving")
		}
	})

	bound := provider.listeners()
	require.Len(t, bound, 1, "the provider must be called once for the serving plane")
	assert.Equal(t, []string{"127.0.0.1:0"}, provider.addresses(),
		"the provider must receive the configured web.addr")
	assert.Equal(t, bound[0].Addr().String(), served.Addr().String(),
		"the engine must serve exactly the listener the provider returned")

	run.cancel()
	result := run.wait(t, 20*time.Second)
	require.NoError(t, result.err)
	assert.Equal(t, 0, result.code)
	assert.ErrorIs(t, served.Close(), net.ErrClosed,
		"shutdown must close the provider-owned listener")
}
