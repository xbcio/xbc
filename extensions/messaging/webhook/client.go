package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xbcio/xbc/plugin"
)

type queuedDelivery struct{ delivery Delivery }

// Client supports direct Deliver and bounded asynchronous Enqueue.
type Client struct {
	cfg       Config
	transport Transport
	policy    EndpointPolicy
	observer  Observer
	queue     chan queuedDelivery
	closing   chan struct{}

	admissionMu    sync.Mutex
	accepting      bool
	senders        sync.WaitGroup
	active         sync.WaitGroup
	started        atomic.Bool
	startAttempted bool

	runCtx  context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup

	stopOnce sync.Once
	stopErr  error
}

func newClient(cfg Config, transport Transport, resolver Resolver, policy EndpointPolicy, observer Observer) *Client {
	if policy == nil {
		policy = DefaultEndpointPolicy{}
	}
	if transport == nil {
		transport = newSafeTransport(cfg, resolver, policy)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	return &Client{
		cfg:       cfg,
		transport: transport,
		policy:    policy,
		observer:  observer,
		queue:     make(chan queuedDelivery, cfg.QueueSize),
		closing:   make(chan struct{}),
		accepting: true,
		runCtx:    runCtx,
		cancel:    cancel,
	}
}

// Enqueue admits one defensive snapshot into the bounded worker queue. Block
// mode waits for capacity, caller cancellation, or shutdown; reject mode
// returns ErrQueueFull immediately when no slot is available.
func (c *Client) Enqueue(ctx context.Context, delivery Delivery) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.started.Load() {
		select {
		case <-c.closing:
			return ErrClosed
		default:
			return ErrNotStarted
		}
	}
	prepared, _, err := c.prepare(ctx, delivery)
	if err != nil {
		return err
	}
	queued := false
	defer func() {
		if !queued {
			wipe(prepared.Secret)
		}
	}()
	if !c.beginAdmission() {
		return ErrClosed
	}
	defer c.senders.Done()
	item := queuedDelivery{delivery: prepared}
	if c.cfg.Backpressure == BackpressureReject {
		select {
		case <-c.closing:
			return ErrClosed
		default:
		}
		select {
		case c.queue <- item:
			queued = true
			return nil
		case <-c.closing:
			return ErrClosed
		default:
			return ErrQueueFull
		}
	}
	select {
	case c.queue <- item:
		queued = true
		return nil
	case <-c.closing:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Deliver performs one synchronous delivery with the configured retry policy.
func (c *Client) Deliver(ctx context.Context, delivery Delivery) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared, _, err := c.prepare(ctx, delivery)
	if err != nil {
		return Result{DeliveryID: delivery.ID, Err: err}, err
	}
	defer wipe(prepared.Secret)
	if !c.beginAdmission() {
		return Result{DeliveryID: prepared.ID, Err: ErrClosed}, ErrClosed
	}
	// Add before releasing the sender. finishClose waits for senders before
	// active.Wait, so no Add can race the Wait.
	c.active.Add(1)
	c.senders.Done()
	defer c.active.Done()
	return c.execute(ctx, prepared)
}

func (c *Client) beginAdmission() bool {
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	if !c.accepting {
		return false
	}
	c.senders.Add(1)
	return true
}

func (c *Client) prepare(ctx context.Context, delivery Delivery) (Delivery, *url.URL, error) {
	if err := ctx.Err(); err != nil {
		return Delivery{}, nil, err
	}
	delivery = delivery.Clone()
	prepared := false
	defer func() {
		if !prepared {
			wipe(delivery.Secret)
		}
	}()
	delivery.ID = strings.TrimSpace(delivery.ID)
	delivery.Event = strings.TrimSpace(delivery.Event)
	if delivery.ID == "" {
		id, err := randomID()
		if err != nil {
			return Delivery{}, nil, errors.New("webhook: generate delivery ID")
		}
		delivery.ID = id
	}
	if !validHeaderValue(delivery.ID) {
		return Delivery{}, nil, errors.New("webhook: delivery ID has an invalid header value")
	}
	if delivery.Event == "" {
		return Delivery{}, nil, errors.New("webhook: event is required")
	}
	if !validHeaderValue(delivery.Event) {
		return Delivery{}, nil, errors.New("webhook: event has an invalid header value")
	}
	if len(delivery.Secret) == 0 {
		return Delivery{}, nil, errors.New("webhook: signing secret is required")
	}
	if len(delivery.Payload) > c.cfg.MaxPayloadBytes {
		return Delivery{}, nil, ErrPayloadTooLarge
	}
	if err := validateHeaders(delivery.Headers); err != nil {
		return Delivery{}, nil, err
	}
	target, err := validateURL(ctx, delivery.URL, c.cfg.AllowHTTP, c.policy)
	if err != nil {
		return Delivery{}, nil, err
	}
	delivery.URL = target.String()
	prepared = true
	return delivery, target, nil
}

func (c *Client) execute(ctx context.Context, delivery Delivery) (result Result, err error) {
	started := time.Now().UTC()
	defer func() {
		if recover() != nil {
			// A custom Transport or response Body is untrusted application code.
			// Isolate its panic so one queued delivery cannot kill a worker and
			// wedge queue drainage.
			result = Result{
				DeliveryID:  delivery.ID,
				Attempts:    1,
				StartedAt:   started,
				CompletedAt: time.Now().UTC(),
				Err:         ErrNetwork,
			}
			err = ErrNetwork
		}
		// Observation belongs to this single finalization point so even a panic
		// from response cleanup produces exactly one final result.
		c.observe(result)
		result = result.Clone()
	}()
	return c.deliver(ctx, delivery)
}

func (c *Client) deliver(ctx context.Context, delivery Delivery) (Result, error) {
	result := Result{DeliveryID: delivery.ID, StartedAt: time.Now().UTC()}
	var finalErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		result.Attempts = attempt
		attemptResult, retryAfterDelay, retryable, err := c.attempt(ctx, delivery)
		result.StatusCode = attemptResult.StatusCode
		result.Response = attemptResult.Response
		result.Truncated = attemptResult.Truncated
		if err == nil {
			finalErr = nil
			break
		}
		finalErr = err
		if !retryable || attempt == c.cfg.MaxAttempts {
			break
		}
		delay := retryDelay(attempt, c.cfg.InitialBackoff, c.cfg.MaxBackoff, c.cfg.Jitter)
		if retryAfterDelay > delay {
			delay = retryAfterDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			finalErr = ctx.Err()
			attempt = c.cfg.MaxAttempts
		case <-timer.C:
		}
	}
	result.CompletedAt = time.Now().UTC()
	result.Err = finalErr
	return result, finalErr
}

