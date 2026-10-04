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
		[]plugin.Key{web.AuthenticationMiddlewareKey, web.ErrorBoundaryKey, web.PanicBoundaryKey},
		web.ReservedMiddlewareKeys(),
	)

	// None of the three keys may be reachable as a Definition: the Server
	// assembles all three stages, so web.Bundle() carries the server alone.
	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(web.Bundle()))
	require.Len(t, entries, 1, "web.Bundle() must contain only the server Definition")
	require.True(t, pluginmodel.SameDefinition(
		entries[0].Definition, pluginmodel.Definition(web.Definition())))
}

// TestStartRejectsMiddlewareBelowPhaseRecover pins the floor that makes
// "outermost" true rather than approximately true. Phases sort ascending and
// ordering pins only reach within one phase, so a contributed middleware at
// PhaseRecover-1 would silently run outside the Server's panic boundary -- and
// a panic in its own code would escape every ordered stage. The phase is
// therefore refused at startup instead of documented as a hazard.
func TestStartRejectsMiddlewareBelowPhaseRecover(t *testing.T) {
	server, ctx, _ := newPingServer(t, web.DefaultConfig(), serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{{
			Identity: plugin.Identity{Plugin: "outside-recovery"},
			Value: fakeMiddleware{
				handler: func(_ context.Context, c *web.Ctx) error { return nil },
				order:   web.Order{Phase: web.PhaseRecover - 1},
			},
		}},
	})

	err := server.Start(ctx)
	require.Error(t, err)
	var phaseErr *web.MiddlewarePhaseOutsideRecoveryError
	require.ErrorAs(t, err, &phaseErr)
	assert.Equal(t, plugin.Key("outside-recovery"), phaseErr.Identity.Plugin)
	assert.Equal(t, web.PhaseRecover-1, phaseErr.Phase)
	assert.Contains(t, err.Error(), "panic boundary", "the error must name the guarantee the phase would break")
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
