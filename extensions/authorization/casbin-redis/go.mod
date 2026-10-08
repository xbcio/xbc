module github.com/xbcio/xbc/extensions/authorization/casbin-redis

go 1.25.0

// XBC core and the Casbin integration SPI are resolved through the repository
// workspace during development. Published consumers must use real tagged XBC
// versions; this module intentionally has no local replace or placeholder XBC
// requirement.
require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/casbin/casbin/v2 v2.135.0
	github.com/casbin/redis-watcher/v2 v2.5.0
	github.com/redis/go-redis/v9 v9.21.0
)

require (
	github.com/bmatcuk/doublestar/v4 v4.6.1 // indirect
	github.com/casbin/govaluate v1.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
)
