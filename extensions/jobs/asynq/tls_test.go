package asynq

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

	xbcconfig "github.com/xbcio/xbc/config"
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

	cfg := bindAsynqConfig(t, map[string]any{
		"redis": map[string]any{
			"tls": map[string]any{
				"ca_file":              certFile,
				"cert_file":            certFile,
				"key_file":             keyFile,
				"server_name":          "redis.internal",
				"min_version":          "1.3",
				"insecure_skip_verify": true,
			},
		},
	})
	if cfg.Redis.TLS.CAFile != certFile || cfg.Redis.TLS.CertFile != certFile || cfg.Redis.TLS.KeyFile != keyFile {
		t.Fatalf("TLS material = %+v", cfg.Redis.TLS)
	}
	if cfg.Redis.TLS.ServerName != "redis.internal" || cfg.Redis.TLS.MinVersion != "1.3" || !cfg.Redis.TLS.InsecureSkipVerify {
		t.Fatalf("TLS options = %+v", cfg.Redis.TLS)
	}

	defaults := bindAsynqConfig(t, map[string]any{
		"redis": map[string]any{"tls": map[string]any{"ca_file": certFile}},
	})
	if defaults.Redis.TLS.MinVersion != defaultTLSMinVersion {
		t.Fatalf("TLS.MinVersion = %q, want %q", defaults.Redis.TLS.MinVersion, defaultTLSMinVersion)
	}
	if defaults.Redis.TLS.InsecureSkipVerify {
		t.Fatal("TLS.InsecureSkipVerify = true, want false by default")
	}
}

func TestPrepareConfigRejectsInvalidTLS(t *testing.T) {
	tests := []struct {
		name     string
		values   map[string]any
		schema   bool
		wantText string
	}{
		{
			name:     "certificate without key",
			values:   map[string]any{"redis": map[string]any{"tls": map[string]any{"cert_file": "/etc/redis/client.pem"}}},
			wantText: "redis.tls.cert_file",
		},
		{
			name:     "key without certificate",
			values:   map[string]any{"redis": map[string]any{"tls": map[string]any{"key_file": "/etc/redis/client-key.pem"}}},
			wantText: "redis.tls.key_file",
		},
		{
			name:     "unsupported minimum version",
			values:   map[string]any{"redis": map[string]any{"tls": map[string]any{"ca_file": "/etc/redis/ca.pem", "min_version": "1.1"}}},
			schema:   true,
			wantText: "redis.tls.min_version",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := bindAsynqConfigWithoutValidation(test.values)
			if err != nil {
				t.Fatalf("Bind() error = %v", err)
			}
			if test.schema {
				err = xbcconfig.Validate(&cfg, "plugins.asynq")
			} else {
				err = cfg.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("validation error = %v, want text %q", err, test.wantText)
			}
		})
	}
}

func TestAssemblePluginLoadsTLSMaterialEagerly(t *testing.T) {
	cfg := validTestConfig("127.0.0.1:6379")
	cfg.Redis.TLS.CAFile = filepath.Join(t.TempDir(), "missing.pem")

	p, err := assemblePlugin(cfg, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "redis.tls.ca_file") {
		t.Fatalf("assemblePlugin() = (%v, %v), want an error naming redis.tls.ca_file", p, err)
	}
}

// TestInitAppliesTLSMaterialToRedisOptions is the wiring test: the options the
// Redis constructor receives carry the material built from the section, and
// Init's ping completes a real TLS handshake with a TLS-enabled server.
func TestInitAppliesTLSMaterialToRedisOptions(t *testing.T) {
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

	cfg := validTestConfig(server.Addr())
	cfg.Redis.TLS.CAFile = certFile
	p := newTestPlugin(t, cfg, HandlerFunc(func(context.Context, Task) error { return nil }))

	var captured *goredis.Options
	p.factory.newRedis = func(options *goredis.Options) *goredis.Client {
		captured = options
		return goredis.NewClient(options)
	}

	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	defer func() {
		if err := p.stop(context.Background()); err != nil {
			t.Fatalf("stop() error = %v", err)
		}
	}()

	if captured == nil {
		t.Fatal("init() did not use the configured Redis factory")
	}
	if captured.TLSConfig == nil {
		t.Fatal("Redis options carry no TLSConfig")
	}
	if captured.TLSConfig.RootCAs == nil {
		t.Fatal("TLSConfig.RootCAs = nil, want the configured CA bundle")
	}
	if captured.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLSConfig.MinVersion = %#x, want TLS 1.2", captured.TLSConfig.MinVersion)
	}
	if captured.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLSConfig.InsecureSkipVerify = true, want false")
	}
}

func TestAssemblePluginLeavesTLSDisabledByDefault(t *testing.T) {
	p := newTestPlugin(t, validTestConfig("127.0.0.1:6379"), HandlerFunc(func(context.Context, Task) error { return nil }))
	if p.tlsConfig != nil {
		t.Fatalf("tlsConfig = %#v, want nil when the section configures no TLS", p.tlsConfig)
	}
	if options := p.cfg.Redis.options(p.tlsConfig); options.TLSConfig != nil {
		t.Fatalf("TLSConfig = %#v, want nil when the section configures no TLS", options.TLSConfig)
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
