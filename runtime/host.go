package runtime

import (
	"context"
	"strings"
	"time"

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

// ProcessInstance answers with the same string the runtime logs at startup and
// hands to placement as the requesting instance, so a slot's recorded holder and
// a lease claimant published by any plugin name one process identically. It
// needs no lock: settings are bound once, before any Plugin is constructed.
func (host hostAdapter) ProcessInstance() string { return host.app.settings.Instance() }

// Service answers with the process-wide identity the runtime stamped onto its
// own output -- the startup report, the JSON log fields -- so a plugin that
// publishes a telemetry resource or a build-info metric names this process
// exactly as the runtime does. Like ProcessInstance it reads bound settings
// without a lock: they are settled once, before any Plugin is constructed.
func (host hostAdapter) Service() plugin.ServiceIdentity { return host.app.settings.ServiceIdentity() }

// IngressStopBudget answers with the window the shutdown walk's phase A runs
// under. It reads the same bound settings shutdown.go derives that phase's
// deadline from, so a transport validating its pre_drain_delay against this
// number is validating it against the budget the deadline will actually be
// built from.
func (host hostAdapter) IngressStopBudget() time.Duration {
	return host.app.settings.IngressStopBudget()
}

func (host hostAdapter) TrafficGate() <-chan struct{} { return host.app.trafficGate }

func (host hostAdapter) SubmitTask(identity plugin.Identity, fn func(context.Context), critical bool) bool {
	if host.app.tasks == nil {
		return false
	}
	return host.app.tasks.submit(identity.Normalized(), fn, critical)
}

// Admission and AdmissionFor expose the per-workload admission budget through
// the same host the managed-task path goes through. Like SubmitTask they read
// the task runtime without a lock: bootstrap publishes it under stateMu before
// any Plugin is constructed, and nothing replaces it afterwards. A nil runtime
// answers nil, which Context reports as an unbounded limiter.
func (host hostAdapter) Admission(identity plugin.Identity) plugin.Admission {
	if host.app.tasks == nil {
		return nil
	}
	return host.app.tasks.admissionFor(identity.Normalized(), "")
}

func (host hostAdapter) AdmissionFor(identity plugin.Identity, workload plugin.WorkloadKey) plugin.Admission {
	if host.app.tasks == nil {
		return nil
	}
	return host.app.tasks.admissionFor(identity.Normalized(), workload)
}

func (host hostAdapter) RequestShutdown(identity plugin.Identity, reason string) bool {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		reason = "requested"
	}
	return host.app.requestStop("plugin:" + identity.Normalized().String() + ":" + reason)
}
