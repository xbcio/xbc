package ginmount_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web/extensions/interop/ginmount"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
	"github.com/xbcio/xbc/transport/web/extensions/testing/apptest"
)

// recordingController registers one route and records when its Setup ran, so a
// test can assert both the call order and the group it was given.
type recordingController struct {
	name  string
	order *[]string
}

func (c *recordingController) Setup(group *gin.RouterGroup) {
	*c.order = append(*c.order, c.name)
	group.GET("/"+c.name, func(ctx *gin.Context) { ctx.String(http.StatusOK, c.name) })
}

func TestControllerRegistrar(t *testing.T) {
	t.Run("mount path", func(t *testing.T) {
		registrar := ginmount.NewControllerRegistrar("/api/v1", nil, &recordingController{name: "a", order: new([]string)})
		if got := registrar.MountPath(); got != "/api/v1" {
			t.Errorf("MountPath() = %q, want %q", got, "/api/v1")
		}
	})

	t.Run("group handlers apply and setups run in order", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		order := make([]string, 0, 2)
		handler := func(c *gin.Context) {
			c.Header("X-Group", "yes")
			c.Next()
		}
		registrar := ginmount.NewControllerRegistrar("/api/v1", []gin.HandlerFunc{handler},
			&recordingController{name: "first", order: &order},
			&recordingController{name: "second", order: &order},
		)

		engine := gin.New()
		registrar.RegisterGin(engine)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/second", nil))

		if got := recorder.Code; got != http.StatusOK {
			t.Errorf("status = %d, want %d", got, http.StatusOK)
		}
		if got := recorder.Body.String(); got != "second" {
			t.Errorf("body = %q, want %q", got, "second")
		}
		if got := recorder.Header().Get("X-Group"); got != "yes" {
			t.Errorf("X-Group = %q, want %q", got, "yes")
		}
		wantOrder := []string{"first", "second"}
		if len(order) != len(wantOrder) || order[0] != wantOrder[0] || order[1] != wantOrder[1] {
			t.Errorf("Setup order = %v, want %v", order, wantOrder)
		}
	})

	t.Run("nil controller panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("NewControllerRegistrar did not panic on a nil controller")
			}
		}()
		ginmount.NewControllerRegistrar("/api/v1", nil, nil)
	})
}

func TestControllerRegistrarIsRegistrar(t *testing.T) {
	var _ ginmount.Registrar = ginmount.NewControllerRegistrar("/api/v1", nil, &recordingController{name: "a", order: new([]string)})
}

// jsonController answers with a plain Gin JSON body, the shape of a controller
// that keeps its own Setup method and response format.
type jsonController struct{}

func (*jsonController) Setup(group *gin.RouterGroup) {
	group.GET("/controller/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"pong": true})
	})
}

// controllerDefinition exports a ControllerRegistrar through the same Registrar
// contract a hand-written registrar uses, so the E2E drives the production
// composition path rather than constructing the plugin value directly.
var controllerDefinition = plugin.Define(
	"ginmount-test-controller",
	func(plugin.BuildContext) (*ginmount.ControllerRegistrar, error) {
		return ginmount.NewControllerRegistrar("/api/v1", nil, &jsonController{}), nil
	},
	plugin.Options[*ginmount.ControllerRegistrar]{
		Exports: plugin.Contracts(
			plugin.ExportAs[ginmount.Registrar](func(value *ginmount.ControllerRegistrar) ginmount.Registrar { return value }),
		),
	},
)

// ginmountAndRequestIDConfig activates the mount and the requestid middleware
// so the trace id can be followed from the inbound header to the response.
const ginmountAndRequestIDConfig = "plugins:\n  ginmount:\n    enabled: true\n  requestid:\n    header: \"X-Request-ID\"\n    trust_incoming: true\n    max_length: 128\n"

func TestControllerRegistrarThroughApplication(t *testing.T) {
	app := apptest.New(t,
		apptest.WithBundles(ginmount.Bundle(), requestid.Bundle(), plugin.BundleOf(controllerDefinition)),
		apptest.WithYAML(ginmountAndRequestIDConfig),
	)

	const traceID = "trace-controller-1"
	request, err := http.NewRequest(http.MethodGet, app.URL("/api/v1/controller/ping"), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("X-Request-ID", traceID)

	response, err := app.Client.Do(request)
	if err != nil {
		t.Fatalf("GET /api/v1/controller/ping: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := response.Header.Get("X-Request-ID"); got != traceID {
		t.Errorf("response X-Request-ID = %q, want %q", got, traceID)
	}

	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if body["pong"] != true {
		t.Errorf("body = %v, want pong=true", body)
	}
}
