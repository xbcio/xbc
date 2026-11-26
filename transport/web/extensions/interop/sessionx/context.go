package sessionx

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/extensions/authentication/session"
)

// DefaultContextKey is the gin.Context key a Handler publishes under when
// WithContextKey is not supplied.
const DefaultContextKey = "xbc.session"

// state is one request's published outcome. It is stored by value so a later
// reader cannot mutate what another Handler or helper sees.
type state struct {
	// checked reports that Middleware, Login, or Logout has run for this
	// request. It lets Login tell a verified absence from an unchecked request.
	checked bool
	// presented reports that the client sent at least one cookie under the
	// configured name, whether or not it was usable.
	presented bool
	// valid reports that session is a live session.
	valid   bool
	session session.Session
	// err is a storage failure from verification. It never carries the ID.
	err error
}

// load returns the outcome this Handler published, or the zero state when it
// has not run for the request.
func (h *Handler) load(c *gin.Context) state {
	if h == nil {
		return state{}
	}
	return stateAt(c, h.key)
}

func stateAt(c *gin.Context, key string) state {
	if c == nil {
		return state{}
	}
	value, exists := c.Get(key)
	if !exists {
		return state{}
	}
	record, ok := value.(state)
	if !ok {
		return state{}
	}
	return record
}

// Current returns the verified session stored under DefaultContextKey as a
// defensive copy. It reports false when the request has no valid session,
// whether because none was presented, it was unusable, or verification hit a
// storage failure.
func Current(c *gin.Context) (session.Session, bool) {
	return currentAt(c, DefaultContextKey)
}

// Current returns the verified session this Handler published, as a defensive
// copy. See the package-level Current for the reporting rules.
func (h *Handler) Current(c *gin.Context) (session.Session, bool) {
	if h == nil {
		return session.Session{}, false
	}
	return currentAt(c, h.key)
}

func currentAt(c *gin.Context, key string) (session.Session, bool) {
	record := stateAt(c, key)
	if !record.valid {
		return session.Session{}, false
	}
	return copySession(record.session), true
}

// CurrentID returns the verified session's opaque ID from DefaultContextKey.
// The ID is a bearer credential and must never reach a log, an error message,
// or a response body; prefer Current unless the ID itself is required.
func CurrentID(c *gin.Context) (string, bool) {
	return currentIDAt(c, DefaultContextKey)
}

// CurrentID returns this Handler's verified session's opaque ID. See the
// package-level CurrentID for the handling rule.
func (h *Handler) CurrentID(c *gin.Context) (string, bool) {
	if h == nil {
		return "", false
	}
	return currentIDAt(c, h.key)
}

func currentIDAt(c *gin.Context, key string) (string, bool) {
	record := stateAt(c, key)
	if !record.valid || strings.TrimSpace(record.session.ID) == "" {
		return "", false
	}
	return record.session.ID, true
}

// Presented reports whether the client sent a session cookie under
// DefaultContextKey, regardless of whether it was valid. It distinguishes
// "the client has no session" from "the client presented one this server
// rejected". It is false when Middleware has not run for the request.
func Presented(c *gin.Context) bool {
	return presentedAt(c, DefaultContextKey)
}

// Presented reports whether the client sent a cookie under this Handler's
// configured name. See the package-level Presented.
func (h *Handler) Presented(c *gin.Context) bool {
	if h == nil {
		return false
	}
	return presentedAt(c, h.key)
}

func presentedAt(c *gin.Context, key string) bool {
	return stateAt(c, key).presented
}

// StoreError returns the storage failure verification hit under
// DefaultContextKey, or nil. A non-nil error means the request's session
// status is unknown rather than absent, so a host that treats it as a plain
// authentication failure would report a dependency outage as bad credentials.
// The error never carries the session ID.
func StoreError(c *gin.Context) error {
	return storeErrorAt(c, DefaultContextKey)
}

// StoreError returns the storage failure this Handler's verification hit, or
// nil. See the package-level StoreError.
func (h *Handler) StoreError(c *gin.Context) error {
	if h == nil {
		return nil
	}
	return storeErrorAt(c, h.key)
}

func storeErrorAt(c *gin.Context, key string) error {
	return stateAt(c, key).err
}

// copySession copies the attributes one level deep, plus the JSON-shaped maps
// and slices the neutral core stores, so a reader cannot mutate the published
// value through Current.
func copySession(value session.Session) session.Session {
	value.Attributes = copyAttributes(value.Attributes)
	return value
}

func copyAttributes(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	copied := make(map[string]any, len(source))
	for key, value := range source {
		copied[key] = copyValue(value)
	}
	return copied
}

func copyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return copyAttributes(typed)
	case []any:
		copied := make([]any, len(typed))
		for index, item := range typed {
			copied[index] = copyValue(item)
		}
		return copied
	default:
		return value
	}
}
