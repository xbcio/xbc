package plugin

import "github.com/xbcio/xbc/config"

// Starter is a product-level composition entry point: one value carrying a
// documented, integration-tested default policy for a kind of process, so an
// application selects that policy instead of assembling one Bundle at a time
// and copying a baseline configuration between services.
//
// It contributes exactly two things, and they are deliberately one value
// rather than two arguments. Bundles are the Definitions the application
// selects; Defaults is the lowest configuration layer those Definitions are
// read through, which is what lets a capability be on by default without the
// application owning a file that says so. Handing them over separately would
// admit a composition whose defaults name a configuration path none of its
// Bundles declares -- a section nobody owns, which the loader refuses rather
// than ignores.
//
// A Starter does not select a transport engine. An engine is a choice about
// the process rather than about the baseline, and the adapter module that
// provides one is not something a protocol-neutral package may import; the
// composition root selects it beside the Starter.
//
// A Starter is also not a mechanism for changing a capability's semantics.
// The values a capability needs are the owning plugin's own declared defaults,
// and a Starter that repeated them would be a second copy of the same decision
// that drifts from the first. What a Starter decides is which of them are on.
//
// The vocabulary lives here rather than in the runtime for the same reason
// PlacementSource does: a transport module must be able to implement it, and
// the architecture guard forbids a transport module from importing runtime.
type Starter interface {
	// Bundles returns the Definitions this starter selects. They compose
	// exactly as a composition root's own Bundles do -- the starter adds
	// selection, never precedence.
	Bundles() []Bundle
	// Defaults returns the configuration layer this starter contributes,
	// merged below every other source: a file, a profile overlay, an
	// Override, and an environment variable all win over it.
	Defaults() config.Defaults
}
