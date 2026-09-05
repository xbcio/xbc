# xbc — 插件化 Go 应用运行时设计

- **初始日期**：2026-08-23
- **合并修订**：2026-09-02
- **仓库**：`git@github.com:xbcio/xbc.git`
- **Module**：`github.com/xbcio/xbc`
- **状态**：现行权威设计

本文定义 XBC 的插件语义、公开组合 API、装配事务、生命周期和 Web 扩展模型。package/module 的 owner 与依赖边界以[包布局设计](./2026-08-26-xbc-package-layout-design.md)为准；两份文档共同描述唯一现行实现。

## 1. 定位与目标

XBC 是与传输协议无关的 Go 插件应用运行时。Core 负责显式组合、严格配置、依赖图、资源所有权、生命周期、托管任务和确定性关机；Web 与外部系统集成由各自 module 拥有。

目标：

1. 每个受框架管理的运行时能力都用同一个 Plugin 模型表达。
2. 插件选择在应用组合根中可见，依赖和 contract 在工厂运行前完成校验。
3. 配置、图、歧义和顺序错误在开放流量前失败，并给出稳定身份。
4. 工厂成功返回后，资源所有权立即且唯一地转移给 XBC；任意失败都能逆序回滚。
5. 生命周期、任务和流量门行为与具体 transport 无关。
6. 普通实现 package、Bundle 与 Prelude 可安全 import，不产生进程级注册副作用。
7. Core 保持小而可理解；协议与重型 SDK 不进入其依赖闭包。

非目标：运行时动态加载、classpath/package 扫描、通用 Service Locator、配置热重载、任意 scope、AOP/proxy、自动生成协议最低公分母，以及为尚未实现的 transport 预建占位 API。

## 2. 核心决策

### 2.1 一切受管理资源都是 Plugin

Plugin 是 XBC 能识别、配置、构造、连线、启动、停止并通过 contract 暴露的最小运行时单元。HTTP server、中间件、路由贡献者、数据库 client、scheduler 和应用 service 只要需要框架管理，就使用同一模型。

“Plugin”不是必须实现的 marker interface，也没有基类。一个具体值之所以是 Plugin，仅因为一个不可变 Definition 声明并拥有它。普通第三方类型可直接作为 primary value；需要适配生命周期时在 Definition 中提供 typed adapter。

package、feature、Bundle 或 Prelude 是静态组合概念，不因此成为运行时 Plugin。相反，每个需要独立身份、生命周期或顺序的能力都应有自己的 Definition；例如 Web error boundary 与 Web server 是两个 Plugin。

### 2.2 显式构造优先

XBC 解决的是独立 module 在同一进程中形成确定性图和共享生命周期的问题，不把普通 Go 程序改造成通用 DI 容器：

- package-level Definition 明确声明 constructor；
- typed Input token 明确声明 factory 可读取的依赖；
- consumer-owned interface 通过 contract 导出；
- `plugin.BuildContext` 只承载已预绑定 slot、身份和 logger；
- `plugin.Context` 只用于生命周期中的取消、日志、流量门、托管任务和关机请求；
- 不扫描字段、方法或“已初始化值”，不允许按类型临时查找任意对象。

### 2.3 显式 Bundle 是主组合 API

应用通过 `xbc.New(xbc.WithBundles(...))` 明确交付 side-effect-free Bundle。配置决定一个已组合 Definition 是否启用、展开几个实例以及实例参数，但配置不会发现未组合的代码。

`xbc.Run()` 只服务于明确选择 import-side-effect 风格的可执行程序。叶子 `.../autoload` 是可选适配器，不是默认架构；Prelude 本身始终无副作用。

## 3. 术语与不变量

### 3.1 Definition

`plugin.Definition` 是 opaque、immutable、canonical declaration handle。它静态拥有：

- 稳定 `plugin.Key`；
- 单实例或多实例 cardinality；
- activation 与配置路径；
- primary concrete result type；
- factory，或由纯 planner 生成的 factory；
- 完整 typed Inputs；
- primary value 导出的额外 interface contracts；
- 必要的 typed lifecycle adapters。

每个实现 package 在 package scope 只求值一次 `plugin.Define*`，并让 `Definition()` 始终返回同一 handle：

