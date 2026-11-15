package ginmount_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/interop/ginmount"
	"github.com/xbcio/xbc/transport/web/extensions/testing/apptest"
)

// ginmountConfig is the smallest configuration that activates the plugin; the
// Registrars themselves are inputs, not configuration.
const ginmountConfig = "plugins:\n  ginmount:\n    enabled: true\n"

// apiPlugin is the application-shaped Registrar: it owns a Gin middleware
// stack and its routes, exactly as a host with an existing Gin application
// would.
type apiPlugin struct{}

func (*apiPlugin) MountPath() string { return "/api/v1" }

func (*apiPlugin) RegisterGin(engine *gin.Engine) {
	engine.Use(func(c *gin.Context) {
		c.Header("X-Test-Middleware", "yes")
		c.Next()
	})
	group := engine.Group("")
	group.GET("/hello/:name", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"name": c.Param("name"), "q": c.Query("q")})
	})
	group.POST("/echo", func(c *gin.Context) {
		var body struct {
			Message string `json:"message"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": body.Message})
	})
}

var apiDefinition = plugin.Define(
	"ginmount-test-api",
	func(plugin.BuildContext) (*apiPlugin, error) { return &apiPlugin{}, nil },
	plugin.Options[*apiPlugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[ginmount.Registrar](func(value *apiPlugin) ginmount.Registrar { return value }),
		),
	},
)

var apiBundle = plugin.BundleOf(apiDefinition)

// sharedPlugin declares the same prefix as apiPlugin. It proves the prefix is
// mounted once: a second mount at the same path would be refused by the route
// table as an overlap before the application could start.
type sharedPlugin struct{}

func (*sharedPlugin) MountPath() string { return "/api/v1" }

func (*sharedPlugin) RegisterGin(engine *gin.Engine) {
	engine.GET("/shared", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"shared": true})
	})
}

var sharedDefinition = plugin.Define(
	"ginmount-test-shared",
	func(plugin.BuildContext) (*sharedPlugin, error) { return &sharedPlugin{}, nil },
	plugin.Options[*sharedPlugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[ginmount.Registrar](func(value *sharedPlugin) ginmount.Registrar { return value }),
		),
	},
)

var sharedBundle = plugin.BundleOf(sharedDefinition)

// nativePlugin keeps its routes in the XBC route table: one whose handler body
// is still Gin-shaped, which is the per-route migration path Handler exists
// for, and one ordinary XBC handler.
type nativePlugin struct{}

func (*nativePlugin) RegisterRoutes(router *web.Router) {
	router.GET("/native/:id", ginengine.Handler(func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "q": c.Query("q")})
	})).Auth(web.Public())
	router.GET("/native/plain", func(_ context.Context, c *web.Ctx) error {
		c.JSON(http.StatusOK, map[string]any{"native": true, "q": c.Query("q")})
		return nil
	}).Auth(web.Public())
}

var nativeDefinition = plugin.Define(
	"ginmount-test-native",
	func(plugin.BuildContext) (*nativePlugin, error) { return &nativePlugin{}, nil },
	plugin.Options[*nativePlugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *nativePlugin) web.RouteContributor { return value }),
		),
	},
)

var nativeBundle = plugin.BundleOf(nativeDefinition)

func TestMountedGinEngineAndHandlerAdapter(t *testing.T) {
	app := apptest.New(t,
		apptest.WithBundles(ginmount.Bundle(), apiBundle, sharedBundle, nativeBundle),
		apptest.WithYAML(ginmountConfig),
	)

	t.Run("mounted route strips the prefix", func(t *testing.T) {
		response, body := getJSON(t, app, "/api/v1/hello/world?q=x")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		if got := response.Header.Get("X-Test-Middleware"); got != "yes" {
			t.Errorf("X-Test-Middleware = %q, want %q", got, "yes")
		}
		if body["name"] != "world" || body["q"] != "x" {
			t.Errorf("body = %v, want name=world q=x", body)
		}
	})

	t.Run("mounted body binding", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, app.URL("/api/v1/echo"), strings.NewReader(`{"message":"hi"}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Client.Do(request)
		if err != nil {
			t.Fatalf("POST /api/v1/echo: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		var body map[string]any
		decodeBody(t, response, &body)
		if body["message"] != "hi" {
			t.Errorf("body = %v, want message=hi", body)
		}
	})

	t.Run("registrars sharing a prefix share the engine", func(t *testing.T) {
		response, body := getJSON(t, app, "/api/v1/shared")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		if body["shared"] != true {
			t.Errorf("body = %v, want shared=true", body)
		}
	})

	t.Run("handler adapter copies route params and query", func(t *testing.T) {
		response, body := getJSON(t, app, "/native/abc?q=y")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		if body["id"] != "abc" || body["q"] != "y" {
			t.Errorf("body = %v, want id=abc q=y", body)
		}
	})

	t.Run("native route beside the mount is unaffected", func(t *testing.T) {
		response, body := getJSON(t, app, "/native/plain?q=z")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		if body["native"] != true || body["q"] != "z" {
			t.Errorf("body = %v, want native=true q=z", body)
		}

		// The mount still answers its own subtree in the same process and the
		// same coordinate space.
		response, body = getJSON(t, app, "/api/v1/hello/beside?q=w")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		if body["name"] != "beside" || body["q"] != "w" {
			t.Errorf("body = %v, want name=beside q=w", body)
		}
	})
}

func TestPluginWithoutRegistrarsMountsNothing(t *testing.T) {
	app := apptest.New(t,
		apptest.WithBundles(ginmount.Bundle()),
		apptest.WithYAML(ginmountConfig),
	)

	response, _ := getJSON(t, app, "/api/v1/hello/world")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: a plugin with no Registrar must mount no subtree", response.StatusCode, http.StatusNotFound)
	}
}

func getJSON(t *testing.T, app *apptest.App, path string) (*http.Response, map[string]any) {
	t.Helper()
	response, err := app.Client.Get(app.URL(path))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	var body map[string]any
	decodeBody(t, response, &body)
	return response, body
}

func decodeBody(t *testing.T, response *http.Response, out any) {
	t.Helper()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if len(payload) == 0 {
		return
	}
	if err := json.Unmarshal(payload, out); err != nil {
		t.Fatalf("decode response body %q: %v", payload, err)
	}
}
