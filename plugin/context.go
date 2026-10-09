package plugin

import (
	"context"
	"time"

	"github.com/xbcio/xbc/log"
)

// RuntimeHost is the narrow runtime port exposed through Context. It carries
// no value publication, lookup, or initialized-plugin enumeration.
type RuntimeHost interface {
	ExecutionContext() context.Context
	Logger() log.Logger
	ProcessInstance() string
	TrafficGate() <-chan struct{}
	SubmitTask(id Identity, fn func(context.Context), critical bool) bool
	RequestShutdown(id Identity, reason string) bool
	// Admission reports the admission limiter for the workload a submitting
	// Plugin belongs to. It may return nil, which Context reports as an
	// unbounded limiter.
	Admission(id Identity) Admission
	// AdmissionFor reports the admission limiter for a named workload, for a
	// shared Plugin that runs work on behalf of contributors that declare one.
	// It may return nil, which Context reports as an unbounded limiter.
	AdmissionFor(id Identity, workload WorkloadKey) Admission
}

// Context is the lifecycle-operation parameter for one Plugin instance. It
// exposes only cancellation, identity, logging, Start-scoped task admission,
// and shutdown request; configuration and dependency wiring belong elsewhere.
type Context struct {
	host RuntimeHost
	id   Identity
}

var _ context.Context = (*Context)(nil)

// NewRuntimeContext is framework assembly API. Ordinary Plugins receive a
// Context from lifecycle methods and never construct one: the host passed here
// owns the execution context the Context reports, so a plugin building its own
// would run lifecycle work against a host the runtime does not own. An
// architecture guard keeps the runtime the only production caller.
func NewRuntimeContext(host RuntimeHost, id Identity) *Context {
	return &Context{host: host, id: id.Normalized()}
}

func (c *Context) executionContext() context.Context {
	if c == nil || c.host == nil {
		return context.Background()
	}
	ctx := c.host.ExecutionContext()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (c *Context) Deadline() (time.Time, bool) { return c.executionContext().Deadline() }
func (c *Context) Done() <-chan struct{}       { return c.executionContext().Done() }
func (c *Context) Err() error                  { return c.executionContext().Err() }
func (c *Context) Value(key any) any           { return c.executionContext().Value(key) }

func (c *Context) Name() string       { return c.id.Plugin.String() }
func (c *Context) Key() Key           { return c.id.Plugin }
func (c *Context) Instance() string   { return c.id.Instance }
func (c *Context) Identity() Identity { return c.id }

// ProcessInstance returns the identity of the process this Plugin runs in: the
// configured instance id when the deployment gave one, and the runtime's
// derived default otherwise. Every Plugin in one process reports the same
// string, and two replicas of one deployment report different ones.
//
// That is the opposite axis from Instance, which names this Plugin among the
// instances of its own Definition and reads the same in every replica. Code
// writing to a store shared by the replicas -- a lease claimant, a claim row, a
// reported holder -- wants this string; a log field explaining which
// configuration section acted wants Instance.
//
// Treat the value as opaque. It is a token to compare and to publish, not a
// structure to parse: the derived default's spelling is the runtime's business
// and may change. It is empty only when there is no runtime behind the Context,
// which a plugin under a real application never sees.
func (c *Context) ProcessInstance() string {
	if c == nil || c.host == nil {
		return ""
	}
	return c.host.ProcessInstance()
}

func (c *Context) Log() log.Logger {
	if c == nil || c.host == nil || c.host.Logger() == nil {
		return log.L()
	}
	return c.host.Logger()
}

// TrafficGate remains closed throughout fallible traffic preparation and is
// closed exactly once by the runtime after every participant succeeds. A
// Start-owned serving task waits on this channel before exposing ingress.
func (c *Context) TrafficGate() <-chan struct{} {
	if c == nil || c.host == nil {
		return nil
	}
	return c.host.TrafficGate()
}

// Admission reports the admission limiter for this Plugin's workload. Work a
// Plugin runs outside Context.Go -- a queue worker's handler invocation, a task
// handed to a pool executor -- takes one unit of that quota for as long as it
// runs, so max_goroutines bounds the workload's concurrent work however it was
// started. See Admission for what the limiter guarantees.
//
// A Plugin that belongs to no workload gets a limiter that admits immediately:
// unowned plugins are the ones a standby process exists to run, and rationing
// them would defeat that. The call is nil-safe and may be used before Start.
func (c *Context) Admission() Admission {
	if c == nil || c.host == nil {
		return noAdmission{}
	}
	if admission := c.host.Admission(c.id); admission != nil {
		return admission
	}
	return noAdmission{}
}

// AdmissionFor reports the admission limiter for a named workload, for a shared
// Plugin that runs work on behalf of the workload that declared it. Its
// attribution source is Entry.Workload: a plugin that collects workload-tagged
// producers -- a queue worker collecting handlers, a scheduler collecting jobs
// -- knows which workload each unit of work belongs to, and charges that
// workload's quota for it rather than its own (a shared plugin usually belongs
// to no workload at all).
//
// The workload must be one this process carries; a workload it does not host
// has no limiter, because there is no work of that workload here to bound. The
// empty key is never a valid workload and is treated the same way. Like
// Admission, the result is never nil-valued and the call is nil-safe.
func (c *Context) AdmissionFor(workload WorkloadKey) Admission {
	if c == nil || c.host == nil || workload == "" {
		return noAdmission{}
	}
	if admission := c.host.AdmissionFor(c.id, workload); admission != nil {
		return admission
	}
	return noAdmission{}
}

// Go submits a non-critical managed task. Submission is accepted only while
// this Plugin's Start hook is executing and only while its workload has a unit
// of its budget to spare; false means one of the two said no, and the warning
// the runtime logs names which. A non-critical task may return whenever its
// work is done, so it does not count as the long-lived capability the runtime
// requires of a startable application; use GoCritical for work that must last
// the process lifetime.
//
// A submitted task runs under a "workload" profiler label when this Plugin
// belongs to one, so a CPU profile can be read per workload. Plugins that belong
// to no workload are left unlabelled, because a label is inherited by every
// goroutine started under it and shared infrastructure serves every workload.
func (c *Context) Go(fn func(context.Context)) bool {
	return c != nil && c.host != nil && c.host.SubmitTask(c.id, fn, false)
}

// GoCritical submits a task whose panic or unprompted return requests
// application shutdown. It is accepted and refused on the same terms as Go.
// Because such a task is expected to run for the process lifetime, it
// satisfies the runtime's long-lived capability requirement.
func (c *Context) GoCritical(fn func(context.Context)) bool {
	return c != nil && c.host != nil && c.host.SubmitTask(c.id, fn, true)
}

func (c *Context) RequestShutdown(reason string) bool {
	return c != nil && c.host != nil && c.host.RequestShutdown(c.id, reason)
}
