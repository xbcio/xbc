package tracing

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestModuleManifestPinsOpenTelemetryWithoutLocalSubstitutions(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	modules := map[string]string{
		"go.opentelemetry.io/otel": "v1.45.0",
		"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc": "v1.44.0",
		"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp": "v1.44.0",
		"go.opentelemetry.io/otel/sdk":                                    "v1.44.0",
		"go.opentelemetry.io/otel/trace":                                  "v1.45.0",
	}
	for module, version := range modules {
		pattern := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(module) + `\s+` + regexp.QuoteMeta(version) + `(?:\s*//.*)?$`)
		if !pattern.MatchString(manifest) {
			t.Errorf("go.mod must pin %s %s exactly", module, version)
		}
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).MatchString(manifest) {
		t.Fatal("go.mod must not contain replace directives")
	}
	pseudoVersion := regexp.MustCompile(`-\d{14}-[0-9a-f]+$`)
	for _, line := range strings.Split(manifest, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "github.com/xbcio/xbc") &&
			(fields[1] == "v0.0.0" || pseudoVersion.MatchString(fields[1])) {
			t.Fatalf("go.mod contains unpublished internal dependency: %s", strings.TrimSpace(line))
		}
	}
}
