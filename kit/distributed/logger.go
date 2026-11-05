package distributed

// Logger is the diagnostic sink the distributed primitives write to. It is the
// smallest surface they use rather than a logging framework, so both the
// framework's log.Logger and a host's *slog.Logger satisfy it while this
// package stays free of any logging dependency. A nil Logger discards
// diagnostics.
type Logger interface {
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// nopLogger discards every diagnostic. It is the default so a zero-value
// option never dereferences a nil sink.
type nopLogger struct{}

func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}
