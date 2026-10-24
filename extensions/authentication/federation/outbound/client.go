package outbound

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseHeaderBytes bounds the response headers a federation endpoint may
// return, independently of the response-body limit.
const maxResponseHeaderBytes = 1 << 20

// redacted is the only rendering a Secret ever produces.
const redacted = "[REDACTED]"

var (
	// ErrTooManyRedirects reports a redirect chain longer than the policy
	// allows, including any redirect under NoRedirects.
	ErrTooManyRedirects = errors.New("federation/outbound: too many redirects")
	// ErrRedirectRejected reports a redirect that is not a GET or HEAD
	// redirect. A redirected credential-bearing request is never replayed at a
	// new location.
	ErrRedirectRejected = errors.New("federation/outbound: redirect rejected by policy")
	// ErrInsecureRedirect reports an HTTPS-to-HTTP redirect.
	ErrInsecureRedirect = errors.New("federation/outbound: redirect downgrades HTTPS")
	// ErrBodyTooLarge reports a response body above the applied limit.
	ErrBodyTooLarge = errors.New("federation/outbound: response body exceeds the limit")
	// ErrBodyRead reports a failure while reading a response body.
	ErrBodyRead = errors.New("federation/outbound: response body read failed")
	// ErrNilResponse reports ReadBody called without a response or body.
	ErrNilResponse = errors.New("federation/outbound: nil response")
)

// NewClient returns an HTTP client governed by policy: HTTPS enforcement,
// address validation at resolution and connect time, per-hop redirect
// validation, and the configured timeouts. The zero Policy selects every
// default. NewClient performs no network I/O.
func NewClient(policy Policy) (*http.Client, error) {
	return newClient(policy, nil, nil)
}

// newClient is the injectable constructor used by tests. A nil resolver uses
// the standard library resolver; a nil dial function uses a net.Dialer whose
// Control hook re-validates the address at connect time.
func newClient(policy Policy, resolver hostResolver, dial dialContextFunc) (*http.Client, error) {
	normalized, err := policy.normalized()
	if err != nil {
		return nil, err
	}
	if resolver == nil {
		resolver = netResolver{resolver: net.DefaultResolver}
	}
	dialer := &net.Dialer{
		Timeout:   normalized.DialTimeout,
		KeepAlive: 30 * time.Second,
		Control:   normalized.control,
	}
	if dial == nil {
		dial = dialer.DialContext
	}

	transport := &http.Transport{
		// Proxies are deliberately unsupported: a proxy resolves and connects
		// on our behalf, which bypasses every address validation above.
		Proxy:                  nil,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    normalized.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  normalized.ResponseHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		IdleConnTimeout:        90 * time.Second,
		MaxIdleConns:           100,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		DialContext:            newValidatingDialContext(normalized, resolver, dial),
	}
	return &http.Client{
		Transport:     policyTransport{policy: normalized, base: transport},
		Timeout:       normalized.TotalTimeout,
		CheckRedirect: normalized.checkRedirect,
	}, nil
}

// policyTransport validates every outgoing request URL, including the initial
// request and every redirect hop, before the request reaches the network. The
// dial path validates addresses; this wrapper is what enforces the scheme.
type policyTransport struct {
	policy Policy
	base   http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t policyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, ErrURLRejected
	}
	if err := t.policy.validateURL(request.URL); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(request)
}

// CloseIdleConnections forwards to the wrapped transport so callers can
// release idle connections when their component shuts down.
func (t policyTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// checkRedirect enforces the redirect policy for one hop: a GET or HEAD
// request only, the configured hop count, full URL re-validation, no HTTPS
// downgrade, and no credential-bearing headers across origins.
func (p Policy) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return ErrRedirectRejected
	}
	if len(via) > p.MaxRedirects {
		return ErrTooManyRedirects
	}
	original := via[0]
	if original.Method != http.MethodGet && original.Method != http.MethodHead {
		return ErrRedirectRejected
	}
	if err := p.validateURL(request.URL); err != nil {
		return err
	}
	previous := via[len(via)-1]
	if strings.EqualFold(previous.URL.Scheme, "https") && strings.EqualFold(request.URL.Scheme, "http") {
		return ErrInsecureRedirect
	}
	// Always compare with the original request. net/http may rebuild every
	// redirected request from the original headers, so comparing only with the
	// immediately previous hop could reintroduce credentials on A -> B -> A.
	if !sameOrigin(original.URL, request.URL) {
		for _, header := range []string{
			"Authorization", "Proxy-Authorization", "Cookie", "Cookie2", "Referer", "Origin",
		} {
			request.Header.Del(header)
		}
	}
	return nil
}

// sameOrigin reports whether two URLs share a scheme and host.
func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

// ReadBody reads a response body under limit and rejects a body that grows
// past it. A non-positive limit selects DefaultMaxBodyBytes. ReadBody does not
// close the body; the caller owns the response.
func ReadBody(response *http.Response, limit int64) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, ErrNilResponse
	}
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyRead, err)
	}
	var extra [1]byte
	switch read, readErr := response.Body.Read(extra[:]); {
	case read > 0:
		return nil, ErrBodyTooLarge
	case readErr != nil && !errors.Is(readErr, io.EOF):
		return nil, fmt.Errorf("%w: %v", ErrBodyRead, readErr)
	default:
		return body, nil
	}
}

// Secret is sensitive material such as a client secret or a bind password.
// String and Format both render [REDACTED]. Format is what makes the guarantee
// hold for the fmt verbs that dispatch methods: %#v would otherwise print the
// Go-syntax value and an invalid verb such as %d would otherwise print fmt's
// %!verb(...) fallback, both carrying the raw string.
//
// One verb escapes Format: fmt handles %p before any method dispatch, and with
// a string-kind operand its fallback renders the value. Never apply %p to a
// Secret; applying it to a pointer to one prints the address and is safe.
// Converting a Secret to string is the deliberate, single point of
// declassification.
//
// Secret protects the value itself when it is formatted, logged, or serialized
// through an interface it implements: String and Format cover fmt, and
// MarshalText covers encoding/json and other TextMarshaler-aware encoders --
// including when the Secret is a field of a structure encoded as a whole. A
// serializer that consults neither interface still sees the underlying string,
// so a carrying type with its own wire format must redact that format instead
// of relying on this type alone.
type Secret string

// String implements fmt.Stringer and renders [REDACTED].
func (s Secret) String() string { return redacted }

// Format implements fmt.Formatter and renders [REDACTED] for every verb.
func (s Secret) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, redacted)
}

// MarshalText implements encoding.TextMarshaler and renders [REDACTED].
//
// It is the serialization boundary for interfaces that bypass fmt: encoding/json
// and other TextMarshaler-aware encoders consult TextMarshaler instead of
// String, both for a bare Secret and for one held by a structure encoded as a
// whole. Without it a JSON diagnostic would carry the raw value.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}
