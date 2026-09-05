package web

import (
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/ordering"
)

type fixtureMiddleware struct {
	handler gin.HandlerFunc
	order   Order
}

func (m fixtureMiddleware) Handler() gin.HandlerFunc { return m.handler }
func (m fixtureMiddleware) Order() Order             { return m.order }

func middlewareEntry(key plugin.Key, instance string, order Order) plugin.Entry[Middleware] {
	return plugin.Entry[Middleware]{
		Identity: plugin.Identity{Plugin: key, Instance: instance},
		Value: fixtureMiddleware{
			handler: func(*gin.Context) {},
			order:   order,
		},
	}
}

func middlewareIdentities(entries []plugin.Entry[Middleware]) []string {
	identities := make([]string, len(entries))
	for i, entry := range entries {
		identities[i] = entry.Identity.String()
	}
	return identities
}

// Fixture identity keys shared by the ordering test cases below keep every
// Prefer/Require/PreferInstance/RequireInstance call site free of ad hoc
// string literals.
const (
	authenticationKey           plugin.Key = "authentication"
	authenticationMiddlewareKey plugin.Key = "authentication-middleware"
	authorizationKey            plugin.Key = "authorization"
	metricsKey                  plugin.Key = "metrics"
	producerKey                 plugin.Key = "producer"
	requestIDKey                plugin.Key = "request-id"
	securityKey                 plugin.Key = "security"
	tracingKey                  plugin.Key = "tracing"
	webErrorBoundaryKey         plugin.Key = "web-error-boundary"
	fixtureKeyA                 plugin.Key = "a"
	fixtureKeyB                 plugin.Key = "b"
)

func TestPhaseString(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		phase Phase
		want  string
	}{
		{PhaseRecover, "recover"},
		{PhaseObserve, "observe"},
		{PhaseError, "error"},
		{PhaseSecurity, "security"},
		{PhaseAuth, "auth"},
		{PhaseBusiness, "business"},
		{Phase(175), "phase(175)"},
	} {
		assert.Equal(t, test.want, test.phase.String())
	}
}

func TestOrderRefConstructorsCarryTypedKeyInstanceAndStrictness(t *testing.T) {
	t.Parallel()

	const key plugin.Key = "authentication-middleware"

	tests := []struct {
		name         string
		ref          OrderRef
		instance     string
		required     bool
		stringRender string
	}{
		{
			name:         "prefer all instances",
			ref:          Prefer(key),
			stringRender: "authentication-middleware",
		},
		{
			name:         "require all instances",
			ref:          Require(key),
			required:     true,
			stringRender: "authentication-middleware",
		},
		{
			name:         "prefer exact named instance",
			ref:          PreferInstance(key, "admin"),
			instance:     "admin",
			stringRender: "authentication-middleware[admin]",
		},
		{
			name:         "require exact named instance",
			ref:          RequireInstance(key, "admin"),
			instance:     "admin",
			required:     true,
			stringRender: "authentication-middleware[admin]",
		},
		{
			name:         "empty exact instance means canonical default",
			ref:          PreferInstance(key, ""),
			instance:     plugin.DefaultInstance,
			stringRender: "authentication-middleware[default]",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, key, test.ref.Key())
			assert.Equal(t, test.instance, test.ref.InstanceName())
			assert.Equal(t, test.required, test.ref.Required())
			assert.Equal(t, test.stringRender, test.ref.String())
		})
	}
}

func TestOrderMiddlewaresUsesPhaseThenCanonicalIdentityNotCollectionOrder(t *testing.T) {
	t.Parallel()

	base := []plugin.Entry[Middleware]{
		middlewareEntry("z-business", "", Order{Phase: PhaseBusiness}),
		middlewareEntry("z-security", "", Order{Phase: PhaseSecurity}),
		middlewareEntry("a-security", "red", Order{Phase: PhaseSecurity}),
		middlewareEntry("a-security", "", Order{Phase: PhaseSecurity}),
		middlewareEntry("a-security", "blue", Order{Phase: PhaseSecurity}),
		middlewareEntry("recover", "", Order{Phase: PhaseRecover}),
	}
	want := []string{
		"recover",
		"a-security[blue]",
		"a-security",
		"a-security[red]",
		"z-security",
		"z-business",
	}

	for rotation := range len(base) {
		entries := slices.Clone(base)
		entries = append(entries[rotation:], entries[:rotation]...)

		ordered, misses, err := orderMiddlewares(entries)
		require.NoError(t, err)
		assert.Empty(t, misses)
		assert.Equal(t, want, middlewareIdentities(ordered), "rotation %d", rotation)
	}
}

