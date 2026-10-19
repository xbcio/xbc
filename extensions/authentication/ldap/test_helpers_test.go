package ldap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// testProviderName is the provider name most tests register.
const testProviderName = "corp"

// testConfig returns a valid configuration for the given directory URL. TLS
// trust and address-policy switches are filled in by the caller.
func testConfig(rawURL string) Config {
	return Config{
		Name:                testProviderName,
		Issuer:              "ldaps://directory.test",
		URL:                 rawURL,
		ServiceBindDN:       "cn=xbc-search,ou=services,dc=example,dc=com",
		ServiceBindPassword: outbound.Secret("service-secret"),
		BaseDN:              "ou=people,dc=example,dc=com",
		UserFilter:          "(uid=%s)",
		SubjectAttribute:    "entryUUID",
		ClaimAttributes:     map[string]string{"email": "mail", "display_name": "cn"},
		AllowPrivate:        true,
		DialTimeout:         2 * time.Second,
		OperationTimeout:    2 * time.Second,
		Now:                 func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
	}
}

// newProvider builds a provider from config, failing the test on error.
func newProvider(t *testing.T, config Config) *Provider {
	t.Helper()
	provider, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

// fakeDirectory records every call so tests can prove when the network seam is
// never reached and what distinguished name and password the user bind used.
type fakeDirectory struct {
	lookupResult userEntry
	lookupErr    error
	bindErr      error

	lookups atomic.Int32
	binds   atomic.Int32

	mu           sync.Mutex
	lastUsername string
	lastDN       string
	lastPassword string
}

func (d *fakeDirectory) lookupUser(_ context.Context, username string) (userEntry, error) {
	d.lookups.Add(1)
	d.mu.Lock()
	d.lastUsername = username
	d.mu.Unlock()
	if d.lookupErr != nil {
		return userEntry{}, d.lookupErr
	}
	return d.lookupResult, nil
}

func (d *fakeDirectory) authenticate(_ context.Context, dn, password string) error {
	d.binds.Add(1)
	d.mu.Lock()
	d.lastDN = dn
	d.lastPassword = password
	d.mu.Unlock()
	return d.bindErr
}

// staticResolver pins name resolution to a fixed answer.
type staticResolver struct {
	addresses map[string][]netip.Addr
	err       error

	calls    atomic.Int32
	lastHost atomic.Value
}

func (r *staticResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	r.calls.Add(1)
	r.lastHost.Store(host)
	if r.err != nil {
		return nil, r.err
	}
	addresses, ok := r.addresses[host]
	if !ok {
		return nil, fmt.Errorf("no static answer for %q", host)
	}
	return addresses, nil
}

// testCA is an in-memory certificate authority for TLS tests.
type testCA struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pool        *x509.CertPool
}

// newTestCA creates a self-signed authority and returns it with a pool that
// trusts only it.
func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "xbc ldap test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return &testCA{certificate: certificate, key: key, pool: pool}
}

