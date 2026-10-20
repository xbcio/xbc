package cron

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

	miniredis "github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"

	xbcconfig "github.com/xbcio/xbc/config"
)

// TestConfigBindsTLSDefaults checks the nested section through the binder:
// min_version and insecure_skip_verify get their documented defaults even
// three struct levels below plugins.cron.
func TestConfigBindsTLSDefaults(t *testing.T) {
	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"cron": map[string]any{
				"distributed": map[string]any{
					"redis": map[string]any{
						"addr": "redis.internal:6379",
						"tls":  map[string]any{"ca_file": "/etc/redis/ca.pem"},
					},
				},
			},
		},
	}, "XBC_CRON_TEST_")
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	config := defaultConfig()
	if err := environment.Bind("plugins.cron", &config); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if config.Distributed.Redis.TLS.CAFile != "/etc/redis/ca.pem" {
		t.Fatalf("TLS.CAFile = %q", config.Distributed.Redis.TLS.CAFile)
	}
	if config.Distributed.Redis.TLS.MinVersion != defaultTLSMinVersion {
		t.Fatalf("TLS.MinVersion = %q, want %q", config.Distributed.Redis.TLS.MinVersion, defaultTLSMinVersion)
	}
	if config.Distributed.Redis.TLS.InsecureSkipVerify {
		t.Fatal("TLS.InsecureSkipVerify = true, want false by default")
	}
}

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

func TestPrepareConfigValidatesTLS(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{
			name: "certificate without key",
			edit: func(config *Config) {
				config.Distributed.Redis.TLS.CertFile = "/etc/redis/client.pem"
			},
			want: "distributed.redis.tls.cert_file",
		},
		{
			name: "key without certificate",
			edit: func(config *Config) {
				config.Distributed.Redis.TLS.KeyFile = "/etc/redis/client-key.pem"
			},
			want: "distributed.redis.tls.key_file",
		},
		{
			name: "unsupported minimum version",
			edit: func(config *Config) {
				config.Distributed.Redis.TLS.CAFile = "/etc/redis/ca.pem"
				config.Distributed.Redis.TLS.MinVersion = "1.0"
			},
			want: "distributed.redis.tls.min_version",
		},
		{
			name: "TLS with a selected instance",
			edit: func(config *Config) {
				config.Distributed.RedisInstance = "locks"
				config.Distributed.Redis.TLS.CAFile = "/etc/redis/ca.pem"
			},
			want: "distributed.redis.tls configures the client cron would own",
		},
		{
			// With no address and no instance, the distributed lock comes from
			// an externally provided lease.Locker and cron opens no Redis
			// connection at all, so the TLS section has no client to configure.
			name: "TLS without a client cron owns",
			edit: func(config *Config) {
				config.Distributed.Enabled = true
				config.Distributed.Redis.TLS.CAFile = "/etc/redis/ca.pem"
			},
			want: "distributed.redis.tls has nowhere to take effect",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := defaultConfig()
			test.edit(&config)
			_, err := prepareConfig(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("prepareConfig() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestPrepareConfigAcceptsTLS(t *testing.T) {
	certFile, keyFile := writeTestCertificatePair(t)
	config := defaultConfig()
	config.Distributed.Redis.Addr = "redis.internal:6379"
	config.Distributed.Redis.TLS = TLSConfig{
		CAFile:     certFile,
		CertFile:   certFile,
		KeyFile:    keyFile,
		ServerName: "redis.internal",
		MinVersion: "1.3",
	}
	if _, err := prepareConfig(config); err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
}

// TestPrepareConfigAcceptsExternalLockerWithoutTLS keeps the boundary narrow:
// the rejection only fires when the unused TLS section actually asks for TLS.
// A distributed deployment that supplies its own lease.Locker and configures no
// TLS keeps planning exactly as before.
func TestPrepareConfigAcceptsExternalLockerWithoutTLS(t *testing.T) {
	config := defaultConfig()
	config.Distributed.Enabled = true
	if _, err := prepareConfig(config); err != nil {
		t.Fatalf("prepareConfig() error = %v, want the external lease.Locker path accepted", err)
	}
}

// TestRedisClientOptionsLoadsTLSMaterial is the wiring test at the seam the
// plugin uses: the options handed to redis.NewClient carry the material built
// from the section.
func TestRedisClientOptionsLoadsTLSMaterial(t *testing.T) {
	certFile, keyFile := writeTestCertificatePair(t)
	options, err := redisClientOptions(RedisConfig{
		Addr: "redis.internal:6379",
		TLS: TLSConfig{
			CAFile:     certFile,
			CertFile:   certFile,
			KeyFile:    keyFile,
			ServerName: "redis.internal",
			MinVersion: "1.3",
		},
	})
	if err != nil {
		t.Fatalf("redisClientOptions() error = %v", err)
	}
	if options.TLSConfig == nil {
		t.Fatal("Redis options carry no TLSConfig")
	}
	if options.TLSConfig.RootCAs == nil {
		t.Fatal("TLSConfig.RootCAs = nil, want the configured CA bundle")
	}
	if options.TLSConfig.ServerName != "redis.internal" {
		t.Fatalf("TLSConfig.ServerName = %q, want redis.internal", options.TLSConfig.ServerName)
	}
	if options.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("TLSConfig.MinVersion = %#x, want TLS 1.3", options.TLSConfig.MinVersion)
	}
	if options.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLSConfig.InsecureSkipVerify = true, want false")
	}
	if len(options.TLSConfig.Certificates) != 1 {
		t.Fatalf("TLSConfig.Certificates = %d, want the configured client pair", len(options.TLSConfig.Certificates))
	}

	disabled, err := redisClientOptions(RedisConfig{Addr: "redis.internal:6379"})
	if err != nil {
		t.Fatalf("redisClientOptions(no TLS) error = %v", err)
	}
	if disabled.TLSConfig != nil {
		t.Fatalf("TLSConfig = %#v, want nil when the section configures no TLS", disabled.TLSConfig)
	}
}

func TestNewConfiguredPluginFailsOnUnreadableTLSMaterial(t *testing.T) {
	config := defaultConfig()
	config.Distributed.Enabled = true
	config.Distributed.Redis.Addr = "127.0.0.1:1"
	config.Distributed.Redis.TLS.CAFile = filepath.Join(t.TempDir(), "missing.pem")
	prepared, err := prepareConfig(config)
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	p, err := newConfiguredPlugin(prepared, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "distributed.redis.tls.ca_file") {
		t.Fatalf("newConfiguredPlugin() = (%v, %v), want an error naming distributed.redis.tls.ca_file", p, err)
	}
}

// TestNewConfiguredPluginNegotiatesTLSAgainstRedis closes the loop: the
// material the section declares is what the construction-time ping uses.
func TestNewConfiguredPluginNegotiatesTLSAgainstRedis(t *testing.T) {
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

	config := defaultConfig()
	config.Distributed.Enabled = true
	config.Distributed.Redis.Addr = server.Addr()
	config.Distributed.Redis.TLS.CAFile = certFile
	prepared, err := prepareConfig(config)
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	p, err := newConfiguredPlugin(prepared, nil, nil, nil)
	if err != nil {
		t.Fatalf("newConfiguredPlugin() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.stop(context.Background()); err != nil {
			t.Fatalf("stop() error = %v", err)
		}
	})

	client, ok := p.ownedRedis.(*redis.Client)
	if !ok {
		t.Fatalf("ownedRedis = %T, want *redis.Client", p.ownedRedis)
	}
	if client.Options().TLSConfig == nil {
		t.Fatal("owned Redis client carries no TLSConfig")
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
