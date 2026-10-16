package biz

// Config is the business response contract configuration. Composing biz.Bundle
// is the opt-in to the success envelope and the business error mapper; the one
// switch below additionally moves failures onto the envelope.
type Config struct {
	// FailureEnvelope renders every failure response the Server writes as the
	// same {success, code, message, data, requestId} envelope the success path
	// uses. It is off by default: failures stay RFC 9457 Problem Details, and a
	// failure that carries no machine code is always left as one. The HTTP
	// status is never affected.
	FailureEnvelope bool `yaml:"failure_envelope" default:"false"`
}

// DefaultConfig returns the default business contract configuration: failures
// keep the RFC 9457 Problem Detail body.
func DefaultConfig() Config { return Config{} }
