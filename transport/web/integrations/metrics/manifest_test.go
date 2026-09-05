package metrics

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestModuleManifestUsesPinnedPrometheusWithoutLocalSubstitutions(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	pinned := regexp.MustCompile(`(?m)^\s*github\.com/prometheus/client_golang\s+v1\.23\.2\s*$`)
	if !pinned.MatchString(manifest) {
		t.Fatal("go.mod must pin github.com/prometheus/client_golang v1.23.2 exactly")
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).MatchString(manifest) {
		t.Fatal("go.mod must not contain replace directives")
	}
	for _, line := range strings.Split(manifest, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "github.com/xbcio/xbc") &&
			(fields[1] == "v0.0.0" || regexp.MustCompile(`-\d{14}-[0-9a-f]+$`).MatchString(fields[1])) {
			t.Fatalf("go.mod contains unpublished internal dependency: %s", strings.TrimSpace(line))
		}
	}
}
