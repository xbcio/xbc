package jwt

import (
	"time"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable Definition and configuration identity of the JWT plugin.
const Key plugin.Key = "jwt"

// Plugin validates and issues HMAC JWTs. It is the protocol-neutral half of JWT
// authentication: it exports an authentication.Authenticator and publishes the
// configured credential carriage (Header and TokenScheme) for the transport
// adapters that extract the token from a request. A constructed Plugin's
// compiled configuration is immutable; nothing about verification depends on
// state that changes after construction.
type Plugin struct {
	compiled *compiledConfig

	// now is a field only to make expiration behavior deterministic in tests.
	now func() time.Time
}

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[authentication.Authenticator](func(value *Plugin) authentication.Authenticator { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable Plugin from cfg. Secret has no default and
// must contain at least 32 bytes.
func New(cfg Config) (*Plugin, error) {
	return newPlugin(cfg)
}

func newPlugin(cfg Config) (*Plugin, error) {
	return newPluginWithClock(cfg, time.Now)
}

func newPluginWithClock(cfg Config, now func() time.Time) (*Plugin, error) {
	compiled, err := compileConfig(cfg, now)
	if err != nil {
		return nil, err
	}
	return &Plugin{compiled: compiled, now: now}, nil
}

// Definition returns JWT's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns JWT's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// Header returns the normalized header field name that carries the token. It
// names an HTTP header for the Web adapter and the equivalent metadata key for
// any other transport that carries header fields.
func (p *Plugin) Header() string { return p.compiled.header }

// TokenScheme returns the normalized authorization scheme that prefixes the
// token inside Header ("Bearer" by default). It is also the challenge every
// outcome of this plugin carries.
func (p *Plugin) TokenScheme() string { return p.compiled.scheme }

func (p *Plugin) clock() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}
