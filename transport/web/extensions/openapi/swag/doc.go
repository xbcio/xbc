// Package swag serves API documentation generated from business-code comments
// by github.com/swaggo/swag. It exposes the generated JSON document and an
// embedded Swagger UI through XBC's Web transport.
//
// # Usage
//
// Generate a docs package from the application's annotated handlers, import it
// from the executable so its named document is registered with swaggo/swag,
// and compose this package's Bundle explicitly:
//
//	import (
//		_ "example.com/orders/internal/docs"
//
//		"github.com/xbcio/xbc"
//		"github.com/xbcio/xbc/transport/web/extensions/openapi/swag"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			swag.Bundle(),
//		))
//	}
//
// A plugins.swag section selects the generated instance and configures only
// its public routes; API title, version, models, operations, and responses
// belong to the generated document:
//
//	plugins:
//	  swag:
//	    instance_name: swagger
//	    json_path: /swagger.json
//	    ui_path: /docs
//	    ui_enabled: true
//
// Business handlers use swaggo/swag annotations:
//
//	// createOrder creates an order.
//	//
//	//	@Summary	Create an order
//	//	@Tags		orders
//	//	@Accept		json
//	//	@Produce	json
//	//	@Param		request	body		CreateOrderRequest	true	"Order"
//	//	@Success	201		{object}	CreateOrderResponse
//	//	@Failure	400		{object}	web.ProblemDetail
//	//	@Router		/orders [post]
//	func createOrder(c *gin.Context) {}
//
// Generate and commit the document with a pinned tool version, for example:
//
//	go run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
//	  -g main.go -d .,internal/handlers --parseInternal \
//	  --parseDependencyLevel 1 -o docs --ot go,json
//
// Generated docs packages use swaggo/swag's named registry. Config.InstanceName
// makes the selected document explicit and allows multiple generated instances
// to coexist. Plugin construction fails before startup when that instance is
// absent or its rendered document is invalid. The document is snapshotted once;
// later mutations of generated SwaggerInfo do not change served bytes.
package swag
