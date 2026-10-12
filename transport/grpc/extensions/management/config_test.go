package management

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoopbackAddressesNeedNoConsent pins the default posture: the listener
// exists for an operator on this machine, so every loopback spelling binds
// without allow_remote.
func TestLoopbackAddressesNeedNoConsent(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9091", ":9091", "localhost:9091", "[::1]:9091"} {
		cfg := Config{Addr: addr}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Config{Addr: %q}.Validate() = %v, want nil", addr, err)
		}
	}
}

// TestRemoteAddressesNeedConsentAndTLS walks the two refusals the binding rule
// composes: a non-loopback host without allow_remote, and allow_remote without
// a certificate. Each error must name the key the operator has to change, and
// the consented, certificated case must pass.
func TestRemoteAddressesNeedConsentAndTLS(t *testing.T) {
	certFile, keyFile := writeCertificatePair(t)

	for _, addr := range []string{"0.0.0.0:9091", "10.1.2.3:9091", "mgmt.example:9091"} {
		cfg := Config{Addr: addr}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("Config{Addr: %q}.Validate() = nil, want a refusal", addr)
		}
		if !strings.Contains(err.Error(), "not loopback") || !strings.Contains(err.Error(), "allow_remote") {
			t.Errorf("Config{Addr: %q}.Validate() = %v, want the error to name allow_remote", addr, err)
		}

		consented := Config{Addr: addr, AllowRemote: true}
		err = consented.Validate()
		if err == nil {
			t.Fatalf("Config{Addr: %q, AllowRemote: true}.Validate() = nil, want a refusal without TLS", addr)
		}
		if !strings.Contains(err.Error(), "tls.cert_file") {
			t.Errorf("Config{Addr: %q, AllowRemote: true}.Validate() = %v, want the error to name tls.cert_file", addr, err)
		}

		complete := Config{Addr: addr, AllowRemote: true, TLS: TLSConfig{CertFile: certFile, KeyFile: keyFile}}
		if err := complete.Validate(); err != nil {
			t.Errorf("a consented, certificated remote address was refused: %v", err)
		}
	}
}

// TestAnAddresslessSectionIsRefused pins that the section itself asks for the
// listener: a plugins.grpc-management section without a usable address is a
// configuration mistake, not a way to disable the plane.
func TestAnAddresslessSectionIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		want string
	}{
		{"empty", "", "must be a non-empty address"},
		{"whitespace only", "   ", "must be a non-empty address"},
		{"padded", " 127.0.0.1:9091", "must not be padded with whitespace"},
		{"no port", "127.0.0.1", "is not a host:port address"},
		{"empty port", "127.0.0.1:", "must name a port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Config{Addr: tc.addr}.Validate()
			if err == nil {
				t.Fatalf("addr %q was accepted", tc.addr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("addr %q error = %v, want it to contain %q", tc.addr, err, tc.want)
			}
		})
	}
}

// TestAllowRemoteWithoutAnAddressIsRefused pins the dead-key refusal: consent
// to expose a listener that does not exist is a mistake worth naming.
func TestAllowRemoteWithoutAnAddressIsRefused(t *testing.T) {
	err := Config{AllowRemote: true}.Validate()
	if err == nil || !strings.Contains(err.Error(), "non-empty address") {
		t.Fatalf("Validate() with allow_remote and no address = %v, want a refusal naming the address", err)
	}
}

