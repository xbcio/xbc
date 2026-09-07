// Package elasticsearch provides named timeout-aware Elasticsearch clients with
// bounded asynchronous bulk indexing. This module pins
// github.com/elastic/go-elasticsearch/v8 at v8.19.1. Importing this package is
// side-effect free. Prefer explicit composition with elasticsearch.Bundle();
// executables that intentionally use process-wide autoload may blank-import
// the autoload subpackage.
//
// # Usage
//
// Each named section below plugins.elasticsearch constructs one primary
// *elasticsearch.Client. Its concrete type and BulkIndexer contract resolve to
// the same plugin identity and lifecycle owner. A consumer declares exact typed
// inputs and reads them only during construction:
//
//	var searchClient = plugin.RefToInstance[*elasticsearch.Client](
//		elasticsearch.Key,
//		"search",
//	)
//	var searchBulk = plugin.RefToInstance[elasticsearch.BulkIndexer](
//		elasticsearch.Key,
//		"search",
//	)
//
//	type searchIndex struct {
//		client *elasticsearch.Client
//		bulk   elasticsearch.BulkIndexer
//	}
//
//	var searchDefinition = plugin.Define(
//		"search-index",
//		func(ctx plugin.BuildContext) (*searchIndex, error) {
//			return &searchIndex{
//				client: searchClient.Get(ctx).Value,
//				bulk:   searchBulk.Get(ctx).Value,
//			}, nil
//		},
//		plugin.Options[*searchIndex]{
//			Inputs: plugin.Inputs(searchClient, searchBulk),
//		},
//	)
//
//	func Bundle() plugin.Bundle {
//		return plugin.BundleOf(searchDefinition)
//	}
//
// The composition root includes both elasticsearch.Bundle() and the consumer's
// Bundle. Client.Do applies the configured timeout, and callers must close every
// successful response body. Client.Raw is an explicit escape hatch that does
// not apply that timeout. Bulk admission is bounded and uses the configured
// block or reject policy; Flush is a barrier for previously admitted items.
// During shutdown the primary Client stops admission, drains and flushes its
// worker, then closes the transport.
//
// Supply API keys, passwords, and private certificate authorities through
// secret-backed configuration. Credentials in node URLs are rejected, and this
// package does not include configured credentials in its own errors or logs.
package elasticsearch
