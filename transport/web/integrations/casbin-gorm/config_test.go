package casbingorm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/plugin"
)

func TestDefaultConfigAndNormalization(t *testing.T) {
	want := Config{DBInstance: plugin.DefaultInstance, Table: defaultTable}
	assert.Equal(t, want, defaultConfig())

	prepared, err := prepareConfig(Config{})
	require.NoError(t, err)
	assert.Equal(t, want, prepared)

	prepared, err = prepareConfig(Config{DBInstance: " writer ", Table: "policy", TablePrefix: "tenant_", Migrate: true})
	require.NoError(t, err)
	assert.Equal(t, Config{DBInstance: "writer", Table: "policy", TablePrefix: "tenant_", Migrate: true}, prepared)
}

func TestConfigStrictlyValidatesDatabaseInstanceAndSQLIdentifiers(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
		field  string
	}{
		{name: "database instance", config: Config{DBInstance: "bad instance", Table: defaultTable}, field: "db_instance"},
		{name: "qualified table", config: Config{Table: "public.casbin_rule"}, field: "table"},
		{name: "quoted table", config: Config{Table: `"casbin_rule"`}, field: "table"},
		{name: "hyphenated table", config: Config{Table: "casbin-rule"}, field: "table"},
		{name: "table whitespace", config: Config{Table: " casbin_rule"}, field: "table"},
		{name: "table statement", config: Config{Table: "rules;DROP_TABLE"}, field: "table"},
		{name: "qualified prefix", config: Config{Table: defaultTable, TablePrefix: "public.tenant"}, field: "table_prefix"},
		{name: "prefix whitespace", config: Config{Table: defaultTable, TablePrefix: "tenant "}, field: "table_prefix"},
		{name: "prefix punctuation", config: Config{Table: defaultTable, TablePrefix: "tenant-1"}, field: "table_prefix"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.field)
		})
	}

	for _, identifier := range []string{"casbin_rule", "_casbin2", "Policy2"} {
		t.Run("valid table "+identifier, func(t *testing.T) {
			require.NoError(t, (Config{Table: identifier, TablePrefix: "tenant_"}).Validate())
		})
	}
}
