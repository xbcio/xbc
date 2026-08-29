package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Server is the Gin-backed Web plugin. It implements the two-phase
// readiness contract from package-layout design §5.1: Start (Runner) binds
// the listening socket but accepts no traffic yet; OpenTraffic
// (TrafficOpener), called once every Runner in the application has
// finished Start, is what actually begins serving requests.
type Server struct {
	plugin.Base

	cfg Config

	// listener, when non-nil, replaces the net.Listen call inside Start.
	// Only transport/web/export_test.go's setListener ever assigns it -- see that
	// file's doc comment for why there is deliberately no public setter.
	listener net.Listener

	mu      sync.Mutex
	engine  *gin.Engine
	router  *Router
	ln      net.Listener
	srv     *http.Server
	started bool // Start completed successfully
	served  bool // OpenTraffic has handed srv.Serve to a managed goroutine
}

var (
	_ plugin.Plugin        = (*Server)(nil)
	_ plugin.Configurable  = (*Server)(nil)
	_ plugin.Runner        = (*Server)(nil)
	_ plugin.TrafficOpener = (*Server)(nil)
	_ plugin.Closer        = (*Server)(nil)
)

// ConfigPtr implements plugin.Configurable. Package assembly binds
// "plugins.web" onto this pointer before Start ever runs.
func (s *Server) ConfigPtr() any { return &s.cfg }

// Addr returns the actual address Start bound. It is the only way to learn
// the real port when Config.Addr is "host:0" and the kernel picked an
// ephemeral one, and it is safe to call once Start has returned (nothing
// about it depends on OpenTraffic having run yet).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// ginLogWriter adapts an xbc log.Logger to io.Writer so gin's own startup
// banner and internal warnings land in the same structured log stream as
// everything else, instead of a second, unstructured format fighting for
// stdout.
type ginLogWriter struct {
	logger log.Logger
	level  log.Level
}

func (w ginLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		if w.level == log.ErrorLevel {
			w.logger.Error(msg)
		} else {
			w.logger.Info(msg)
		}
	}
	return len(p), nil
}

// setGinMode derives gin's run mode from the logger capability already bound
// to this plugin. Web must not reach across owner boundaries to read core's
// "log.level" configuration key: doing so would create a runtime-only
// dependency that Go's import graph cannot check. If debug entries are
// enabled, gin's own diagnostics are useful; otherwise release mode avoids a
// noisy duplicate startup banner.
func setGinMode(logger log.Logger) {
	if logger != nil && logger.Enabled(log.DebugLevel) {
		gin.SetMode(gin.DebugMode)
		return
	}
	gin.SetMode(gin.ReleaseMode)
}

