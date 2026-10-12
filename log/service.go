package log

import (
	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// ServiceIdentity is the process identity the runtime attaches to the logging
// configuration before Init. Its three members are stamped onto every record
// a JSON-encoded sink writes as service.name, service.version, and
// service.instance.id -- the OpenTelemetry resource keys, the same names the
// tracing resource uses -- so an operator can select one deployment's
// machine-readable lines out of a shared collector without every call site
// repeating the fields, and so the fields cannot collide with the short keys
// a call site writes for its own purposes ("service" for an RPC service,
// "instance" for a plugin instance), which would produce duplicate JSON keys.
//
// It is deliberately not a configuration key: a deployment never writes it in
// YAML, the runtime fills it from xbc.service and xbc.instance_id, and
// leaving it zero (a library embedding, a test) keeps the JSON output exactly
// as it was before this existed. Console output never carries it -- console
// is for a human reading one process, where three repeated fields per line
// are noise; see the JSON-vs-console split in zap.go's Init.
type ServiceIdentity struct {
	Name     string
	Version  string
	Instance string
}

// populated reports whether the identity carries anything worth stamping. A
// zero identity leaves the encoders exactly as they always were, which is
// what keeps a caller that never sets it -- log's own tests included --
// byte-for-byte unaffected.
func (s ServiceIdentity) populated() bool {
	return s.Name != "" || s.Version != "" || s.Instance != ""
}

// identityEncoder wraps a JSON encoder and prepends the process identity to
// every entry's fields.
//
// It works at the encoder rather than the logger because the rule is
// per-sink, not per-logger: one Logger fans out to a JSON file sink and a
// console stdout sink, and only the JSON half stamps the identity. A
// logger.With would have no way to tell the two apart.
//
// The identity fields precede the call site's own fields in the encoded
// record -- zap writes its built-in keys (level, ts, caller, msg) first, then
// whatever fields it was handed, and this type hands over the identity ahead
// of the entry's -- so they read as a stable block: grep for
// `"service.name":"orders"` selects one deployment's lines without the number
// of contextual fields changing what the line looks like.
type identityEncoder struct {
	zapcore.Encoder
	fields []zapcore.Field
}

func newIdentityEncoder(encoder zapcore.Encoder, identity ServiceIdentity) zapcore.Encoder {
	return identityEncoder{
		Encoder: encoder,
		fields: []zapcore.Field{
			zap.String("service.name", identity.Name),
			zap.String("service.version", identity.Version),
			zap.String("service.instance.id", identity.Instance),
		},
	}
}

// Clone implements zapcore.Encoder: the clone reuses the same immutable
// field slice, so a derived entry never reallocates it.
func (e identityEncoder) Clone() zapcore.Encoder {
	return identityEncoder{Encoder: e.Encoder.Clone(), fields: e.fields}
}

// EncodeEntry implements zapcore.Encoder: the identity fields are prepended
// to this entry's own. The combined slice is freshly allocated because the
// caller owns its fields slice and append could otherwise write into spare
// capacity the caller still intends to reuse.
func (e identityEncoder) EncodeEntry(entry zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	combined := make([]zapcore.Field, 0, len(e.fields)+len(fields))
	combined = append(combined, e.fields...)
	combined = append(combined, fields...)
	return e.Encoder.EncodeEntry(entry, combined)
}
