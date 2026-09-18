package web

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInFlightGateAdmitsUpToItsLimitAndRefusesWithoutBlocking pins the whole
// acquire/release contract at the level the request path cannot observe: a full
// gate answers false immediately instead of waiting for a slot, and one release
// frees exactly one slot. A blocking acquire here would be invisible in
// production until the process stopped answering entirely -- the request that
// should have been refused would simply hang until something upstream timed it
// out.
func TestInFlightGateAdmitsUpToItsLimitAndRefusesWithoutBlocking(t *testing.T) {
	gate := newInFlightGate(2)

	require.True(t, gate.acquire())
	require.True(t, gate.acquire())
	assert.False(t, gate.acquire(), "a saturated gate must refuse rather than block")
	assert.Equal(t, InFlightStats{Limit: 2, InFlight: 2}, gate.stats(),
		"a refused request must not occupy a slot or be counted as a rejection it did not record")

	gate.release()
	assert.True(t, gate.acquire(), "one release must free exactly one slot")

	gate.release()
	gate.release()
	assert.Equal(t, InFlightStats{Limit: 2}, gate.stats(),
		"every admitted request must be matched by exactly one release")
}

// TestResolveMaxInFlightIsPositiveAndDerivesFromGOMAXPROCS pins both halves of
// the default. Zero is the documented "derive it" value rather than "no limit"
// or "refuse everything", and a configured value is used verbatim -- including
// one that differs from what the process could run at once, since an operator
// who names a ceiling owns that choice.
func TestResolveMaxInFlightIsPositiveAndDerivesFromGOMAXPROCS(t *testing.T) {
	assert.Equal(t, runtime.GOMAXPROCS(0), resolveMaxInFlight(0),
		"the zero default is GOMAXPROCS, which is what this process can actually run at once")
	assert.GreaterOrEqual(t, resolveMaxInFlight(0), 1,
		"a derived ceiling of zero would make the process permanently unavailable")

	assert.Equal(t, 7, resolveMaxInFlight(7))
	assert.Equal(t, runtime.GOMAXPROCS(0)+1, resolveMaxInFlight(runtime.GOMAXPROCS(0)+1),
		"a configured ceiling above the core count is still honoured verbatim")
}

// TestRenderInFlightGateReportsTheOperatorsNumbers pins the startup line's
// content. It is the only place a deployment learns which ceiling is in force
// and where it came from, so the two origins must not render identically.
func TestRenderInFlightGateReportsTheOperatorsNumbers(t *testing.T) {
	derived := renderInFlightGate(InFlightStats{Limit: 8}, true)
	assert.Contains(t, derived, "limit=8")
	assert.Contains(t, derived, "GOMAXPROCS")
	assert.Contains(t, derived, "retry_after=1s")

	configured := renderInFlightGate(InFlightStats{Limit: 8, Rejections: 3}, false)
	assert.Contains(t, configured, "web.max_in_flight")
	assert.Contains(t, configured, "rejections=3")
	assert.NotContains(t, configured, "GOMAXPROCS")
}

// TestSaturationIsReportedOnceAtEachEdgeOfAnEpisode pins the state machine that
// bounds the gate's logging: one report when refusals start, one when the
// process has drained, and none at all for the refusals in between. Reporting
// per refusal is what this replaces -- it made the log volume proportional to
// the rejection count precisely when the process had no capacity to spare.
//
// The counts are asserted too, because a bounded report that loses refusals is
// no better than none: what the two lines carry between them must add up to
// every refusal the gate recorded.
func TestSaturationIsReportedOnceAtEachEdgeOfAnEpisode(t *testing.T) {
	gate := newInFlightGate(2)
	require.True(t, gate.acquire())
	require.True(t, gate.acquire())

	report, opened := gate.openSaturation()
	require.True(t, opened, "the first refusal of an episode must be the one that reports it")
	assert.Equal(t, saturationReport{sinceLastReport: 1, total: 1}, report)

	for range 3 {
		_, opened := gate.openSaturation()
		assert.False(t, opened, "a refusal inside an open episode must only add to the count")
	}
	assert.Equal(t, int64(4), gate.stats().Rejections,
		"a refusal the gate does not report must still be counted")

	gate.release()
	_, closed := gate.closeSaturation()
	assert.False(t, closed, "an episode must not be closed while requests are still in flight")

	gate.release()
	recovery, closed := gate.closeSaturation()
	require.True(t, closed, "draining to no requests in flight must close the episode")
	assert.Equal(t, saturationReport{sinceLastReport: 3, total: 4}, recovery,
		"the closing line must carry exactly the refusals the opening line did not")

	_, closed = gate.closeSaturation()
	assert.False(t, closed, "an episode must be closed once, not on every release afterwards")
}

// TestSaturationReportsRearmAfterRecovery pins the other half of the bound: it
// is one pair of lines per episode, not one pair per process. An operator who
// missed the first warn must still learn that refusals resumed, so closing an
// episode has to leave the gate able to report the next one.
func TestSaturationReportsRearmAfterRecovery(t *testing.T) {
	gate := newInFlightGate(1)

	require.True(t, gate.acquire())
	_, opened := gate.openSaturation()
	require.True(t, opened)
	gate.release()
	_, closed := gate.closeSaturation()
	require.True(t, closed)

	require.True(t, gate.acquire())
	report, opened := gate.openSaturation()
	require.True(t, opened, "the first refusal after a recovery must open a new episode")
	assert.Equal(t, saturationReport{sinceLastReport: 1, total: 2}, report,
		"a new episode reports its own refusals, not ones already reported")
}

// TestConcurrentRefusalsElectExactlyOneReporter pins the election itself. The
// opening line is the only signal an operator gets at the moment refusals start,
// so a burst that arrives all at once must neither lose it -- leaving saturation
// silent -- nor hand it to every refusal, which is the noise this replaces.
func TestConcurrentRefusalsElectExactlyOneReporter(t *testing.T) {
	const refusals = 64

	gate := newInFlightGate(1)
	require.True(t, gate.acquire())

	var reporters atomic.Int64
	var opening saturationReport
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for range refusals {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if report, opened := gate.openSaturation(); opened {
				// Only the elected refusal reaches this, so the write races
				// nothing, and done.Wait below publishes it.
				opening = report
				reporters.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()

	assert.Equal(t, int64(1), reporters.Load(),
		"a burst of refusals must elect exactly one reporter for the episode")
	assert.Equal(t, int64(refusals), gate.stats().Rejections)
	assert.Positive(t, opening.sinceLastReport,
		"the opening line must claim at least the refusal that elected it")

	gate.release()
	recovery, closed := gate.closeSaturation()
	require.True(t, closed)
	assert.Equal(t, int64(refusals), recovery.total)
	assert.Equal(t, int64(refusals), opening.sinceLastReport+recovery.sinceLastReport,
		"the two lines of an episode must account for every refusal exactly once")
}
