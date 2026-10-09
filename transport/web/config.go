package web

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	defaultAddr                = ":8080"
	defaultBasePath            = "/"
	defaultReadTimeout         = 10 * time.Second
	defaultReadHeaderTimeout   = 5 * time.Second
	defaultWriteTimeout        = 30 * time.Second
	defaultIdleTimeout         = 60 * time.Second
	defaultMaxHeaderBytes      = 1 << 20 // 1 MiB
	defaultMaxRequestBodyBytes = 10 << 20
	defaultMaxMultipartMemory  = 8 << 20
	// defaultMaxInFlight is the admission ceiling applied when web.max_in_flight
	// is left unset. It counts requests rather than runnable goroutines, so it
	// is deliberately independent of the processor count; see resolveMaxInFlight.
	defaultMaxInFlight = 1024
)

// Config is Web's root-level "web" configuration section. The process-wide
// shutdown timeout deliberately does not live here: it governs the whole
// application, while Server.Stop drains only for the deadline supplied by the
// runtime.
//
// TrustedProxies is empty by default, which makes Gin ignore forwarded client
// addresses unless the deployment explicitly names trusted reverse proxies.
// MaxRequestBodyBytes is a transport-wide hard ceiling. MaxMultipartMemory only
// controls how much parsed multipart data remains in memory; the request-body
// ceiling still limits the complete request.
//
// MaxInFlight is the process-wide admission ceiling: the number of requests the
// Server serves at once. Zero -- the default -- applies a fixed cap of 1024
// requests, deliberately measured in requests rather than processors: the bound
// exists to stop a process accumulating unbounded concurrent work, while a
// request waiting on a database, an upstream call or a lock holds no processor
// at all, so an IO-bound instance can comfortably serve far more requests than
// it has cores. A positive value is the ceiling verbatim; size it from the
// deployment's own memory and dependency budget when the fixed default is wrong
// for it. The gate it feeds is assembled by the Server rather than contributed,
// so the limit cannot be removed by leaving a plugin out; see inflight.go.
//
// WriteTimeout is one deadline for a whole response, which is the right shape
// for request/response traffic and the wrong shape for a response with no known
// length. A route that streams -- server-sent events, a progress feed, a chunked
// export -- is severed mid-body when it outlives the budget, and it lifts the
// deadline for its own connection rather than the deployment raising it for
// every route:
//
//	http.NewResponseController(c.Writer()).SetWriteDeadline(time.Time{})
//
// That reaches the connection through the writer stack, wrappers included, so it
// works under gzip, request timeout and idempotency as well as on its own. It is
// per response, so nothing else loses its bound.
type Config struct {
	Addr                string           `yaml:"addr"                   default:":8080"   validate:"required"`
	BasePath            string           `yaml:"base_path"              default:"/"       validate:"required,startswith=/"`
	ReadTimeout         time.Duration    `yaml:"read_timeout"           default:"10s"     validate:"gt=0"`
	ReadHeaderTimeout   time.Duration    `yaml:"read_header_timeout"    default:"5s"      validate:"gt=0"`
	WriteTimeout        time.Duration    `yaml:"write_timeout"          default:"30s"     validate:"gt=0"`
	IdleTimeout         time.Duration    `yaml:"idle_timeout"           default:"60s"     validate:"gt=0"`
	MaxHeaderBytes      int              `yaml:"max_header_bytes"       default:"1048576" validate:"gt=0"`
	MaxRequestBodyBytes int64            `yaml:"max_request_body_bytes" default:"10485760" validate:"gt=0"`
	MaxMultipartMemory  int64            `yaml:"max_multipart_memory"   default:"8388608" validate:"gt=0"`
	MaxInFlight         int              `yaml:"max_in_flight"          default:"0"       validate:"gte=0"`
	TrustedProxies      []string         `yaml:"trusted_proxies"                          validate:"dive,required"`
	Shutdown            ShutdownConfig   `yaml:"shutdown"`
	Security            SecurityConfig   `yaml:"security"`
	Recovery            RecoveryConfig   `yaml:"recovery"`
	Management          ManagementConfig `yaml:"management"`
	TLS                 TLSConfig        `yaml:"tls"`
}

