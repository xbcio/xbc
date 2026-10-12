module github.com/xbcio/xbc/tests/composition

go 1.25.0

// XBC modules imported below have no published tags yet, so they
// intentionally do not appear in require. Repository development resolves
// them through the root go.work; local replace directives, v0.0.0, and
// synthetic pseudo-versions would only hide a broken publication boundary.
require (
	github.com/stretchr/testify v1.12.1
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)
