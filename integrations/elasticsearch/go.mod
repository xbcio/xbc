module github.com/xbcio/xbc/integrations/elasticsearch

go 1.25.0

// The core module is deliberately absent until it has a real release tag.
// Repository builds resolve XBC imports through go.work; this publishable
// manifest intentionally has no filesystem replace or placeholder version.
require github.com/elastic/go-elasticsearch/v8 v8.19.1

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/elastic/elastic-transport-go/v8 v8.8.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
)