func TestOrderMiddlewaresAppliesBeforeAndAfterWithinPhase(t *testing.T) {
	t.Parallel()

	entries := []plugin.Entry[Middleware]{
		middlewareEntry("audit", "", Order{
			Phase:  PhaseObserve,
			Before: []OrderRef{Prefer(metricsKey)},
		}),
		middlewareEntry(requestIDKey, "", Order{Phase: PhaseObserve}),
		middlewareEntry(metricsKey, "", Order{
			Phase: PhaseObserve,
			After: []OrderRef{Require(requestIDKey)},
		}),
	}

	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t,
		[]string{"audit", "request-id", "metrics"},
		middlewareIdentities(ordered),
	)
}

func TestOrderMiddlewaresBareKeyTargetsEveryEnabledInstance(t *testing.T) {
	t.Parallel()

	entries := []plugin.Entry[Middleware]{
		middlewareEntry("consumer", "", Order{
			Phase: PhaseSecurity,
			After: []OrderRef{Require(producerKey)},
		}),
		middlewareEntry(producerKey, "west", Order{Phase: PhaseSecurity}),
		middlewareEntry(producerKey, "east", Order{Phase: PhaseSecurity}),
	}

	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t,
		[]string{"producer[east]", "producer[west]", "consumer"},
		middlewareIdentities(ordered),
	)
}

func TestOrderMiddlewaresInstanceRefTargetsExactlyOneInstance(t *testing.T) {
	t.Parallel()

	entries := []plugin.Entry[Middleware]{
		middlewareEntry(producerKey, "z", Order{Phase: PhaseSecurity}),
		middlewareEntry("consumer", "", Order{
			Phase:  PhaseSecurity,
			Before: []OrderRef{RequireInstance(producerKey, "a")},
		}),
		middlewareEntry(producerKey, "a", Order{Phase: PhaseSecurity}),
	}

	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t,
		[]string{"consumer", "producer[a]", "producer[z]"},
		middlewareIdentities(ordered),
	)
}

func TestOrderMiddlewaresReportsSoftMissesWithoutFailing(t *testing.T) {
	t.Parallel()

	entries := []plugin.Entry[Middleware]{
		middlewareEntry("audit", "", Order{
			Phase:  PhaseObserve,
			After:  []OrderRef{Prefer(tracingKey)},
			Before: []OrderRef{PreferInstance(metricsKey, "regional")},
		}),
	}

	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Equal(t, []string{"audit"}, middlewareIdentities(ordered))
	require.Equal(t, []MiddlewareOrderMiss{
		{
			Middleware: plugin.Identity{Plugin: "audit"},
			Reference:  Prefer(tracingKey),
			Direction:  ordering.After,
		},
		{
			Middleware: plugin.Identity{Plugin: "audit"},
			Reference:  PreferInstance(metricsKey, "regional"),
			Direction:  ordering.Before,
		},
	}, misses)
	assert.Contains(t, misses[0].String(), "audit")
	assert.Contains(t, misses[0].String(), "tracing")
}

func TestOrderMiddlewaresRequiredMissingTargetFails(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		order     Order
		direction ordering.Direction
		ref       OrderRef
	}{
		{
			name:      "after bare key",
			order:     Order{Phase: PhaseAuth, After: []OrderRef{Require(authenticationMiddlewareKey)}},
			direction: ordering.After,
			ref:       Require(authenticationMiddlewareKey),
		},
		{
			name:      "before exact instance",
			order:     Order{Phase: PhaseAuth, Before: []OrderRef{RequireInstance(authorizationKey, "admin")}},
			direction: ordering.Before,
			ref:       RequireInstance(authorizationKey, "admin"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, misses, err := orderMiddlewares([]plugin.Entry[Middleware]{
				middlewareEntry("consumer", "", test.order),
			})
			assert.Nil(t, misses)

			var missing *MissingMiddlewareOrderTargetError
			require.ErrorAs(t, err, &missing)
			assert.Equal(t, plugin.Identity{Plugin: "consumer"}, missing.Middleware)
			assert.Equal(t, test.ref, missing.Reference)
			assert.Equal(t, test.direction, missing.Direction)
			assert.Contains(t, err.Error(), test.ref.String())
		})
	}
}

func TestOrderMiddlewaresExactRequiredTargetDoesNotMatchAnotherInstance(t *testing.T) {
	t.Parallel()

	_, _, err := orderMiddlewares([]plugin.Entry[Middleware]{
		middlewareEntry("consumer", "", Order{
			Phase: PhaseAuth,
			After: []OrderRef{RequireInstance(authenticationMiddlewareKey, "admin")},
		}),
		middlewareEntry(authenticationMiddlewareKey, "public", Order{Phase: PhaseAuth}),
	})

	var missing *MissingMiddlewareOrderTargetError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, "admin", missing.Reference.InstanceName())
}

