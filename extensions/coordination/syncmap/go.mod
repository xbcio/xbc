module github.com/xbcio/xbc/extensions/coordination/syncmap

go 1.26.0

// This module deliberately requires nothing. The syncmap contract publishes a
// vocabulary of interfaces, one option struct, and sentinel errors built only
// from the standard library, so any plugin, transport, or application
// composition root can depend on it without inheriting a dependency closure.
