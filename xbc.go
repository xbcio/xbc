package xbc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/xbcio/xbc/internal/conf"
	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/internal/inject"
	"github.com/xbcio/xbc/log"
)

// osExit is the process-termination hook Run goes through -- indirected
// through a package var, mirroring log/zap.go's own exitFunc, so a test can
// observe Run's tail sequence (log.Sync() then exit) in-process without
// forking a subprocess.
var osExit = os.Exit

// source records which registration path a plugin arrived by. The two paths
// carry different intent strength, so they get different enable rules (§6.4,
// implemented in stage_expand.go).
type source int

const (
	sourceImport   source = iota // blank import + init(): capability is available
	sourceRegister               // app.Register(): an explicit statement of intent
)

// defaultInstance is the instance name used when a plugin doesn't ask for a
// specific one.
const defaultInstance = "default"

// entry is one registered plugin prototype, before expansion.
type entry struct {
	proto Plugin
	name  string
	src   source
	multi bool
}

var (
	registerMu sync.Mutex
	registered []entry
)

// Register registers a plugin from a package init(). Enabled only when a
// matching config section exists (see stage_expand.go's enable-rule matrix).
//
// Register cannot return an error -- it exists to be called from init(),
// which has no error channel -- so a malformed plugin (an unresolvable name,
// or a name collision with an earlier registration) panics instead. This
// mirrors database/sql.Register: a mistake here explodes at program startup,
// inside the offending package's own init(), not silently later during a
// request.
func Register(p Plugin) {
	registerMu.Lock()
	defer registerMu.Unlock()

	e, err := newEntry(p, sourceImport)
	if err != nil {
		panic(err)
	}
	for _, existing := range registered {
		if existing.name == e.name {
			panic(fmt.Errorf(
				"xbc: 插件名 %q 冲突：%T 与 %T 推导出同一个名字，请给其中一个显式实现 Name() 改名",
				e.name, existing.proto, e.proto))
		}
	}
	registered = append(registered, e)
}

// App is the assembled application. Every exported method returns *App (or
// nothing), so main() reads top to bottom: New().Register(...).Run().
//
// Most of these fields are written by a later assembly stage, not here. They
// are declared up front so every stage agrees on one set of names -- a struct
// field costs nothing until something writes it, and go vet does not flag a
// field nobody reads yet.
type App struct {
	mu      sync.Mutex
	entries []entry

	// Stage 1-4 results.
	order   []*instance // topological order, written by resolve (stage 4)
	migrate bool        // whether stage 6 runs, decided by cli.go

	// HTTP, assembled by stage 7 and driven by stages 8-10.
	router      *Router
	httpServer  *http.Server
	listener    net.Listener
	ready       chan struct{}  // closed once the listener accepts
	routeCounts map[string]int // plugin name -> routes it registered

	// softMisses collects every soft ordering constraint (After/Before)
	// that named something which does not exist -- from stage 4's plugin
	// sort and from this stage's middleware sort alike. cli.go's startup
	// log renders them as one warning block; they are never fatal, since a
	// preference that points at an absent plugin is a stale preference,
	// not a broken dependency.
	softMisses []graph.Miss

	// middlewareChain is the ordered chain assembled by stage 7. It is kept
	// on App so cli.go's startup log can print the final order without
	// re-running orderMiddlewares -- printing a second, independently
	// computed order would be a chance for the log to disagree with what
	// gin actually runs.
	middlewareChain []mwEntry

	// Managed goroutine lifecycle, see goroutine.go.
	runCtx         context.Context // handed to every ctx.Go / ctx.GoCritical callback
	cancel         context.CancelFunc
	wg             *sync.WaitGroup
	criticalCh     chan struct{} // closed once by triggerCritical
	criticalOnce   sync.Once
	criticalReason string

	exitCode int

	// registry is the (type, instance) store shared by every Context of this
	// App -- see registry.go.
	registry *registry

	// cfg is the merged configuration, filled by stage 1's loadConfig and
	// read by every stage after it. It stays nil until then, so anything
	// that touches it before stage 1 is a pipeline-ordering bug, not a
	// missing nil check.
	cfg *Config
}

// instance is one expanded plugin instance -- the unit every stage after
// expansion operates on. Expansion fills plugin/name/instance/src/ctx;
// resolve fills fields/deps/provides; initAll flips inited.
type instance struct {
	plugin   Plugin
	name     string // plugin name, e.g. "gorm"
	instance string // instance name, e.g. "default" / "readonly"
	src      source
	ctx      *Context

	// fields is the xbc-tag scan of this instance's plugin, filled by
	// resolve's pass 0. Stage 5 injects into these fields and harvests the
	// provide-tagged ones back out, so both stages read the same scan
	// instead of each doing their own -- one scan, one source of truth for
	// what the plugin's tags actually said.
	fields []inject.FieldSpec

	deps     Deps
	provides []Dep
	inited   bool
}

// New creates an App seeded with every plugin registered via the
// package-level Register.
func New() *App {
	registerMu.Lock()
	seed := make([]entry, len(registered))
	copy(seed, registered)
	registerMu.Unlock()
	return &App{entries: seed, registry: newRegistry()}
}

