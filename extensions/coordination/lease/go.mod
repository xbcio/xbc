module github.com/xbcio/xbc/extensions/coordination/lease

go 1.25.0

// This module deliberately requires nothing. The lease contract is a
// protocol-neutral vocabulary of two interfaces built only from the standard
// library, so a plugin, a transport, or an application composition root can
// depend on it without inheriting a dependency closure.
