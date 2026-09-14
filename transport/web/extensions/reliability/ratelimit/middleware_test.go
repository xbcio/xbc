package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

func rateLimitEngine(t *testing.T, cfg Config, downstream *atomic.Int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(web.Handle(p.Handler()))
	engine.GET("/resource", func(c *gin.Context) {
		downstream.Add(1)
		c.Status(http.StatusOK)
	})
	return engine
}

func rateRequest(engine http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	req.RemoteAddr = remoteAddr
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	return response
}

func TestGlobalLimiterReturns429AndRetryAfter(t *testing.T) {
	cfg := Config{Rate: 0.25, Burst: 1, Scope: ScopeGlobal}
	var downstream atomic.Int64
	engine := rateLimitEngine(t, cfg, &downstream)

	first := rateRequest(engine, "192.0.2.1:1000")
	second := rateRequest(engine, "198.51.100.2:2000")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", second.Code)
	}
	retry, err := strconv.Atoi(second.Header().Get("Retry-After"))
	if err != nil || retry < 1 || retry > 4 {
		t.Fatalf("Retry-After = %q, want integer in [1,4]", second.Header().Get("Retry-After"))
	}
	if got := downstream.Load(); got != 1 {
		t.Fatalf("downstream calls = %d, want 1", got)
	}
}

func TestClientIPScopeUsesIndependentBuckets(t *testing.T) {
	cfg := Config{Rate: 0.01, Burst: 1, Scope: ScopeClientIP}
	var downstream atomic.Int64
	engine := rateLimitEngine(t, cfg, &downstream)

	statuses := []int{
		rateRequest(engine, "192.0.2.1:1000").Code,
		rateRequest(engine, "192.0.2.1:1001").Code,
		rateRequest(engine, "198.51.100.2:2000").Code,
	}
	want := []int{http.StatusOK, http.StatusTooManyRequests, http.StatusOK}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", statuses, want)
		}
	}
}

func TestGlobalLimiterIsConcurrencySafe(t *testing.T) {
	const (
		burst    = 5
		requests = 64
	)
	cfg := Config{Rate: 0.000001, Burst: burst, Scope: ScopeGlobal}
	var downstream atomic.Int64
	engine := rateLimitEngine(t, cfg, &downstream)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			response := rateRequest(engine, "192.0.2."+strconv.Itoa(i%8+1)+":1000")
			if response.Code == http.StatusOK {
				allowed.Add(1)
			} else if response.Code != http.StatusTooManyRequests {
				t.Errorf("status = %d, want 200 or 429", response.Code)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got := allowed.Load(); got != burst {
		t.Fatalf("allowed = %d, want %d", got, burst)
	}
	if got := downstream.Load(); got != burst {
		t.Fatalf("downstream calls = %d, want %d", got, burst)
	}
}

func TestRetryAfterClampsInsteadOfOverflowing(t *testing.T) {
	cfg := Config{Rate: 1e-300, Burst: 1, Scope: ScopeGlobal}
	var downstream atomic.Int64
	engine := rateLimitEngine(t, cfg, &downstream)
	_ = rateRequest(engine, "192.0.2.1:1000")
	response := rateRequest(engine, "192.0.2.1:1000")
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if got := response.Header().Get("Retry-After"); got != strconv.Itoa(maxInt) {
		t.Fatalf("Retry-After = %q, want %d", got, maxInt)
	}
}

func TestClientIPLimiterIsConcurrencySafe(t *testing.T) {
	const (
		clients  = 8
		requests = 64
	)
	cfg := Config{Rate: 0.000001, Burst: 1, Scope: ScopeClientIP}
	var downstream atomic.Int64
	engine := rateLimitEngine(t, cfg, &downstream)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			response := rateRequest(engine, "192.0.2."+strconv.Itoa(i%clients+1)+":"+strconv.Itoa(1000+i))
			if response.Code == http.StatusOK {
				allowed.Add(1)
			} else if response.Code != http.StatusTooManyRequests {
				t.Errorf("status = %d, want 200 or 429", response.Code)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got := allowed.Load(); got != clients {
		t.Fatalf("allowed = %d, want one initial request for each of %d clients", got, clients)
	}
}
