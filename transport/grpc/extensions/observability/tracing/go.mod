module github.com/xbcio/xbc/transport/grpc/extensions/observability/tracing

go 1.25.0

// Core, transport/grpc, and the neutral tracing plugin
// github.com/xbcio/xbc/extensions/observability/tracing are deliberately
// resolved by the repository workspace until they have real published
// versions. Do not add a local replace or an unpublished placeholder
// requirement here. It is a module of its own so OpenTelemetry never enters
// transport/grpc's closure.
require (
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/sdk v1.44.0
	go.opentelemetry.io/otel/trace v1.45.0
	google.golang.org/grpc v1.81.1
)

require (
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
