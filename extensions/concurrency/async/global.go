package async

import (
	"context"
	"sync/atomic"

	"github.com/xbcio/xbc/log"
)

// globalPool holds the process-wide bound Pool, installed by the Definition's
// Init and unbound by Stop. The zero value (nil) means no Pool is installed.
var globalPool atomic.Pointer[Pool]

// Spawn is the global entry point for a caller with no explicit dependency on
// a particular Pool, delegating to the process-wide bound Pool. It returns
// ErrNotInstalled when none is bound, and never falls back to a bare
// goroutine: a caller that wants that fallback writes it explicitly, so the
// decision stays visible at the call site rather than hidden in this package.
func Spawn(ctx context.Context, name string, task func(context.Context)) error {
	pool := globalPool.Load()
	if pool == nil {
		return ErrNotInstalled
	}
	return pool.Spawn(ctx, name, task)
}

// bindGlobal installs pool as the process-wide Spawner if none is currently
// bound. If another Pool is already bound (another App or instance), it
// leaves that binding untouched and logs a warning instead of overwriting it.
func bindGlobal(pool *Pool, logger log.Logger) {
	if globalPool.CompareAndSwap(nil, pool) {
		return
	}
	if logger == nil {
		logger = log.L()
	}
	logger.Warn("async: a pool is already installed globally; keeping the existing binding")
}

// unbindGlobal clears the global binding only if pool is still the one
// installed, so a Pool that lost the initial CAS in bindGlobal never unbinds
// a different Pool's installation on its own Stop.
func unbindGlobal(pool *Pool) {
	globalPool.CompareAndSwap(pool, nil)
}
