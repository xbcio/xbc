package session

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/xbcio/xbc/extensions/authentication"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web"
)

var (
	// ErrNoWritableCarrier reports that the configured carrier chain cannot
	// install a credential on a response. A chain of read-only carriers can
	// verify existing sessions but cannot log a new one in.
	ErrNoWritableCarrier = errors.New("session-http: no writable session carrier")
	// ErrNotWritable reports a write through a carrier that only reads.
	ErrNotWritable = errors.New("session-http: session carrier is read-only")
)

// Carrier is one request-scoped way a session ID travels between a client and
// the server.
//
// Extract classifies the request as Absent, Presented, or Malformed and never
// reports an operational error: a carrier that cannot answer is a construction
// problem, not a request outcome. Whether a presented value names a live
// session is the neutral session manager's decision, never the carrier's.
//
// A writable carrier installs the session's opaque ID on the response (Issue)
// and removes it again (Clear). At most one carrier in a chain may be
// writable, and a read-only carrier is not a degraded carrier: a header
// describes one request, so it has no client state to install or clear.
type Carrier interface {
	Extract(ctx *web.Ctx) authentication.CredentialResult
	Writable() bool
	Issue(ctx *web.Ctx, value sessioncore.Session) error
	Clear(ctx *web.Ctx)
}

// CookieCarrier carries the opaque session ID in an HTTP cookie. It is the
// default carrier and the only built-in one that writes.
//
// It reads the cookie carriage the neutral session plugin publishes, so a host
// configures cookie name, path, domain, and security attributes once in
// plugins.session. Session cookies contain only the cryptographically random
// identifier; all identity and attribute data stays server-side.
type CookieCarrier struct {
	settings sessioncore.CookieSettings
}

// NewCookieCarrier validates settings and returns a cookie carrier. The
// constructor refuses a cookie shape that would weaken the transport
// invariant (a script-readable cookie, an unsafe name, path, or domain), so a
// misconfiguration surfaces at construction rather than on a live response.
func NewCookieCarrier(settings sessioncore.CookieSettings) (*CookieCarrier, error) {
	if err := validateCookieSettings(settings); err != nil {
		return nil, err
	}
	return &CookieCarrier{settings: settings}, nil
}

// Writable reports true: a cookie carrier installs and clears the cookie.
func (*CookieCarrier) Writable() bool { return true }

// Extract reads the configured cookie. A cookie under the configured name
// that appears more than once is Malformed, not a silent first-wins: the
// carrier cannot pick a value in that case. An empty value is Malformed as
// well.
func (carrier *CookieCarrier) Extract(ctx *web.Ctx) authentication.CredentialResult {
	if carrier == nil || ctx == nil || ctx.Request() == nil {
		return authentication.Absent()
	}
	cookies := ctx.Request().CookiesNamed(carrier.settings.Name)
	switch len(cookies) {
	case 0:
		return authentication.Absent()
	case 1:
		if cookies[0] == nil || cookies[0].Value == "" {
			return authentication.Malformed(authentication.ReasonMalformedCredential)
		}
		return authentication.Presented(cookies[0].Value)
	default:
		return authentication.Malformed(authentication.ReasonMalformedCredential)
	}
}

// Issue installs value's opaque ID as the configured cookie. It refuses a
// missing response writer, a malformed ID, and an already expired session with
// the neutral session package's ErrInvalidCookie sentinel. The error never
// carries the ID.
func (carrier *CookieCarrier) Issue(ctx *web.Ctx, value sessioncore.Session) error {
	if carrier == nil {
		return errors.New("session-http: cookie carrier is unavailable")
	}
	if ctx == nil || ctx.Writer() == nil {
		return errors.New("session-http: cookie carrier requires a response writer")
	}
	if strings.TrimSpace(value.ID) == "" || value.ID != strings.TrimSpace(value.ID) {
		return sessioncore.ErrInvalidCookie
	}
	remaining := time.Until(value.ExpiresAt)
	if remaining <= 0 {
		return sessioncore.ErrInvalidCookie
	}
	carrier.set(ctx, value, remaining)
	return nil
}