```go
var definition = plugin.Define(/* ... */)

func Definition() plugin.Definition { return definition }
```

禁止在 `Definition()` 内重新调用 `plugin.Define*`。同一个 canonical handle 被多个 Bundle 引入会去重；相同 key 对应不同 declaration handle 是冲突，Plan 必须拒绝并报告两个来源。

### 3.2 Plugin instance 与 Identity

一个 enabled Definition 按配置展开为零个、一个或多个实例。实例唯一身份是：

```go
type Identity struct {
    Plugin   plugin.Key
    Instance string
}
```

空实例名归一化为 `plugin.DefaultInstance`。日志、图、错误、contract entry、启动报告与关机报告都使用同一 Identity；不得从 Go 类型、package path、变量名或显示名称推导身份。

### 3.3 Contract

primary concrete type自动成为可解析 contract。额外 interface 必须由 Definition 静态导出：

```go
plugin.Contracts(
    plugin.ExportAs[CommandBus](func(value *Dispatcher) CommandBus { return value }),
)
```

witness function 让编译器证明 primary type 可赋给 interface；Plan 仍检查 nil、非 interface、重复导出和 adapter 边界。contract 表达能力，不产生匿名值；每个解析结果始终保留 producer Identity。

consumer 应拥有最窄 interface。producer 不需要 import consumer 的实现，Web/Core 也不需要认识所有贡献者。

### 3.4 Typed Input

Input token 是 package-level、immutable query metadata，本身不保存 binding。每个 App、Definition instance 和 factory invocation 的 binding 都在私有构造上下文中，因此同一个 token 可安全复用和并发使用。

现行查询只有四类：

| 构造器 | 语义 | `Get` 结果 |
|---|---|---|
| `plugin.RefTo[T](key)` / `RefToInstance[T](key, instance)` | 指定 Definition identity 必须导出 `T` | `plugin.Entry[T]` |
| `plugin.RequireOne[T]()` | 全图恰好一个 `T` exporter | `plugin.Entry[T]` |
| `plugin.OptionalOne[T]()` | 零个或恰好一个 exporter | `(plugin.Entry[T], bool)` |
| `plugin.Collect[T]()` | 收集全部 exporter | `[]plugin.Entry[T]` |

`Entry[T]` 同时携带 `Identity` 和 `Value`。`Collect` 结果按稳定图序返回新 slice；歧义、缺失和 contract 不匹配在 Plan 阶段失败，而不是在 factory 内选择一个“碰巧可用”的值。

### 3.5 BuildContext 与 lifecycle Context

`plugin.BuildContext` 仅在一次 factory 调用内有效。Input 的 `Get` 只能读取该 factory 声明过且已经预绑定的 slot；factory 返回后，所有 context copy 共享失效状态，再读取会 panic。它不是 `context.Context`，不能被后台 goroutine 留存。

`*plugin.Context` 是 lifecycle operation 参数并实现 `context.Context`。它提供当前 Identity、logger、全局 traffic gate、Start 期间的 `Go`/`GoCritical` 任务提交和 `RequestShutdown`。它不提供配置、contract lookup、运行中实例枚举或值发布；配置和依赖在构造前已经冻结。

### 3.6 Plan

`plugin.Plan[P]` 是 `DefinePlanned` 的纯结果：完整 InputSet 加一个 final factory。planner 可根据 prepared config 选择 token，但不得打开文件、建立连接、启动 goroutine、读取依赖值或取得生命周期 context。

应用级 Plan 是所有 Definition 完成配置展开、semantic preparation、per-instance planning、contract resolution 和 graph wiring 后的冻结结果。Plan 含 factory descriptor，但尚未调用任何 factory。

### 3.7 Bundle 与 Prelude

`plugin.Bundle` 是 canonical Definitions 的 side-effect-free 静态集合。它没有 key、配置、依赖、contract 或 lifecycle，也不是 Plugin。

- `plugin.BundleOf(definitions...)` 建立叶子组合；
- `plugin.CombineBundles(bundles...)` 建立聚合组合；
- 实现 package 通常同时提供 `Definition()` 与 `Bundle()`；
- Prelude 只组合已有 Bundle，不复制 Definition、不改变 activation、不含 `init()`；
- 重复 canonical handle 在 Plan 中折叠，来源信息保留用于诊断。

