package web

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"sync/atomic"

	"github.com/xbcio/xbc/log"
)

// retryAfterSeconds is the Retry-After value every rejection carries. It is a
// constant rather than an estimate: this gate holds no queue, so there is no
// completion time to predict, and the honest signal to a client is "this is a
// transient refusal, come back shortly" rather than a promise about when a slot
// frees.
const retryAfterSeconds = 1

// problemCodeOverloaded is the machine-readable code on the 503 this gate
// answers with. It names the condition (the process is already serving its
// whole in-flight allowance) rather than the knob that produced it, so a client
// switching on the code does not have to know how the limit was configured.
const problemCodeOverloaded = "server_overloaded"

// inFlightGate is Web's process-level admission gate. It bounds how many
// requests the process serves at once and answers the excess with 503 instead
// of queueing it: the process is already saturated, and a queue in front of a
// saturated process only turns a fast refusal into a slow one while the
// connection, the body buffer, and the deadline are still held.
//
// It is deliberately not a Middleware. Contributed middleware is ordered,
// configurable and optional, and an admission ceiling an application can leave
// out of the chain is not a ceiling. (*Server).Start therefore installs the
// gate as the first element of the framework chain, ahead of capRequestBody,
// ahead of the error boundary, and ahead of every contributed middleware --
// including PhaseRecover. A rejected request runs no plugin code at all and is
// counted by no business metric; the gate keeps its own count instead.
//
// The limit is a process property, not a per-route or per-plugin one. A
// per-plugin ceiling cannot bound the sum an instance accepts, which is the
// quantity that decides whether the instance survives its traffic: pools that
// are each individually reasonable still add up past the machine.
//
// The gate spawns no goroutine. Admission is a non-blocking send on a buffered
// channel and release is the matching receive, so an admitted request costs one
// token and a rejected one costs nothing.
type inFlightGate struct {
	// limit is the effective ceiling, resolved once in (*Server).Start from
	// web.max_in_flight and never changed afterwards.
	limit int
	// slots holds one token per admitted request. Its capacity is exactly
	// limit, so "the channel is full" and "the process is saturated" are one
	// state rather than two that could drift apart.
	slots chan struct{}
	// rejections counts requests refused for want of a slot, so the gate can be
	// seen firing even though it deliberately writes into no business metric.
	rejections atomic.Int64
	// saturated marks a saturation episode the gate has reported and not yet
	// reported the end of. It is what bounds the logging: refusals inside an
	// open episode only add to the count.
	saturated atomic.Bool
	// reported is the rejection total as of the gate's last log line, so a
	// report can name the refusals nobody has seen yet rather than repeating a
	// running total.
	reported atomic.Int64
}

// saturationReport is what one report line carries: the refusals recorded since
// the gate last logged, which is the part an operator has not seen, and the
// total since start, which is what makes two lines comparable.
type saturationReport struct {
	sinceLastReport int64
	total           int64
}

// newInFlightGate builds a gate around an already-resolved positive ceiling.
// Refusing to admit anything is never the intent -- a limit of zero would make
// the process permanently unavailable -- so the caller must pass a resolved
// value; resolveMaxInFlight is the only such source.
func newInFlightGate(limit int) *inFlightGate {
	return &inFlightGate{limit: limit, slots: make(chan struct{}, limit)}
}

// acquire takes one slot without blocking. A false return means limit requests
// are already being served and the caller must refuse this one without running
// anything downstream.
func (g *inFlightGate) acquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns the slot acquire took. The admitted path reaches it through a
// defer, so a handler that panics releases exactly as one that returns: the
// panic boundary that recovers it is downstream of this gate -- PhaseRecover
// is a middleware, and this is outside every middleware -- and a slot lost to a
// panic would narrow the process permanently, one lost slot per panic.
//
// It is only valid after a successful acquire: the receive blocks when the gate
// is empty, and the acquire/defer pair is what guarantees the token is there.
func (g *inFlightGate) release() { <-g.slots }

