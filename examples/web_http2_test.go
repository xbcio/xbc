// Package examples_test verifies application-level composition paths.
package examples_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
)

// TestPublicCompositionNegotiatesHTTP2 pins the implicit contract between
// transport/web and an engine adapter from outside both packages: Web wraps
// the listener with TLS and advertises h2 in ALPN, and the gin adapter must
// leave its own http.Server.TLSConfig nil so net/http still recognizes the
// *tls.Listener and installs its bundled HTTP/2 handler. An adapter that set
// a TLSConfig would make the same request negotiate http/1.1 (or fail), which
// is why this test boots the real composition through xbc.New rather than a
// package-internal fixture.
func TestPublicCompositionNegotiatesHTTP2(t *testing.T) {
	const shutdownTimeout = 2 * time.Second

	addr := freeAddress(t)
	certFile, keyFile, roots := writeHTTP2Certificate(t)

	configPath := filepath.Join(t.TempDir(), "application.yml")
	config := fmt.Sprintf(`xbc:
  shutdown_timeout: %s
log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: %q
  tls:
    cert_file: %q
    key_file: %q
    http2: true
`, shutdownTimeout, addr, certFile, keyFile)
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	probe := plugin.Define("http2-probe", func(plugin.BuildContext) (*http2ProbePlugin, error) {
		return &http2ProbePlugin{}, nil
	}, plugin.Options[*http2ProbePlugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *http2ProbePlugin) web.RouteContributor { return value }),
		),
	})

	app, err := xbc.New(xbc.WithBundles(web.Bundle(), ginengine.Bundle(), plugin.BundleOf(probe)))
	require.NoError(t, err)

	type executeResult struct {
		code int
		err  error
	}
	parent, cancel := context.WithCancel(context.Background())
	done := make(chan executeResult, 1)
	go func() {
		code, executeErr := app.Execute(parent, []string{"--config", configPath})
		done <- executeResult{code: code, err: executeErr}
	}()

	finished := false
	t.Cleanup(func() {
		cancel()
		if finished {
			return
		}
		select {
		case result := <-done:
			if result.err != nil {
				t.Errorf("Execute cleanup result: code=%d error=%v", result.code, result.err)
			}
		case <-time.After(shutdownTimeout + time.Second):
			t.Error("timed out waiting for Execute cleanup")
		}
	})

	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots},
			ForceAttemptHTTP2: true,
		},
	}
	t.Cleanup(client.CloseIdleConnections)
	endpoint := "https://" + addr + "/h2"

	var response *http.Response
	startupDeadline := time.Now().Add(probeWait)
	for {
		resp, requestErr := client.Get(endpoint)
		if requestErr == nil {
			response = resp
			break
		}
		select {
		case result := <-done:
			finished = true
			t.Fatalf("Execute returned before the h2 route answered: code=%d error=%v", result.code, result.err)
		default:
		}
		if time.Now().After(startupDeadline) {
			t.Fatalf("timed out waiting for the h2 route: %v", requestErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, 2, response.ProtoMajor,
		"the public composition with web.tls.http2 must negotiate HTTP/2 through the gin adapter")
	require.NotNil(t, response.TLS)
	assert.Equal(t, "h2", response.TLS.NegotiatedProtocol)

	cancel()
	select {
	case result := <-done:
		finished = true
		require.NoError(t, result.err)
		assert.Equal(t, 0, result.code)
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("timed out waiting for Execute to return")
	}
}

// http2ProbePlugin contributes one public route so the composition has
// something to answer over HTTP/2. It exists as a plugin rather than through
// the health bundle because the route is the assertion's subject.
type http2ProbePlugin struct{}

var _ web.RouteContributor = (*http2ProbePlugin)(nil)

func (*http2ProbePlugin) RegisterRoutes(router *web.Router) {
	router.GET("/h2", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusOK)
		return nil
	}).Name("http2.probe").Auth(web.Public())
}

// writeHTTP2Certificate writes a self-signed pair for 127.0.0.1 and the pool a
// client needs to verify it. It mirrors the transport/web test helper, which
// is not importable across modules; the generated name keeps it distinct from
// anything else in this package.
func writeHTTP2Certificate(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
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
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)

	dir := t.TempDir()
	certFile = filepath.Join(dir, "http2.crt")
	keyFile = filepath.Join(dir, "http2.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	encodedKey, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0o600))

	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	roots = x509.NewCertPool()
	roots.AddCert(certificate)
	return certFile, keyFile, roots
}
