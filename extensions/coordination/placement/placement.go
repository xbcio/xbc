package placement

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xbcio/xbc/extensions/coordination/lease"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is this module's stable Definition and configuration identity.
const Key plugin.Key = "placement"

// HealthKey is the stable identity of the readiness probe that reports whether
// this process's slot ownership is still being confirmed.
const HealthKey plugin.Key = "placement-health"

// placementSourceLease labels every decision this package makes, so an operator
// reading doctor output can tell a lease round from a static configuration.
const placementSourceLease = "lease"

const (
	defaultKeyPrefix     = "xbc"
	defaultRenewInterval = 10 * time.Second
	defaultStandbyRetry  = 5 * time.Second
)

// installedPlacement is the placement this process decided with.
//
// It exists because one decision is made in two places that cannot see each
// other. The hosted set is settled before the graph exists -- that is the whole
// reason an unhosted workload contributes no Definition -- so the Locker comes
// from the composition root rather than from a Definition. The slots that
// decision won, however, have to be renewed for the process's whole life and
// handed back before it stops, and only the graph reaches Start and PreStop.
//
// The clipboard between the two is process-scoped because the thing it carries
// is process-scoped: one process runs one composition root, decides one hosted
// set, and holds at most one slot per workload. A second Placement in the same
// process would be a second answer to a question that has exactly one, so New
// installs it and the Definitions read it.
var installedPlacement atomic.Pointer[Placement]

// Definition returns this package's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns side-effect-free composition data for lease-backed placement.
//
// Selecting it is what keeps the slots alive: the Definition behind it renews
// them under the process's lifecycle and releases them before it stops. It does
// not decide anything -- the hosted set is already fixed by the Placement the
// composition root built -- so Bundle and New are always used together.
func Bundle() plugin.Bundle { return bundle }

var definition = plugin.Define(
	Key,
	func(plugin.BuildContext) (*Placement, error) { return installed() },
	plugin.Options[*Placement]{
		Lifecycle: plugin.Lifecycle[*Placement]{
			Start:   (*Placement).start,
			PreStop: (*Placement).preStop,
			Stop:    (*Placement).stop,
		},
	},
)

// bundle carries the Definition that keeps the slots alive together with the
// readiness probe that reports whether they are still being confirmed. Both are
// unconditionally active once selected, because for this module selecting the
// Bundle is the entire opt-in: a deployment that placed a process by lease and
// then silently got no renewal, or no probe, would have no way to notice.
var bundle = plugin.CombineBundles(plugin.BundleOf(definition), healthBundle)

// installed returns the Placement this process decided with.
//
// It is the factory half of installedPlacement, and its error is the one place
// a composition that selected this Bundle without building a Placement is
// caught. That combination is never a deployment someone chose: it is either a
// Bundle that reached the graph without New, or a blank import of this module's
// autoload package in a process that never called New, and both would otherwise
// leave every slot unrenewed and unreleased with nothing to say so.
func installed() (*Placement, error) {
	value := installedPlacement.Load()
	if value == nil {
		return nil, errors.New("placement: this Bundle was selected but no Placement was installed; build one with placement.New and pass it with WithPlacement, or drop the import that selected this Bundle")
	}
	return value, nil
}

// Option configures New.
type Option func(*options) error

type options struct {
	keyPrefix     string
	ttl           time.Duration
	renewInterval time.Duration
	standbyRetry  time.Duration
}

func defaultOptions() options {
	return options{
		keyPrefix:     defaultKeyPrefix,
		renewInterval: defaultRenewInterval,
		standbyRetry:  defaultStandbyRetry,
	}
}

// WithKeyPrefix sets the namespace every slot key is built under, ahead of the
// constant "workload" segment that says what kind of key it is. The default is
// "xbc", producing keys such as "xbc:workload:sast:0".
func WithKeyPrefix(prefix string) Option {
	return func(config *options) error {
		config.keyPrefix = prefix
		return nil
	}
}

// WithTTL sets how long a won slot survives without a renewal, and therefore
// how long a killed holder's slot stays unavailable. The default is three
// renewal intervals.
func WithTTL(ttl time.Duration) Option {
	return func(config *options) error {
		config.ttl = ttl
		return nil
	}
}

// WithRenewInterval sets how often a held slot is renewed.
func WithRenewInterval(interval time.Duration) Option {
	return func(config *options) error {
		config.renewInterval = interval
		return nil
	}
}

