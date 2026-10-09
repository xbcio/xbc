// Command quickstart demonstrates explicit, side-effect-free XBC composition.
//
// Run from the repository root with:
//
//	go run ./examples/quickstart --config examples/quickstart/application.yml
//	curl localhost:8080/api/v1/hello
//
// Use the doctor subcommand to validate the complete plan without constructing
// resources or opening a listener, and the validate subcommand to construct the
// graph, run each plugin's Preflight, and print the route table and
// authentication policy a start would serve -- still without opening a
// listener:
//
//	go run ./examples/quickstart doctor --config examples/quickstart/application.yml
//	go run ./examples/quickstart validate --config examples/quickstart/application.yml
//
// The example also registers a subcommand of its own, which runs instead of
// booting: it probes a running instance's liveness endpoint at the address this
// same configuration names. A command's arguments are its own, so the shared
// flags precede its name:
//
//	go run ./examples/quickstart --config examples/quickstart/application.yml probe
//
// @title XBC Quickstart API
// @version dev
// @description XBC quickstart service.
// @BasePath /api/v1
//
//go:generate go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g main.go -d .,internal/greeter --parseInternal --parseDependencyLevel 1 -o docs --ot go,json
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/config"
	_ "github.com/xbcio/xbc/examples/quickstart/docs"
	"github.com/xbcio/xbc/examples/quickstart/internal/greeter"
	"github.com/xbcio/xbc/extensions/concurrency/async"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/openapi/swag"
	"github.com/xbcio/xbc/transport/web/extensions/response/biz"
	"github.com/xbcio/xbc/transport/web/extensions/security/cors"
	"github.com/xbcio/xbc/transport/web/prelude"
)

func main() {
	xbc.Run(
		xbc.WithCommand("probe", "check a running instance's liveness endpoint", probe),
		xbc.WithBundles(
			prelude.Bundle(),
			ginengine.Bundle(),
			biz.Bundle(),
			cors.Bundle(),
			swag.Bundle(),
			greeter.Bundle(),
			async.Bundle(),
		),
	)
}

// probe is the example's own subcommand: it runs instead of booting, reads the
// address, base path, and liveness route from the same merged configuration a
// boot would serve from, and opens -- and closes -- its own short-lived
// connection. A deployment job runs it after a rollout, which is why the
// service needs no second binary to check itself.
//
// It returns nil for a healthy instance and an error otherwise, which is the
// whole exit-code contract: the runtime reports the error under the command's
// name and exits 1. The context is canceled when the process is asked to stop,
// so the request below ends with it rather than outliving the process.
func probe(ctx context.Context, env *config.Environment, args []string) error {
	// A command owns its arguments, so a shared flag written after its name
	// arrives here instead of being parsed by the runtime. Saying so is the
	// difference between a usage mistake and a probe that quietly used the
	// searched-for configuration file.
	if len(args) > 0 {
		return fmt.Errorf("probe takes no arguments; the shared flags precede the command name (xbc --config FILE probe), so %v reached the command", args)
	}
	// A bind address is not a dial address: ":8080" names every interface, and
	// this command runs where the process does.
	address := configString(env, "web.addr")
	if strings.HasPrefix(address, ":") {
		address = "127.0.0.1" + address
	}
	url := "http://" + address +
		strings.TrimSuffix(configString(env, "web.base_path"), "/") +
		configString(env, "plugins.health-http.liveness_path")

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %s", url, response.Status)
	}
	fmt.Println("xbc quickstart: " + url + " answered 200")
	return nil
}

// configString reads a configuration path the composition declares. An absent
// section reads as the empty string, which composes a URL that fails naming
// itself rather than a probe that silently checks the wrong address.
func configString(env *config.Environment, path string) string {
	value, _ := env.Get(path).(string)
	return value
}
