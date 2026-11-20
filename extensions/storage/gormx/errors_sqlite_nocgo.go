//go:build !cgo

package gormx

// sqliteUniqueViolation always reports false: without cgo the SQLite driver is
// a stub that cannot open a database, so it never produces a unique-key error.
func sqliteUniqueViolation(error) bool { return false }
