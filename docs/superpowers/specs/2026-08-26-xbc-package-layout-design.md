# xbc — 面向多运行栈的包布局设计

- **初始日期**：2026-08-26
- **合并修订**：2026-09-02
- **仓库**：`git@github.com:xbcio/xbc.git`
- **Core module**：`github.com/xbcio/xbc`
- **状态**：现行权威设计
- **定位**：通过 Plugin 构建 Web、任务型、分布式及未来其他运行栈

本文定义 package/module owner、依赖方向、应用组合边界和发布约束。Plugin Definition、typed Inputs/contracts、Plan/Construct 和生命周期语义以[插件框架设计](./2026-08-23-xbc-plugin-framework-design.md)为准；两份文档共同描述唯一现行模型。

## 0. 最终结论

`xbc` 不是以 Gin 为内核的 Web 框架，而是与传输协议无关的插件应用运行时：

- 根包 `xbc` 是应用启动与嵌入的窄门面；
- `plugin` 提供 canonical Definition、typed Inputs/contracts、Bundle 和生命周期 SPI；
- `internal/pluginmodel` 保存 erased generic metadata，不对外构成 API；
- `internal/assembly` 负责 Plan 与 Construct，`internal/runtime` 负责 Execute、任务、流量门与关机；
- `transport/web` 是可选 Gin-backed module，Core 不依赖它；
- Web 内建能力由 `transport/web/<name>` package 拥有，随 Web module 发布；
- Web 重型集成与协议无关外部集成按真实依赖拆独立 module；
- 应用首选显式 `Bundle()` + `xbc.WithBundles(...)`，不依赖 import 时序发现能力；
- `transport/web/prelude` 是无副作用静态组合，不在 `init()` 中注册；
- 只有明确选择该风格的 executable 才 blank-import 叶子 `.../autoload`；autoload 只是可选进程适配器；
- 配置控制已组合 Definition 的 activation 与实例，不负责发现未组合 package。

主入口：

```go
package main

import (
    "context"
    "os"

    "github.com/xbcio/xbc"
    "github.com/xbcio/xbc/transport/web/biz"
    "github.com/xbcio/xbc/transport/web/prelude"

    "example.com/orders/internal/orders"
)

func run(ctx context.Context) (int, error) {
    app, err := xbc.New(xbc.WithBundles(
        prelude.Bundle(),
        biz.Bundle(),
        orders.Bundle(),
    ))
    if err != nil {
        return 1, err
    }
    return app.Execute(ctx, os.Args[1:])
}
```

这里的 import 是普通 Go 编译期依赖，Bundle 调用是可审查的 composition root。普通 package import、`Definition()`、`Bundle()` 与 Prelude 都不修改进程状态，也不创建连接、goroutine 或 listener。

当前 `go.work` 联结 20 个 module：Core、examples、`transport/web`、7 个 Web integration 和 10 个协议无关 integration。module 数量不是目标；只有真实依赖隔离、独立版本或发布节奏才值得拆 module。gRPC 当前未实现，因此不创建空 `transport/grpc`、空 manifest 或占位接口。

## 1. 取舍原则

### 1.1 从成熟项目借边界，不复制目录模板

| 来源 | 借鉴原则 | XBC 落点 |
|---|---|---|
| Go 工具链与常见仓库 | `internal` 隐藏实现，public package 按职责命名 | `internal/runtime`、`internal/assembly`、`internal/pluginmodel` |
| Kubernetes | owner 清晰、入口薄、依赖单向、架构规则可执行 | 六符号根门面、领域 owner、`tests/architecture` |
| Hertz | runtime、协议实现、工具与可选集成分离 | Core 与 `transport/web` 分 module |
| Spring / Spring Boot | 条件装配、稳定契约、可选重依赖隔离 | immutable Definition + explicit Bundle + typed contract |

Spring 类比仅用于解释职责：Definition 近似 BeanDefinition，Plugin instance 近似 bean，Bundle 近似显式 configuration/import set。XBC 的装配由 canonical Definition、typed Input token 和 explicit Bundle 驱动；公开模型保持静态、可检查，不引入 classpath scanning、annotation、proxy、通用 application context 或任意 scope。

