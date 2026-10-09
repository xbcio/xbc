package runtime

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

type taskRuntime struct {
	mu            sync.Mutex
	admitting     map[plugin.Identity]bool
	groups        map[plugin.Identity]*pluginTasks
	criticalTasks int
	closed        bool
	shuttingDown  atomic.Bool
	logger        log.Logger
	onCritical    func(string)

	// budget bounds how many units of work each workload may run at once. Its
	// zero value bounds nothing, which is what a process whose workloads
	// declare no budget keeps.
	budget workloadBudget

	// hosted reports whether this process carries a workload: the plan's hosted
	// set. It answers the admission question attribution cannot -- "may a
	// shared Plugin charge this workload's quota" -- because a plugin acting
	// for a workload this process does not carry would be taking a slot no work
	// of that workload here will ever give back. Nil hosts nothing.
	hosted func(plugin.WorkloadKey) bool

	// attribute reports which workload owns a submitting Plugin: the plan's
	// identity-to-workload table. Nil attributes nothing.
	//
	// It is not the budget's own field because it is not a budget concept. Two
	// features read it -- the per-workload task budget and the profiler label
	// every managed task runs under -- and they ask different questions of the
	// same answer: a workload that declares no max_goroutines is attributed and
	// charged nothing, so the budget's verdict cannot stand in for attribution.
	attribute func(plugin.Identity) (plugin.WorkloadKey, bool)
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
	// The budget is charged inside the admission window and before any of the
	// bookkeeping below, so a refused submission leaves no task group, no
	// waiter, and no critical count behind it.
	//
	// Charging after the admission check rather than before it is what keeps a
	// budget refusal from being reported as a closed window: the two are
	// different conditions with different remedies -- one is a Plugin
	// submitting outside its Start hook, the other is a workload that has used
	// up the concurrency it was configured with -- and the two warn lines are
	// where they stay distinguishable, because Go's bool return carries no
	// reason and Context.Go has no error to return.
	workload := runtime.workloadOf(identity)
	charged, refusal := runtime.budget.charge(workload)
	if refusal != nil {
		runtime.mu.Unlock()
		runtime.log().Warn("xbc: managed task rejected over its workload goroutine budget",
			"plugin", identity.String(),
			"workload", refusal.workload.String(),
			"limit", refusal.limit,
			"running", refusal.running,
			"rejected", refusal.rejected,
			"critical", critical)
		return false
	}
	group := runtime.groups[identity]
	if group == nil {
		ctx, cancel := context.WithCancel(context.Background())
		group = &pluginTasks{ctx: ctx, cancel: cancel}
		runtime.groups[identity] = group
	}
	group.wg.Add(1)
	if critical {
		runtime.criticalTasks++
	}
	runtime.mu.Unlock()

	go runtime.runTask(identity, group, fn, critical, charged, workload)
	return true
}

// runTask runs one managed task.
//
// charged is the workload whose budget slot this submission took, empty when it
// took none; workload is the workload the submitting Plugin belongs to, empty
// when it belongs to none. They differ: a workload that declares no
// max_goroutines is attributed but charges nothing.
func (runtime *taskRuntime) runTask(
	identity plugin.Identity,
	group *pluginTasks,
	fn func(context.Context),
	critical bool,
	charged plugin.WorkloadKey,
	workload plugin.WorkloadKey,
) {
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
	// The slot this submission charged is given back as soon as the task body
	// has returned, panicked or not, and without waiting for the scope to be
	// stopped: the budget bounds how many tasks of a workload are running, so a
	// slot has to be released by the task that held it. Deriving the count from
	// this group's waiter instead would be wrong twice over -- a group belongs
	// to one Plugin and is dropped when that Plugin stops, while a workload
	// spans Plugins and outlives them, and the waiter counts tasks that have
	// not been released rather than slots that are held.
	//
	// Declared last so that it runs first, ahead of the judgment above and of
	// the waiter release. Releasing before the judgment keeps the budget out of
	// the one branch on this path that calls back into the runtime, and it
	// releases under the budget's own lock only, never taskRuntime.mu, so no
	// lock this goroutine holds can be re-entered by that call.
	defer runtime.budget.release(charged)
	runWithWorkloadLabel(group.ctx, workload, fn)
}

