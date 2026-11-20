//go:build !cgo

package gormx

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	gormlib "gorm.io/gorm"
)

func TestSQLiteUniqueViolationWithoutCGO(t *testing.T) {
	assert.False(t, sqliteUniqueViolation(nil))
	assert.False(t, sqliteUniqueViolation(errors.New("UNIQUE constraint failed: duplicate_probe.name")))
	assert.False(t, sqliteUniqueViolation(fmt.Errorf("insert order: %w", errors.New("constraint failed"))))
	assert.False(t, sqliteUniqueViolation(errors.Join(errors.New("retry exhausted"), errors.New("UNIQUE constraint failed"))))
}

func TestIsDuplicateKeyWithoutCGO(t *testing.T) {
	assert.False(t, IsDuplicateKey(nil))
	assert.True(t, IsDuplicateKey(gormlib.ErrDuplicatedKey))

	mysqlDuplicate := &mysql.MySQLError{Number: mysqlErrDuplicateEntry, Message: "Duplicate entry '1' for key 'PRIMARY'"}
	assert.True(t, IsDuplicateKey(mysqlDuplicate))
	assert.True(t, IsDuplicateKey(fmt.Errorf("insert order: %w", mysqlDuplicate)))
	assert.False(t, IsDuplicateKey(&mysql.MySQLError{Number: 1054, Message: "Unknown column 'x'"}))

	postgresDuplicate := &pgconn.PgError{Code: postgresUniqueViolation, Message: "duplicate key value violates unique constraint"}
	assert.True(t, IsDuplicateKey(postgresDuplicate))
	assert.True(t, IsDuplicateKey(fmt.Errorf("insert order: %w", postgresDuplicate)))
	assert.False(t, IsDuplicateKey(&pgconn.PgError{Code: "42601", Message: "syntax error"}))
}
