// Package banner renders one startup banner through the logging facade.
//
// The banner is a presentation concern, not a diagnostic: it states which
// application is booting and gets out of the way. Importing this package and
// evaluating its declarations has no side effect. Prefer explicit composition
// with banner.Bundle(); executables that deliberately use process-wide
// autoload may blank-import the autoload subpackage.
//
// # Why this is a plugin
//
// A banner is optional, so it must be selectable the same way every other
// capability is: an application that does not want it never composes the
// Bundle, and the framework's plugins.banner.enabled switch vetoes it even
// when the Bundle is selected. Keeping it out of the runtime core also keeps
// the ASCII-art dependency out of every binary that does not ask for it.
//
// # Startup phase and order
//
// The banner is written from the Start hook, which is the earliest phase that
// emits deliberate boot output: factories also run for the doctor and
// validate diagnostics, and Init is meant for ownership transfer rather than
// display. XBC invokes Start hooks in dependency-graph order and breaks ties
// between dependency-free instances by identity, so this plugin -- which
// declares no inputs -- runs beside the first plugins that need nothing. It
// does not control that position; the framework exposes no per-phase
// priority, and inventing a dependency would only move it later.
//
// One record is emitted, once per process. Nothing is printed by the factory,
// by Definition, or by Bundle.
//
// # Output shape
//
// The art is sent as a single log message with embedded newlines and no ANSI
// escapes. That is the shape both built-in formats handle without breaking
// their structure: the JSON encoding writes one object per record and escapes
// the newlines inside its message field, while the console encoding prints
// the message as written and therefore shows the banner as its own lines.
// Configured text may not contain control characters or line breaks, and text
// taken from the runtime is stripped of control characters before rendering,
// so no path can introduce an escape sequence into that message.
//
// The entry carries no fields and no other configuration values: the rendered
// text is the whole record.
//
// # Text
//
// The text is resolved in this order:
//
//   - plugins.banner.text, when it is not blank;
//   - the runtime's service name (xbc.service.name, or the name derived from
//     the build or executable);
//   - the process instance id;
//   - the fixed fallback "XBC".
//
// # Styles and fonts
//
// plugins.banner.style selects "ascii" (the default), which renders the
// multi-line art, or "plain", which renders one plain line for an environment
// that does not want the oversized output. plugins.banner.font selects a font
// the pinned renderer embeds; the default is "standard". The plain style
// ignores the font, but a configured font name is still validated so a typo
// cannot sit silently in a manifest until someone switches styles.
//
// # Enablement
//
// plugins.banner.enabled is the framework-owned section switch and defaults
// to true. Setting it to false disables the Definition during planning, which
// means no instance is constructed and no Start hook runs: there is no banner
// and no per-plugin "disabled" line, because the plugin that would write one
// does not exist. The runtime's own disabled-plugin report names the section
// and the reason at Info level.
//
// # Shutdown
//
// The plugin owns a normalized configuration value and an immutable
// Definition. It starts no goroutine, submits no managed task, and opens no
// connection, so it implements neither Drain nor Stop and has nothing to
// release during shutdown.
//
// # Diagnostics
//
// doctor resolves and prints the plan without constructing plugins, and
// validate constructs them but runs Preflight instead of Start, so neither
// command renders a banner. Only a run that reaches Start does.
//
// # Usage
//
//	app, err := xbc.New(xbc.WithBundles(
//		banner.Bundle(),
//	))
//
//	plugins:
//	  banner:
//	    text: "order service"
//	    style: ascii
//	    font: standard
//
// Leave text empty to derive it from the process identity, or choose
// style: plain for a single-line startup message.
package banner