### 1.2 package 是职责边界

一个 package 应回答“谁拥有这项能力”。不要创建无法验证 owner 的 `common`、`utils`、`pkg`、`core`、`engine`；不要按启动阶段机械拆 package。时间顺序属于 runtime pipeline，协议和业务能力属于各自领域 owner。

`transport/` 只是 repository namespace，不是可 import 的统一 transport 抽象。只有两个以上真实实现证明存在稳定公共模型时，才抽取跨协议 contract。

### 1.3 一个概念只有一个 canonical owner

- application facade：根 `xbc`；
- Plugin declaration/composition：`plugin`；
- generic-erased metadata：`internal/pluginmodel`；
- Plan/Construct：`internal/assembly`；
- Execute/lifecycle/tasks/process：`internal/runtime`；
- configuration：`config`；
- logging：`log`；
- deterministic generic ordering：`plugin/ordering`；
- HTTP types and execution order：`transport/web`；
- one integration：其实现 package/module。

每个概念只保留一个 owner，alias 或 wrapper 不得形成平行入口。本仓库仍在首个正式 tag 前，架构清晰度优先。

### 1.4 module 只解决真实隔离问题

同一 owner、同一发布节奏且依赖轻的 Web built-in 放在 `transport/web` module 内。引入数据库 driver、Redis/Kafka client、对象存储 SDK、OpenTelemetry exporter 等明显依赖重量时，才拆独立 module。

子 module manifest 只声明真实上游版本，不写本地 `replace`、`v0.0.0` 或伪造 pseudo-version。仓库内未发布依赖由 `go.work` 解析；发布时按依赖拓扑打真实 tag。

## 2. 当前布局

```text
xbc/
├── xbc.go                         # 六符号应用门面
├── plugin/                        # public protocol-neutral Plugin SPI
│   └── ordering/                  # stdlib-only deterministic ordering
├── config/                        # Environment/source/strict bind
├── log/                           # structured logging facade
├── security/                      # protocol-neutral security marker/contracts
├── internal/
│   ├── pluginmodel/               # opaque handle/token erased representation
│   ├── assembly/                  # BuildPlan, Construct, unwind primitives
│   ├── runtime/                   # Execute, lifecycle, tasks, gate, shutdown
│   ├── autoload/                  # optional process adapter, private
│   └── cli/                       # argument parsing
├── transport/
│   └── web/                       # independent Gin-backed module
│       ├── prelude/               # side-effect-free production baseline Bundle
│       ├── autoload/              # optional leaf adapter for web.Bundle()
│       ├── <built-in>/            # Web-owned lightweight Plugins
│       └── integrations/<name>/   # independent heavy Web integration modules
├── integrations/<name>/           # independent protocol-neutral modules
├── examples/                      # external-consumer-style module
├── tests/architecture/            # dependency/API/composition guards
├── tests/integration/             # public facade behavior
└── scripts/                       # workspace and migration tooling
```

### 2.1 根门面为什么只有六个符号

根包公开：

| 符号 | 责任 |
|---|---|
| `App` | 一个 single-use application handle |
| `Option` | `New` 的宿主选项类型 |
| `New` | 保存组合选择，不执行 Plan 或 Construct |
| `WithBundles` | 显式交付 side-effect-free Bundles |
| `Run` | optional autoload 风格的进程入口 |
| `(*App).Execute` | 在调用方 context 下执行一次完整 pipeline |

根包不重导出 Web 类型、不接受 live Plugin、不公开 internal planner/runtime，也不提供第二套注册方法。显式 composition 使用 `New(WithBundles(...))`；`Run` 只是可选进程 convenience，不是更高优先级的主模型。

### 2.2 实现 package 的统一形态

一个可复用实现 package 通常拥有：

```go
const Key plugin.Key = "orders"

var definition = plugin.DefineConfigured(/* ... */)
var bundle = plugin.BundleOf(definition)

func Definition() plugin.Definition { return definition }
func Bundle() plugin.Bundle         { return bundle }
```

