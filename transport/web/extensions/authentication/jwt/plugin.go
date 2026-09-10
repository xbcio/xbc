package jwt

import (
	"time"

	"github.com/xbcio/xbc/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable Definition and configuration identity of the JWT plugin.
const Key plugin.Key = "jwt"

// Plugin validates and issues HMAC JWTs and contributes a credential extractor
// and an authenticator to the Web transport's authentication middleware. A
// constructed Plugin's compiled configuration is immutable; nothing about
// verification depends on state that changes after construction.
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
			plugin.ExportAs[web.CredentialExtractor](func(value *Plugin) web.CredentialExtractor { return value }),
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

func (p *Plugin) clock() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}