// set writes the Set-Cookie header for value with an already computed
// remaining lifetime. Plugin.SetCookie uses it after asking the neutral
// plugin for the lifetime, so both write paths emit exactly one cookie shape.
func (carrier *CookieCarrier) set(ctx *web.Ctx, value sessioncore.Session, remaining time.Duration) {
	maxAge := int((remaining + time.Second - 1) / time.Second)
	http.SetCookie(ctx.Writer(), &http.Cookie{
		Name:     carrier.settings.Name,
		Value:    value.ID,
		Path:     carrier.settings.Path,
		Domain:   carrier.settings.Domain,
		Expires:  value.ExpiresAt,
		MaxAge:   maxAge,
		HttpOnly: carrier.settings.HTTPOnly,
		Secure:   carrier.settings.Secure,
		SameSite: carrier.settings.SameSite,
	})
}

// Clear expires the configured cookie on the client. It does not revoke the
// server-side session; call Revoke or Logout for that.
func (carrier *CookieCarrier) Clear(ctx *web.Ctx) {
	if carrier == nil || ctx == nil || ctx.Writer() == nil {
		return
	}
	http.SetCookie(ctx.Writer(), &http.Cookie{
		Name:     carrier.settings.Name,
		Value:    "",
		Path:     carrier.settings.Path,
		Domain:   carrier.settings.Domain,
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: carrier.settings.HTTPOnly,
		Secure:   carrier.settings.Secure,
		SameSite: carrier.settings.SameSite,
	})
}

const (
	defaultHeaderName   = "Authorization"
	defaultHeaderScheme = "Session"
)

// HeaderCarrierConfig configures a read-only HeaderCarrier. Empty fields
// select the defaults: the Authorization header carrying the Session scheme.
type HeaderCarrierConfig struct {
	// Header is the request header name.
	Header string `yaml:"name"`
	// Scheme is the authentication scheme expected before the ID, for example
	// Session for "Authorization: Session <id>". It is mandatory in effect:
	// an empty value selects Session, and a raw, scheme-less header is not
	// supported.
	Scheme string `yaml:"scheme"`
}

// DefaultHeaderCarrierConfig returns the documented header carriage:
// Authorization: Session <id>.
func DefaultHeaderCarrierConfig() HeaderCarrierConfig {
	return HeaderCarrierConfig{Header: defaultHeaderName, Scheme: defaultHeaderScheme}
}

// HeaderCarrier reads a session ID from a request header. It never writes:
// a header describes one request, so there is no client state to install or
// clear.
type HeaderCarrier struct {
	config HeaderCarrierConfig
}

// NewHeaderCarrier validates config and returns a read-only header carrier.
// Empty fields select the defaults from DefaultHeaderCarrierConfig.
func NewHeaderCarrier(config HeaderCarrierConfig) (*HeaderCarrier, error) {
	config.Header = strings.TrimSpace(config.Header)
	if config.Header == "" {
		config.Header = defaultHeaderName
	}
	config.Scheme = strings.TrimSpace(config.Scheme)
	if config.Scheme == "" {
		config.Scheme = defaultHeaderScheme
	}
	if !validHTTPToken(config.Header) {
		return nil, errors.New("session-http: header name must be a valid HTTP token")
	}
	if !validHTTPToken(config.Scheme) {
		return nil, errors.New("session-http: header scheme must be a valid HTTP token")
	}
	return &HeaderCarrier{config: config}, nil
}

// Writable reports false: a header carrier only reads.
func (*HeaderCarrier) Writable() bool { return false }

// Extract reads the configured header. A header that appears more than once
// is Malformed; a value that does not carry the configured scheme, or carries
// nothing after it, is Malformed as well.
func (carrier *HeaderCarrier) Extract(ctx *web.Ctx) authentication.CredentialResult {
	if carrier == nil || ctx == nil || ctx.Request() == nil {
		return authentication.Absent()
	}
	values := ctx.Request().Header.Values(carrier.config.Header)
	switch len(values) {
	case 0:
		return authentication.Absent()
	case 1:
	default:
		return authentication.Malformed(authentication.ReasonMalformedCredential)
	}
	value := strings.TrimSpace(values[0])
	prefix := carrier.config.Scheme + " "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return authentication.Malformed(authentication.ReasonMalformedCredential)
	}
	value = strings.TrimSpace(value[len(prefix):])
	if value == "" {
		return authentication.Malformed(authentication.ReasonMalformedCredential)
	}
	return authentication.Presented(value)
}

