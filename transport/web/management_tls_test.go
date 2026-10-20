package web_test

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// TestManagementClientCertificatesAreRequiredOnlyOnTheManagementListener is
// the handshake half of management.client_ca_file: the setting has to reach the
// management listener as a verification policy, and it must not reach the
// serving listener. Both halves need a real socket -- a recorder cannot show a
// refused handshake, and it cannot show a request that was served without a
// client certificate because the policy stayed where it was configured.
//
// The accepted client is what makes the two refusals evidence: a listener that
// refused every client would satisfy both negative assertions but fail the
// first one, and one that demanded nothing would fail the refusals while
// keeping the first.
func TestManagementClientCertificatesAreRequiredOnlyOnTheManagementListener(t *testing.T) {
	dir := t.TempDir()
	caFile, ca, caKey := writeClientCA(t, dir, "management-client-ca")
	// The second authority carries the configured one's subject with a
	// different key. A client picks a certificate by issuer name, so this one is
	// really offered to the server and can only be refused by verifying its
	// signature -- which is the difference between requiring a certificate and
	// verifying one.
	_, foreignCA, foreignKey := writeClientCA(t, t.TempDir(), "management-client-ca")
	certFile, keyFile, roots := writeTestCertificate(t)

	cfg := managementConfig()
	cfg.TLS = web.TLSConfig{CertFile: certFile, KeyFile: keyFile}
	cfg.Management.ClientCAFile = caFile
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	servingAddr := server.Addr()
	managementAddr := server.ManagementAddr()
	require.NotEmpty(t, servingAddr)
	require.NotEmpty(t, managementAddr, "a configured management address must be bound")
	require.NotEqual(t, servingAddr, managementAddr, "the planes must not share one listener")

	client := func(certificates ...tls.Certificate) *http.Client {
		return &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:      roots,
					Certificates: certificates,
					NextProtos:   []string{"http/1.1"},
				},
				ForceAttemptHTTP2: false,
			},
		}
	}
	managementURL := "https://" + managementAddr + "/-/metrics"

	var response *http.Response
	// The deadline bounds failure detection only: the gate has been released,
	// but a handshake-driven start can trail the release under a loaded race
	// build, and a short window would report that as a policy failure.
	require.True(t, pollUntil(5*time.Second, 20*time.Millisecond, func() bool {
		resp, err := client(writeClientCertificate(t, ca, caKey, 1)).Get(managementURL)
		if err != nil {
			return false
		}
		response = resp
		return true
	}), "a client certificate signed by management.client_ca_file must reach the management listener")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, managementMarker, string(body), "the accepted request must be the management route's own handler")

	// No certificate at all. Depending on the negotiated TLS version the refusal
	// surfaces during the handshake or at the first read, so the assertion is on
	// the request failing, not on where it failed.
	_, err = client().Get(managementURL)
	assert.Error(t, err, "a management listener with a client CA must refuse a client that presents no certificate")

	// A certificate the configured bundle did not issue, offered under the same
	// issuer name so the server really has to verify it.
	_, err = client(writeClientCertificate(t, foreignCA, foreignKey, 2)).Get(managementURL)
	assert.Error(t, err, "a certificate from another CA must fail verification")

	// The serving listener keeps web.tls.client_auth: none -- no client
	// certificate here -- so a policy configured for the management plane must
	// not demand one from business traffic.
	serving := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots, NextProtos: []string{"http/1.1"}},
			ForceAttemptHTTP2: false,
		},
	}
	var servingResponse *http.Response
	require.True(t, pollUntil(5*time.Second, 20*time.Millisecond, func() bool {
		resp, err := serving.Get("https://" + servingAddr + "/ping")
		if err != nil {
			return false
		}
		servingResponse = resp
		return true
	}), "a client presenting no certificate must still reach the serving listener")
	defer servingResponse.Body.Close()
	assert.Equal(t, http.StatusOK, servingResponse.StatusCode,
		"management mTLS must stay on the management listener; web.tls.client_auth is none for business traffic")
}