## 4. 公开 API 形态

### 4.1 根门面

根包保持六符号窄门面：`App`、`Option`、`New`、`WithBundles`、`Run` 和 `(*App).Execute`。普通显式组合：

```go
app, err := xbc.New(xbc.WithBundles(
    prelude.Bundle(),
    orders.Bundle(),
))
if err != nil {
    return err
}
code, err := app.Execute(ctx, os.Args[1:])
```

`New` 只保存 immutable Bundle 选择，不加载配置、规划、构造资源或执行 lifecycle。`App` 单次执行；第二次 `Execute` 必须失败。

### 4.2 无配置、静态依赖

```go
var handlers = plugin.Collect[Handler]()

var definition = plugin.Define(
    Key,
    func(ctx plugin.BuildContext) (*Dispatcher, error) {
        return NewDispatcher(handlers.Get(ctx)), nil
    },
    plugin.Options[*Dispatcher]{
        Inputs: plugin.Inputs(handlers),
        Exports: plugin.Contracts(
            plugin.ExportAs[CommandBus](
                func(value *Dispatcher) CommandBus { return value },
            ),
        ),
    },
)
```

token 的声明、`Inputs` 中的授权和 factory 内的 `Get` 必须是同一个值。未声明 token 的读取是编程错误，不能退化成 live lookup。

### 4.3 有配置、静态依赖

```go
var store = plugin.RequireOne[Store]()

var definition = plugin.DefineConfigured(
    Key,
    plugin.ConfigSpec[Config]{
        Defaults: defaultConfig,
        Prepare:  prepareConfig,
    },
    func(ctx plugin.BuildContext, cfg Config) (*Service, error) {
        return NewService(cfg, store.Get(ctx).Value), nil
    },
    plugin.Options[*Service]{
        Activation: plugin.WhenConfigured("plugins.service"),
        Inputs:     plugin.Inputs(store),
    },
)
```

`Defaults` 每次返回 fresh value；`Prepare` 只做 pure semantic normalization/validation。static inputs 不因插件有配置而使用 planner。

### 4.4 配置决定依赖

```go
var local = plugin.OptionalOne[LocalStore]()

var definition = plugin.DefinePlanned(
    Key,
    plugin.ConfigSpec[Config]{Defaults: defaultConfig, Prepare: prepareConfig},
    func(cfg Config) (plugin.Plan[*Service], error) {
        if cfg.RemoteInstance != "" {
            remote := plugin.RefToInstance[RemoteStore](RemoteKey, cfg.RemoteInstance)
            return plugin.PlanOf(
                plugin.Inputs(remote),
                func(ctx plugin.BuildContext) (*Service, error) {
                    return NewRemoteService(cfg, remote.Get(ctx).Value), nil
                },
            ), nil
        }
        return plugin.PlanOf(
            plugin.Inputs(local),
            func(ctx plugin.BuildContext) (*Service, error) {
                entry, ok := local.Get(ctx)
                return NewLocalService(cfg, entry, ok), nil
            },
        ), nil
    },
)
```

`DefinePlanned` 只用于 config 真正改变 dependency set 的场景。planner 返回后 Inputs、factory 与 graph edge 全部冻结。

### 4.5 生命周期 adapter

primary value 可以直接实现 `plugin.Initializer`、`Migrator`、`Runner`、`TrafficOpener`、`Closer`，也可在 `plugin.Options[P].Lifecycle` 中提供 typed adapter：

```go
plugin.Options[*redis.Client]{
    Lifecycle: plugin.Lifecycle[*redis.Client]{
        Stop: func(client *redis.Client, _ context.Context) error {
            return client.Close()
        },
    },
}
```

同一 stage 不得既由 primary method 实现又由 adapter 声明；Plan 在构造前拒绝歧义。

## 5. 配置、activation 与实例

### 5.1 配置所有权

Core 加载 Environment，Definition 决定自己的 typed Config。默认路径是 `plugins.<key>`；`Options.ConfigPath` 可声明 owner 明确的 canonical path，例如 Web 使用根级 `web`。插件只接收 prepared typed config，不在 lifecycle 中自行读取全局 Environment。

