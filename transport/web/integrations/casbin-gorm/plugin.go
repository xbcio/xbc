package casbingorm

import (
	"errors"
	"fmt"

	gormadapter "github.com/casbin/gorm-adapter/v3"
	"github.com/xbcio/xbc/plugin"
	casbinplugin "github.com/xbcio/xbc/transport/web/integrations/casbin"
	"gorm.io/gorm"
)

// Key is the stable configuration and dependency identity of this plugin.
const Key plugin.Key = "casbin-gorm"

const gormPluginKey plugin.Key = "gorm"

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Provider]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.casbin-gorm"),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(provider *Provider) casbinplugin.AdapterProvider { return provider }),
		),
		Lifecycle: plugin.Lifecycle[*Provider]{
			Migrate: (*Provider).migrate,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns the canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns side-effect-free composition data for explicit selection.
func Bundle() plugin.Bundle { return bundle }

// plan declares the exact named *gorm.DB input selected by this instance. It
// is pure and does not create an adapter or touch the database.
func plan(config Config) (plugin.Plan[*Provider], error) {
	database := plugin.RefToInstance[*gorm.DB](gormPluginKey, config.DBInstance)
	return plugin.PlanOf(plugin.Inputs(database), func(context plugin.BuildContext) (*Provider, error) {
		db := database.Get(context).Value
		if db == nil {
			return nil, fmt.Errorf("casbin-gorm: GORM database instance %q is nil", config.DBInstance)
		}
		return newProvider(db, config)
	}), nil
}

// migrate is inert unless migrate=true. The enabled path deliberately creates
// a fresh session whose context does not carry the construction-time
// TurnOffAutoMigrate marker, then delegates both table and unique-index shape
// to the pinned upstream adapter constructor.
func (provider *Provider) migrate(context *plugin.Context) error {
	if provider == nil {
		return errors.New("casbin-gorm: Migrate requires a provider")
	}
	if !provider.config.Migrate {
		return nil
	}
	if context == nil {
		return errors.New("casbin-gorm: Migrate requires a non-nil plugin context")
	}
	if err := context.Err(); err != nil {
		return err
	}
	if provider.db == nil {
		return errors.New("casbin-gorm: Migrate requires a database")
	}

	migrationDB := provider.db.Session(&gorm.Session{NewDB: true, Context: context})
	if migrationDB == nil || migrationDB.Error != nil {
		if migrationDB == nil {
			return errors.New("casbin-gorm: create migration database session: nil session")
		}
		return fmt.Errorf("casbin-gorm: create migration database session: %w", migrationDB.Error)
	}
	if _, err := gormadapter.NewAdapterByDBUseTableName(
		migrationDB,
		provider.config.TablePrefix,
		provider.config.Table,
	); err != nil {
		return fmt.Errorf("casbin-gorm: migrate policy table: %w", err)
	}
	return nil
}
