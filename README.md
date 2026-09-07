# xbc

xbc 是一个与传输协议无关的 Go 插件应用运行时。Core 只负责显式组合、严格配置绑定、依赖规划、资源构造、生命周期、托管任务和逆序关机；Web 运行栈及外部系统集成按实际 owner 和依赖重量隔离，不进入 core 的依赖闭包。

仓库仍处于首个正式 tag 之前。根 `go.work` 当前联结 23 个 module：22 个产品 module（core、examples、`transport/web`、9 个 Web integration、10 个协议无关 integration）及 1 个 tooling module（`scripts/plugin-snapshots`）。架构测试保证每个仓库内 `go.mod` 都被 workspace、Makefile 和 CI 覆盖。可发布子 module 禁止本地 `replace`、`v0.0.0` 或伪版本；正式发布必须按依赖拓扑使用真实 tag。

## 目录与边界

| 路径 | 职责 |
| --- | --- |
| 根目录 | `xbc.Run`、`App`、`New` 等六符号应用门面；普通应用唯一推荐入口 |
| `runtime/` | 根级低层应用执行 owner：App 状态、私有命令解析、启动执行、生命周期、托管任务、关机和进程适配；普通应用仍应使用根 `xbc` 门面 |
| `plugin/` | Plugin 框架子树；应用以顶层 Definition、typed Inputs/contracts、Bundle 和生命周期 SPI 为门面，不放具体插件 |
| `plugin/model/` | Definition、Input token 与 Bundle 的低层类型擦除表示；供框架和工具使用 |
| `plugin/assembly/` | Definition 实例规划、配置绑定、typed input/contract 校验、依赖图和 Construct 事务 |
| `plugin/autoload/` | 可选的进程级 Bundle 收集器；仅供 runtime 与叶子 `.../autoload` adapter 使用 |
| `config/`、`log/` | 分层配置与日志门面，彼此独立 |
| `security/rbac/` | 协议无关的独立 RBAC 业务插件；拥有 Definition、Config、Manager、Backend、Permission 与 autoload |
| `transport/web/` | 可选 Gin-backed Web 运行栈；独立 module，不进入 core 依赖闭包 |
| `transport/web/prelude/` | side-effect-free Web 生产基线 Bundle；只组合 canonical Definition，不创建资源或改变进程组合 |
| `transport/web/<name>/` | 15 个与 Web 紧耦合、轻量且同版本发布的内建插件 package |
| `transport/web/rbac/` | 非插件的 RBAC Web 薄适配 package；只提供显式 `RequireAll` / `RequireAny` middleware |
| `transport/web/integrations/<name>/` | 9 个依赖第三方系统或重型 SDK 的 Web integration；每项独立 module |
| `integrations/<name>/` | 10 个协议无关的外部系统 integration；每项独立 module |
| `examples/` | 独立 module；可运行的消费方示例 |
| `tests/architecture/` | 依赖方向、公开 API、组合副作用和 module 完整性守卫 |

这个布局遵循成熟 Go 项目常见的三条规则：用根级 `runtime` 统一承载低层进程执行及其私有命令解析，并把 Plugin 模型、装配和可选收集机制归入同一 `plugin` owner；按 owner 而不是技术标签组织 package；只在需要独立版本、依赖隔离或发布节奏时拆 Go module。根包 `xbc` 因而保持稳定薄门面，Web 内建能力跟随 Web 运行栈发布，重型集成才承担独立 module 的维护成本。不要新增模糊的 `common`、`utils`、`pkg`，也不要为未实现能力创建空目录或占位 API。未来真正实现新的协议运行栈时再建立 `transport/<stack>`；gRPC 本轮明确不实现。完整约束由 `AGENTS.md` 与 `tests/architecture/` 中的守卫共同定义。

## 已实现插件

每个受 XBC 管理的能力都由 package-level `plugin.Define*` 创建一个 canonical、immutable Definition，并由 `Definition()` 返回同一 handle。工厂依赖只通过 `RefTo`、`RequireOne`、`OptionalOne` 或 `Collect` 声明，interface 能力只通过 `ExportAs`/`Contracts` 导出；每个实现 package 同时提供 side-effect-free `Bundle()` 供应用显式组合。

