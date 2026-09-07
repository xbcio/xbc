// Package pprof exposes Go runtime profiles through XBC's Web transport.
//
// # Usage
//
// Compose Web and pprof explicitly; ordinary imports have no registration
// effects:
//
//	app, err := xbc.New(xbc.WithBundles(
//		web.Bundle(),
//		pprof.Bundle(),
//	))
//
// A plugins.pprof section activates the Definition. Remote access should
// disable the loopback exception and use a secret of at least 32 bytes:
//
//	plugins:
//	  pprof:
//	    enabled: true
//	    path: /debug/pprof
//	    token: ${PPROF_TOKEN}
//	    allow_loopback: false
//
// A token-authenticated client can request a profile with the configured
// header (X-XBC-Pprof-Token by default):
//
//	func goroutineProfile(ctx context.Context, token string) (*http.Response, error) {
//		req, err := http.NewRequestWithContext(
//			ctx,
//			http.MethodGet,
//			"http://127.0.0.1:8080/debug/pprof/goroutine?debug=1",
//			nil,
//		)
//		if err != nil {
//			return nil, err
//		}
//		req.Header.Set("X-XBC-Pprof-Token", token)
//		return http.DefaultClient.Do(req)
//	}
//
// The endpoint is disabled unless plugins.pprof exists and enabled is true.
// Once enabled, every request is authorized from the direct peer address or a
// constant-time token comparison; forwarded client-address headers are never
// trusted. Runtime profiles can contain sensitive data and consume substantial
// CPU, so keep the endpoint on a restricted management network.
//
// This plugin exports web.RouteContributor but owns no standalone server; Web
// controls serving and connection-drain lifecycle. Applications using xbc.Run
// may opt into process-global composition through the leaf autoload adapter.
package pprof