// issueServerCertificate creates a leaf certificate for the given DNS names
// and IP addresses.
func (ca *testCA) issueServerCertificate(t *testing.T, dnsNames []string, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "xbc ldap test server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// --- Minimal BER codec for the fake directory ------------------------------

const (
	berTagInteger       = 0x02
	berTagOctetString   = 0x04
	berTagEnumerated    = 0x0a
	berTagSequence      = 0x30
	berTagSet           = 0x31
	berTagBindRequest   = 0x60
	berTagBindResponse  = 0x61
	berTagUnbindRequest = 0x42
	berTagSearchRequest = 0x63
	berTagSearchEntry   = 0x64
	berTagSearchDone    = 0x65
	berTagExtendedReq   = 0x77
	berTagExtendedResp  = 0x78
	berTagContextZero   = 0x80
)

// berTLV encodes one definite-length tag-length-value element.
func berTLV(tag byte, content []byte) []byte {
	encoded := []byte{tag}
	length := len(content)
	switch {
	case length < 0x80:
		encoded = append(encoded, byte(length))
	default:
		var lengthBytes [8]byte
		binary.BigEndian.PutUint64(lengthBytes[:], uint64(length))
		first := 0
		for first < len(lengthBytes) && lengthBytes[first] == 0 {
			first++
		}
		encoded = append(encoded, byte(0x80|(len(lengthBytes)-first)))
		encoded = append(encoded, lengthBytes[first:]...)
	}
	return append(encoded, content...)
}

func berInt(tag byte, value int) []byte {
	if value == 0 {
		return berTLV(tag, []byte{0})
	}
	var encoded []byte
	for remaining := value; remaining > 0; remaining >>= 8 {
		encoded = append([]byte{byte(remaining)}, encoded...)
	}
	return berTLV(tag, encoded)
}

func berString(tag byte, value string) []byte { return berTLV(tag, []byte(value)) }

func berChildren(tag byte, children ...[]byte) []byte {
	var content []byte
	for _, child := range children {
		content = append(content, child...)
	}
	return berTLV(tag, content)
}

// berMessage wraps a protocol operation in an LDAPMessage.
func berMessage(id int, operation []byte) []byte {
	return berChildren(berTagSequence, berInt(berTagInteger, id), operation)
}

// tlv is one parsed BER element.
type tlv struct {
	tag     byte
	content []byte
}

// readTLV reads one definite-length BER element from stream.
func readTLV(stream io.Reader) (tlv, error) {
	var header [2]byte
	if _, err := io.ReadFull(stream, header[:1]); err != nil {
		return tlv{}, err
	}
	if _, err := io.ReadFull(stream, header[1:2]); err != nil {
		return tlv{}, err
	}
	length := int(header[1])
	if length&0x80 != 0 {
		count := length & 0x7f
		if count == 0 || count > 4 {
			return tlv{}, errors.New("unsupported BER length")
		}
		lengthBytes := make([]byte, count)
		if _, err := io.ReadFull(stream, lengthBytes); err != nil {
			return tlv{}, err
		}
		length = 0
		for _, b := range lengthBytes {
			length = length<<8 | int(b)
		}
	}
	content := make([]byte, length)
	if _, err := io.ReadFull(stream, content); err != nil {
		return tlv{}, err
	}
	return tlv{tag: header[0], content: content}, nil
}

// parseTLVs splits a constructed element's content into children.
func parseTLVs(content []byte) ([]tlv, error) {
	var result []tlv
	for len(content) > 0 {
		tag := content[0]
		if len(content) < 2 {
			return nil, errors.New("truncated BER element")
		}
		length := int(content[1])
		offset := 2
		if length&0x80 != 0 {
			count := length & 0x7f
			if count == 0 || count > 4 || len(content) < 2+count {
				return nil, errors.New("unsupported BER length")
			}
			length = 0
			for _, b := range content[2 : 2+count] {
				length = length<<8 | int(b)
			}
			offset = 2 + count
		}
		if len(content) < offset+length {
			return nil, errors.New("truncated BER content")
		}
		result = append(result, tlv{tag: tag, content: content[offset : offset+length]})
		content = content[offset+length:]
	}
	return result, nil
}

// berInteger decodes a non-negative integer element.
func berInteger(value tlv) (int, error) {
	result := 0
	for _, b := range value.content {
		result = result<<8 | int(b)
	}
	return result, nil
}

// --- Fake LDAP directory server --------------------------------------------

// bindObservation records one bind request.
type bindObservation struct {
	dn       string
	password string
}

// searchObservation records one search request.
type searchObservation struct {
	base       string
	sizeLimit  int
	filter     tlv
	attributes []string
}

// fakeEntry is one entry the fake directory returns for a search.
type fakeEntry struct {
	dn         string
	attributes map[string][]string
}

// fakeDirectoryServer is a minimal LDAP server: simple bind, subtree search,
// unbind, and the StartTLS extended operation. It exists so the real dial,
// TLS, filter, and bind code can be exercised without a directory product.
type fakeDirectoryServer struct {
	listener       net.Listener
	tlsConfig      *tls.Config
	implicitTLS    bool
	enableStartTLS bool

	serviceDN       string
	servicePassword string
	userDN          string
	userPassword    string
	serviceBindCode uint16
	userBindCode    uint16
	entries         []fakeEntry
	searchDoneCode  uint16
	hangBinds       bool
	hang            bool

	accepted         atomic.Int32
	startTLSRequests atomic.Int32

	mu       sync.Mutex
	binds    []bindObservation
	searches []searchObservation
}

// newFakeDirectoryServer starts a server on loopback. implicitTLS wraps every
// connection; otherwise the server speaks plaintext and enables StartTLS when
// enableStartTLS is set.
func newFakeDirectoryServer(t *testing.T, tlsConfig *tls.Config, implicitTLS, enableStartTLS bool) *fakeDirectoryServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeDirectoryServer{
		listener:        listener,
		tlsConfig:       tlsConfig,
		implicitTLS:     implicitTLS,
		enableStartTLS:  enableStartTLS,
		serviceDN:       "cn=xbc-search,ou=services,dc=example,dc=com",
		servicePassword: "service-secret",
		userDN:          "uid=alice,ou=people,dc=example,dc=com",
		userPassword:    "user-secret",
		entries: []fakeEntry{{
			dn: "uid=alice,ou=people,dc=example,dc=com",
			attributes: map[string][]string{
				"uid":       {"alice"},
				"entryUUID": {"11111111-2222-3333-4444-555555555555"},
				"mail":      {"alice@example.com"},
				"cn":        {"Alice"},
			},
		}},
	}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func (s *fakeDirectoryServer) port() string {
	return fmt.Sprintf("%d", s.listener.Addr().(*net.TCPAddr).Port)
}

func (s *fakeDirectoryServer) acceptedCount() int { return int(s.accepted.Load()) }

func (s *fakeDirectoryServer) observations() ([]bindObservation, []searchObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bindObservation(nil), s.binds...), append([]searchObservation(nil), s.searches...)
}

func (s *fakeDirectoryServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.accepted.Add(1)
		go s.handle(conn)
	}
}