处理顺序固定：fresh defaults → 严格 decode → structural validation → pure `Prepare` → activation/instance plan。未知字段、类型错误、非法实例名、orphan plugin section 和 semantic error 都在 factory 前失败。

YAML、profile、programmatic overrides 与 ENV 的优先级由配置文档定义。文档和示例只展示 Definition 声明的 canonical path。

### 5.2 Activation

Activation 零值表示已组合 Definition 默认参与，除非配置显式 `enabled: false`。`plugin.WhenConfigured(path)` 表示只有 merged environment 中存在该路径时才启用。Activation 是 Definition metadata，不由“通过哪个组合入口进入”决定。

配置只控制已经进入 Bundle 的 Definition。存在 `plugins.<key>` 却未组合对应 key 是 orphan configuration，必须失败；这防止“写了配置但能力静默未启用”。

### 5.3 单实例与多实例

`plugin.SingleInstance` 是默认 cardinality；`plugin.MultipleInstances` 把配置节的直接子项展开为命名实例。每个实例独立运行 defaults、decode、Prepare 与 planning，使用规范化 Identity 参与 graph 和 contract resolution。

多实例消费者必须显式选择：用 `RefToInstance` 指定 identity，或用 `Collect` 接受全部。`RequireOne`/`OptionalOne` 遇到多个 exporter 必须报歧义，不自动选择 default 或声明顺序中的第一个。

## 6. Plan / Construct / Execute

### 6.1 Plan：无资源的冻结阶段

`internal/assembly.BuildPlan` 执行：

1. 展平 Bundle，并按 declaration identity 去重；
2. 校验 key、canonical handle、config path、primary concrete type、contract 与 lifecycle descriptor；
3. 加载并严格绑定每个 enabled instance 的配置；
4. 执行 defaults、validation、Prepare 和必要的 pure planner；
5. 冻结每个 instance 的 Inputs 与 final factory；
6. 建立唯一 identity index 与 contract index；
7. 解析所有 token，建立 dependency edge 并稳定拓扑排序；
8. 生成 immutable Plan 与可诊断的 disabled/ordered 列表。

这一阶段不得调用 primary factory。`doctor` 在 Plan 与报告后返回，因此能检查配置、contract、歧义、缺失依赖和环，而不建立连接、不创建文件、不启动 goroutine、不监听端口。

依赖排序仅由 typed Inputs 产生；Bundle 顺序和 import 顺序都没有依赖语义。

### 6.2 Construct：唯一资源事务

`internal/assembly.Construct` 按 graph order：

1. 从已构造 producer materialize 当前 factory 的 pre-bound slots；
2. 创建一次性 `BuildContext`；
3. 在 panic boundary 内调用 factory；
4. 使 BuildContext 及其所有 copy 失效；
5. 拒绝 nil 和 typed-nil primary result；
6. factory 成功返回非 nil 后立即把所有权转移给 XBC；
7. 创建 lifecycle Context 并调用 Init；
8. 把 instance 加入完整 rollback set。

factory 在返回错误前仍拥有自己创建的局部资源，必须自行清理；成功返回后不得再自行关闭。所有权一旦转移，即使 Init 失败或 panic，XBC 也必须调用该 instance 的 Stop。任意 factory/Init 失败会在返回错误前逆序 unwind 已拥有的所有 instance；一个 Stop error 或 panic 不得截断后续清理。

### 6.3 Execute pipeline

Plan 成功且不是 `doctor` 后，执行顺序为：

```text
Construct + Init
→ optional Migrate
→ Start
→ OpenTraffic
→ release one global traffic gate
→ wait for shutdown
→ reverse unwind
```

所有可失败的 traffic preparation 在关闭的 gate 后完成。`TrafficOpener` 不得自行暴露 ingress；需要 serving 的 Start-owned task 先等待 `ctx.TrafficGate()`，只有全部 participant 成功后 runtime 才一次性放行。

没有 Runner、托管任务或其他可保持进程存活的能力时，启动必须失败，不能用永久空等伪装成正常服务。

### 6.4 生命周期与任务