// Start is readiness phase 1 (plugin.Runner): assemble the gin.Engine,
// install the internal CurrentRoute-recording middleware, collect and order
// every MiddlewareProvider's contributions, register every RouteProvider's
// routes, freeze the route table, notify every RouteCatalogConsumer, render
// web's own startup report, and finally bind the listening socket. Start
// returning successfully means the port is bound and Addr() is readable --
// it does NOT mean requests are being served; that only begins once
// OpenTraffic runs (design §5.1, §8.1).
func (s *Server) Start(ctx *plugin.Context) error {
	logger := ctx.Log()
	setGinMode(logger)
	gin.DefaultWriter = ginLogWriter{logger: logger, level: log.InfoLevel}
	gin.DefaultErrorWriter = ginLogWriter{logger: logger, level: log.ErrorLevel}

	engine := gin.New()

	// recordCurrentRoute must be the very first engine.Use() call -- outside
	// every user middleware -- so CurrentRoute keeps working even for a
	// request a later middleware aborts early (design §5.6). It closes over
	// routeTable's pointers, which freeze() below fills in once the route
	// table is complete; the middleware itself only ever runs at request
	// time, well after Start has returned.
	routes, frozen, index := newRouteTable()
	engine.Use(recordCurrentRoute(frozen, index))

	mwExtensions, err := plugin.Extensions[MiddlewareProvider](ctx)
	if err != nil {
		return err
	}
	var entries []mwEntry
	for _, ext := range mwExtensions {
		for _, mw := range ext.Value.Middlewares() {
			entries = append(entries, mwEntry{
				Middleware: mw,
				qname:      qualify(ext.Identity.Plugin.String(), mw.Name),
			})
		}
	}
	ordered, misses, err := orderMiddlewares(entries)
	if err != nil {
		return err
	}
	for _, e := range ordered {
		engine.Use(e.Handler)
	}

	// newRouter's engine.Group(basePath) must run after every engine.Use()
	// call above, not before -- see newRouter's own doc comment for why.
	router := newRouter(engine, s.cfg.BasePath, routes, frozen, index)

	routeExtensions, err := plugin.Extensions[RouteProvider](ctx)
	if err != nil {
		return err
	}
	for _, ext := range routeExtensions {
		ext.Value.RegisterRoutes(router)
	}

	catalog := router.freeze()

	consumerExtensions, err := plugin.Extensions[RouteCatalogConsumer](ctx)
	if err != nil {
		return err
	}
	for _, ext := range consumerExtensions {
		if err := ext.Value.RoutesReady(catalog); err != nil {
			return fmt.Errorf("xbc: 插件 %s 的 RoutesReady 失败: %w", ext.Identity, err)
		}
	}

	if len(ordered) > 0 {
		logger.Info(renderMiddlewareChain(ordered))
	}
	if len(misses) > 0 {
		logger.Info(renderSoftMisses(misses))
	}
	logger.Info(renderRouteTable(catalog.All()))

	ln := s.listener
	if ln == nil {
		ln, err = net.Listen("tcp", s.cfg.Addr)
		if err != nil {
			return fmt.Errorf("xbc: 监听 %s 失败: %w", s.cfg.Addr, err)
		}
	}

	s.mu.Lock()
	s.engine = engine
	s.router = router
	s.ln = ln
	s.srv = &http.Server{
		Handler:      engine,
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
	}
	s.started = true
	s.mu.Unlock()
	return nil
}

// OpenTraffic is readiness phase 2 (plugin.TrafficOpener): hand the bound
// listener to http.Server.Serve inside a managed critical goroutine, and
// return immediately -- it must never block on Serve itself (design §5.1
// rule 5). Serve returning with http.ErrServerClosed is the expected,
// graceful-shutdown outcome (Stop calls srv.Shutdown/Close to produce
// exactly that); any other error is logged because nothing else would ever
// see it, but this goroutine still lets Go's own "unprompted return"
// judgment (design §5.5) run against the application-level shutdown flag
// rather than second-guessing it here.
func (s *Server) OpenTraffic(ctx *plugin.Context) error {
	s.mu.Lock()
	srv := s.srv
	ln := s.ln
	if srv == nil || ln == nil {
		s.mu.Unlock()
		return fmt.Errorf("xbc: web.Server 尚未成功 Start，无法开放流量")
	}
	s.served = true
	s.mu.Unlock()

	logger := ctx.Log()
	ctx.GoCritical(func(context.Context) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("xbc: HTTP 服务异常终止", "error", err)
		}
	})
	return nil
}

// Stop implements plugin.Closer. ctx carries whatever is left of core's
// shared shutdown deadline (design §5.3); Server must use it to bound its
// own drain instead of assuming an unlimited budget.
//
// Two shapes have to be told apart:
//
//   - OpenTraffic ran: srv.Serve is (or was) actively accepting connections,
//     so the correct stop is srv.Shutdown(ctx) -- drain in-flight requests,
//     stop accepting new ones -- falling back to srv.Close() if the deadline
//     is hit first.
//   - OpenTraffic never ran (startup aborted between Start and the
//     traffic-opening barrier): srv exists but was never handed to Serve,
//     so http.Server has no listener registered to close on Shutdown's
//     behalf (that registration only happens inside Serve itself). The raw,
//     bound-but-never-accepted listener must be closed directly here, or the
//     fd leaks for the life of the process.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	served := s.served
	srv := s.srv
	ln := s.ln
	s.mu.Unlock()

	if !started {
		return nil
	}

	if served {
		if srv == nil {
			return nil
		}
		if err := srv.Shutdown(ctx); err != nil {
			if cerr := srv.Close(); cerr != nil {
				return fmt.Errorf("xbc: 优雅关闭超时，强制关闭也失败: %w", cerr)
			}
		}
		return nil
	}

	if ln != nil {
		if err := ln.Close(); err != nil {
			return fmt.Errorf("xbc: 关闭未开放流量的监听器失败: %w", err)
		}
	}
	return nil
}