规则：

1. `plugin.Define`、`DefineConfigured` 或 `DefinePlanned` 只在 package scope 求值一次；
2. `Definition()` 返回该 canonical identifier，不临时创建 handle；
3. `Bundle()` 只组合 canonical handles/Bundles，不读取 config、不构造值；
4. factory 返回 concrete primary value；额外 interface 用 typed contracts 导出；
5. factory 的所有依赖由 typed Input token 声明和读取；
6. package 可提供直接 `New` 供普通 Go 使用，但 XBC runtime ownership 只来自 Definition；
7. import 实现 package 不改变任何 process-wide composition。

一个 package 可定义多个 Plugin。当两个能力需要独立 Identity、lifecycle、contract 或 domain order 时，应有独立 Definition；不要为了“每 package 一个 Plugin”把它们塞进同一 live object。

### 2.3 Bundle、Prelude 与 autoload

#### Bundle

Bundle 是 composition data，不是 runtime object。叶子 package 通常 `BundleOf(definition)`；聚合 package 使用 `CombineBundles`。Bundle 无 key、activation、config、dependency 或 lifecycle。

同一个 canonical Definition 可从多个 Bundle 到达，Plan 按 declaration identity 去重。相同 key 的不同 handle 不是可覆盖关系，必须报 source-aware collision。

#### Prelude

Prelude 是 curated Bundle：

```go
var bundle = plugin.CombineBundles(
    web.Bundle(),
    recovery.Bundle(),
    requestid.Bundle(),
    accesslog.Bundle(),
    securityheaders.Bundle(),
    gzip.Bundle(),
    timeout.Bundle(),
    health.Bundle(),
)

func Bundle() plugin.Bundle { return bundle }
```

Prelude 必须 side-effect-free：没有 `init()`，不 import 私有 autoload adapter，不 clone Definition，不改 activation，也不创建资源。它只是降低 composition root 的重复，不隐藏应用策略。CORS、业务 envelope、认证授权、文档和 telemetry 等选择留给应用显式加入。

#### Optional leaf autoload

只有 deliberate blank-import executable 需要 autoload：

```go
// package integrations/redis/autoload
func init() {
    autoload.Declare(redis.Bundle())
}
```

这里的 `autoload` 是 Core 私有 adapter；叶子 package 只能把 parent package 的 canonical `Bundle()` 原样交给它。它不得创建 Definition、复制 Bundle、读取配置或创建 live resource。

普通 library、实现 package 与 Prelude 都不得 import `internal/autoload`。显式应用不要 blank import；直接把 parent `Bundle()` 传给 `WithBundles`。若调用 `WithBundles`，该 App 只使用显式交付的组合，不暗中混入进程 adapter 的内容。

### 2.4 配置不承担发现职责

Definition 默认拥有 `plugins.<key>`；必要时通过 `ConfigPath` 声明 owner 明确的 canonical section，例如 Web 的 `web`。配置值控制 activation、实例和参数，但不会加载未组合 package。

存在一个 plugin section 而 composition 中没有对应 Definition 是 orphan configuration，必须在 Plan 失败。调用方必须修正 Bundle 或删除配置；配置不会触发动态 import 或 package 扫描。

多实例 Definition 将 section 的直接子项展开为 stable instance names；dependency consumer 通过 `RefToInstance` 或 `Collect` 明确选择。实例名进入 Identity、日志、图和报告。

## 3. 每个 package/module 的职责