// runWithWorkloadLabel runs fn under a profiler label naming the workload the
// submitting Plugin belongs to, so a CPU profile can be read per workload.
//
// The label is the only way to get that answer. A stack already says which
// Plugin is burning the CPU, because the frames are that Plugin's own code, but
// workload membership is a composition-time fact and appears in no frame: two
// processes running the same binary attribute the same function to different
// workloads depending on which slots they won.
//
// A Plugin that belongs to no workload is left unlabelled rather than labelled
// as unowned, and that is a correctness matter rather than a preference. Labels
// are inherited by every goroutine started under them, and the plugins that
// belong to no workload are the shared ones -- the Web server's accept loop
// above all -- whose children go on to serve every workload's routes. Labelling
// that loop would file each of those requests under a name that denies the
// workload actually being served. Untagged therefore means "not attributable to
// one workload", which is the truth about shared infrastructure, and it is also
// why the labels cover managed background work rather than request handling.
func runWithWorkloadLabel(ctx context.Context, workload plugin.WorkloadKey, fn func(context.Context)) {
	if workload == "" {
		fn(ctx)
		return
	}
	pprof.Do(ctx, pprof.Labels(workloadProfileLabel, workload.String()), fn)
}

// workloadProfileLabel is the profiler label key the runtime files managed tasks
// under. It matches the configuration root the same workloads are declared in,
// so one word means one thing across a profile, a doctor report and a YAML file.
const workloadProfileLabel = "workload"

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

// criticalTaskCount reports how many critical managed tasks were admitted, not
// how many are still running. Counting cumulatively is safe for the liveness
// judgement it feeds: a critical task that has already returned has requested
// shutdown on its way out, so startup aborts before the count is consulted.
func (runtime *taskRuntime) criticalTaskCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.criticalTasks
}

func (runtime *taskRuntime) log() log.Logger {
	if runtime.logger == nil {
		return log.L()
	}
	return runtime.logger
}

// configureWorkloadBudget installs the per-workload task budgets together with
// the attribution both the budgets and the managed tasks' profiler labels read.
//
// The three halves arrive separately because they become knowable at different
// moments. The limits are configuration, so bootstrap reads them straight out
// of the workloads root, before anything has been planned. The attribution and
// the hosted set are properties of the frozen graph -- a Definition claims
// membership either through WorkloadOf or through Options[W].Workload, and only
// a finished plan has proven the two agree, and only a finished plan knows
// which workloads were carried -- so both are supplied as closures over that
// plan and resolved on demand.
//
// A process that declares no budget still installs attribution, because the
// labels do not depend on a limit: "which workload is this task" and "may this
// workload run another task" are separate questions.
//
// All three are written here once and only read afterwards, which is why none
// is synchronized: this runs before any Plugin code can execute, and every read
// happens after, either on the goroutine that wrote them or on a goroutine
// created from it.
func (runtime *taskRuntime) configureWorkloadBudget(
	limits map[plugin.WorkloadKey]int,
	attribute func(plugin.Identity) (plugin.WorkloadKey, bool),
	hosted func(plugin.WorkloadKey) bool,
) {
	runtime.attribute = attribute
	runtime.hosted = hosted
	runtime.budget.limits = limits
	runtime.budget.bounded = len(limits) > 0
	runtime.budget.running = make(map[plugin.WorkloadKey]int, len(limits))
	runtime.budget.rejected = make(map[plugin.WorkloadKey]uint64, len(limits))
}

// workloadOf reports the workload a submitting Plugin belongs to, or the empty
// key when it belongs to none. A valid WorkloadKey is never empty -- the
// validator rejects it -- so the empty key is an unambiguous "no workload" and
// needs no second return value to say so.
func (runtime *taskRuntime) workloadOf(identity plugin.Identity) plugin.WorkloadKey {
	if runtime.attribute == nil {
		return ""
	}
	workload, attributed := runtime.attribute(identity)
	if !attributed {
		return ""
	}
	return workload
}

