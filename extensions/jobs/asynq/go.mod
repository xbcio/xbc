module github.com/xbcio/xbc/extensions/jobs/asynq

go 1.25.0

// The core module is deliberately absent until it has a real release tag.
// Repository builds resolve XBC imports through a workspace; this publishable
// manifest intentionally has no filesystem replace or placeholder version.
require (
	github.com/alicebob/miniredis/v2 v2.38.0 // test only
	github.com/hibiken/asynq v0.26.0
	github.com/redis/go-redis/v9 v9.21.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
