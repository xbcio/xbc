package federation

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xbcio/xbc/plugin"
)

const (
	// BackendMemory keeps login transactions in this process. It is an
	// explicit single-replica choice: an authorization request served by one
	// replica and a callback served by another would not find the transaction.
	BackendMemory = "memory"
	// BackendRedis shares login transactions through the named Redis client
	// instance. It is the default because a deployment that scales past one
	// replica must keep working without a configuration change.
	BackendRedis = "redis"

	defaultBackend        = BackendRedis
	defaultRedisPrefix    = "xbc:federation:"
	defaultCookieName     = "xbc_federation"
	defaultCookiePath     = "/"
	defaultCookieSameSite = "lax"
	defaultFailurePath    = "/login/failed"
	defaultReturnTo       = "/"
)

const (
	// MaxTransactionTTL is the hard upper bound on a login transaction's
	// lifetime. The configuration may shorten it but never extend it: a login
	// transaction that outlives the authorization round trip is only a longer
	// window for a replay.
	MaxTransactionTTL = 10 * time.Minute
	// MaxReservationTTL is the hard upper bound on a single set-if-absent
	// reservation. The SAML adapter uses that primitive as its assertion
	// replay cache, so the bound also caps how long the cache retains an
	// assertion ID.
	MaxReservationTTL = 24 * time.Hour
)

// Config is bound from plugins.federation-http.
type Config struct {
	// Backend is redis or memory. Redis is the default; memory is correct only
	// for a single-replica deployment and must be chosen explicitly.
	Backend string `yaml:"backend" default:"redis"`
	// RedisInstance names the Redis client instance the redis backend uses.
	RedisInstance string `yaml:"redis_instance" default:"default"`
	// RedisPrefix is prepended to every Redis key. Transaction and
	// reservation keys live under separate segments below it.
	RedisPrefix string `yaml:"redis_prefix" default:"xbc:federation:"`
	// TransactionTTL is how long a login transaction stays usable. It must be
	// positive and no greater than MaxTransactionTTL.
	TransactionTTL time.Duration `yaml:"transaction_ttl" default:"10m"`
	// CookieName is the name of the browser-binding cookie. It is not the
	// session cookie: that name belongs to plugins.session.
	CookieName string `yaml:"cookie_name" default:"xbc_federation"`
	// CookiePath is the binding cookie's path. It must cover both the start
	// and the callback route of every adapter.
	CookiePath string `yaml:"cookie_path" default:"/"`
	// CookieDomain is the binding cookie's domain. Empty means host-only.
	CookieDomain string `yaml:"cookie_domain"`
	// CookieSecure marks the binding cookie Secure. Keep it enabled outside
	// local development.
	CookieSecure bool `yaml:"cookie_secure" default:"true"`
	// CookieSameSite is lax, strict, or none. SAML deployments whose ACS
	// receives a cross-site POST need none.
	CookieSameSite string `yaml:"cookie_same_site" default:"lax"`
	// FailurePath is the in-application landing page a failed login redirects
	// to. The fixed error code travels in its error query parameter.
	FailurePath string `yaml:"failure_path" default:"/login/failed"`
	// DefaultReturnTo is where a login lands when return_to is absent or not
	// acceptable.
	DefaultReturnTo string `yaml:"default_return_to" default:"/"`
	// AllowedReturnOrigins is the complete allowlist of absolute origins a
	// return_to may name, each written as https://host[:port]. Empty means
	// only same-origin relative paths are accepted.
	AllowedReturnOrigins []string `yaml:"allowed_return_origins"`
}

// DefaultConfig returns the bounded defaults applied when a field is unset.
func DefaultConfig() Config {
	return Config{
		Backend:         defaultBackend,
		RedisInstance:   "default",
		RedisPrefix:     defaultRedisPrefix,
		TransactionTTL:  MaxTransactionTTL,
		CookieName:      defaultCookieName,
		CookiePath:      defaultCookiePath,
		CookieSecure:    true,
		CookieSameSite:  defaultCookieSameSite,
		FailurePath:     defaultFailurePath,
		DefaultReturnTo: defaultReturnTo,
	}
}

