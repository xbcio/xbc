package session

import "github.com/gin-gonic/gin"

const sessionContextKey = "xbc/transport/web/extensions/authentication/session.current"

// Current returns the authenticated request session as a defensive copy.
func Current(c *gin.Context) (Session, bool) {
	if c == nil {
		return Session{}, false
	}
	value, exists := c.Get(sessionContextKey)
	if !exists {
		return Session{}, false
	}
	current, ok := value.(Session)
	if !ok || current.Subject == "" || current.ID == "" {
		return Session{}, false
	}
	return cloneSession(current), true
}

func setCurrent(c *gin.Context, value Session) {
	c.Set(sessionContextKey, cloneSession(value))
}
