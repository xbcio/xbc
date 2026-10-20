package redis

import (
	"context"
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
	goredis "github.com/redis/go-redis/v9"
)

// writeTestCertificatePair writes a self-signed server certificate and key to
// a temporary directory and returns their paths. The certificate is generated
// in-process: a test that reached a certificate authority over the network
// would be testing the network.
func writeTestCertificatePair(t *testing.T) (certFile, keyFile string) {
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
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
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

	cfg := bindConfig(t, map[string]any{
		"tls": map[string]any{
			"ca_file":              certFile,
			"cert_file":            certFile,
			"key_file":             keyFile,
			"server_name":          "redis.internal",
			"min_version":          "1.3",
			"insecure_skip_verify": true,
		},
	})
	if cfg.TLS.CAFile != certFile || cfg.TLS.CertFile != certFile || cfg.TLS.KeyFile != keyFile {
		t.Fatalf("TLS material = %+v", cfg.TLS)
	}
	if cfg.TLS.ServerName != "redis.internal" || cfg.TLS.MinVersion != "1.3" || !cfg.TLS.InsecureSkipVerify {
		t.Fatalf("TLS options = %+v", cfg.TLS)
	}

	defaults := bindConfig(t, map[string]any{"tls": map[string]any{"ca_file": certFile}})
	if defaults.TLS.MinVersion != defaultTLSMinVersion {
		t.Fatalf("TLS.MinVersion = %q, want %q", defaults.TLS.MinVersion, defaultTLSMinVersion)
	}
	if defaults.TLS.InsecureSkipVerify {
		t.Fatal("TLS.InsecureSkipVerify = true, want false by default")
	}
}

func TestPrepareConfigRejectsInvalidTLS(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		want   string
	}{
		{
			name:   "certificate without key",
			values: map[string]any{"tls": map[string]any{"cert_file": "/etc/redis/client.pem"}},
			want:   "tls.cert_file",
		},
		{
			name:   "key without certificate",
			values: map[string]any{"tls": map[string]any{"key_file": "/etc/redis/client-key.pem"}},
			want:   "tls.key_file",
		},
		{
			name:   "unsupported minimum version",
			values: map[string]any{"tls": map[string]any{"ca_file": "/etc/redis/ca.pem", "min_version": "1.1"}},
			want:   "tls.min_version",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := bindPreparedConfig(test.values)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("prepare error = %v, want text %q", err, test.want)
			}
		})
	}
}