func TestOrderMiddlewaresConsistentCrossPhaseReferenceIsRedundant(t *testing.T) {
	t.Parallel()

	entries := []plugin.Entry[Middleware]{
		middlewareEntry(authenticationKey, "", Order{Phase: PhaseAuth}),
		middlewareEntry(securityKey, "", Order{
			Phase:  PhaseSecurity,
			Before: []OrderRef{Require(authenticationKey)},
		}),
		middlewareEntry("business", "", Order{
			Phase: PhaseBusiness,
			After: []OrderRef{Prefer(authenticationKey)},
		}),
	}

	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t,
		[]string{"security", "authentication", "business"},
		middlewareIdentities(ordered),
	)
}

func TestOrderMiddlewaresPhaseContradictionAlwaysFails(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		order     Order
		direction ordering.Direction
	}{
		{
			name: "soft after later phase",
			order: Order{
				Phase: PhaseSecurity,
				After: []OrderRef{Prefer(authenticationKey)},
			},
			direction: ordering.After,
		},
		{
			name: "required before earlier phase",
			order: Order{
				Phase:  PhaseAuth,
				Before: []OrderRef{Require(securityKey)},
			},
			direction: ordering.Before,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries := []plugin.Entry[Middleware]{
				middlewareEntry(securityKey, "", Order{Phase: PhaseSecurity}),
				middlewareEntry(authenticationKey, "", Order{Phase: PhaseAuth}),
			}
			if test.direction == ordering.After {
				entries[0] = middlewareEntry(securityKey, "", test.order)
			} else {
				entries[1] = middlewareEntry(authenticationKey, "", test.order)
			}

			_, _, err := orderMiddlewares(entries)
			var conflict *PhaseConflictError
			require.ErrorAs(t, err, &conflict)
			assert.Equal(t, test.direction, conflict.Direction)
			assert.Contains(t, err.Error(), "security")
			assert.Contains(t, err.Error(), "authentication")
		})
	}
}

func TestOrderMiddlewaresSamePhaseCycleAlwaysFails(t *testing.T) {
	t.Parallel()

	for _, required := range []bool{false, true} {
		ref := Prefer(fixtureKeyB)
		back := Prefer(fixtureKeyA)
		if required {
			ref = Require(fixtureKeyB)
			back = Require(fixtureKeyA)
		}

		_, _, err := orderMiddlewares([]plugin.Entry[Middleware]{
			middlewareEntry(fixtureKeyA, "", Order{Phase: PhaseSecurity, After: []OrderRef{ref}}),
			middlewareEntry(fixtureKeyB, "", Order{Phase: PhaseSecurity, After: []OrderRef{back}}),
		})
		require.Error(t, err)

		var cycle *ordering.CycleError
		require.ErrorAs(t, err, &cycle)
		assert.Equal(t, []string{"a", "b", "a"}, cycle.Path)
	}
}

func TestOrderMiddlewaresRejectsDuplicateProducerIdentity(t *testing.T) {
	t.Parallel()

	_, _, err := orderMiddlewares([]plugin.Entry[Middleware]{
		middlewareEntry("cors", "", Order{Phase: PhaseSecurity}),
		middlewareEntry("cors", plugin.DefaultInstance, Order{Phase: PhaseSecurity}),
	})

	var duplicate *DuplicateMiddlewareIdentityError
	require.ErrorAs(t, err, &duplicate)
	assert.Equal(t, plugin.Identity{Plugin: "cors", Instance: plugin.DefaultInstance}, duplicate.Identity)
}

func TestOrderMiddlewaresFrameworkOutermostPin(t *testing.T) {
	t.Parallel()

	entries := []plugin.Entry[Middleware]{
		middlewareEntry("focused-boundary", "", Order{Phase: PhaseError}),
		middlewareEntry(webErrorBoundaryKey, "", Order{Phase: PhaseError}),
		middlewareEntry("observer", "", Order{Phase: PhaseObserve}),
		middlewareEntry(securityKey, "", Order{Phase: PhaseSecurity}),
	}

	ordered, misses, err := orderMiddlewares(
		entries,
		pinMiddlewareOutermost(Require(webErrorBoundaryKey)),
	)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t,
		[]string{"observer", "web-error-boundary", "focused-boundary", "security"},
		middlewareIdentities(ordered),
	)
}

func TestOrderMiddlewaresOutermostBareKeyPinsAllInstancesAsOneOuterGroup(t *testing.T) {
	t.Parallel()

	ordered, misses, err := orderMiddlewares(
		[]plugin.Entry[Middleware]{
			middlewareEntry("focused-boundary", "", Order{Phase: PhaseError}),
			middlewareEntry(webErrorBoundaryKey, "secondary", Order{Phase: PhaseError}),
			middlewareEntry(webErrorBoundaryKey, "", Order{Phase: PhaseError}),
		},
		pinMiddlewareOutermost(Require(webErrorBoundaryKey)),
	)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t,
		[]string{"web-error-boundary", "web-error-boundary[secondary]", "focused-boundary"},
		middlewareIdentities(ordered),
	)
}