// admissionFor reports the admission limiter a Plugin runs its unmanaged work
// against, or nil when nothing bounds that work.
//
// workload is the workload the work is being run for, empty when the asker
// means "the submitting Plugin's own". The two are one code path because the
// decision is the same in both cases: the limiter exists only for a workload
// this process carries and bounds, which is exactly when there is a quota to
// charge. A plugin that belongs to no workload, a workload this process does
// not host, and a workload that declares no max_goroutines all answer nil, and
// Context reports the nil answer as an unbounded limiter rather than an error:
// none of them is a mistake the caller can fix, and a shared plugin asking
// about a workload that is not here is the ordinary case of a process that
// hosts fewer roles than the application declares.
//
// The limit is read without the budget mutex under the same rule as bounded:
// configureWorkloadBudget wrote the map once, before any Plugin code could run,
// and nothing writes it again.
func (runtime *taskRuntime) admissionFor(identity plugin.Identity, workload plugin.WorkloadKey) plugin.Admission {
	if runtime == nil || !runtime.budget.bounded {
		return nil
	}
	if workload == "" {
		workload = runtime.workloadOf(identity)
		if workload == "" {
			return nil
		}
	}
	if runtime.hosted != nil && !runtime.hosted(workload) {
		return nil
	}
	if limit, limited := runtime.budget.limits[workload]; !limited || limit <= 0 {
		return nil
	}
	return &workloadAdmission{budget: &runtime.budget, workload: workload}
}

// workloadBudget bounds how many units of work one workload may run at any one
// time and counts the submissions it refused.
//
// A unit of work is a managed task or a unit an admission Acquire took; both
// draw on the same count, because the question max_goroutines answers is how
// much of one workload is in flight at once, not how it was started. See
// plugin.Admission for why the two paths share one number.
//
// The bound is on concurrency, never on the cumulative number of submissions. A
// slot is taken when a task is admitted and given back when that task returns,
// so a workload that runs ten thousand short-lived tasks one after another is
// refused nothing, while one that keeps ten alive at once is exactly what a
// limit of ten refuses. Charging without releasing would quietly turn the limit
// into a lifetime submission cap, at which point a long-running process would
// eventually stop being able to submit anything at all -- a far worse outcome
// than the unbounded behaviour it was meant to improve on.
//
// It owns its own mutex rather than sharing taskRuntime.mu. The release half
// runs on a managed task's exit path, one branch of which calls back into the
// runtime (a critical failure escalates through onCritical into requestStop and
// closeAdmission, both of which take taskRuntime.mu), so keeping the budget on
// a separate lock keeps that path free of any lock the callback could re-enter.
// It also makes the lock order one-directional and easy to state: submit takes
// taskRuntime.mu and then budget.mu, and nothing anywhere takes them the other
// way round.
type workloadBudget struct {
	// mu guards everything below.
	mu sync.Mutex
	// bounded is len(limits) > 0, cached so that a process whose workloads
	// declare no budget pays one boolean test per submission instead of a mutex
	// acquisition. It is what makes the unconfigured case indistinguishable
	// from the behaviour that preceded this budget.
	bounded bool
	// limits holds the workloads that asked for a bound. A workload absent from
	// it is unbounded, which is what it gets by declaring no max_goroutines.
	limits map[plugin.WorkloadKey]int
	// running counts the units of work currently alive per bounded workload:
	// managed tasks, and every unit an admission Acquire took.
	running map[plugin.WorkloadKey]int
	// rejected counts, per bounded workload, how many submissions it has
	// refused for exceeding its limit. It is cumulative rather than a gauge
	// because the question it answers is "is this workload being held back",
	// which a snapshot of one moment cannot answer.
	rejected map[plugin.WorkloadKey]uint64
	// freed is the channel an admission waits on, closed and dropped by the
	// next release. It is one channel for the whole budget rather than one per
	// workload because a release cannot tell which workload a waiter wants:
	// every waiter re-checks under mu anyway, and the waiters are worker
	// goroutines counted in tens, so a shared wake-up costs a re-check per
	// waiter and saves a map of channels. It is created lazily and left nil
	// while nobody waits, so an uncontended release allocates nothing.
	freed chan struct{}
	// waiting counts the goroutines blocked in acquire. It exists so release
	// can skip the broadcast when nobody is listening, which is the common case
	// once a workload is at its ceiling for a while: without it every release
	// would close and replace a channel nobody reads.
	waiting int
}

