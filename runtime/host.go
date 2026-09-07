package runtime

import (
	"context"
	"strings"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// hostAdapter is the narrow reverse port behind one lifecycle Context. It
// deliberately exposes no value registry, service locator, or Plugin scan.
type hostAdapter struct {
	app    *App
	logger log.Logger
}

func (host hostAdapter) ExecutionContext() context.Context {
	host.app.stateMu.Lock()
	defer host.app.stateMu.Unlock()
	if host.app.executionCtx == nil {
		return context.Background()
	}
	return host.app.executionCtx
}

func (host hostAdapter) Logger() log.Logger {
	if host.logger != nil {
		return host.logger
	}
	return host.app.log()
}

func (host hostAdapter) TrafficGate() <-chan struct{} { return host.app.trafficGate }

func (host hostAdapter) SubmitTask(identity plugin.Identity, fn func(context.Context), critical bool) bool {
	if host.app.tasks == nil {
		return false
	}
	return host.app.tasks.submit(identity.Normalized(), fn, critical)
}

func (host hostAdapter) RequestShutdown(identity plugin.Identity, reason string) bool {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		reason = "requested"
	}
	return host.app.requestStop("plugin:" + identity.Normalized().String() + ":" + reason)
}