// Issue reports ErrNotWritable: a header carrier cannot install client state.
func (*HeaderCarrier) Issue(*web.Ctx, sessioncore.Session) error { return ErrNotWritable }

// Clear is a no-op: there is no client state to clear.
func (*HeaderCarrier) Clear(*web.Ctx) {}

// carrierChain is the ordered carrier list a Plugin consults. Extraction
// evaluates every carrier and fails closed: the first Presented value wins,
// but any Malformed result fails the whole chain so a duplicated or unusable
// credential is never masked by a later usable one.
type carrierChain struct {
	carriers []Carrier
	// writer is the single writable carrier, or nil for a read-only chain.
	writer Carrier
}

// newCarrierChain validates the chain composition: at least one carrier, no
// nil entries, and at most one writer.
func newCarrierChain(carriers []Carrier) (*carrierChain, error) {
	if len(carriers) == 0 {
		return nil, errors.New("session-http: at least one carrier is required")
	}
	chain := &carrierChain{carriers: make([]Carrier, 0, len(carriers))}
	for _, carrier := range carriers {
		if carrier == nil {
			return nil, errors.New("session-http: carrier cannot be nil")
		}
		if carrier.Writable() {
			if chain.writer != nil {
				return nil, errors.New("session-http: at most one carrier may be writable")
			}
			chain.writer = carrier
		}
		chain.carriers = append(chain.carriers, carrier)
	}
	return chain, nil
}

// Extract evaluates every carrier in order and combines the outcomes.
func (chain *carrierChain) Extract(ctx *web.Ctx) authentication.CredentialResult {
	if chain == nil {
		return authentication.Absent()
	}
	result := authentication.Absent()
	presented := false
	for _, carrier := range chain.carriers {
		current := carrier.Extract(ctx)
		if current.Status() == authentication.CredentialStatusMalformed {
			return current
		}
		if current.Status() == authentication.CredentialStatusPresented && !presented {
			result, presented = current, true
		}
	}
	return result
}

func validateCookieSettings(settings sessioncore.CookieSettings) error {
	if !validHTTPToken(settings.Name) {
		return errors.New("session-http: cookie name must be a valid HTTP token")
	}
	if !validCookiePath(settings.Path) {
		return errors.New("session-http: cookie path must be absolute and free of control characters or semicolons")
	}
	if !validCookieDomain(settings.Domain) {
		return errors.New("session-http: cookie domain contains unsafe characters")
	}
	if !settings.HTTPOnly {
		return errors.New("session-http: cookie http_only must remain enabled")
	}
	switch settings.SameSite {
	case 0, http.SameSiteDefaultMode, http.SameSiteLaxMode, http.SameSiteStrictMode, http.SameSiteNoneMode:
	default:
		return errors.New("session-http: cookie same_site value is not supported")
	}
	if settings.SameSite == http.SameSiteNoneMode && !settings.Secure {
		return errors.New("session-http: cookie SameSite=None requires secure=true")
	}
	if strings.HasPrefix(settings.Name, "__Host-") &&
		(!settings.Secure || settings.Path != "/" || settings.Domain != "") {
		return errors.New("session-http: __Host- cookies require secure=true, path=/, and no domain")
	}
	if strings.HasPrefix(settings.Name, "__Secure-") && !settings.Secure {
		return errors.New("session-http: __Secure- cookies require secure=true")
	}
	return nil
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') {
			continue
		}
		switch character {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validCookiePath(value string) bool {
	if !strings.HasPrefix(value, "/") {
		return false
	}
	return !containsControl(value) && !strings.Contains(value, ";")
}

func validCookieDomain(value string) bool {
	if value == "" {
		return true
	}
	if containsControl(value) || strings.ContainsAny(value, " /:;,@\\") {
		return false
	}
	for _, label := range strings.Split(strings.TrimPrefix(value, "."), ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for i := 0; i < len(label); i++ {
			character := label[i]
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}