type attemptResult struct {
	StatusCode int
	Response   []byte
	Truncated  bool
}

func (c *Client) attempt(parent context.Context, delivery Delivery) (attemptResult, time.Duration, bool, error) {
	ctx, cancel := context.WithTimeout(parent, c.cfg.RequestTimeout)
	stopShutdownCancel := context.AfterFunc(c.runCtx, cancel)
	defer func() {
		stopShutdownCancel()
		cancel()
	}()

	target, err := validateURL(ctx, delivery.URL, c.cfg.AllowHTTP, c.policy)
	if err != nil {
		return attemptResult{}, 0, false, err
	}
	timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(delivery.Payload))
	if err != nil {
		return attemptResult{}, 0, false, &endpointError{reason: "invalid request URL"}
	}
	for key, value := range delivery.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Set(HeaderDeliveryID, delivery.ID)
	req.Header.Set(HeaderEvent, delivery.Event)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderSignature, signature(delivery.Secret, timestamp, delivery.Payload))
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	customHeaders := make([]string, 0, len(delivery.Headers))
	for key := range delivery.Headers {
		customHeaders = append(customHeaders, http.CanonicalHeaderKey(key))
	}

	client := &http.Client{Transport: c.transport, Timeout: c.cfg.RequestTimeout}
	client.CheckRedirect = c.redirectPolicy(customHeaders)
	response, err := client.Do(req)
	if err != nil {
		var endpoint *endpointError
		if errors.As(err, &endpoint) || errors.Is(err, ErrEndpointRejected) {
			return attemptResult{}, 0, false, ErrEndpointRejected
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if parent.Err() != nil {
				return attemptResult{}, 0, false, parent.Err()
			}
			if c.runCtx.Err() != nil {
				return attemptResult{}, 0, false, context.Canceled
			}
			return attemptResult{}, 0, true, context.DeadlineExceeded
		}
		return attemptResult{}, 0, true, ErrNetwork
	}
	defer response.Body.Close()
	body, truncated, readErr := readBounded(response.Body, c.cfg.MaxResponseBytes)
	out := attemptResult{StatusCode: response.StatusCode, Response: body, Truncated: truncated}
	if readErr != nil {
		return out, 0, true, ErrNetwork
	}
	if truncated {
		return out, 0, false, ErrResponseTooLarge
	}
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		return out, 0, false, nil
	}
	retryable := retryableStatus(response.StatusCode)
	statusErr := &HTTPStatusError{StatusCode: response.StatusCode, Retryable: retryable}
	return out, retryAfter(response.Header.Get("Retry-After"), time.Now(), c.cfg.MaxRetryAfter), retryable, statusErr
}

