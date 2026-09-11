module github.com/xbcio/xbc/extensions/authentication

go 1.25.0

// This module deliberately requires nothing. The authentication contract is a
// protocol-neutral vocabulary of interfaces and value types built only from the
// standard library, so any transport or plugin can depend on it without
// inheriting a dependency closure.
