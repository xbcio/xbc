// Package gormquery provides protocol-neutral query helpers for services that
// build GORM queries: pagination, sorting, time ranges, and "update only when
// non-empty" assignment clauses.
//
// The package depends on GORM and the GORM Gen field package only. It imports
// no web framework, no transport, and no error type that carries an HTTP
// meaning, so a caller decides how a failure is presented to its own clients.
//
// # Pagination
//
// Pagination binds the "current" and "pageSize" query parameters. The struct
// tags follow the form-tag convention Gin uses for query binding, including its
// "default=..." option; reading the tags is a convention only, the package
// never imports Gin or any other web framework. The methods are defensive on
// their own: a nil field, a nil receiver, or a non-positive value falls back to
// DefaultCurrent and DefaultPageSize, so a value built in code behaves like one
// bound from a request. PageSize is capped at MaxPageSize so one request cannot
// ask for an unbounded page. The offset is never negative, including when
// Current is absurdly large.
//
// ToOffsetAndLimit clears Current and PageSize after converting them;
// OffsetAndLimit, Offset, and Limit leave the receiver untouched. See
// "Mutating and pure variants" below.
//
// # Sorting
//
// Sorter binds "sorterField" and "sorterOrder". ToOrderExpr resolves the field
// through a Dao (typically a generated query handle) and returns a GORM Gen
// expression: "ascend" or "asc" (case-insensitive) sorts ascending and every
// other value, including the empty string, sorts descending, matching the
// values existing clients send. An unknown field yields an error wrapping
// ErrUnknownSortField that names the table and the field. It is not an HTTP
// error; a caller maps it to its own status code.
//
// # Time ranges
//
// DateTimeRange binds "start_time" and "end_time" and expects the
// "2006-01-02 15:04:05" layout, which the binding tag enforces. ToExpr returns
// a BETWEEN expression only when both ends are present and parse; otherwise it
// returns nil so a caller can skip the predicate entirely.
//
// # Non-empty assignments
//
// TypedColumn and AssignmentNotEmptyColumns build the right-hand side of an
// ON DUPLICATE KEY UPDATE clause for upserts: a column is overwritten only when
// the proposed value is not empty, so an upsert carrying a partial record does
// not erase data the incoming row did not mention. The generated expression
// uses the MySQL-style VALUES(column) form. A column may be named as a plain
// string, as a "type:column" prefix shorthand, or as a TypedColumn value; see
// AssignmentNotEmptyColumns for the accepted prefixes.
//
// # Security constraints
//
// Column names are identifiers, not data. Every name is validated against a
// conservative identifier pattern and an invalid name panics at the call site;
// names are expected to be code constants and never request input. The fixed
// empty-value literals (the empty string, zero, the all-zero UUID, and the zero
// date forms) are part of the generated SQL, while every caller-supplied custom
// empty value is passed as a bound parameter, so a quote or a backslash in such
// a value is data and cannot change the statement.
//
// # Mutating and pure variants
//
// ToOffsetAndLimit, ToOrderExpr, and ToExpr clear the receiver after a
// successful conversion. That is not decoration: request value objects embed
// these structs and are sometimes handed to GORM as a query condition
// afterwards, so leaving bound parameters behind would turn "current=1" into a
// predicate on a column that does not exist. The same behavior keeps existing
// call sites working unchanged. New code that does not need the side effect can
// use OffsetAndLimit, OrderExpr, and Expr, which never modify their inputs.
package gormquery
