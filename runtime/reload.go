package runtime

import (
	"strings"
	"sync"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
)

// logLevelPath is the one configured leaf a running process applies in place.
const logLevelPath = loggingSection + ".level"

// configReload is the configuration watch one App owns.
//
// bootstrap fills the locators and the boot configuration; from the moment
// the watch starts, only its callback replaces last and only unwind clears
// stop. No other App state is involved, which is what keeps a reload from
// touching a plan, an instance, or a task scope.
type configReload struct {
	// file and profile are the locators the boot resolved, reused verbatim:
	// --config and --profile name the configuration this process was started
	// with, and a reload re-reads it rather than discovering a different one.
	file    string
	profile string
	// universe is the frozen section ownership the boot built. Reusing the
	// same value is what makes a reload validate against the composition
	// that is actually running, not a rebuilt approximation of it.
	universe *config.Universe

	// mu serializes a reload's whole move -- read, diff, decide, accept --
	// against the next one, and keeps last consistent under it. The file
	// watch promises not to call its callback concurrently, but the reload
	// taking its own exclusion is what keeps that promise from being a
	// dependency: the guarantee this state needs is stated here.
	mu   sync.Mutex
	last *config.Environment
	// stop ends the file watch. It belongs to the execute goroutine alone:
	// startConfigWatch sets it and unwind clears it, and no other goroutine
	// reads it.
	stop func() error
}

// startConfigWatch arms the configuration watch on the files this boot read.
// It runs on the execute goroutine once the run reached servable -- after the
// traffic gate opened and liveness was asserted -- and never earlier, so no
// plugin's Start can race a reload, and before wait, so a run that observed
// its running state is already watching and every path that follows hands
// unwind the same watch state.
//
// A run that read no file starts no watch: an empty file list can report
// nothing, and the ENV layer is fixed for the life of the process, so a watch
// there would only idle a goroutine. A watch that cannot be armed is a
// warning rather than a failure: the configuration has already been read and
// the process is already serving, and refusing to serve over a monitor that
// could not attach would turn an operational convenience into an outage. The
// warning names what could not be watched, which is what an operator who
// never sees a reload needs in order to find the cause.
func (a *App) startConfigWatch() {
	files := a.env.Files()
	if len(files) == 0 {
		return
	}
	// The callback's argument -- the files whose content may differ -- is
	// deliberately ignored: a reload re-reads every source anyway, so the
	// notification is a trigger, not a result.
	stop, err := config.WatchFiles(files, func([]string) { a.reloadOnce() })
	if err != nil {
		a.log().Warn("xbc: configuration reload is unavailable; the configuration files could not be watched",
			"files", files, "error", err)
		return
	}
	a.reload.stop = stop
}

// stopConfigWatch ends the watch and joins a reload already in flight, so
// nothing re-reads or applies configuration after it returns. unwind calls it
// first, before the pre-stop phase and every Stop: from there the process is
// being torn down under a shutdown budget, and a configuration change must
// not be applied to a graph that is coming apart. It is idempotent.
func (a *App) stopConfigWatch() {
	stop := a.reload.stop
	if stop == nil {
		return
	}
	a.reload.stop = nil
	if err := stop(); err != nil {
		a.log().Warn("xbc: stopping the configuration watch failed", "error", err)
	}
}

