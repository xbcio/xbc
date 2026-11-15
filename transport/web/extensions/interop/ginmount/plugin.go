package ginmount

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable plugin identity and the last segment of the section that
// activates it.
const Key plugin.Key = "ginmount"

// ConfigPath is the configuration section this plugin is activated by. It is
// spelled out rather than derived because the same path is quoted in the
// application's configuration, and a renamed section must not silently keep
// naming the old one here.
const ConfigPath = "plugins.ginmount"

// Registrar contributes one Gin subtree to the XBC route table.
//
// It is an input contract rather than an option because the subtree belongs to
// an existing application plugin: that plugin already owns its controllers and
// its Gin middleware, and exporting a Registrar is how it keeps them while
// letting this package own the placement.
type Registrar interface {
	// MountPath is the literal, non-root prefix the subtree is served
	// beneath, for example "/api/v1". It is refused when it is empty, does
	// not start with "/", is the root, ends with "/", or carries Gin routing
	// syntax (":" or "*"): a mount owns a whole subtree, and each of those
	// shapes either names no subtree or claims one the route table cannot
	// describe as a literal row.
	MountPath() string

	// RegisterGin installs middleware and routes on the engine. Routes are
	// relative to MountPath: the mount strips the prefix before the engine
	// sees the request, exactly as http.StripPrefix defines it.
	RegisterGin(*gin.Engine)
}

// registration is one validated Registrar together with the prefix it
// declared when the plugin was built. The path is read once, at build time, so
// what validation approved is what RegisterRoutes mounts even if a Registrar
// computes its path dynamically.
type registration struct {
	path      string
	registrar Registrar
}

// Plugin mounts every registered Gin subtree as one XBC mount row per prefix.
type Plugin struct {
	registrations []registration
}

var _ web.RouteContributor = (*Plugin)(nil)

var registrarInput = plugin.Collect[Registrar]()

var definition = plugin.Define(
	Key,
	func(context plugin.BuildContext) (*Plugin, error) {
		return newPlugin(context, registrarInput.Get(context))
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured(ConfigPath),
		Inputs:     plugin.Inputs(registrarInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns ginmount's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns ginmount's side-effect-free composition bundle.
func Bundle() plugin.Bundle { return bundle }

// newPlugin validates every collected Registrar and freezes the prefix each
// one declared. It is the single construction path, so the package's own tests
// can exercise the exact value the graph builds without starting a runtime.
//
// A nil Registrar is refused rather than skipped: a plugin that exported a nil
// value asked to contribute a subtree and failed, and mounting nothing on its
// behalf would leave the prefix silently served by the framework's 404.
func newPlugin(_ plugin.BuildContext, entries []plugin.Entry[Registrar]) (*Plugin, error) {
	registrations := make([]registration, 0, len(entries))
	for _, entry := range entries {
		if entry.Value == nil {
			return nil, fmt.Errorf("ginmount: plugin %s exported a nil Registrar", entry.Identity)
		}
		path := entry.Value.MountPath()
		if err := validateMountPath(path); err != nil {
			return nil, fmt.Errorf("ginmount: registrar of plugin %s: %w", entry.Identity, err)
		}
		registrations = append(registrations, registration{path: path, registrar: entry.Value})
	}
	return &Plugin{registrations: registrations}, nil
}

// validateMountPath refuses the prefix shapes a mount cannot express, quoting
// the value so a misconfigured Registrar names what it returned.
func validateMountPath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("MountPath %q is empty", path)
	case !strings.HasPrefix(path, "/"):
		return fmt.Errorf("MountPath %q must begin with %q", path, "/")
	case path == "/":
		return fmt.Errorf("MountPath %q cannot be the service root", path)
	case strings.HasSuffix(path, "/"):
		return fmt.Errorf("MountPath %q must not end with %q", path, "/")
	case strings.ContainsAny(path, ":*"):
		return fmt.Errorf("MountPath %q contains %q or %q, which is routing syntax rather than a literal prefix", path, ":", "*")
	}
	return nil
}

// RegisterRoutes mounts one Gin engine per distinct prefix, in the order the
// prefixes first appeared in the input.
//
// Registrars naming the same prefix share one engine: a host that already
// splits its Gin setup across several registration functions expects one
// middleware stack, and a second engine at the same prefix would stack a second
// copy of every middleware on the same routes.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	if p == nil {
		return
	}

	type mount struct {
		path   string
		engine *gin.Engine
	}
	mounts := make([]*mount, 0, len(p.registrations))
	byPath := make(map[string]*mount, len(p.registrations))
	for _, registration := range p.registrations {
		m, ok := byPath[registration.path]
		if !ok {
			// gin.New, not gin.Default: Default installs Gin's own logger and
			// recovery, and a mounted subtree must report through the host's
			// middleware, which the registrars install themselves. Two
			// recoveries would also mean the inner one swallows a panic the
			// host's request log was meant to record.
			m = &mount{path: registration.path, engine: gin.New()}
			byPath[m.path] = m
			mounts = append(mounts, m)
		}
		registration.registrar.RegisterGin(m.engine)
	}

	for _, m := range mounts {
		// Auth(web.Public()) is an explicit declaration, not an exemption: the
		// mounted engine carries the host's own authentication middleware, so
		// XBC's authentication chain is not what protects the subtree. The rest
		// of the framework still applies -- request ids, access logs, body
		// limits, and the in-flight gate wrap the mount from the outside.
		//
		// The relative path is empty because the group already carries the
		// prefix; Mount resolves it to the group's own path.
		router.Group(m.path).Auth(web.Public()).Mount("", m.engine)
	}
}
