package web

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/xbcio/xbc/plugin"
)

func TestNewConstructsSideEffectFreeServer(t *testing.T) {
	t.Parallel()
	server := New()
	assert.NotNil(t, server)
	assert.Equal(t, DefaultConfig(), server.cfg)
}

func TestDefinitionAndBundleAreCanonicalOpaqueHandles(t *testing.T) {
	t.Parallel()

	var zeroDefinition plugin.Definition
	assert.NotEqual(t, zeroDefinition, Definition())
	assert.Equal(t, Definition(), Definition())

	bundle := Bundle()
	assert.False(t, reflect.DeepEqual(bundle, plugin.Bundle{}))
	assert.True(t, reflect.DeepEqual(bundle, Bundle()))
}
