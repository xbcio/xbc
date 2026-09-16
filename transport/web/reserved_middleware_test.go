package web_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// TestReservedMiddlewareKeysNameTheFrameworkAssembledStages pins the reserved
// set. Every entry here is a plugin key that no Definition may claim, which is
// only reviewable if the set itself is asserted somewhere.
func TestReservedMiddlewareKeysNameTheFrameworkAssembledStages(t *testing.T) {
	assert.Equal(t, []plugin.Key{web.AuthenticationMiddlewareKey}, web.ReservedMiddlewareKeys())

	// The error boundary is reserved by nothing: it is a real Definition
	// selected through web.Bundle(), so it must not appear above.
	assert.NotContains(t, web.ReservedMiddlewareKeys(), web.ErrorBoundaryKey)
}

// TestStartRejectsAContributedMiddlewareClaimingAReservedIdentity is the guard
// that makes the reservation real. Without it the framework entry and the
// contributed one collide as a duplicate identity, which reports the symptom
// instead of the rule: authentication enforcement is assembled by the Server
// because web.security defaults to deny, and a plugin must not be able to
// substitute itself for that stage.
func TestStartRejectsAContributedMiddlewareClaimingAReservedIdentity(t *testing.T) {
	for _, reserved := range web.ReservedMiddlewareKeys() {
		t.Run(reserved.String(), func(t *testing.T) {
			server, ctx, _ := newPingServer(t, web.DefaultConfig(), serverInputs{
				middlewares: []plugin.Entry[web.Middleware]{{
					Identity: plugin.Identity{Plugin: reserved},
					Value: fakeMiddleware{
						handler: func(_ context.Context, c *web.Ctx) error { return nil },
						order:   web.Order{Phase: web.PhaseAuth},
					},
				}},
			})

			err := server.Start(ctx)
			require.Error(t, err)
			var reservedErr *web.ReservedMiddlewareIdentityError
			require.ErrorAs(t, err, &reservedErr)
			assert.Equal(t, reserved, reservedErr.Identity.Plugin)
			assert.NotEmpty(t, reservedErr.Reason, "the error must say why the identity is reserved")
			assert.Contains(t, err.Error(), reserved.String())
		})
	}
}