// openSaturation records a refused request and elects the single refusal that
// reports the episode: the caller logs only when opened is true.
//
// The count and the reporting duty are deliberately separated. Logging every
// refusal makes the log volume proportional to the rejection count exactly when
// the process can least afford it -- saturation is the moment the gate exists to
// protect, and a saturated process would then spend another round of CPU and IO
// on describing its own refusals. What an operator needs instead is the two
// events: refusals started, refusals stopped. The count in between is already
// aggregable, from the report lines and from Server.InFlightStats.
//
// The first refusal of an episode is always visible, and immediately: the flag
// transition and the duty to log are one atomic operation, so exactly one
// goroutine wins the false->true edge and it logs synchronously, before the 503
// it is about to write. A design that read the flag and then set it could elect
// two reporters, and one that set the flag before deciding to log could elect
// none.
func (g *inFlightGate) openSaturation() (saturationReport, bool) {
	g.rejections.Add(1)
	if !g.saturated.CompareAndSwap(false, true) {
		return saturationReport{}, false
	}
	return g.report(), true
}

// closeSaturation ends an open episode once a release has left no request in
// flight, and reports the refusals the episode never logged.
//
// Draining completely -- rather than merely dropping below the ceiling -- is
// what keeps a process parked at its ceiling from alternating between the two
// report lines: entering at full occupancy and leaving at empty is the widest
// hysteresis band the gate can apply, and it needs neither a timer goroutine nor
// a clock to apply it. Its cost is that a process which stays busy through a
// lull reports one episode where an operator might have counted two; the live
// reading in between is Server.InFlightStats.
//
// An episode cannot open without at least one request in flight -- a refusal
// means the slots are all taken -- so every episode is followed by a release
// that can close it.
func (g *inFlightGate) closeSaturation() (saturationReport, bool) {
	// Two cheap reads keep the admitted path off a read-modify-write: on all but
	// the release that ends an episode, the gate is still occupied or was never
	// marked saturated.
	if len(g.slots) != 0 || !g.saturated.Load() {
		return saturationReport{}, false
	}
	if !g.saturated.CompareAndSwap(true, false) {
		return saturationReport{}, false
	}
	return g.report(), true
}

// report claims the refusals recorded since the gate's last line and marks them
// reported, so no report repeats another's numbers. A report that raced a
// concurrent one claims zero rather than a negative count.
func (g *inFlightGate) report() saturationReport {
	total := g.rejections.Load()
	previous := g.reported.Swap(total)
	if total <= previous {
		return saturationReport{total: total}
	}
	return saturationReport{sinceLastReport: total - previous, total: total}
}

// stats reports one consistent snapshot of the gate. InFlight is derived from
// the channel itself rather than from a second counter, so it cannot disagree
// with the slot count the gate actually enforces.
func (g *inFlightGate) stats() InFlightStats {
	return InFlightStats{
		Limit:      g.limit,
		InFlight:   len(g.slots),
		Rejections: g.rejections.Load(),
	}
}

