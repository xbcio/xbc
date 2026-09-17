// Command quickstart demonstrates explicit, side-effect-free XBC composition.
//
// Run from the repository root with:
//
//	go run ./examples/quickstart --config examples/quickstart/application.yml
//	curl localhost:8080/api/v1/hello
//
// Use the doctor subcommand to validate the complete plan without constructing
// resources or opening a listener.
//
// @title XBC Quickstart API
// @version dev
// @description XBC quickstart service.
// @BasePath /api/v1
//
//go:generate go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g main.go -d .,internal/greeter --parseInternal --parseDependencyLevel 1 -o docs --ot go,json
package main

import (
	"github.com/xbcio/xbc"
	_ "github.com/xbcio/xbc/examples/quickstart/docs"
	"github.com/xbcio/xbc/examples/quickstart/internal/greeter"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/openapi/swag"
	"github.com/xbcio/xbc/transport/web/extensions/response/biz"
	"github.com/xbcio/xbc/transport/web/extensions/security/cors"
	"github.com/xbcio/xbc/transport/web/prelude"
)

func main() {
	xbc.Run(xbc.WithBundles(
		prelude.Bundle(),
		ginengine.Bundle(),
		biz.Bundle(),
		cors.Bundle(),
		swag.Bundle(),
		greeter.Bundle(),
	))
}
