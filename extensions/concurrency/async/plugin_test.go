package async

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitPoolRejectsNilArguments(t *testing.T) {
	err := initPool(nil, runtimeContext(newFakeHost(), ""))
	assert.Error(t, err)

	pool := newPool(DefaultConfig(), nil)
	err = initPool(pool, nil)
	assert.Error(t, err)
}

func TestInitPoolOpensAdmissionSoLaterStartCanSpawn(t *testing.T) {
	resetGlobal(t)
	pool := newPool(DefaultConfig(), nil)
	require.NoError(t, initPool(pool, runtimeContext(newFakeHost(), "")))
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	// Admission must already be open immediately after Init, before any
	// Start phase runs, so another plugin's Init/Start can Spawn.
	done := make(chan struct{})
	require.NoError(t, pool.Spawn(context.Background(), "during-init-window", func(context.Context) { close(done) }))
	<-done
}

func TestDefinitionIsStableAcrossCalls(t *testing.T) {
	assert.Equal(t, Definition(), Definition())
}
