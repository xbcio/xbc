package architecture_test

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestArchReleaseManifestIsCurrent regenerates the repository's release
// manifest and compares it with the checked-in one. The manifest is the
// release-train order: modules are tagged wave by wave, and a module may only
// gain a sibling requirement once every module it imports has a published
// tag. A new module or a new cross-module import therefore changes the tag
// order, and this guard makes that change explicit instead of letting it
// enter unnoticed.
func TestArchReleaseManifestIsCurrent(t *testing.T) {
	root := archRepositoryRoot(t)
	command := exec.Command("go", "run", "./scripts/release-manifest", "-check")
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "release manifest check failed:\n%s", output)
}
