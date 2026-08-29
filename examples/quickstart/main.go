// Command quickstart is the smallest complete xbc application: an import
// list and one call.
//
// Everything the application is made of is expressed as an import. The web
// module's autoload package contributes an HTTP server; the greeter package
// contributes a route. Neither is named again below, and neither is passed to
// anything -- their registration packages declare them into the catalog, and
// xbc.Run freezes that catalog, loads configuration, orders the plugins by
// their declared dependencies and drives their lifecycle.
//
// Run these commands from the examples module directory:
//
//	go run ./quickstart --config quickstart/application.yml
//	curl localhost:8080/api/v1/hello
//
// To see what would be assembled without starting anything -- no port bound,
// no connection opened -- use the built-in doctor subcommand:
//
//	go run ./quickstart doctor --config quickstart/application.yml
package main

import (
	"github.com/xbcio/xbc"
	_ "github.com/xbcio/xbc/examples/quickstart/internal/greeter"
	_ "github.com/xbcio/xbc/transport/web/autoload"
)

func main() { xbc.Run() }
