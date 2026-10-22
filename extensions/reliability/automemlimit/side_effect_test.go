package automemlimit_test

import (
	"reflect"
	"runtime/debug"
	"runtime/metrics"
	"testing"

	"github.com/xbcio/xbc/extensions/reliability/automemlimit"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/autoload"
)

const softLimitMetric = "/gc/gomemlimit:bytes"

// readSoftLimit observes the process soft memory limit without changing it.
func readSoftLimit(t *testing.T) uint64 {
	t.Helper()
	samples := []metrics.Sample{{Name: softLimitMetric}}
	metrics.Read(samples)
	if kind := samples[0].Value.Kind(); kind != metrics.KindUint64 {
		t.Fatalf("runtime metric %s has kind %v, want %v", softLimitMetric, kind, metrics.KindUint64)
	}
	return samples[0].Value.Uint64()
}

// TestOrdinaryImportDefinitionAndBundleHaveNoSideEffects is the guard against
// moving the installation out of the lifecycle: importing the package and
// evaluating its declarations must not touch the process soft memory limit,
// and it must not join the autoload composition.
//
// A sentinel limit is installed first, so the assertion is about this
// evaluation rather than about whatever limit the host happened to have: any
// successful install performed while evaluating a declaration shows up as a
// changed value, not merely as a value that differed from an ambient one.
func TestOrdinaryImportDefinitionAndBundleHaveNoSideEffects(t *testing.T) {
	const sentinel = int64(64 << 20)
	previous := debug.SetMemoryLimit(sentinel)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })

	before := readSoftLimit(t)
	if before != uint64(sentinel) {
		t.Fatalf("sentinel soft limit = %d, want %d", before, sentinel)
	}

	var zeroDefinition plugin.Definition
	if automemlimit.Definition() == zeroDefinition || automemlimit.Definition() != automemlimit.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := automemlimit.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, automemlimit.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
	if defaults := automemlimit.DefaultConfig(); defaults.Ratio != 0.9 || defaults.Fallback != "none" {
		t.Fatalf("DefaultConfig() = %#v, want the documented defaults", defaults)
	}

	after := readSoftLimit(t)
	if after != before {
		t.Fatalf("soft limit changed from %d to %d while only declarations were evaluated", before, after)
	}
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary import or Bundle() mutated the autoload composition")
	}
}