// WithStandbyRetry sets how often a process that won no slot retries. It bounds
// takeover latency together with the TTL, so a shorter interval shortens how
// long a freed slot sits idle.
func WithStandbyRetry(interval time.Duration) Option {
	return func(config *options) error {
		config.standbyRetry = interval
		return nil
	}
}

func (config *options) validate() error {
	if config.keyPrefix == "" || strings.TrimSpace(config.keyPrefix) != config.keyPrefix {
		return fmt.Errorf("placement: key prefix must be non-empty and have no surrounding whitespace, got %q", config.keyPrefix)
	}
	if config.renewInterval < time.Millisecond {
		return fmt.Errorf("placement: renew interval must be at least 1ms, got %s", config.renewInterval)
	}
	if config.standbyRetry < time.Millisecond {
		return fmt.Errorf("placement: standby retry must be at least 1ms, got %s", config.standbyRetry)
	}
	if config.ttl == 0 {
		config.ttl = 3 * config.renewInterval
	}
	if config.ttl < time.Millisecond {
		return fmt.Errorf("placement: ttl must be at least 1ms, got %s", config.ttl)
	}
	if config.renewInterval > config.ttl/2 {
		return fmt.Errorf(
			"placement: renew interval %s must not exceed half of ttl %s\n  → a slot needs at least two renewal windows inside its ttl, otherwise a single slow round expires it with no chance to retry",
			config.renewInterval, config.ttl)
	}
	return nil
}

// Placement is one process's lease-backed hosting decision, and the plugin that
// keeps it alive.
//
// It implements plugin.PlacementSource, so the runtime asks it which workloads
// this process carries; it is also the Definition behind Bundle, so the same
// value renews those slots under Start and gives them back under PreStop.
type Placement struct {
	locker       lease.Locker
	prefix       string
	ttl          time.Duration
	renewEvery   time.Duration
	standbyEvery time.Duration

	// rng picks the slot a workload's search starts at. It is guarded because
	// Resolve and the standby loop both draw from it, and the draw must stay
	// random: a fixed starting index makes every standby race the same slot and
	// lose the same way forever instead of spreading over the workload's set.
	rngMu sync.Mutex
	rng   *mathrand.Rand

	mu       sync.Mutex
	resolved bool
	decision plugin.Placement
	admitted []plugin.Workload
	held     []*heldSlot
	// instance is the process identity the resolving request carried, kept so
	// Stats can label this process's slots with it. It is written once, by the
	// round that resolves, under mu.
	instance string
	started  bool
	stopping bool
	// handback is every slot this process won but never hosted: a standby's win
	// on its way out, recorded so a handover that failed is still owed. See
	// adoptHandback for why these are kept out of held.
	handback []*heldSlot
	// looping reports that a managed loop was submitted -- the renewal loop, or
	// the standby retry loop -- so PreStop knows whether it has one to wait for.
	// Both loops touch the store, so both are loops a release has to be ordered
	// after.
	looping bool
	logger  log.Logger

	// quiet is closed once, by quiesce, and is what tells both managed loops to
	// stop touching the store.
	quiet     chan struct{}
	quietOnce sync.Once

	// loopDone is closed when the managed loop has returned -- or by Stop when
	// no loop was ever started, so a PreStop that waits on it can never wait on
	// a goroutine that does not exist.
	loopDone     chan struct{}
	loopDoneOnce sync.Once

	loops sync.WaitGroup

	renewFailures atomic.Uint64
}

var _ plugin.PlacementSource = (*Placement)(nil)

// New builds the lease-backed placement for this process and installs it.
//
// The caller supplies the Locker, because placement is decided before the
// plugin graph exists and nothing inside the graph can be consulted yet. New
// performs no I/O: the slots are won by Resolve, which the runtime calls before
// it plans.
func New(locker lease.Locker, options ...Option) (*Placement, error) {
	if locker == nil {
		return nil, errors.New("placement: New requires a non-nil lease store; placement is decided before the plugin graph exists, so the composition root has to supply the store it reads")
	}
	config := defaultOptions()
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("placement: option %d is nil", index)
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if err := config.validate(); err != nil {
		return nil, err
	}

	value := &Placement{
		locker:       locker,
		prefix:       config.keyPrefix,
		ttl:          config.ttl,
		renewEvery:   config.renewInterval,
		standbyEvery: config.standbyRetry,
		rng:          newSource(),
		logger:       log.L(),
		quiet:        make(chan struct{}),
		loopDone:     make(chan struct{}),
	}
	installedPlacement.Store(value)
	return value, nil
}

