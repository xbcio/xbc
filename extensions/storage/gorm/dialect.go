package gorm

import (
	"fmt"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/driver/sqlserver"
	gormlib "gorm.io/gorm"
)

func dialector(driver, dsn string) (gormlib.Dialector, error) {
	switch driver {
	case "mysql":
		return mysql.Open(dsn), nil
	case "postgres":
		return postgres.Open(dsn), nil
	case "sqlite":
		return sqlite.Open(dsn), nil
	case "sqlserver":
		return sqlserver.Open(dsn), nil
	default:
		return nil, fmt.Errorf("xbc gorm: unsupported driver %q (supported: mysql, postgres, sqlite, sqlserver)", driver)
	}
}
