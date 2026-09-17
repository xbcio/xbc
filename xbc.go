// Package xbc is the narrow application facade for the XBC Plugin runtime.
//
// An application whose process belongs to XBC composes its Bundles and calls
// Run:
//
//	func main() {
//		xbc.Run(xbc.WithBundles(
//			webprelude.Bundle(),
//			ginengine.Bundle(),
//			orders.Bundle(),
//		))
//	}
//
// Run owns the process on the application's behalf: os.Args, SIGINT/SIGTERM
// graceful shutdown, forced termination when a stop signal repeats, error
// reporting, logger flushing, and the exit code. It does not return.
//
// A caller that owns the process itself uses New and App.Execute, which touch
// none of those facilities -- for instance to supply its own parent context, or
// to embed XBC in a larger process that already handles signals and exit:
//
//	app, err := xbc.New(xbc.WithBundles(orders.Bundle()))
//	if err != nil { /* handle */ }
//	code, err := app.Execute(ctx, args)
//
// A Bundle is side-effect-free composition data. Definitions are immutable
// canonical handles; planning freezes configuration, contracts, typed inputs,
// lifecycle descriptors, and the dependency graph before any factory runs.
//
// Applications that deliberately prefer blank imports may use leaf autoload
// adapters and call Run without Options. Ordinary implementation and Prelude
// imports never mutate process state.
package xbc

import (
	"context"

	"github.com/xbcio/xbc/plugin"
	appruntime "github.com/xbcio/xbc/runtime"
)

// App is a single-use assembled application with a deliberately narrow public
// surface. Runtime and construction details remain behind this facade.
type App struct{ impl *appruntime.App }

// Option configures New and Run.
type Option = appruntime.Option

// WithBundles explicitly composes the application's canonical Definitions.
func WithBundles(bundles ...plugin.Bundle) Option { return appruntime.WithBundles(bundles...) }

// New creates an App the caller executes itself, without loading
// configuration or constructing resources. It owns no process facility: use it
// when the process is not XBC's, and prefer Run when it is.
func New(options ...Option) (*App, error) {
	implementation, err := appruntime.New(options...)
	if err != nil {
		return nil, err
	}
	return &App{impl: implementation}, nil
}

// Execute plans, constructs, starts, and eventually unwinds the App once. The
// caller supplies the parent context and arguments, and remains responsible
// for signals, error reporting, logger flushing, and the exit code.
func (app *App) Execute(ctx context.Context, args []string) (int, error) {
	return app.impl.Execute(ctx, args)
}

// Run assembles an App from options and runs it as the whole process, owning
// os.Args, signal-driven shutdown including forced termination on a repeated
// stop signal, error reporting, logger flushing, and process exit. It does not
// return.
func Run(options ...Option) { appruntime.Run(options...) }
