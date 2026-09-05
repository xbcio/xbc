# 配置约定

XBC 的配置由 core 统一加载，但每个配置节只由对应能力 owner 解释。可运行的完整基线见 [`examples/quickstart/application.yml`](../examples/quickstart/application.yml)。

## 文件、profile 与优先级

没有 `--config` 时，XBC 从当前工作目录依次查找并使用第一个存在的文件：

1. `application.yml`
2. `configs/application.yml`

显式传入 `--config <path>` 时，该文件必须存在，只使用该文件，不再搜索默认位置。`--profile prod` 会在基础文件之后合并同目录、同扩展名的 `application-prod.yml`；未传该参数时读取 `XBC_PROFILE`。profile 文件是可选 overlay，不存在时不会报错。例如：

```bash
go run ./examples/quickstart --config examples/quickstart/application.yml --profile prod
```

对应 overlay 是 `examples/quickstart/application-prod.yml`。最终优先级从低到高为：字段 `default` 标签、基础 YAML、profile YAML、嵌入式调用方传入的 `config.Options.Overrides`、环境变量。CLI 当前不暴露 `--set`；`Overrides` 只用于自行嵌入配置加载器的程序。

YAML 字符串不会展开 `${VAR}`。不要写 `secret: ${JWT_SECRET}`，应直接使用下文的环境变量覆盖或由应用注入专用 secret 组件。

## 顶级命名空间

```yaml
xbc:
  shutdown_timeout: 30s
  auto_migrate: false

log:
  level: info
  console:
    enabled: true
    format: console

web:
  addr: ":8080"
  base_path: "/api/v1"
  read_timeout: 10s
  read_header_timeout: 5s
  write_timeout: 30s
  idle_timeout: 60s
  max_header_bytes: 1048576
  max_request_body_bytes: 10485760
  max_multipart_memory: 8388608
  trusted_proxies: []

plugins:
  recovery:
    stack: true

app:
  feature_x: true
```

所有权固定如下：

- `xbc`：核心运行时设置，例如整轮关闭预算和是否自动迁移。
- `log`：日志模块设置。
- `web`：HTTP transport 运行栈的唯一配置节。
- `plugins.<key>`：插件的默认配置位置；`key` 必须等于插件的 `Definition.Key`。
- `app`：业务自由配置，框架不解释其结构。

HTTP 地址属于 `transport/web`，因此没有含糊的全局 `server` 节；Web Definition 通过静态 `ConfigPath` 把根级 `web` 声明为唯一 canonical path。

其他插件未声明自定义 `ConfigPath` 时仍使用 `plugins.<key>`。`enabled: false` 可以显式关闭插件；具体插件是始终参与装配还是只在配置节存在时启用，由其 Definition 的 Activation 决定。配置了 `plugins.<key>` 却没有在 composition root 加入对应 Bundle，会作为 orphan configuration 拒绝启动；已知结构中的拼写错误也会被严格校验拒绝。

## 环境变量映射

环境变量名由完整绑定路径生成：加 `XBC_` 前缀，将点号转换为下划线，再转成大写。

| 配置路径 | 环境变量 |
| --- | --- |
| `xbc.shutdown_timeout` | `XBC_XBC_SHUTDOWN_TIMEOUT` |
| `log.level` | `XBC_LOG_LEVEL` |
| `web.addr` | `XBC_WEB_ADDR` |
| `plugins.jwt.secret` | `XBC_PLUGINS_JWT_SECRET` |
| `plugins.redis.cache.password` | `XBC_PLUGINS_REDIS_CACHE_PASSWORD` |

布尔值、数字、duration、字符串及 `[]string` 可直接覆盖；字符串切片使用逗号分隔，例如 `XBC_PLUGINS_JWT_AUDIENCE=admin-api,worker-api`。复杂对象、对象切片和动态 map 不适合通过单个环境变量表达，应放在 YAML、应用私有配置源或通过插件的程序化选项注入。

Web 环境变量统一使用 `XBC_WEB_*`。

## Web 生产基线

应用应把 `transport/web/prelude.Bundle()` 传给 `xbc.WithBundles(...)`。这个轻量 Web starter 组合 `web`、`recovery`、`requestid`、`accesslog`、`securityheaders`、`gzip`、`timeout` 与 `health`；各能力仍使用自己的 `plugins.<key>` 配置。可选业务 envelope、CORS、JWT/session、Casbin、Swagger、Prometheus 和 OpenTelemetry 不在 starter 中，因为它们会选择应用 API 契约、策略、凭据或可选重依赖，应按需把对应 `Bundle()` 加入 composition root。