// ShutdownConfig controls Web-owned behavior after runtime cancellation. The
// shared application shutdown deadline remains owned by runtime; Web never
// creates a second budget.
type ShutdownConfig struct {
	// PreDrainDelay keeps a serving listener open after lifecycle contexts have
	// been cancelled and before http.Server.Shutdown closes it. This gives a
	// readiness endpoint an externally reachable propagation window. It is an
	// opportunity, not confirmation that a load balancer has removed the
	// instance, and normal application routes remain available during it.
	PreDrainDelay time.Duration `yaml:"pre_drain_delay" default:"0s" validate:"gte=0"`
}

// RecoveryConfig controls the Server-assembled panic boundary (PhaseRecover).
// There is deliberately no enable/disable switch: the boundary is a required
// stage, not an opt-in one, and the whole point of collapsing it from a plugin
// into this section is that a composition root can no longer leave it out.
//
// This is a value, not a pointer, field. A pointer sub-struct silently loses
// both environment-variable overrides and default-tag filling in this
// repository's configuration binder, which is a known trap -- see
// SecurityConfig and ShutdownConfig for the same shape.
type RecoveryConfig struct {
	// Stack includes a runtime stack in the panic boundary's log entry. Panic
	// values, request headers, query strings, and bodies are never logged
	// regardless of this setting.
	Stack bool `yaml:"stack" default:"true"`
}

const (
	// defaultManagementAddr is empty on purpose: an unset management address
	// means "no management listener", which is the behaviour every deployment
	// had before the section existed. An address is an explicit opt-in.
	defaultManagementAddr = ""
	// defaultTLSMinVersion is the floor a deployment gets without declaring
	// one. It matches what a bare net/http server negotiates today, so making
	// the value explicit changes no handshake; it only makes the floor
	// visible, and configurable, in the section an operator reads.
	defaultTLSMinVersion = "1.2"
	// defaultTLSClientAuth is "none" for the same reason: mutual TLS is a
	// deployment decision, never a default.
	defaultTLSClientAuth = "none"
)

// ManagementConfig declares a second, optional listener for operator
// endpoints. It exists because a metrics scrape or a CPU profile is not
// business traffic: it must be reachable while the admission gate is saturated
// and must not be reachable from wherever the business port is exposed.
//
// An empty Addr is the default and means there is no management listener at
// all: routes registered through Router.Management land on the serving
// listener, served exactly as if the scope did not exist. Configuring an
// address is what moves them onto their own listener, their own engine, and
// their own (framework-owned, admission-free) middleware chain; see
// Router.Management for the registration side of that contract.
//
// Access control on the management listener is the bound address itself, not
// an authentication policy: an operator surface with no business identity has
// no principal to authenticate. A hostless address (":9091") therefore binds
// the loopback interface, a host that is not loopback requires AllowRemote,
// and AllowRemote in turn requires web.tls.cert_file, so a management port is
// never exposed to a network unencrypted.
type ManagementConfig struct {
	Addr string `yaml:"addr" validate:"omitempty,hostname_port"`
	// AllowRemote permits binding a management address that is not loopback.
	// It is refused without a serving certificate, because the alternative --
	// an unauthenticated operator surface reachable from the network in
	// cleartext -- is never what an operator meant to configure.
	AllowRemote bool `yaml:"allow_remote"`
}

// TLSConfig terminates TLS on the serving listener from a certificate pair on
// disk. An empty CertFile is the default and serves plain HTTP, which is the
// shape a deployment behind a TLS-terminating proxy wants; setting it is what
// moves termination into this process.
//
// The management listener (ManagementConfig) reuses the same material, so a
// deployment gets one certificate, one client-auth policy, and one reload
// path for both ports.
//
// ReloadInterval is how often the certificate pair is allowed to be checked
// for a change, at most once per handshake: zero -- the default -- checks on
// every handshake. A nonzero value bounds the check rate rather than the
// staleness of the certificate: a rotate is picked up by the first handshake
// after the interval has elapsed, while established connections keep the
// certificate they negotiated until they next complete a handshake.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// MinVersion is the lowest TLS version accepted, spelled "1.2" or "1.3".
	// TLS 1.0 and 1.1 are deliberately not addressable: they are deprecated by
	// RFC 8996, and a framework that let a deployment name them would be
	// offering a downgrade it knows to be unsafe.
	MinVersion string `yaml:"min_version" default:"1.2" validate:"omitempty,oneof=1.2 1.3"`
	// ClientCAFile is the PEM bundle client certificates are verified against.
	// It belongs with ClientAuth: a CA with no verification mode has nothing to
	// verify, and a mode with no CA could only accept certificates it cannot
	// validate.
	ClientCAFile string `yaml:"client_ca_file"`
	// ClientAuth is one of none, verify_if_given, or require_and_verify. The
	// two verify modes both require ClientCAFile. The modes Go names but this
	// section does not expose -- request, require_any -- accept a certificate
	// without verifying it, which is a state that reads as mutual TLS in a
	// review and authenticates nobody.
	ClientAuth string `yaml:"client_auth" default:"none" validate:"omitempty,oneof=none verify_if_given require_and_verify"`
	// CipherSuites restricts the TLS 1.2 cipher suites by Go name, for example
	// TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256. It has no effect on TLS 1.3,
	// whose suites are not configurable in Go; see tls.CipherSuites for why
	// the names accepted here are the secure set only. Empty -- the default --
	// uses Go's own defaults.
	CipherSuites   []string      `yaml:"cipher_suites" validate:"dive,required"`
	ReloadInterval time.Duration `yaml:"reload_interval" default:"0s" validate:"gte=0"`
}

