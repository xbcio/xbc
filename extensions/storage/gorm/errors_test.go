package gorm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	gormlib "gorm.io/gorm"
)

func TestIsDuplicateKey(t *testing.T) {
	t.Run("NilErrorIsNotADuplicate", func(t *testing.T) {
		assert.False(t, IsDuplicateKey(nil))
	})

	t.Run("GormSentinel", func(t *testing.T) {
		assert.True(t, IsDuplicateKey(gormlib.ErrDuplicatedKey))
		assert.True(t, IsDuplicateKey(fmt.Errorf("insert order: %w", gormlib.ErrDuplicatedKey)))
		assert.True(t, IsDuplicateKey(errors.Join(errors.New("retry exhausted"), gormlib.ErrDuplicatedKey)))
	})

	t.Run("MySQLDuplicateEntry", func(t *testing.T) {
		err := &mysql.MySQLError{Number: mysqlErrDuplicateEntry, Message: "Duplicate entry '1' for key 'PRIMARY'"}
		assert.True(t, IsDuplicateKey(err))
		assert.True(t, IsDuplicateKey(fmt.Errorf("insert order: %w", err)))
		assert.True(t, IsDuplicateKey(errors.Join(errors.New("retry exhausted"), err)))
	})

	t.Run("MySQLOtherError", func(t *testing.T) {
		assert.False(t, IsDuplicateKey(&mysql.MySQLError{Number: 1054, Message: "Unknown column 'x'"}))
	})

	t.Run("PostgresUniqueViolation", func(t *testing.T) {
		err := &pgconn.PgError{Code: postgresUniqueViolation, Message: "duplicate key value violates unique constraint"}
		assert.True(t, IsDuplicateKey(err))
		assert.True(t, IsDuplicateKey(fmt.Errorf("insert order: %w", err)))
		assert.True(t, IsDuplicateKey(errors.Join(errors.New("retry exhausted"), err)))
	})

	t.Run("PostgresOtherError", func(t *testing.T) {
		assert.False(t, IsDuplicateKey(&pgconn.PgError{Code: "42601", Message: "syntax error"}))
	})

	t.Run("MessageTextAloneIsNotEnough", func(t *testing.T) {
		assert.False(t, IsDuplicateKey(errors.New("Error 1062: Duplicate entry '1' for key 'PRIMARY'")))
		assert.False(t, IsDuplicateKey(errors.New("duplicate key value violates unique constraint \"orders_pkey\"")))
		assert.False(t, IsDuplicateKey(fmt.Errorf("insert order: %w", errors.New("duplicate key"))))
	})

	t.Run("UnrelatedError", func(t *testing.T) {
		assert.False(t, IsDuplicateKey(errors.New("connection refused")))
	})
}
