module github.com/xbcio/xbc/transport/web/extensions/authorization/casbin-redis

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
