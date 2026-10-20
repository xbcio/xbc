package websocket

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	coderwebsocket "github.com/coder/websocket"
)

// echoServe reads messages and writes each one back unchanged. A close frame
// ends the conversation without reporting an error: the peer closing is the
// normal end of a WebSocket, not a handler failure.
func echoServe(ctx context.Context, conn *Conn) error {
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			if coderwebsocket.CloseStatus(err) != -1 {
				return nil
			}
			return err
		}
		if err := conn.Write(ctx, kind, data); err != nil {
			return err
		}
	}
}

func TestHandlerUpgradesThroughTheFullStack(t *testing.T) {
	app := startTestApp(t, Options{}, echoServe, 0)

	conn, response, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if response == nil {
		t.Fatal("Dial() returned no handshake response")
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want %d", response.StatusCode, http.StatusSwitchingProtocols)
	}
	if !app.recorder.saw("/ws") {
		t.Fatal("the recording middleware did not observe the upgrade request")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, coderwebsocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	kind, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if kind != coderwebsocket.MessageText || string(data) != "hello" {
		t.Fatalf("Read() = %v %q, want text \"hello\"", kind, data)
	}

	if err := conn.Close(coderwebsocket.StatusNormalClosure, "test complete"); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitFor(t, 5*time.Second, "the handler to unregister the cleanly closed socket", app.plugin.registryEmpty)
}

func TestHandlerRefusesNonUpgradeRequestsWithoutAPartialUpgrade(t *testing.T) {
	app := startTestApp(t, Options{}, echoServe, 0)

	response, body := app.get(t, "/ws")
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plain GET status = %d, want %d", response.StatusCode, http.StatusUpgradeRequired)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("plain GET content type = %q, want the protocol library's text/plain refusal", contentType)
	}
	if bytes.Contains(body, []byte(`"code"`)) {
		t.Fatalf("plain GET body = %q, want no Problem Detail from Web after the protocol refusal", body)
	}
	if !app.recorder.saw("/ws") {
		t.Fatal("the recording middleware did not observe the plain GET")
	}

	response, body = app.get(t, "/ws-protected")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if !strings.Contains(response.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("unauthenticated content type = %q, want application/problem+json", response.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), `"code":"unauthenticated"`) {
		t.Fatalf("unauthenticated body = %q, want the problem code", body)
	}
	if response.Header.Get("Upgrade") == "websocket" {
		t.Fatal("the unauthenticated response carried an Upgrade header")
	}
	if app.recorder.saw("/ws-protected") {
		t.Fatal("the business middleware ran for a route the authentication phase rejected")
	}

	// The same request with a credential passes authentication, reaches the
	// upgrader, and is refused by the protocol rather than by the auth layer.
	response, _ = app.getWithHeaders(t, "/ws-protected", map[string]string{"X-Test-Credential": "alice"})
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("authenticated plain GET status = %d, want %d", response.StatusCode, http.StatusUpgradeRequired)
	}
	if !app.recorder.saw("/ws-protected") {
		t.Fatal("the recording middleware did not observe the authenticated request")
	}
}

func TestOriginPolicyAcceptsTheSameHostAndDeclaredPatterns(t *testing.T) {
	t.Run("same host", func(t *testing.T) {
		app := startTestApp(t, Options{}, echoServe, 0)
		conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{
			HTTPHeader: http.Header{"Origin": {"http://" + app.server.Addr()}},
		})
		if err != nil {
			t.Fatalf("Dial() with the server's own origin error = %v", err)
		}
		_ = conn.CloseNow()
	})

	t.Run("cross origin refused", func(t *testing.T) {
		app := startTestApp(t, Options{}, echoServe, 0)
		_, response, err := app.dial(t, "/ws", coderwebsocket.DialOptions{
			HTTPHeader: http.Header{"Origin": {"http://cross-origin.example"}},
		})
		if err == nil {
			t.Fatal("Dial() from a cross origin succeeded")
		}
		if response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-origin response = %v, want status %d", response, http.StatusForbidden)
		}
		if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
			t.Fatalf("cross-origin content type = %q, want the protocol library's text/plain refusal", contentType)
		}
		if body := readAll(t, response); !strings.Contains(string(body), "not authorized") {
			t.Fatalf("cross-origin body = %q, want the protocol library's refusal", body)
		}
	})

	t.Run("declared pattern", func(t *testing.T) {
		app := startTestApp(t, Options{OriginPatterns: []string{"cross-origin.example"}}, echoServe, 0)
		conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{
			HTTPHeader: http.Header{"Origin": {"http://cross-origin.example"}},
		})
		if err != nil {
			t.Fatalf("Dial() from a declared origin error = %v", err)
		}
		_ = conn.CloseNow()
	})
}

