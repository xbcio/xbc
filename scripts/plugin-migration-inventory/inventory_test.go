package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateInventoryClassifiesMigrationBlockers(t *testing.T) {
	root := t.TempDir()
	writeInventoryFixture(t, root, "go.work", "go 1.25.0\n\nuse ./app\n")
	writeInventoryFixture(t, root, "app/go.mod", "module example.com/app\n\ngo 1.25.0\n")
	writeInventoryFixture(t, root, "app/fixture/plugin.go", `package fixture

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

const (
	Key plugin.Key = "fixture"
	AuthKey plugin.Key = "auth"
)

var definition = plugin.Define(Key, func(plugin.BuildContext) (*legacy, error) {
	return &legacy{}, nil
})

func Definition() plugin.Definition { return definition }
func Bundle() plugin.Bundle { return plugin.BundleOf(definition) }

type legacy struct {
	plugin.Base
	ctx *plugin.Context
	value any `+"`xbc:\"inject\"`"+`
}

func (*legacy) ConfigPtr() any { return nil }
func (*legacy) Dependencies() plugin.Deps {
	return plugin.Deps{Before: []plugin.Key{"web"}}
}

func typedOrder() web.Order {
	return web.Order{After: []web.OrderRef{web.Require(AuthKey)}}
}

func literalOrder() web.Order {
	return web.Order{After: []web.OrderRef{web.Require("auth")}}
}
`)
	writeInventoryFixture(t, root, "app/runtime/instance.go", `package runtime

import "github.com/xbcio/xbc/plugin"

// Framework runtime ownership is not Plugin implementation retention.
type instance struct { context *plugin.Context }
`)
	writeInventoryFixture(t, root, "app/noncanonical/plugin.go", `package noncanonical

import "github.com/xbcio/xbc/plugin"

func Definition() plugin.Definition {
	return plugin.Define("noncanonical", func(plugin.BuildContext) (*int, error) {
		value := 1
		return &value, nil
	})
}
`)
	writeInventoryFixture(t, root, "app/fixture/autoload/register.go", `package autoload

import (
	"example.com/app/fixture"
	"github.com/xbcio/xbc/plugin/catalog"
)

func init() { catalog.Declare(fixture.Definition()) }
`)

	inventory, err := generateInventory(root)
	require.NoError(t, err)

	assert.Equal(t, 1, inventory.Summary.Modules)
	assert.Equal(t, 4, inventory.Summary.GoFiles)
	assert.NotZero(t, inventory.Summary.MigrationBlockers)
	assert.Len(t, inventory.ContextRetentions, 2)
	assert.Len(t, inventory.GenericOrderReferences, 1)
	assert.Len(t, inventory.WebLiteralOrderReferences, 1)

	accessors := make(map[string]DefinitionAccessor)
	for _, accessor := range inventory.DefinitionAccessors {
		accessors[accessor.Definition] = accessor
	}
	assert.True(t, accessors["fixture"].Canonical)
	assert.Equal(t, "package-level-identifier", accessors["fixture"].Shape)
	assert.False(t, accessors["noncanonical"].Canonical)
	assert.Equal(t, "constructor-call", accessors["noncanonical"].Shape)

	apis := make(map[string]int)
	for _, total := range inventory.RetiredAPITotals {
		apis[total.API] = total.Count
	}
	assert.NotZero(t, apis["plugin.Base"])
	assert.NotZero(t, apis["plugin.Deps"])
	assert.NotZero(t, apis["ConfigPtr"])
	assert.NotZero(t, apis[`xbc:"inject"`])
	assert.NotZero(t, apis[catalogImportPath])
	assert.NotZero(t, apis["plugin/catalog.Declare"])

	generated, err := marshalInventory(inventory)
	require.NoError(t, err)
	again, err := generateInventory(root)
	require.NoError(t, err)
	regenerated, err := marshalInventory(again)
	require.NoError(t, err)
	assert.Equal(t, string(generated), string(regenerated), "inventory must be deterministic")
}

func writeInventoryFixture(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
}
