package jwt

import "github.com/gin-gonic/gin"

const claimsContextKey = "xbc/transport/web/integrations/jwt.claims"

// ClaimsFromContext returns the claims verified by JWT middleware for the
// current request. Public and excluded requests do not carry JWT claims.
func ClaimsFromContext(c *gin.Context) (Claims, bool) {
	if c == nil {
		return nil, false
	}
	value, exists := c.Get(claimsContextKey)
	if !exists {
		return nil, false
	}
	claims, ok := value.(Claims)
	return claims, ok
}

// SubjectFromContext returns the verified sub claim for the current request.
func SubjectFromContext(c *gin.Context) (string, bool) {
	claims, ok := ClaimsFromContext(c)
	if !ok {
		return "", false
	}
	subject, err := claims.GetSubject()
	if err != nil || subject == "" {
		return "", false
	}
	return subject, true
}
