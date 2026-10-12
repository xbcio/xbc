package httpclient

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeServerCA exports the test server's self-signed certificate as the PEM
// bundle an operator would configure in tls.ca_file.
func writeServerCA(t *testing.T, server *httptest.Server) string {
	t.Helper()
	certificate := server.Certificate()
	require.NotNil(t, certificate)
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificate.Raw,
	}), 0o600))
	return path
}

func TestConfiguredCAClientVerifiesThePeer(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{
		BaseURL: server.URL,
		TLS:     TLSConfig{CAFile: writeServerCA(t, server)},
	}, nil, nil)
	response, err := client.Client().Get(server.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func TestUnknownAuthorityIsRejected(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL}, nil, nil)
	_, err := client.Client().Get(server.URL)
	require.Error(t, err, "a self-signed peer must fail verification against the system roots")
	assert.Contains(t, err.Error(), "certificate")
}

func TestUnreadableOrEmptyCABundleFailsConstruction(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.pem")
	_, err := newClient("test", mustNormalize(t, Config{TLS: TLSConfig{CAFile: missing}}), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ca_file")

	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
	_, err = newClient("test", mustNormalize(t, Config{TLS: TLSConfig{CAFile: empty}}), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PEM")
}

func TestNoTLSMaterialKeepsGoDefaults(t *testing.T) {
	tlsConfig, err := (TLSConfig{}).clientTLSConfig()
	require.NoError(t, err)
	assert.Nil(t, tlsConfig, "no TLS configuration means the system roots and no client certificate")
}