- Init/Start/OpenTraffic 等 hook 同步执行并受 panic boundary 保护；runtime 不尝试强制抢占任意插件代码。
- `ctx.Go` 和 `ctx.GoCritical` 只在所属 Plugin 的 Start hook 执行窗口内接受任务；窗口关闭后提交返回 false。
- critical task panic 或非预期返回会请求应用级关机；普通任务由插件自行定义失败语义。
- shutdown 先关闭全局和各 Plugin 的任务准入。
- 对每个 Plugin 的逆序清理顺序是 **Stop → cancel managed tasks → join**。
- 全部 Plugin 共享一个 `xbc.shutdown_timeout` deadline；不是每个 Stop 各得一份预算。
- 卡住的 Stop 在共享 deadline 后被放弃并报告，不能永久阻塞整个进程。
- Stop 由 XBC 至多调用一次，但实现必须能安全处理任意部分启动状态，并宜保持自身幂等。

### 6.5 Dependency order 与 domain order 分离

constructor dependency 只描述“构造 B 需要 A”，由 typed Inputs 建图。middleware、authentication 或其他领域执行顺序不应伪造成构造依赖。

各领域拥有自己的 typed order reference、缺失策略与 cycle 诊断。需要同时表达构造依赖和执行顺序时，分别声明两者，不能依赖 Bundle 或 import 顺序作为隐藏 tie-break。

## 7. Web 模型

### 7.1 独立 transport owner

`transport/web` 是可选 Gin-backed module，Core 不 import Gin。Web server 自身是 Plugin，并通过 typed contract 收集：

- `web.Middleware`；
- `web.RouteContributor`；
- `web.RouteCatalogListener`；
- `web.ErrorMapper` 等窄能力。

贡献者以普通 primary value 导出 interface contract。Web server 的 factory 从 `plugin.Collect[T]` 获得带 Identity 的稳定 entry。

### 7.2 中间件身份与排序

每个独立排序的 middleware 都有自己的 Plugin Identity。`web.Order` 使用稳定 phase 和 typed `[]web.OrderRef`：

```go
web.Order{
    Phase: web.PhaseAuth,
    After: []web.OrderRef{web.Require(authentication.Key)},
}
```

- `web.Require` / `RequireInstance`：目标缺失即启动失败；
- `web.Prefer` / `PreferInstance`：目标缺失只形成诊断 miss；
- security prerequisite 必须使用 required reference；
- key/instance 由 typed helper 构造，不用散落的字符串顺序名；
- phase 冲突、required target 缺失、重复 identity 和 cycle 在流量开放前失败。

实现 `security.RequiresPrincipal` 的 middleware 由 Web sorter 自动 pin 在 canonical authentication middleware 之后；显式反向约束必须失败，不能悄悄倒置安全链。

### 7.3 路由与认证状态

Route table 在 Start 中由 route contracts 构建并冻结，在 traffic gate 打开前通知 listener 一次。listener 只观察 immutable snapshot；没有 Web server 时不会被其他运行栈误触发。

route authentication 不能把“遗漏”解释成 public。`RouteInfo.Auth` 保留三个不同状态：

1. 未声明：采用 restrictive default；
2. `web.Public()`：明确公开；
3. `web.Accepts(schemes...)`：明确允许的认证 scheme。

credential 解析必须区分 absent、malformed、rejected 与 authenticated；malformed credential 不得继续尝试更宽松路径。scheme 名称、重复、顺序、challenge 和多 credential 行为都应在冻结或启动测试中确定。

### 7.4 错误边界

`web.Handle` 让 handler 返回普通 Go error；`web.AbortError` 供 middleware 立即终止；原生 Gin handler 可用 `c.Error` 报告。独立 `web.onerror` Plugin 汇总 `web.ErrorMapper` contracts，未知错误固定映射为不泄漏原因的 500 Problem Detail。panic 由 recovery Plugin 处理，不混进普通 error mapping。

成功响应由 handler 明确写出。可选 `web/biz` Bundle 提供显式 success envelope 与业务错误 mapping，但不进入 Prelude，也不把失败伪装成 HTTP 200。`web.ProblemDetail` 是 RFC 9457 wire DTO，不进入协议无关领域层。

### 7.5 Prelude

