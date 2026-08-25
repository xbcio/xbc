package xbc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/log"
)

// migrateAll drives stage 6. It runs only when a.migrate is true, set by
// cli.go from --migrate / the migrate subcommand / server.auto_migrate --
// migration is a side-effecting write operation, and binding it to every
// boot would mean every rolling restart silently touches the schema.
func (a *App) migrateAll(insts []*instance) error {
	if !a.migrate {
		return nil
	}
	for _, inst := range insts {
		migrator, ok := inst.plugin.(Migrator)
		if !ok {
			continue
		}
		if err := migrator.Migrate(inst.ctx); err != nil {
			return fmt.Errorf("xbc: 插件 %s 迁移失败: %w", inst.label(), err)
		}
	}
	return nil
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

// setGinMode derives gin's run mode from log.level. Debug logging implies
// gin's own debug mode (route dump, warnings); anything quieter gets gin's
// release mode -- gin's debug banner is noisy in production logs and
// duplicates what xbc's own startup log already prints.
func setGinMode(level string) {
	lvl, err := log.ParseLevel(level)
	if err == nil && lvl == log.DebugLevel {
		gin.SetMode(gin.DebugMode)
		return
	}
	gin.SetMode(gin.ReleaseMode)
}

// assembleHTTP drives stage 7: load middleware in Phase order, register
// routes, freeze the route table, then run PostRoutes. Per spec §4.1
// constraint g, middleware is loaded before any route exists (gin requires
// engine.Use before route registration), so middleware must never read
// route metadata at load time -- only at request time via ctx.Route.
func (a *App) assembleHTTP(insts []*instance) error {
	setGinMode(a.cfg.Log.Level)
	gin.DefaultWriter = ginLogWriter{logger: log.L(), level: log.InfoLevel}
	gin.DefaultErrorWriter = ginLogWriter{logger: log.L(), level: log.ErrorLevel}

	engine := gin.New()

	var entries []mwEntry
	for _, inst := range insts {
		mp, ok := inst.plugin.(MiddlewareProvider)
		if !ok {
			continue
		}
		for _, mw := range mp.Middlewares() {
			entries = append(entries, mwEntry{Middleware: mw, qname: qualify(inst.name, mw.Name), plugin: inst.name})
		}
	}
	ordered, misses, err := orderMiddlewares(entries)
	if err != nil {
		return err
	}
	a.softMisses = append(a.softMisses, misses...)
	a.middlewareChain = ordered // Task 15's startup log renders this section directly
	for _, e := range ordered {
		engine.Use(e.Handler)
	}

	// newRouter's engine.Group(basePath) must run after every engine.Use()
	// call above, not before: gin's RouterGroup.Group snapshots the parent
	// group's Handlers slice by value at call time (routergroup.go's
	// combineHandlers copies, it never re-reads the parent later), so a
	// group created before Use() would keep routing through an empty
	// middleware chain forever, no matter how many handlers Use() adds to
	// the engine afterward -- silently serving requests with none of the
	// ordered middleware ever running.
	router := newRouter(engine, a.cfg.Server.BasePath)
	a.router = router

	if a.routeCounts == nil {
		a.routeCounts = make(map[string]int)
	}
	for _, inst := range insts {
		rp, ok := inst.plugin.(RouteProvider)
		if !ok {
			continue
		}
		before := len(*router.routes)
		rp.RegisterRoutes(router)
		// RouteInfo carries no owner field (Plan 2's minimal form, §11), so a
		// per-plugin route count can only be recovered by diffing the frozen
		// table's length around the one call that plugin makes -- Task 15's
		// startup log needs this count for the "routes(N)" capability tag.
		a.routeCounts[inst.id()] = len(*router.routes) - before
	}

	router.freeze()

	for _, inst := range insts {
		pr, ok := inst.plugin.(PostRouter)
		if !ok {
			continue
		}
		if err := pr.PostRoutes(inst.ctx); err != nil {
			return fmt.Errorf("xbc: 插件 %s 的 PostRoutes 失败: %w", inst.label(), err)
		}
	}
	return nil
}

// startRunners drives stage 8: concurrently start every Runner. Start must
// return quickly -- long-running loops go through ctx.Go / ctx.GoCritical.
// errgroup would pull in a dependency for something a WaitGroup + a Mutex
// already do; any Start failure rolls back everything already initialized.
func (a *App) startRunners(insts []*instance) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, inst := range insts {
		runner, ok := inst.plugin.(Runner)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(inst *instance, runner Runner) {
			defer wg.Done()
			if err := runner.Start(inst.ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("xbc: 插件 %s 启动失败: %w", inst.label(), err))
				mu.Unlock()
			}
		}(inst, runner)
	}
	wg.Wait()

	if len(errs) == 0 {
		return nil
	}
	if a.cancel != nil {
		a.cancel()
	}
	if a.wg != nil {
		a.wg.Wait()
	}
	a.rollback(insts)
	return errors.Join(errs...)
}

// serve drives stage 9: block running the HTTP server until one of three
// signals arrives -- an OS interrupt, a GoCritical failure, or Serve's own
// error -- then hands off to shutdown. a.listener lets tests pass a :0
// listener and pick their own port; a.ready is closed once Serve has
// actually started accepting, so a test never has to guess with a sleep.
func (a *App) serve() error {
	if a.listener == nil {
		ln, err := net.Listen("tcp", a.cfg.Server.Addr)
		if err != nil {
			return fmt.Errorf("xbc: 监听 %s 失败: %w", a.cfg.Server.Addr, err)
		}
		a.listener = ln
	}
	a.httpServer = &http.Server{
		Handler:      a.router.engine,
		ReadTimeout:  a.cfg.Server.ReadTimeout,
		WriteTimeout: a.cfg.Server.WriteTimeout,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.httpServer.Serve(a.listener) }()

	if a.ready != nil {
		close(a.ready)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		return a.shutdown("signal")
	case <-a.criticalCh:
		return a.shutdown("critical")
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

// shutdown drives stage 10: stop accepting new requests, drain in-flight
// ones, cancel every managed goroutine and wait for them, stop every
// initialized plugin in reverse topological order (spec §4.1 constraint a:
// dependents before their dependencies), then force-kill anything still
// stuck past shutdown_timeout. reason distinguishes a clean signal-triggered
// shutdown from a GoCritical-triggered one, which additionally sets the
// process exit code to 1.
func (a *App) shutdown(reason string) error {
	log.L().Info("xbc: 开始关闭", "reason", reason)

	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
	defer cancel()

	if a.httpServer != nil {
		if err := a.httpServer.Shutdown(ctx); err != nil {
			log.L().Error("xbc: 优雅关闭超时，强制关闭", "error", err)
			_ = a.httpServer.Close()
		}
	}

	if a.cancel != nil {
		a.cancel()
	}
	if a.wg != nil {
		a.wg.Wait()
	}

	a.rollback(a.order)

	if reason == "critical" {
		a.exitCode = 1
	}
	return nil
}
