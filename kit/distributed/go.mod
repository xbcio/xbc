module github.com/xbcio/xbc/kit/distributed

go 1.26.0

// This module is framework-free by construction: production code depends only
// on the standard library and the Redis client, and never on any
// github.com/xbcio/xbc package. See tests/architecture/kit_modules_test.go.
require (
	github.com/alicebob/miniredis/v2 v2.38.0 // test only
	github.com/redis/go-redis/v9 v9.21.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
