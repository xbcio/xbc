package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeliverSignsExactRawBodyAndCopiesInput(t *testing.T) {
	payload := []byte("{ \"message\": \"unchanged whitespace\" }\n")
	secret := []byte("original-secret")
	headers := map[string]string{"X-Tenant": "original"}
	var capturedBody []byte
	var capturedHeader http.Header
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var err error
		capturedBody, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		capturedHeader = request.Header.Clone()
		return testResponse(request, http.StatusNoContent, nil), nil
	})
	client := newClient(testConfig(), transport, nil, nil, nil)
	t.Cleanup(func() { _ = client.stop(context.Background()) })

	delivery := testDelivery()
	delivery.Payload = payload
	delivery.Secret = secret
	delivery.Headers = headers
	result, err := client.Deliver(context.Background(), delivery)
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if result.StatusCode != http.StatusNoContent || result.Attempts != 1 {
		t.Fatalf("Deliver() result = %#v", result)
	}
	if !bytes.Equal(capturedBody, payload) {
		t.Fatalf("request body = %q, want exact %q", capturedBody, payload)
	}
	if got := capturedHeader.Get(HeaderDeliveryID); got != delivery.ID {
		t.Fatalf("%s = %q", HeaderDeliveryID, got)
	}
	if got := capturedHeader.Get(HeaderEvent); got != delivery.Event {
		t.Fatalf("%s = %q", HeaderEvent, got)
	}
	if got := capturedHeader.Get("X-Tenant"); got != "original" {
		t.Fatalf("X-Tenant = %q", got)
	}
	timestamp := capturedHeader.Get(HeaderTimestamp)
	if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		t.Fatalf("timestamp %q is invalid: %v", timestamp, err)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(payload)
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := capturedHeader.Get(HeaderSignature); !hmac.Equal([]byte(got), []byte(wantSignature)) {
		t.Fatalf("signature = %q, want %q", got, wantSignature)
	}
	if got := capturedHeader.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if !bytes.Equal(secret, []byte("original-secret")) || !bytes.Equal(payload, []byte("{ \"message\": \"unchanged whitespace\" }\n")) || headers["X-Tenant"] != "original" {
		t.Fatal("Deliver modified caller-owned input")
	}

	capturedBody[0] ^= 0xff
	if result.Response != nil && len(result.Response) > 0 {
		result.Response[0] ^= 0xff
	}
}

