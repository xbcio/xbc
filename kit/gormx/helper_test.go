package gormx

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormtests "gorm.io/gorm/utils/tests"
)

// newTestDB builds a GORM handle that never reaches a database: the dummy
// dialector quotes identifiers with backticks and binds every variable with a
// question mark, so clauses render into inspectable SQL plus a variable list.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormtests.DummyDialector{}, &gorm.Config{DryRun: true})
	require.NoError(t, err, "opening the statement builder failed")
	return db
}

// render builds the given clauses into SQL and records every bound variable.
func render(t *testing.T, db *gorm.DB, expressions ...clause.Expression) (string, []any) {
	t.Helper()
	statement := &gorm.Statement{DB: db}
	for _, expression := range expressions {
		expression.Build(statement)
	}
	return statement.SQL.String(), statement.Vars
}

// renderAssignment renders the single assignment for column from set.
func renderAssignment(t *testing.T, db *gorm.DB, set clause.Set, column string) (string, []any) {
	t.Helper()
	for _, assignment := range set {
		if assignment.Column.Name == column {
			return render(t, db, clause.Set{assignment})
		}
	}
	t.Fatalf("assignment for column %q not found", column)
	return "", nil
}
