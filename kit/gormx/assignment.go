package gormx

import (
	"fmt"
	"regexp"
	"strings"

	"gorm.io/gorm/clause"
)

// ColumnType describes the empty-value rules applied to a TypedColumn.
type ColumnType string

const (
	// TypeString treats NULL and the empty string as empty.
	TypeString ColumnType = "string"
	// TypeNumeric treats NULL, the empty string, and zero as empty; negative
	// and zero values are configurable per column.
	TypeNumeric ColumnType = "numeric"
	// TypeUUID also treats the all-zero UUID as empty.
	TypeUUID ColumnType = "uuid"
	// TypeDateTime also treats the zero date forms as empty.
	TypeDateTime ColumnType = "datetime"
	// TypeJSON also treats '{}', '[]', and 'null' as empty.
	TypeJSON ColumnType = "json"
	// TypeBoolean treats NULL and the empty string as empty.
	TypeBoolean ColumnType = "boolean"
)

// TypedColumn names one column and the empty-value rules that apply to it when
// it participates in a non-empty assignment.
type TypedColumn struct {
	Name              string
	Type              ColumnType
	CustomEmptyValues []string
	AllowZero         bool
	AllowNegative     bool
}

// NumericColumn creates a numeric column that treats zero as empty and allows
// negative values.
func NumericColumn(name string) TypedColumn {
	return TypedColumn{
		Name:          name,
		Type:          TypeNumeric,
		AllowZero:     false,
		AllowNegative: true,
	}
}

// NumericColumnAllowZero creates a numeric column that treats zero as a real
// value and allows negative values.
func NumericColumnAllowZero(name string) TypedColumn {
	return TypedColumn{
		Name:          name,
		Type:          TypeNumeric,
		AllowZero:     true,
		AllowNegative: true,
	}
}

// NumericColumnPositive creates a numeric column that treats zero as empty and
// rejects negative values.
func NumericColumnPositive(name string) TypedColumn {
	return TypedColumn{
		Name:          name,
		Type:          TypeNumeric,
		AllowZero:     false,
		AllowNegative: false,
	}
}

// UUIDColumn creates a column that also treats the all-zero UUID as empty.
func UUIDColumn(name string) TypedColumn {
	return TypedColumn{Name: name, Type: TypeUUID}
}

// DateTimeColumn creates a column that also treats the zero date forms as
// empty.
func DateTimeColumn(name string) TypedColumn {
	return TypedColumn{Name: name, Type: TypeDateTime}
}

// JSONColumn creates a column that also treats '{}', '[]', and 'null' as
// empty.
func JSONColumn(name string) TypedColumn {
	return TypedColumn{Name: name, Type: TypeJSON}
}

// StringColumn creates a string column with optional custom empty values.
func StringColumn(name string, customEmptyValues ...string) TypedColumn {
	return TypedColumn{
		Name:              name,
		Type:              TypeString,
		CustomEmptyValues: customEmptyValues,
	}
}

// BooleanColumn creates a boolean column that treats NULL and the empty string
// as empty.
func BooleanColumn(name string) TypedColumn {
	return TypedColumn{Name: name, Type: TypeBoolean}
}

// columnNamePattern is the only shape accepted for a column name. Names are
// code constants, never request input; anything else is a programming error and
// panics rather than reaching a statement.
var columnNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// AssignmentNotEmptyColumns builds assignment clauses for an upsert's
// ON DUPLICATE KEY UPDATE list. A column is overwritten only when the incoming
// value is not empty; otherwise the existing value is kept.
//
// Three input forms may be mixed:
//
//  1. Plain string, treated as a string column:
//     "name", "email"
//
//  2. Prefix shorthand:
//     "numeric:age"             numeric, zero is empty, negative allowed
//     "numeric+zero:score"      numeric, zero is a real value
//     "numeric+positive:qty"    numeric, zero is empty, negative is empty
//     "uuid:domain_id"          also treats the all-zero UUID as empty
//     "datetime:created_at"     also treats the zero date forms as empty
//     "json:metadata"           also treats '{}', '[]', and 'null' as empty
//     "boolean:enabled"         NULL and the empty string only
//     "string:status:N/A"       string with custom empty values after the name
//
// The aliases numeric/num/n, numeric+zero/num+zero/n+z,
// numeric+positive/num+positive/n+p, uuid/u, datetime/dt/d, json/j,
// boolean/bool/b, and string/str/s are accepted. An unrecognized prefix is
// treated as part of a string column name, matching the shorthand's role as a
// convenience rather than a type system.
//
//  3. A TypedColumn value:
//     NumericColumn("age"), UUIDColumn("domain_id")
//
// Column names must match ^[A-Za-z_][A-Za-z0-9_]*$; an invalid name panics.
// Custom empty values are bound as statement parameters and never spliced into
// the SQL text.
func AssignmentNotEmptyColumns(columns ...any) clause.Set {
	values := make(map[string]any, len(columns))
	for _, column := range columns {
		typed := parseColumn(column)
		name := mustColumnName(typed.Name)

		conditions, vars := emptyConditions(name, typed)
		sql := "CASE WHEN " + strings.Join(conditions, " OR ") +
			" THEN " + name + " ELSE VALUES(" + name + ") END"
		values[name] = clause.Expr{SQL: sql, Vars: vars}
	}
	return clause.Assignments(values)
}

