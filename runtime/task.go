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

	// budget bounds how many managed tasks each workload may run at once. Its
	// zero value bounds nothing, which is what a process whose workloads
	// declare no budget keeps.
	budget workloadBudget

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
// The two halves arrive separately because they become knowable at different
// moments. The limits are configuration, so bootstrap reads them straight out
// of the workloads root, before anything has been planned. The attribution is a
// property of the frozen graph -- a Definition claims membership either through
// WorkloadOf or through Options[W].Workload, and only a finished plan has
// proven the two agree -- so it is supplied as a closure over that plan and
// resolved per submission instead.
//
// A process that declares no budget still installs attribution, because the
// labels do not depend on a limit: "which workload is this task" and "may this
// workload run another task" are separate questions.
//
// Both are written here once and only read afterwards, which is why neither is
// synchronized: this runs before any Plugin code can execute, and every read
// happens after, either on the goroutine that wrote them or on a goroutine
// created from it.
func (runtime *taskRuntime) configureWorkloadBudget(limits map[plugin.WorkloadKey]int, attribute func(plugin.Identity) (plugin.WorkloadKey, bool)) {
	runtime.attribute = attribute
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

// workloadBudget bounds how many managed tasks one workload may run at any one
// time and counts the submissions it refused.
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
	// mu guards the three maps below.
	mu sync.Mutex
	// bounded is len(limits) > 0, cached so that a process whose workloads
	// declare no budget pays one boolean test per submission instead of a mutex
	// acquisition. It is what makes the unconfigured case indistinguishable
	// from the behaviour that preceded this budget.
	bounded bool
	// limits holds the workloads that asked for a bound. A workload absent from
	// it is unbounded, which is what it gets by declaring no max_goroutines.
	limits map[plugin.WorkloadKey]int
	// running counts the managed tasks currently alive per bounded workload.
	running map[plugin.WorkloadKey]int
	// rejected counts, per bounded workload, how many submissions it has
	// refused for exceeding its limit. It is cumulative rather than a gauge
	// because the question it answers is "is this workload being held back",
	// which a snapshot of one moment cannot answer.
	rejected map[plugin.WorkloadKey]uint64
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

// release gives back the slot charge took. The empty key is the answer charge
// gives for a submission it charged nothing, so releasing it is a no-op.
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
	budget.mu.Unlock()
}

// workloadBudgetReport is one bounded workload's budget state, for diagnostics.
type workloadBudgetReport struct {
	Workload plugin.WorkloadKey
	Limit    int
	// Running is how many of the workload's managed tasks are alive right now.
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

// workloadTaskLimits reads the per-workload managed-task budgets out of the
// workloads root.
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
