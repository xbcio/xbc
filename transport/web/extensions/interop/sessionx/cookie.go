package sessionx

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/session"
)

// setCookie installs value's opaque ID as the configured cookie. The attributes
// match the Web transport's session carrier exactly, so one session core
// produces one cookie shape across carriers.
func (h *Handler) setCookie(w http.ResponseWriter, value session.Session, remaining time.Duration) {
	if w == nil {
		return
	}
	maxAge := int((remaining + time.Second - 1) / time.Second)
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookie.Name,
		Value:    value.ID,
		Path:     h.cookie.Path,
		Domain:   h.cookie.Domain,
		Expires:  value.ExpiresAt,
		MaxAge:   maxAge,
		HttpOnly: h.cookie.HTTPOnly,
		Secure:   h.cookie.Secure,
		SameSite: h.cookie.SameSite,
	})
}

// clearCookie expires the configured cookie on the client. It does not revoke
// the server-side session; Logout does.
func (h *Handler) clearCookie(w http.ResponseWriter) {
	if w == nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookie.Name,
		Value:    "",
		Path:     h.cookie.Path,
		Domain:   h.cookie.Domain,
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: h.cookie.HTTPOnly,
		Secure:   h.cookie.Secure,
		SameSite: h.cookie.SameSite,
	})
}

// validateCookieSettings refuses a cookie shape that would weaken the
// transport invariant: a script-readable cookie, an unsafe name, path, or
// domain, or a SameSite combination the browser would reject. The errors never
// quote the session value, because none exists at construction.
func validateCookieSettings(settings session.CookieSettings) error {
	if !validHTTPToken(settings.Name) {
		return errors.New("sessionx: cookie name must be a valid HTTP token")
	}
	if !validCookiePath(settings.Path) {
		return errors.New("sessionx: cookie path must be absolute and free of control characters or semicolons")
	}
	if !validCookieDomain(settings.Domain) {
		return errors.New("sessionx: cookie domain contains unsafe characters")
	}
	if !settings.HTTPOnly {
		return errors.New("sessionx: cookie http_only must remain enabled")
	}
	switch settings.SameSite {
	case 0, http.SameSiteDefaultMode, http.SameSiteLaxMode, http.SameSiteStrictMode, http.SameSiteNoneMode:
	default:
		return errors.New("sessionx: cookie same_site value is not supported")
	}
	if settings.SameSite == http.SameSiteNoneMode && !settings.Secure {
		return errors.New("sessionx: cookie SameSite=None requires secure=true")
	}
	if strings.HasPrefix(settings.Name, "__Host-") &&
		(!settings.Secure || settings.Path != "/" || settings.Domain != "") {
		return errors.New("sessionx: __Host- cookies require secure=true, path=/, and no domain")
	}
	if strings.HasPrefix(settings.Name, "__Secure-") && !settings.Secure {
		return errors.New("sessionx: __Secure- cookies require secure=true")
	}
	return nil
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
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
		for index := 0; index < len(label); index++ {
			character := label[index]
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
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7f {
			return true
		}
	}
	return false
}
