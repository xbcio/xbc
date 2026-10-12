package management

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	stdpprof "net/http/pprof"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xbcio/xbc/plugin"
)

// Plugin serves the gRPC stack's operator endpoints on a listener of its own:
// the neutral registry's exposition when the metrics plugin is selected, and
// Go's profiling handlers when pprof is enabled. It deliberately carries no
// health route -- readiness is consulted by whatever routes traffic to this
// process, which is the serving port's job -- and no authentication: its
// access control is the bound address.
//
// The listener follows the Web transport's management plane end to end. It is
// bound in Start, serves only once the traffic gate releases, and is stopped
// last: the plugin opens no traffic and depends on none, so the runtime's
// shutdown walk passes it during the ingress and the drain phases and reaches
// it only in the final one. A scrape issued while business traffic is still
// finishing -- the window an operator watches a rollout through -- therefore
// lands, the same scrape still lands once the serving plane is gone and the
// drain phase is running, and the listener is closed while the remaining stops
// run, never after the process is gone.
type Plugin struct {
	config   Config
	gatherer prometheus.Gatherer
	handler  http.Handler

	mu      sync.Mutex
	started bool
	served  bool
	server  *http.Server
	ln      net.Listener
}

var (
	_ plugin.Runner = (*Plugin)(nil)
	_ plugin.Closer = (*Plugin)(nil)
)

// newHandler builds the management mux. The route set is exactly the operator
// endpoints this package owns, and each is conditional: /metrics exists only
// when a gatherer was injected, so a deployment that selects no metrics plugin
// answers 404 there rather than serving an empty exposition, and the pprof
// subtree exists only under the opt-in.
func newHandler(cfg Config, gatherer prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	if gatherer != nil {
		expose := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError})
		mux.HandleFunc("/metrics", func(writer http.ResponseWriter, request *http.Request) {
			// A scrape is a point-in-time reading; a cached one is a lie.
			writer.Header().Set("Cache-Control", "no-store")
			expose.ServeHTTP(writer, request)
		})
	}
	if cfg.Pprof {
		// The five registrations are the standard set net/http/pprof declares
		// itself: the index, which also dispatches the named profiles (heap,
		// goroutine, ...), and the four endpoints that are not profiles. Naming
		// them here is what keeps this listener's route set equal to the
		// documented standard one instead of a subset an operator would
		// discover by 404.
		mux.HandleFunc("/debug/pprof/", stdpprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", stdpprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", stdpprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", stdpprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", stdpprof.Trace)
	}
	return mux
}

// hasEndpoints reports whether the mux serves anything an operator can use.
func hasEndpoints(cfg Config, gatherer prometheus.Gatherer) bool {
	return gatherer != nil || cfg.Pprof
}

// Start binds the management listener and admits a serving task that waits on
// the same traffic gate as every ingress task, so the operator surface cannot
// answer a request before the runtime opens traffic -- a scrape must never
// observe a process that never finished starting.
//
// Binding here rather than at construction is what keeps construction
// side-effect-free; a bind failure is a Start failure, and the task is
// admitted only after the socket exists, so a rejected Start leaves nothing
// behind but the closed listener.
func (p *Plugin) Start(ctx *plugin.Context) error {
	if p == nil {
		return errors.New("grpc-management: Start requires a non-nil plugin")
	}
	if ctx == nil {
		return errors.New("grpc-management: Start requires a non-nil plugin context")
	}
	logger := ctx.Log()

	material, err := p.config.TLS.material()
	if err != nil {
		return err
	}
	address := p.config.bindAddr()
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("grpc-management: failed to listen on %s: %w", address, err)
	}
	// Wrapping the bound listener is what keeps TLS out of the handler set: it
	// terminates at the socket, and no handler asks whether the connection
	// beneath it speaks TLS.
	if material != nil {
		ln = tls.NewListener(ln, material)
	}

	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		_ = ln.Close()
		return errors.New("grpc-management: already started")
	}
	// The http.Server carries no read, write, or idle timeout on purpose. A
	// CPU profile is a response measured in tens of seconds, so a write
	// deadline would truncate exactly the request the listener exists for, and
	// the remaining timeouts defend against slow clients on a surface that is
	// loopback-only unless an operator has declared, in the configuration,
	// that it may be remote.
	server := &http.Server{Handler: p.handler}
	p.started = true
	p.server = server
	p.ln = ln
	p.mu.Unlock()

	if !hasEndpoints(p.config, p.gatherer) {
		logger.Warn("grpc-management: the management listener serves no endpoints; select the metrics plugin or set pprof: true, or remove the plugins.grpc-management section")
	}

	gate := ctx.TrafficGate()
	accepted := ctx.GoCritical(func(taskCtx context.Context) {
		select {
		case <-gate:
		case <-taskCtx.Done():
			return
		}

		p.mu.Lock()
		p.served = true
		p.mu.Unlock()
		if serveErr := server.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
			logger.Error("xbc: gRPC management HTTP service terminated abnormally", "error", serveErr)
		}
	})
	if !accepted {
		_ = ln.Close()
		p.mu.Lock()
		p.started = false
		p.server = nil
		p.ln = nil
		p.mu.Unlock()
		return errors.New("grpc-management: serving task was rejected outside Start admission")
	}
	logger.Info("grpc-management: management listener " + ln.Addr().String())
	return nil
}

// Stop closes the management listener. The plugin opens no traffic and depends
// on none, so the runtime walks past it during the ingress and drain phases
// and reaches it last: by the time this runs, the serving listener is long
// gone, and nothing this listener reports on is still accepting work.
//
// A listener that crossed the traffic gate drains through http.Server.Shutdown
// on the runtime's one remaining deadline -- an in-flight profile finishes, a
// new connection is refused -- and is force-closed if the deadline expires
// first, so no live connection survives Stop under any path. The same contract
// bounds the Web transport's management engine.
func (p *Plugin) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	started := p.started
	served := p.served
	server := p.server
	ln := p.ln
	p.mu.Unlock()
	if !started {
		return nil
	}

	if served && server != nil {
		if err := server.Shutdown(ctx); err != nil {
			// The deadline ended the drain with work still running; closing
			// now is what makes "no live connection after Stop" true, and the
			// drain error is what the runtime reports.
			if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				return fmt.Errorf("grpc-management: shutdown failed (%v) and forced close failed: %w", err, closeErr)
			}
			return fmt.Errorf("grpc-management: shutdown failed: %w", err)
		}
		return nil
	}
	// The gate never released, so no serving task is running and the socket is
	// the only thing to release. The task, if one was admitted, is still
	// waiting for the gate; the runtime cancels this plugin's task scope after
	// Stop returns.
	if ln != nil {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("grpc-management: failed to close listener before traffic gate release: %w", err)
		}
	}
	return nil
}

// addr returns the bound address, or an empty string before Start. It exists
// for tests and for diagnostics that need the resolved port of a ":0" bind.
func (p *Plugin) addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}
