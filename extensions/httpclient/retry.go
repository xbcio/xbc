package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/xbcio/xbc/extensions/observability/tracing"
)

// instrumentationName is the tracer scope every span this package creates is
// reported under: the full package path, so a backend can group and configure
// these spans without guessing at a bare package name.
const instrumentationName = "github.com/xbcio/xbc/extensions/httpclient"

// ErrStopped is what every request issued through a client after Stop fails
// with. It is a sentinel rather than a formatted error so a caller can
// classify a shutdown race without matching on text.
var ErrStopped = errors.New("http-client: client is stopped")

type idempotentContextKey struct{}

// Idempotent marks ctx so the retry engine treats the request as safe to send
// again even though its method alone does not say so. A POST whose caller
// knows the peer deduplicates it -- an idempotency key the peer honours, a
// read-shaped query sent as POST -- is the request this exists for; without
// the mark, the method set alone decides and POST is never retried.
func Idempotent(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, idempotentContextKey{}, true)
}

// retryTransport is the whole outbound engine: it is the RoundTripper behind
// the *http.Client this package hands out, so every caller keeps the standard
// net/http surface and the policy lives at one seam instead of in a bespoke
// request API. It owns the total budget, the per-attempt timeout and span,
// the retry decision, and the metrics of both.
type retryTransport struct {
	config   Config
	next     http.RoundTripper
	metrics  *clientMetrics
	tracer   tracing.Handle
	instance string
	// stopped points at the owning Client's flag so a request admitted before
	// Stop is refused at its next attempt boundary and never rebuilds.
	stopped *atomic.Bool
}

// RoundTrip performs the request under the configured policy.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("http-client: nil request")
	}
	if t.stopped.Load() {
		return nil, ErrStopped
	}
	callerCtx := req.Context()
	if callerCtx == nil {
		callerCtx = context.Background()
	}
	// The total budget is derived once per RoundTrip. A caller deadline
	// already inside it is kept as the effective bound: the caller's context
	// is a contract, not a suggestion.
	budgetCtx := callerCtx
	var budgetCancel context.CancelFunc
	if t.config.TotalTimeout > 0 {
		if deadline, ok := callerCtx.Deadline(); !ok || time.Until(deadline) > t.config.TotalTimeout {
			budgetCtx, budgetCancel = context.WithTimeout(callerCtx, t.config.TotalTimeout)
		}
	}
	response, err := t.retry(req, callerCtx, budgetCtx)
	if err != nil {
		if budgetCancel != nil {
			budgetCancel()
		}
		return nil, err
	}
	// The budget lives until the caller closes the final body: the body has
	// not been read yet, and releasing the budget now would abort the read.
	// Closing this outer wrapper first closes the attempt's own wrapper
	// inside it -- ending its span and releasing its timer -- and then
	// releases the budget.
	if response.Body == nil {
		response.Body = http.NoBody
	}
	response.Body = &attemptBody{ReadCloser: response.Body, finish: func() {
		if budgetCancel != nil {
			budgetCancel()
		}
	}}
	return response, nil
}

