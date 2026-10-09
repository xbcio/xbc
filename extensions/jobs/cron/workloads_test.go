package cron

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
)

// TestFactoryAcceptsAProcessThatContributesNoJobs covers the standby shape: a
// process whose Plugins contribute no job constructs, starts, and provides the
// long-lived task the runtime requires, without scheduling anything.
func TestFactoryAcceptsAProcessThatContributesNoJobs(t *testing.T) {
	config := defaultConfig()
	prepared, err := prepareConfig(config)
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	p, err := newConfiguredPlugin(prepared, nil, nil, nil)
	if err != nil {
		t.Fatalf("newConfiguredPlugin() with no jobs error = %v", err)
	}
	if len(p.jobs) != 0 {
		t.Fatalf("jobs = %d, want none", len(p.jobs))
	}

	host := newTestHost()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	submitted, critical := host.counts()
	if submitted != 1 || critical != 1 {
		t.Fatalf("managed tasks = %d critical = %d, want one critical task that waits for the stop", submitted, critical)
	}
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}

	// stop must not deadlock on the task it exists to end.
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.stop(stopCtx); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	host.close()
}

// TestInvocationChargesTheWorkloadItsContributorBelongsTo pins the attribution
// down: the scheduler submits every runner with its own identity, so without an
// explicit charge a workload's budget would never see the work done on its
// behalf. The job of an unowned contributor charges nothing.
func TestInvocationChargesTheWorkloadItsContributorBelongsTo(t *testing.T) {
	quota := &recordingAdmission{}
	host := newTestHost()
	host.admissionForWorkload = func(plugin.WorkloadKey) plugin.Admission { return quota }

	held := make(chan int, 2)
	job := &funcJob{name: "scan", spec: "@hourly", run: func(context.Context) error {
		held <- quota.balance()
		return nil
	}}
	prepared, err := prepareConfig(defaultRunImmediatelyConfig())
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	p, err := newConfiguredPlugin(prepared, []plugin.Entry[JobContributor]{workloadEntry("sast", job)}, nil, nil)
	if err != nil {
		t.Fatalf("newConfiguredPlugin() error = %v", err)
	}
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	host.openTraffic()
	defer stopTestPlugin(t, p, host)

	select {
	case balance := <-held:
		if balance != 1 {
			t.Fatalf("quota balance while the job ran = %d, want the unit taken", balance)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the job never ran")
	}
	awaitCondition(t, 2*time.Second, func() bool { return quota.balance() == 0 }, "the unit to be given back")

	want := []admissionQuery{{identity: plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}, workload: "sast", shared: true}}
	if got := host.queries(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("admission queries = %+v, want %+v", got, want)
	}
}

// TestInvocationWaitsForItsWorkloadsQuotaInsteadOfSkipping keeps the charge
// blocking rather than refusing: a job whose workload is at its limit waits for
// a unit, and the wait ends with the shutdown that abandons it -- it never runs
// unbounded.
func TestInvocationWaitsForItsWorkloadsQuotaInsteadOfSkipping(t *testing.T) {
	quota := newGatedAdmission(0)
	host := newTestHost()
	host.admissionForWorkload = func(plugin.WorkloadKey) plugin.Admission { return quota }

	ran := make(chan struct{}, 1)
	job := &funcJob{name: "scan", spec: "@hourly", run: func(context.Context) error {
		ran <- struct{}{}
		return nil
	}}
	prepared, err := prepareConfig(defaultRunImmediatelyConfig())
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	p, err := newConfiguredPlugin(prepared, []plugin.Entry[JobContributor]{workloadEntry("sast", job)}, nil, nil)
	if err != nil {
		t.Fatalf("newConfiguredPlugin() error = %v", err)
	}
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	host.openTraffic()

	awaitCondition(t, 2*time.Second, func() bool { return quota.waiting() == 1 }, "the invocation to park on the quota")
	select {
	case <-ran:
		t.Fatal("the job ran while its workload's quota was fully taken")
	case <-time.After(50 * time.Millisecond):
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.stop(stopCtx); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	host.close()
	select {
	case <-ran:
		t.Fatal("the job ran after stop")
	default:
	}
}

// TestUnownedJobChargesNoQuota is the other half of the attribution: a
// contributor that belongs to no workload is not asked about, and its
// invocation still runs.
func TestUnownedJobChargesNoQuota(t *testing.T) {
	ran := make(chan struct{}, 1)
	job := &funcJob{name: "housekeeping", spec: "@hourly", run: func(context.Context) error {
		ran <- struct{}{}
		return nil
	}}
	p := newTestPlugin(t, func(config *Config) { config.RunImmediately = true }, nil, job)
	host := newTestHost()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	host.openTraffic()
	defer stopTestPlugin(t, p, host)

	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("the unowned job never ran")
	}
	for _, query := range host.queries() {
		if query.shared {
			t.Fatalf("an unowned job asked for a workload's quota: %+v", query)
		}
	}
}

func defaultRunImmediatelyConfig() Config {
	config := defaultConfig()
	config.RunImmediately = true
	return config
}

// recordingAdmission counts what an invocation charged, in order, so a test can
// tell the unit was held for the whole job rather than taken and returned early.
type recordingAdmission struct {
	mu    sync.Mutex
	order []string
	held  int
}

func (a *recordingAdmission) Acquire(context.Context) (func(), error) {
	a.mu.Lock()
	a.order = append(a.order, "acquire")
	a.held++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.held--
		a.order = append(a.order, "release")
		a.mu.Unlock()
	}, nil
}

func (a *recordingAdmission) balance() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held
}

// gatedAdmission is a quota of a fixed size whose units come back only when the
// holder releases them. size 0 grants nothing.
type gatedAdmission struct {
	mu      sync.Mutex
	size    int
	held    int
	parked  int
	changed chan struct{}
}

func newGatedAdmission(size int) *gatedAdmission {
	return &gatedAdmission{size: size, changed: make(chan struct{})}
}

func (a *gatedAdmission) Acquire(ctx context.Context) (func(), error) {
	for {
		a.mu.Lock()
		if a.held < a.size {
			a.held++
			a.mu.Unlock()
			return func() {
				a.mu.Lock()
				a.held--
				close(a.changed)
				a.changed = make(chan struct{})
				a.mu.Unlock()
			}, nil
		}
		a.parked++
		changed := a.changed
		a.mu.Unlock()
		select {
		case <-changed:
			a.mu.Lock()
			a.parked--
			a.mu.Unlock()
		case <-ctx.Done():
			a.mu.Lock()
			a.parked--
			a.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

func (a *gatedAdmission) waiting() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.parked
}
