package runtime

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type taskRuntime struct {
	mu           sync.Mutex
	admitting    map[plugin.Identity]bool
	groups       map[plugin.Identity]*pluginTasks
	spawned      int
	closed       bool
	shuttingDown atomic.Bool
	logger       log.Logger
	onCritical   func(string)
}

type pluginTasks struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	errs []error
}

func newTaskRuntime(logger log.Logger, onCritical func(string)) *taskRuntime {
	return &taskRuntime{
		admitting:  make(map[plugin.Identity]bool),
		groups:     make(map[plugin.Identity]*pluginTasks),
		logger:     logger,
		onCritical: onCritical,
	}
}

func (runtime *taskRuntime) openStart(identity plugin.Identity) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	identity = identity.Normalized()
	if runtime.closed || runtime.shuttingDown.Load() {
		return false
	}
	runtime.admitting[identity] = true
	return true
}

func (runtime *taskRuntime) closeStart(identity plugin.Identity) {
	runtime.mu.Lock()
	delete(runtime.admitting, identity.Normalized())
	runtime.mu.Unlock()
}

func (runtime *taskRuntime) submit(identity plugin.Identity, fn func(context.Context), critical bool) bool {
	identity = identity.Normalized()
	if fn == nil {
		runtime.log().Warn("xbc: nil managed task rejected", "plugin", identity.String(), "critical", critical)
		return false
	}
	runtime.mu.Lock()
	if runtime.closed || !runtime.admitting[identity] {
		runtime.mu.Unlock()
		runtime.log().Warn("xbc: managed task rejected outside owning Plugin Start", "plugin", identity.String(), "critical", critical)
		return false
	}
	group := runtime.groups[identity]
	if group == nil {
		ctx, cancel := context.WithCancel(context.Background())
		group = &pluginTasks{ctx: ctx, cancel: cancel}
		runtime.groups[identity] = group
	}
	group.wg.Add(1)
	runtime.spawned++
	runtime.mu.Unlock()

	go runtime.runTask(identity, group, fn, critical)
	return true
}

func (runtime *taskRuntime) runTask(identity plugin.Identity, group *pluginTasks, fn func(context.Context), critical bool) {
	defer group.wg.Done()
	var failure error
	defer func() {
		if recovered := recover(); recovered != nil {
			failure = fmt.Errorf("xbc: plugin %s managed task panic: %v\n%s", identity, recovered, debug.Stack())
		}
		if failure != nil {
			group.record(failure)
			if critical && !runtime.shuttingDown.Load() && runtime.onCritical != nil {
				runtime.onCritical(failure.Error())
			} else if !critical {
				runtime.log().Error("xbc: non-critical managed task failed", "plugin", identity.String(), "error", failure)
			}
			return
		}
		if critical && !runtime.shuttingDown.Load() && group.ctx.Err() == nil {
			failure = fmt.Errorf("xbc: plugin %s critical managed task unexpectedly returned", identity)
			group.record(failure)
			if runtime.onCritical != nil {
				runtime.onCritical(failure.Error())
			}
		}
	}()
	fn(group.ctx)
}

func (group *pluginTasks) record(err error) {
	group.mu.Lock()
	group.errs = append(group.errs, err)
	group.mu.Unlock()
}

func (group *pluginTasks) failures() error {
	group.mu.Lock()
	defer group.mu.Unlock()
	return errors.Join(append([]error(nil), group.errs...)...)
}

func (runtime *taskRuntime) closeAdmission() {
	runtime.shuttingDown.Store(true)
	runtime.mu.Lock()
	runtime.closed = true
	clear(runtime.admitting)
	runtime.mu.Unlock()
}

func (runtime *taskRuntime) stopPlugin(identity plugin.Identity, deadline context.Context) error {
	runtime.mu.Lock()
	identity = identity.Normalized()
	group := runtime.groups[identity]
	delete(runtime.groups, identity)
	runtime.mu.Unlock()
	if group == nil {
		return nil
	}
	group.cancel()
	return waitTasks(group, identity.String(), deadline)
}

func (runtime *taskRuntime) drainRemaining(deadline context.Context) error {
	runtime.mu.Lock()
	leftovers := runtime.groups
	runtime.groups = make(map[plugin.Identity]*pluginTasks)
	runtime.mu.Unlock()
	var errs []error
	for identity, group := range leftovers {
		group.cancel()
		if err := waitTasks(group, identity.String(), deadline); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func waitTasks(group *pluginTasks, owner string, deadline context.Context) error {
	done := make(chan struct{})
	go func() {
		group.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return group.failures()
	case <-deadline.Done():
	}
	select {
	case <-done:
		return group.failures()
	default:
		return errors.Join(group.failures(), fmt.Errorf("xbc: plugin %s managed tasks did not exit within the shutdown budget", owner))
	}
}

func (runtime *taskRuntime) spawnedCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.spawned
}

func (runtime *taskRuntime) log() log.Logger {
	if runtime.logger == nil {
		return log.L()
	}
	return runtime.logger
}