// CloseIdleConnections forwards to the transport beneath. net/http's
// Client.CloseIdleConnections only reaches a RoundTripper that implements
// this method, so without it the call -- and Stop, which makes it -- would
// silently leave the *http.Transport pool open. The assertion mirrors what
// net/http itself does when a caller closes a custom transport's idle
// connections.
func (t *retryTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// retry is the attempt loop behind RoundTrip: it decides what each attempt's
// outcome means and returns either the response the caller should see or the
// error that ended the sequence.
//
// The order inside one retry decision is deliberate: the retry is classified
// first, the remaining total budget is checked before anything is thrown
// away, and only then is the current response discarded -- so the last
// response a peer sent is always what a caller gets when the budget cannot
// cover another attempt, with its body still readable. A response is drained
// and closed before the wait, not after it, because the wait may last as long
// as MaxBackoff and an undiscarded connection would be held for all of it.
func (t *retryTransport) retry(req *http.Request, callerCtx, budgetCtx context.Context) (*http.Response, error) {
	// pendingBody carries the replayed body into the next attempt. A nil
	// value means the original body was absent or http.NoBody, which the
	// clone already carries correctly.
	var pendingBody io.ReadCloser
	for attempt := 1; ; attempt++ {
		if t.stopped.Load() {
			return nil, ErrStopped
		}
		attemptCtx, cancelAttempt := context.WithTimeout(budgetCtx, t.config.Timeout)
		attemptReq := req.Clone(attemptCtx)
		if pendingBody != nil {
			attemptReq.Body = pendingBody
		}
		attemptCtx, span := t.startAttempt(attemptCtx, attemptReq)
		attemptReq = attemptReq.WithContext(attemptCtx)
		endAttempt := func() {
			cancelAttempt()
			if span != nil {
				span.End()
			}
		}

		started := time.Now()
		response, err := t.next.RoundTrip(attemptReq)
		elapsed := time.Since(started)

		if err != nil {
			t.metrics.observe(req.Method, codeError, elapsed)
			endAttempt()
			// A cancellation that came from outside this attempt -- the
			// caller's context or the total budget -- is not retried: the
			// deadline is already decided. An expired per-attempt timeout is
			// retried like any other connection outcome.
			if budgetCtx.Err() != nil || callerCtx.Err() != nil {
				return nil, err
			}
			if !t.mayRetry(attempt, req) {
				return nil, err
			}
			body, bodyErr := replayBody(req)
			if bodyErr != nil {
				// The attempt's own failure is what the caller classifies, so
				// it stays the wrapped error; the replay failure rides along
				// because it is the reason no retry followed. The response
				// path below makes the mirroring choice: there the peer's
				// response is returned as sent, not an error about the
				// aborted retry.
				return nil, fmt.Errorf("%w; retry skipped: %v", err, bodyErr)
			}
			delay := t.backoff(attempt)
			if !withinBudget(budgetCtx, delay) {
				return nil, err
			}
			t.metrics.observeRetry(reasonConnection)
			if waitErr := waitRetry(budgetCtx, delay); waitErr != nil {
				return nil, waitErr
			}
			pendingBody = body
			continue
		}

		// The attempt's span and timeout live until its response body is
		// closed -- by discardBody for a response that feeds a retry, by the
		// caller for the response that ends the call.
		if response.Body == nil {
			response.Body = http.NoBody
		}
		response.Body = &attemptBody{ReadCloser: response.Body, finish: endAttempt}

		t.metrics.observe(req.Method, strconv.Itoa(response.StatusCode), elapsed)
		reason, retryable := retryableStatus(response.StatusCode)
		if !retryable || !t.mayRetry(attempt, req) {
			return response, nil
		}
		var retryAfter time.Duration
		if response.StatusCode == http.StatusTooManyRequests {
			retryAfter = retryAfterDelay(response.Header.Get("Retry-After"), time.Now(), t.config.Retry.MaxRetryAfter)
		}
		delay := t.backoff(attempt)
		if retryAfter > delay {
			delay = retryAfter
		}
		if !withinBudget(budgetCtx, delay) {
			return response, nil
		}
		// The replayed body is prepared before the current response is
		// discarded: a GetBody that fails still leaves the caller the
		// response the peer just sent, not an error about the next attempt.
		body, bodyErr := replayBody(req)
		if bodyErr != nil {
			return response, nil
		}
		discardBody(response)
		t.metrics.observeRetry(reason)
		if waitErr := waitRetry(budgetCtx, delay); waitErr != nil {
			return nil, waitErr
		}
		pendingBody = body
	}
}

// startAttempt opens the attempt's client span under whoever called, injects
// its context into the attempt's own headers, and returns the span-bearing
// context. Injection happens per attempt, not once per call, so a retry
// carries a new traceparent that points at the retry's own span -- the
// backend then sees three attempts where there were three.
func (t *retryTransport) startAttempt(ctx context.Context, attemptReq *http.Request) (context.Context, trace.Span) {
	if t.tracer == nil {
		return ctx, nil
	}
	tracer := t.tracer.Tracer(instrumentationName)
	if tracer == nil {
		return ctx, nil
	}
	ctx, span := tracer.Start(ctx, "HTTP "+attemptReq.Method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(attemptReq.Method),
			semconv.ServerAddressKey.String(attemptReq.URL.Hostname()),
		),
	)
	if trace.SpanContextFromContext(ctx).IsValid() {
		t.tracer.Inject(ctx, propagation.HeaderCarrier(attemptReq.Header))
	}
	return ctx, span
}

// mayRetry is the gate every retry reason passes before a second attempt is
// considered: the attempt budget, the method's idempotency, and whether the
// body can be produced again. The last check is what makes replayBody's
// direct GetBody call safe, and it is the only place the question is asked:
// a body that was consumed by the first attempt and cannot be replayed would
// send the retry empty, so it is not retried at all -- the peer answered,
// and answering again with a truncated request is worse than reporting the
// first outcome.
func (t *retryTransport) mayRetry(attempt int, req *http.Request) bool {
	return attempt < t.config.Retry.MaxAttempts && idempotentRequest(req) && replayableRequest(req)
}

