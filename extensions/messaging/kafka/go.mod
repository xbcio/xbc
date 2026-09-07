module github.com/xbcio/xbc/extensions/messaging/kafka

go 1.25.0

// The core module is deliberately absent until it has a real release tag.
// Repository builds resolve XBC imports through go.work; this publishable
// manifest intentionally has no filesystem replace or placeholder version.
require github.com/segmentio/kafka-go v0.4.49

require (
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.1.2 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)
