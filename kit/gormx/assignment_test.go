package gormx

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectedAssignmentSQL renders the SQL the package is expected to produce for
// one column. The outer column is quoted by the clause builder; the column
// references inside the CASE expression stay bare, as they are validated
// identifiers.
func expectedAssignmentSQL(column string, extraConditions ...string) string {
	conditions := []string{
		"VALUES(" + column + ") IS NULL",
		"TRIM(VALUES(" + column + ")) = ''",
	}
	conditions = append(conditions, extraConditions...)
	return "`" + column + "`=CASE WHEN " + strings.Join(conditions, " OR ") +
		" THEN " + column + " ELSE VALUES(" + column + ") END"
}

func TestAssignmentNotEmptyColumnsTypes(t *testing.T) {
	db := newTestDB(t)

	for _, testCase := range []struct {
		name        string
		input       any
		column      string
		extra       []string
		wantVars    []any
		wantQueries int
	}{
		{name: "plain string", input: "name", column: "name"},
		{
			name:   "numeric",
			input:  "numeric:age",
			column: "age",
			extra:  []string{"VALUES(age) = 0"},
		},
		{
			name:   "numeric allows zero",
			input:  "numeric+zero:score",
			column: "score",
		},
		{
			name:   "numeric positive",
			input:  "numeric+positive:qty",
			column: "qty",
			extra:  []string{"VALUES(qty) = 0", "VALUES(qty) < 0"},
		},
		{
			name:   "uuid",
			input:  "uuid:domain_id",
			column: "domain_id",
			extra:  []string{"VALUES(domain_id) = '00000000-0000-0000-0000-000000000000'"},
		},
		{
			name:   "datetime",
			input:  "datetime:created_at",
			column: "created_at",
			extra: []string{
				"VALUES(created_at) = '0000-00-00'",
				"VALUES(created_at) = '0000-00-00 00:00:00'",
			},
		},
		{
			name:   "json",
			input:  "json:metadata",
			column: "metadata",
			extra: []string{
				"VALUES(metadata) = '{}'",
				"VALUES(metadata) = '[]'",
				"VALUES(metadata) = 'null'",
			},
		},
		{
			name:   "boolean",
			input:  "boolean:enabled",
			column: "enabled",
		},
		{
			name:        "string with custom empty values",
			input:       "string:status:N/A:null",
			column:      "status",
			extra:       []string{"VALUES(status) = ?", "VALUES(status) = ?"},
			wantVars:    []any{"N/A", "null"},
			wantQueries: 2,
		},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			set := AssignmentNotEmptyColumns(testCase.input)
			require.Len(t, set, 1)

			sql, vars := renderAssignment(t, db, set, testCase.column)
			assert.Equal(t, expectedAssignmentSQL(testCase.column, testCase.extra...), sql)
			assert.Equal(t, testCase.wantVars, vars)
			assert.Equal(t, testCase.wantQueries, strings.Count(sql, "?"))
		})
	}
}