// retryableStatus classifies a response. The retryable set is deliberately
// three gateway statuses plus 429: they mean "this peer is momentarily not
// serving", where another attempt can land on a healthy peer. 500 and its
// siblings mean the peer processed the request and failed, where an automatic
// retry can double whatever the request was worth and where the peer's own
// answer is the one the caller should see.
func retryableStatus(code int) (string, bool) {
	switch code {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return reasonStatus, true
	case http.StatusTooManyRequests:
		return reasonRetryAfter, true
	}
	return "", false
}

// idempotentRequest reports whether the request may be sent again. The
// method set is the RFC 9110 safe-and-idempotent one; POST and PATCH are
// never retried without the explicit Idempotent mark, because the engine
// cannot know what a second POST would do to the peer's state.
func idempotentRequest(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	marked, _ := req.Context().Value(idempotentContextKey{}).(bool)
	return marked
}

// replayableRequest reports whether a second attempt can carry the same body.
// A request built from a bytes.Reader, strings.Reader, or bytes.Buffer has
// GetBody set by net/http; a one-shot reader does not.
func replayableRequest(req *http.Request) bool {
	if req.Body == nil || req.Body == http.NoBody {
		return true
	}
	return req.GetBody != nil
}

// replayBody produces the body of the next attempt. A nil result means the
// request has no body and the clone's own value -- nil or http.NoBody -- is
// already correct.
//
// It is only ever reached through mayRetry, whose replayableRequest check is
// the single gate that guarantees GetBody is set: a request that cannot
// produce its body again is refused there, before any retry decision is
// acted on, and never arrives here to fail mid-sequence with the previous
// response already discarded.
func replayBody(req *http.Request) (io.ReadCloser, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("http-client: replay request body: %w", err)
	}
	if body == nil {
		return nil, errors.New("http-client: request body cannot be replayed")
	}
	return body, nil
}

// backoff returns the wait before the next attempt: the configured initial
// backoff doubled once per completed attempt, capped at MaxBackoff, then
// reduced to a jittered value in the upper half of that span. The jitter
// keeps independent clients that failed against the same peer from
// synchronizing their retries into a second wave.
func (t *retryTransport) backoff(attempt int) time.Duration {
	base := t.config.Retry.InitialBackoff
	for range attempt - 1 {
		if base >= t.config.Retry.MaxBackoff {
			break
		}
		base *= 2
	}
	if base > t.config.Retry.MaxBackoff {
		base = t.config.Retry.MaxBackoff
	}
	half := base / 2
	return half + time.Duration(mathrand.Int64N(int64(half)+1))
}

// withinBudget reports whether the remaining total budget can still absorb a
// wait of delay and an attempt after it. Without a deadline the budget is
// unbounded and every wait fits.
func withinBudget(ctx context.Context, delay time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > delay
}

// waitRetry sleeps for delay unless the budget or the caller's context ends
// first. It reports why the wait was cut short so the caller sees the
// deadline that actually bound the call.
func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("http-client: waiting to retry: %w", ctx.Err())
	}
}

// discardBody drains a bounded prefix of the response -- enough for the
// connection to be reusable -- and closes it, which ends the attempt's span
// and releases its timeout timer. The bound matters: a peer streaming an
// unbounded body must not be able to hold a retry hostage.
func discardBody(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	_ = response.Body.Close()
}

// attemptBody keeps one attempt alive for as long as its response is
// readable: closing it ends the span and cancels the attempt's context
// exactly once, whether the caller read the body, discarded it, or abandoned
// it mid-stream (net/http closes a replaced or abandoned body itself).
type attemptBody struct {
	io.ReadCloser
	once   sync.Once
	finish func()
}

func (b *attemptBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.finish)
	return err
}

// retryAfterDelay interprets a Retry-After header, capped at the configured
// maximum. It deliberately duplicates the webhook scheduler's equivalent:
// one capability plugin module may not import another, and the semantics are
// small enough that one copy per module is cheaper than a shared package
// neither module's dependency closure wants.
func retryAfterDelay(value string, now time.Time, maximum time.Duration) time.Duration {
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
	if delay <= 0 {
		return 0
	}
	if delay > maximum {
		return maximum
	}
	return delay
}
