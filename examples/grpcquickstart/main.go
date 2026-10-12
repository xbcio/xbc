// Command grpcquickstart demonstrates a gRPC service composed from the gRPC
// starter and one business plugin, configured from the environment alone.
//
// Run from the repository root with:
//
//	go run ./examples/grpcquickstart
//
// The example ships no configuration file: the starter's lowest configuration
// layer turns on access logging, the gRPC server reads its own defaults, and
// every setting a deployment changes is an environment variable. The listener
// defaults to :9090; move it with
//
//	XBC_GRPC_ADDR=127.0.0.1:50051 go run ./examples/grpcquickstart
//
// and call it with any gRPC client, for example grpcurl with the schema from
// proto/ (reflection stays off by default because it publishes the schema;
// XBC_GRPC_REFLECTION_ENABLED=true turns it on):
//
//	grpcurl -plaintext -import-path examples/grpcquickstart/proto -proto greeter/v1/greeter.proto \
//		-d '{"name":"XBC"}' localhost:9090 greeter.v1.GreeterService/SayHello
//	grpcurl -plaintext localhost:9090 grpc.health.v1.Health/Check
//
// Doctor and validate work without a file too:
//
//	go run ./examples/grpcquickstart doctor
//	go run ./examples/grpcquickstart validate
//
// The committed code under internal/gen is generated from proto/ by buf, with
// both protoc plugins pinned in buf.gen.yaml. Regenerate from this directory:
//
//go:generate go run github.com/bufbuild/buf/cmd/buf@v1.73.0 generate
package main

import (
	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/examples/grpcquickstart/internal/greeter"
	grpcstarter "github.com/xbcio/xbc/transport/grpc/starter"
)

func main() {
	xbc.Run(
		xbc.WithStarter(grpcstarter.GRPC()),
		xbc.WithBundles(greeter.Bundle()),
	)
}
