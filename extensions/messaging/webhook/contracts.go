package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

const (
	HeaderDeliveryID = "X-Webhook-ID"
	HeaderEvent      = "X-Webhook-Event"
	HeaderTimestamp  = "X-Webhook-Timestamp"
	HeaderSignature  = "X-Webhook-Signature"
)

var (
	ErrClosed           = errors.New("webhook: client is closed")
	ErrNotStarted       = errors.New("webhook: asynchronous workers are not started")
	ErrQueueFull        = errors.New("webhook: delivery queue is full")
	ErrPayloadTooLarge  = errors.New("webhook: payload exceeds configured limit")
	ErrResponseTooLarge = errors.New("webhook: response exceeds configured limit")
	ErrEndpointRejected = errors.New("webhook: endpoint rejected by policy")
	ErrNetwork          = errors.New("webhook: network request failed")
)

// HTTPStatusError reports a non-success response without retaining response
// headers or a potentially sensitive response body in the error string.
type HTTPStatusError struct {
	StatusCode int
	Retryable  bool
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("webhook: endpoint returned HTTP %d", e.StatusCode)
}

// Delivery describes one domain-neutral webhook. Payload, Headers, and Secret
// are defensively copied at every admission boundary.
type Delivery struct {
	ID      string
	URL     string
	Event   string
	Payload []byte
	Headers map[string]string
	Secret  []byte
}

// Clone returns a deep copy suitable for retaining asynchronously.
func (d Delivery) Clone() Delivery {
	d.Payload = append([]byte(nil), d.Payload...)
	d.Secret = append([]byte(nil), d.Secret...)
	if d.Headers != nil {
		source := d.Headers
		d.Headers = make(map[string]string, len(source))
		for key, value := range source {
			d.Headers[key] = value
		}
	}
	return d
}

// Result contains bounded, non-secret delivery telemetry. Err never includes
// the request payload or signing secret.
type Result struct {
	DeliveryID  string
	StatusCode  int
	Attempts    int
	StartedAt   time.Time
	CompletedAt time.Time
	Response    []byte
	Truncated   bool
	Err         error
}

// Clone returns a defensive result copy.
func (r Result) Clone() Result {
	r.Response = append([]byte(nil), r.Response...)
	return r
}

// Observer receives one final result per synchronous or queued delivery.
// Implementations must return promptly; panics are isolated by Client.
type Observer interface{ Observe(context.Context, Result) }

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(context.Context, Result)

func (f ObserverFunc) Observe(ctx context.Context, result Result) { f(ctx, result) }

// Dispatcher is the asynchronous business-facing capability.
type Dispatcher interface {
	Enqueue(context.Context, Delivery) error
}

// Transport is the injectable HTTP round-trip seam. The default transport
// performs actual-dial IP validation. A custom implementation bypasses that
// protection and is therefore appropriate only for controlled infrastructure
// or deterministic tests.
type Transport interface {
	RoundTrip(*http.Request) (*http.Response, error)
}

// Resolver resolves every candidate address before the default transport
// dials any of them. The default transport dials the selected numeric address,
// so the operating system cannot perform a second, unvalidated DNS lookup.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// EndpointPolicy can replace the default public-address policy with
// deployment-specific URL and resolved-IP rules. Base URL syntax and scheme
// rules remain enforced independently by Config.
type EndpointPolicy interface {
	ValidateURL(context.Context, *url.URL) error
	ValidateIP(context.Context, string, netip.Addr) error
}
