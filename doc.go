// Package xbc is the core of a protocol-agnostic plugin framework.
//
// # The entry point
//
// An application is a list of blank imports and one call:
//
//	import (
//		_ "example.com/order-service/internal/order"
//		"github.com/xbcio/xbc"
//		_ "github.com/xbcio/xbc/web/autoload"
//	)
//
//	func main() { xbc.Run() }
//
// Each imported autoload package declares its plugins from init() into the
// default catalog. Run freezes that catalog, loads configuration, decides
// which plugins are enabled, orders them by their declared dependencies, and
// drives them through their lifecycle until something asks the process to
// stop. Ordinary library packages such as web remain safe to import without
// mutating process state.
//
// There is no registration call in main. A plugin is declared where it is
// defined, by the package that owns it, and the import list is the whole of
// what an application chooses to include.
//
// # What this package knows nothing about
//
// The core has no HTTP server, no gRPC server, and no dependency on either.
// It cannot import github.com/xbcio/xbc/web -- that is an independent module
// which depends on this one, never the reverse -- and it contains no
// special-casing for it. A protocol module participates through the same
// interfaces any other plugin uses:
//
//   - plugin.Runner prepares and binds, and must return without serving.
//   - plugin.TrafficOpener starts accepting, and is called only once every
//     Runner in the application has bound successfully.
//   - plugin.Closer stops, within a shared budget the core enforces.
//
// That is the entire contract between the core and anything that serves
// traffic. It is why the core never sorts plugins by name, never checks
// whether "web" is present, and needs no change to accommodate a protocol it
// has not seen.
//
// # Identity
//
// A plugin's identity is its Definition.Key, and nothing else. That single
// string is the key for its configuration section, its dependency edges, its
// log fields and its Identity in the extension system. Nothing is derived
// from the Go type or package path, so renaming a package cannot silently
// change which configuration section a plugin reads.
//
// # Isolation
//
// Only immutable Definitions are process-global. Everything a run mutates --
// plugin instances, Contexts, the value registry, the managed task group --
// belongs to one App. A private catalog (see WithDefinitions) isolates those
// runtime values. Process facilities such as environment variables, OS
// signals, the configured global logger, and protocol-library globals may
// still be shared, so multiple Apps are not a complete process-isolation
// boundary.
package xbc
