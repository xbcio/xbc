module github.com/xbcio/xbc/transport/web

go 1.25.0

// github.com/xbcio/xbc is deliberately absent from require: the core module has
// no published tag yet, so any version string here -- including the zero
// pseudo-version -- makes the go command try to resolve a revision that does not
// exist, which breaks even workspace-mode builds. Resolution currently comes from
// the repository-root go.work. A local replace would paper over this at the cost
// of the one thing transport/web/go.mod must be able to prove, namely that an
// outside user can `go get` it. Once core is tagged, run `go mod tidy` here
// (with GOWORK=off) and the require line appears with the real version. Release
// order is fixed: core -> transport/web -> examples.
require (
	github.com/go-playground/validator/v10 v10.30.3
	github.com/stretchr/testify v1.12.1
)

require (
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/time v0.15.0
)