// DefaultConfig returns the production-safe defaults used by New and by the
// configuration binder.
func DefaultConfig() Config {
	return Config{
		Addr:                defaultAddr,
		BasePath:            defaultBasePath,
		ReadTimeout:         defaultReadTimeout,
		ReadHeaderTimeout:   defaultReadHeaderTimeout,
		WriteTimeout:        defaultWriteTimeout,
		IdleTimeout:         defaultIdleTimeout,
		MaxHeaderBytes:      defaultMaxHeaderBytes,
		MaxRequestBodyBytes: defaultMaxRequestBodyBytes,
		MaxMultipartMemory:  defaultMaxMultipartMemory,
		Shutdown:            ShutdownConfig{PreDrainDelay: 0},
		Security:            SecurityConfig{Default: SecurityDeny},
		Recovery:            RecoveryConfig{Stack: true},
		Management:          ManagementConfig{Addr: defaultManagementAddr},
		TLS: TLSConfig{
			MinVersion:     defaultTLSMinVersion,
			ClientAuth:     defaultTLSClientAuth,
			ReloadInterval: 0,
		},
	}
}

// Validate checks constraints that cannot be expressed by configuration tags.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return fmt.Errorf("web: addr cannot be empty")
	}
	if c.BasePath == "" || c.BasePath[0] != '/' || strings.ContainsAny(c.BasePath, "?#\r\n") {
		return fmt.Errorf("web: base_path must be an absolute HTTP path without a query or fragment")
	}
	if c.ReadTimeout <= 0 || c.ReadHeaderTimeout <= 0 || c.WriteTimeout <= 0 || c.IdleTimeout <= 0 {
		return fmt.Errorf("web: server timeouts must be greater than zero")
	}
	if c.MaxHeaderBytes <= 0 || c.MaxRequestBodyBytes <= 0 || c.MaxMultipartMemory <= 0 {
		return fmt.Errorf("web: request size limits must be greater than zero")
	}
	if c.MaxInFlight < 0 {
		return fmt.Errorf("web: max_in_flight must not be negative")
	}
	if c.Shutdown.PreDrainDelay < 0 {
		return fmt.Errorf("web: shutdown.pre_drain_delay must not be negative")
	}
	for _, proxy := range c.TrustedProxies {
		if proxy == "" || proxy != strings.TrimSpace(proxy) {
			return fmt.Errorf("web: trusted_proxies contains an empty or whitespace-padded address")
		}
		if strings.Contains(proxy, "/") {
			if _, _, err := net.ParseCIDR(proxy); err != nil {
				return fmt.Errorf("web: trusted proxy %q is not a valid CIDR: %w", proxy, err)
			}
			continue
		}
		if net.ParseIP(proxy) == nil {
			return fmt.Errorf("web: trusted proxy %q is not a valid IP address", proxy)
		}
	}
	if err := c.Security.Validate(); err != nil {
		return err
	}
	if err := c.Management.validate(c.TLS); err != nil {
		return err
	}
	if err := c.TLS.validate(); err != nil {
		return err
	}
	return nil
}