// TestManagementClientCAStartupErrorsNameTheFile pins the split the validator
// deliberately keeps: parsing the bundle is I/O, so it happens where the
// listener is assembled and fails Start with the path in the message. The
// serving listener is bound before the management listener is assembled, so a
// bad bundle must also give that socket back -- asserted by re-binding the
// serving address, which is only possible once it is really closed.
func TestManagementClientCAStartupErrorsNameTheFile(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeTestCertificate(t)
	malformed := filepath.Join(dir, "management-ca.pem")
	require.NoError(t, os.WriteFile(malformed, []byte("not a certificate"), 0o600))
	empty := filepath.Join(dir, "empty-management-ca.pem")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "missing file", path: filepath.Join(dir, "absent-management-ca.pem")},
		{name: "non-PEM file", path: malformed},
		{name: "empty file", path: empty},
	} {
		t.Run(test.name, func(t *testing.T) {
			reserved, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			servingAddr := reserved.Addr().String()
			require.NoError(t, reserved.Close())

			cfg := web.Config{
				Addr:       servingAddr,
				BasePath:   "/",
				Management: web.ManagementConfig{Addr: "127.0.0.1:0", ClientCAFile: test.path},
				TLS:        web.TLSConfig{CertFile: certFile, KeyFile: keyFile},
			}
			server, ctx, host := newPingServer(t, cfg, serverInputs{})

			err = server.Start(ctx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.path, "a bundle that cannot be used must name the file to fix")
			assert.Contains(t, err.Error(), "management.client_ca_file",
				"the error must name the setting the operator configured, not the serving listener's key")
			assert.NotContains(t, err.Error(), "tls.client_ca_file",
				"a management bundle failure must not be reported as a tls.client_ca_file problem")
			assert.Empty(t, server.Addr())
			assert.Empty(t, server.ManagementAddr())
			assert.Empty(t, host.submittedTasks(), "a failed Start must admit no serving task")

			rebound, err := net.Listen("tcp", servingAddr)
			require.NoError(t, err, "the serving listener bound before the management CA failure must not be left open")
			require.NoError(t, rebound.Close())
		})
	}
}

// TestPaddedManagementClientCAPathIsTrimmed pins that
// management.client_ca_file is read the way tls.client_ca_file is: the
// validator decides presence through TrimSpace, so a padded path names the same
// bundle and must not fail Start on a path that differs only by the spaces
// around it. Both handshakes are asserted -- accepted and refused -- so a Start
// that trimmed the path for the read but left an empty pool cannot pass on the
// refusal alone.
func TestPaddedManagementClientCAPathIsTrimmed(t *testing.T) {
	dir := t.TempDir()
	caFile, ca, caKey := writeClientCA(t, dir, "management-client-ca")
	certFile, keyFile, roots := writeTestCertificate(t)

	cfg := managementConfig()
	cfg.TLS = web.TLSConfig{CertFile: certFile, KeyFile: keyFile}
	cfg.Management.ClientCAFile = "  " + caFile + "  "

	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})
	require.NoError(t, server.Start(ctx), "a padded path names the same bundle and must start")
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	managementAddr := server.ManagementAddr()
	require.NotEmpty(t, managementAddr, "a configured management address must be bound")

	client := func(certificates ...tls.Certificate) *http.Client {
		return &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:      roots,
					Certificates: certificates,
					NextProtos:   []string{"http/1.1"},
				},
				ForceAttemptHTTP2: false,
			},
		}
	}
	var response *http.Response
	require.True(t, pollUntil(5*time.Second, 20*time.Millisecond, func() bool {
		resp, err := client(writeClientCertificate(t, ca, caKey, 1)).Get("https://" + managementAddr + "/-/metrics")
		if err != nil {
			return false
		}
		response = resp
		return true
	}), "the padded CA path must verify the certificate it names")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)

	_, err := client().Get("https://" + managementAddr + "/-/metrics")
	assert.Error(t, err, "the padded path must still require a client certificate, not merely load")
}

