package mailx

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	gomail "github.com/wneessen/go-mail"
)

// TestNewBuildsSenderAndCloseStopsIt covers the direct-construction seam: New
// returns a working Sender, Closer releases it, and a closed sender rejects
// further sends. The fake server supplies the transport; no real service is
// contacted.
func TestNewBuildsSenderAndCloseStopsIt(t *testing.T) {
	server := newFakeSMTPServer(t)
	config := bindConfig(t, serverValues(t, server))

	sender, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	closer, ok := sender.(Closer)
	if !ok {
		t.Fatalf("New() returned %T, which does not implement Closer", sender)
	}

	sendMessage(t, sender, "direct@example.com", "direct construction")
	assertSingleMessage(t, server, "direct construction")

	if err := closer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := sender.DialAndSend(gomail.NewMsg()); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after Close error = %v, want ErrClosed", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

// TestNewVerifiesConnectivityLikeThePlugin proves the verification probe runs
// on the direct path and leaves no message behind.
func TestNewVerifiesConnectivityLikeThePlugin(t *testing.T) {
	server := newFakeSMTPServer(t)
	values := serverValues(t, server)
	values["verify"] = true

	sender, err := New(bindConfig(t, values))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := sender.(Closer); ok {
			_ = closer.Close()
		}
	})
	server.waitForAccepted(t, 1)
	if messages := server.received(); len(messages) != 0 {
		t.Fatalf("the verification probe sent %d messages, want none", len(messages))
	}
}

// TestNewFailureNamesOnlyTheDefaultInstanceAndAddress pins the direct path's
// error contract: the default instance label and the address, never the
// password.
func TestNewFailureNamesOnlyTheDefaultInstanceAndAddress(t *testing.T) {
	const secret = "new-secret-password-value"
	host, port := closedAddress(t)
	config := bindConfig(t, map[string]any{
		"host":      host,
		"port":      port,
		"username":  "service",
		"password":  secret,
		"auth_type": "none",
		"tls":       map[string]any{"policy": "none"},
	})

	sender, err := New(config)
	if err == nil {
		t.Fatal("New() error = nil, want a refused connection")
	}
	if sender != nil {
		t.Fatalf("New() returned %T after a failed verification", sender)
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	for _, want := range []string{"mail: instance default", address} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error rendered the password: %q", err)
	}
}

// TestNewRejectsAnIncompleteConfig proves the exported path keeps the plugin's
// cross-field validation instead of deferring every mistake to the first send.
func TestNewRejectsAnIncompleteConfig(t *testing.T) {
	sender, err := New(Config{})
	if err == nil {
		t.Fatal("New() error = nil, want the missing host rejected")
	}
	if sender != nil {
		t.Fatalf("New() returned %T with an invalid config", sender)
	}
	if !strings.Contains(err.Error(), "host is required") {
		t.Fatalf("error = %q, want the missing host named", err)
	}
}
