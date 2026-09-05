# XBC Elasticsearch integration

This independent module pins `github.com/elastic/go-elasticsearch/v8` at
`v8.19.1`. Each map key below `plugins.elasticsearch` creates one named
`*elasticsearch.Client` primary value. The same value is exported through the
`BulkIndexer` contract, so request and bulk APIs share one plugin identity and
one lifecycle owner.

Prefer explicit composition:

```go
xbc.New(xbc.WithBundles(elasticsearch.Bundle(), application.Bundle()))
```

`Definition()` always returns the package's canonical declaration handle, and
`Bundle()` is side-effect free. Executables that deliberately use default
autoload composition may blank-import
`github.com/xbcio/xbc/integrations/elasticsearch/autoload`.

`Client.Do` is the stable timeout-aware request API. `Client.Raw` is an explicit
escape hatch to the vendor generated API and requires callers to supply their
own bounded contexts. Bulk admission is bounded and supports `block` or `reject`
backpressure. During XBC startup, the bulk worker is admitted as a managed task.
Shutdown stops admission, drains accepted items, flushes, and only then closes
the transport. `Stop`/`Close` is safe before startup, after partial startup, and
on repeated calls.