func readBounded(reader io.Reader, maximum int) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(reader, int64(maximum)))
	if err != nil {
		return body, false, err
	}
	var extra [1]byte
	n, err := io.ReadFull(reader, extra[:])
	if n > 0 {
		return body, true, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return body, false, nil
	}
	return body, false, err
}

func (c *Client) redirectPolicy(customHeaders []string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return &endpointError{reason: "invalid redirect chain"}
		}
		if len(via) > c.cfg.MaxRedirects {
			return &endpointError{reason: "too many redirects"}
		}
		if _, err := validateURL(req.Context(), req.URL.String(), c.cfg.AllowHTTP, c.policy); err != nil {
			return err
		}
		previous := via[len(via)-1]
		if strings.EqualFold(previous.URL.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http") {
			return &endpointError{reason: "HTTPS downgrade"}
		}
		if req.Method != http.MethodPost {
			return &endpointError{reason: "method-changing redirect"}
		}
		// Always compare with the original request. net/http may rebuild every
		// redirected request from the original headers, so comparing only with
		// the immediately previous hop could reintroduce secrets on A -> B -> B.
		if !sameOrigin(via[0].URL, req.URL) {
			for _, key := range customHeaders {
				req.Header.Del(key)
			}
			for _, key := range []string{
				"Authorization", "Proxy-Authorization", "Cookie", "Cookie2", "Referer", "Origin",
				HeaderDeliveryID, HeaderEvent, HeaderTimestamp, HeaderSignature,
			} {
				req.Header.Del(key)
			}
		}
		return nil
	}
}

func (c *Client) start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("webhook: Start requires a plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Keep worker registration serialized with Stop. Add must never race a
	// Wait, and a partially admitted Start remains owned for Stop to unwind.
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	if !c.accepting {
		return ErrClosed
	}
	if c.started.Load() || c.startAttempted {
		return errors.New("webhook: Start called more than once")
	}
	c.startAttempted = true

	admitted := 0
	for range c.cfg.Workers {
		c.workers.Add(1)
		if !ctx.GoCritical(func(taskCtx context.Context) {
			defer c.workers.Done()
			c.worker(taskCtx)
		}) {
			c.workers.Done()
			c.accepting = false
			close(c.closing)
			return fmt.Errorf("webhook: Start admitted %d of %d workers: runtime rejected managed task", admitted, c.cfg.Workers)
		}
		admitted++
	}
	c.started.Store(true)
	return nil
}