`transport/web/prelude.Bundle()` 组合 Web server、recovery、request ID、access log、security headers、gzip、timeout 与 health。它：

- 不含 `init()`；
- 不复制或修改成员 Definition；
- 不强制覆盖成员 activation；
- 不创建 live resource；
- 不包含 CORS、业务 envelope、认证授权、API documentation 或 telemetry exporter 等应用策略。

应用显式组合 Prelude 与所需可选 Bundle。

## 8. 日志与可观测性

`log` 是协议无关门面，Plugin 经 `ctx.Log()` 获得自动附加 Identity 的 logger。调用方依赖窄接口，不必 import 具体 binding。Trace context 通过 operation context 传播，不作为构造依赖保存。

日志使用 structured KV；encoder 层负责 console/json 渲染与敏感字段脱敏。替换默认 logger 后，脱敏和 sink 语义也由替代实现负责，宿主必须明确承担该责任。

启动报告至少包含 Plan 的 enabled order、disabled Definition、迁移选择及 domain-order miss。报告是诊断，不改变图语义；不得通过日志中的“识别到某方法”弥补缺失的静态声明。

## 9. 测试与架构守卫

### 9.1 Definition/Bundle 合规

架构测试应验证：

- 实现 package 有 package-level `plugin.Define*` canonical handle；
- `Definition()` 只返回该 identifier；
- `Bundle()` side-effect-free 且引用 canonical handle；
- 普通实现 package 与 Prelude 不 import `internal/autoload`；
- Prelude 无 `init`；
- leaf autoload 只把 parent `Bundle()` 交给私有适配器；
- 同 key/different handle、重复 contract、非法 primary、生命周期歧义均在 Plan 失败。

### 9.2 Plan/Construct 行为

测试至少覆盖：

- doctor 不调用 factory；
- defaults、decode、validation、Prepare、planner 的固定顺序；
- token 缺失、歧义、exact ref、optional 与 deterministic collect；
- retained BuildContext copy 在 factory 后失效；
- nil/typed-nil、factory panic/error 与 Init panic/error；
- ownership transfer 后完整逆序 unwind；
- partial lifecycle 下 Stop 安全且最多调用一次；
- Start-only task admission，critical task shutdown，Stop → cancel → join；
- traffic gate 不在任何 preparation 失败时开放；
- shared shutdown budget 能越过卡住的 Stop。

### 9.3 Workspace 与 API

每个 `go.work` module 都必须被 Makefile、CI 和架构测试覆盖。根 `go test ./...` 不进入嵌套 module，不能代替 workspace-wide validation。公开 API guard 固定根门面与 plugin model；改变设计时应同步更新 guard 和所有 caller，不能 skip、删除或弱化测试。

仓库 inventory 必须覆盖全 workspace、测试、文档和示例并保持零违规；架构演进必须同步更新 guard、调用方和权威文档。

## 10. 已知取舍

| 取舍 | 代价 | 选择理由 |
|---|---|---|
| 应用显式列出 Bundle | composition root 多几行 import 与调用 | 插件选择可审查、可测试，不依赖 import 时序 |
| Input token 需声明并重复放入 `Inputs` | 比任意 resolver 多少量样板 | factory 权限、graph edge 与诊断在运行前确定 |
| primary 只有一个 concrete owner | 多个独立资源需拆 Plugin 或包进 owner struct | 生命周期和清理责任唯一，不产生匿名值 |
| interface contract 必须显式导出 | producer 多一段 witness | consumer-owned abstraction 可静态验证，避免扫描 |
| planner 仅处理动态 dependency set | 作者需区分 static 与 planned definition | 常见 API 简单，副作用边界清楚 |
| 不支持运行时图 mutation | 动态启停需要重启 | identity、contract、lifecycle 与并发语义保持可证明 |
| domain order 独立建图 | 同一能力可能写两类约束 | 构造依赖与执行语义不会相互污染 |

判断设计是否仍然成立的标准很简单：任意 runtime-owned value 都能追溯到一个 canonical Definition 和一个 Identity；任意 factory 读取都来自声明过的 typed Input；任意应用选择都能在显式 Bundle 组合根中看到；Plan 完成前不获取资源，Construct 失败前不遗留资源。