func TestNegotiationFollowsTheRouteOptions(t *testing.T) {
	t.Run("zero options negotiate nothing", func(t *testing.T) {
		app := startTestApp(t, Options{}, echoServe, 0)
		conn, response, err := app.dial(t, "/ws", coderwebsocket.DialOptions{
			Subprotocols:    []string{"chat.v1"},
			CompressionMode: coderwebsocket.CompressionNoContextTakeover,
		})
		if err != nil {
			t.Fatalf("Dial() error = %v", err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		if got := conn.Subprotocol(); got != "" {
			t.Fatalf("negotiated subprotocol = %q, want none", got)
		}
		if got := response.Header.Get("Sec-WebSocket-Extensions"); got != "" {
			t.Fatalf("negotiated extensions = %q, want none", got)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Write(ctx, coderwebsocket.MessageText, []byte("plain")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if _, data, err := conn.Read(ctx); err != nil || string(data) != "plain" {
			t.Fatalf("Read() = %q, %v, want \"plain\"", data, err)
		}
	})

	t.Run("declared subprotocol and compression", func(t *testing.T) {
		app := startTestApp(t, Options{Subprotocols: []string{"chat.v1"}, Compression: true}, echoServe, 0)
		conn, response, err := app.dial(t, "/ws", coderwebsocket.DialOptions{
			Subprotocols:    []string{"chat.v1"},
			CompressionMode: coderwebsocket.CompressionNoContextTakeover,
		})
		if err != nil {
			t.Fatalf("Dial() error = %v", err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		if got := conn.Subprotocol(); got != "chat.v1" {
			t.Fatalf("negotiated subprotocol = %q, want %q", got, "chat.v1")
		}
		if got := response.Header.Get("Sec-WebSocket-Extensions"); !strings.Contains(got, "permessage-deflate") {
			t.Fatalf("negotiated extensions = %q, want permessage-deflate", got)
		}

		// Above the compression threshold, so the round trip exercises the
		// negotiated extension instead of a small frame that is always sent raw.
		message := bytes.Repeat([]byte("xbc "), 1024)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Write(ctx, coderwebsocket.MessageBinary, message); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		kind, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if kind != coderwebsocket.MessageBinary || !bytes.Equal(data, message) {
			t.Fatalf("Read() = %v with %d bytes, want the %d-byte message back", kind, len(data), len(message))
		}
	})

	t.Run("read limit stops an oversized message", func(t *testing.T) {
		readErrors := make(chan error, 1)
		serve := func(ctx context.Context, conn *Conn) error {
			_, _, err := conn.Read(ctx)
			readErrors <- err
			return nil
		}
		app := startTestApp(t, Options{ReadLimit: 8}, serve, 0)
		conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
		if err != nil {
			t.Fatalf("Dial() error = %v", err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Write(ctx, coderwebsocket.MessageText, []byte("this message is longer than eight bytes")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		select {
		case err := <-readErrors:
			if !errors.Is(err, coderwebsocket.ErrMessageTooBig) {
				t.Fatalf("handler Read() error = %v, want ErrMessageTooBig", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the handler never observed the oversized message")
		}
	})
}

func TestAnOpenSocketHoldsOneAdmissionSlot(t *testing.T) {
	hold := newHoldServe()
	app := startTestApp(t, Options{}, hold.serve, 1)

	first, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err != nil {
		t.Fatalf("first Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = first.CloseNow() })
	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first handler never started")
	}

	// The handler blocks for the life of the socket, so the single admission
	// slot is still held while this request arrives: it must be refused before
	// the upgrader is reached.
	response, body := app.get(t, "/ws")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("request while the slot is held status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if !strings.Contains(string(body), `"code":"server_overloaded"`) {
		t.Fatalf("request while the slot is held body = %q, want server_overloaded", body)
	}

	// Releasing the socket releases the slot.
	if err := first.Close(coderwebsocket.StatusNormalClosure, "release the slot"); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitFor(t, 5*time.Second, "the admission slot to be released", func() bool {
		response, _ := app.get(t, "/ws")
		return response.StatusCode != http.StatusServiceUnavailable
	})
}
