package web

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestPrincipalRoundTripUsesDefensiveAttributeCopies(t *testing.T) {
	gc, _ := gin.CreateTestContext(nil)
	c := newCtx(gc)
	attributes := map[string]any{"role": "admin"}
	assert.True(t, SetPrincipal(c, Principal{
		Subject:    " alice ",
		AuthMethod: " jwt ",
		Attributes: attributes,
	}))
	attributes["role"] = "attacker"

	principal, ok := CurrentPrincipal(c)
	assert.True(t, ok)
	assert.Equal(t, "alice", principal.Subject)
	assert.Equal(t, "jwt", principal.AuthMethod)
	assert.Equal(t, "admin", principal.Attributes["role"])

	principal.Attributes["role"] = "mutated"
	again, ok := CurrentPrincipal(c)
	assert.True(t, ok)
	assert.Equal(t, "admin", again.Attributes["role"])
}

func TestSetPrincipalRejectsMissingIdentity(t *testing.T) {
	assert.False(t, SetPrincipal(nil, Principal{Subject: "alice"}))
	gc, _ := gin.CreateTestContext(nil)
	c := newCtx(gc)
	assert.False(t, SetPrincipal(c, Principal{Subject: "  "}))
	_, ok := CurrentPrincipal(c)
	assert.False(t, ok)
}

func TestCurrentPrincipalRejectsUnexpectedContextValue(t *testing.T) {
	gc, _ := gin.CreateTestContext(nil)
	gc.Set(principalContextKey, "alice")
	_, ok := CurrentPrincipal(newCtx(gc))
	assert.False(t, ok)
}
