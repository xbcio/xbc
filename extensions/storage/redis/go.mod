module github.com/xbcio/xbc/extensions/storage/redis

go 1.25.0

// The core module is deliberately absent from require until it has a real
// release tag. Local development resolves it through a Go workspace; no
// unpublished placeholder dependency or filesystem substitution is used.
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
	golang.org/x/sys v0.45.0 // indirect
)
