module github.com/xbcio/xbc/extensions/tasks

go 1.25.0

// This module deliberately requires nothing. The task facade is a
// protocol-neutral vocabulary -- task handles, provider bindings, and the
// executor interfaces a local or remote runtime installs -- built only from
// the standard library, so any plugin can define or submit tasks without
// inheriting a dependency closure, and so a composition that selects only the
// local executor never compiles the remote one in.
