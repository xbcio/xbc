package ldap

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConfigAcceptsTheTwoTLSShapes(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
		st   bool
	}{
		{name: "ldaps", url: "ldaps://directory.example.com", st: false},
		{name: "ldaps with port", url: "ldaps://directory.example.com:1636", st: false},
		{name: "starttls", url: "ldap://directory.example.com", st: true},
		{name: "starttls with port", url: "ldap://directory.example.com:1389", st: true},
		{name: "ipv6 literal", url: "ldaps://[2001:db8::1]:636", st: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(test.url)
			config.AllowPrivate = false
			config.StartTLS = test.st
			if err := config.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			provider := newProvider(t, config)
			wantPort := "636"
			if strings.HasPrefix(test.url, "ldap:") {
				wantPort = "389"
			}
			if strings.Contains(test.url, ":1636") {
				wantPort = "1636"
			}
			if strings.Contains(test.url, ":1389") {
				wantPort = "1389"
			}
			if provider.port != wantPort {
				t.Fatalf("port = %q, want %q", provider.port, wantPort)
			}
		})
	}
}

func TestConfigRejectsUnsafeURLs(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
		st   bool
	}{
		{name: "empty", url: "", st: false},
		{name: "plain http scheme", url: "https://directory.example.com", st: false},
		{name: "cldap", url: "cldap://directory.example.com", st: false},
		{name: "ldapi", url: "ldapi://%2Fvar%2Frun%2Fldapi", st: false},
		{name: "no host", url: "ldaps://", st: false},
		{name: "userinfo", url: "ldaps://user:pass@directory.example.com", st: false},
		{name: "query", url: "ldaps://directory.example.com/?x=1", st: false},
		{name: "fragment", url: "ldaps://directory.example.com/#x", st: false},
		{name: "path", url: "ldaps://directory.example.com/dc=example", st: false},
		{name: "empty port", url: "ldaps://directory.example.com:", st: false},
		{name: "port zero", url: "ldaps://directory.example.com:0", st: false},
		{name: "port too large", url: "ldaps://directory.example.com:65536", st: false},
		{name: "port not numeric", url: "ldaps://directory.example.com:ldap", st: false},
		{name: "ipv6 zone", url: "ldaps://[fe80::1%25en0]:636", st: false},
		{name: "bad host character", url: "ldaps://directory!example.com", st: false},
		{name: "plaintext ldap", url: "ldap://directory.example.com", st: false},
		{name: "starttls on ldaps", url: "ldaps://directory.example.com", st: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(test.url)
			config.StartTLS = test.st
			if err := config.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate = %v, want ErrInvalidConfig", err)
			}
			if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestConfigRejectsInvalidStaticFields(t *testing.T) {
	valid := testConfig("ldaps://directory.example.com")
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "empty name", mutate: func(c *Config) { c.Name = "" }},
		{name: "name starts with dash", mutate: func(c *Config) { c.Name = "-corp" }},
		{name: "name with space", mutate: func(c *Config) { c.Name = "corp directory" }},
		{name: "name too long", mutate: func(c *Config) { c.Name = strings.Repeat("a", 65) }},
		{name: "empty issuer", mutate: func(c *Config) { c.Issuer = "  " }},
		{name: "issuer control character", mutate: func(c *Config) { c.Issuer = "corp\x00" }},
		{name: "empty service dn", mutate: func(c *Config) { c.ServiceBindDN = "" }},
		{name: "invalid service dn", mutate: func(c *Config) { c.ServiceBindDN = "not a dn" }},
		{name: "empty service password", mutate: func(c *Config) { c.ServiceBindPassword = "" }},
		{name: "empty base dn", mutate: func(c *Config) { c.BaseDN = "" }},
		{name: "invalid base dn", mutate: func(c *Config) { c.BaseDN = "=broken" }},
		{name: "empty filter", mutate: func(c *Config) { c.UserFilter = "" }},
		{name: "filter without placeholder", mutate: func(c *Config) { c.UserFilter = "(uid=alice)" }},
		{name: "filter with two placeholders", mutate: func(c *Config) { c.UserFilter = "(&(uid=%s)(mail=%s))" }},
		{name: "filter with another verb", mutate: func(c *Config) { c.UserFilter = "(uid=%d)" }},
		{name: "filter with another verb in a second clause", mutate: func(c *Config) { c.UserFilter = "(uid=%s)(cn=%v)" }},
		{name: "filter with a literal percent", mutate: func(c *Config) { c.UserFilter = "(uid=%s)(cn=%%)" }},
		{name: "filter without wrapper", mutate: func(c *Config) { c.UserFilter = "uid=%s" }},
		{name: "filter unbalanced", mutate: func(c *Config) { c.UserFilter = "(&(uid=%s)" }},
		{name: "filter too long", mutate: func(c *Config) { c.UserFilter = "(" + strings.Repeat("a", 512) + "=%s)" }},
		{name: "invalid subject attribute", mutate: func(c *Config) { c.SubjectAttribute = "entry uuid" }},
		{name: "subject attribute wildcard", mutate: func(c *Config) { c.SubjectAttribute = "*" }},
		{name: "empty claim name", mutate: func(c *Config) { c.ClaimAttributes = map[string]string{"": "mail"} }},
		{name: "claim name with space", mutate: func(c *Config) { c.ClaimAttributes = map[string]string{"display name": "cn"} }},
		{name: "empty claim attribute", mutate: func(c *Config) { c.ClaimAttributes = map[string]string{"email": " "} }},
		{name: "claim attribute with paren", mutate: func(c *Config) { c.ClaimAttributes = map[string]string{"email": "mail)(uid=*"} }},
		{name: "negative dial timeout", mutate: func(c *Config) { c.DialTimeout = -time.Second }},
		{name: "negative operation timeout", mutate: func(c *Config) { c.OperationTimeout = -time.Second }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if err := config.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate = %v, want ErrInvalidConfig", err)
			}
			if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestConfigDefaultsAndDefensiveCopies(t *testing.T) {
	config := testConfig("ldaps://directory.example.com")
	config.SubjectAttribute = ""
	config.ClaimAttributes = map[string]string{"email": "mail"}
	config.DialTimeout = 0
	config.OperationTimeout = 0
	config.Now = nil
	provider := newProvider(t, config)

	if provider.subjectAttribute != defaultSubjectAttribute {
		t.Fatalf("SubjectAttribute = %q, want %q", provider.subjectAttribute, defaultSubjectAttribute)
	}
	if provider.dialTimeout != defaultDialTimeout {
		t.Fatalf("DialTimeout = %s, want %s", provider.dialTimeout, defaultDialTimeout)
	}
	if provider.operationTimeout != defaultOperationTimeout {
		t.Fatalf("OperationTimeout = %s, want %s", provider.operationTimeout, defaultOperationTimeout)
	}
	if provider.now == nil {
		t.Fatal("Now was not defaulted")
	}

	config.ClaimAttributes["email"] = "changed"
	if provider.claimAttributes["email"] != "mail" {
		t.Fatal("provider retained the caller's claim map")
	}
}

func TestSearchAttributesDeduplicatesAndKeepsTheSubject(t *testing.T) {
	config := testConfig("ldaps://directory.example.com")
	config.SubjectAttribute = "entryUUID"
	config.ClaimAttributes = map[string]string{
		"email": "mail",
		"uuid":  "entryUUID",
		"name":  "cn",
	}
	provider := newProvider(t, config)
	attributes := provider.searchAttributes()
	want := map[string]bool{"entryUUID": true, "mail": true, "cn": true}
	if len(attributes) != len(want) {
		t.Fatalf("attributes = %v, want %d unique entries", attributes, len(want))
	}
	for _, attribute := range attributes {
		if !want[attribute] {
			t.Fatalf("unexpected attribute %q", attribute)
		}
	}
}

func TestValidAttributeDescriptionAcceptsNamesAndOIDs(t *testing.T) {
	for _, value := range []string{"cn", "entryUUID", "objectGUID", "1.2.840.113556.1.4.1", "x-foo"} {
		if !validAttributeDescription(value) {
			t.Fatalf("validAttributeDescription(%q) = false", value)
		}
	}
	for _, value := range []string{"", " cn", "cn ", "c n", "1..2", ".1.2", "1.2.", "(cn)", "cn=*", "ü"} {
		if validAttributeDescription(value) {
			t.Fatalf("validAttributeDescription(%q) = true", value)
		}
	}
}

func TestValidProviderNameMatchesTheFederationRule(t *testing.T) {
	for _, value := range []string{"a", "A1", "corp-directory", "corp_directory", "1corp"} {
		if !validProviderName(value) {
			t.Fatalf("validProviderName(%q) = false", value)
		}
	}
	for _, value := range []string{"", "-corp", "_corp", "corp directory", "corp!", strings.Repeat("a", 65)} {
		if validProviderName(value) {
			t.Fatalf("validProviderName(%q) = true", value)
		}
	}
}