根级 `web` 的默认安全值为：`read_timeout: 10s`、`read_header_timeout: 5s`、`write_timeout: 30s`、`idle_timeout: 60s`、1 MiB header 上限、10 MiB request body 上限和 8 MiB multipart 内存阈值。`max_request_body_bytes` 是整个请求体的硬上限；`max_multipart_memory` 只决定 multipart parser 在转存临时文件前可使用多少内存，不能替代总大小限制。

`trusted_proxies` 默认为空，因而 `X-Forwarded-For` 等转发头不会影响 Gin 的客户端地址。只有部署在已知反向代理后时，才应填入精确代理 IP 或 CIDR；不要使用 `0.0.0.0/0` 或 `::/0`。例如：

```yaml
web:
  trusted_proxies:
    - "10.20.0.0/16"
    - "2001:db8:42::/64"
```

内建 404、405、413、超时、panic、认证授权错误，以及 Gin 请求绑定/校验错误，都返回 `application/problem+json`（RFC 9457）。公开模型 `web.ProblemDetail` 对齐 Spring 的五个标准成员，并通过顶层扩展成员携带稳定业务 `code`；它只属于 HTTP 出网层，不是领域错误基类。核心 `web.onerror` 中间件是普通错误边界：它捕获 `web.Handle` / `web.AbortError` 和原生 Gin `c.Error` 报告的错误，并组合应用插件提供的 `web.ErrorMapper`；mapper 按依赖顺序首个匹配生效，未知错误固定脱敏为 500，错误不能伪装成 HTTP 200。需要局部错误策略的插件也可用 `web.OnError(...)` 贡献嵌套边界。panic 不属于 `OnError`，仍由独立 recovery 中间件处理。

请求绑定直接使用 Gin 的 `ShouldBindJSON`、`ShouldBindQuery`、`ShouldBindUri`、`ShouldBindHeader` 或 `ShouldBindWith`，不要使用会立即写入 400 响应的 `Bind*` / `MustBind*`。绑定失败时返回 `web.ParamError(err, &request)`；该函数只适配错误，不读取 body、不选择 binder，也不重复执行 validator。校验失败通过 `errors` 返回公开字段名、约束 `code` 和安全 `message`，不回显被拒绝的值。需要拒绝未知 JSON 字段时，应在应用启动阶段显式调用 Gin 官方的 `gin.EnableJsonDecoderDisallowUnknownFields()`；额外格式应实现 Gin 的 `binding.Binding` / `binding.BindingBody` 扩展点，而不是在 Web 包增加平行绑定 API。Swagger integration 会在 `components.schemas.ProblemDetail` 中发布同一错误契约。

需要 GFA 风格统一成功响应的应用可以在 composition root 额外加入 `biz.Bundle()`，并在 handler 中显式返回 `biz.OK`、`biz.Created`、`biz.Accepted` 或 `biz.Paginated`。成功 wire model 为 `{success, code, message, data, requestId}`，分页 payload 为 `{items, total}`；`requestId` 只取 requestid 插件已经校验或生成的值。业务错误可在 HTTP 应用边界使用 `biz.NewError`（默认 422）或 `biz.NewStatusError`；插件贡献的 `biz.onerror` 基于 `web.OnError`，在核心安全边界内将 `biz.Error` 转换为带稳定 `code` 的 Problem Detail。与 GFA 不同，XBC 不把业务失败包装成 HTTP 200，不把未知 error 原文发给客户端，也不会隐式改写流、文件或第三方 handler 的成功响应。该插件没有配置项，把 Bundle 加入 composition root 即是完整 opt-in；若领域包需要保持协议中立，应继续定义普通领域 error，再由应用插件实现自有 `web.ErrorMapper`。

## 单实例与多实例插件

JWT、Cron、Health 等单实例插件直接配置在 `plugins.<key>`。Web 运行栈使用自己的 canonical 根级 `web`：

```yaml
plugins:
  jwt:
    issuer: "orders"
    audience: ["orders-api"]
    # secret 建议由 XBC_PLUGINS_JWT_SECRET 覆盖，至少 32 bytes。
```

Redis、GORM 等多实例插件在下一层使用稳定实例名；实例名也进入依赖解析、启动报告和环境变量路径：

```yaml
plugins:
  redis:
    cache:
      addr: "redis-cache.internal:6379"
      db: 0
      ping: true
    locks:
      addr: "redis-locks.internal:6379"
      db: 1
      ping: true

  gorm:
    primary:
      driver: postgres
      dsn: "host=db.internal user=app dbname=orders sslmode=require"
      max_open_conn: 30
      max_idle_conn: 10
    reporting:
      driver: mysql
      dsn: "app:password@tcp(reporting.internal:3306)/reporting?parseTime=true"
```

多实例 section 下的每个直接子项都是一个实例，不要把连接字段直接写到 `plugins.redis.addr` 或 `plugins.gorm.dsn`。

## 默认 Quickstart 组合

Quickstart 只启用不依赖数据库、Redis、broker、对象存储或集群 peer 的安全 Web 基线：

