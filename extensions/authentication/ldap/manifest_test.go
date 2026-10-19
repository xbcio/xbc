package ldap

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestModuleManifestIsPublishable(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	for _, expected := range []string{
		"module github.com/xbcio/xbc/extensions/authentication/ldap",
		"go 1.25.0",
		"github.com/go-ldap/ldap/v3 v3.4.14",
		"github.com/xbcio/xbc/extensions/authentication v0.2.0",
	} {
		if !strings.Contains(manifest, expected) {
			t.Fatalf("go.mod does not contain %q", expected)
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

func TestModuleImportsOnlyAllowedDependencies(t *testing.T) {
	allowed := []string{
		"github.com/go-ldap/ldap/v3",
		"github.com/xbcio/xbc/extensions/authentication",
		"github.com/xbcio/xbc/extensions/authentication/federation/outbound",
	}
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
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, `"`) || !strings.Contains(line, ".") {
				continue
			}
			path := strings.Trim(line, `"`)
			if !strings.Contains(strings.Split(path, "/")[0], ".") {
				continue // standard library
			}
			permitted := false
			for _, prefix := range allowed {
				if path == prefix || strings.HasPrefix(path, prefix+"/") {
					permitted = true
					break
				}
			}
			if !permitted {
				t.Fatalf("%s imports the third-party package %q, which this module must not depend on", entry.Name(), path)
			}
		}
	}
}

func TestModuleDoesNotImportSiblingProtocolsOrTransports(t *testing.T) {
	forbidden := []string{
		"github.com/xbcio/xbc/extensions/authentication/oidc",
		"github.com/xbcio/xbc/extensions/authentication/oauth2",
		"github.com/xbcio/xbc/plugin",
		"github.com/xbcio/xbc/transport/",
	}
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
		for _, forbiddenPrefix := range forbidden {
			if strings.Contains(string(data), `"`+forbiddenPrefix) {
				t.Fatalf("%s imports or mentions %q", entry.Name(), forbiddenPrefix)
			}
		}
	}
}