// budgetRefusal is one submission the budget refused. It carries the numbers an
// operator needs to tell a workload that is genuinely over its budget from one
// whose limit is simply too small for the job, which is why it reports the
// running count and not only the limit.
type budgetRefusal struct {
	workload plugin.WorkloadKey
	limit    int
	running  int
	rejected uint64
}

// charge accounts one submission against its workload's budget.
//
// workload is the workload the submitting Plugin belongs to, empty when it
// belongs to none. It returns the workload whose slot was taken -- empty when
// nothing was charged -- or a refusal when the submission must not be admitted.
// A Plugin that belongs to no workload is charged nothing: it has no budget to
// exceed, and there is deliberately no process-wide budget, because one would
// ration the unowned plugins that a standby process exists to run and would
// couple unrelated workloads to each other through a single shared ceiling.
// The per-workload budget here is the whole of the admission quota, and it is
// what Context.Admission hands to a queue worker or a pooled executor as well
// as what this method charges managed tasks against: one workload, one number,
// rather than a second process-wide allowance beside it.
func (budget *workloadBudget) charge(workload plugin.WorkloadKey) (plugin.WorkloadKey, *budgetRefusal) {
	if !budget.bounded || workload == "" {
		return "", nil
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	limit, limited := budget.limits[workload]
	if !limited {
		return "", nil
	}
	running := budget.running[workload]
	if running >= limit {
		budget.rejected[workload]++
		return "", &budgetRefusal{
			workload: workload,
			limit:    limit,
			running:  running,
			rejected: budget.rejected[workload],
		}
	}
	budget.running[workload] = running + 1
	return workload, nil
}

// acquire is charge's blocking half, and the admission path into the same
// budget. It waits while the workload is at its limit and reports the context's
// error if the wait ends first.
//
// It waits rather than refuses because its callers have no client to hand a
// refusal to: a queue worker that cannot be admitted would have to return an
// error for a task it already dequeued, which burns a retry attempt and
// eventually archives the work, while the honest answer to "the workload is
// busy" is to wait until it is not. The context is what bounds the wait, and
// its caller chooses it: a task's own context for a queue handler, the pool's
// stop context for an executor task, so the wait never outlives the work.
//
// No fairness is promised. Every waiter re-checks under the mutex when a slot
// is given back, so a long wait is possible under sustained contention and a
// slot is never lost -- only the order in which waiters are served is
// unspecified.
func (budget *workloadBudget) acquire(ctx context.Context, workload plugin.WorkloadKey) (plugin.WorkloadKey, error) {
	if !budget.bounded || workload == "" {
		return "", nil
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	limit, limited := budget.limits[workload]
	if !limited {
		return "", nil
	}
	for budget.running[workload] >= limit {
		freed := budget.freed
		if freed == nil {
			freed = make(chan struct{})
			budget.freed = freed
		}
		budget.waiting++
		budget.mu.Unlock()
		select {
		case <-ctx.Done():
			budget.mu.Lock()
			budget.waiting--
			return "", ctx.Err()
		case <-freed:
		}
		budget.mu.Lock()
		budget.waiting--
		// The unlock above is what lets a release run at all, so the loop has
		// to re-check the limit rather than assume the wake-up was for it: the
		// release may have freed a slot another waiter took first.
	}
	budget.running[workload]++
	return workload, nil
}

// release gives back the slot charge or acquire took. The empty key is what
// both answer for a submission they charged nothing, so releasing it is a
// no-op.
func (budget *workloadBudget) release(workload plugin.WorkloadKey) {
	if workload == "" {
		return
	}
	budget.mu.Lock()
	if running := budget.running[workload]; running > 1 {
		budget.running[workload] = running - 1
	} else {
		delete(budget.running, workload)
	}
	if budget.waiting > 0 && budget.freed != nil {
		close(budget.freed)
		budget.freed = nil
	}
	budget.mu.Unlock()
}

// workloadAdmission is the plugin.Admission view of one workload's budget.
type workloadAdmission struct {
	budget   *workloadBudget
	workload plugin.WorkloadKey
}

// Acquire implements plugin.Admission.
func (admission *workloadAdmission) Acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	charged, err := admission.budget.acquire(ctx, admission.workload)
	if err != nil {
		return nil, err
	}
	return sync.OnceFunc(func() { admission.budget.release(charged) }), nil
}

// workloadBudgetReport is one bounded workload's budget state, for diagnostics.
type workloadBudgetReport struct {
	Workload plugin.WorkloadKey
	Limit    int
	// Running is how many units of the workload's work are alive right now:
	// its managed tasks plus every unit an admission took.
	Running int
	// Rejected is how many submissions this workload has refused so far.
	Rejected uint64
}

// workloadBudgets returns the budget state of every workload that declares one,
// sorted by key.
//
// A workload without a budget is absent rather than reported with a zero limit:
// "unbounded" and "bounded at zero" are different answers, only one of them is
// configurable, and a diagnostic that printed 0 for the common case would make
// the rare case unreadable.
func (runtime *taskRuntime) workloadBudgets() []workloadBudgetReport {
	if runtime == nil || !runtime.budget.bounded {
		return nil
	}
	runtime.budget.mu.Lock()
	defer runtime.budget.mu.Unlock()
	reports := make([]workloadBudgetReport, 0, len(runtime.budget.limits))
	for workload, limit := range runtime.budget.limits {
		reports = append(reports, workloadBudgetReport{
			Workload: workload,
			Limit:    limit,
			Running:  runtime.budget.running[workload],
			Rejected: runtime.budget.rejected[workload],
		})
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Workload < reports[j].Workload })
	return reports
}