- `web`、`recovery`、`requestid`、`accesslog`
- `cors`、`securityheaders`、`gzip`
- `health`、`swagger`

程序必须在 composition root 显式加入所需 Bundle，并提供对应配置节。例如增加 Redis：

```go
app, err := xbc.New(xbc.WithBundles(
    prelude.Bundle(),
    redis.Bundle(),
))
```

```yaml
plugins:
  redis:
    default:
      addr: "127.0.0.1:6379"
```

外部设施插件只有在其 Bundle 已加入 composition root 且配置激活对应 Definition 时才会创建资源。生产应用应按需组合，不要为了“预留”而加入所有插件。

## 常用业务组合

### API Key + 审计

API Key 插件只保存 SHA-256 摘要，不应在 YAML 中保存明文 key；比较使用常量时间语义。实际 key 通过安全渠道交给调用方：

```yaml
plugins:
  apikey:
    header: "X-API-Key"
    allow_bearer: true
    min_key_bytes: 32
    static:
      - id: "partner-a-2026-08"
        app_id: "partner-a"
        subject: "partner-a-service"
        # 示例占位摘要；替换为真实 key 的 64 位小写/大写十六进制 SHA-256。
        sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

  auditlog:
    async: true
    queue_size: 2048
    overflow: drop_newest
    sink_timeout: 2s
    request_id_header: "X-Request-ID"
```

路由标记 `.Public()` 时认证插件放行；受保护路由得到统一的 `web.Principal`，审计记录可关联 principal、route name、request ID、状态码和耗时。高频或动态凭据应通过 API Key 插件的 repository 选项注入，而不是频繁改静态 YAML。

### 幂等写接口

只有显式标记 `.Idempotent()` 的路由会进入幂等处理。单进程开发可使用内存 backend：

```yaml
plugins:
  idempotency:
    backend: memory
    header: "Idempotency-Key"
    ttl: 24h
    pending_ttl: 30s
    max_request_bytes: 1048576
    max_response_bytes: 1048576
```

多副本部署应改用共享 Redis，并在 composition root 加入 `redis.Bundle()`：

```yaml
plugins:
  redis:
    coordination:
      addr: "redis.internal:6379"
      password: ""
      db: 2

  idempotency:
    backend: redis
    redis_instance: coordination
    redis_prefix: "orders:idempotency:"
    ttl: 24h
    pending_ttl: 30s
    operation_timeout: 2s
```

幂等 key 不是认证凭据；网关和日志应避免无界记录请求体或敏感 header。

### Session + 租户隔离

`session` 的 Cookie 只携带至少 32 bytes 随机生成的 opaque bearer ID；身份和属性保存在服务端，内置存储只以 ID 的 SHA-256 摘要索引，不是把身份明文或“加密对象”放进 Cookie。单进程开发可用 `memory`，多副本必须选择共享 Redis：

```yaml
plugins:
  redis:
    auth:
      addr: "redis.internal:6379"
      db: 4

  session:
    backend: redis
    redis_instance: auth
    redis_prefix: "orders:session:"
    name: "__Host-xbc_session"
    path: "/"
    http_only: true
    secure: true
    same_site: lax
    ttl: 24h
    idle_ttl: 30m
    touch_interval: 5m

  tenant:
    required: true
    header: "X-Tenant-ID"
    tenant_id_attribute: "tenant_id"
    tenant_ids_attribute: "tenant_ids"
    tenant_attributes_attribute: "tenant_attributes"
    auto_select_single: true
```

登录处理器通过 `session.Manager.Create` 创建 session，再调用 `SetCookie`；权限提升或登录状态变化后使用原子 `Rotate`，登出时使用 `Revoke` 和 `ClearCookie`。session 属性中的 `tenant_id` / `tenant_ids` 必须由服务端认证流程写入。`X-Tenant-ID` 只是从已验证成员集合中选择租户，永远不能自行证明成员关系；匿名请求返回 401，伪造或越权选择返回 403。Cookie 是 bearer credential，生产环境必须使用 HTTPS，并为会改变状态的浏览器请求另行采用 SameSite 和 CSRF 防护策略。

### 事务 Outbox + Webhook

业务数据和待发布事件必须使用同一个调用方持有的 GORM transaction 写入；`outbox.Service.Enqueue(ctx, tx, event)` 不会自行开始或提交事务。dispatcher 用短事务原子 claim 租约，随后在数据库事务外发布，因此提供 at-least-once 而不是 exactly-once：下游必须以 `Event.ID` 去重。

