package biz

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	require.NotEqual(t, zero, Definition(), "Definition() must return one non-zero canonical handle")
	assert.Equal(t, Definition(), Definition())

	bundle := Bundle()
	assert.False(t, reflect.DeepEqual(bundle, plugin.Bundle{}), "Bundle() returned an empty bundle")
	assert.True(t, reflect.DeepEqual(bundle, Bundle()), "Bundle() returned different composition content")
}

func TestPluginContributesErrorOrderedMiddleware(t *testing.T) {
	instance := New()
	var middleware web.Middleware = instance
	assert.NotNil(t, middleware.Handler())
	assert.Equal(t, web.Order{Phase: web.PhaseError}, middleware.Order())
}