| package/module | 负责 | 明确不负责 |
|---|---|---|
| 根 `xbc` | 六符号 facade；委托私有 runtime | 保存 graph、实现 lifecycle、协议类型、Plugin implementation |
| `plugin` | Definition/Options/ConfigSpec/Plan、Inputs/contracts、Bundle、Identity、lifecycle Context/capabilities | process-global composition state、container algorithm、协议类型 |
| `plugin/ordering` | hard/soft edge 和确定性 sort | Plugin value、配置、lifecycle、反射；非 stdlib 依赖 |
| `internal/pluginmodel` | opaque Definition、Input token、Bundle 的 erased metadata | public API、配置加载、resource construction |
| `internal/assembly` | Bundle flatten、config expansion、BuildPlan、contract/input resolution、Construct、owned unwind | process signal、optional autoload choice、具体 transport |
| `internal/runtime` | App state、bootstrap、Execute、migration/start/gate、managed tasks、bounded shutdown、process adaptation | protocol-specific server、integration implementation |
| `internal/autoload` | 收集 optional leaf Bundles 供无显式组合的进程入口消费 | public composition API、Definition creation、resource acquisition |
| `internal/cli` | 纯参数解析和 command data | config load、Plan、Construct、logging init |
| `config` | source、Environment、strict typed bind、default/validation | Plugin selection、lifecycle、日志初始化 |
| `log` | structured logger/trace facade 与默认 binding | application composition、协议执行 |
| `security` | protocol-neutral security markers/contracts | HTTP routing、credential transport |
| `transport/web` | Server、Router、route snapshot、Principal、middleware/order contracts、Web error model | Core orchestration、外部系统 integration、未来 transport |
| `transport/web/prelude` | curated side-effect-free Web baseline Bundle | `init` registration、应用策略、live resource |
| `transport/web/<name>` | 一项 Web-owned lightweight capability | Core internals、其他 transport contract |
| `transport/web/integrations/<name>` | 一项依赖 Web 与重型/第三方 SDK 的 integration | Core internal state、其他 transport contract |
| `integrations/<name>` | 一项 protocol-neutral integration 与其 owned resource lifecycle | Gin/HTTP 入站类型、container algorithm |
| `examples` | 模拟外部 consumer，展示 explicit composition | 被 Core/Web 反向 import、直接依赖 private runtime |

`plugin` 可以依赖它必须公开的稳定门面和私有 representation，但不能依赖根 `xbc`、transport 或 integration。Core internal 可以依赖 `plugin`/`config`/`log`，但不能反向 import optional module。Web 与 integrations 向下依赖 Core contract，Core 不向上认识它们。

## 4. Plan / Construct 的 private owner

### 4.1 为什么都在 `internal/assembly`

Plan 与 Construct 是 App 内部事务，不是插件作者可替换的容器 SPI。公开它们会暴露 erased metadata、mutable ownership set 和 rollback sequencing，使 runtime boundary 无法演进。因此外部调用方只交 Bundle 并调用 `App.Execute`。

`internal/assembly` 内部仍把两个阶段明确分开：

```text
BuildPlan: metadata/config only, no factory invocation
Construct: factory + Init + ownership transaction
```

Plan 冻结 Definition、instance、prepared config、Input binding、contract index、lifecycle descriptor 和 graph。Construct 只消费冻结 Plan，不再次发现 capability、不重解依赖、不读取新的配置。

### 4.2 唯一 identity/contract index

每个 enabled instance 在 Plan 中产生一项 identity record；primary concrete type自动索引，额外 interface contracts 来自 Definition。所有 `Ref`/`One`/`Optional`/`Many` query 都解析到这个 index。

所有 publication 都来自 Definition 的静态声明，并进入同一个 contract index。Web middleware、route contributor、health contributor、job source 等都是该 index 上的普通 interface。

### 4.3 resource ownership

factory 成功返回 non-nil primary value时所有权转移给 Construct。Construct 在 Init 前把 instance 放进 rollback set；因此 Init 失败仍调用 Stop。factory 返回 error 时尚未转移的局部资源由 factory 自己清理。

失败 unwind 与正常 shutdown 都逆 graph order，并收集全部 error/panic。runtime 为整体 shutdown 提供一个共享 deadline；单个卡住的 Stop 不得拖死整个进程。

## 5. 协议无关 Execute 生命周期

公开 `App.Execute(ctx, args)` 与 `Run` 最终进入同一 pipeline：

