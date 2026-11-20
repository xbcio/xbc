package log

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// JSONKeys renames the five base keys of a record produced by NewJSONCore. An
// empty field keeps the built-in name (ts, level, msg, caller, stack), so the
// zero value reproduces the layout jsonEncoderConfig sets for the built-in
// JSON sinks.
//
// JSONKeys covers the keys the encoder itself writes. The names carrying trace
// context are a separate decision: Config.Trace owns those, because trace
// fields are stamped into a logger at derivation time as ordinary KV pairs
// rather than written by the encoder.
type JSONKeys struct {
	Time    string // default "ts"
	Level   string // default "level"
	Message string // default "msg"
	Caller  string // default "caller"
	Stack   string // default "stack"
}

// JSONCoreOptions configures NewJSONCore.
type JSONCoreOptions struct {
	// Writer receives one JSON object per record. Required. A writer that also
	// implements Sync() error has it called by the core's Sync; for any other
	// writer Sync is a no-op, matching zapcore.AddSync. Writes through this
	// core are serialized, but sharing the writer with another core or using
	// it concurrently outside this core is the caller's responsibility.
	Writer io.Writer

	// Level is the level the core admits. With Exact false it admits Level and
	// above; the zero value is InfoLevel. Ignored when Enabler is non-nil.
	Level zapcore.Level

	// Exact restricts the core to exactly Level instead of Level and above. It
	// is what lets two cores split an error-only topic from an info-only one
	// without sending every error record to both. Ignored when Enabler is
	// non-nil.
	Exact bool

	// Enabler, when non-nil, replaces Level and Exact entirely.
	Enabler zapcore.LevelEnabler

	// Keys renames the base record keys; see JSONKeys.
	Keys JSONKeys

	// TimeLayout is the time.Format layout for the timestamp. Empty keeps
	// RFC3339Nano, the package's JSON default.
	TimeLayout string

	// UpperLevel renders the level as INFO instead of info.
	UpperLevel bool

	// FullCaller renders the caller's full file path instead of the
	// package-relative short form. The caller field itself appears only when
	// the assembling logger collects caller information (Init's Config.Caller
	// or zap.AddCaller).
	FullCaller bool

	// Fields supplies static fields appended to every record. It runs exactly
	// once, when the core is constructed, so it suits values fixed for the
	// core's lifetime -- an application name, a host name -- and not values
	// that change per record. The result goes through the built-in masking
	// blacklist like any other field, so a password-named static field is
	// emitted redacted.
	Fields func() []zap.Field
}

// NewJSONCore builds a configurable JSON core, starting from the same encoder
// baseline as this package's built-in JSON sinks. The returned core is meant to
// be wired through Attach, which adds the process-wide masking and level gates;
// used on its own it is a plain zapcore.Core and filters only by Level/Exact
// or Enabler.
//
// Validation rejects a nil Writer, a blank or duplicate key, a key that
// collides with the trace field layout active at construction, and a time
// layout the runtime refuses to format. Errors are English and contain no
// record data.
func NewJSONCore(opts JSONCoreOptions) (zapcore.Core, error) {
	if opts.Writer == nil {
		return nil, errors.New("log.json_core: writer is required")
	}
	keys, err := resolveJSONKeys(opts.Keys)
	if err != nil {
		return nil, err
	}
	if err := validateJSONTimeLayout(opts.TimeLayout); err != nil {
		return nil, err
	}

	cfg := jsonEncoderConfig()
	cfg.TimeKey = keys.Time
	cfg.LevelKey = keys.Level
	cfg.MessageKey = keys.Message
	cfg.CallerKey = keys.Caller
	cfg.StacktraceKey = keys.Stack
	if opts.TimeLayout != "" {
		cfg.EncodeTime = zapcore.TimeEncoderOfLayout(opts.TimeLayout)
	}
	if opts.UpperLevel {
		cfg.EncodeLevel = zapcore.CapitalLevelEncoder
	}
	if opts.FullCaller {
		cfg.EncodeCaller = zapcore.FullCallerEncoder
	}

	var enabler zapcore.LevelEnabler = opts.Enabler
	if enabler == nil {
		if opts.Exact {
			enabler = exactLevel(opts.Level)
		} else {
			enabler = opts.Level
		}
	}

	core := zapcore.NewCore(zapcore.NewJSONEncoder(cfg), zapcore.AddSync(opts.Writer), enabler)
	if opts.Fields != nil {
		// Masked here rather than only through Attach: these fields are fixed
		// into the core at construction, before any attached maskCore sees the
		// core, so the seam's masker would never reach them.
		core = core.With(newMasker(nil).apply(opts.Fields()))
	}
	return core, nil
}

// exactLevel admits one level and nothing else, unlike zapcore.Level, which
// admits that level and everything above it.
type exactLevel zapcore.Level

func (l exactLevel) Enabled(lv zapcore.Level) bool { return lv == zapcore.Level(l) }

// resolveJSONKeys fills the defaults from jsonEncoderConfig so the core and the
// built-in JSON encoder cannot drift apart, then rejects a layout that would
// produce a duplicate or unsearchable record. Blank means "keep the default";
// whitespace-only and duplicates are configuration errors.
func resolveJSONKeys(k JSONKeys) (JSONKeys, error) {
	base := jsonEncoderConfig()
	fields := []struct {
		name  string
		value *string
		def   string
	}{
		{"time", &k.Time, base.TimeKey},
		{"level", &k.Level, base.LevelKey},
		{"message", &k.Message, base.MessageKey},
		{"caller", &k.Caller, base.CallerKey},
		{"stack", &k.Stack, base.StacktraceKey},
	}
	for _, f := range fields {
		if *f.value == "" {
			*f.value = f.def
			continue
		}
		if strings.TrimSpace(*f.value) == "" {
			return JSONKeys{}, fmt.Errorf("log.json_core: keys.%s must not be blank", f.name)
		}
	}

	seen := make(map[string]string, len(fields))
	for _, f := range fields {
		if prev, dup := seen[*f.value]; dup {
			return JSONKeys{}, fmt.Errorf("log.json_core: keys.%s and keys.%s must differ", prev, f.name)
		}
		seen[*f.value] = f.name
	}

	// The layout is validated against the trace names active now. Init may
	// rename them later; keeping the two layouts distinct across a re-init is
	// the caller's responsibility, because a constructor cannot see a future
	// configuration.
	trace := currentTraceConfig()
	activeTraceFields := []struct {
		name  string
		value string
	}{
		{"trace_id", trace.TraceID},
		{"span_id", trace.SpanID},
		{"span_name", trace.SpanName},
		{"request_id", trace.RequestID},
	}
	for _, f := range fields {
		for _, t := range activeTraceFields {
			if *f.value == t.value {
				return JSONKeys{}, fmt.Errorf("log.json_core: keys.%s conflicts with the active trace field %s", f.name, t.name)
			}
		}
	}
	return k, nil
}

// validateJSONTimeLayout probes formatting once. time.Format has no error
// return, so this guards the only failure a layout can cause; any layout the
// runtime formats is accepted and becomes the core's timestamp layout.
func validateJSONTimeLayout(layout string) (err error) {
	if layout == "" {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.New("log.json_core: time_layout cannot format a time")
		}
	}()
	_ = time.Unix(0, 0).UTC().Format(layout)
	return nil
}
