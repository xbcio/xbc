package web_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

func TestOrdinaryImportExposesOnlyExplicitCanonicalComposition(t *testing.T) {
	var zeroDefinition plugin.Definition
	if web.Definition() == zeroDefinition || web.Definition() != web.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	if reflect.DeepEqual(web.Bundle(), plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(web.Bundle(), web.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}

// The following declarations are a compile-time pin on the exported helpers'
// neutral Ctx signatures. Go only lets a function value satisfy a variable of
// a named function type when its parameter and result types match exactly, so
// if any of these seven helpers ever regresses back to taking *gin.Context,
// this file stops compiling instead of silently drifting.
var (
	_ func(*web.Ctx, web.ProblemDetail)    = web.AbortProblem
	_ func(*web.Ctx, web.ProblemDetail)    = web.WriteProblem
	_ func(*web.Ctx, web.Principal) bool   = web.SetPrincipal
	_ func(*web.Ctx) (web.Principal, bool) = web.CurrentPrincipal
	_ func(*web.Ctx) (web.RouteInfo, bool) = web.CurrentRoute
	_ func(*web.Ctx) bool                  = web.AuthenticationExempt
	_ func(*web.Ctx, error)                = web.AbortError
)
