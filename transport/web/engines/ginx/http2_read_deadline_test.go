package ginx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// http2ReadDeadlineObservation is what the handler saw while it read a stalled
// HTTP/2 body under a read deadline it had set for itself.
type http2ReadDeadlineObservation struct {
	proto   string
	setErr  error
	readErr error
	elapsed time.Duration
	ctxErr  error
}

// TestAHandlerReadDeadlineOverHTTP2IsMappedTo500 drives the read-deadline stall
// through this adapter over a real HTTP/2 connection. It is the gin-side twin
// of the web-level row in transport/web/http2_test.go, and it exists because
// the adapter sits between the handler and the stream: the deadline has to
// reach the stream through gin's writer, and the error the handler returns has
// to reach the framework's boundary unchanged.
//
// The expected answer is 500 rather than 408, and the row records why: the
// expired HTTP/2 stream read deadline leaves the request context alive, so the
// boundary cannot tell this stall from an upstream call's deadline. That is
// the standard library's behavior and the contract documented under HTTP error
// contract in package web; if it ever changes, this row turns red rather than
// the behavior changing silently.
func TestAHandlerReadDeadlineOverHTTP2IsMappedTo500(t *testing.T) {
	addr, roots, observed := serveHTTP2ReadDeadlineEngine(t)

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots},
			ForceAttemptHTTP2: true,
		},
	}
	t.Cleanup(client.CloseIdleConnections)

	// The body never completes on purpose, so a handler that answered without
	// reading it would leave the client's write loop blocked on this pipe past
	// any client timeout. The watchdog turns that into a failed request instead
	// of a hung test.
	body, writer := io.Pipe()
	watchdog := time.AfterFunc(4*time.Second, func() { _ = writer.CloseWithError(errors.New("watchdog: body still open")) })
	t.Cleanup(func() { watchdog.Stop(); _ = writer.Close() })
	go func() { _, _ = writer.Write([]byte("abc")) }()
	request, err := http.NewRequest(http.MethodPost, "https://"+addr+"/stall", body)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "text/plain")

	response, err := client.Do(request)
	require.NoError(t, err, "the server must answer instead of leaving the stalled client hanging")
	defer response.Body.Close()

	var observation http2ReadDeadlineObservation
	select {
	case observation = <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never reported what its read saw")
	}
	assert.Equal(t, "HTTP/2.0", observation.proto)
	require.NoError(t, observation.setErr, "SetReadDeadline must reach the HTTP/2 stream through gin's writer")
	assert.ErrorIs(t, observation.readErr, os.ErrDeadlineExceeded,
		"the stalled read must end with the deadline error, not with EOF or a reset")
	assert.GreaterOrEqual(t, observation.elapsed, 100*time.Millisecond,
		"the read must have waited for the deadline rather than failed at once")
	assert.Less(t, observation.elapsed, 3*time.Second,
		"only the deadline can end this read, because the client never finishes its body")
	assert.NoError(t, observation.ctxErr,
		"an expired HTTP/2 stream read deadline leaves the request context alive")

	assert.Equal(t, 2, response.ProtoMajor, "the row must run on HTTP/2")
	assert.Equal(t, http.StatusInternalServerError, response.StatusCode,
		"a live request context keeps the read deadline at the framework's 500")
	assert.Contains(t, readBody(t, response), `"internal_server_error"`)
}

// serveHTTP2ReadDeadlineEngine serves one /stall route on a real gin engine
// behind a TLS listener that negotiates h2. The adapter's http.Server carries no
// TLSConfig of its own, so net/http's Serve enables its bundled HTTP/2 handler
// for the *tls.Conn the listener yields.
func serveHTTP2ReadDeadlineEngine(t *testing.T) (string, *x509.CertPool, <-chan http2ReadDeadlineObservation) {
	t.Helper()
	restoreProcessGlobals(t)

	engine, err := Factory{}.NewEngine(web.Options{})
	require.NoError(t, err)

	observed := make(chan http2ReadDeadlineObservation, 1)
	engine.Handle(http.MethodPost, "/stall", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			observation := http2ReadDeadlineObservation{proto: c.Request().Proto}
			observation.setErr = http.NewResponseController(c.Writer()).SetReadDeadline(time.Now().Add(readDeadlineBudget))
			if observation.setErr != nil {
				observed <- observation
				return observation.setErr
			}
			started := time.Now()
			_, err := io.ReadAll(c.Request().Body)
			observation.elapsed = time.Since(started)
			observation.readErr = err
			observation.ctxErr = c.Request().Context().Err()
			observed <- observation
			return err
		},
	})

	certificate, leaf := newSelfSignedCertificate(t)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"h2"},
	})
	served := make(chan error, 1)
	go func() { served <- engine.Serve(tlsListener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, engine.Shutdown(ctx))
		require.ErrorIs(t, <-served, http.ErrServerClosed)
	})
	return listener.Addr().String(), roots, observed
}

func newSelfSignedCertificate(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, leaf
}