func (s *fakeDirectoryServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	stream := conn
	if s.implicitTLS {
		tlsConn := tls.Server(conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		stream = tlsConn
	}
	for {
		message, err := readTLV(stream)
		if err != nil {
			return
		}
		children, err := parseTLVs(message.content)
		if err != nil || len(children) < 2 {
			return
		}
		id, err := berInteger(children[0])
		if err != nil {
			return
		}
		operation := children[1]
		switch operation.tag {
		case berTagBindRequest:
			if !s.handleBind(stream, id, operation) {
				return
			}
		case berTagSearchRequest:
			s.handleSearch(stream, id, operation)
		case berTagUnbindRequest:
			return
		case berTagExtendedReq:
			s.startTLSRequests.Add(1)
			if !s.enableStartTLS {
				_ = writeAll(stream, berMessage(id, berChildren(berTagExtendedResp,
					berInt(berTagEnumerated, 2), berString(berTagOctetString, ""), berString(berTagOctetString, ""))))
				return
			}
			if err := writeAll(stream, berMessage(id, berChildren(berTagExtendedResp,
				berInt(berTagEnumerated, 0), berString(berTagOctetString, ""), berString(berTagOctetString, "")))); err != nil {
				return
			}
			tlsConn := tls.Server(stream, s.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			stream = tlsConn
		default:
			return
		}
	}
}

// handleBind answers one bind request and reports whether the connection stays
// open.
func (s *fakeDirectoryServer) handleBind(stream net.Conn, id int, operation tlv) bool {
	children, err := parseTLVs(operation.content)
	if err != nil || len(children) < 3 {
		return false
	}
	dn := string(children[1].content)
	password := string(children[2].content)
	s.mu.Lock()
	s.binds = append(s.binds, bindObservation{dn: dn, password: password})
	s.mu.Unlock()
	if s.hangBinds {
		time.Sleep(2 * time.Second)
		return true
	}

	code := uint16(49)
	switch {
	case dn == s.serviceDN && password == s.servicePassword:
		code = s.serviceBindCode
	case dn == s.userDN && password == s.userPassword:
		code = s.userBindCode
	}
	response := berChildren(berTagBindResponse, berInt(berTagEnumerated, int(code)),
		berString(berTagOctetString, ""), berString(berTagOctetString, ""))
	return writeAll(stream, berMessage(id, response)) == nil
}

// handleSearch records one search and answers with the canned entries. A hang
// server reads the request and never answers, which drives the timeout tests.
func (s *fakeDirectoryServer) handleSearch(stream net.Conn, id int, operation tlv) {
	children, err := parseTLVs(operation.content)
	if err != nil || len(children) < 8 {
		return
	}
	observation := searchObservation{base: string(children[0].content)}
	observation.sizeLimit, _ = berInteger(children[3])
	observation.filter = children[6]
	attributes, _ := parseTLVs(children[7].content)
	for _, attribute := range attributes {
		observation.attributes = append(observation.attributes, string(attribute.content))
	}
	s.mu.Lock()
	s.searches = append(s.searches, observation)
	s.mu.Unlock()
	if s.hang {
		time.Sleep(2 * time.Second)
		return
	}
	for _, entry := range s.entries {
		if writeAll(stream, berMessage(id, encodeEntry(entry))) != nil {
			return
		}
	}
	_ = writeAll(stream, berMessage(id, berChildren(berTagSearchDone,
		berInt(berTagEnumerated, int(s.searchDoneCode)),
		berString(berTagOctetString, ""), berString(berTagOctetString, ""))))
}

// encodeEntry builds one SearchResultEntry operation.
func encodeEntry(entry fakeEntry) []byte {
	names := make([]string, 0, len(entry.attributes))
	for name := range entry.attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	partials := make([][]byte, 0, len(names))
	for _, name := range names {
		values := make([][]byte, 0, len(entry.attributes[name]))
		for _, value := range entry.attributes[name] {
			values = append(values, berString(berTagOctetString, value))
		}
		partials = append(partials, berChildren(berTagSequence,
			berString(berTagOctetString, name), berChildren(berTagSet, values...)))
	}
	return berChildren(berTagSearchEntry, berString(berTagOctetString, entry.dn),
		berChildren(berTagSequence, partials...))
}

func writeAll(stream io.Writer, data []byte) error {
	_, err := stream.Write(data)
	return err
}

// equalityFilter decodes the expected shape of a simple equality filter and
// returns its attribute description and assertion value.
func equalityFilter(t *testing.T, filter tlv) (string, string) {
	t.Helper()
	if filter.tag != 0xa3 {
		t.Fatalf("filter tag = %#x, want an equalityMatch (0xa3)", filter.tag)
	}
	children, err := parseTLVs(filter.content)
	if err != nil || len(children) != 2 {
		t.Fatalf("equality filter has %d children, want 2", len(children))
	}
	if children[0].tag != berTagOctetString || children[1].tag != berTagOctetString {
		t.Fatalf("equality filter children have tags %#x and %#x", children[0].tag, children[1].tag)
	}
	return string(children[0].content), string(children[1].content)
}