// Bundle returns the Bundle that keeps this placement's slots alive. It is the
// same Bundle as the package-level Bundle(): the Definition reads the process's
// placement, so every Placement is kept alive by one canonical Definition
// rather than by a per-instance one.
func (p *Placement) Bundle() plugin.Bundle { return bundle }

// Resolve wins at most one slot per admitted workload and reports the resulting
// hosted set.
//
// It is called once, before the assembly plan is built, and its answer is
// final: a Placement that has already resolved returns the same decision rather
// than competing again, because the runtime and the doctor command may both ask
// and a second round would leak the first round's slots.
func (p *Placement) Resolve(request plugin.PlacementRequest) (plugin.Placement, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolved {
		return p.decision, nil
	}

	admitted, notes, err := admissionOrder(request)
	if err != nil {
		return plugin.Placement{}, err
	}
	held, heldNotes, err := p.acquireAll(context.Background(), admitted)
	if err != nil {
		// The slots won before the failure come back with the error instead of
		// being disposed of inside acquireAll -- see there for why the disposal
		// cannot live in one place -- and this is the lock context that can
		// record what is still owed, because p.mu is already held for the whole
		// decision.
		//
		// Best effort first, and only the remainder is recorded: a store that is
		// answering takes the slot back here and nothing is left over, which is
		// what keeps a failed cold start from reporting slots it does not have.
		// The hard error is unchanged either way -- a process that cannot read
		// the store fails rather than guessing that it hosts everything.
		owed, releaseErr := p.handBack(context.Background(), held)
		p.handback = append(p.handback, owed...)
		if releaseErr != nil {
			p.logger.Warn("placement: a slot won before the placement round failed could not be given back, it stays owed to the store",
				"error", releaseErr)
		}
		return plugin.Placement{}, err
	}
	notes = append(notes, heldNotes...)

	decision := plugin.Placement{
		Source: placementSourceLease,
		Hosted: hostedKeys(held),
		Notes:  notes,
	}
	if len(held) > 0 {
		decision.Holder = claimant(request.Instance, held[0].lease.Owner())
		// This is the one line that binds the two names a slot has: the process
		// the operator can go and look at, and the token the store has in the
		// key. Nothing else prints them together -- Holder carries one, the
		// store holds the other -- so a wedged holder found by reading the store
		// directly is traceable to a process only from here.
		for _, slot := range held {
			p.logger.Info("placement: slot won",
				"workload", slot.workload.String(),
				"slot", slot.index,
				"key", slot.key,
				"instance", request.Instance,
				"owner", slot.lease.Owner())
		}
	} else {
		decision.Notes = append(decision.Notes,
			"this process won no slot and starts as a standby; it hosts only the plugins that belong to no workload and retries until it wins one")
	}

	p.admitted = admitted
	p.held = held
	p.instance = request.Instance
	p.decision = decision
	p.resolved = true
	return decision, nil
}

// claimant names who holds a slot, preferring the process over the token.
//
// The instance is the answer to the question an operator is actually asking --
// which process do I go and look at -- and the token answers only "some claim
// exists". The token is still what is reported when no instance was offered,
// because a process that holds slots and names no claimant at all would read in
// a diagnostic exactly like a static decision that claims nothing.
func claimant(instance, token string) string {
	if instance != "" {
		return instance
	}
	return token
}

