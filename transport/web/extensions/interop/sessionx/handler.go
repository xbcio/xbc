package sessionx

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/extensions/authentication/session"
)

var (
	// ErrUnavailable reports a nil Handler or a nil session manager.
	ErrUnavailable = errors.New("sessionx: session manager is unavailable")
	// ErrRequestUnavailable reports a call without a Gin request.
	ErrRequestUnavailable = errors.New("sessionx: request is unavailable")
	// ErrSubjectRequired reports Login with a blank subject.
	ErrSubjectRequired = errors.New("sessionx: subject cannot be empty")
)

// Handler binds the neutral session core to a Gin host. Build it once with
// New, install Middleware on the Gin engine, and use the helpers to read the
// request's outcome.
//
// manager, cookie, and lifetime are the three values the neutral plugin
// publishes for a transport: the AttributeManager that verifies IDs, the
// CookieSettings that name the cookie, and Lifetime, which reports how long an
// issued ID may still reach a client. Passing the values instead of the
// concrete plugin keeps this carrier usable with any implementation of the
// neutral contracts.
type Handler struct {
	manager  session.AttributeManager
	cookie   session.CookieSettings
	lifetime func(session.Session) (time.Duration, error)
	key      string
}

// Option customizes a Handler.
type Option func(*options)

type options struct {
	contextKey string
}

// WithContextKey stores the request outcome under key instead of
// DefaultContextKey. Use it when another middleware already claims the default
// key, or to keep several Handlers' outcomes apart in one request. The
// Handler's methods follow the selected key; the package-level helpers read
// DefaultContextKey only. An empty or blank key is refused by New.
func WithContextKey(key string) Option {
	return func(built *options) {
		built.contextKey = key
	}
}

// New builds a Handler. A nil manager or lifetime function is refused, and so
// is a cookie configuration that would weaken the cookie's security shape; the
// neutral plugin's own configuration validation already enforces the same
// invariants, so a value obtained from Plugin.Cookie always passes.
func New(
	manager session.AttributeManager,
	cookie session.CookieSettings,
	lifetime func(session.Session) (time.Duration, error),
	opts ...Option,
) (*Handler, error) {
	if manager == nil {
		return nil, ErrUnavailable
	}
	if lifetime == nil {
		return nil, errors.New("sessionx: lifetime function is required")
	}
	if err := validateCookieSettings(cookie); err != nil {
		return nil, err
	}

	built := options{contextKey: DefaultContextKey}
	for _, opt := range opts {
		if opt != nil {
			opt(&built)
		}
	}
	if strings.TrimSpace(built.contextKey) == "" {
		return nil, errors.New("sessionx: context key cannot be empty")
	}

	return &Handler{
		manager:  manager,
		cookie:   cookie,
		lifetime: lifetime,
		key:      built.contextKey,
	}, nil
}

// Middleware verifies the configured cookie and publishes the outcome on the
// Gin context. It never aborts the chain and never writes a response.
//
// Exactly one non-empty cookie is verified through the neutral manager, which
// also renews the session's idle lease. No cookie means nothing was presented;
// a duplicated or empty cookie is unusable and is not verified. The outcome is
// available through Current, CurrentID, Presented, and StoreError.
func (h *Handler) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h == nil || c == nil {
			return
		}

		record := state{checked: true}
		if c.Request != nil {
			cookies := c.Request.CookiesNamed(h.cookie.Name)
			record.presented = len(cookies) > 0
			if len(cookies) == 1 && cookies[0] != nil && cookies[0].Value != "" {
				value, verdict, err := h.manager.Verify(c.Request.Context(), cookies[0].Value)
				switch verdict {
				case session.VerdictValid:
					record.session = value
					record.valid = true
				case session.VerdictStoreError:
					record.err = err
				}
			}
		}

		c.Set(h.key, record)
	}
}

