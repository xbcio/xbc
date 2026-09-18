module github.com/xbcio/xbc/extensions/coordination/placement

go 1.25.0

// github.com/xbcio/xbc is deliberately absent from require while the core
// module has no published tag. Repository development resolves it through
// go.work; adding a local replace or a placeholder version here would make this
// independently published module unusable downstream.
//
// The production closure is core plus the protocol-neutral lease and health
// contracts. Everything below is test-only: the tests drive a real Redis
// backend and a real application so the placement semantics are exercised
// through the same seams a deployment uses.
require (
	github.com/alicebob/miniredis/v2 v2.38.0 // test only
	github.com/redis/go-redis/v9 v9.21.0     // test only
	github.com/stretchr/testify v1.12.1      // test only
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)