// TestTLSDeclarationsAreCheckedTogether pins the pairwise and value rules the
// validator owns: the certificate and key travel together, neither may be
// whitespace-padded, and the floor is one of the two versions Go still calls
// safe.
func TestTLSDeclarationsAreCheckedTogether(t *testing.T) {
	certFile, keyFile := writeCertificatePair(t)

	for _, tc := range []struct {
		name string
		tls  TLSConfig
		want string
	}{
		{"certificate without key", TLSConfig{CertFile: certFile}, "must be set together"},
		{"key without certificate", TLSConfig{KeyFile: keyFile}, "must be set together"},
		{"padded certificate", TLSConfig{CertFile: " " + certFile, KeyFile: keyFile}, "must not be padded with whitespace"},
		{"unknown floor", TLSConfig{CertFile: certFile, KeyFile: keyFile, MinVersion: "1.0"}, "must be \"1.2\" or \"1.3\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Config{Addr: "127.0.0.1:9091", TLS: tc.tls}.Validate()
			if err == nil {
				t.Fatalf("TLS declaration %+v was accepted", tc.tls)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestBindAddrResolvesAHostlessAddressToLoopback pins where the hostless
// spelling actually lands, which is what makes the loopback validation rule
// true rather than aspirational.
func TestBindAddrResolvesAHostlessAddressToLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want string
	}{
		{":9091", "127.0.0.1:9091"},
		{"localhost:9091", "localhost:9091"},
		{"127.0.0.1:9091", "127.0.0.1:9091"},
		{"0.0.0.0:9091", "0.0.0.0:9091"},
	} {
		if got := (Config{Addr: tc.addr}).bindAddr(); got != tc.want {
			t.Errorf("Config{Addr: %q}.bindAddr() = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// TestMaterialLoadsTheConfiguredPair exercises the I/O edge the validator
// deliberately does not touch: the pair is read where the listener is bound,
// the configured floor reaches the tls.Config, and a missing pair is a startup
// error rather than a silent plaintext listener.
func TestMaterialLoadsTheConfiguredPair(t *testing.T) {
	certFile, keyFile := writeCertificatePair(t)

	if material, err := (TLSConfig{}).material(); err != nil || material != nil {
		t.Fatalf("material() without a certificate = (%v, %v), want (nil, nil)", material, err)
	}

	material, err := TLSConfig{CertFile: certFile, KeyFile: keyFile}.material()
	if err != nil {
		t.Fatalf("material() = %v", err)
	}
	if material == nil || len(material.Certificates) != 1 {
		t.Fatalf("material() = %+v, want one loaded certificate", material)
	}
	if material.MinVersion != tls.VersionTLS12 {
		t.Errorf("material().MinVersion = %#x, want the default TLS 1.2 floor", material.MinVersion)
	}

	raised, err := TLSConfig{CertFile: certFile, KeyFile: keyFile, MinVersion: "1.3"}.material()
	if err != nil {
		t.Fatalf("material() with the 1.3 floor = %v", err)
	}
	if raised.MinVersion != tls.VersionTLS13 {
		t.Errorf("material().MinVersion = %#x, want TLS 1.3", raised.MinVersion)
	}

	missing := TLSConfig{CertFile: filepath.Join(t.TempDir(), "absent.crt"), KeyFile: filepath.Join(t.TempDir(), "absent.key")}
	_, err = missing.material()
	if err == nil {
		t.Fatal("a missing certificate pair was accepted")
	}
	if !strings.Contains(err.Error(), "grpc-management: tls.cert_file/tls.key_file") {
		t.Errorf("missing pair error = %v, want it to name the keys", err)
	}
}

// TestMaterialRefusesUnreadableFilesWithoutQuotingThem pins the I/O error's
// shape: the wrapped cause is what names the file, and it must not be the
// message an operator reads in a log without the keys that explain it.
func TestMaterialRefusesUnreadableFilesWithoutQuotingThem(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.crt")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", garbage, err)
	}
	_, err := (TLSConfig{CertFile: garbage, KeyFile: garbage}).material()
	if err == nil {
		t.Fatal("a malformed certificate pair was accepted")
	}
	if !strings.Contains(err.Error(), "grpc-management: tls.cert_file/tls.key_file") {
		t.Errorf("malformed pair error = %v, want it to name the keys", err)
	}
}

// TestNormalizeConfigAppliesTheTLSFloor pins the programmatic path: a directly
// constructed Config reaches the same normalized value the binder produces.
func TestNormalizeConfigAppliesTheTLSFloor(t *testing.T) {
	normalized, err := normalizeConfig(Config{Addr: "127.0.0.1:9091"})
	if err != nil {
		t.Fatalf("normalizeConfig() = %v", err)
	}
	if normalized.TLS.MinVersion != defaultTLSMinVersion {
		t.Errorf("normalizeConfig() floor = %q, want %q", normalized.TLS.MinVersion, defaultTLSMinVersion)
	}

	if _, err := normalizeConfig(Config{Addr: "127.0.0.1:9091", TLS: TLSConfig{CertFile: "c.crt", KeyFile: "c.key", MinVersion: "1.1"}}); err == nil {
		t.Fatal("normalizeConfig() accepted an unsafe floor")
	}
}
