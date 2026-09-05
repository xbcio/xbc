package objectstorage

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleManifestPinsSDKWithoutLocalCorePlaceholder(t *testing.T) {
	contents, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("ReadFile(go.mod) error = %v", err)
	}
	manifest := string(contents)
	for _, dependency := range []string{
		"github.com/aws/aws-sdk-go-v2 v1.41.0",
		"github.com/aws/aws-sdk-go-v2/credentials v1.19.6",
		"github.com/aws/aws-sdk-go-v2/service/s3 v1.95.0",
		"github.com/aws/smithy-go v1.24.0",
	} {
		if !strings.Contains(manifest, dependency) {
			t.Fatalf("go.mod does not pin %q", dependency)
		}
	}
	for _, line := range strings.Split(manifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "replace ") || strings.HasPrefix(trimmed, "github.com/xbcio/xbc ") {
			t.Fatalf("go.mod contains forbidden internal dependency directive %q", trimmed)
		}
	}
}

func TestOrdinaryImplementationDoesNotDeclareAutoloadBundles(t *testing.T) {
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
			t.Fatalf("ordinary implementation file %s declares an autoload Bundle", entry.Name())
		}
	}
}

func TestProductionSourcesDoNotUseRetiredPluginAPIs(t *testing.T) {
	forbidden := []string{
		"plugin.Base",
		"plugin.Plugin",
		"ConfigPtr(",
		"Dependencies(",
		"Provides(",
		"plugin.Provide",
		"plugin.Get",
		"plugin.MustGet",
		"plugin.Extensions",
		"plugin/catalog",
	}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, retired := range forbidden {
			if strings.Contains(string(contents), retired) {
				t.Errorf("production source %s contains retired API %q", path, retired)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir() error = %v", err)
	}
}
