// Command quickstart demonstrates explicit, side-effect-free XBC composition.
//
// Run from the repository root with:
//
//	go run ./examples/quickstart --config examples/quickstart/application.yml
//	curl localhost:8080/api/v1/hello
//
// Use the doctor subcommand to validate the complete plan without constructing
// resources or opening a listener.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/examples/quickstart/internal/greeter"
	"github.com/xbcio/xbc/transport/web/biz"
	"github.com/xbcio/xbc/transport/web/cors"
	"github.com/xbcio/xbc/transport/web/integrations/swagger"
	"github.com/xbcio/xbc/transport/web/prelude"
)

func main() {
	app, err := xbc.New(xbc.WithBundles(
		prelude.Bundle(),
		biz.Bundle(),
		cors.Bundle(),
		swagger.Bundle(),
		greeter.Bundle(),
	))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code, err := app.Execute(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if code != 0 {
		os.Exit(code)
	}
}