```yaml
plugins:
  gorm:
    primary:
      driver: postgres
      dsn: "host=db.internal user=app dbname=orders sslmode=require"

  outbox:
    db_instance: primary
    table: xbc_outbox_events
    migrate: true
    max_payload_bytes: 1048576
    worker:
      enabled: true
      poll_interval: 1s
      batch_size: 100
      concurrency: 4
      lease_duration: 30s
      renew_interval: 10s
      publish_timeout: 10s
      max_attempts: 10

  webhook:
    partner:
      workers: 4
      queue_size: 1024
      backpressure: block
      max_attempts: 5
      request_timeout: 10s
      max_payload_bytes: 1048576
      max_response_bytes: 65536
      allow_http: false
```

`migrate: true` 只允许 XBC 的 migration stage 建表，不会在普通 `Init` 中隐式改 schema。启用 worker 时必须由应用注入一个 `outbox.Publisher`；它可以适配命名 Kafka producer 或 Webhook client。Webhook 适配器应从受信任的订阅仓库读取目标 URL 和 secret，不要让事件 payload 任意指定凭据。默认 Webhook transport 仅允许 HTTPS 和公网地址，在每次实际 dial 时校验解析 IP，并对每次 redirect 重新校验目标；响应体、队列和重试均有硬上限。私网目标只能通过显式注入的 `EndpointPolicy` 放行，不能仅靠 `allow_http` 绕过 SSRF 策略。

### Asynq + 对象存储

大文件或长任务不要塞进 HTTP handler 或 Redis task payload。先流式写入对象存储，再把稳定 object key 和业务 ID 作为小任务入队；handler 读取对象并按业务幂等键处理：

```yaml
plugins:
  objectstorage:
    assets:
      backend: s3
      max_object_bytes: 67108864
      max_list_items: 1000
      s3:
        endpoint: "s3.internal:9000"
        tls: true
        region: "us-east-1"
        bucket: "orders-assets"
        path_style: true
        # access_key_id / secret_key 应由部署 secret 注入。

  asynq:
    redis:
      addr: "redis.internal:6379"
      db: 5
    queues:
      critical: 6
      default: 3
      low: 1
    default_queue: default
    concurrency: 10
    default_max_retries: 25
    default_timeout: 30m
    shutdown_timeout: 8s
```

`objectstorage` 的 local backend 适合开发和单机，使用受限根目录、原子临时文件替换和流式大小限制；多副本应使用实现 S3 API 的共享 backend。Asynq 在 `OpenTraffic` 后才开始拉取任务，handler 注册在 `Start` 冻结，并在关闭时停止准入和 drain。若“业务提交成功”和“任务入队成功”必须原子关联，应先写 outbox，再由 publisher 入队，不能依赖提交后的双写。

### 分布式 Cron

任务通过 Go API 注册，配置只控制 scheduler 与租约。单副本默认不需要 Redis；多副本必须启用分布式模式并选择一个共享 Redis 实例：

```yaml
plugins:
  redis:
    coordination:
      addr: "redis.internal:6379"
      db: 3

  cron:
    timezone: "Asia/Shanghai"
    seconds: false
    concurrency: skip
    distributed:
      enabled: true
      key_prefix: "orders:cron"
      ttl: 30s
      renew_interval: 10s
      redis_instance: coordination
```

分布式模式用随机 owner token、`SET NX` TTL 租约和 Lua 原子续租/释放，保证同一 job 在健康副本间只有租约 owner 执行；节点失联后由 TTL 触发接管。它提供的是 at-most-one-active-owner 协调，不等于业务事务的 exactly-once：任务仍应设计为幂等，并记录可重试的运行结果。

## 运维端点与 secret

Health 默认只返回聚合状态，`detail_policy: never` 可避免向未认证请求泄漏依赖错误。pprof 和远程关机默认关闭；如确需暴露，应优先绑定仅运维网络可达的地址，并配置至少 32 bytes 的 token：

```yaml
plugins:
  health:
    liveness_path: "/healthz"
    readiness_path: "/readyz"
    detail_policy: never

  pprof:
    enabled: true
    path: "/debug/pprof"
    header: "X-XBC-Pprof-Token"
    allow_loopback: false
    # token 由 XBC_PLUGINS_PPROF_TOKEN 提供。

  gracefulshutdown:
    http:
      enabled: true
      path: "/-/shutdown"
      header: "X-XBC-Shutdown-Token"
      allow_loopback: false
      # token 由 XBC_PLUGINS_GRACEFULSHUTDOWN_HTTP_TOKEN 提供。
```

先在 composition root 加入相应 Bundle，再配置这些 section。不要把 JWT secret、Redis/数据库密码、API key 明文或运维 token 提交到仓库；环境变量只是最低限度的部署接口，生产环境宜由 secret manager 注入。不要信任公网传入的 forwarded headers，除非请求先经过会清洗这些 header 的可信反向代理。远程关机最终复用 core 的统一取消、HTTP drain 和逆序插件关闭路径，不应另行调用 `os.Exit`。
