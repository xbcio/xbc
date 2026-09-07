package casbingorm

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/xbcio/xbc/plugin"
)

const defaultTable = "casbin_rule"

var sqlIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config selects the shared GORM connection and the Casbin policy table.
// Migrate is deliberately false by default: schema writes occur only in XBC's
// explicit migration stage.
type Config struct {
	DBInstance  string `yaml:"db_instance" default:"default"`
	Table       string `yaml:"table" default:"casbin_rule"`
	TablePrefix string `yaml:"table_prefix"`
	Migrate     bool   `yaml:"migrate" default:"false"`
}

func defaultConfig() Config {
	return Config{
		DBInstance: plugin.DefaultInstance,
		Table:      defaultTable,
	}
}

func prepareConfig(config Config) (Config, error) {
	defaults := defaultConfig()
	if strings.TrimSpace(config.DBInstance) == "" {
		config.DBInstance = defaults.DBInstance
	}
	if config.Table == "" {
		config.Table = defaults.Table
	}

	config.DBInstance = plugin.NormalizeInstance(strings.TrimSpace(config.DBInstance))
	if err := plugin.ValidateInstanceName(config.DBInstance); err != nil {
		return Config{}, fmt.Errorf("casbin-gorm: invalid db_instance: %w", err)
	}
	if !sqlIdentifierPattern.MatchString(config.Table) {
		return Config{}, fmt.Errorf("casbin-gorm: table must be one unquoted SQL identifier, got %q", config.Table)
	}
	if config.TablePrefix != "" && !sqlIdentifierPattern.MatchString(config.TablePrefix) {
		return Config{}, fmt.Errorf("casbin-gorm: table_prefix must be an empty value or one unquoted SQL identifier, got %q", config.TablePrefix)
	}
	return config, nil
}

// Validate checks configuration without acquiring or querying a database.
func (config Config) Validate() error {
	_, err := prepareConfig(config)
	return err
}
