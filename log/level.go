package log

import (
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// levelState is the default backend's shared level. Init hands every sink's
// core this one object instead of a fixed zapcore.Level, so SetLevel moves the
// level the cores actually read: every logger already handed out -- including
// the ones plugins captured at construction -- observes the change at its next
// write, with no re-assembly and no new Logger. A core keeps its reference, so
// the object outlives every Init that installs it.
//
// The AtomicLevel is deliberately not exported: it accepts any zapcore value
// directly, bypassing the validation ParseLevel exists to enforce, so
// consumers go through SetLevel instead.
var levelState = struct {
	level zap.AtomicLevel
	// decided records whether a level has been chosen at all. NewAtomicLevelAt
	// stores a value up front, so without this flag CurrentLevel before any
	// decision would report a level as if the framework had picked one.
	decided atomic.Bool
}{level: zap.NewAtomicLevelAt(zapcore.InfoLevel)}

// SetLevel moves the default backend's level and reports the parsed level.
//
// It is the one configuration key a running process applies in place: unlike
// Init, it does not re-assemble the backend, so it neither closes the file
// sink loggers captured earlier are still writing to nor needs the DI graph
// rebuilt; see zap.go's Init for why calling Init repeatedly is not an option.
// An unknown level returns an error and moves nothing -- see ParseLevel for
// why a misspelled level must not degrade silently.
//
// A call before Init, or while logging is disabled, is recorded rather than
// discarded: CurrentLevel reports it, and the next Init then decides the
// level from its config anyway.
func SetLevel(s string) (Level, error) {
	lv, err := ParseLevel(s)
	if err != nil {
		return lv, err
	}
	applyLevel(lv)
	return lv, nil
}

// CurrentLevel reports the level the default backend filters at: the most
// recent decision, whether that came from SetLevel or from Init's config.
// Before either, it reports the level an unspecified config would produce.
func CurrentLevel() Level {
	if !levelState.decided.Load() {
		lv, _ := ParseLevel("") // what an empty log.level means, per ParseLevel's contract
		return lv
	}
	return Level(levelState.level.Level())
}

// applyLevel is the one place a level moves, so Init and SetLevel cannot
// drift. Init calls it before installing the cores rather than through
// SetLevel, because it has already parsed the string and reporting a parse
// error twice would be dead code.
func applyLevel(lv Level) {
	levelState.level.SetLevel(zapcore.Level(lv))
	levelState.decided.Store(true)
}
