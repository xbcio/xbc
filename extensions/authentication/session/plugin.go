package session

import (
	"context"
	"fmt"
	"net/http"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is this plugin's stable configuration, dependency, and Definition
// identity.
const Key plugin.Key = "session"

// redisPluginKey mirrors the stable plugin.Key identity owned by the optional
// extensions/storage/redis plugin. session must not import that sibling
// integration module (each optional integration is independently
// versioned), so the identity is duplicated here as a typed constant.
const redisPluginKey plugin.Key = "redis"

// Plugin owns session persistence. It is the protocol-neutral half of session
// authentication: it exports an authentication.Authenticator for opaque
// session IDs, the Manager that creates, rotates, and revokes them, and the
// AttributeManager that verifies them and maintains their attributes in place.
// It also publishes the configured cookie carriage (Cookie, Lifetime) for the
// transport adapter that moves the ID to and from the client. It embeds
// *manager so it satisfies both manager contracts directly through promoted
// methods, which plugin.ExportAs requires of a Definition's primary type.
type Plugin struct {
	config normalizedConfig
	*manager
	owned *MemoryStore
}

var (
	_ Manager          = (*Plugin)(nil)
	_ AttributeManager = (*Plugin)(nil)
)

type options struct {
	store     Store
	observers []Observer
}

// Option customizes a programmatically assembled plugin.
type Option func(*options)

// WithStore injects a Store and bypasses configured backend construction. Its
// lifecycle remains caller-owned.
func WithStore(store Store) Option { return func(o *options) { o.store = store } }

// WithObserver registers an observer for the plugin's session events. It may
// be supplied more than once; observers are notified in the order supplied.
// A nil observer is ignored. Observers contributed by other plugins through
// ObserverContributor are notified as well when the plugin is assembled
// through its Definition.
func WithObserver(observer Observer) Option {
	return func(o *options) {
		if !isNilObserver(observer) {
			o.observers = append(o.observers, observer)
		}
	}
}

// observerContributors gathers observers exported by other Definitions.
var observerContributors = plugin.Collect[ObserverContributor]()

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[authentication.Authenticator](func(value *Plugin) authentication.Authenticator { return value }),
			plugin.ExportAs(func(value *Plugin) Manager { return value }),
			plugin.ExportAs(func(value *Plugin) AttributeManager { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns session's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns session's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// plan selects the Store's inputs from the prepared config: both backends take
// the contributed observers, while the redis backend additionally requires the
// named Redis client instance the configuration selects. The planner itself
// acquires no resource; the returned Plan's factory does that once the graph
// has wired the declared inputs.
func plan(cfg Config) (plugin.Plan[*Plugin], error) {
	config, err := normalizeConfig(cfg)
	if err != nil {
		return plugin.Plan[*Plugin]{}, err
	}

	if config.backend == BackendMemory {
		return plugin.PlanOf(plugin.Inputs(observerContributors), func(context plugin.BuildContext) (*Plugin, error) {
			return newMemoryPlugin(config, collectObservers(observerContributors.Get(context)))
		}), nil
	}

	client := plugin.RefToInstance[goredis.UniversalClient](redisPluginKey, config.redisInstance)
	return plugin.PlanOf(plugin.Inputs(observerContributors, client), func(context plugin.BuildContext) (*Plugin, error) {
		value := client.Get(context).Value
		if value == nil {
			return nil, fmt.Errorf("session: Redis client instance %q is unavailable", config.redisInstance)
		}
		store, err := NewRedisStore(value, config.redisPrefix)
		if err != nil {
			return nil, err
		}
		return newPlugin(config, store, nil, collectObservers(observerContributors.Get(context)))
	}), nil
}

// New constructs a plugin from an explicit configuration. The redis backend
// requires WithStore for direct construction outside the Definition-driven
// composition, which resolves a named Redis client from the graph instead.
func New(cfg Config, opts ...Option) (*Plugin, error) {
	var built options
	for _, opt := range opts {
		if opt != nil {
			opt(&built)
		}
	}
	config, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if built.store != nil {
		return newPlugin(config, built.store, nil, built.observers)
	}
	if config.backend != BackendMemory {
		return nil, fmt.Errorf("session: backend %q requires WithStore(...) for direct construction", config.backend)
	}
	return newMemoryPlugin(config, built.observers)
}

func newMemoryPlugin(config normalizedConfig, observers []Observer) (*Plugin, error) {
	owned, err := NewMemoryStore(config.cleanupInterval)
	if err != nil {
		return nil, err
	}
	return newPlugin(config, owned, owned, observers)
}

// newPlugin wires the store's optional capabilities. A positive max_sessions
// needs the subject index, so construction fails when the store has none
// instead of letting the first Create discover it: an application that asked
// for a session bound must not start without one. A MemoryStore also receives
// the observer list so its expiry sweeper can report EventExpired.
func newPlugin(config normalizedConfig, store Store, owned *MemoryStore, observers []Observer) (*Plugin, error) {
	if config.maxSessions > 0 {
		if _, ok := store.(IndexedStore); !ok {
			return nil, fmt.Errorf("session: max_sessions requires a Store that implements IndexedStore")
		}
	}
	if memory, ok := store.(*MemoryStore); ok {
		memory.setObservers(observers)
	}
	return &Plugin{
		config:  config,
		manager: &manager{store: store, cfg: config, observers: append([]Observer(nil), observers...)},
		owned:   owned,
	}, nil
}

// collectObservers flattens the contributed observers in graph order. A
// contributor is best-effort telemetry: a nil contributor, a nil observer, and
// a panicking Observers callback are recorded through the log facade and
// skipped instead of failing application construction.
func collectObservers(contributors []plugin.Entry[ObserverContributor]) []Observer {
	var observers []Observer
	for _, entry := range contributors {
		for _, observer := range contributorObservers(entry) {
			if !isNilObserver(observer) {
				observers = append(observers, observer)
			}
		}
	}
	return observers
}

func contributorObservers(entry plugin.Entry[ObserverContributor]) (observers []Observer) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.L().Error("session: session observer contributor panicked", "plugin", entry.Identity.String())
			observers = nil
		}
	}()
	if isNilObserver(entry.Value) {
		return nil
	}
	return entry.Value.Observers()
}

