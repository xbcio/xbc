module github.com/xbcio/xbc/extensions/coordination/placement

go 1.25.0

// github.com/xbcio/xbc is deliberately absent from require while the core
// module has no published tag. Repository development resolves it through
// go.work; adding a local replace or a placeholder version here would make this
// independently published module unusable downstream.
//
// The production closure is core plus the protocol-neutral lease, health, and
// metrics contracts, and the Prometheus client library the optional metrics
// collector is built on. Everything else is test-only: the tests drive a real
// Redis backend and a real application so the placement semantics are
// exercised through the same seams a deployment uses.
require (
	github.com/prometheus/client_golang v1.23.2
)

require (
	github.com/alicebob/miniredis/v2 v2.38.0 // test only
	github.com/redis/go-redis/v9 v9.21.0     // test only
	github.com/stretchr/testify v1.12.1      // test only
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