```text
parse command
→ load Environment and runtime settings
→ BuildPlan
→ doctor: report and return
→ Construct + Init
→ optional Migrate
→ Start (open per-Plugin task admission only for this hook)
→ OpenTraffic behind closed global gate
→ release traffic gate once
→ wait for stop request
→ reverse Stop → cancel → join
```

### 5.1 process signal 只属于 `Run`

嵌入式 `Execute` 由调用方 context 控制，不自行订阅 OS signal。包级 `Run` 是 process adapter，可以安装 SIGINT/SIGTERM watcher。signal 必须在任何 Plugin Init 前可见，启动中收到 stop request 时也要进入相同 rollback path。

### 5.2 readiness 与 traffic gate

Core 不按 key 识别 `web` 或未来 transport。任何 primary value只要实现 `plugin.TrafficOpener`，就参与协议无关 preparation。Start-owned serving task等待 `ctx.TrafficGate()`；所有 preparation 成功后 runtime 一次性 close gate。

`OpenTraffic` 可以完成会失败的最终准备，但不得自行开放 ingress。若任一 participant 失败，gate 永不开放，已构造资源逆序 unwind。

### 5.3 managed tasks

`ctx.Go`/`GoCritical` 只能在 owner 的 Start hook 期间提交。runtime 在 hook 返回后关闭该 owner 的准入，shutdown 时先关闭全局准入，再逆序处理 Plugin：

```text
Stop primary value
→ cancel this Plugin's managed task context
→ join this Plugin's tasks
```

这个顺序让 Stop 有机会先关闭 listener/consumer 并阻止新工作，再取消后台 loop。`GoCritical` 的 panic 或未请求关机的返回会触发应用级 stop；它不是 detached goroutine。

### 5.4 shared shutdown budget

`xbc.shutdown_timeout` 是整个进程从 stop 到退出的预算。所有 Stop、task join 和剩余 cleanup 共享一个 deadline，不按 Plugin 重置。runtime 在受控 goroutine 中调用 Stop 并 select deadline；实现即使忽略 context 也不能永久阻塞退出。

### 5.5 liveness

一个没有 Runner、managed task 或其他存活能力的普通 run 必须明确失败；`doctor` 和 `migrate` 是有界命令例外。永久 `select {}` 不是 liveness 实现。

## 6. Web owner 与扩展边界

### 6.1 Web contract 不进入 Core

Gin `Engine`、Router、Middleware、route metadata、Principal、Problem Detail 和 auth policy 都归 `transport/web`。Core 不需要知道请求、路由或 middleware phase。

Web server Definition 使用 typed `Collect` 输入收集 middleware、route contributor 与 route listener。每个 entry 保留 Plugin Identity，既用于确定排序，也用于错误和启动报告。

### 6.2 domain order 不污染 dependency graph

`plugin/ordering` 提供 stdlib-only hard/soft graph primitive；Web 用自己的 `OrderRef` 赋予 key/instance、required/preferred 和 phase 语义。

- constructor dependency：typed Input edge；
- middleware execution order：Web Order edge；
- security precondition：required Web Order edge；
- `security.RequiresPrincipal`：Web sorter 自动 pin 到 canonical authentication middleware 之后。

这几类约束可同时存在，但不能相互替代。Bundle declaration order不作为 domain tie-break；同 phase 无 edge 时按 normalized Identity 稳定排序。

### 6.3 route freeze 与 listener

route contributor 在 Start 中构建 table，Web server 完成校验后冻结 snapshot；listener 在 traffic gate 开放前恰好观察一次完整 table。运行中不允许添加 route 或修改 auth policy。

route policy 的 absent/public/explicit schemes 是三个状态。零值不能意外公开 endpoint。Swagger、auth 与审计从同一 frozen snapshot 读取，不维护平行 route directory。

### 6.4 health owner

健康探针不是 Core 固定协议。`transport/web/health` 拥有 Web probe route 与 contributor contract；未来其他 transport 可实现自己的暴露方式。贡献者通过 typed contract 被 health Plugin 收集，不需要 Core 增加 capability method。

## 7. Integration 布局

### 7.1 Web built-in