// admissionOrder filters the declared workloads by configuration and fixes the
// order they are attempted in: exclusive workloads first, so a scarce process
// goes to the workload that cannot share one, then the rest by key.
//
// The enabled veto is applied here rather than at acquisition, because it is
// the deployment's statement that this process must never carry the workload.
// Hard veto means hard: a slot that is sitting free in the store does not
// override it.
func admissionOrder(request plugin.PlacementRequest) ([]plugin.Workload, []string, error) {
	admitted := make([]plugin.Workload, 0, len(request.Workloads))
	seen := make(map[plugin.WorkloadKey]bool, len(request.Workloads))
	var notes []string
	for _, workload := range request.Workloads {
		if !request.Admits(workload.Key) {
			notes = append(notes, fmt.Sprintf("workload %q is disabled in this process's configuration, so no slot was attempted", workload.Key))
			continue
		}
		// "At most one slot per workload" is this component's invariant to keep,
		// not its caller's: a request that names a workload twice would otherwise
		// win two of its slots, and the process would carry a replica count the
		// composition never declared.
		if seen[workload.Key] {
			return nil, nil, fmt.Errorf(
				"placement: workload %q is declared twice in this process's placement request\n  → a workload occupies at most one slot per process, so a repeated entry cannot be honoured; declare it once",
				workload.Key)
		}
		seen[workload.Key] = true
		if workload.Replicas < 1 {
			return nil, nil, fmt.Errorf(
				"placement: workload %q declares %d replicas\n  → a workload with no slot can never be carried; declare at least one with WithReplicas",
				workload.Key, workload.Replicas)
		}
		admitted = append(admitted, workload)
	}
	sort.Slice(admitted, func(i, j int) bool {
		if admitted[i].Exclusive != admitted[j].Exclusive {
			return admitted[i].Exclusive
		}
		return admitted[i].Key < admitted[j].Key
	})
	return admitted, notes, nil
}

// acquireAll wins at most one slot per workload in admitted order, and stops as
// soon as it has won an exclusive one.
//
// Stopping there is what makes exclusivity true rather than aspirational: the
// process now carries a workload with process-wide side effects, so every
// additional slot it took would be a workload sharing a process it was promised
// it would not have to share.
//
// A round that fails returns the slots it had already won along with the error,
// and disposing of them is the caller's job rather than this function's. That is
// not a convenience: a partial win has to be recorded as well as released --
// released on a context that is not the caller's, and kept on the owed list when
// the release did not land -- and recording it needs p.mu. Resolve calls this
// while holding p.mu for the whole decision, so an adoptHandback from in here
// would deadlock on its caller's own lock, while the standby loop, the other
// caller, holds nothing at all. There is no single lock context the disposal can
// live in, so it lives at the two call sites, each of which knows its own. Lock
// order is unaffected by the choice: Placement.mu → heldSlot.mu is the package's
// existing nesting and a slot never reaches back into Placement, so releasing
// under p.mu adds no order that was not already there -- Resolve is in any case
// already holding p.mu across store calls.
func (p *Placement) acquireAll(ctx context.Context, admitted []plugin.Workload) ([]*heldSlot, []string, error) {
	var held []*heldSlot
	var notes []string
	for _, workload := range admitted {
		slot, won, err := p.acquire(ctx, workload)
		if err != nil {
			// A partial win is not a decision anyone can act on, so it is owed
			// back before the error is acted on: leaving the slots held would
			// make a process that failed to plan the owner of capacity it never
			// hosts. Dropping them here instead of handing them up would make
			// them unreachable from every path that could still give them back,
			// which is the leak this return value exists to close.
			return held, nil, err
		}
		if !won {
			notes = append(notes, fmt.Sprintf("workload %q holds all %d of its slots elsewhere, so this process is not carrying it", workload.Key, workload.Replicas))
			continue
		}
		held = append(held, slot)
		notes = append(notes, fmt.Sprintf("workload %q holds slot %d of %d (%s)", workload.Key, slot.index, workload.Replicas, slot.key))
		if workload.Exclusive {
			break
		}
	}
	return held, notes, nil
}

// acquire wins one slot of workload, or reports that every slot is held
// elsewhere.
//
// The search starts at a random index and wraps. Starting at zero would make
// every candidate try the same slot first and lose the same way on every
// attempt, so a freed slot would be found by whichever process happened to be
// scheduled next rather than by the search spreading over the set.
func (p *Placement) acquire(ctx context.Context, workload plugin.Workload) (*heldSlot, bool, error) {
	offset := p.randomOffset(workload.Replicas)
	for attempt := 0; attempt < workload.Replicas; attempt++ {
		index := (offset + attempt) % workload.Replicas
		key := p.slotKey(workload.Key, index)
		won, acquired, err := p.locker.TryAcquire(ctx, key, p.ttl)
		if err != nil {
			return nil, false, fmt.Errorf(
				"placement: cannot reach the lease store to win a slot for workload %q at %q: %w\n  → the hosted set has to be decided before the graph is built, so a process that cannot read the store must not guess and host everything instead; check the lease backend, or drop WithPlacement",
				workload.Key, key, err)
		}
		if !acquired {
			continue
		}
		return newHeldSlot(workload.Key, index, key, won), true, nil
	}
	return nil, false, nil
}

