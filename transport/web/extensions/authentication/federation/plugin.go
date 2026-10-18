package federation

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	sessionweb "github.com/xbcio/xbc/transport/web/extensions/authentication/session"
)

// Key is the stable configuration, dependency, and Definition identity of the
// shared federation web base.
const Key plugin.Key = "federation-http"

// redisPluginKey mirrors the stable plugin.Key identity owned by the optional
// extensions/storage/redis plugin. This module must not import that sibling
// integration module (each optional integration is independently versioned),
// so the identity is duplicated here as a typed constant.
const redisPluginKey plugin.Key = "redis"

var (
	// resolverInput requires the application's identity policy. Missing or
	// duplicated implementations fail at plan time.
	resolverInput = plugin.RequireOne[federationcore.IdentityResolver]()
	// sessionInput requires exactly one Web session adapter.
	sessionInput = plugin.RequireOne[sessionweb.Manager]()
	// sessionCoreInput requires the neutral session plugin whose
	// configuration owns the session cookie name. Reading the name there is
	// what keeps this module from duplicating plugins.session.
	sessionCoreInput = plugin.RequireOne[*sessioncore.Plugin]()
)

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Plugin]{
		// The shared base exists only for a composition that selects it, and
		// the adapters that consume it are the reason to select it.
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(value *Plugin) Login { return value }),
		),
	},
)

// bundle selects the Web session adapter and the neutral session plugin it
// pairs with alongside the shared base: a federation login that cannot create
// a session is not a composition anyone wants, and the declared inputs would
// reject it at planning time anyway.
var bundle = plugin.CombineBundles(sessionweb.Bundle(), plugin.BundleOf(definition))

// Definition returns the shared base's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the shared base
// and the session plugins it hands sessions to.
func Bundle() plugin.Bundle { return bundle }

type options struct {
	store  Store
	now    func() time.Time
	logger log.Logger
}

// Option customizes a directly constructed Plugin.
type Option func(*options)

// WithStore injects a transaction store and bypasses backend construction. Its
// lifecycle remains caller-owned.
func WithStore(store Store) Option { return func(o *options) { o.store = store } }

// WithClock injects the clock used for transaction lifetimes and expiry
// checks.
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// WithLogger injects the logger internal failure detail is written to.
func WithLogger(logger log.Logger) Option { return func(o *options) { o.logger = logger } }

// New constructs a Plugin directly. The redis backend requires WithStore for
// direct construction outside the Definition-driven composition, which
// resolves a named Redis client from the graph instead.
//
// sessionCookieName is the configured name of the session cookie, normally
// sessioncore.Plugin.Cookie().Name. The Definition resolves it from the graph;
// a direct caller passes it explicitly.
func New(
	cfg Config,
	resolver federationcore.IdentityResolver,
	sessions sessionweb.Manager,
	sessionCookieName string,
	opts ...Option,
) (*Plugin, error) {
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
	store := built.store
	if store == nil {
		if config.backend != BackendMemory {
			return nil, fmt.Errorf("federation-http: backend %q requires WithStore(...) for direct construction", config.backend)
		}
		store = NewMemoryStore()
	}
	return newPlugin(config, resolver, sessions, sessionCookieName, store, built.now, built.logger)
}

// plan selects the backend's inputs from the prepared config. The planner
// itself acquires no resource; the returned Plan's factory does that once the
// graph has wired the declared inputs.
func plan(cfg Config) (plugin.Plan[*Plugin], error) {
	config, err := normalizeConfig(cfg)
	if err != nil {
		return plugin.Plan[*Plugin]{}, err
	}
	inputs := []plugin.Input{resolverInput, sessionInput, sessionCoreInput}
	if config.backend == BackendRedis {
		client := plugin.RefToInstance[goredis.UniversalClient](redisPluginKey, config.redisInstance)
		inputs = append(inputs, client)
		return plugin.PlanOf(plugin.Inputs(inputs...), func(ctx plugin.BuildContext) (*Plugin, error) {
			value := client.Get(ctx).Value
			if value == nil {
				return nil, fmt.Errorf("federation-http: Redis client instance %q is unavailable", config.redisInstance)
			}
			store, err := NewRedisStore(value, config.redisPrefix)
			if err != nil {
				return nil, err
			}
			return construct(ctx, config, store)
		}), nil
	}
	return plugin.PlanOf(plugin.Inputs(inputs...), func(ctx plugin.BuildContext) (*Plugin, error) {
		return construct(ctx, config, NewMemoryStore())
	}), nil
}

// construct binds the graph-resolved dependencies to one instance.
func construct(ctx plugin.BuildContext, config normalizedConfig, store Store) (*Plugin, error) {
	return newPlugin(
		config,
		resolverInput.Get(ctx).Value,
		sessionInput.Get(ctx).Value,
		sessionCoreInput.Get(ctx).Value.Cookie().Name,
		store,
		nil,
		ctx.Log(),
	)
}

func newPlugin(
	config normalizedConfig,
	resolver federationcore.IdentityResolver,
	sessions sessionweb.Manager,
	sessionCookieName string,
	store Store,
	now func() time.Time,
	logger log.Logger,
) (*Plugin, error) {
	if nilInterface(resolver) {
		return nil, errors.New("federation-http: requires an IdentityResolver")
	}
	if nilInterface(sessions) {
		return nil, errors.New("federation-http: requires a session manager")
	}
	if strings.TrimSpace(sessionCookieName) == "" {
		return nil, errors.New("federation-http: session cookie name cannot be empty")
	}
	if config.cookieName == sessionCookieName {
		return nil, fmt.Errorf("federation-http: cookie_name must differ from the session cookie name %q", sessionCookieName)
	}
	if nilInterface(store) {
		return nil, errors.New("federation-http: requires a transaction store")
	}
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = log.L()
	}
	return &Plugin{
		config:        config,
		resolver:      resolver,
		sessions:      sessions,
		sessionCookie: sessionCookieName,
		store:         store,
		now:           now,
		logger:        logger,
	}, nil
}

// nilInterface reports whether value is nil or a typed-nil implementation
// stored in an interface slot.
func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