// workloadTaskLimits reads the per-workload work budgets out of the workloads
// root: the count each workload's managed tasks and admitted units share.
//
// It runs at bootstrap, which is before a plan exists, so it reads every
// declared workload rather than this process's hosted set. That is not a
// shortcut: a workload's max_goroutines is configuration, and a workload this
// process does not carry contributes no Definition to the graph, so nothing can
// ever submit against it and its limit can never be consulted. Reading the
// declared set here also keeps the answer independent of the placement source,
// which bootstrap has not consulted yet and which is application code.
func workloadTaskLimits(bundles []plugin.Bundle, env *config.Environment) (map[plugin.WorkloadKey]int, error) {
	sections, err := assembly.ReadWorkloadSections(bundles, env)
	if err != nil {
		return nil, err
	}
	limits := make(map[plugin.WorkloadKey]int, len(sections))
	for _, section := range sections {
		if section.Config.MaxGoroutines > 0 {
			limits[section.Workload.Key] = section.Config.MaxGoroutines
		}
	}
	return limits, nil
}

// workloadOf attributes one Plugin to the workload that owns it.
//
// The answer comes from the finished plan rather than from the Bundles, because
// the plan is the reconciled record of what was actually built: membership can
// be claimed either by WorkloadOf or by Options[W].Workload, and only the plan
// has already proven the two agree. Attribution therefore matches the graph
// that exists, not the graph the composition root intended.
//
// It is consulted only from the charge path, which is reachable only while a
// Plugin's Start hook is executing, and planning assigns the plan on that same
// goroutine before the first Start hook runs. Every caller therefore observes a
// written plan; a nil one -- doctor, or a submission reached before planning --
// answers "no workload" rather than panicking.
func (a *App) workloadOf(identity plugin.Identity) (plugin.WorkloadKey, bool) {
	return a.plan.WorkloadOf(identity)
}

// hostsWorkload reports whether this process carries a workload: whether the
// plan declares it as hosted.
//
// It is the admission half of attribution. Attribution says which workload a
// Plugin belongs to and is answered per Plugin; this says whether a workload is
// here at all and is asked only when a shared Plugin names a workload it acts
// for. Like workloadOf it reads the plan assigned before the first Start hook
// ran and answers "not here" for a nil plan.
func (a *App) hostsWorkload(key plugin.WorkloadKey) bool {
	if key == "" {
		return false
	}
	for _, workload := range a.plan.Workloads() {
		if workload.Workload.Key == key {
			return workload.Hosted
		}
	}
	return false
}