// slotKey is one slot's key: <prefix>:workload:<workload>:<index>.
func (p *Placement) slotKey(workload plugin.WorkloadKey, index int) string {
	return p.prefix + ":workload:" + workload.String() + ":" + strconv.Itoa(index)
}

func (p *Placement) randomOffset(replicas int) int {
	if replicas <= 1 {
		return 0
	}
	p.rngMu.Lock()
	defer p.rngMu.Unlock()
	return p.rng.IntN(replicas)
}

// newSource seeds a per-Placement generator from the system's entropy source.
// A clock-derived seed would be enough for one process, but several Placements
// are routinely built inside one test binary's tick, and a shared seed there
// would make the very correlation this offset exists to break.
//
// A counter is mixed in unconditionally rather than only when the entropy read
// fails, because the failure mode of a silently reused seed is the exact
// behaviour the offset is there to prevent: every standby would start its search
// at the same index, race the same slot, and lose identically for ever. The
// counter makes two Placements in one process differ even if crypto/rand is
// unavailable, and crypto/rand makes two processes differ.
func newSource() *mathrand.Rand {
	var seed [2]uint64
	seed[0] = uint64(sourceOrdinal.Add(1))
	seed[1] = uint64(time.Now().UnixNano())
	for index := range seed {
		var raw [8]byte
		if _, err := cryptorand.Read(raw[:]); err == nil {
			seed[index] ^= binary.LittleEndian.Uint64(raw[:])
		}
	}
	return mathrand.New(mathrand.NewPCG(seed[0], seed[1]))
}

// sourceOrdinal distinguishes generator seeds built within one process.
var sourceOrdinal atomic.Uint64

func hostedKeys(held []*heldSlot) []plugin.WorkloadKey {
	hosted := make([]plugin.WorkloadKey, 0, len(held))
	for _, slot := range held {
		hosted = append(hosted, slot.workload)
	}
	sort.Slice(hosted, func(i, j int) bool { return hosted[i] < hosted[j] })
	return hosted
}

// releaseAll gives back every slot in held, reporting rather than aborting on a
// failure: one unreachable key must not stop the others from being returned,
// and a lease that cannot be released expires on its own.
func releaseAll(ctx context.Context, held []*heldSlot) error {
	var failures []error
	for _, slot := range held {
		if err := slot.release(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// snapshotHeld copies the held slice so callers can walk it without holding the
// Placement mutex. Every slot's own state stays behind its own mutex, so the
// two locks are never nested.
func (p *Placement) snapshotHeld() []*heldSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*heldSlot(nil), p.held...)
}

// adoptHandback records slots this process won but is not hosting, so that a
// handover which did not complete is still owed to the store.
//
// They are deliberately kept out of held. held is what this process hosts: it
// is what the renewal loop keeps alive, what the readiness probe reports on and
// what Stats publishes as xbc_workload_held. A standby's win is none of those
// things -- the process was assembled without that workload and is restarting
// to take it up -- so putting it in held would arrange for a slot on its way
// back to be renewed, and would tell an operator that a process carries a
// workload it is in the middle of handing over.
func (p *Placement) adoptHandback(slots []*heldSlot) {
	if len(slots) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handback = append(p.handback, slots...)
}

// releasable is every slot this process still owes the store: the ones it
// hosts, plus the ones it won without hosting and has not confirmed as handed
// back. Release is idempotent, so a slot that was already given back costs
// nothing here, which is what lets the Stop backstop walk this list
// unconditionally.
func (p *Placement) releasable() []*heldSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	slots := make([]*heldSlot, 0, len(p.held)+len(p.handback))
	slots = append(slots, p.held...)
	return append(slots, p.handback...)
}

// stopRequested reports, without blocking, that this placement must stop
// touching the store.
//
// Both signals are read because neither implies the other. The run's context is
// cancelled by the runtime the moment the process is asked to stop, before any
// hook of this plugin runs; quiet is closed by this placement's own shutdown
// hooks, which is what a Stop called on a run that was never cancelled -- a
// test, or an application torn down without a stop request -- has to be
// observed through.
func (p *Placement) stopRequested(runCtx context.Context) bool {
	if runCtx.Err() != nil {
		return true
	}
	select {
	case <-p.quiet:
		return true
	default:
		return false
	}
}

func (p *Placement) currentLogger() log.Logger {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logger == nil {
		return log.L()
	}
	return p.logger
}