// TestManagementClientCAFailureNeverAsksForTheManagementSocket pins the order
// inside Start: the CA bundle is read before the management socket is
// requested, so a bad bundle cannot leave a bound operator port behind. A
// listener provider is the only seam that can observe "never asked" -- a failed
// Start reports no ManagementAddr whether the socket was never bound or was
// bound and closed -- and the handover also shows the serving socket was given
// back to the test, which still holds it.
func TestManagementClientCAFailureNeverAsksForTheManagementSocket(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeTestCertificate(t)
	missingCA := filepath.Join(dir, "absent-management-ca.pem")

	var handed []*closeRecordingListener
	provider := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		recorded := &closeRecordingListener{Listener: ln}
		handed = append(handed, recorded)
		return recorded, nil
	}}

	cfg := web.Config{
		Addr:       "127.0.0.1:0",
		BasePath:   "/",
		Management: web.ManagementConfig{Addr: "127.0.0.1:0", ClientCAFile: missingCA},
		TLS:        web.TLSConfig{CertFile: certFile, KeyFile: keyFile},
	}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	server.SetListenerProvider(providerEntry("tcp-provider", provider))

	err := server.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "management.client_ca_file")
	assert.Equal(t, []string{cfg.Addr}, provider.addresses(),
		"the management plane must never ask for its socket when the CA bundle cannot be loaded")
	require.Len(t, handed, 1, "only the serving plane may have been handed a socket")
	assert.True(t, handed[0].closed.Load(), "the serving listener handed over before the failure must be closed")
	assert.Empty(t, server.Addr())
	assert.Empty(t, server.ManagementAddr())
	assert.Empty(t, host.submittedTasks(), "a failed Start must admit no serving task")
}

// closeRecordingListener records the Close the Server performs on a
// provider-supplied listener. Start closes the serving socket synchronously on
// the management CA failure path, so a flag would suffice; the atomic keeps the
// race detector satisfied without relying on that ordering.
type closeRecordingListener struct {
	net.Listener
	closed atomic.Bool
}

func (l *closeRecordingListener) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

// TestRemoteManagementWithMutualTLSBindsThroughTheListenerProvider covers the
// matrix's accepting row end to end on a host this test cannot depend on: the
// configured address is remote, the provider supplies a loopback socket, and
// the management listener still demands the configured client certificate.
func TestRemoteManagementWithMutualTLSBindsThroughTheListenerProvider(t *testing.T) {
	dir := t.TempDir()
	caFile, ca, caKey := writeClientCA(t, dir, "management-client-ca")
	certFile, keyFile, roots := writeTestCertificate(t)

	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Management = web.ManagementConfig{
		Addr:         "0.0.0.0:9091",
		AllowRemote:  true,
		ClientCAFile: caFile,
	}
	cfg.TLS = web.TLSConfig{CertFile: certFile, KeyFile: keyFile}

	provider := &recordingListenerProvider{listen: func(string) (net.Listener, error) {
		return net.Listen("tcp", "127.0.0.1:0")
	}}
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{managementContributor("/-/metrics")},
	})
	server.SetListenerProvider(providerEntry("tcp-provider", provider))

	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	managementAddr := server.ManagementAddr()
	require.NotEmpty(t, managementAddr, "the provider-supplied management socket must be reported")
	assert.Equal(t, []string{cfg.Addr, cfg.Management.Addr}, provider.addresses(),
		"each plane must ask the provider with its own configured address")

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      roots,
				Certificates: []tls.Certificate{writeClientCertificate(t, ca, caKey, 1)},
				NextProtos:   []string{"http/1.1"},
			},
			ForceAttemptHTTP2: false,
		},
	}
	var response *http.Response
	require.True(t, pollUntil(5*time.Second, 20*time.Millisecond, func() bool {
		resp, err := client.Get("https://" + managementAddr + "/-/metrics")
		if err != nil {
			return false
		}
		response = resp
		return true
	}), "a validated remote management configuration must bind and serve its plane")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
}
