package placement

import (
	cryptorand "crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zeroEntropyReader answers every read with zero bytes and no error.
//
// It is how "the system's entropy contributed nothing" is spelled for a test.
// crypto/rand.Read cannot be made to report a failure a test can observe -- it
// takes the process down rather than returning the error -- so the defect's
// effect, a seed built out of nothing, is reproduced by a source that yields no
// entropy instead of one that fails. The read still succeeds, so the seed is
// whatever the code makes of eight zero bytes.
type zeroEntropyReader struct{}

func (zeroEntropyReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// TestSlotSearchSpreadsEvenWithoutEntropy pins the seed a Placement's generator
// is built from.
//
// The random starting index exists so that standbys do not all race the same
// slot and lose the same way for ever. A seed drawn only from the system's
// entropy source collapses exactly that: with no entropy every Placement in
// every process would draw from the same stream and start its search at the
// same index -- the correlation the offset exists to break, and one that stays
// invisible until a takeover depends on it. The generator therefore mixes in a
// process-local counter and the clock as well, and this test runs with the
// entropy source contributing nothing, so the counter and the clock are all
// that is left to tell one Placement from the next.
//
// crypto/rand.Reader is process-global and is restored before the test returns;
// this package runs no test in parallel, so no other test can observe it
// substituted.
func TestSlotSearchSpreadsEvenWithoutEntropy(t *testing.T) {
	previous := cryptorand.Reader
	cryptorand.Reader = zeroEntropyReader{}
	t.Cleanup(func() { cryptorand.Reader = previous })

	const (
		rounds   = 50
		replicas = 4
	)
	locker := newMemoryLocker()
	locker.deny = true

	for round := 0; round < rounds; round++ {
		value := mustNew(t, locker)
		_, err := value.Resolve(workloadRequest(ordinary("offset", replicas)))
		require.NoError(t, err)
	}

	attempts := locker.attempts()
	require.Len(t, attempts, rounds*replicas, "every round attempts every slot exactly once")

	// The first attempt of a round is the slot its search started at, the same
	// observation TestSlotSearchStartsAtARandomOffset makes.
	starts := make(map[string]bool)
	for round := 0; round < rounds; round++ {
		key := attempts[round*replicas]
		starts[key[len("xbc:workload:offset:"):]] = true
	}
	assert.Greater(t, len(starts), 1,
		"every Placement started its search at the same slot (%v); a seed built from no entropy makes every standby race the same one", starts)
}
