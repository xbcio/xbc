// Package webhook provides bounded asynchronous and synchronous outbound
// webhook delivery with HMAC signing, SSRF-resistant dialing, safe redirects,
// retry classification, and graceful draining. Importing this package is
// side-effect free; applications explicitly compose Bundle or opt in through
// the autoload subpackage.
//
// # Usage
//
// Configure a named client below plugins.webhook, declare an exact typed input
// at package scope, and read its pre-bound value inside the consuming factory:
//
//	var partnersClient = plugin.RefToInstance[*webhook.Client](webhook.Key, "partners")
//
//	type notifier struct {
//		client *webhook.Client
//	}
//
//	var notifierDefinition = plugin.Define(
//		"notifier",
//		func(ctx plugin.BuildContext) (*notifier, error) {
//			return &notifier{client: partnersClient.Get(ctx).Value}, nil
//		},
//		plugin.Options[*notifier]{
//			Inputs: plugin.Inputs(partnersClient),
//		},
//	)
//
//	func Definition() plugin.Definition { return notifierDefinition }
//
//	func Bundle() plugin.Bundle { return plugin.BundleOf(notifierDefinition) }
//
//	func applicationBundle() plugin.Bundle {
//		return plugin.CombineBundles(webhook.Bundle(), Bundle())
//	}
//
//	func (n *notifier) Deliver(ctx context.Context, target string, payload, secret []byte) (webhook.Result, error) {
//		return n.client.Deliver(ctx, webhook.Delivery{
//			URL:     target,
//			Event:   "order.created",
//			Payload: payload,
//			Secret:  secret,
//		})
//	}
//
//	func (n *notifier) Enqueue(ctx context.Context, target string, payload, secret []byte) error {
//		return n.client.Enqueue(ctx, webhook.Delivery{
//			URL:     target,
//			Event:   "order.created",
//			Payload: payload,
//			Secret:  secret,
//		})
//	}
//
// Deliver applies retries synchronously. Enqueue uses a bounded queue and the
// configured block or reject backpressure policy; the queued workers wait for
// XBC's global traffic gate before sending, so a delivery accepted during
// startup is not dispatched until every Plugin finished its traffic
// preparation. Payload, response, retry, and request limits are bounded. Drain
// refuses further Enqueue calls and waits within the drain budget for the
// queued deliveries without cancelling their workers. A worker that is still
// waiting at the traffic gate when the drain arrives abandons its accepted
// deliveries instead of collecting them: each is reported to the Observer with
// ErrClosed and the discarded count is logged, so an accepted delivery is
// never dropped silently. Synchronous Deliver stays available through Drain;
// Stop then refuses further Deliver calls, waits for the calls already in
// flight, aborts whatever outlives its own budget, and closes idle
// connections.
//
// HTTPS is required by default. The default transport rejects private and
// otherwise non-public targets, validates every DNS answer and the actual peer,
// and revalidates redirects while stripping cross-origin sensitive headers.
// HMAC metadata is added through the package headers. A custom Transport
// bypasses actual-dial SSRF validation and must provide equivalent protection;
// AllowHTTP and custom endpoint policies should be restricted to controlled
// deployments. Keep signing secrets out of logs and long-lived shared state.
package webhook