// reloadOnce re-reads the configuration sources and moves the process as far
// toward the new configuration as a running process can go.
//
// Sources are re-read through the same config.Options the boot used, so
// everything Load validates -- an unknown key, an unowned section, an
// unresolvable environment overlay -- rejects the whole change and keeps the
// running configuration. The change is then classified path by path
// (classifyReload), and if any path belongs to something a running process
// cannot change, the whole change is rejected: a file that mixes a reloadable
// change with a restart-required one never applies half of it.
//
// Rejection is a warning, not a failure. The process keeps serving what it
// was already configured with, and the operator acts on the paths the
// warning names.
func (a *App) reloadOnce() {
	a.reload.mu.Lock()
	defer a.reload.mu.Unlock()

	next, err := config.Load(config.Options{
		File:      a.reload.file,
		Profile:   a.reload.profile,
		EnvPrefix: config.DefaultEnvPrefix,
		Defaults:  a.defaults,
		Universe:  a.reload.universe,
	})
	if err != nil {
		a.log().Warn("xbc: configuration reload rejected; keeping the running configuration", "error", err)
		return
	}
	changed := config.ChangedPaths(a.reload.last, next)
	if len(changed) == 0 {
		// A write that restores the bytes already read -- or that lands
		// where no layer reads -- still fires the watch; nothing changed in
		// the merged view, so there is nothing to accept or report beyond
		// this. The base still advances: it names the same configuration.
		a.log().Debug("xbc: configuration files changed without changing the merged configuration")
		a.reload.last = next
		return
	}
	applied, accepted, restart := classifyReload(changed)
	if len(restart) > 0 {
		a.log().Warn("xbc: configuration reload rejected; these paths require a restart",
			"paths", reloadPathLabels(next, restart))
		return
	}
	if len(applied) > 0 {
		if err := a.applyLogLevel(next); err != nil {
			a.log().Warn("xbc: configuration reload rejected; keeping the running configuration", "error", err)
			return
		}
	}
	// The base advances only on acceptance: a rejected change is reported
	// against the configuration that is still running, so the next reload
	// reports it again -- and a file fixed back to its accepted shape diffs
	// as the change it is.
	a.reload.last = next
	if len(accepted) > 0 {
		a.log().Info("xbc: application configuration changed", "paths", reloadPathLabels(next, accepted))
	}
}

// applyLogLevel moves the backend to the level the new configuration names,
// bound and validated through the same path the boot used, so a value a boot
// would refuse is refused here too.
//
// It deliberately does not go through log.Init: Init closes the file sink the
// loggers already handed out -- the ones plugins captured at construction --
// are still writing to. SetLevel moves the level under them instead.
func (a *App) applyLogLevel(env *config.Environment) error {
	logging, err := bindLogging(env)
	if err != nil {
		return err
	}
	previous := log.CurrentLevel()
	level, err := log.SetLevel(logging.Level)
	if err != nil {
		return err
	}
	a.log().Info("xbc: log level changed", "level", level.String(), "previous", previous.String())
	return nil
}

// classifyReload splits the changed paths into the three outcomes one reload
// can have for a path: applied (moved in place), accepted (part of the
// configuration the process now holds, with no runtime action) and
// restart-required.
//
// Everything not named here requires a restart, which is the safe default.
// The plugin and workload sections are the composition the process froze at
// planning time -- an enabled flip is as much a graph change as a new
// instance -- and every leaf under xbc was resolved once during bootstrap:
// the shutdown budgets sized the unwind the process will run, instance_id
// names a placement lease the process holds, auto_migrate gated a stage that
// has run, and slow_startup_after paced a startup that has finished.
// log.level is the one leaf that can move under a live process; the rest of
// the log section re-assembles sinks, which is the restart log.Init performs.
// app.* belongs to the application author and the runtime never interprets
// it, so a change there is accepted and reported.
func classifyReload(changed []string) (applied, accepted, restart []string) {
	for _, path := range changed {
		switch {
		case path == logLevelPath:
			applied = append(applied, path)
		case path == applicationSection || strings.HasPrefix(path, applicationSection+"."):
			accepted = append(accepted, path)
		default:
			restart = append(restart, path)
		}
	}
	return applied, accepted, restart
}

// reloadPathLabels renders changed paths together with the sources that now
// set them, so a report names the file an operator has to edit. A path no
// source carries any more -- one the change removed -- has no origin to name
// and is reported as the bare path. Values never enter: a label is composed
// of a path and layer labels only, so a masked leaf's value cannot leak here
// any more than it can through Sources or OriginsUnder.
func reloadPathLabels(env *config.Environment, paths []string) []string {
	labels := make([]string, 0, len(paths))
	for _, path := range paths {
		origins := env.OriginsUnder(path)
		if len(origins) == 0 {
			labels = append(labels, path)
			continue
		}
		labels = append(labels, path+" ("+strings.Join(origins, ", ")+")")
	}
	return labels
}
