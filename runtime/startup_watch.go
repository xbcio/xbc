package runtime

import (
	"sync"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// The startup phase labels, in the order execute walks them. They are the same
// strings the timing breakdown prints (startupPhases.String), because an
// operator who reads "phase start" in a slow-startup warning and "start 31s"
// in the breakdown must not have to work out that those are two names for one
// phase.
const (
	phaseBootstrap = "bootstrap"
	phasePlanning  = "planning"
	phaseConstruct = "construct"
	phaseMigrate   = "migrate"
	phaseStart     = "start"
	phaseTraffic   = "traffic"
)

// startupProgress is where the startup currently is: the phase execute has
// entered, and, inside a phase that invokes plugin hooks, the plugin and stage
// it handed control to last.
//
// It exists because every other startup diagnostic is written after the fact.
// StageTiming is appended once a hook returns, and both the released-gate line
// and the per-plugin breakdown run after all phases have returned, so a boot
// that never finishes produces none of them. This record is written before
// control is handed over, which is what makes it readable while the handover
// has not come back.
//
// It is the one piece of startup state read from a second goroutine -- the
// watchdog -- so unlike startupTiming it is mutex-protected rather than
// relying on the execute goroutine being the only party.
type startupProgress struct {
	mu        sync.Mutex
	phase     string
	identity  plugin.Identity
	stage     assembly.Stage
	since     time.Time
	concluded bool
}

// startupPosition is one consistent read of a startupProgress.
type startupPosition struct {
	phase    string
	identity plugin.Identity
	stage    assembly.Stage
	since    time.Time
}

// enterPhase records that startup has reached phase.
//
// The plugin and stage are cleared, never carried over. They describe a
// handover that the previous phase has already come back from, and reporting
// "phase migrate" beside a plugin left over from construction would be a
// contradiction an operator has no way to detect -- it names a plugin that is,
// at that moment, doing nothing at all.
func (p *startupProgress) enterPhase(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase = phase
	p.identity = plugin.Identity{}
	p.stage = ""
	p.since = time.Now()
}

// enterStage records the plugin and stage the current phase is handing control
// to. It is assembly.ConstructOptions.OnStageBegin, so it runs on the execute
// goroutine immediately before the hook itself.
func (p *startupProgress) enterStage(identity plugin.Identity, stage assembly.Stage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.identity = identity
	p.stage = stage
	p.since = time.Now()
}

func (p *startupProgress) position() startupPosition {
	p.mu.Lock()
	defer p.mu.Unlock()
	return startupPosition{phase: p.phase, identity: p.identity, stage: p.stage, since: p.since}
}

// conclude records that the startup sequence is over, whether or not it
// succeeded, so that nothing reports a position again.
//
// The reverse unwind is the reason this is needed rather than relying on the
// watchdog being stopped. A startup that fails hands straight to unwind, and a
// migrate-only run unwinds after migrating; in both the watchdog is still
// running, so a slow unwind would otherwise produce a report naming the phase
// and plugin of a handover that has already come back -- a plugin that is at
// that moment doing nothing. The unwind reports itself, under its own budget.
//
// A stop request alone is deliberately not the signal. An operator who
// interrupts a boot that is stuck in a plugin hook requests a stop that cannot
// be honoured, because the hook never returns and the unwind never begins;
// that is exactly when naming the plugin still holding startup matters most.
func (p *startupProgress) conclude() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.concluded = true
}

func (p *startupProgress) hasConcluded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.concluded
}

// watchSlowStartup reports every interval, until the returned stop is called,
// that startup has not finished and where it is. A zero or negative interval
// disables the report entirely and starts no goroutine.
//
// The logger is passed in rather than read from the App on every tick: the
// watchdog is the only part of startup that runs off the execute goroutine,
// and reading a field that goroutine also writes would make an operational
// report a data race.
//
// stop is idempotent and joins the goroutine before returning, so a caller
// that has stopped the watchdog knows no further report can appear -- which is
// what lets execute silence it before announcing the released gate, without
// leaving a line that contradicts the announcement in flight.
func (a *App) watchSlowStartup(logger log.Logger, startedAt time.Time, interval time.Duration) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				a.reportSlowStartup(logger, time.Since(startedAt), interval)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}

// reportSlowStartup names the phase, and where there is one the plugin and
// stage, that a startup which has not finished is currently inside.
//
// It warns rather than reporting at debug for the reason the released-gate
// line is info and the per-plugin breakdown is not: this record carries a
// fixed number of fields whatever an application selected, so its cost does
// not follow the plugin count. It is also the only startup diagnostic that can
// be emitted while a hook is still running, so on the boot that needs it there
// is nothing else to read.
//
// It repeats on purpose. One line says a startup is slow; consecutive lines
// naming the same plugin and stage say it is stuck, and lines whose phase or
// plugin keeps moving say it is slow but progressing -- a distinction no single
// report can make. Nothing here is a configured value: a phase name, an
// identity, a stage name and three durations.
func (a *App) reportSlowStartup(logger log.Logger, elapsed, threshold time.Duration) {
	if a.progress.hasConcluded() {
		return
	}
	position := a.progress.position()
	logger.Warn("xbc: startup has not finished within the slow startup threshold",
		"elapsed", elapsed.String(),
		"threshold", threshold.String(),
		"phase", position.phase,
		"plugin", position.identity.String(),
		"stage", string(position.stage),
		"waiting", time.Since(position.since).String(),
	)
}
