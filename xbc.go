// Package xbc is the narrow application facade for the XBC Plugin runtime.
//
// Explicit Bundle composition is the primary entry point:
//
//	app, err := xbc.New(xbc.WithBundles(
//		webprelude.Bundle(),
//		ginengine.Bundle(),
//		orders.Bundle(),
//	))
//	if err != nil { /* handle */ }
//	code, err := app.Execute(context.Background(), os.Args[1:])
//
// A Bundle is side-effect-free composition data. Definitions are immutable
// canonical handles; planning freezes configuration, contracts, typed inputs,
// lifecycle descriptors, and the dependency graph before any factory runs.
//
// Applications that deliberately prefer blank imports may use leaf autoload
// adapters and Run. Ordinary implementation and Prelude imports never mutate
// process state.
package xbc

import (
	"context"

	"github.com/xbcio/xbc/plugin"
	appruntime "github.com/xbcio/xbc/runtime"
)

// App is a single-use assembled application with a deliberately narrow public
// surface. Runtime and construction details remain behind this facade.
type App struct{ impl *appruntime.App }

// Option configures New.
type Option = appruntime.Option

// WithBundles explicitly composes the application's canonical Definitions.
func WithBundles(bundles ...plugin.Bundle) Option { return appruntime.WithBundles(bundles...) }

// New creates an App without loading configuration or constructing resources.
func New(options ...Option) (*App, error) {
	implementation, err := appruntime.New(options...)
	if err != nil {
		return nil, err
	}
	return &App{impl: implementation}, nil
}

// Execute plans, constructs, starts, and eventually unwinds the App once.
func (app *App) Execute(ctx context.Context, args []string) (int, error) {
	return app.impl.Execute(ctx, args)
}

// Run is the process-owned entry point for optional autoload composition.
func Run() { appruntime.Run() }
