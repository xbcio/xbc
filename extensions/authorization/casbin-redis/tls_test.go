package casbinredis

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/casbin/casbin/v2/persist"
	rediswatcher "github.com/casbin/redis-watcher/v2"
)

// writeTestCertificatePair writes a self-signed server certificate and key to
// a temporary directory and returns their paths. The certificate is generated
// in-process: a test that reached a certificate authority over the network
// would be testing the network.
func writeTestCertificatePair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	return writeTestCertificatePairWithSANs(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})
}

// writeTestCertificatePairWithSANs writes a self-signed pair with exactly the
// given SANs, so a test can require the client to verify a name that is not the
// dialed address.
func writeTestCertificatePairWithSANs(t *testing.T, dnsNames []string, ips []net.IP) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey() error = %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate() error = %v", err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("x509.MarshalECPrivateKey() error = %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "redis.crt")
	keyFile = filepath.Join(dir, "redis.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

func TestConfigTLSBindsAndDefaults(t *testing.T) {
	certFile, keyFile := writeTestCertificatePair(t)

	cfg, err := bindConfig(map[string]any{
		"tls": map[string]any{
			"ca_file":              certFile,
			"cert_file":            certFile,
			"key_file":             keyFile,
			"server_name":          "redis.internal",
			"min_version":          "1.3",
			"insecure_skip_verify": true,
		},
	})
	if err != nil {
		t.Fatalf("bindConfig() error = %v", err)
	}
	if cfg.TLS.CAFile != certFile || cfg.TLS.CertFile != certFile || cfg.TLS.KeyFile != keyFile {
		t.Fatalf("TLS material = %+v", cfg.TLS)
	}
	if cfg.TLS.ServerName != "redis.internal" || cfg.TLS.MinVersion != "1.3" || !cfg.TLS.InsecureSkipVerify {
		t.Fatalf("TLS options = %+v", cfg.TLS)
	}

	defaults, err := bindConfig(map[string]any{"tls": map[string]any{"ca_file": certFile}})
	if err != nil {
		t.Fatalf("bindConfig() error = %v", err)
	}
	if defaults.TLS.MinVersion != defaultTLSMinVersion {
		t.Fatalf("TLS.MinVersion = %q, want %q", defaults.TLS.MinVersion, defaultTLSMinVersion)
	}
	if defaults.TLS.InsecureSkipVerify {
		t.Fatal("TLS.InsecureSkipVerify = true, want false by default")
	}
	if DefaultConfig().TLS.MinVersion != defaultTLSMinVersion {
		t.Fatalf("DefaultConfig().TLS.MinVersion = %q, want %q", DefaultConfig().TLS.MinVersion, defaultTLSMinVersion)
	}
}

func TestConfigRejectsInvalidTLS(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "certificate without key",
			mutate: func(c *Config) { c.TLS.CertFile = "/etc/redis/client.pem" },
			want:   "tls.cert_file",
		},
		{
			name:   "key without certificate",
			mutate: func(c *Config) { c.TLS.KeyFile = "/etc/redis/client-key.pem" },
			want:   "tls.key_file",
		},
		{
			name:   "unsupported minimum version",
			mutate: func(c *Config) { c.TLS.CAFile = "/etc/redis/ca.pem"; c.TLS.MinVersion = "1.0" },
			want:   "tls.min_version",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestNewLoadsTLSMaterialEagerly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TLS.CAFile = filepath.Join(t.TempDir(), "missing.pem")
	factory, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "tls.ca_file") {
		t.Fatalf("New() = (%v, %v), want an error naming tls.ca_file", factory, err)
	}
}