// TestBuildClientAppliesTLSMaterialToEveryMode is the wiring test: each mode's
// factory receives options whose TLSConfig carries the material built from the
// section. Asserting the parsed configuration alone would not show that the
// constructor ever sees it.
func TestBuildClientAppliesTLSMaterialToEveryMode(t *testing.T) {
	certFile, keyFile := writeTestCertificatePair(t)

	tests := []struct {
		name    string
		values  map[string]any
		capture func(*clientFactory) func() *tls.Config
	}{
		{
			name: "standalone",
			values: map[string]any{
				"ping": false,
				"tls": map[string]any{
					"ca_file": certFile, "cert_file": certFile, "key_file": keyFile,
					"server_name": "redis.internal", "min_version": "1.3",
				},
			},
			capture: func(factory *clientFactory) func() *tls.Config {
				var captured *tls.Config
				factory.standalone = func(options *goredis.Options) goredis.UniversalClient {
					captured = options.TLSConfig
					return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
				}
				return func() *tls.Config { return captured }
			},
		},
		{
			name: "sentinel",
			values: map[string]any{
				"mode": "sentinel", "addrs": []any{"10.0.0.1:26379"}, "master_name": "primary", "ping": false,
				"tls": map[string]any{
					"ca_file": certFile, "cert_file": certFile, "key_file": keyFile,
					"server_name": "redis.internal", "min_version": "1.3",
				},
			},
			capture: func(factory *clientFactory) func() *tls.Config {
				var captured *tls.Config
				factory.failover = func(options *goredis.FailoverOptions) goredis.UniversalClient {
					captured = options.TLSConfig
					return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
				}
				return func() *tls.Config { return captured }
			},
		},
		{
			name: "cluster",
			values: map[string]any{
				"mode": "cluster", "addrs": []any{"10.0.0.1:6379"}, "ping": false,
				"tls": map[string]any{
					"ca_file": certFile, "cert_file": certFile, "key_file": keyFile,
					"server_name": "redis.internal", "min_version": "1.3",
				},
			},
			capture: func(factory *clientFactory) func() *tls.Config {
				var captured *tls.Config
				factory.cluster = func(options *goredis.ClusterOptions) goredis.UniversalClient {
					captured = options.TLSConfig
					return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
				}
				return func() *tls.Config { return captured }
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := preparedConfig(t, test.values)
			factory := defaultClientFactory()
			tlsConfig := test.capture(&factory)

			client, err := buildClient(factory, cfg)
			if err != nil {
				t.Fatalf("buildClient() error = %v", err)
			}
			t.Cleanup(func() { _ = stopClient(client, context.Background()) })

			got := tlsConfig()
			if got == nil {
				t.Fatal("constructor options carry no TLSConfig")
			}
			if got.RootCAs == nil {
				t.Fatal("TLSConfig.RootCAs = nil, want the configured CA bundle")
			}
			if got.ServerName != "redis.internal" {
				t.Fatalf("TLSConfig.ServerName = %q, want redis.internal", got.ServerName)
			}
			if got.MinVersion != tls.VersionTLS13 {
				t.Fatalf("TLSConfig.MinVersion = %#x, want TLS 1.3", got.MinVersion)
			}
			if got.InsecureSkipVerify {
				t.Fatal("TLSConfig.InsecureSkipVerify = true, want false")
			}
			if len(got.Certificates) != 1 {
				t.Fatalf("TLSConfig.Certificates = %d, want the configured client pair", len(got.Certificates))
			}
		})
	}
}

func TestBuildClientLeavesTLSDisabledByDefault(t *testing.T) {
	cfg := preparedConfig(t, map[string]any{"ping": false})
	factory := defaultClientFactory()
	var captured *goredis.Options
	factory.standalone = func(options *goredis.Options) goredis.UniversalClient {
		captured = options
		return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	}

	client, err := buildClient(factory, cfg)
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })
	if captured == nil {
		t.Fatal("buildClient() did not use the configured factory")
	}
	if captured.TLSConfig != nil {
		t.Fatalf("TLSConfig = %#v, want nil when the section configures no TLS", captured.TLSConfig)
	}
}

func TestBuildClientFailsConstructionOnUnreadableTLSMaterial(t *testing.T) {
	cfg := preparedConfig(t, map[string]any{
		"ping": false,
		"tls":  map[string]any{"ca_file": filepath.Join(t.TempDir(), "missing.pem")},
	})
	client, err := buildClient(defaultClientFactory(), cfg)
	if err == nil || !strings.Contains(err.Error(), "tls.ca_file") {
		t.Fatalf("buildClient() = (%v, %v), want an error naming tls.ca_file", client, err)
	}
}

// TestBuildClientNegotiatesTLSAgainstRedis closes the loop: the material the
// section declares is what the handshake actually uses.
func TestBuildClientNegotiatesTLSAgainstRedis(t *testing.T) {
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

	cfg := preparedConfig(t, map[string]any{
		"addr": server.Addr(),
		"tls":  map[string]any{"ca_file": certFile},
	})
	client, err := buildClient(defaultClientFactory(), cfg)
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })

	if err := client.Set(context.Background(), "tls", "negotiated", 0).Err(); err != nil {
		t.Fatalf("Set() over TLS error = %v", err)
	}
	value, err := server.Get("tls")
	if err != nil || value != "negotiated" {
		t.Fatalf("server value = %q, %v; want negotiated", value, err)
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
