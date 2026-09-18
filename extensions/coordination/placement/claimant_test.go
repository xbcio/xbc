package placement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheClaimantIsTheProcessRatherThanTheStoreToken is the operator-facing half
// of a lease decision.
//
// A slot has two names: the process that won it, and the token the store keeps
// in the key. Only the first one can be acted on -- an operator given a token
// has nothing to go and look at -- so the token must not be what a decision
// reports as its claimant while an identity is available. The token is still
// reachable, per slot, because it is what makes a claim found by reading the
// store attributable to this process at all.
func TestTheClaimantIsTheProcessRatherThanTheStoreToken(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)

	decision, err := value.Resolve(instanceRequest("scanner-2", ordinary("sast", 2)))
	require.NoError(t, err)

	assert.Equal(t, "scanner-2", decision.Holder)

	stats := value.Stats()
	assert.Equal(t, "scanner-2", stats.Instance)
	require.Len(t, stats.Held, 1)
	assert.NotEqual(t, "scanner-2", stats.Held[0].Owner,
		"the store's token is its own; reporting the identity as the claimant must not overwrite it")
	assert.Equal(t, locker.ownerOf(stats.Held[0].Key), stats.Held[0].Owner,
		"the token a slot reports is the one the store actually has in the key")
}

// TestAClaimWithNoIdentityStillNamesSomething covers the direct caller: Resolve
// is public and a caller outside a run has no instance to offer.
//
// The token is reported then, rather than nothing. A process that holds slots
// and names no claimant would read in a diagnostic exactly like a static
// decision, which claims nothing at all -- the one reading a report could not
// tell "held by someone I cannot name" from "held by no one".
func TestAClaimWithNoIdentityStillNamesSomething(t *testing.T) {
	locker := newMemoryLocker()
	value := mustNew(t, locker)

	decision, err := value.Resolve(workloadRequest(ordinary("sast", 2)))
	require.NoError(t, err)

	stats := value.Stats()
	require.Len(t, stats.Held, 1)
	assert.Equal(t, stats.Held[0].Owner, decision.Holder,
		"with no identity offered, the claimant falls back to the token")
	assert.NotEmpty(t, decision.Holder)
	assert.Empty(t, stats.Instance, "no identity was offered, and none is invented")
}

// TestAStandbyNamesNoClaimantEvenWhenItHasAnIdentity keeps the identity from
// turning into a claim. Holder answers "who holds this", and a standby holds
// nothing: reporting its identity there would make a process that carries only
// unowned plugins indistinguishable from one that won a slot.
func TestAStandbyNamesNoClaimantEvenWhenItHasAnIdentity(t *testing.T) {
	locker := newMemoryLocker()
	locker.deny = true
	value := mustNew(t, locker)

	decision, err := value.Resolve(instanceRequest("scanner-2", ordinary("sast", 3)))
	require.NoError(t, err)

	assert.Empty(t, decision.Hosted)
	assert.Empty(t, decision.Holder, "a process that holds nothing claims nothing, whatever it is called")

	stats := value.Stats()
	assert.True(t, stats.Standby)
	assert.Equal(t, "scanner-2", stats.Instance,
		"the identity is a property of the process, so a standby still reports it")
}
