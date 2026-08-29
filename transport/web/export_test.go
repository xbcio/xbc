package web

import "net"

// setListener injects a pre-bound net.Listener for Start to use instead of
// calling net.Listen itself. This lives here, rather than as a public
// setter, for the same reason net/http keeps its own test-only knobs behind
// an internal _test.go file: a "SetListener" method on the production API
// surface would exist purely so a test could reach into a private
// implementation detail, and every future reader of server.go would have to
// wonder why a production HTTP server exposes swapping its own socket after
// construction.
func (s *Server) setListener(ln net.Listener) {
	s.listener = ln
}
