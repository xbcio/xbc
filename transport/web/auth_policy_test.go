package web

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/extensions/authentication"
)

const (
	testSchemeAPIKey authentication.Scheme = "apikey"
	testSchemeJWT    authentication.Scheme = "jwt"
)

func TestRouteAuthenticationPolicyDistinguishesAbsentPublicAndExplicit(t *testing.T) {
	router, _ := newTestRouter("/api")
	router.GET("/default", func(context.Context, *Ctx) error { return nil })
	router.GET("/public", func(context.Context, *Ctx) error { return nil }).Auth(Public())
	router.GET("/explicit", func(context.Context, *Ctx) error { return nil }).Auth(Accepts(testSchemeJWT, testSchemeAPIKey))

	catalog, err := router.freeze()
	require.NoError(t, err)

	absent, ok := catalog.Lookup(http.MethodGet, "/api/default")
	require.True(t, ok)
	assert.Nil(t, absent.Auth, "an absent policy must remain distinct from explicitly public")

	public, ok := catalog.Lookup(http.MethodGet, "/api/public")
	require.True(t, ok)
	require.NotNil(t, public.Auth)
	assert.True(t, public.Auth.IsPublic())
	assert.Nil(t, public.Auth.Schemes())

	explicit, ok := catalog.Lookup(http.MethodGet, "/api/explicit")
	require.True(t, ok)
	require.NotNil(t, explicit.Auth)
	assert.False(t, explicit.Auth.IsPublic())
	assert.Equal(t, []authentication.Scheme{testSchemeJWT, testSchemeAPIKey}, explicit.Auth.Schemes())

	var zero RouteInfo
	assert.Nil(t, zero.Auth)
	assert.False(t, zero.Auth.IsPublic(), "RouteInfo's zero value must use the restrictive default, never public access")
}

func TestAuthPolicyIsSealedAndCannotRepresentPublicWithSchemes(t *testing.T) {
	typ := reflect.TypeOf(AuthPolicy{})
	for i := 0; i < typ.NumField(); i++ {
		assert.NotEmpty(t, typ.Field(i).PkgPath, "AuthPolicy field %q must not be exported", typ.Field(i).Name)
	}

	public := Public()
	assert.True(t, public.IsPublic())
	assert.Nil(t, public.Schemes())

	explicit := Accepts(testSchemeJWT)
	assert.False(t, explicit.IsPublic())
	assert.Equal(t, []authentication.Scheme{testSchemeJWT}, explicit.Schemes())
}

func TestRouteFreezeRejectsInvalidExplicitAuthenticationSchemes(t *testing.T) {
	tests := []struct {
		name   string
		policy AuthPolicy
		want   string
	}{
		{name: "no schemes", policy: Accepts(), want: "accepts no authentication schemes"},
		{name: "empty scheme", policy: Accepts(""), want: "has an empty authentication scheme at index 0"},
		{name: "duplicate scheme", policy: Accepts(testSchemeJWT, testSchemeJWT), want: `accepts duplicate authentication scheme "jwt"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router, frozen := newTestRouter("/api")
			route := router.GET("/private", func(context.Context, *Ctx) error { return nil }).Auth(test.policy)

			catalog, err := router.freeze()
			require.Error(t, err)
			assert.ErrorContains(t, err, test.want)
			assert.Nil(t, catalog)
			assert.False(t, *frozen, "a failed freeze must not publish partial frozen state")

			assert.NotPanics(t, func() { route.Name("still mutable") })
		})
	}
}

func TestRouteAuthenticationPolicyIsDefensivelyCopied(t *testing.T) {
	source := []authentication.Scheme{testSchemeJWT, testSchemeAPIKey}
	policy := Accepts(source...)
	source[0] = "mutated-source"

	router, _ := newTestRouter("/api")
	router.GET("/private", func(context.Context, *Ctx) error { return nil }).Auth(policy)
	policy.schemes[0] = "mutated-policy"

	catalog, err := router.freeze()
	require.NoError(t, err)

	first, ok := catalog.Lookup(http.MethodGet, "/api/private")
	require.True(t, ok)
	assert.Equal(t, []authentication.Scheme{testSchemeJWT, testSchemeAPIKey}, first.Auth.Schemes())

	returned := first.Auth.Schemes()
	returned[0] = "mutated-return"
	first.Auth.schemes[0] = "mutated-lookup"

	all := catalog.All()
	require.Len(t, all, 1)
	all[0].Auth.schemes[0] = "mutated-all"

	again, ok := catalog.Lookup(http.MethodGet, "/api/private")
	require.True(t, ok)
	assert.Equal(t, []authentication.Scheme{testSchemeJWT, testSchemeAPIKey}, again.Auth.Schemes())
}
