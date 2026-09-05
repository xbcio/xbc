# xbc

xbc 是一个与传输协议无关的 Go 插件应用运行时。Core 只负责显式组合、严格配置绑定、依赖规划、资源构造、生命周期、托管任务和逆序关机；Web 运行栈及外部系统集成按实际 owner 和依赖重量隔离，不进入 core 的依赖闭包。

仓库仍处于首个正式 tag 之前。根 `go.work` 当前联结 20 个 module（core、examples、`transport/web`、7 个 Web integration、10 个协议无关 integration），架构测试保证每个仓库内 `go.mod` 都被 workspace、Makefile 和 CI 覆盖。可发布子 module 禁止本地 `replace`、`v0.0.0` 或伪版本；正式发布必须按依赖拓扑使用真实 tag。

## 目录与边界

| 路径 | 职责 |
| --- | --- |
| 根目录 | `xbc.Run`、`App`、`New` 等六符号应用门面；普通应用唯一推荐入口 |
| `internal/runtime/` | App 状态、启动执行、生命周期、托管任务、关机和进程适配 |
| `internal/assembly/` | Definition 实例规划、配置绑定、typed input/contract 校验、依赖图和 Construct 事务 |
| `internal/cli/` | 仅依赖标准库的命令解析 |
| `plugin/` | 协议无关的 Definition、typed Inputs/contracts、Bundle、顺序和生命周期 SPI；不放具体插件 |
| `config/`、`log/` | 分层配置与日志门面，彼此独立 |
| `transport/web/` | 可选 Gin-backed Web 运行栈；独立 module，不进入 core 依赖闭包 |
| `transport/web/prelude/` | side-effect-free Web 生产基线 Bundle；只组合 canonical Definition，不创建资源或改变进程组合 |
| `transport/web/<name>/` | 15 个与 Web 紧耦合、轻量且同版本发布的内建插件 package |
| `transport/web/integrations/<name>/` | 7 个依赖第三方系统或重型 SDK 的 Web integration；每项独立 module |
| `integrations/<name>/` | 10 个协议无关的外部系统 integration；每项独立 module |
| `examples/` | 独立 module；可运行的消费方示例 |
| `tests/architecture/` | 依赖方向、公开 API、组合副作用和 module 完整性守卫 |

这个布局遵循成熟 Go 项目常见的三条规则：用 `internal` 隐藏非扩展面；按 owner 而不是技术标签组织 package；只在需要独立版本、依赖隔离或发布节奏时拆 Go module。根包 `xbc` 因而保持稳定薄门面，Web 内建能力跟随 Web 运行栈发布，重型集成才承担独立 module 的维护成本。不要新增模糊的 `common`、`utils`、`pkg`，也不要为未实现能力创建空目录或占位 API。未来真正实现新的协议运行栈时再建立 `transport/<stack>`；gRPC 本轮明确不实现。完整约束见[包布局设计](docs/superpowers/specs/2026-08-26-xbc-package-layout-design.md)。

## 已实现插件

每个受 XBC 管理的能力都由 package-level `plugin.Define*` 创建一个 canonical、immutable Definition，并由 `Definition()` 返回同一 handle。工厂依赖只通过 `RefTo`、`RequireOne`、`OptionalOne` 或 `Collect` 声明，interface 能力只通过 `ExportAs`/`Contracts` 导出；每个实现 package 同时提供 side-effect-free `Bundle()` 供应用显式组合。

普通实现 package 和 Prelude import 都不会修改进程状态。应用主路径是 `xbc.New(xbc.WithBundles(...))`；只有刻意采用 `xbc.Run()` 的叶子 executable 才应 blank-import 可选 `.../autoload` 适配器，library 与 Prelude 不使用该路径。

| 所有权 | 路径与插件 | 主要能力 |
| --- | --- | --- |
| Web 运行栈 | `transport/web` | HTTP server、Gin Router、冻结路由目录、Principal 与稳定中间件阶段 |
| Web 内建 package | `transport/web/{biz,recovery,requestid,accesslog,cors,securityheaders,gzip,timeout,ratelimit,apikey,tenant,auditlog,health,pprof,gracefulshutdown}` | 可选业务响应契约、Web 基线、安全、治理与管理能力；随 `transport/web` 一起版本化 |
| Web integration module | `transport/web/integrations/{jwt,session,casbin,idempotency,swagger,metrics,tracing}` | 认证授权、幂等、OpenAPI、Prometheus 与 OpenTelemetry |
| 协议无关 integration module | `integrations/{gorm,redis,objectstorage,elasticsearch,kafka,asynq,outbox,webhook,cron,raft}` | 数据、存储、消息、投递、调度与集群能力 |

