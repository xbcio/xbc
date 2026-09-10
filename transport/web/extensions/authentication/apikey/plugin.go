package apikey

import (
	"errors"
	"reflect"

	"github.com/xbcio/xbc/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and Definition identity of the API-key
// plugin.
const Key plugin.Key = "apikey"

type runtimeState struct {
	config normalizedConfig
	repo   Repository
	static *StaticRepository
}

// Plugin authenticates HTTP requests through a pluggable digest repository
// and contributes a credential extractor and an authenticator to the Web
// transport's authentication middleware. It is fully constructed before New
// or its Definition's factory returns.
type Plugin struct {
	state *runtimeState
}

type constructorOptions struct {
	repository    Repository
	repositorySet bool
}

// Option customizes direct Plugin construction.
type Option func(*constructorOptions)

// WithRepository uses repository instead of Config.Static. It is useful for
// directly constructed plugins whose credentials live in an external store.
func WithRepository(repository Repository) Option {
	return func(options *constructorOptions) {
		options.repository = repository
		options.repositorySet = true
	}
}

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return New(cfg)
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

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// New constructs a directly usable Plugin. Without WithRepository, cfg.Static
// is validated and compiled into the built-in repository before New returns.
// The two repository sources are mutually exclusive.
func New(cfg Config, options ...Option) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}

	settings := constructorOptions{}
	for _, option := range options {
		if option != nil {
			option(&settings)
		}
	}

	var repository Repository
	var static *StaticRepository
	if settings.repositorySet {
		if isNilRepository(settings.repository) {
			return nil, errors.New("apikey: WithRepository requires a non-nil repository")
		}
		if len(cfg.Static) != 0 {
			return nil, errors.New("apikey: configure either static credentials or an injected repository, not both")
		}
		repository = settings.repository
	} else {
		static, err = NewStaticRepository(cfg.Static)
		if err != nil {
			return nil, err
		}
		repository = static
	}

	return &Plugin{state: &runtimeState{
		config: normalized,
		repo:   repository,
		static: static,
	}}, nil
}

func isNilRepository(repository Repository) bool {
	if repository == nil {
		return true
	}
	value := reflect.ValueOf(repository)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Definition returns apikey's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns apikey's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// ReplaceStaticCredentials atomically rotates the built-in repository. It is
// unavailable when the plugin uses an injected Repository.
func (p *Plugin) ReplaceStaticCredentials(credentials []StaticCredential) error {
	if p == nil || p.state == nil {
		return errors.New("apikey: static rotation requires a constructed plugin")
	}
	if p.state.static == nil {
		return errors.New("apikey: static rotation is unavailable with an injected repository")
	}
	return p.state.static.Replace(credentials)
}
