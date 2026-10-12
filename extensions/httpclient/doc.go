// Package httpclient provides one configured outbound HTTP client per named
// downstream. Importing this package is side-effect free; applications
// explicitly compose Bundle or opt in through the autoload subpackage.
//
// # Usage
//
// Configure a named client below plugins.http-client, declare an exact typed
// input at package scope, and read the standard *http.Client inside the
// consuming factory:
//
//	var gateway = plugin.RefToInstance[*httpclient.Client](httpclient.Key, "gateway")
//
//	type payments struct {
//		client *http.Client
//	}
//
//	var paymentsDefinition = plugin.Define(
//		"payments",
//		func(ctx plugin.BuildContext) (*payments, error) {
//			return &payments{client: gateway.Get(ctx).Value.Client()}, nil
//		},
//		plugin.Options[*payments]{
//			Inputs: plugin.Inputs(gateway),
//		},
//	)
//
//	func Definition() plugin.Definition { return paymentsDefinition }
//
//	func Bundle() plugin.Bundle { return plugin.BundleOf(paymentsDefinition) }
//
//	func (p *payments) Charge(ctx context.Context, payload []byte) (*http.Response, error) {
//		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://payments.example/charge", bytes.NewReader(payload))
//		if err != nil {
//			return nil, err
//		}
//		return p.client.Do(request)
//	}
//
// The configured base_url is for the operator and for the caller that
// configures its request library with it (resty's SetBaseURL); the plugin
// does not rewrite request URLs, so what goes on the wire is what the caller
// builds with the standard net/http API. base_url is operations
// configuration rather than a data-sourced value, so the plugin applies no
// SSRF allowlist: Client() accepts any URL the caller builds, and a caller
// that composes request URLs from user input owns that protection.
//
// An error returned by the standard client may be net/http's own
// *url.Error, which quotes the request URL -- its query included -- because
// net/http produces it and callers classify transport failures through the
// errors.As chain it preserves. Errors this package constructs never quote
// the URL; a caller that logs a *url.Error and needs the query redacted does
// so at that boundary.
//
// # Retries
//
// The retry policy covers exactly the outcomes where another attempt can
// land on a healthy peer: connection-level failures, 502, 503, 504, and 429.
// A status of 500 is not retried -- the peer processed the request and
// failed, and its answer belongs to the caller. An attempt is only made
// again when the method is idempotent (GET, HEAD, OPTIONS, PUT, DELETE) or
// the caller marked the context with Idempotent, and when the request body
// can be replayed in full; a one-shot body is sent once. An attempt's span,
// traceparent header, timeout, and duration metric are per attempt: three
// attempts are three spans and three samples, not one call counted three
// times. Idle connections of a finally returned response are drained before
// the wait between attempts; a response that is handed back is handed back
// with its body intact.
//
// # Timeouts
//
// timeout bounds one attempt including reading its response body.
// total_timeout bounds the whole call -- every attempt, every wait, and the
// final body read -- and defaults to timeout. A caller context with an
// earlier deadline wins over both, and when the remaining budget cannot
// cover a wait and another attempt, the last response is returned rather
// than the call being cut short mid-retry.
//
// # TLS and observability
//
// tls.ca_file pins the peer to a private CA, tls.cert_file/key_file present
// a client certificate, and tls.server_name overrides the verified name;
// there is no switch that disables verification. A composition that selected
// the tracing plugin gets one client span per attempt with W3C propagation
// headers on the attempt's own request; one that selected the metrics plugin
// gets http_client_requests_total, http_client_request_duration_seconds,
// and http_client_retries_total, each labelled by the instance name and
// never by URL, host, or path. Neither selection is required.
//
// # Lifecycle
//
// A client holds a connection pool, not work: it has no Drain, and Stop --
// which refuses further requests and closes idle connections -- is correct
// whether or not anything else ran.
package httpclient
