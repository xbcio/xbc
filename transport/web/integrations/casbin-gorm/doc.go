// Package casbingorm provides a persistent Casbin adapter backed by one exact,
// named *gorm.DB exported by XBC's GORM integration. The database connection
// remains owned by its producer; this package neither opens nor closes it.
//
// # Usage
//
// Compose the GORM producer, this adapter provider, and Casbin explicitly:
//
//	app, err := xbc.New(xbc.WithBundles(
//		gormplugin.Bundle(),
//		casbingorm.Bundle(),
//		casbinplugin.Bundle(),
//	))
//	if err != nil {
//		return err
//	}
//	_, err = app.Execute(ctx, args)
//	return err
//
// Configure each casbin-gorm instance with db_instance naming the exact GORM
// instance it consumes. table defaults to casbin_rule, table_prefix is
// optional, and migrate defaults to false. Construction always disables the
// upstream adapter's implicit AutoMigrate behavior. Setting migrate:true only
// permits schema creation during XBC's explicit Migrate stage; normal startup
// never performs DDL.
//
// AdapterProvider.Adapter returns an interface whose dynamic value is
// *gormadapter.Adapter. This preserves Casbin's persistence methods while
// keeping connection ownership with the GORM plugin.
//
// The pinned upstream gorm-adapter v3.39.0 legacy Adapter.Transaction method
// recreates its transaction adapter with the default table name. It is safe for
// casbin_rule, but applications using a custom table must use a corrected
// upstream/fork (SAS currently supplies one with a module replacement) or the
// adapter's transaction-context API that retains table metadata. XBC does not
// hide this upstream behavior behind a fork or local replacement.
package casbingorm
