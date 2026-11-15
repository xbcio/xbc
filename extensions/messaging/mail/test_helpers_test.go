package mail

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	xbcconfig "github.com/xbcio/xbc/config"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// fakeMessage is one message the fake SMTP server accepted.
type fakeMessage struct {
	mailFrom   string
	recipients []string
	data       string
}

// fakeSMTPServer is a minimal SMTP endpoint. It speaks just enough of the
// protocol for go-mail's dial, transaction, and quit paths, advertises neither
// STARTTLS nor AUTH, and records what it received. Tests therefore need no
// network access beyond the loopback interface.
type fakeSMTPServer struct {
	listener net.Listener
	wg       sync.WaitGroup

	mu       sync.Mutex
	accepted int
	messages []fakeMessage
	conns    map[net.Conn]struct{}
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	server := &fakeSMTPServer{listener: listener, conns: make(map[net.Conn]struct{})}
	server.wg.Add(1)
	go server.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		server.mu.Lock()
		for connection := range server.conns {
			_ = connection.Close()
		}
		server.mu.Unlock()
		server.wg.Wait()
	})
	return server
}

func (s *fakeSMTPServer) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	return host, port
}

func (s *fakeSMTPServer) acceptedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

func (s *fakeSMTPServer) received() []fakeMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeMessage(nil), s.messages...)
}

func (s *fakeSMTPServer) serve() {
	defer s.wg.Done()
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.accepted++
		s.conns[connection] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, connection)
				s.mu.Unlock()
				_ = connection.Close()
			}()
			s.handle(connection)
		}()
	}
}

func (s *fakeSMTPServer) handle(connection net.Conn) {
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	write := func(line string) bool {
		if _, err := writer.WriteString(line + "\r\n"); err != nil {
			return false
		}
		return writer.Flush() == nil
	}
	if !write("220 fake ESMTP ready") {
		return
	}
	var message fakeMessage
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			if !write("250-fake") || !write("250-8BITMIME") || !write("250 SMTPUTF8") {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			message.mailFrom = strings.TrimSpace(line[len("MAIL FROM:"):])
			if !write("250 2.1.0 Ok") {
				return
			}
		case strings.HasPrefix(upper, "RCPT TO:"):
			message.recipients = append(message.recipients, strings.TrimSpace(line[len("RCPT TO:"):]))
			if !write("250 2.1.5 Ok") {
				return
			}
		case upper == "DATA":
			if !write("354 End data with <CR><LF>.<CR><LF>") {
				return
			}
			var body strings.Builder
			for {
				dataLine, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				if dataLine == ".\r\n" {
					break
				}
				body.WriteString(dataLine)
			}
			message.data = body.String()
			s.mu.Lock()
			s.messages = append(s.messages, message)
			s.mu.Unlock()
			message = fakeMessage{}
			if !write("250 2.0.0 Ok: queued") {
				return
			}
		case upper == "QUIT":
			_ = write("221 2.0.0 Bye")
			return
		case upper == "RSET":
			message = fakeMessage{}
			if !write("250 2.0.0 Ok") {
				return
			}
		default:
			if !write("250 2.0.0 Ok") {
				return
			}
		}
	}
}

// waitForAccepted blocks until the server observed want connections or fails
// the test after a bounded wait.
func (s *fakeSMTPServer) waitForAccepted(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.acceptedCount() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("server accepted %d connections, want %d", s.acceptedCount(), want)
}

// serverValues returns a configuration that points at the fake server with
// verification disabled, the plain-SMTP policy, and no authentication. Tests
// override individual keys before binding.
func serverValues(t *testing.T, server *fakeSMTPServer) map[string]any {
	t.Helper()
	host, port := server.hostPort(t)
	return map[string]any{
		"host":      host,
		"port":      port,
		"auth_type": "none",
		"tls":       map[string]any{"policy": "none"},
		"verify":    false,
	}
}

// closedAddress returns a loopback host:port that was just released, so a
// connection attempt is refused immediately.
func closedAddress(t *testing.T) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback address: %v", err)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split released address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse released port: %v", err)
	}
	return host, port
}

func newEnvironment(t *testing.T, instances map[string]any) *xbcconfig.Environment {
	t.Helper()
	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"mail": instances,
		},
	}, "XBC_MAIL_TEST_UNSET_")
	if err != nil {
		t.Fatalf("config.NewEnvironment: %v", err)
	}
	return environment
}

// bindConfig binds and prepares one plugins.mail.default section.
func bindConfig(t *testing.T, values map[string]any) Config {
	t.Helper()
	config, err := bindPreparedConfig(values)
	if err != nil {
		t.Fatalf("bind prepared config: %v", err)
	}
	return config
}

func bindConfigWithoutValidation(values map[string]any) (Config, error) {
	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"mail": map[string]any{"default": values},
		},
	}, "XBC_MAIL_TEST_UNSET_")
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := environment.Bind("plugins.mail.default", &config); err != nil {
		return Config{}, err
	}
	return config, nil
}

// bindPreparedConfig runs the assembly path for one plugins.mail.default
// section: bind with defaults, validate the tags, then prepare. Prepare is
// taken from the Definition rather than called directly so that a ConfigSpec
// which stopped wiring it fails here.
func bindPreparedConfig(values map[string]any) (Config, error) {
	config, err := bindConfigWithoutValidation(values)
	if err != nil {
		return Config{}, err
	}
	if err := xbcconfig.Validate(&config, "plugins.mail.default"); err != nil {
		return Config{}, err
	}
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(definition))
	if !ok || descriptor.Config == nil || descriptor.Config.Prepare == nil {
		return Config{}, fmt.Errorf("mail definition declares no configuration Prepare")
	}
	prepared, err := descriptor.Config.Prepare(config)
	if err != nil {
		return Config{}, err
	}
	result, ok := prepared.(Config)
	if !ok {
		return Config{}, fmt.Errorf("Prepare returned %T, want Config", prepared)
	}
	return result, nil
}