func TestQueuedDeliveryUsesDefensiveSnapshot(t *testing.T) {
	release := make(chan struct{})
	requestStarted := make(chan struct{})
	captured := make(chan *http.Request, 1)
	var body []byte
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(requestStarted)
		<-release
		var err error
		body, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		captured <- request.Clone(context.Background())
		return testResponse(request, http.StatusOK, nil), nil
	})
	observed := make(chan Result, 1)
	client := newClient(testConfig(), transport, nil, nil, ObserverFunc(func(_ context.Context, result Result) {
		observed <- result
	}))
	startManagedTestClient(t, client)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		_ = client.stop(context.Background())
	})

	delivery := testDelivery()
	originalPayload := append([]byte(nil), delivery.Payload...)
	originalSecret := append([]byte(nil), delivery.Secret...)
	if err := client.Enqueue(context.Background(), delivery); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("queued delivery did not start")
	}
	for i := range delivery.Payload {
		delivery.Payload[i] = 'x'
	}
	for i := range delivery.Secret {
		delivery.Secret[i] = 'y'
	}
	delivery.Headers["X-Tenant"] = "mutated"
	unblock()

	var request *http.Request
	select {
	case request = <-captured:
	case <-time.After(time.Second):
		t.Fatal("request was not captured")
	}
	if !bytes.Equal(body, originalPayload) {
		t.Fatalf("queued body = %q, want %q", body, originalPayload)
	}
	if got := request.Header.Get("X-Tenant"); got != "tenant-1" {
		t.Fatalf("queued header = %q", got)
	}
	timestamp := request.Header.Get(HeaderTimestamp)
	if got, want := request.Header.Get(HeaderSignature), signature(originalSecret, timestamp, originalPayload); got != want {
		t.Fatalf("queued signature = %q, want %q", got, want)
	}
	select {
	case result := <-observed:
		if result.Err != nil {
			t.Fatalf("observed result error = %v", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("final result was not observed")
	}
}

func TestRetryClassificationAndAttempts(t *testing.T) {
	tests := []struct {
		name         string
		firstStatus  int
		firstErr     error
		wantAttempts int
		wantError    bool
	}{
		{name: "server error", firstStatus: http.StatusServiceUnavailable, wantAttempts: 2},
		{name: "request timeout", firstStatus: http.StatusRequestTimeout, wantAttempts: 2},
		{name: "too early", firstStatus: 425, wantAttempts: 2},
		{name: "rate limited", firstStatus: http.StatusTooManyRequests, wantAttempts: 2},
		{name: "network", firstErr: io.ErrUnexpectedEOF, wantAttempts: 2},
		{name: "ordinary client error", firstStatus: http.StatusBadRequest, wantAttempts: 1, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MaxAttempts = 3
			var calls atomic.Int32
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				call := calls.Add(1)
				if call == 1 {
					if test.firstErr != nil {
						return nil, test.firstErr
					}
					return testResponse(request, test.firstStatus, nil), nil
				}
				return testResponse(request, http.StatusOK, nil), nil
			})
			client := newClient(cfg, transport, nil, nil, nil)
			t.Cleanup(func() { _ = client.stop(context.Background()) })
			result, err := client.Deliver(context.Background(), testDelivery())
			if test.wantError != (err != nil) {
				t.Fatalf("Deliver() error = %v, wantError %v", err, test.wantError)
			}
			if result.Attempts != test.wantAttempts || int(calls.Load()) != test.wantAttempts {
				t.Fatalf("attempts result=%d calls=%d, want %d", result.Attempts, calls.Load(), test.wantAttempts)
			}
			if test.wantError {
				var statusError *HTTPStatusError
				if !errors.As(err, &statusError) || statusError.StatusCode != test.firstStatus || statusError.Retryable {
					t.Fatalf("error = %#v", err)
				}
			}
		})
	}
}

func TestRetryAfterIsCappedAndCallerCancellationStopsBackoff(t *testing.T) {
	t.Run("retry after cap", func(t *testing.T) {
		cfg := testConfig()
		cfg.MaxAttempts = 2
		cfg.InitialBackoff = time.Millisecond
		cfg.MaxBackoff = time.Millisecond
		cfg.MaxRetryAfter = 25 * time.Millisecond
		var calls atomic.Int32
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				response := testResponse(request, http.StatusTooManyRequests, nil)
				response.Header.Set("Retry-After", "3600")
				return response, nil
			}
			return testResponse(request, http.StatusOK, nil), nil
		})
		client := newClient(cfg, transport, nil, nil, nil)
		t.Cleanup(func() { _ = client.stop(context.Background()) })
		started := time.Now()
		result, err := client.Deliver(context.Background(), testDelivery())
		elapsed := time.Since(started)
		if err != nil || result.Attempts != 2 {
			t.Fatalf("Deliver() = %#v, %v", result, err)
		}
		if elapsed < 20*time.Millisecond {
			t.Fatalf("Retry-After cap was not honored; elapsed %v", elapsed)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("Retry-After was not capped; elapsed %v", elapsed)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		cfg := testConfig()
		cfg.MaxAttempts = 3
		cfg.InitialBackoff = 10 * time.Second
		cfg.MaxBackoff = 10 * time.Second
		first := make(chan struct{})
		var calls atomic.Int32
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-first:
			default:
				close(first)
			}
			return testResponse(request, http.StatusServiceUnavailable, nil), nil
		})
		client := newClient(cfg, transport, nil, nil, nil)
		t.Cleanup(func() { _ = client.stop(context.Background()) })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := client.Deliver(ctx, testDelivery())
			done <- err
		}()
		select {
		case <-first:
		case <-time.After(time.Second):
			t.Fatal("first attempt did not run")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Deliver() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Deliver did not stop its backoff after caller cancellation")
		}
		if calls.Load() != 1 {
			t.Fatalf("transport calls = %d, want 1", calls.Load())
		}
	})
}