// Login creates a session for subject and installs its opaque ID as the
// configured cookie. A valid session the request already carries is revoked
// first, so a fixed ID cannot survive a successful login. A request without a
// usable session does not need a revocation and still logs in.
//
// A blank subject is refused before anything is created or revoked. A storage
// failure while checking the presented session fails the login closed, because
// the anti-fixation step could not be completed. When issuing the cookie fails,
// the new session is revoked before the error is returned, so a failed login
// leaves neither a live session nor a new cookie. The new session is also
// published on the Gin context.
func (h *Handler) Login(c *gin.Context, subject string, attributes map[string]any) (session.Session, error) {
	if h == nil || h.manager == nil {
		return session.Session{}, ErrUnavailable
	}
	if c == nil || c.Request == nil || c.Writer == nil {
		return session.Session{}, ErrRequestUnavailable
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return session.Session{}, ErrSubjectRequired
	}
	ctx := c.Request.Context()

	presented, err := h.loginCandidate(c)
	if err != nil {
		return session.Session{}, err
	}
	if presented != "" {
		if err := h.manager.Revoke(ctx, presented); err != nil && !errors.Is(err, session.ErrNotFound) {
			return session.Session{}, err
		}
	}

	value, err := h.manager.Create(ctx, subject, attributes)
	if err != nil {
		return session.Session{}, err
	}
	remaining, err := h.lifetime(value)
	if err != nil {
		// The credential never reached the client, so the session must not
		// stay live behind an unusable response.
		_ = h.manager.Revoke(ctx, value.ID)
		return session.Session{}, err
	}
	h.setCookie(c.Writer, value, remaining)
	c.Set(h.key, state{
		checked:   true,
		presented: h.requestPresented(c),
		valid:     true,
		session:   value,
	})
	return value, nil
}

// Logout revokes the session the request presents and clears the configured
// cookie. An absent, malformed, or already revoked session is not an error:
// logout is idempotent. A storage failure is returned after the cookie is
// cleared, so the client stops presenting a session the server could not
// retire.
func (h *Handler) Logout(c *gin.Context) error {
	if h == nil || h.manager == nil {
		return ErrUnavailable
	}
	if c == nil || c.Request == nil {
		return ErrRequestUnavailable
	}

	candidate := ""
	if record := h.load(c); record.checked && record.valid {
		candidate = record.session.ID
	} else if cookies := c.Request.CookiesNamed(h.cookie.Name); len(cookies) == 1 &&
		cookies[0] != nil && cookies[0].Value != "" {
		candidate = cookies[0].Value
	}

	var revokeErr error
	if candidate != "" {
		if err := h.manager.Revoke(c.Request.Context(), candidate); err != nil &&
			!errors.Is(err, session.ErrNotFound) && !errors.Is(err, session.ErrInvalidCookie) {
			revokeErr = err
		}
	}
	h.clearCookie(c.Writer)
	c.Set(h.key, state{checked: true, presented: h.requestPresented(c)})
	return revokeErr
}

// loginCandidate resolves the ID of a valid session the request already
// carries. A checked request uses the outcome Middleware already published, so
// a verification is never repeated and a storage failure stays visible. An
// unchecked request (Login on a route without the middleware) verifies the
// cookie itself. A duplicated or unusable cookie has no candidate: there is no
// single live session to revoke.
func (h *Handler) loginCandidate(c *gin.Context) (string, error) {
	if record := h.load(c); record.checked {
		if record.err != nil {
			return "", record.err
		}
		if record.valid {
			return record.session.ID, nil
		}
		return "", nil
	}

	cookies := c.Request.CookiesNamed(h.cookie.Name)
	if len(cookies) != 1 || cookies[0] == nil || cookies[0].Value == "" {
		return "", nil
	}
	value, verdict, err := h.manager.Verify(c.Request.Context(), cookies[0].Value)
	switch verdict {
	case session.VerdictValid:
		return value.ID, nil
	case session.VerdictStoreError:
		return "", err
	default:
		return "", nil
	}
}

// requestPresented reports whether the client sent any cookie under the
// configured name, regardless of its value.
func (h *Handler) requestPresented(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	return len(c.Request.CookiesNamed(h.cookie.Name)) > 0
}
