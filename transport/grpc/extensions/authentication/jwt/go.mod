module github.com/xbcio/xbc/transport/grpc/extensions/authentication/jwt

go 1.25.0

// github.com/xbcio/xbc, github.com/xbcio/xbc/transport/grpc, and the neutral
// authenticator github.com/xbcio/xbc/extensions/authentication/jwt are
// deliberately absent from require until those modules have published tags.
// Local development resolves them through a workspace; adding a local replace
// or placeholder version here would make this module unpublishable. It is a
// module of its own so the JWT library never enters transport/grpc's closure.
require google.golang.org/grpc v1.81.1

require (
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.3 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