func TestAssignmentNotEmptyColumnsInputForms(t *testing.T) {
	db := newTestDB(t)

	for _, testCase := range []struct {
		prefix string
		typed  TypedColumn
	}{
		{prefix: "numeric:age", typed: NumericColumn("age")},
		{prefix: "numeric+zero:score", typed: NumericColumnAllowZero("score")},
		{prefix: "numeric+positive:qty", typed: NumericColumnPositive("qty")},
		{prefix: "uuid:domain_id", typed: UUIDColumn("domain_id")},
		{prefix: "datetime:created_at", typed: DateTimeColumn("created_at")},
		{prefix: "json:metadata", typed: JSONColumn("metadata")},
		{prefix: "boolean:enabled", typed: BooleanColumn("enabled")},
		{prefix: "string:status:N/A", typed: StringColumn("status", "N/A")},
	} {
		testCase := testCase
		t.Run(testCase.typed.Name, func(t *testing.T) {
			fromPrefix := AssignmentNotEmptyColumns(testCase.prefix)
			fromTyped := AssignmentNotEmptyColumns(testCase.typed)
			require.Len(t, fromPrefix, 1)
			require.Len(t, fromTyped, 1)

			prefixSQL, prefixVars := renderAssignment(t, db, fromPrefix, testCase.typed.Name)
			typedSQL, typedVars := renderAssignment(t, db, fromTyped, testCase.typed.Name)
			assert.Equal(t, typedSQL, prefixSQL)
			assert.Equal(t, typedVars, prefixVars)
		})
	}

	t.Run("aliases resolve like their canonical prefixes", func(t *testing.T) {
		for _, testCase := range []struct {
			alias     string
			canonical string
		}{
			{alias: "num:age", canonical: "numeric:age"},
			{alias: "n:age", canonical: "numeric:age"},
			{alias: "n+z:score", canonical: "numeric+zero:score"},
			{alias: "num+zero:score", canonical: "numeric+zero:score"},
			{alias: "n+p:qty", canonical: "numeric+positive:qty"},
			{alias: "num+positive:qty", canonical: "numeric+positive:qty"},
			{alias: "u:domain_id", canonical: "uuid:domain_id"},
			{alias: "dt:created_at", canonical: "datetime:created_at"},
			{alias: "d:created_at", canonical: "datetime:created_at"},
			{alias: "j:metadata", canonical: "json:metadata"},
			{alias: "bool:enabled", canonical: "boolean:enabled"},
			{alias: "b:enabled", canonical: "boolean:enabled"},
			{alias: "str:status:N/A", canonical: "string:status:N/A"},
			{alias: "s:status:N/A", canonical: "string:status:N/A"},
		} {
			aliasSet := AssignmentNotEmptyColumns(testCase.alias)
			canonicalSet := AssignmentNotEmptyColumns(testCase.canonical)
			require.Len(t, aliasSet, 1)
			require.Len(t, canonicalSet, 1)
			assert.Equal(t,
				canonicalSet[0].Column.Name,
				aliasSet[0].Column.Name,
				"column name for %q", testCase.alias,
			)

			aliasSQL, aliasVars := renderAssignment(t, db, aliasSet, aliasSet[0].Column.Name)
			canonicalSQL, canonicalVars := renderAssignment(t, db, canonicalSet, aliasSet[0].Column.Name)
			assert.Equal(t, canonicalSQL, aliasSQL, "SQL for %q", testCase.alias)
			assert.Equal(t, canonicalVars, aliasVars, "vars for %q", testCase.alias)
		}
	})

	t.Run("unrecognized prefix is a string column name", func(t *testing.T) {
		set := AssignmentNotEmptyColumns("mystery:label")
		require.Len(t, set, 1)
		sql, vars := renderAssignment(t, db, set, "label")
		assert.Equal(t, expectedAssignmentSQL("label"), sql)
		assert.Empty(t, vars)
	})

	t.Run("forms may be mixed and assignments are ordered by column", func(t *testing.T) {
		set := AssignmentNotEmptyColumns("name", "numeric:age", NumericColumn("balance"))
		require.Len(t, set, 3)

		names := make([]string, 0, len(set))
		for _, assignment := range set {
			names = append(names, assignment.Column.Name)
		}
		assert.Equal(t, []string{"age", "balance", "name"}, names)
	})
}

func TestAssignmentNotEmptyColumnsRejectsInvalidIdentifiers(t *testing.T) {
	for _, name := range []string{
		"",
		"1st",
		"bad name",
		"bad-name",
		"bad;drop",
		"bad`name",
		"bad.column",
		"bad'quote",
	} {
		name := name
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			assertInvalidColumnPanics(t, name, func() {
				AssignmentNotEmptyColumns(name)
			})
		})
	}

	t.Run("typed column name", func(t *testing.T) {
		assertInvalidColumnPanics(t, "bad-name", func() {
			AssignmentNotEmptyColumns(TypedColumn{Name: "bad-name", Type: TypeString})
		})
	})
}

func assertInvalidColumnPanics(t *testing.T, name string, build func()) {
	t.Helper()
	defer func() {
		recovered := recover()
		require.NotNil(t, recovered, "an invalid column name must panic")
		message := fmt.Sprint(recovered)
		assert.Contains(t, message, "invalid column name")
		assert.Contains(t, message, name)
	}()
	build()
}

func TestAssignmentNotEmptyColumnsParameterizesCustomEmptyValues(t *testing.T) {
	db := newTestDB(t)

	customValues := []string{"N/A", `x' OR '1'='1`, `back\slash`}
	set := AssignmentNotEmptyColumns(StringColumn("status", customValues...))
	require.Len(t, set, 1)

	sql, vars := renderAssignment(t, db, set, "status")
	assert.Equal(t, expectedAssignmentSQL("status",
		"VALUES(status) = ?",
		"VALUES(status) = ?",
		"VALUES(status) = ?",
	), sql)
	assert.Equal(t, []any{"N/A", `x' OR '1'='1`, `back\slash`}, vars)
	assert.Equal(t, 3, strings.Count(sql, "?"))

	for _, value := range customValues {
		assert.NotContains(t, sql, value, "a custom empty value must not enter the SQL text")
	}
	assert.NotContains(t, sql, "'1'='1")
	assert.NotContains(t, sql, `\`)
	assert.NotContains(t, sql, "' OR '")
}
