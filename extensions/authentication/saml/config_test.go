package saml

import (
	"crypto/x509"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

func TestNewAppliesDefaults(t *testing.T) {
	provider := newTestProvider(t, nil)
	if len(provider.nameIDFormats) != 1 || provider.nameIDFormats[0] != NameIDFormatPersistent {
		t.Fatalf("nameIDFormats = %v, want [persistent]", provider.nameIDFormats)
	}
	if len(provider.signatureAlgs) != len(allowedSignatureAlgorithms) {
		t.Fatalf("signatureAlgs = %v, want every SHA-2 algorithm", provider.signatureAlgs)
	}
	if _, ok := provider.signatureAlgSet[AlgorithmRSASHA256]; !ok {
		t.Fatal("default signature algorithms do not include RSA-SHA256")
	}
	if provider.leeway != defaultLeeway {
		t.Fatalf("leeway = %s, want %s", provider.leeway, defaultLeeway)
	}
	if _, ok := provider.replay.(*MemoryReplayCache); !ok {
		t.Fatalf("replay cache = %T, want *MemoryReplayCache", provider.replay)
	}
	if !provider.now().Equal(testNow) {
		t.Fatalf("now = %s, want %s", provider.now(), testNow)
	}
}

func TestNewAcceptsExplicitAlgorithmsAndFormats(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.NameIDFormats = []string{NameIDFormatUnspecified, NameIDFormatPersistent, NameIDFormatUnspecified}
		config.AllowedSignatureAlgorithms = []string{AlgorithmRSASHA256, AlgorithmRSASHA256, AlgorithmECDSASHA512}
		config.Leeway = maxLeeway
		config.SPCertificate = spCertificate
	})
	if len(provider.nameIDFormats) != 2 {
		t.Fatalf("nameIDFormats = %v, want deduplicated", provider.nameIDFormats)
	}
	if len(provider.signatureAlgs) != 2 {
		t.Fatalf("signatureAlgs = %v, want deduplicated", provider.signatureAlgs)
	}
	if provider.leeway != maxLeeway {
		t.Fatalf("leeway = %s, want %s", provider.leeway, maxLeeway)
	}
}

func TestNewAcceptsDevelopmentHTTPPolicy(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.SSOURL = "http://idp.example.com/sso"
		config.ACSURL = "http://sp.example.com/acs?tenant=one"
		config.Policy = outbound.Policy{AllowHTTP: true}
	})
	if provider.ssoURL != "http://idp.example.com/sso" {
		t.Fatalf("ssoURL = %q", provider.ssoURL)
	}
	if provider.acsURL != "http://sp.example.com/acs?tenant=one" {
		t.Fatalf("acsURL = %q", provider.acsURL)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	cases := map[string]func(*Config){
		"empty name":                 func(config *Config) { config.Name = "" },
		"name with dot":              func(config *Config) { config.Name = "corp.example" },
		"name starting with dash":    func(config *Config) { config.Name = "-corp" },
		"name with underscore first": func(config *Config) { config.Name = "_corp" },
		"name too long":              func(config *Config) { config.Name = strings.Repeat("a", 65) },
		"empty issuer":               func(config *Config) { config.Issuer = "" },
		"issuer with whitespace":     func(config *Config) { config.Issuer = "https://idp example.com" },
		"relative sso url":           func(config *Config) { config.SSOURL = "/sso" },
		"sso userinfo":               func(config *Config) { config.SSOURL = "https://user:pass@idp.example.com/sso" },
		"sso fragment":               func(config *Config) { config.SSOURL = "https://idp.example.com/sso#fragment" },
		"sso query":                  func(config *Config) { config.SSOURL = "https://idp.example.com/sso?tenant=one" },
		"sso plain http":             func(config *Config) { config.SSOURL = "http://idp.example.com/sso" },
		"sso unsupported scheme":     func(config *Config) { config.SSOURL = "ftp://idp.example.com/sso" },
		"relative acs url":           func(config *Config) { config.ACSURL = "/acs" },
		"empty entity id":            func(config *Config) { config.EntityID = "" },
		"entity id with whitespace":  func(config *Config) { config.EntityID = "https://sp example.com" },
		"no idp certificate":         func(config *Config) { config.IdPCertificates = nil },
		"nil idp certificate":        func(config *Config) { config.IdPCertificates = []*x509.Certificate{nil} },
		"sp key without certificate": func(config *Config) { config.SPPrivateKey = spKey },
		"sp key mismatch": func(config *Config) {
			config.SPCertificate = spCertificate
			config.SPPrivateKey = untrustedKey
		},
		"transient nameid": func(config *Config) { config.NameIDFormats = []string{NameIDFormatTransient} },
		"email nameid":     func(config *Config) { config.NameIDFormats = []string{NameIDFormatEmail} },
		"unknown nameid":   func(config *Config) { config.NameIDFormats = []string{"urn:example:unknown"} },
		"sha1 signature": func(config *Config) {
			config.AllowedSignatureAlgorithms = []string{"http://www.w3.org/2000/09/xmldsig#rsa-sha1"}
		},
		"unknown signature":    func(config *Config) { config.AllowedSignatureAlgorithms = []string{"urn:example:algorithm"} },
		"invalid claim name":   func(config *Config) { config.ClaimAttributes = map[string]string{"bad name": "mail"} },
		"claim name leading -": func(config *Config) { config.ClaimAttributes = map[string]string{"-email": "mail"} },
		"invalid attribute":    func(config *Config) { config.ClaimAttributes = map[string]string{"email": "mail address"} },
		"negative leeway":      func(config *Config) { config.Leeway = -time.Second },
		"excessive leeway":     func(config *Config) { config.Leeway = maxLeeway + time.Nanosecond },
		"invalid policy":       func(config *Config) { config.Policy = outbound.Policy{DialTimeout: -time.Second} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := testConfig()
			mutate(&config)
			if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New = %v, want ErrInvalidConfig", err)
			}
			if err := config.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Config.Validate = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestNewRejectsTooManyClaimAttributes(t *testing.T) {
	claims := make(map[string]string, maxAttributes+1)
	for index := 0; index <= maxAttributes; index++ {
		claims["claim_"+strconv.Itoa(index)] = "attribute" + strconv.Itoa(index)
	}
	config := testConfig()
	config.ClaimAttributes = claims
	if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New = %v, want ErrInvalidConfig", err)
	}
}

func TestNewCopiesConfiguration(t *testing.T) {
	config := testConfig()
	config.ClaimAttributes = map[string]string{"email": "mail"}
	certificates := []*x509.Certificate{idpCertificate}
	config.IdPCertificates = certificates
	provider, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	config.ClaimAttributes["email"] = "changed"
	config.ClaimAttributes["extra"] = "changed"
	certificates[0] = rotatedCertificate
	config.NameIDFormats = []string{NameIDFormatUnspecified}

	if provider.claimAttributes["email"] != "mail" || len(provider.claimAttributes) != 1 {
		t.Fatalf("claimAttributes changed through the caller's map: %v", provider.claimAttributes)
	}
	if !provider.idpCertificates[0].Equal(idpCertificate) {
		t.Fatal("IdPCertificates changed through the caller's slice")
	}
	if len(provider.nameIDFormats) != 1 || provider.nameIDFormats[0] != NameIDFormatPersistent {
		t.Fatalf("nameIDFormats changed through the caller's slice: %v", provider.nameIDFormats)
	}
}

func TestConfigValidateAcceptsBaseline(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("Config.Validate: %v", err)
	}
}
