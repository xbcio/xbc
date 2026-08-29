package web

import "github.com/xbcio/xbc/plugin"

// Key is the stable catalog and configuration identity of the HTTP server
// plugin. Applications that assemble a private catalog can use it to inspect
// or replace the definition without depending on the Server implementation
// type.
const Key plugin.Key = "web"

// Definition returns the immutable catalog entry for the HTTP server plugin.
// Calling Definition has no side effects; use package transport/web/autoload when an
// application intentionally wants import-time registration in the default
// catalog.
func Definition() plugin.Definition {
	return plugin.Definition{
		Key:        Key,
		Factory:    func() plugin.Plugin { return new(Server) },
		Instances:  plugin.SingleInstance,
		Activation: plugin.Always,
	}
}