普通实现 package 和 Prelude import 都不会修改进程状态。应用主路径是 `xbc.New(xbc.WithBundles(...))`；只有刻意采用 `xbc.Run()` 的叶子 executable 才应 blank-import 可选 `.../autoload` 适配器，library 与 Prelude 不使用该路径。

| 所有权 | 路径与插件 | 主要能力 |
| --- | --- | --- |
| Web 运行栈 | `transport/web` | HTTP server、Gin Router、冻结路由目录、Principal 与稳定中间件阶段 |
| 协议无关业务插件 | `security/rbac`（plugin key `rbac`） | RBAC Definition、Config、Manager、Backend、Permission 与 autoload；不依赖具体传输协议 |
| Web 内建 package | `transport/web/{biz,recovery,requestid,accesslog,cors,securityheaders,gzip,timeout,ratelimit,apikey,tenant,auditlog,health,pprof,gracefulshutdown}` | 可选业务响应契约、Web 基线、安全、治理与管理能力；随 `transport/web` 一起版本化 |
| Web RBAC 适配 package（非插件） | `transport/web/rbac` | 把 `web.CurrentPrincipal` 接入 `security/rbac.Manager`，只提供 `RequireAll` / `RequireAny` |
| Web integration module | `transport/web/integrations/{jwt,session,casbin,casbin-gorm,casbin-redis,idempotency,swagger,metrics,tracing}` | 认证授权、Casbin 持久化/多副本同步、幂等、OpenAPI、Prometheus 与 OpenTelemetry |
| 协议无关 integration module | `integrations/{gorm,redis,objectstorage,elasticsearch,kafka,asynq,outbox,webhook,cron,raft}` | 数据、存储、消息、投递、调度与集群能力 |

`cron` 的分布式模式使用随机 owner token、`SET NX` TTL 租约及 Lua 原子续租/释放；失联节点由 TTL 接管，旧 owner 不能续租或删除新 owner 的锁。`outbox`、`webhook` 等组合插件复用基础能力但保持独立 module 和显式依赖；数据库、Redis、Kafka、Raft 等外部设施插件不会被 Quickstart 默认启用。

业务授权由协议无关的 `security/rbac` 独立插件提供 backend-neutral 的 `Manager`、AND/OR 检查与角色/权限替换；默认精确依赖 `casbin[default]` 导出的 `Backend`。`transport/web/rbac` 只是面向 Web 的 `RequireAll` / `RequireAny` 薄适配器，不拥有 Definition、Config 或 autoload，也不计入 Web 内建插件。Casbin、GORM adapter 和 Redis Watcher 仍分别由三个 integration module 持有。应用自己的用户/角色/权限表、`u<ID>` / `r<ID>` 编码、管理 CRUD、审计和跨业务表事务不进入框架插件。业务代码优先依赖 `security/rbac.Manager`；只有迁移或 Casbin 特有高级操作才直接依赖 `casbin.EnforcerProvider`。未来 ABAC 若有真实业务需求，将作为 `security/abac` 平行独立插件及相应传输适配器设计；本次不实现占位目录或 API。

## Quickstart

要求 Go 1.25 或更高版本。从仓库根目录运行：

```bash
go run ./examples/quickstart doctor --config examples/quickstart/application.yml
go run ./examples/quickstart --config examples/quickstart/application.yml
```

完整的组合说明、配置入口和验证请求见 [Quickstart](docs/quickstart.md)。Web 运行契约见 [Web package documentation](https://pkg.go.dev/github.com/xbcio/xbc/transport/web)，认证、持久化、消息和多副本部署示例见 [Deployment recipes](docs/recipes.md)。

## 开发与验证

```bash
make fmt        # 格式化全仓 Go 文件
make check      # gofmt 检查 + 每个 workspace module 的 go vet/go test
make test-race  # 每个 workspace module 的 race test
```

不要用根目录的 `go test ./...` 代替全仓验证：Go 的递归 package pattern 不进入嵌套 module。Makefile 与 CI 从 `go.work` 动态发现 module，架构测试同时扫描磁盘，任何遗漏都会失败。