`cron` 的分布式模式使用随机 owner token、`SET NX` TTL 租约及 Lua 原子续租/释放；失联节点由 TTL 接管，旧 owner 不能续租或删除新 owner 的锁。`outbox`、`webhook` 等组合插件复用基础能力但保持独立 module 和显式依赖；数据库、Redis、Kafka、Raft 等外部设施插件不会被 Quickstart 默认启用。

## Quickstart

要求 Go 1.25 或更高版本。从仓库根目录运行：

```bash
go run ./examples/quickstart doctor --config examples/quickstart/application.yml
go run ./examples/quickstart --config examples/quickstart/application.yml
```

Quickstart 的 composition root 显式交付无副作用的 Bundle：

```go
app, err := xbc.New(xbc.WithBundles(
    prelude.Bundle(),
    biz.Bundle(),
    cors.Bundle(),
    swagger.Bundle(),
    greeter.Bundle(),
))
```

`prelude.Bundle()` 组合不依赖外部设施的生产基线：`web`、`recovery`、`requestid`、`accesslog`、`securityheaders`、`gzip`、`timeout` 和 `health`；`biz`、需要应用策略的 `cors` 与 `swagger`，以及应用自己的 `greeter` 均保持显式。`doctor` 只执行 Plan，验证配置、activation、typed Inputs/contracts 和依赖图，不创建资源；正常执行随后进入 Construct，并由该事务统一接管成功构造的资源。可直接检查：

```bash
curl -i localhost:8080/api/v1/hello
curl -i -H 'Content-Type: application/json' \
  -d '{"name":"XBC"}' localhost:8080/api/v1/hello
# 未知字段、畸形 JSON、DTO 校验失败统一返回 application/problem+json
curl -i -H 'Content-Type: application/json' \
  -d '{"name":"X","unexpected":true}' localhost:8080/api/v1/hello
curl localhost:8080/api/v1/healthz
curl localhost:8080/api/v1/readyz
curl localhost:8080/api/v1/openapi.json
open http://localhost:8080/api/v1/docs
```

Web 默认提供生产安全的 HTTP timeout、header/body/multipart 上限，且不信任任何代理转发头；只有 `web.trusted_proxies` 明确列出的 IP/CIDR 才能影响客户端地址。404、405、请求体超限、panic、认证授权和严格 DTO 绑定等内建错误统一采用 RFC 9457 `web.ProblemDetail`。业务 handler 推荐使用 `web.Handle` 返回普通 Go error；核心 `web.onerror` 边界集中捕获 `web.Handle`、`web.AbortError` 和 Gin `c.Error` 报告的错误，并通过 typed contracts 组合应用插件导出的 `web.ErrorMapper`，未知错误固定返回不泄漏原因的 500。panic 仍由独立 recovery 中间件处理。可选 `web/biz` 插件提供显式的 `biz.OK` / `Created` / `Paginated` 成功 envelope，并通过 `biz.onerror` 将 `biz.Error` 转换为正确的 4xx/5xx Problem Details，而不是伪装成 HTTP 200。`ProblemDetail` 只是 HTTP 出网 DTO，不应进入领域层。Web 配置的 canonical 位置只有根级 `web:`。

增加协议无关 integration 时，显式导入实现 package 并把它的 Bundle 放入同一个 composition root。例如 Redis：

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

配置所有权、环境变量覆盖和 Quickstart 完整示例见[配置约定](docs/configuration.md)。

## 开发与验证

```bash
make fmt        # 格式化全仓 Go 文件
make check      # gofmt 检查 + 每个 workspace module 的 go vet/go test
make test-race  # 每个 workspace module 的 race test
```

不要用根目录的 `go test ./...` 代替全仓验证：Go 的递归 package pattern 不进入嵌套 module。Makefile 与 CI 从 `go.work` 动态发现 module，架构测试同时扫描磁盘，任何遗漏都会失败。
