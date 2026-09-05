# XBC 插件扩展任务

状态：已完成  
日期：2026-08-30

## 目标

在不污染 core module、没有隐式注册副作用、没有占位依赖版本的前提下，补齐设计文档中除 gRPC 外的插件，并增加常用 Web、集群治理及可快速复用的业务插件。所有插件都必须具备真实配置、生命周期、Definition/autoload、失败路径和并发测试，禁止只创建目录或空壳 API。

## P0：设计范围

- [x] Web 路由元数据：`Name`、`Public`、`Perm`、`Idempotent`
- [x] 插件触发统一 shutdown 的窄 capability
- [x] 调查 SAS 中可复用能力，排除内部业务/CMDB/JIRA/ULP 等专有集成
- [x] 更新 package-layout 权威设计中的 module 现状与新增边界

## SAS 调研结论

调研对象：`/Users/10097292/Desktop/work/code/asop/sas`（只读调查，不复制内部业务代码）。

- 采纳为通用插件：Asynq、API Key/Bearer、对象存储、Elasticsearch、Kafka、Prometheus Metrics、OpenTelemetry、pprof、分布式 Cron/运行协调。
- 采纳原则：能力可独立配置和关闭、生命周期可由 XBC 托管、契约可脱离 SAS 领域模型，并且不会把可选重依赖带入 core。
- 明确排除：SAS 内部领域服务、JIRA、CMDB、ULP/SPC 等企业专有集成；这些能力缺少跨项目稳定契约，不应进入通用框架。
- 依赖基线参考 SAS 已验证版本，但最终以每个独立 module 的 `go.mod` 与兼容测试为准；禁止通过本地 `replace` 或占位版本制造可发布假象。

## P1：基础插件

- [x] `transport/web/cors`
- [x] `transport/web/integrations/jwt`
- [x] `integrations/gorm`
- [x] `integrations/redis`
- [x] `transport/web/ratelimit`
- [x] `integrations/cron`（单机 + Redis 租约锁分布式模式，owner token、Lua 原子续租/释放、故障接管）
- [x] `transport/web/health`
- [x] `transport/web/integrations/swagger`
- [x] `transport/web/integrations/casbin`

## P1：Web 常用插件

- [x] `transport/web/recovery`
- [x] `transport/web/requestid`
- [x] `transport/web/accesslog`
- [x] `transport/web/securityheaders`
- [x] `transport/web/gzip`
- [x] `transport/web/timeout`

## P1：集群与进程治理

- [x] `integrations/raft`（HashiCorp Raft；真实 transport/store/FSM/snapshot/bootstrap/join/leader 状态）
- [x] `transport/web/gracefulshutdown`（程序化 controller + 安全的 drain/shutdown endpoint；复用 core 统一逆序关机）

## P2：业务组合插件

- [x] `transport/web/apikey`：Header/Bearer、常量时间校验、哈希存储、可插拔 key repository、principal 注入、按路由公开属性放行
- [x] `transport/web/integrations/idempotency`：仅作用于 `.Idempotent()` 路由，Redis 原子占位/完成缓存/租约恢复，重复请求安全重放
- [x] `transport/web/auditlog`：结合 request ID、路由、principal、状态码、耗时；可插拔 sink，默认结构化日志，异步有界队列与可靠关闭
- [x] `transport/web/integrations/session`：高熵 opaque bearer Cookie、内存/Redis 服务端 session store、原子轮换/撤销、固定攻击防护，并发布统一 `web.Principal`
- [x] `transport/web/tenant`：从可信 Principal/header/host 解析租户，校验成员关系并发布不可伪造的请求租户上下文
- [x] `integrations/outbox`：复用命名 GORM，在业务事务内写入事件；租约 claim、重试退避、故障接管与优雅 drain，publisher 可注入
- [x] `integrations/webhook`：有界异步投递、HMAC 签名、退避重试、响应大小限制、私网/回环 SSRF 防护与可靠关闭
- [x] `integrations/asynq`：依赖 Redis 的分布式任务 client/worker，队列权重、重试/超时、优雅停止、可注入 handler 注册
- [x] `integrations/objectstorage`：统一 Store 契约，local 与 S3 backend，流式上传/下载、大小限制、路径穿越防护、生命周期关闭

## P2：第二批通用集成

先实现独立 module，默认不进入 Quickstart；完成前需确认公共 API 不泄漏厂商细节。

- [x] `integrations/kafka`：producer/consumer group、backpressure、重试、优雅 rebalance/关闭
- [x] `integrations/elasticsearch`：命名客户端、health、bulk worker、关闭 flush
- [x] `transport/web/integrations/metrics`：Prometheus registry 与 Web/runtime 指标，避免全局 default registry 污染
- [x] `transport/web/integrations/tracing`：OpenTelemetry SDK/provider、OTLP exporter、W3C propagation、关闭 flush
- [x] `transport/web/pprof`：默认关闭或仅 loopback，禁止无保护公网暴露

## 明确不做

- gRPC module、占位目录或占位 API
- SAS 的内部领域服务及企业专有集成
- 在普通实现包 `init()` 中自动注册；只有叶子插件 `autoload` 与显式组合入口 `transport/web/prelude` 可以 `catalog.Declare`
- core `go.mod` 引入可选重依赖
- 子 module 中本地 `replace`、`v0.0.0`、伪版本或未发布占位版本

## 全局验收

- [x] 每个实现包提供 `Key`、`Config`、`Plugin`（Web 为 `Server`）、`New`、`Definition()` 和显式 `autoload`
- [x] 单元测试覆盖配置校验、生命周期回滚/关闭、并发和安全失败路径
- [x] `go.work`、CI module 缓存、架构守卫、README、配置文档和 Quickstart 同步更新
- [x] `make fmt`
- [x] `make check`
- [x] `make test-race`
- [x] Quickstart 启动、HTTP smoke test、统一 graceful shutdown

## 完成记录

- 2026-08-30：完成 32 个独立可选 module；Web 运行栈位于 `transport/web`，直接依赖它的插件归入 `transport/web/plugins/<name>`，协议无关集成保留在 `plugins/{data,messaging,cluster}/<name>`；未创建 gRPC module、占位目录或占位 API。
- 完整验收：`make fmt`、`make check`、`make test-race` 全部通过；Quickstart doctor、HTTP/health/OpenAPI smoke 和 SIGTERM graceful shutdown 通过。
- 发布边界：workspace module 完整，普通实现包无 `catalog.Declare`，子 module 无 local `replace`、XBC `v0.0.0` 或 XBC pseudo-version。

## 后续补充：doc.go 使用示例

状态：已完成（2026-08-30）

- [x] 32 个插件根包的 `doc.go` 均包含标准 `# Usage` 小节
- [x] 每个 Usage 小节至少提供一个基于真实公共 API 的 Go 代码块，而非仅描述能力
- [x] 架构测试递归发现所有插件 module，并阻止缺少 `doc.go`、Usage 标题或代码块
- [x] `make fmt`
- [x] `make check`
- [x] `make test-race`
- [x] `git diff --check`

完成记录：32/32 个插件已补齐 Usage；示例覆盖 autoload、依赖获取、Provider/Contributor、客户端调用及安全配置等实际接入路径，并通过全 workspace 检查和竞态测试。
