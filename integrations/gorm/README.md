# XBC GORM integration

This module owns GORM and its database drivers without adding them to XBC core.
Import `github.com/xbcio/xbc/integrations/gorm` and compose
`gormplugin.Bundle()` explicitly. Importing the package and calling
`Definition()` or `Bundle()` are side-effect free. Executables that intentionally
use XBC's default composition may instead blank-import
`github.com/xbcio/xbc/integrations/gorm/autoload`.

```go
app, err := xbc.New(
    xbc.WithBundles(
        gormplugin.Bundle(),
        application.Bundle(),
    ),
)
```

Each key below `plugins.gorm` is a separate plugin instance whose sole primary
value is one `*gorm.DB`:

```yaml
plugins:
  gorm:
    default:
      driver: mysql
      dsn: ${MYSQL_DSN}
      max_open_conn: 20
      max_idle_conn: 10
      conn_max_lifetime: 1h
      conn_max_idle_time: 30m
      prepare_stmt: false
      skip_default_transaction: false
    readonly:
      driver: postgres
      dsn: ${POSTGRES_READONLY_DSN}
```

Consumers use a typed input declared at package scope:

```go
var readonlyDB = plugin.RefToInstance[*gorm.DB](gormplugin.Key, "readonly")

var definition = plugin.Define(
    "reporting-repository",
    func(ctx plugin.BuildContext) (*Repository, error) {
        return NewRepository(readonlyDB.Get(ctx).Value), nil
    },
    plugin.Options[*Repository]{Inputs: plugin.Inputs(readonlyDB)},
)
```

Supported drivers are `mysql` (default), `postgres`, `sqlite`, and `sqlserver`.
`dsn` is required. `max_open_conn` defaults to 20 and must be at least 1;
`max_idle_conn` defaults to 10 and must be between 0 and `max_open_conn`.
Connection lifetime defaults to one hour and idle time to 30 minutes; set a
duration to zero to disable that limit. Construction opens and configures the
pool, Init verifies it with a ping, and Stop idempotently closes prepared
statements and the underlying `sql.DB`.
