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
	github.com/gin-gonic/gin v1.12.0
	github.com/stretchr/testify v1.12.1
)
