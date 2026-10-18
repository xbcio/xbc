package federation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testExternalIdentity returns a fully populated identity. The fixed zone
// proves SessionAttributes normalizes auth_time to UTC.
func testExternalIdentity() ExternalIdentity {
	return ExternalIdentity{
		Protocol: "oidc",
		Provider: "corp",
		Issuer:   "https://issuer.test",
		Subject:  "external-1",
		AuthTime: time.Date(2026, 10, 18, 12, 30, 0, 0, time.FixedZone("CEST", 2*60*60)),
	}
}

func TestReservedAttributeKeys(t *testing.T) {
	for key, want := range map[string]string{
		AttributeProtocol: "idp_protocol",
		AttributeProvider: "idp",
		AttributeIssuer:   "idp_issuer",
		AttributeAuthTime: "auth_time",
	} {
		if key != want {
			t.Fatalf("reserved key = %q, want %q", key, want)
		}
		if !IsReservedAttribute(key) {
			t.Fatalf("IsReservedAttribute(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"", "role", "Idp", "auth_time ", "idp_protocol2"} {
		if IsReservedAttribute(key) {
			t.Fatalf("IsReservedAttribute(%q) = true, want false", key)
		}
	}
}

func TestSessionAttributesMergesReservedAttributes(t *testing.T) {
	identity := testExternalIdentity()
	attributes, err := SessionAttributes(identity, map[string]any{"role": "admin", "level": 3})
	if err != nil {
		t.Fatalf("SessionAttributes() error = %v", err)
	}
	if attributes["role"] != "admin" || attributes["level"] != 3 {
		t.Fatalf("application attributes missing: %v", attributes)
	}
	if attributes[AttributeProtocol] != "oidc" {
		t.Fatalf("%s = %v, want oidc", AttributeProtocol, attributes[AttributeProtocol])
	}
	if attributes[AttributeProvider] != "corp" {
		t.Fatalf("%s = %v, want corp", AttributeProvider, attributes[AttributeProvider])
	}
	if attributes[AttributeIssuer] != "https://issuer.test" {
		t.Fatalf("%s = %v, want issuer", AttributeIssuer, attributes[AttributeIssuer])
	}
	authTime, ok := attributes[AttributeAuthTime].(time.Time)
	if !ok {
		t.Fatalf("%s type = %T, want time.Time", AttributeAuthTime, attributes[AttributeAuthTime])
	}
	want := time.Date(2026, 10, 18, 10, 30, 0, 0, time.UTC)
	if !authTime.Equal(want) {
		t.Fatalf("%s = %v, want %v", AttributeAuthTime, authTime, want)
	}
	if authTime.Location() != time.UTC {
		t.Fatalf("%s location = %v, want UTC", AttributeAuthTime, authTime.Location())
	}
	if _, err := json.Marshal(attributes); err != nil {
		t.Fatalf("merged attributes are not JSON-safe: %v", err)
	}
}

func TestSessionAttributesRejectsReservedKeyOverride(t *testing.T) {
	for _, key := range []string{AttributeProtocol, AttributeProvider, AttributeIssuer, AttributeAuthTime} {
		attributes, err := SessionAttributes(testExternalIdentity(), map[string]any{key: "attacker"})
		if !errors.Is(err, ErrReservedAttribute) {
			t.Fatalf("SessionAttributes(%q) error = %v, want ErrReservedAttribute", key, err)
		}
		if attributes != nil {
			t.Fatalf("SessionAttributes(%q) = %v, want nil on error", key, attributes)
		}
	}
}

func TestSessionAttributesCopiesApplicationAttributes(t *testing.T) {
	nested := map[string]any{"team": "core"}
	source := map[string]any{
		"nested":  nested,
		"groups":  []any{"engineering"},
		"tags":    []string{"a"},
		"blob":    []byte{1, 2},
		"labels":  map[string]string{"cn": "alice"},
		"aliases": map[string][]string{"mail": {"a@example.test", "b@example.test"}},
	}
	attributes, err := SessionAttributes(testExternalIdentity(), source)
	if err != nil {
		t.Fatalf("SessionAttributes() error = %v", err)
	}

	source["nested"].(map[string]any)["team"] = "mutated"
	source["groups"].([]any)[0] = "mutated"
	source["tags"].([]string)[0] = "mutated"
	source["blob"].([]byte)[0] = 7
	source["labels"].(map[string]string)["cn"] = "mutated"
	source["aliases"].(map[string][]string)["mail"][0] = "mutated"
	if attributes["nested"].(map[string]any)["team"] != "core" ||
		attributes["groups"].([]any)[0] != "engineering" ||
		attributes["tags"].([]string)[0] != "a" ||
		attributes["blob"].([]byte)[0] != 1 ||
		attributes["labels"].(map[string]string)["cn"] != "alice" ||
		attributes["aliases"].(map[string][]string)["mail"][0] != "a@example.test" {
		t.Fatalf("SessionAttributes retained a caller reference: %v", attributes)
	}

	attributes["nested"].(map[string]any)["team"] = "mutated-again"
	attributes["groups"].([]any)[0] = "mutated-again"
	attributes["tags"].([]string)[0] = "mutated-again"
	attributes["blob"].([]byte)[0] = 9
	attributes["labels"].(map[string]string)["cn"] = "mutated-again"
	attributes["aliases"].(map[string][]string)["mail"][0] = "mutated-again"
	if source["nested"].(map[string]any)["team"] != "mutated" ||
		source["groups"].([]any)[0] != "mutated" ||
		source["tags"].([]string)[0] != "mutated" ||
		source["blob"].([]byte)[0] != 7 ||
		source["labels"].(map[string]string)["cn"] != "mutated" ||
		source["aliases"].(map[string][]string)["mail"][0] != "mutated" {
		t.Fatal("SessionAttributes mutated the caller's map")
	}
}

func TestSessionAttributesRejectsNonJSONAttributes(t *testing.T) {
	for name, value := range map[string]any{
		"func":    func() {},
		"chan":    make(chan int),
		"nan":     json.Number("NaN"),
		"complex": complex(1, 2),
	} {
		if _, err := SessionAttributes(testExternalIdentity(), map[string]any{name: value}); !errors.Is(err, ErrInvalidAttribute) {
			t.Fatalf("SessionAttributes(%s) error = %v, want ErrInvalidAttribute", name, err)
		}
	}
}

func TestSessionAttributesEnforcesSizeLimit(t *testing.T) {
	under := map[string]any{"blob": strings.Repeat("a", MaxSessionAttributesBytes-1024)}
	if _, err := SessionAttributes(testExternalIdentity(), under); err != nil {
		t.Fatalf("SessionAttributes(under limit) error = %v", err)
	}

	over := map[string]any{"blob": strings.Repeat("a", MaxSessionAttributesBytes)}
	attributes, err := SessionAttributes(testExternalIdentity(), over)
	if !errors.Is(err, ErrAttributeSize) {
		t.Fatalf("SessionAttributes(over limit) error = %v, want ErrAttributeSize", err)
	}
	if attributes != nil {
		t.Fatalf("SessionAttributes(over limit) = %v, want nil on error", attributes)
	}
}

func TestSessionAttributesWithNilAttributes(t *testing.T) {
	attributes, err := SessionAttributes(testExternalIdentity(), nil)
	if err != nil {
		t.Fatalf("SessionAttributes(nil) error = %v", err)
	}
	if len(attributes) != 4 {
		t.Fatalf("SessionAttributes(nil) = %v, want four reserved attributes", attributes)
	}
	for _, key := range []string{AttributeProtocol, AttributeProvider, AttributeIssuer, AttributeAuthTime} {
		if _, ok := attributes[key]; !ok {
			t.Fatalf("SessionAttributes(nil) missing %q", key)
		}
	}
}