与 Web 生命周期紧耦合、依赖轻、同版本发布的能力位于 `transport/web/<name>`，例如 recovery、requestid、accesslog、CORS、security headers、gzip、timeout、rate limit、API key、tenant、audit、health、pprof 和 graceful shutdown。

它们是独立 Plugin Definitions/Bundles，不是 server 内的 capability scan。需要独立中间件顺序的能力必须有独立 Identity。

### 7.2 Web heavy integration

JWT/session/Casbin/idempotency/Swagger/metrics/tracing 等依赖 Web contract 且带第三方或重型 SDK，分别位于 `transport/web/integrations/<name>` 独立 module。删除一个 module 不影响 Core 或其他 integration 编译。

### 7.3 Protocol-neutral integration

GORM、Redis、对象存储、Elasticsearch、Kafka、Asynq、Outbox、Webhook、Cron、Raft 位于 `integrations/<name>`。它们不能 import Gin 或 Web route type；若需要把能力暴露到 HTTP，由应用或 Web-owned adapter 通过 narrow contract 组合。

组合 integration 使用 typed Inputs 依赖基础能力。例如 consumer 根据配置选择 Redis instance 时使用 `DefinePlanned` + `RefToInstance`；收集多个 publisher/job source 时使用 `Collect`。不得在 lifecycle 中临时查全局对象。

### 7.4 Application Plugins

业务代码放在应用自己的 package。它可以定义 route contributor、service、repository adapter 或 worker 等一个或多个 Plugin，并通过自己的 `Bundle()` 汇总。应用 Bundle 与框架 Bundle 使用完全相同的模型，没有“业务组件”第二套容器。

## 8. 依赖方向

允许的主要方向：

```text
plugin/ordering ──────────────────────► stdlib
config ───────────────────────────────► stdlib + narrow config deps
log ──────────────────────────────────► logging/trace deps
plugin ───────────────────────────────► internal/pluginmodel, log
internal/pluginmodel ─────────────────► stdlib, log
internal/assembly ────────────────────► plugin, config, log, internal/pluginmodel
internal/runtime ─────────────────────► internal/assembly/autoload/cli,
                                        plugin, config, log
xbc facade ───────────────────────────► internal/runtime, plugin
transport/web ────────────────────────► core public packages + Gin
transport/web/<built-in> ─────────────► transport/web + core public packages
web integration ──────────────────────► transport/web + core public packages
protocol-neutral integration ─────────► core public packages + its SDK
examples/application ─────────────────► xbc facade + selected public modules
```

禁止：

1. Core import 任一 transport 或 integration；
2. `plugin` import 根 facade、transport 或 implementation；
3. public package import private runtime/assembly；
4. implementation package import `internal/autoload`；
5. Prelude import `internal/autoload` 或拥有 `init`；
6. examples 直接 import Core private implementation；
7. protocol-neutral integration import Gin/Web；
8. 可选 module 反向成为 Core 必需依赖。

Go 的 `internal` 可见性按 import path 树判断，examples 路径可能仍位于允许范围，因此“examples 不直接 import private Core”需要架构测试检查 direct imports，而不是只依赖编译器。

## 9. 公开 API 与架构守卫

### 9.1 API owner 表

| API | owner |
|---|---|
| `App` / `Option` / `New` / `WithBundles` / `Run` / `App.Execute` | 根 `xbc` |
| Definition、Options、ConfigSpec、Plan、Inputs、contracts、Bundle、Identity、lifecycle | `plugin` |
| generic hard/soft ordering | `plugin/ordering` |
| Environment/source/bind | `config` |
| Logger/trace facade | `log` |
| security markers | `security` |
| Server/Router/Middleware/OrderRef/route/auth/error DTO | `transport/web` |
| one optional capability | its implementation package/module |

`internal/*` 中为 package 协作导出的 Go identifier 不构成外部 API 稳定性承诺。若外部使用场景无法由 facade 满足，应先设计最窄 public contract，不能公开整个 assembly/runtime。

### 9.2 必须执行的 guards

