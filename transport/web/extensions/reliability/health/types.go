package health

import (
	"context"
	"fmt"
	"time"
)

// Kind identifies the operational question a check answers.
type Kind string

const (
	// Liveness asks whether the process is alive and should remain running.
	Liveness Kind = "liveness"
	// Readiness asks whether the instance can currently serve production work.
	Readiness Kind = "readiness"
)

func (k Kind) valid() bool { return k == Liveness || k == Readiness }

// Status is the aggregate or individual health state.
type Status string

const (
	Up   Status = "up"
	Down Status = "down"
)

// DetailPolicy controls whether diagnostic errors may be rendered by a
// protocol adapter. Errors always remain available through the programmatic
// Result API; this policy only governs their external representation.
type DetailPolicy string

const (
	// DetailNever prevents errors from crossing a protocol boundary. It is the
	// default because dependency errors commonly contain internal addresses.
	DetailNever DetailPolicy = "never"
	// DetailAlways allows protocol adapters to include check errors.
	DetailAlways DetailPolicy = "always"
)

// Checker performs one protocol-independent health check.
type Checker interface {
	Check(context.Context) error
}

// CheckFunc adapts a function to Checker.
type CheckFunc func(context.Context) error

// Check invokes f. A nil CheckFunc reports an error rather than panicking.
func (f CheckFunc) Check(ctx context.Context) error {
	if f == nil {
		return fmt.Errorf("health: nil CheckFunc")
	}
	return f(ctx)
}

// NamedChecker is one named contribution. Timeout zero inherits the plugin's
// default timeout; a negative timeout is invalid. A zero Kind means Readiness,
// making the safer dependency-oriented probe the default.
type NamedChecker struct {
	Name    string
	Kind    Kind
	Timeout time.Duration
	Checker Checker
}

// Contributor supplies checks to the health aggregator through a typed Plugin
// contract. Implementations should return a fresh slice or treat the returned
// slice as immutable.
type Contributor interface {
	HealthChecks() []NamedChecker
}

// Result is one completed check. Error is retained for programmatic callers;
// Web output includes it only when DetailAlways is configured.
type Result struct {
	Name    string
	Kind    Kind
	Status  Status
	Latency time.Duration
	Error   error
}

// Report is the stable, name-sorted result of one probe. Status is Down when
// any included check is down and Up otherwise, including when there are no
// checks for the requested Kind.
type Report struct {
	Kind   Kind
	Status Status
	Checks []Result
}

// Healthy reports whether the aggregate status is Up.
func (r Report) Healthy() bool { return r.Status == Up }
