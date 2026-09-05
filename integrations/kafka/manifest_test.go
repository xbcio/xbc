package kafka

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleManifestPinsKafkaGoAndHasNoLocalCorePlaceholder(t *testing.T) {
	contents, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("ReadFile(go.mod) error = %v", err)
	}
	manifest := string(contents)
	if !strings.Contains(manifest, "github.com/segmentio/kafka-go v0.4.49") {
		t.Fatal("go.mod does not pin kafka-go v0.4.49")
	}
	for _, line := range strings.Split(manifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "replace ") || strings.HasPrefix(trimmed, "github.com/xbcio/xbc v0.0.0") {
			t.Fatalf("go.mod contains forbidden directive %q", trimmed)
		}
	}
}

func TestOrdinaryImplementationDoesNotDeclareAutoload(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		contents, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		source := string(contents)
		if strings.Contains(source, "internal/autoload") || strings.Contains(source, "autoload.Declare(") {
			t.Fatalf("ordinary implementation file %s declares autoload composition", entry.Name())
		}
	}
}
