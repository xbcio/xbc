package session

import (
	"errors"

	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable Definition identity of the Web session adapter. It is
// distinct from the neutral session plugin's own key, session, which owns the
// plugins.session section: the adapter has no activation of its own and stays
// tied to that plugin.
const Key plugin.Key = "session-http"

// Manager is the handler-facing session API for Web applications: the neutral
// lifecycle plus installing and clearing the opaque session cookie. Create and
// Rotate return the value whose ID SetCookie installs.
//
// Manager intentionally keeps its original method set: it embeds the neutral
// Manager and adds only the two cookie methods. Login, Logout, Refresh, and
// Patch are the request-scoped helpers on Handler; a consumer that wants both
// resolves the plugin's primary *Plugin value (plugin.RefTo[*Plugin]) rather
// than widening this contract. The neutral plugin exports its extended
// sessioncore.AttributeManager separately for verification and in-place
// attribute updates.
type Manager interface {
	sessioncore.Manager
	SetCookie(c *web.Ctx, value sessioncore.Session) error
	ClearCookie(c *web.Ctx)
}

// Plugin carries session IDs for the neutral session authenticator: it
// extracts the ID from the configured carrier chain and installs or clears the
// credential for handlers. Every session-lifecycle and verification decision
// stays with the neutral plugin. extended is the same neutral plugin viewed
// through its AttributeManager contract, so the helpers that verify or update
// attributes depend on that interface rather than on the concrete plugin.
type Plugin struct {
	sessioncore.Manager
	core          *sessioncore.Plugin
	extended      sessioncore.AttributeManager
	cookieCarrier *CookieCarrier
	carriers      *carrierChain
	writer        Carrier
}

var (
	_ web.CredentialExtractor = (*Plugin)(nil)
	_ Manager                 = (*Plugin)(nil)
	_ Handler                 = (*Plugin)(nil)
)

var sessionInput = plugin.RefTo[*sessioncore.Plugin](sessioncore.Key)

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Plugin]{
		// The adapter follows the session plugin it pairs with: configuring
		// plugins.session turns both on, and leaving it out leaves both dormant.
		// The optional plugins.session-http section only selects carriers; it
		// never activates the adapter.
		Activation: plugin.WhenConfigured("plugins." + sessioncore.Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.CredentialExtractor](func(value *Plugin) web.CredentialExtractor { return value }),
			plugin.ExportAs(func(value *Plugin) Manager { return value }),
		),
	},
)

// bundle selects the neutral session plugin alongside the adapter. An adapter
// without the session plugin it feeds is not a composition anyone wants, and
// RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(sessioncore.Bundle(), plugin.BundleOf(definition))

// plan builds the carrier chain from the prepared configuration. The planner
// itself acquires no resource; the returned Plan's factory runs once the graph
// has wired the neutral session plugin.
func plan(config Config) (plugin.Plan[*Plugin], error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return plugin.Plan[*Plugin]{}, err
	}
	return plugin.PlanOf(plugin.Inputs(sessionInput), func(ctx plugin.BuildContext) (*Plugin, error) {
		return newFromConfig(sessionInput.Get(ctx).Value, normalized)
	}), nil
}

// Option customizes a directly constructed Plugin.
type Option func(*options)

type options struct {
	config      Config
	configSet   bool
	carriers    []Carrier
	carriersSet bool
}

// WithConfig selects the built-in carriers from an explicit configuration. It
// is mutually exclusive with WithCarriers.
func WithConfig(config Config) Option {
	return func(built *options) {
		built.config = config
		built.configSet = true
	}
}

// WithCarriers supplies an explicit ordered carrier chain, replacing the
// built-in selection. At least one carrier is required, nil entries are
// refused, and at most one may be writable. It is mutually exclusive with
// WithConfig.
func WithCarriers(carriers ...Carrier) Option {
	return func(built *options) {
		built.carriersSet = true
		built.carriers = append([]Carrier(nil), carriers...)
	}
}

// New builds a Web session adapter over the neutral session plugin. Without
// options it behaves exactly as before the carrier abstraction existed: the
// cookie configured by plugins.session, and nothing else.
func New(core *sessioncore.Plugin, opts ...Option) (*Plugin, error) {
	if core == nil {
		return nil, errors.New("session-http: requires the session plugin")
	}
	var built options
	for _, opt := range opts {
		if opt != nil {
			opt(&built)
		}
	}
	if built.configSet && built.carriersSet {
		return nil, errors.New("session-http: WithConfig and WithCarriers are mutually exclusive")
	}
	cookie, err := NewCookieCarrier(core.Cookie())
	if err != nil {
		return nil, err
	}
	if built.carriersSet {
		return newPlugin(core, cookie, built.carriers)
	}
	normalized, err := normalizeConfig(built.config)
	if err != nil {
		return nil, err
	}
	return newFromConfig(core, normalized)
}

// newFromConfig resolves the configured carrier names and builds the adapter.
func newFromConfig(core *sessioncore.Plugin, config normalizedConfig) (*Plugin, error) {
	if core == nil {
		return nil, errors.New("session-http: requires the session plugin")
	}
	cookie, err := NewCookieCarrier(core.Cookie())
	if err != nil {
		return nil, err
	}
	carriers, err := buildCarriers(config, cookie)
	if err != nil {
		return nil, err
	}
	return newPlugin(core, cookie, carriers)
}

func newPlugin(core *sessioncore.Plugin, cookie *CookieCarrier, carriers []Carrier) (*Plugin, error) {
	chain, err := newCarrierChain(carriers)
	if err != nil {
		return nil, err
	}
	return &Plugin{
		Manager:       core.Manager(),
		core:          core,
		extended:      core,
		cookieCarrier: cookie,
		carriers:      chain,
		writer:        chain.writer,
	}, nil
}

// Definition returns the Web adapter's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the Web adapter
// and the neutral session plugin it feeds.
func Bundle() plugin.Bundle { return bundle }

// SetCookie installs value's opaque ID as the configured session cookie. It
// refuses a stopped session plugin, a malformed ID, and an expired session.
// It writes the cookie regardless of which carriers the chain selected: this
// method is explicitly about the cookie.
func (p *Plugin) SetCookie(c *web.Ctx, value sessioncore.Session) error {
	remaining, err := p.core.Lifetime(value)
	if err != nil {
		return err
	}
	if c == nil || c.Writer() == nil {
		return errors.New("session: cookie context cannot be nil")
	}
	p.cookieCarrier.set(c, value, remaining)
	return nil
}

// ClearCookie expires the configured session cookie on the client. It does not
// revoke the server-side session; call Revoke for that.
func (p *Plugin) ClearCookie(c *web.Ctx) {
	p.cookieCarrier.Clear(c)
}
