# XBC 插件 owner 布局迁移任务

状态：已完成  
日期：2026-08-30

## 目标

让目录直接表达依赖和所有权：Web 运行栈固定由 `transport/web` 持有，所有直接依赖 Web/Gin capability 的插件位于 `transport/web/plugins/<name>`；只有协议无关的基础设施集成保留在 `plugins/<category>/<name>`。每个运行栈和具体插件继续作为独立 Go module，namespace 目录不包含 `go.mod` 或 Go package。

这是开发期有意的 breaking change。更新 module path、import、workspace、文档和架构守卫，不保留旧 import path 兼容壳，也不创建 gRPC 占位。

## 目标布局

- `transport/web`：Gin-backed Web 运行栈
- `transport/web/plugins/{accesslog,apikey,auditlog,casbin,cors,gracefulshutdown,gzip,health,idempotency,jwt,metrics,pprof,ratelimit,recovery,requestid,securityheaders,session,swagger,tenant,timeout,tracing}`
- `plugins/data/{elasticsearch,gorm,objectstorage,redis}`
- `plugins/messaging/{asynq,kafka,outbox,webhook}`
- `plugins/cluster/{cron,raft}`

未来若真正实现 gRPC，运行栈 owner 为 `transport/grpc`；在有真实契约、实现和测试前不创建该目录或 module。

## 实施清单

### P0：module 与源码迁移

- [x] 将 Web 运行栈从旧插件分类迁至 `transport/web`
- [x] 将 21 个 Web-coupled 插件平铺迁至 `transport/web/plugins/<name>`
- [x] 保留 data、messaging、cluster 三类协议无关插件的顶层 owner
- [x] 更新 module declaration、源码 import、autoload、测试和示例
- [x] 删除迁空的 `plugins/{transport,web,auth,governance,observability,management}`

### P1：仓库集成

- [x] 更新 `go.work`，覆盖 core、examples 和 32 个可选 module
- [x] 更新 Quickstart、配置、CI 和全部推荐 import
- [x] 让插件架构守卫同时发现 `transport/web/plugins/*` 与 `plugins/**`
- [x] 增加 Web owner、namespace、manifest、依赖方向和 retired path 守卫
- [x] 更新根 README、插件 README、配置文档和 package-layout 权威设计
- [x] 更新上一阶段扩展任务中的最终路径说明

## 不变量

- `plugin/` 继续只拥有 Context、Definition、Catalog、ordering 等 core SPI
- `transport/web` 与每个具体插件各自拥有独立 `go.mod`
- 普通实现包不得调用 `catalog.Declare`；叶子插件只有 `autoload` 可显式注册，`transport/web/prelude` 是唯一的 Web 组合注册入口
- core `go.mod` 和生产依赖闭包不引入任何 transport 或具体插件 module
- 子 module 不得使用本地 `replace`、XBC `v0.0.0` 或伪版本
- 不添加 gRPC module、目录或占位 API
- 不创建旧路径兼容包

## 验收

- [x] `make fmt`
- [x] `make check`
- [x] `make test-race`
- [x] Quickstart doctor、HTTP/health/OpenAPI smoke 与 SIGTERM graceful shutdown
- [x] `git diff --check`
- [x] 全仓不存在旧插件 import path 引用
- [x] 所有仓库 module 均被 `go.work` 覆盖

## 完成记录

- 32 个可选 module 已按 owner 归位：1 个 Web runtime、21 个 Web-owned 插件和 10 个协议无关集成。
- `go.work` 共覆盖 core、examples 与上述 32 个 module；架构测试同时检查 workspace 完整性、namespace 和依赖边界。
- 旧插件 import path 已清零，旧 Web 分类目录已删除且由 retired path 守卫防止复活。
- 已通过全 workspace 格式、静态检查、测试、竞态测试和 Quickstart 行为验收。