// validate checks the management listener's own settings against the serving
// TLS material. The two sections are validated together because the access
// rule spans both: an address reachable from the network is only permissible
// when the process terminates TLS itself.
func (m ManagementConfig) validate(tlsConfig TLSConfig) error {
	if strings.TrimSpace(m.Addr) == "" {
		if m.AllowRemote {
			return fmt.Errorf("web: management.allow_remote is set but management.addr is empty, so there is no management listener to permit")
		}
		return nil
	}
	if m.Addr != strings.TrimSpace(m.Addr) {
		return fmt.Errorf("web: management.addr must not be padded with whitespace")
	}
	host, port, err := net.SplitHostPort(m.Addr)
	if err != nil {
		return fmt.Errorf("web: management.addr %q is not a host:port address: %w", m.Addr, err)
	}
	if port == "" {
		return fmt.Errorf("web: management.addr %q must name a port", m.Addr)
	}
	if !managementHostIsLoopback(host) && !m.AllowRemote {
		return fmt.Errorf(
			"web: management.addr %q binds an address that is not loopback\n"+
				"  → the management listener carries no business authentication, so its access control is the bound address; set management.allow_remote: true (which requires web.tls.cert_file) to expose it, or leave the host out to bind loopback",
			m.Addr)
	}
	if m.AllowRemote && strings.TrimSpace(tlsConfig.CertFile) == "" {
		return fmt.Errorf(
			"web: management.allow_remote requires web.tls.cert_file\n" +
				"  → a remotely reachable management listener is an unauthenticated operator surface; the certificate is what keeps it off a cleartext network",
		)
	}
	return nil
}

// managementHostIsLoopback reports whether a management address's host part
// names this machine only. An empty host -- the ":9091" spelling, which a bare
// net.Listen would bind to every interface -- counts as loopback because the
// Server resolves it to the loopback interface before listening; "localhost"
// counts because it resolves to the loopback interface. Every
// other host, IP or name, is treated as remote: a hostname this process cannot
// resolve offline might point anywhere, and the conservative reading is the
// one that demands explicit consent.
func managementHostIsLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bindAddr resolves Addr to the address the Server actually listens on. The
// resolution exists because a hostless spelling does not mean what it looks
// like once it reaches net.Listen: ":9091" binds every interface, which on this
// listener would publish an operator surface that carries no authentication to
// the network. Filling the missing host with loopback is what makes
// managementHostIsLoopback's reading of an empty host true rather than
// aspirational -- a config that validated as loopback-only must be reachable
// from this machine only.
//
// An address that already names a host is passed through untouched, including
// the explicit remote spellings allow_remote exists to consent to.
func (m ManagementConfig) bindAddr() string {
	host, port, err := net.SplitHostPort(m.Addr)
	if err != nil || host != "" {
		return m.Addr
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// validate checks the TLS material and the declarations around it. It does
// not read the certificate: whether the files exist and parse is a startup
// question with a startup error, answered where the listener is configured
// (see tlsMaterial), not by a configuration validator whose contract is to
// stay I/O-free.
func (c TLSConfig) validate() error {
	certFile := strings.TrimSpace(c.CertFile)
	keyFile := strings.TrimSpace(c.KeyFile)
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("web: tls.cert_file and tls.key_file must be set together")
	}
	if c.CertFile != certFile || c.KeyFile != keyFile {
		return fmt.Errorf("web: tls.cert_file and tls.key_file must not be padded with whitespace")
	}
	if certFile == "" {
		for _, declaration := range []struct {
			set     bool
			message string
		}{
			{c.ClientCAFile != "", "web: tls.client_ca_file is set but tls.cert_file is empty, so there is no TLS listener to verify clients on"},
			{c.ReloadInterval > 0, "web: tls.reload_interval is set but tls.cert_file is empty, so there is no certificate to reload"},
		} {
			if declaration.set {
				return fmt.Errorf("%s", declaration.message)
			}
		}
		return nil
	}
	if _, err := c.minVersion(); err != nil {
		return err
	}
	clientAuth, err := c.clientAuth()
	if err != nil {
		return err
	}
	verified := clientAuth == tls.VerifyClientCertIfGiven || clientAuth == tls.RequireAndVerifyClientCert
	switch {
	case verified && strings.TrimSpace(c.ClientCAFile) == "":
		return fmt.Errorf(
			"web: tls.client_auth %q requires tls.client_ca_file\n"+
				"  → without a CA bundle no client certificate can be verified, so the setting would authenticate nobody",
			c.ClientAuth)
	case !verified && strings.TrimSpace(c.ClientCAFile) != "":
		return fmt.Errorf(
			"web: tls.client_ca_file is set but tls.client_auth is %q\n"+
				"  → a CA bundle only has a job when client certificates are verified; set client_auth to verify_if_given or require_and_verify",
			c.ClientAuth)
	}
	if _, err := c.cipherSuiteIDs(); err != nil {
		return err
	}
	if c.ReloadInterval < 0 {
		return fmt.Errorf("web: tls.reload_interval must not be negative")
	}
	return nil
}

// minVersion resolves the configured version floor. An empty value means the
// default, so a directly constructed Config -- which never passed through tag
// defaulting -- validates without having to spell the default out.
func (c TLSConfig) minVersion() (uint16, error) {
	switch c.MinVersion {
	case "", defaultTLSMinVersion:
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("web: tls.min_version %q is not supported; use %q or %q", c.MinVersion, defaultTLSMinVersion, "1.3")
	}
}

// clientAuth resolves the configured client-certificate policy. Like
// minVersion, an empty value means the default.
func (c TLSConfig) clientAuth() (tls.ClientAuthType, error) {
	switch c.ClientAuth {
	case "", defaultTLSClientAuth:
		return tls.NoClientCert, nil
	case "verify_if_given":
		return tls.VerifyClientCertIfGiven, nil
	case "require_and_verify":
		return tls.RequireAndVerifyClientCert, nil
	default:
		return tls.NoClientCert, fmt.Errorf(
			"web: tls.client_auth %q is not supported; use %q, %q, or %q",
			c.ClientAuth, defaultTLSClientAuth, "verify_if_given", "require_and_verify")
	}
}

// cipherSuiteIDs resolves the configured cipher-suite names against
// tls.CipherSuites -- the secure set. Naming an insecure suite is refused
// rather than honoured: the whole reason to write a suite list is to restrict
// what gets negotiated, and a list containing RC4 or 3DES does the opposite.
// Names are matched case-sensitively against Go's own, which is also how a
// typo is caught here instead of silently restricting nothing.
func (c TLSConfig) cipherSuiteIDs() ([]uint16, error) {
	if len(c.CipherSuites) == 0 {
		return nil, nil
	}
	known := make(map[string]uint16)
	for _, suite := range tls.CipherSuites() {
		known[suite.Name] = suite.ID
	}
	ids := make([]uint16, 0, len(c.CipherSuites))
	seen := make(map[uint16]string, len(c.CipherSuites))
	for _, name := range c.CipherSuites {
		id, ok := known[name]
		if !ok {
			return nil, fmt.Errorf(
				"web: tls.cipher_suites names %q, which is not a secure TLS cipher suite known to this Go version",
				name)
		}
		if previous, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("web: tls.cipher_suites names %q twice (as %q)", name, previous)
		}
		seen[id] = name
		ids = append(ids, id)
	}
	return ids, nil
}

