package plugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivationZeroValueBehavesLikeAlways(t *testing.T) {
	var zero Activation
	assert.Equal(t, Always.String(), zero.String(), "零值 Activation 必须与 Always 语义等价")

	path, required := zero.RequiresConfigSection()
	assert.Equal(t, "", path)
	assert.False(t, required, "Always（零值）不要求任何配置节")
}

func TestConfiguredRequiresConfigSectionReportsPathAndRequired(t *testing.T) {
	a := Configured("plugins.gorm")
	path, required := a.RequiresConfigSection()
	assert.Equal(t, "plugins.gorm", path)
	assert.True(t, required)
}

func TestActivationStringDistinguishesAlwaysFromConfigured(t *testing.T) {
	assert.Equal(t, "always", Always.String())
	assert.Contains(t, Configured("plugins.gorm").String(), "plugins.gorm")
	assert.NotEqual(t, Always.String(), Configured("plugins.gorm").String())
}

func definitionFixturePlugin() Plugin { return &noBasePlugin{} }

func TestDefinitionValidateAcceptsWellFormed(t *testing.T) {
	d := Definition{Key: "gorm", Factory: definitionFixturePlugin, Activation: Always}
	assert.NoError(t, d.Validate())
}

func TestDefinitionValidateRejectsEmptyKey(t *testing.T) {
	d := Definition{Key: "", Factory: definitionFixturePlugin}
	err := d.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: ")
}

func TestDefinitionValidateRejectsMalformedKey(t *testing.T) {
	d := Definition{Key: "Gorm.DB", Factory: definitionFixturePlugin}
	assert.Error(t, d.Validate(), "插件 key 含大写/点号等保留字符必须被拒绝")
}

func TestDefinitionValidateRejectsNilFactory(t *testing.T) {
	d := Definition{Key: "gorm", Factory: nil}
	err := d.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Factory")
}
