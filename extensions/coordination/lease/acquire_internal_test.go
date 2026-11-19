package lease

import (
	"testing"
	"time"
)

// TestRetryDelayStaysWithinTheConfiguredInterval pins the jitter contract the
// exported option promises: every wait is drawn from [interval/2, interval], so
// a caller's configured interval is the ceiling, and the draws actually spread
// instead of collapsing to one value. A thousand draws make the spread check
// effectively deterministic.
func TestRetryDelayStaysWithinTheConfiguredInterval(t *testing.T) {
	intervals := []time.Duration{
		time.Millisecond,
		5 * time.Millisecond,
		50 * time.Millisecond,
		time.Second,
	}
	for _, interval := range intervals {
		lower, upper := interval/2, interval
		spread := false
		for range 1000 {
			delay := retryDelay(interval)
			if delay < lower || delay > upper {
				t.Fatalf("retryDelay(%s) = %s, want it within [%s, %s]", interval, delay, lower, upper)
			}
			if delay < upper {
				spread = true
			}
		}
		if !spread {
			t.Fatalf("retryDelay(%s) produced no jitter in 1000 draws", interval)
		}
	}
}