// handler returns the gate's chain element. It is installed by (*Server).Start
// as the first handler of the framework chain, so it runs before any contributed
// middleware, before the error boundary, and before the body cap.
//
// The admitted path calls Next itself and releases when the rest of the chain
// has finished, which is what makes the slot cover the whole request rather than
// only this handler. Releasing on return instead would free the slot while the
// request it belongs to is still being served, and the gate would then bound
// nothing.
//
// The refusal goes through the same Problem machinery as the rest of Web. The
// outermost chain element still receives a *Ctx, so AbortProblem renders an RFC
// 9457 503 with a machine-readable code and a Retry-After header rather than a
// bare status line. What it cannot reach from here are the stages that make a
// response attributable -- the resolver the second handler attaches, request-id
// and access-log middleware -- because every one of them is downstream, and a
// contributed ErrorMapper is inside the error boundary and sees nothing at all.
// That is the price of refusing before any of them run, and it is why the gate
// keeps its own count and reports the episodes itself.
//
// It reports episodes rather than refusals: the refusal that finds the gate
// newly saturated logs at warn, the release that finds the process drained again
// logs at info, and every refusal in between only adds to the count the next
// line carries. See openSaturation for why that is the bound and why the first
// line of an episode cannot be lost.
func (g *inFlightGate) handler(logger log.Logger) Handler {
	return func(_ context.Context, c *Ctx) error {
		if !g.acquire() {
			if report, opened := g.openSaturation(); opened {
				logger.Warn("web: in-flight limit reached, refusing requests until in-flight work drains",
					"limit", g.limit,
					"rejections", report.sinceLastReport,
					"rejections_total", report.total,
					"retry_after_seconds", retryAfterSeconds,
				)
			}
			c.SetHeader("Retry-After", strconv.Itoa(retryAfterSeconds))
			AbortProblem(c, NewProblem(http.StatusServiceUnavailable, problemCodeOverloaded))
			return nil
		}
		defer func() {
			g.release()
			if report, closed := g.closeSaturation(); closed {
				logger.Info("web: in-flight limit cleared, admitting requests again",
					"limit", g.limit,
					"rejections", report.sinceLastReport,
					"rejections_total", report.total,
				)
			}
		}()
		c.Next()
		return nil
	}
}

// resolveMaxInFlight turns web.max_in_flight into the ceiling the gate enforces.
// A positive value is what the operator asked for, verbatim. Zero -- the default
// -- derives the ceiling from GOMAXPROCS, which is how much work this process
// can actually run at once, so a deployment that never configures the key still
// gets a finite ceiling instead of an unbounded one.
//
// The derivation belongs here rather than in an init function because the
// runtime applies its own GOMAXPROCS during bootstrap (cgroup-aware), and this
// runs in (*Server).Start, after it: the number read is the one the process is
// really running under, not the host's core count.
//
// Config.Validate rejects a negative value before Start, and GOMAXPROCS is never
// below one, so the result is always a usable ceiling.
func resolveMaxInFlight(configured int) int {
	if configured > 0 {
		return configured
	}
	return runtime.GOMAXPROCS(0)
}

// renderInFlightGate renders the gate for an operator: the ceiling in force,
// where it came from, the Retry-After a refusal carries, and the rejection count
// so far. Start logs it once the gate is assembled, where the count is
// necessarily zero; the live reading for an operator afterwards is
// Server.InFlightStats.
func renderInFlightGate(stats InFlightStats, derived bool) string {
	origin := "web.max_in_flight"
	if derived {
		origin = "derived from GOMAXPROCS"
	}
	return fmt.Sprintf(
		"web: in-flight gate limit=%d (%s) retry_after=%ds rejections=%d",
		stats.Limit,
		origin,
		retryAfterSeconds,
		stats.Rejections,
	)
}

// InFlightStats is a snapshot of the admission gate Server owns. The three
// readings are one value rather than three accessors because they are only
// meaningful together: a rejection count without the ceiling it was measured
// against cannot tell an operator whether the gate is doing its job or the
// ceiling is simply too low, and occupancy is what shows the ceiling being
// approached.
//
// The zero value is the honest answer before Start has assembled a gate, and
// Limit is the effective ceiling -- the derived one when web.max_in_flight is 0.
type InFlightStats struct {
	Limit      int
	InFlight   int
	Rejections int64
}

// InFlightStats reports the admission gate's effective limit, its current
// occupancy, and how many requests it has refused. It is the read side of a
// gate that deliberately writes into no business metric: a refused request
// never reaches the middleware that would have counted it.
func (s *Server) InFlightStats() InFlightStats {
	s.mu.Lock()
	gate := s.inflight
	s.mu.Unlock()
	if gate == nil {
		return InFlightStats{}
	}
	return gate.stats()
}
