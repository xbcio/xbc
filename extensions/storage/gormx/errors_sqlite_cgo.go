//go:build cgo

package gormx

import (
	"errors"

	"github.com/mattn/go-sqlite3"
)

// sqliteUniqueViolation reports whether err is SQLite's UNIQUE or PRIMARY-KEY
// constraint failure. It is compiled only with cgo because the SQLite driver
// depends on cgo.
func sqliteUniqueViolation(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique ||
			sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
	}
	return false
}