// TestOpenAppliesTLSMaterialToWatcherOptions is the wiring test: the options
// the watcher constructors receive carry the material built from the section,
// in both topologies.
func TestOpenAppliesTLSMaterialToWatcherOptions(t *testing.T) {
	certFile, keyFile := writeTestCertificatePair(t)

	t.Run("standalone", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Addrs = []string{"127.0.0.1:6379"}
		cfg.TLS = TLSConfig{
			CAFile: certFile, CertFile: certFile, KeyFile: keyFile,
			ServerName: "redis.internal", MinVersion: "1.3",
		}
		returned := new(stubWatcher)
		var tlsConfig *tls.Config
		factory, err := newFactory(cfg, watcherConstructors{
			standalone: func(_ string, options rediswatcher.WatcherOptions) (persist.Watcher, error) {
				tlsConfig = options.Options.TLSConfig
				if options.SubClient == nil || options.PubClient == nil {
					t.Fatal("watcher options carry no owned clients")
				}
				t.Cleanup(func() {
					_ = options.SubClient.Close()
					_ = options.PubClient.Close()
				})
				return returned, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		watcher, err := factory.Open(newTestEnforcer(t))
		if err != nil || watcher != returned {
			t.Fatalf("Open() = (%T, %v)", watcher, err)
		}

		if tlsConfig == nil {
			t.Fatal("watcher options carry no TLSConfig")
		}
		if tlsConfig.RootCAs == nil {
			t.Fatal("TLSConfig.RootCAs = nil, want the configured CA bundle")
		}
		if tlsConfig.ServerName != "redis.internal" || tlsConfig.MinVersion != tls.VersionTLS13 {
			t.Fatalf("TLSConfig = %#v, want server_name redis.internal and TLS 1.3", tlsConfig)
		}
		if len(tlsConfig.Certificates) != 1 {
			t.Fatalf("TLSConfig.Certificates = %d, want the configured client pair", len(tlsConfig.Certificates))
		}
	})

	t.Run("cluster", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Mode = ModeCluster
		cfg.Addrs = []string{"redis-a.internal:6379"}
		cfg.TLS = TLSConfig{
			CAFile: certFile, CertFile: certFile, KeyFile: keyFile,
			ServerName: "redis.internal", MinVersion: "1.3",
		}
		returned := new(stubWatcher)
		var tlsConfig *tls.Config
		factory, err := newFactory(cfg, watcherConstructors{
			cluster: func(_ string, options rediswatcher.WatcherOptions) (persist.Watcher, error) {
				tlsConfig = options.ClusterOptions.TLSConfig
				return returned, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		watcher, err := factory.Open(newTestEnforcer(t))
		if err != nil || watcher != returned {
			t.Fatalf("Open() = (%T, %v)", watcher, err)
		}

		if tlsConfig == nil {
			t.Fatal("watcher cluster options carry no TLSConfig")
		}
		if tlsConfig.RootCAs == nil || tlsConfig.ServerName != "redis.internal" || tlsConfig.MinVersion != tls.VersionTLS13 {
			t.Fatalf("ClusterOptions.TLSConfig = %#v", tlsConfig)
		}
	})
}

func TestFactoryLeavesTLSDisabledByDefault(t *testing.T) {
	cfg := DefaultConfig()
	factory, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if factory.tlsConfig != nil {
		t.Fatalf("tlsConfig = %#v, want nil when the section configures no TLS", factory.tlsConfig)
	}
}

// TestOpenNegotiatesTLSAgainstRedis closes the loop: the material the section
// declares is what the watcher's own clients use.
func TestOpenNegotiatesTLSAgainstRedis(t *testing.T) {
	certFile, keyFile := writeTestCertificatePair(t)
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("tls.LoadX509KeyPair() error = %v", err)
	}
	server, err := miniredis.RunTLS(&tls.Config{Certificates: []tls.Certificate{certificate}})
	if err != nil {
		t.Fatalf("miniredis.RunTLS() error = %v", err)
	}
	t.Cleanup(server.Close)

	cfg := DefaultConfig()
	cfg.Addrs = []string{server.Addr()}
	cfg.TLS.CAFile = certFile
	factory, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := factory.Open(newTestEnforcer(t))
	if err != nil {
		t.Fatalf("Open() over TLS error = %v", err)
	}
	await(t, "TLS watcher subscription", func() bool {
		return server.PubSubNumSub(cfg.Channel)[cfg.Channel] == 1
	})
	watcher.Close()
}

// TestOpenVerifiesConfiguredServerNameOverride proves the section's server_name
// reaches the tracker handshake rather than only the go-redis options field.
// The certificate carries a DNS SAN for localhost and no IP SAN, while the
// server listens on 127.0.0.1: without the override the dialed address cannot
// verify, and only a handshake that honours the configured name succeeds.
func TestOpenVerifiesConfiguredServerNameOverride(t *testing.T) {
	certFile, keyFile := writeTestCertificatePairWithSANs(t, []string{"localhost"}, nil)
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("tls.LoadX509KeyPair() error = %v", err)
	}
	server, err := miniredis.RunTLS(&tls.Config{Certificates: []tls.Certificate{certificate}})
	if err != nil {
		t.Fatalf("miniredis.RunTLS() error = %v", err)
	}
	t.Cleanup(server.Close)

	cfg := DefaultConfig()
	cfg.Addrs = []string{server.Addr()}
	cfg.TLS.CAFile = certFile
	cfg.TLS.ServerName = "localhost"
	factory, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := factory.Open(newTestEnforcer(t))
	if err != nil {
		t.Fatalf("Open() with server_name localhost error = %v", err)
	}
	await(t, "subscription under the server name override", func() bool {
		return server.PubSubNumSub(cfg.Channel)[cfg.Channel] == 1
	})
	watcher.Close()

	cfg.TLS.ServerName = "wrong.internal"
	factory, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err = factory.Open(newTestEnforcer(t))
	if err == nil {
		watcher.Close()
		t.Fatal("Open() with a mismatched server_name error = nil, want verification to fail")
	}
}

// TestTLSConfiguredEnablesEachSingleField pins the enablement rule: each of
// server_name, insecure_skip_verify, and a certificate pair turns TLS on by
// itself. The binder is deliberately not involved, so a regression in
// configured() cannot hide behind a default tag.
func TestTLSConfiguredEnablesEachSingleField(t *testing.T) {
	certFile, keyFile := writeTestCertificatePair(t)

	tests := []struct {
		name   string
		config TLSConfig
		check  func(*testing.T, *tls.Config)
	}{
		{
			name:   "server name only",
			config: TLSConfig{ServerName: "redis.internal"},
			check: func(t *testing.T, config *tls.Config) {
				if config.ServerName != "redis.internal" {
					t.Fatalf("ServerName = %q, want redis.internal", config.ServerName)
				}
			},
		},
		{
			name:   "insecure skip verify only",
			config: TLSConfig{InsecureSkipVerify: true},
			check: func(t *testing.T, config *tls.Config) {
				if !config.InsecureSkipVerify {
					t.Fatal("InsecureSkipVerify = false, want true")
				}
			},
		},
		{
			name:   "certificate pair only",
			config: TLSConfig{CertFile: certFile, KeyFile: keyFile},
			check: func(t *testing.T, config *tls.Config) {
				if len(config.Certificates) != 1 {
					t.Fatalf("Certificates = %d, want the configured pair", len(config.Certificates))
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := test.config.clientTLSConfig()
			if err != nil {
				t.Fatalf("clientTLSConfig() error = %v", err)
			}
			if config == nil {
				t.Fatal("clientTLSConfig() = nil, want TLS enabled by this field alone")
			}
			if config.MinVersion != tls.VersionTLS12 {
				t.Fatalf("MinVersion = %#x, want the TLS 1.2 default", config.MinVersion)
			}
			test.check(t, config)
		})
	}
}

// TestTLSConfigRejectsUnsupportedMinVersionDirectly covers the direct
// construction path: a caller that builds TLSConfig in Go rather than through
// the binder never sees the schema tag, so validate must reject the version on
// its own.
func TestTLSConfigRejectsUnsupportedMinVersionDirectly(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "1.4"} {
		t.Run(version, func(t *testing.T) {
			config, err := (TLSConfig{MinVersion: version}).clientTLSConfig()
			if err == nil {
				t.Fatalf("clientTLSConfig() error = nil, want the unsupported version rejected")
			}
			if !strings.Contains(err.Error(), "min_version") {
				t.Fatalf("clientTLSConfig() error = %q, want it to name min_version", err)
			}
			if config != nil {
				t.Fatalf("clientTLSConfig() = %#v, want nil on error", config)
			}
		})
	}
}
