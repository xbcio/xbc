package web_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	"github.com/xbcio/xbc/transport/web"
)

// TestReservedMiddlewareKeysNameTheFrameworkAssembledStages pins the reserved
// set. Every entry here is a plugin key that no Definition may claim, which is
// only reviewable if the set itself is asserted somewhere.
func TestReservedMiddlewareKeysNameTheFrameworkAssembledStages(t *testing.T) {
	assert.Equal(t,
		[]plugin.Key{web.AuthenticationMiddlewareKey, web.ErrorBoundaryKey},
		web.ReservedMiddlewareKeys(),
	)

	// Neither key may be reachable as a Definition: the Server assembles both
	// stages, so web.Bundle() carries the server alone.
	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(web.Bundle()))
	require.Len(t, entries, 1, "web.Bundle() must contain only the server Definition")
	require.True(t, pluginmodel.SameDefinition(
		entries[0].Definition, pluginmodel.Definition(web.Definition())))
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
