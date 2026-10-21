// Package gorm integrates named GORM database connections with XBC's plugin
// lifecycle. Importing this package is side-effect free. Applications should
// compose Bundle explicitly; executables may blank-import the autoload
// subpackage when they intentionally use XBC's default composition.
//
// # Usage
//
// Each key below plugins.gorm is a separate instance whose sole primary value
// is one *gorm.io/gorm.DB:
//
//	plugins:
//	  gorm:
//	    default:
//	      driver: mysql
//	      dsn: ${MYSQL_DSN}
//	      max_open_conn: 20
//	      max_idle_conn: 10
//	      conn_max_lifetime: 1h
//	      conn_max_idle_time: 30m
//	      prepare_stmt: false
//	      skip_default_transaction: false
//	      translate_error: false
//	    readonly:
//	      driver: postgres
//	      dsn: ${POSTGRES_READONLY_DSN}
//
// Declare a typed reference once and consume its pre-bound value in another
// Definition's factory. In this example gormplugin aliases this package and
// gormlib aliases gorm.io/gorm:
//
//	var ordersDB = plugin.RefToInstance[*gormlib.DB](gormplugin.Key, "primary")
//
//	var repositoryDefinition = plugin.Define(
//		"order-repository",
//		func(ctx plugin.BuildContext) (*orderRepository, error) {
//			return &orderRepository{db: ordersDB.Get(ctx).Value}, nil
//		},
//		plugin.Options[*orderRepository]{
//			Inputs: plugin.Inputs(ordersDB),
//		},
//	)
//
// Supported drivers are mysql (default), postgres, sqlite, and sqlserver. dsn
// is required. max_open_conn defaults to 20 and must be at least 1;
// max_idle_conn defaults to 10 and must be between 0 and max_open_conn.
// Connection lifetime defaults to one hour and idle time to 30 minutes; set a
// duration to zero to disable that limit.
//
// Each instance owns one connection pool. Construction opens the database and
// applies pool bounds; Init pings it before startup can proceed. Stop closes
// prepared statements and the underlying sql.DB safely after any owned
// lifecycle state. Keep DSNs in secret-backed configuration rather than source
// or logs, and use a least-privilege account for each instance.
//
// translate_error makes GORM translate driver errors into its portable
// sentinel errors, so callers can test for gorm.ErrDuplicatedKey and
// gorm.ErrForeignKeyViolated with errors.Is. It is off by default because the
// translated values replace the driver's own errors.
//
// # Direct embedding
//
// A caller that owns its connection lifecycle without an XBC application can
// construct the same value directly: DefaultConfig returns the binder's
// defaults, Open validates the configuration and applies the pool settings
// without a separate readiness ping, and Close releases the prepared-statement
// store and the connection pool. Own the result for as long as the connection
// is in use, and call Close when it is no longer needed; Close is idempotent
// and safe on a nil database.
//
//	db, err := gormplugin.Open(cfg)
//	if err != nil {
//		return err
//	}
//	defer gormplugin.Close(db)
//
// Open is the path for a host that owns its own lifecycle. It is not part of
// an XBC application's exports: a process that assembles Bundle takes its
// instance from the plugin instead. Do not reach the same data source through
// both paths in one process -- two pools against one database are difficult to
// attribute when connection or transaction behavior differs.
//
// # Readiness
//
// Bundle also selects HealthKey, a second Definition that exports
// health.Contributor and reports one readiness check per configured instance. It
// is a separate Definition because this plugin's primary value is the
// third-party *gorm.DB, which cannot be given a HealthChecks method. The probe
// is inert unless the application also selects the health capability Bundle;
// when it is selected, a configured instance appears in the aggregate readiness
// report as "gorm-health" or "gorm-health/<instance>" with no change at the
// composition root.
package gorm
