module github.com/xbcio/xbc/examples

go 1.25.0

// Neither github.com/xbcio/xbc nor github.com/xbcio/xbc/web appears in require,
// for the same reason web/go.mod omits the core: neither module has a published
// tag yet, so any version string here -- including a zero pseudo-version --
// makes the go command try to resolve a revision that does not exist, which
// breaks even workspace-mode builds. Resolution currently comes from the
// repository-root go.work. A local replace would hide exactly the property
// these examples exist to demonstrate, namely that an outside user can `go get`
// the framework and build this. Once core and web are tagged, run `go mod tidy`
// here (with GOWORK=off) and both require lines appear with real versions.
// Release order is fixed: core -> web -> examples.
require github.com/gin-gonic/gin v1.12.0
