package requestid

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
)

func TestDefinitionAndOrderingContract(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware := New()
	order := middleware.Order()
	if order.Phase != web.PhaseObserve || len(order.Before) != 1 || len(order.After) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
	if ref := order.Before[0]; ref.Key() != accesslog.Key || ref.InstanceName() != "" || ref.Required() {
		t.Fatalf("accesslog order reference = %#v, want optional accesslog key", ref)
	}
}

func TestPropagatesValidatedIncomingID(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	p := New()
	router := gin.New()
	router.Use(web.Handle(p.handle))
	router.GET("/", func(c *gin.Context) {
		fromGin, okGin := From(web.NewCtx(c))
		fromRequest, okRequest := FromRequest(c.Request)
		if !okGin || !okRequest || fromGin != "client-123" || fromRequest != fromGin {
			t.Fatalf("propagation = %q/%v %q/%v", fromGin, okGin, fromRequest, okRequest)
		}
		c.String(http.StatusOK, fromGin)
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(defaultHeader, "client-123")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Header().Get(defaultHeader) != "client-123" || response.Body.String() != "client-123" {
		t.Fatalf("response ID = header %q body %q", response.Header().Get(defaultHeader), response.Body.String())
	}
}

func TestRejectsMalformedDuplicateAndOversizedIDsByReplacingThem(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	for _, values := range [][]string{{"bad id"}, {strings.Repeat("a", defaultMaxLength+1)}, {"one", "two"}} {
		t.Run(strings.Join(values, "_"), func(t *testing.T) {
			p := New()
			router := gin.New()
			router.Use(web.Handle(p.handle))
			router.GET("/", func(c *gin.Context) {
				id, _ := From(web.NewCtx(c))
				c.String(http.StatusOK, id)
			})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, value := range values {
				request.Header.Add(defaultHeader, value)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			id := response.Header().Get(defaultHeader)
			if len(id) != 32 || response.Body.String() != id {
				t.Fatalf("replacement = %q", id)
			}
			for _, value := range values {
				if id == value {
					t.Fatalf("accepted invalid ID %q", value)
				}
			}
		})
	}
}

func TestGeneratedIDsAreConcurrentAndUnique(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	p := New()
	router := gin.New()
	router.Use(web.Handle(p.handle))
	router.GET("/", func(c *gin.Context) {
		id, _ := From(web.NewCtx(c))
		c.String(http.StatusOK, id)
	})

	const requests = 200
	ids := make(chan string, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			ids <- response.Header().Get(defaultHeader)
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]struct{}, requests)
	for id := range ids {
		if len(id) != 32 {
			t.Fatalf("generated ID %q has wrong length", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate generated ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{
		{Header: "Bad\nHeader", MaxLength: 128},
		{Header: defaultHeader, MaxLength: 15},
		{Header: defaultHeader, MaxLength: 1025},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate(%#v) succeeded", cfg)
		}
	}
}
