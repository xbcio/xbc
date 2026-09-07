package casbingorm

import (
	"errors"
	"fmt"

	"github.com/casbin/casbin/v2/persist"
	gormadapter "github.com/casbin/gorm-adapter/v3"
	casbinplugin "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin"
	"gorm.io/gorm"
)

// Provider supplies the concrete gorm-adapter while retaining the shared GORM
// connection only for an explicitly requested migration. It never owns or
// closes that connection or its database/sql pool.
type Provider struct {
	adapter *gormadapter.Adapter
	db      *gorm.DB
	config  Config
}

var _ casbinplugin.AdapterProvider = (*Provider)(nil)

// Adapter returns the concrete *gormadapter.Adapter as a persist.Adapter. The
// interface's dynamic value intentionally remains *gormadapter.Adapter so
// callers can use adapter-specific transaction support when required.
func (provider *Provider) Adapter() persist.Adapter {
	if provider == nil {
		return nil
	}
	return provider.adapter
}

func newProvider(db *gorm.DB, config Config) (*Provider, error) {
	if db == nil {
		return nil, errors.New("casbin-gorm: GORM database is nil")
	}
	if db.Config == nil || db.Statement == nil {
		return nil, errors.New("casbin-gorm: GORM database is not initialized")
	}
	if db.Error != nil {
		return nil, fmt.Errorf("casbin-gorm: GORM database is unusable: %w", db.Error)
	}

	// Session returns a DB handle with cloned session state over the same pool.
	// TurnOffAutoMigrate mutates only that clone, never the producer's *gorm.DB.
	adapterDB := db.Session(&gorm.Session{NewDB: true})
	if adapterDB == nil || adapterDB.Error != nil {
		if adapterDB == nil {
			return nil, errors.New("casbin-gorm: create adapter database session: nil session")
		}
		return nil, fmt.Errorf("casbin-gorm: create adapter database session: %w", adapterDB.Error)
	}
	gormadapter.TurnOffAutoMigrate(adapterDB)

	adapter, err := gormadapter.NewAdapterByDBUseTableName(adapterDB, config.TablePrefix, config.Table)
	if err != nil {
		return nil, fmt.Errorf("casbin-gorm: construct adapter: %w", err)
	}
	if adapter == nil {
		return nil, errors.New("casbin-gorm: construct adapter: upstream returned nil")
	}
	return &Provider{adapter: adapter, db: db, config: config}, nil
}
