// Package gorm integrates named GORM database connections with XBC's plugin
// lifecycle. Importing this package is side-effect free. Applications should
// compose Bundle explicitly; executables may blank-import the autoload
// subpackage when they intentionally use XBC's default composition.
//
// # Usage
//
// Each configured instance has *gorm.io/gorm.DB as its primary value. Declare a
// typed reference once and consume its pre-bound value in another Definition's
// factory. In this example gormplugin aliases this package and gormlib aliases
// gorm.io/gorm:
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
// Each instance owns one connection pool. Construction opens the database and
// applies pool bounds; Init pings it before startup can proceed. Stop closes
// prepared statements and the underlying sql.DB safely after any owned
// lifecycle state. Keep DSNs in secret-backed configuration rather than source
// or logs, and use a least-privilege account for each instance.
package gorm