// Validate checks every statically checkable configuration invariant.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// prepareConfig implements plugin.ConfigSpec.Prepare: it validates c without
// acquiring any resource and returns it unchanged so plan(cfg) can rely on
// already-defaulted, already-validated values.
func prepareConfig(c Config) (Config, error) {
	if _, err := normalizeConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// normalizedConfig is the validated, parsed form of Config. It is built once
// per plugin instance and never mutated.
type normalizedConfig struct {
	backend        string
	redisInstance  string
	redisPrefix    string
	transactionTTL time.Duration
	cookieName     string
	cookiePath     string
	cookieDomain   string
	cookieSecure   bool
	cookieSameSite http.SameSite
	failurePath    string
	returnTo       returnToPolicy
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	d := DefaultConfig()
	if strings.TrimSpace(c.Backend) == "" {
		c.Backend = d.Backend
	}
	if strings.TrimSpace(c.RedisInstance) == "" {
		c.RedisInstance = d.RedisInstance
	}
	if c.RedisPrefix == "" {
		c.RedisPrefix = d.RedisPrefix
	}
	if c.TransactionTTL == 0 {
		c.TransactionTTL = d.TransactionTTL
	}
	if c.CookieName == "" {
		c.CookieName = d.CookieName
	}
	if c.CookiePath == "" {
		c.CookiePath = d.CookiePath
	}
	if strings.TrimSpace(c.CookieSameSite) == "" {
		c.CookieSameSite = d.CookieSameSite
	}
	if c.FailurePath == "" {
		c.FailurePath = d.FailurePath
	}
	if c.DefaultReturnTo == "" {
		c.DefaultReturnTo = d.DefaultReturnTo
	}

	backend := strings.ToLower(strings.TrimSpace(c.Backend))
	if backend != BackendMemory && backend != BackendRedis {
		return normalizedConfig{}, fmt.Errorf("federation-http: backend must be memory or redis")
	}
	if c.TransactionTTL < time.Millisecond || c.TransactionTTL > MaxTransactionTTL {
		return normalizedConfig{}, fmt.Errorf("federation-http: transaction_ttl must be at least 1ms and no greater than %s", MaxTransactionTTL)
	}
	if !validHTTPToken(c.CookieName) {
		return normalizedConfig{}, fmt.Errorf("federation-http: cookie_name must be a valid cookie name")
	}
	if !validCookiePath(c.CookiePath) {
		return normalizedConfig{}, fmt.Errorf("federation-http: cookie_path must be an absolute cookie path without control characters or semicolons")
	}
	if !validCookieDomain(c.CookieDomain) {
		return normalizedConfig{}, fmt.Errorf("federation-http: cookie_domain contains unsafe characters")
	}
	if strings.HasPrefix(c.CookieName, "__Host-") && (!c.CookieSecure || c.CookiePath != "/" || c.CookieDomain != "") {
		return normalizedConfig{}, fmt.Errorf("federation-http: __Host- cookies require secure=true, path=/, and no domain")
	}
	if strings.HasPrefix(c.CookieName, "__Secure-") && !c.CookieSecure {
		return normalizedConfig{}, fmt.Errorf("federation-http: __Secure- cookies require secure=true")
	}
	sameSite, err := parseSameSite(c.CookieSameSite)
	if err != nil {
		return normalizedConfig{}, err
	}
	if sameSite == http.SameSiteNoneMode && !c.CookieSecure {
		return normalizedConfig{}, fmt.Errorf("federation-http: SameSite=None requires secure=true")
	}
	redisInstance := plugin.NormalizeInstance(strings.TrimSpace(c.RedisInstance))
	if err := plugin.ValidateInstanceName(redisInstance); err != nil {
		return normalizedConfig{}, fmt.Errorf("federation-http: invalid redis_instance: %w", err)
	}
	if backend == BackendRedis && (strings.TrimSpace(c.RedisPrefix) == "" || containsControl(c.RedisPrefix)) {
		return normalizedConfig{}, fmt.Errorf("federation-http: redis_prefix cannot be empty or contain control characters")
	}

	failurePath, ok := sanitizeRelativePath(c.FailurePath)
	if !ok {
		return normalizedConfig{}, fmt.Errorf("federation-http: failure_path must be a same-origin relative path")
	}

	policy, err := newReturnToPolicy(c.DefaultReturnTo, c.AllowedReturnOrigins)
	if err != nil {
		return normalizedConfig{}, err
	}

	return normalizedConfig{
		backend:        backend,
		redisInstance:  redisInstance,
		redisPrefix:    c.RedisPrefix,
		transactionTTL: c.TransactionTTL,
		cookieName:     c.CookieName,
		cookiePath:     c.CookiePath,
		cookieDomain:   strings.TrimSpace(c.CookieDomain),
		cookieSecure:   c.CookieSecure,
		cookieSameSite: sameSite,
		failurePath:    failurePath,
		returnTo:       policy,
	}, nil
}

func parseSameSite(value string) (http.SameSite, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "lax":
		return http.SameSiteLaxMode, nil
	case "strict":
		return http.SameSiteStrictMode, nil
	case "none":
		return http.SameSiteNoneMode, nil
	default:
		return 0, fmt.Errorf("federation-http: cookie_same_site must be lax, strict, or none")
	}
}

// validHTTPToken reports whether value is a non-empty RFC 6265 cookie name.
func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)):
		default:
			return false
		}
	}
	return true
}

// validCookiePath reports whether value is an absolute cookie path free of
// control characters and semicolons.
func validCookiePath(value string) bool {
	if value == "" || value[0] != '/' {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < 0x20 || character == 0x7f || character == ';' {
			return false
		}
	}
	return true
}

// validCookieDomain reports whether value contains nothing that would break
// or inject into the Set-Cookie header.
func validCookieDomain(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character <= 0x20 || character == 0x7f || character == ';' || character == ',' || character == '"' {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