type countingReadCloser struct {
	reader io.Reader
	read   int
}

func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.read += n
	return n, err
}
func (*countingReadCloser) Close() error { return nil }

func TestResponseBodyHardLimit(t *testing.T) {
	for _, test := range []struct {
		name          string
		body          string
		wantErr       error
		wantTruncated bool
		wantRead      int
	}{
		{name: "exact boundary", body: "12345678", wantRead: 8},
		{name: "one beyond", body: "123456789", wantErr: ErrResponseTooLarge, wantTruncated: true, wantRead: 9},
		{name: "much larger", body: "123456789abcdefghijklmnopqrstuvwxyz", wantErr: ErrResponseTooLarge, wantTruncated: true, wantRead: 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MaxResponseBytes = 8
			cfg.MaxAttempts = 3
			body := &countingReadCloser{reader: bytes.NewBufferString(test.body)}
			var calls atomic.Int32
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				return testResponse(request, http.StatusOK, body), nil
			})
			client := newClient(cfg, transport, nil, nil, nil)
			t.Cleanup(func() { _ = client.stop(context.Background()) })
			result, err := client.Deliver(context.Background(), testDelivery())
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Deliver() error = %v, want %v", err, test.wantErr)
			}
			if result.Truncated != test.wantTruncated || len(result.Response) != 8 {
				t.Fatalf("result = %#v", result)
			}
			if body.read != test.wantRead {
				t.Fatalf("body bytes read = %d, want %d", body.read, test.wantRead)
			}
			if calls.Load() != 1 {
				t.Fatalf("oversized response retried; calls = %d", calls.Load())
			}
		})
	}
}

func TestDeliveryValidationRejectsInjectionAndUnsafeURL(t *testing.T) {
	client := newClient(testConfig(), roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatal("transport called for invalid delivery")
		return nil, nil
	}), nil, nil, nil)
	t.Cleanup(func() { _ = client.stop(context.Background()) })

	tests := []struct {
		name   string
		mutate func(*Delivery)
		want   error
	}{
		{"missing event", func(d *Delivery) { d.Event = "" }, nil},
		{"event injection", func(d *Delivery) { d.Event = "ok\r\nInjected: true" }, nil},
		{"id injection", func(d *Delivery) { d.ID = "id\nInjected" }, nil},
		{"header injection", func(d *Delivery) { d.Headers["X-Test"] = "ok\r\nInjected: true" }, nil},
		{"reserved signature", func(d *Delivery) { d.Headers["x-webhook-signature"] = "fake" }, nil},
		{"reserved host", func(d *Delivery) { d.Headers["Host"] = "internal" }, nil},
		{"userinfo", func(d *Delivery) { d.URL = "https://user:pass@example.test/hook" }, ErrEndpointRejected},
		{"fragment", func(d *Delivery) { d.URL = "https://example.test/hook#fragment" }, ErrEndpointRejected},
		{"http disabled", func(d *Delivery) { d.URL = "http://example.test/hook" }, ErrEndpointRejected},
		{"payload limit", func(d *Delivery) { d.Payload = make([]byte, 1025) }, ErrPayloadTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			delivery := testDelivery()
			test.mutate(&delivery)
			_, err := client.Deliver(context.Background(), delivery)
			if err == nil {
				t.Fatal("Deliver() error = nil")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Deliver() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestTransportAndObserverPanicsAreIsolated(t *testing.T) {
	var observations atomic.Int32
	client := newClient(testConfig(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		panic("untrusted transport")
	}), nil, nil, ObserverFunc(func(context.Context, Result) {
		observations.Add(1)
		panic("untrusted observer")
	}))
	t.Cleanup(func() { _ = client.stop(context.Background()) })
	result, err := client.Deliver(context.Background(), testDelivery())
	if !errors.Is(err, ErrNetwork) || !errors.Is(result.Err, ErrNetwork) {
		t.Fatalf("Deliver() = %#v, %v", result, err)
	}
	if observations.Load() != 1 {
		t.Fatalf("observer calls = %d, want 1", observations.Load())
	}
}