// Manager returns this plugin's handler-facing session API. The Definition
// exports the same value as AttributeManager for consumers that need
// verification or in-place attribute updates.
func (p *Plugin) Manager() Manager { return p.manager }

// CookieSettings is the normalized cookie carriage from plugins.session. The
// neutral half validates it with the rest of the configuration; only a
// transport that carries sessions in HTTP cookies applies it.
type CookieSettings struct {
	Name     string
	Path     string
	Domain   string
	HTTPOnly bool
	Secure   bool
	SameSite http.SameSite
}

// Cookie returns the configured cookie carriage.
func (p *Plugin) Cookie() CookieSettings {
	return CookieSettings{
		Name:     p.config.name,
		Path:     p.config.path,
		Domain:   p.config.domain,
		HTTPOnly: p.config.httpOnly,
		Secure:   p.config.secure,
		SameSite: p.config.sameSite,
	}
}

// Lifetime reports how long value's opaque ID may still be handed to a client.
// It returns ErrStopped once the plugin stopped, and ErrInvalidCookie for a
// malformed ID or an already expired session.
func (p *Plugin) Lifetime(value Session) (time.Duration, error) {
	return p.manager.lifetime(value)
}

// Stop revokes process-local sessions and stops memory cleanup. Injected and
// Redis stores are externally owned. Repeated and concurrent calls are safe
// because manager.stop and MemoryStore.Close are independently idempotent.
func (p *Plugin) Stop(context.Context) error {
	p.manager.stop()
	if p.owned != nil {
		return p.owned.Close()
	}
	return nil
}
