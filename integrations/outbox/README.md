# XBC transactional outbox

The package exposes one canonical `Definition()` and a side-effect-free
`Bundle()`. Compose `outbox.Bundle()` explicitly with a plugin that exports the
configured `*gorm.DB` instance and, when the worker is enabled, zero or one
plugin exporting `outbox.Publisher`:

```go
app, err := xbc.New(xbc.WithBundles(
    gormplugin.Bundle(),
    publisher.Bundle(),
    outbox.Bundle(),
    orders.Bundle(),
))
```

The outbox configuration selects the GORM producer by instance name:

```yaml
plugins:
  outbox:
    db_instance: writer
    table: xbc_outbox_events
    migrate: true
    worker:
      enabled: true
```

`Service` is the Definition's sole primary value and is also exported as the
`Dispatcher` contract. `Service.Enqueue(ctx, tx, event)` writes through the
caller's existing GORM transaction; it never starts or commits that transaction.
Run XBC's migration stage to create the table when `migrate` is enabled.

The optional worker is prepared during startup and begins polling only after
XBC releases its global traffic gate. Replicas coordinate with renewable,
conditional-update leases and publish outside database transactions. Delivery
is at least once, so publishers and consumers must use `Event.ID` for
idempotency.

Import `github.com/xbcio/xbc/integrations/outbox/autoload` only when intentionally
using XBC's process-wide default composition. Importing the parent package or
calling `Bundle()` has no registration side effects.