架构测试至少固定：

1. 根 facade 只有六个预期符号，`WithBundles` 是唯一 composition Option；
2. Core 依赖闭包不含 Gin、transport 或 integration；
3. `plugin/ordering` 只依赖 stdlib；
4. 实现 package 的 `Definition()` 返回 package-level canonical `plugin.Define*` handle；
5. 每个实现提供 side-effect-free `Bundle()`；
6. 普通实现和 Prelude 无 autoload import，Prelude 无 `init`；
7. 叶子 autoload 只声明 parent `Bundle()`；
8. public package 与 API surface 只包含本设计列出的 owner；
9. every `go.mod` 同时被 `go.work`、Makefile 和 CI 覆盖；
10. examples 只通过 root facade 和 public owner 组合；
11. protocol-neutral integrations 不依赖 Web；
12. repository inventory 对 workspace、test、doc、example 保持零违规。

这些 guard 是设计的一部分。刻意改变设计时必须同步更新 guard、caller 和文档；不能为通过迁移而 skip、删除或降低检查范围。

### 9.3 Validation

从仓库根运行：

```sh
make fmt
make check
make test-race
go run ./examples/quickstart --config examples/quickstart/application.yml
```

不要把根级 `go test ./...` 当全仓测试；Go package pattern 不进入嵌套 module。Makefile 通过 `go.work` 逐 module 执行。

## 10. 版本与发布

`go.work` 是仓库开发基础设施，不会随 module 发布成为 consumer 依赖。独立 module 的 manifest 必须能在真实上游 tag 存在后用 `GOWORK=off` 解析和测试。

发布顺序遵循依赖拓扑：

```text
Core
→ transport/web 与 protocol-neutral基础 integrations
→ 依赖它们的 Web/组合 integrations
→ examples（仅验证，不作为库依赖源）
```

Web built-in 是 `transport/web` module 的 package，不拥有独立 tag。不要通过 filesystem replace、占位 require 或 synthetic pseudo-version 伪造发布成功。

## 11. 明确否决的方案

| 方案 | 否决原因 |
|---|---|
| 根包持有 Router/Middleware/http.Server | Core 被永久绑定到 Web |
| 默认组合中隐式加入 Web 或 management | 对任务型、未来其他 transport 没有统一正确默认值 |
| 普通 import 或 Prelude 在 `init` 修改 composition | 隐藏依赖选择，测试与多 App 隔离困难 |
| autoload 成为唯一入口 | library/embedding 无法获得显式、可测试组合 |
| 注册已构造 live value | 多实例、构造参数与 ownership 无法统一 |
| 通用 resolver 或 application context | factory dependency 不可静态验证，重新引入 Service Locator |
| 反射扫描 field/method/capability | graph 与 lifecycle 只能在构造后发现，无法可靠 doctor/rollback |
| 一个 package 强制只有一个 Plugin | 独立 identity/lifecycle/order 被错误合并 |
| Bundle 本身成为 Plugin | 静态组合与 runtime ownership 混淆 |
| Core 按 key 识别 Web/gRPC readiness | 新 transport 必须修改 Core；TrafficOpener 已表达通用语义 |
| detached goroutine 跑 lifecycle hook | 无法安全抢占，可能与 unwind 并发 |
| 每个 Plugin 独占 shutdown timeout | 总退出时间随 Plugin 数量线性膨胀 |
| 只把 context 传给 Stop 就声称有界 | Stop 可忽略 context；runtime 必须主动 select shared deadline |
| 先 cancel 再无保护地 Wait task group | submission 与 Wait 存在竞态；必须先关闭准入 |
| 预建未实现 transport/module | 占位 API 没有真实 consumer 验证 |
| 把所有可选实现放入 Core module | 下载、升级、安全扫描与发布全部耦合 |

最终判断标准：看到 import path 就能判断 owner；看到 composition root 就能判断应用选择；任何运行时 value 都有 canonical Definition 与 Identity；删除任一 optional module 后 Core 仍独立编译，且普通 package import 永不启动资源或改变应用组合。
