module github.com/xbcio/xbc/extensions/jobs/cron

go 1.25.0

// github.com/xbcio/xbc and the protocol-neutral lease contract are deliberately
// absent until they have real release tags. Repository development resolves them
// through a workspace; adding a local replace or a synthetic version would make
// this independently published module unusable downstream.
//
// This module depends on no sibling plugin module. The Redis-backed lease.Locker
// behind distributed.redis_instance and distributed.redis.addr is implemented
// here, over the go-redis client this module already requires, rather than
// borrowed from the Redis extension (see redis_locker.go).
require (
	github.com/alicebob/miniredis/v2 v2.38.0 // test only
	github.com/redis/go-redis/v9 v9.21.0
	github.com/robfig/cron/v3 v3.0.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)
