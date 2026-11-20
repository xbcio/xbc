package gormx

import (
	"errors"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	gormlib "gorm.io/gorm"
)

const (
	mysqlErrDuplicateEntry = 1062

	postgresUniqueViolation = "23505"
)

// IsDuplicateKey reports whether err is a unique or primary-key conflict. It
// recognizes GORM's ErrDuplicatedKey sentinel, MySQL error number 1062,
// PostgreSQL SQLSTATE 23505, and, in cgo-enabled builds, SQLite's UNIQUE and
// PRIMARY-KEY extended result codes. Without cgo the SQLite driver is a stub
// that cannot open a database, so SQLite errors never reach this function.
//
// The common legacy idiom of searching an error message for "duplicate entry"
// or "duplicate key" is deliberately absent. Those substrings also appear in
// unrelated errors, and translated or wrapped messages vary by driver and
// server version, so a textual fallback turns ordinary failures into false
// positives. An error that carries no structured duplicate-key signal returns
// false even when its message resembles one. Errors from other dialects also
// return false, as does a nil error.
func IsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, gormlib.ErrDuplicatedKey) {
		return true
	}

	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == mysqlErrDuplicateEntry
	}

	if sqliteUniqueViolation(err) {
		return true
	}

	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) {
		return postgresErr.Code == postgresUniqueViolation
	}

	return false
}