// parseColumn normalizes the three accepted input forms into a TypedColumn.
// Any other value is rendered as a string column name and validated like one,
// so a mistaken input fails loudly instead of reaching a statement.
func parseColumn(column any) TypedColumn {
	switch value := column.(type) {
	case TypedColumn:
		return value
	case string:
		return parseColumnString(value)
	default:
		return TypedColumn{Name: fmt.Sprint(value), Type: TypeString}
	}
}

// parseColumnString parses "type:column[:empty...]" shorthand. A string
// without a recognized prefix is a plain string column.
func parseColumnString(value string) TypedColumn {
	if !strings.Contains(value, ":") {
		return TypedColumn{Name: value, Type: TypeString}
	}

	parts := strings.Split(value, ":")
	if len(parts) < 2 {
		return TypedColumn{Name: value, Type: TypeString}
	}

	prefix := parts[0]
	name := parts[1]

	switch {
	case prefix == "numeric" || prefix == "num" || prefix == "n":
		return TypedColumn{Name: name, Type: TypeNumeric, AllowZero: false, AllowNegative: true}

	case prefix == "numeric+zero" || prefix == "num+zero" || prefix == "n+z":
		return TypedColumn{Name: name, Type: TypeNumeric, AllowZero: true, AllowNegative: true}

	case prefix == "numeric+positive" || prefix == "num+positive" || prefix == "n+p":
		return TypedColumn{Name: name, Type: TypeNumeric, AllowZero: false, AllowNegative: false}

	case prefix == "uuid" || prefix == "u":
		return TypedColumn{Name: name, Type: TypeUUID}

	case prefix == "datetime" || prefix == "dt" || prefix == "d":
		return TypedColumn{Name: name, Type: TypeDateTime}

	case prefix == "json" || prefix == "j":
		return TypedColumn{Name: name, Type: TypeJSON}

	case prefix == "boolean" || prefix == "bool" || prefix == "b":
		return TypedColumn{Name: name, Type: TypeBoolean}

	case prefix == "string" || prefix == "str" || prefix == "s":
		return TypedColumn{Name: name, Type: TypeString, CustomEmptyValues: parts[2:]}

	default:
		return TypedColumn{Name: name, Type: TypeString}
	}
}

// mustColumnName returns name when it is a legal identifier and panics
// otherwise. The panic is intentional: a column name is a code constant, and a
// bad one must not become part of a statement.
func mustColumnName(name string) string {
	if !columnNamePattern.MatchString(name) {
		panic(fmt.Sprintf("gormx: invalid column name %q: must match %s", name, columnNamePattern))
	}
	return name
}

// emptyConditions returns the SQL predicates that mark the incoming value as
// empty and the parameters those predicates bind. Only fixed literals stay in
// the SQL text; custom empty values are always parameters.
func emptyConditions(name string, column TypedColumn) ([]string, []any) {
	values := "VALUES(" + name + ")"
	conditions := []string{
		values + " IS NULL",
		"TRIM(" + values + ") = ''",
	}
	var vars []any

	switch column.Type {
	case TypeNumeric:
		if !column.AllowZero {
			conditions = append(conditions, values+" = 0")
		}
		if !column.AllowNegative {
			conditions = append(conditions, values+" < 0")
		}

	case TypeUUID:
		conditions = append(conditions, values+" = '00000000-0000-0000-0000-000000000000'")

	case TypeDateTime:
		conditions = append(conditions,
			values+" = '0000-00-00'",
			values+" = '0000-00-00 00:00:00'",
		)

	case TypeJSON:
		conditions = append(conditions,
			values+" = '{}'",
			values+" = '[]'",
			values+" = 'null'",
		)
	}

	for _, empty := range column.CustomEmptyValues {
		conditions = append(conditions, values+" = ?")
		vars = append(vars, empty)
	}

	return conditions, vars
}
