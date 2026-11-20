//go:build cgo

package gormx

import (
	"context"
	"fmt"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gormlib "gorm.io/gorm"

	"github.com/xbcio/xbc/plugin"
)

func openSQLiteForErrorTests(t *testing.T) *gormlib.DB {
	t.Helper()
	db, err := openDatabase(plugin.BuildContext{}, sqliteConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopDatabase(db, context.Background()) })
	return db
}

func TestIsDuplicateKeySQLite(t *testing.T) {
	t.Run("SQLiteUniqueConstraint", func(t *testing.T) {
		err := sqlite3.Error{Code: sqlite3.ErrConstraint, ExtendedCode: sqlite3.ErrConstraintUnique}
		assert.True(t, IsDuplicateKey(err))
		assert.True(t, IsDuplicateKey(fmt.Errorf("insert order: %w", err)))
	})

	t.Run("SQLitePrimaryKeyConstraint", func(t *testing.T) {
		err := sqlite3.Error{Code: sqlite3.ErrConstraint, ExtendedCode: sqlite3.ErrConstraintPrimaryKey}
		assert.True(t, IsDuplicateKey(err))
	})

	t.Run("SQLiteOtherConstraint", func(t *testing.T) {
		err := sqlite3.Error{Code: sqlite3.ErrConstraint, ExtendedCode: sqlite3.ErrConstraintNotNull}
		assert.False(t, IsDuplicateKey(err))
	})

	t.Run("SQLiteRealDuplicate", func(t *testing.T) {
		db := openSQLiteForErrorTests(t)
		require.NoError(t, db.Exec("CREATE TABLE duplicate_probe (id INTEGER PRIMARY KEY, name TEXT UNIQUE)").Error)
		require.NoError(t, db.Exec("INSERT INTO duplicate_probe (id, name) VALUES (1, 'first')").Error)
		err := db.Exec("INSERT INTO duplicate_probe (id, name) VALUES (1, 'second')").Error
		require.Error(t, err)
		assert.True(t, IsDuplicateKey(err), "driver error: %v", err)
	})

	t.Run("SQLiteMissingTableIsNotADuplicate", func(t *testing.T) {
		db := openSQLiteForErrorTests(t)
		err := db.Exec("INSERT INTO absent_table (id) VALUES (1)").Error
		require.Error(t, err)
		assert.False(t, IsDuplicateKey(err), "driver error: %v", err)
	})
}
