package web_test

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func principalCtx() *web.Ctx {
	return enginetest.NewCtx(httptest.NewRecorder(), nil)
}

func TestPrincipalRoundTripUsesDefensiveAttributeCopies(t *testing.T) {
	c := principalCtx()
	attributes := map[string]any{"role": "admin"}
	assert.True(t, web.SetPrincipal(c, web.Principal{
		Subject:    " alice ",
		AuthMethod: " jwt ",
		Attributes: attributes,
	}))
	attributes["role"] = "attacker"

	principal, ok := web.CurrentPrincipal(c)
	assert.True(t, ok)
	assert.Equal(t, "alice", principal.Subject)
	assert.Equal(t, "jwt", principal.AuthMethod)
	assert.Equal(t, "admin", principal.Attributes["role"])

	principal.Attributes["role"] = "mutated"
	again, ok := web.CurrentPrincipal(c)
	assert.True(t, ok)
	assert.Equal(t, "admin", again.Attributes["role"])
}

func TestSetPrincipalRejectsMissingIdentity(t *testing.T) {
	assert.False(t, web.SetPrincipal(nil, web.Principal{Subject: "alice"}))
	c := principalCtx()
	assert.False(t, web.SetPrincipal(c, web.Principal{Subject: "  "}))
	_, ok := web.CurrentPrincipal(c)
	assert.False(t, ok)
}

func TestCurrentPrincipalRejectsUnexpectedContextValue(t *testing.T) {
	c := principalCtx()
	c.Set(web.PrincipalContextKey, "alice")
	_, ok := web.CurrentPrincipal(c)
	assert.False(t, ok)
}