// Register registers one or more plugins explicitly. Unlike the
// package-level Register (a capability announcement), this is a statement of
// intent: the plugin is enabled by default even without a matching config
// section (§6.4).
//
// Like the package-level Register, this has no error return: app.Register
// calls read as a flat list in main(), and a name collision here is exactly
// as much a programming error as a duplicate package-level Register, so it
// panics for the same reason.
func (a *App) Register(p ...Plugin) *App {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, one := range p {
		e, err := newEntry(one, sourceRegister)
		if err != nil {
			panic(err)
		}
		for _, existing := range a.entries {
			if existing.name == e.name {
				panic(fmt.Errorf(
					"xbc: 插件名 %q 冲突：%T 与 %T 推导出同一个名字，请给其中一个显式实现 Name() 改名",
					e.name, existing.proto, e.proto))
			}
		}
		a.entries = append(a.entries, e)
	}
	return a
}

// Run is the process entry point: main() calls this and nothing else. It
// parses os.Args, drives the pipeline, prints a fatal error to stderr on
// failure, flushes the logger, and exits with the resulting code.
//
// log.Sync() runs here, not inside shutdown(): shutdown is stage 10, reached
// partway through the pipeline (or not at all, e.g. doctor/migrate return
// before it), so flushing there would happen before the pipeline has
// actually finished producing log output. Run is the one place that always
// runs last, on every exit path, so the flush belongs immediately before
// osExit and nowhere else.
func (a *App) Run() {
	code, err := a.run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	_ = log.Sync()
	osExit(code)
}

// run is the testable core behind Run: it never touches os.Exit, so tests
// can call it directly and assert on the returned exit code without forking
// a subprocess. Exit code 2 marks a command-line usage error (flag package's
// own convention); 1 marks a runtime/pipeline failure; 0 is success.
func (a *App) run(args []string) (exitCode int, err error) {
	opts, err := parseArgs(args)
	if err != nil {
		return 2, err
	}
	a.migrate = opts.migrate || opts.subcommand == "migrate"

	if err := a.loadConfig(conf.Options{File: opts.config, Profile: opts.profile, EnvPrefix: "XBC_"}); err != nil {
		return 1, err
	}
	if !a.migrate && a.cfg.Server.AutoMigrate {
		a.migrate = true
	}

	insts, err := a.expand()
	if err != nil {
		return 1, err
	}
	if err := a.bindConfigs(insts); err != nil {
		return 1, err
	}
	order, misses, err := a.resolve(insts)
	if err != nil {
		return 1, err
	}
	a.order = order
	a.softMisses = append(a.softMisses, misses...)

	if opts.subcommand == "doctor" {
		a.printStartupLog(order) // reports only the assembly result, without establishing any connection
		return 0, nil
	}

	// initGoroutines must run right here, immediately before initAll: a
	// plugin's Init already receives a live *Context and is free to call
	// ctx.Go/ctx.GoCritical from inside Init itself (see goroutine.go's own
	// doc comment on initGoroutines), so a.wg/a.runCtx/a.criticalCh must
	// already exist before initAll runs, not merely before startRunners.
	// Placing this any earlier -- e.g. at the top of run(), before the
	// doctor check above -- would make doctor set up a background context
	// it returns without ever cancelling.
	a.initGoroutines()

	if err := a.initAll(order); err != nil {
		return 1, err
	}

	if err := a.migrateAll(order); err != nil {
		return 1, err
	}

	if opts.subcommand == "migrate" {
		// Mirrors shutdown()'s own ordering: a Migrator plugin's Init may
		// have started managed goroutines that must be cancelled and
		// drained before rollback runs, otherwise rollback could Stop a
		// plugin out from under a goroutine that is still using it.
		a.cancel()
		a.wg.Wait()
		a.rollback(order) // a one-shot process; even exiting right after finishing must Stop cleanly, leaving no lingering connections
		return 0, nil
	}

	if err := a.assembleHTTP(order); err != nil {
		return 1, err
	}
	a.printStartupLog(order)

	if err := a.startRunners(order); err != nil {
		return 1, err
	}

	if err := a.serve(); err != nil {
		return 1, err
	}
	if a.exitCode != 0 {
		return a.exitCode, nil
	}
	return 0, nil
}

// newEntry resolves a plugin's name and wraps it into an entry.
//
// Name resolution order: a plugin's own Name() wins whenever it returns a
// non-empty value. That covers both a hand-written Name() (method shadowing
// beats Base's promoted method outright) and a Base-embedding plugin that
// was already bound by an earlier call. Only when Name() comes back empty --
// meaning Base is embedded but has never been bound -- does newEntry fall
// back to deriveName and bind it, so Base.Name() (and any later p.Name()
// call) reflects it from then on. A plugin that neither overrides Name() nor
// embeds Base can't legally reach the empty-string branch at all: the Plugin
// interface requires Name(), so such a plugin must already return something
// non-empty on its own.
func newEntry(p Plugin, src source) (entry, error) {
	name := p.Name()
	if name == "" {
		derived, err := deriveName(p)
		if err != nil {
			return entry{}, fmt.Errorf("xbc: 插件 %T 未实现 Name()，且无法自动推导名字：%w", p, err)
		}
		if !bindBase(p, nil, derived) {
			return entry{}, fmt.Errorf(
				"xbc: 插件 %T 的 Name() 返回空字符串，且未嵌入 xbc.Base，无法确定插件名", p)
		}
		name = p.Name()
	}
	if err := validateName(name); err != nil {
		return entry{}, err
	}

	e := entry{proto: p, name: name, src: src, multi: isMultiInstance(p)}
	return e, nil
}
