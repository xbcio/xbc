package automemlimit

import (
	"fmt"
	"math"
	"runtime/metrics"

	"github.com/KimMachineGun/automemlimit/memlimit"
)

// softLimitMetric is the standard library's read-only view of the soft memory
// limit in force. Reading it is how the conflict check sees a limit another
// owner installed; nothing in this package writes the limit itself, because
// only the automemlimit library may install it.
const softLimitMetric = "/gc/gomemlimit:bytes"

// newProvider assembles the configured provider from the library's two
// sources.
func newProvider(cfg normalizedConfig) memlimit.Provider {
	return providerFrom(cfg, memlimit.FromCgroup, memlimit.FromSystem)
}

// providerFrom is newProvider with its two sources as parameters, so the
// fallback selection is testable without depending on whether the machine
// running the test has cgroups.
func providerFrom(cfg normalizedConfig, fromCgroup, fromSystem memlimit.Provider) memlimit.Provider {
	if cfg.fallback == fallbackSystem {
		// The library's ApplyFallback definition: any primary error falls back.
		// That includes a cgroup read error, which is why "system" is intended
		// for bare metal rather than containers.
		return memlimit.ApplyFallback(fromCgroup, fromSystem)
	}
	return fromCgroup
}

// currentSoftLimit reads the soft memory limit currently installed in this
// process. An unavailable metric is an error rather than a guess, because the
// conflict check must fail safe instead of overwriting a limit it could not
// see.
func currentSoftLimit() (int64, error) {
	samples := []metrics.Sample{{Name: softLimitMetric}}
	metrics.Read(samples)

	sample := samples[0]
	if sample.Value.Kind() != metrics.KindUint64 {
		return 0, fmt.Errorf(
			"automemlimit: runtime metric %s has kind %v, want %v; refusing to guess whether a soft memory limit is already installed",
			softLimitMetric, sample.Value.Kind(), metrics.KindUint64)
	}
	value := sample.Value.Uint64()
	if value > math.MaxInt64 {
		return 0, fmt.Errorf(
			"automemlimit: runtime metric %s reports %d bytes, beyond a signed 64-bit limit", softLimitMetric, value)
	}
	return int64(value), nil
}
