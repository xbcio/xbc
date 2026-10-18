package oidc

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestModuleManifestIsPublishableAndPinsReleasedTags(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	for _, expected := range []string{
		"go 1.25.0",
		"github.com/go-jose/go-jose/v4 v4.1.6",
		"github.com/xbcio/xbc v0.1.0",
		"github.com/xbcio/xbc/extensions/authentication v0.2.0",
		"github.com/xbcio/xbc/extensions/authentication/oidc v0.1.0",
		"github.com/xbcio/xbc/extensions/authentication/session v0.2.0",
		"github.com/xbcio/xbc/transport/web v0.2.0",
		"github.com/xbcio/xbc/transport/web/extensions/authentication/federation v0.1.0",
	} {
		if !strings.Contains(manifest, expected) {
			t.Fatalf("go.mod does not contain exact dependency %q", expected)
		}
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).MatchString(manifest) {
		t.Fatal("go.mod contains a local replace")
	}
	pseudo := regexp.MustCompile(`-\d{14}-[0-9a-f]+$`)
	for _, line := range strings.Split(manifest, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "github.com/xbcio/xbc") && (fields[1] == "v0.0.0" || pseudo.MatchString(fields[1])) {
			t.Fatalf("go.mod contains unpublished XBC dependency %q", strings.TrimSpace(line))
		}
	}
}

// TestModuleImportsOnlyItsOwnProtocol pins the boundary between protocol
// modules: the adapter consumes the neutral OIDC client and the shared
// federation base, never a sibling protocol implementation.
func TestModuleImportsOnlyItsOwnProtocol(t *testing.T) {
	forbidden := []string{
		"github.com/xbcio/xbc/extensions/authentication/oauth2",
		"github.com/xbcio/xbc/extensions/authentication/saml",
		"github.com/xbcio/xbc/extensions/authentication/ldap",
		"github.com/xbcio/xbc/transport/web/extensions/authentication/oauth2",
		"github.com/xbcio/xbc/transport/web/extensions/authentication/saml",
	}
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, prefix := range forbidden {
				if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
					t.Errorf("%s imports the sibling protocol module %q", path, importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAutoloadDeclarationExistsOnlyInAutoloadPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "autoload.Declare") {
			t.Fatalf("ordinary implementation file %s registers globally", entry.Name())
		}
	}
}
