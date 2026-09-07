// Package log is xbc's logging facade.
//
// It follows the SLF4J approach: business code and plugins depend only on
// this package's Logger interface, and the concrete backend is wired up by
// Init (zap by default) or replaced via SetLogger. This package has zero
// framework dependencies and can be used standalone, outside xbc.
//
// # Usage
//
//	cfg := log.DefaultConfig()
//	cfg.Level = "debug"
//	cfg.File.Enabled = true
//	cfg.File.Path = "logs/app.jsonl" // extension determines the format: .jsonl -> json
//	if err := log.Init(cfg); err != nil {
//		panic(err)
//	}
//	defer log.Close()
//
//	ctx := context.Background()
//	log.TInfo(ctx, "service started", "port", 8080, "env", "prod")
//
// Init wires the zap backend from Config. Close flushes and then closes file
// handles and should run on process exit; Sync only flushes, for a long-lived
// process that wants to flush without giving up its handles. Both swallow
// zap's well-known "fsync returns EINVAL against a terminal or pipe" glitch,
// so a deferred Close never reports a false error on ordinary exit.
//
// # Fatal
//
// Fatal logs, flushes, and then terminates the process with exit code 1 --
// it never returns. Flushing is exactly why it belongs on the facade: os.Exit
// runs no deferred calls, and calling zap's own Fatal directly can exit mid
// write, losing the very line that explains why the process died.
//
// Three boundaries apply: Fatal does not run the caller's own defers either,
// so it only suits startup-time failures with nothing left to release --
// return an error instead once the process holds state worth cleaning up.
// Nop() still exits: Nop discards recording, not termination, and a caller
// writing Fatal has already committed to "the next line does not execute."
// Zap(ctx).Fatal(...) also exits, using zap's native semantics without this
// package's flush -- use log.Fatal when that flush is required. The facade
// exposes only debug/info/warn/error/fatal; Panic and DPanic are not provided.
//
// # Tracing
//
// TraceID and RequestID are two encodings of the same 128-bit value: the
// former is the 32-hex-digit form required by W3C, the latter is the
// 26-character Crockford Base32 form used by ULID, whose first 6 bytes are a
// millisecond timestamp -- so request_id is naturally time-ordered and can be
// compared by eye. Span opens a child span and logs one debug line carrying
// elapsed_ms when its done() callback runs.
//
// Interop with an OTel SDK is currently read-only: TraceFrom reads a
// SpanContext an OTel SDK or OTel-based instrumentation already placed into
// ctx when this package's own Trace is absent, but Span never calls into the
// OTel SDK, so spans opened through this package do not appear in a tracing
// backend and never call span.End().
//
// # Masking
//
// Sensitive fields are intercepted and replaced with "***" at the
// zapcore.Core layer, so no call site -- including the Zap() escape hatch --
// can bypass it. The built-in blacklist (password, token, access_token,
// secret, private_key, ak, sk, db_url, dsn, id_card, bank_card, phone, and
// their common variants) is always active and cannot be removed through
// configuration; Config.MaskFields may only append additional field names.
// Field name matching normalizes case and separators first, so accessToken,
// ACCESS_TOKEN, and access-token all hit the same rule, and matching is exact
// per suffix word group rather than by prefix, so phone hits but
// phone_masked and token_count do not.
//
// Replacing the backend with SetLogger also replaces the built-in masking,
// since masking is implemented in this package and a third-party Logger does
// not inherit it; SetLogger writes a warning to stderr on every call for this
// reason.
package log