func (c *Client) worker(taskCtx context.Context) {
	ctx, cancel := context.WithCancel(taskCtx)
	stop := context.AfterFunc(c.runCtx, cancel)
	defer stop()
	defer cancel()
	for item := range c.queue {
		_, _ = c.execute(ctx, item.delivery)
		wipe(item.delivery.Secret)
	}
}

// stop closes admission synchronously, drains accepted work while managed
// workers are still live, and closes transport resources exactly once. It is
// safe before Start, after partial worker admission, and on repeated calls.
func (c *Client) stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.stopOnce.Do(func() {
		// Cancellation of the lifecycle budget aborts in-flight queued requests;
		// the runtime still bounds this synchronous Stop if a dependency ignores
		// cancellation.
		stopCancellation := context.AfterFunc(ctx, c.cancel)
		defer stopCancellation()

		c.admissionMu.Lock()
		if c.accepting {
			c.accepting = false
			close(c.closing)
		}
		c.admissionMu.Unlock()

		// beginAdmission adds senders under admissionMu. Waiting for them before
		// closing the queue prevents a send/close race. Deliver moves into active
		// before releasing its sender slot, so active.Wait cannot race an Add.
		c.senders.Wait()
		close(c.queue)
		c.workers.Wait()
		c.active.Wait()
		c.cancel()
		c.stopErr = closeIdleConnections(c.transport)
	})
	return c.stopErr
}

func closeIdleConnections(transport Transport) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("webhook: transport cleanup panicked")
		}
	}()
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	return nil
}

func (c *Client) observe(result Result) {
	if c.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	c.observer.Observe(context.Background(), result.Clone())
}

func signature(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == 425 || status == http.StatusTooManyRequests || status >= 500 && status <= 599
}

func retryAfter(value string, now time.Time, maximum time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" || maximum <= 0 {
		return 0
	}
	var delay time.Duration
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > 0 && seconds <= int64(math.MaxInt64/int64(time.Second)) {
			delay = time.Duration(seconds) * time.Second
		}
	} else if at, err := http.ParseTime(value); err == nil && at.After(now) {
		delay = at.Sub(now)
	}
	if delay < 0 {
		return 0
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func retryDelay(attempt int, initial, maximum time.Duration, jitter float64) time.Duration {
	base := float64(initial) * math.Pow(2, float64(max(attempt-1, 0)))
	if base > float64(maximum) {
		base = float64(maximum)
	}
	if jitter > 0 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err == nil {
			n := uint64(0)
			for _, value := range random {
				n = n<<8 | uint64(value)
			}
			unit := float64(n) / float64(^uint64(0))
			base *= 1 - jitter + 2*jitter*unit
		}
	}
	if base > float64(maximum) {
		base = float64(maximum)
	}
	return time.Duration(base)
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func validateHeaders(headers map[string]string) error {
	reserved := make(map[string]struct{})
	for _, key := range []string{
		"Host", "Content-Length", "Connection", "Proxy-Connection", "Keep-Alive",
		"Transfer-Encoding", "TE", "Trailer", "Upgrade",
		HeaderDeliveryID, HeaderEvent, HeaderTimestamp, HeaderSignature,
	} {
		reserved[http.CanonicalHeaderKey(key)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(headers))
	for key, value := range headers {
		canonical := http.CanonicalHeaderKey(key)
		if canonical == "" || !validToken(key) {
			return errors.New("webhook: custom header name is invalid")
		}
		if _, duplicate := seen[canonical]; duplicate {
			return fmt.Errorf("webhook: custom header %q is duplicated", canonical)
		}
		seen[canonical] = struct{}{}
		if _, denied := reserved[canonical]; denied {
			return fmt.Errorf("webhook: custom header %q is reserved", canonical)
		}
		if !validHeaderValue(value) {
			return fmt.Errorf("webhook: custom header %q has an invalid value", canonical)
		}
	}
	return nil
}

func validToken(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' {
			continue
		}
		switch ch {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := range len(value) {
		ch := value[i]
		if ch == '\t' {
			continue
		}
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func wipe(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
