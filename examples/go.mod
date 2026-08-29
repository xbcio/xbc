module github.com/xbcio/xbc/examples

go 1.25.0

// Neither github.com/xbcio/xbc nor github.com/xbcio/xbc/transport/web appears
// in require. Neither module has a published tag yet, so any version string --
// including a zero pseudo-version -- makes the go command resolve a revision
// that does not exist and breaks even workspace-mode builds. Resolution comes
// from the repository-root go.work. A local replace would hide exactly what
// these examples must prove: an outside user can `go get` the framework and
// build it. Once core and transport/web are tagged, run `go mod tidy` here with
// GOWORK=off so both real requirements appear. Release order is fixed:
// core -> transport/web -> examples.
require github.com/gin-gonic/gin v1.12.0