func TestOrderMiddlewaresOutermostPinContradictionBecomesCycle(t *testing.T) {
	t.Parallel()

	_, _, err := orderMiddlewares(
		[]plugin.Entry[Middleware]{
			middlewareEntry("focused-boundary", "", Order{
				Phase:  PhaseError,
				Before: []OrderRef{Prefer(webErrorBoundaryKey)},
			}),
			middlewareEntry(webErrorBoundaryKey, "", Order{Phase: PhaseError}),
		},
		pinMiddlewareOutermost(Require(webErrorBoundaryKey)),
	)

	var cycle *ordering.CycleError
	require.ErrorAs(t, err, &cycle)
	assert.Equal(t,
		[]string{"focused-boundary", "web-error-boundary", "focused-boundary"},
		cycle.Path,
	)
}

func TestOrderMiddlewaresOutermostPinRequiresKnownTarget(t *testing.T) {
	t.Parallel()

	_, _, err := orderMiddlewares(
		[]plugin.Entry[Middleware]{
			middlewareEntry("focused-boundary", "", Order{Phase: PhaseError}),
		},
		pinMiddlewareOutermost(Prefer(webErrorBoundaryKey)),
	)

	var missing *MissingMiddlewareOrderTargetError
	require.ErrorAs(t, err, &missing)
	assert.True(t, missing.Reference.Required(), "framework pins are required")
	assert.Equal(t, webErrorBoundaryKey, missing.Reference.Key())
}

func TestOrderMiddlewaresFrameworkAfterPinModelsRequiresPrincipalEdge(t *testing.T) {
	t.Parallel()

	authorizationIdentity := plugin.Identity{Plugin: authorizationKey}

	ordered, misses, err := orderMiddlewares(
		[]plugin.Entry[Middleware]{
			middlewareEntry(authorizationKey, "", Order{Phase: PhaseAuth}),
			middlewareEntry(authenticationMiddlewareKey, "", Order{Phase: PhaseAuth}),
		},
		pinMiddlewareAfter(authorizationIdentity, Require(authenticationMiddlewareKey)),
	)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t,
		[]string{"authentication-middleware", "authorization"},
		middlewareIdentities(ordered),
	)
}

func TestOrderMiddlewaresFrameworkAfterPinRejectsAuthorWrittenInverse(t *testing.T) {
	t.Parallel()

	authorizationIdentity := plugin.Identity{Plugin: authorizationKey}

	_, _, err := orderMiddlewares(
		[]plugin.Entry[Middleware]{
			middlewareEntry(authorizationKey, "", Order{
				Phase:  PhaseAuth,
				Before: []OrderRef{Prefer(authenticationMiddlewareKey)},
			}),
			middlewareEntry(authenticationMiddlewareKey, "", Order{Phase: PhaseAuth}),
		},
		pinMiddlewareAfter(authorizationIdentity, Require(authenticationMiddlewareKey)),
	)

	var cycle *ordering.CycleError
	require.ErrorAs(t, err, &cycle)
}

func TestOrderMiddlewaresFrameworkAfterPinRequiresPredecessor(t *testing.T) {
	t.Parallel()

	_, _, err := orderMiddlewares(
		[]plugin.Entry[Middleware]{
			middlewareEntry(authorizationKey, "", Order{Phase: PhaseAuth}),
		},
		pinMiddlewareAfter(
			plugin.Identity{Plugin: authorizationKey},
			Prefer(authenticationMiddlewareKey),
		),
	)

	var missing *MissingMiddlewareOrderTargetError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, plugin.Identity{Plugin: authorizationKey}, missing.Middleware)
	assert.True(t, missing.Reference.Required(), "framework marker edges are required")
}

func TestOrderMiddlewaresFrameworkAfterPinHonorsHardPhaseBoundary(t *testing.T) {
	t.Parallel()

	_, _, err := orderMiddlewares(
		[]plugin.Entry[Middleware]{
			middlewareEntry(authorizationKey, "", Order{Phase: PhaseSecurity}),
			middlewareEntry(authenticationMiddlewareKey, "", Order{Phase: PhaseAuth}),
		},
		pinMiddlewareAfter(
			plugin.Identity{Plugin: authorizationKey},
			Require(authenticationMiddlewareKey),
		),
	)

	var conflict *PhaseConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, ordering.After, conflict.Direction)
}