// normalizeConfig supplies defaults for direct Server construction. Assembly
// binding already applies the same defaults and rejects explicitly configured
// zero values before Init runs, but direct tests and embedding hosts should be
// safe without having to reproduce the binder lifecycle.
func normalizeConfig(c Config) (Config, error) {
	defaults := DefaultConfig()
	if c.Addr == "" {
		c.Addr = defaults.Addr
	}
	if c.BasePath == "" {
		c.BasePath = defaults.BasePath
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = defaults.ReadTimeout
	}
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = defaults.ReadHeaderTimeout
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = defaults.WriteTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = defaults.IdleTimeout
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = defaults.MaxHeaderBytes
	}
	if c.MaxRequestBodyBytes == 0 {
		c.MaxRequestBodyBytes = defaults.MaxRequestBodyBytes
	}
	if c.MaxMultipartMemory == 0 {
		c.MaxMultipartMemory = defaults.MaxMultipartMemory
	}
	c.TrustedProxies = append([]string(nil), c.TrustedProxies...)
	c.Security = c.Security.normalize()
	// The TLS sub-struct gets the same treatment: its defaults are values
	// ("1.2", "none") rather than zero values, so a directly constructed Config
	// must have them filled the way tag defaulting fills them during binding.
	if c.TLS.MinVersion == "" {
		c.TLS.MinVersion = defaults.TLS.MinVersion
	}
	if c.TLS.ClientAuth == "" {
		c.TLS.ClientAuth = defaults.TLS.ClientAuth
	}
	c.TLS.CipherSuites = append([]string(nil), c.TLS.CipherSuites...)
	return c, c.Validate()
}
