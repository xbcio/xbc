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
// After Close, the facade's global Logger -- obtained via L() from that
// point on -- records to nothing: Close swaps the global to Nop() once its
// sinks are closed (unless a third-party backend was installed via
// SetLogger in the meantime, which Close leaves alone). The same happens
// across a second Init call: closing the previous round's file sinks never
// leaves them reachable through the new global either.
//
// Caching a Logger value (an earlier L() result, or anything derived from
// it via With) across a Close or a second Init is not supported. Once the
// sink behind that cached value has been closed, writes through it are
// discarded and reported as an error through zap's ErrorOutput (stderr by
// default) rather than silently lost -- but they do not resurrect the
// closed file. This is deliberate: without it, a cached Logger writing
// after Close would make lumberjack transparently reopen the very file
// Close just told it to close, growing a file callers were told had been
// shut down.
//
// # Process scope
//
// The facade is process-global: Init installs one backend that every L()
// caller in the process reaches, and Close replaces it for all of them. One
// process therefore hosts one application, because a second one would share
// this logger and each Init and Close would change the other's output. Where a
// second logger is genuinely needed, NewContext binds one to a context and Ctx
// retrieves it -- that path is per-context rather than global, and is how the
// framework's own request handling carries a logger with trace fields. Ctx
// falls back to the global when no logger was bound to the context.
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
//
// # Attached sinks
//
// Attach(core) adds an extra zapcore.Core to the backend Init assembled and
// returns a detach function; an application that needs a sink the config does
// not cover -- shipping logs to Kafka, say -- attaches it from a plugin's
// Start and detaches it from Stop. An attached core is treated like a built-in
// sink: it carries the same masking layer as console and file records pass
// through (so it can never see an unmasked sensitive field), it receives only
// records the global level admits -- including a level moved later by SetLevel
// -- and its own Enabled, and it sits under the sampler, so a record sampling
// drops reaches no attached core either. Delivery is synchronous on the
// caller's goroutine.
//
// Loggers derived before the attachment keep reaching it. This works because
// the seam is a core inside the assembled backend that resolves the current
// attachment set at write time, re-applying the fields a derived logger's With
// calls had fixed; a logger a plugin captured at construction therefore
// delivers to a core attached later without re-deriving anything.
//
// An attached core whose own Write path logs through this package (a Kafka
// client reporting a publish failure, say) does not recurse or amplify: a
// reentrant record on the same goroutine still reaches console and file, but
// is not delivered to the attached set it came from.
//
// Detach is idempotent. After it returns, no new record reaches that core; a
// delivery already in progress may still complete, and detach does not wait
// for writers. Attachments follow the backend across Init re-entry: a later
// Init keeps them and re-wraps each with the new configuration's masker, and
// Close flushes them with the built-in sinks without detaching them. With
// every built-in sink disabled the backend is equivalent to Nop until
// something is attached -- and a core attached afterwards still receives
// records, which is what an application that logs only to its own sink
// relies on. Attachments, like the backend itself, are process-global, and a
// backend installed via SetLogger carries no seam.
//
// A record logged through the facade costs nothing extra while the seam is
// empty, but deriving a logger with With does allocate: at most four objects
// more than the same derivation on the pre-seam backend. Two of them belong
// to zap -- a Tee of two or more cores wraps them in zap's multiCore, whose
// With allocates a fresh children slice and boxes it, while a Tee of one
// returns that core directly -- and two are the seam's: a copy of the
// derivation fields, which is what keeps With's capture-at-derivation
// semantics intact for a caller that reuses and mutates a slice it passed,
// and the boxed record carrying that copy.
package log
