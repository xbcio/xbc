# xbc — 面向多运行栈的包布局设计

- **日期**：2026-08-26
- **仓库**：`git@github.com:xbcio/xbc.git`
- **Core module**：`github.com/xbcio/xbc`
- **状态**：已实现（2026-08-28 撤销 `internal/runtime` / `internal/startupreport` 两个伪边界并同步最终契约）
- **定位**：通过插件快速构建 Web、RPC/gRPC、分布式服务与微服务
- **优先级**：在产品定位、插件发现与装配机制、通用生命周期、health 归属及包/module 边界上取代 `2026-08-23-xbc-plugin-framework-design.md` 的冲突结论

---

## 0. 最终结论

`xbc` **不是一个以 Gin 为内核的 Web 框架**，而是一个与传输协议无关的插件应用运行时：

- 核心只负责插件发现、配置绑定、依赖解析、初始化、启动、托管任务和逆序关闭；
- `web`、`grpc` 以及未来其他协议能力都是可选插件；
- 同一个应用可以只启用 Web、只启用 gRPC，也可以同时启用多个运行栈；
- 根包和核心插件契约不得出现 `gin.Context`、`http.Server`、`google.golang.org/grpc.Server`、路由或中间件类型；
- 重型可选运行栈使用独立 Go module，不能把 Gin/gRPC 依赖带给纯任务或纯 RPC 服务；
- 应用不手写 `Register` 或插件构造清单；可链接插件在包初始化期只向目录声明**无副作用工厂**，`xbc.Run()` 冻结目录后自动装配。

目标入口：

```go
package main

import (
    _ "example.com/order-service/internal/order"

    "github.com/xbcio/xbc"
    _ "github.com/xbcio/xbc/web/autoload"
)

func main() {
    xbc.Run()
}
```

这里的 import 是 Go 编译期依赖选择，不是运行时扫描：被链接的业务注册包或显式 `autoload` 包声明“有哪些插件可用”，配置决定实例与启停，依赖图决定顺序。注册动作只登记无副作用的 Definition/Factory，不得在 `init()` 中创建连接、启动任务或监听端口；普通实现包（例如 `web`）本身保持无注册副作用。

本次落地范围只包含 core、`web` 与 `examples` 三个 module。`grpc`、`management/health`、`integration/*` 在真正实现前**不创建目录、不创建空 `go.mod`、不创建占位 API**——本文档里凡是提到它们的地方，约束的都是“将来实现时不许越界”，不是现在就该落盘的结构。多加一个 `_ "github.com/xbcio/xbc/grpc"` 的 import 行属于将来时，不属于当前入口。

---

## 1. 从 Spring / Spring Boot 借什么

借鉴的是**稳定能力分层与可选模块隔离**，不是 Java 的目录名和自动扫描机制。

| Spring / Spring Boot | 核心思想 | xbc 对应 |
|---|---|---|
| `spring-core` | 与上层框架无关的基础机制 | `topology`、`internal/container/inject` |
| `spring-beans` | 定义、实例化、依赖解析与注入 | `internal/container` |
| `spring-context` | 组件上下文与统一生命周期 | 根包 `xbc` + `plugin.Context` |
| `SpringApplication` | 应用启动门面 | `xbc.Run()`、`xbc.New(options...)` |
| `Environment` | 外部化配置、profile、配置源 | `config.Environment` |
| `spring-web*` | 可选 Web 运行栈，不污染 core/context | `web` |
| gRPC / messaging starter | 引入可选能力及其默认装配 | 独立 `grpc` 等插件 module |
| auto-configuration | 从可用定义按配置装配 | `plugin/catalog` + 插件定义的启用条件 |
| actuator | 可选运维能力与协议适配分离 | 按需建立 `management/health`，HTTP/gRPC 暴露仍由各自适配器负责 |
| test modules | 面向扩展作者的测试支持 | 将来的 `plugintest`，有真实用户后再建立 |

三项不能错误类比：

1. **starter 不是实现包。** Spring Boot starter 主要是依赖聚合描述；Go 中最接近它的是“import 一个自声明 Definition 的插件 module + 默认配置”，不能把所有插件实现统称为 starter。
2. **`Opt[T]()` 不是 `@ConditionalOnMissingBean`。** 前者表示依赖不存在也能运行；后者表示满足缺失条件时才注册另一个组件，语义不同。xbc 当前没有后者的直接等价物。
3. **actuator 不是默认 Web 内核。** 它是显式选择的运维模块，不应由通用 `xbc.New()` 暗中加入。

Go 没有可扫描 classpath，因此不伪造 `@ComponentScan`。xbc 采用更窄的**编译期目录发现**：链接进二进制的业务注册包或显式 `autoload` 包在 `init()` 中仅声明 `plugin.Definition`，`xbc.Run()` 再按环境配置创建应用私有实例。可复用实现包可以只提供无副作用的 `Definition()`，由调用方选择私有 Catalog 或 `autoload`。它借鉴 Spring Boot 的“依赖可用 → 条件装配”，但不扫描源码、不加载 `.so`，也不允许 `init()` 执行运行时 I/O。当前落地已经删除 `App.Register`、全局 live instance 列表以及显式/隐式双路径。

### 1.1 从 Kubernetes / Hertz 借什么，不照搬什么

Kubernetes 值得借鉴的是**明确 owner、薄入口、单向 internal 边界和可执行的架构守卫**：命令入口不承载领域实现，稳定 API 由固定包拥有，内部装配细节不能被可选模块反向引用。xbc 因而让 `plugin`、`config`、`topology` 各自成为 canonical owner，把应用私有的装配细节关进 `internal/container`，并用 `go list` 与 AST 守卫真实依赖图。但当前仓库远小于 Kubernetes，既不机械建立庞大的 `/cmd` 树，也不为了“像大项目”增加 `/pkg`、`staging` 或多层 `internal`；一个只有单一 `xbc.Run()` 入口的库，增加这些层次只会延长 import path、制造伪边界——把运行内核关进 `internal/runtime` 就是这样一次伪边界，已经撤销（§2.1）。

Hertz 值得借鉴的是**运行时与工具/生成器、协议实现和可选集成分离**：运行时库不应因为工具链或某个可选实现而携带全部依赖。xbc 对应地把协议无关运行时留在 core module，把 Gin-backed 实现放进独立 `web` module；将来若有生成器，它也应独立于运行时。注意这条分离的单位是 **module**，不是包：真正挡住 Gin 的是 `web` 有自己的 `go.mod`，而不是 core 内部多一层 `internal/`。普通 `web` import 必须无注册副作用，只有显式 blank import `web/autoload` 才写入默认 Catalog。没有照搬的是 Hertz 以 HTTP engine 为中心的包树——xbc 还要承载 gRPC、consumer 和纯任务应用，因此 HTTP 类型不能成为根包骨架。

---

## 2. 包与模块划分原则

### 2.1 包是依赖边界，不是文件收纳盒

只有同时满足下列条件，才新建包：

1. 有一句独立、稳定的职责描述；
2. 有清晰的允许依赖与禁止依赖；
3. 类型之间的内聚大于它们与原包的内聚；
4. 拆分后不会为了访问私有状态而导出一批伪 API。

代码行数、文件数和“看起来对称”都不是拆包理由，但**共享同一份应用状态、生命周期不变量和单向可见性**是拆包理由。根包现在有十二个生产 Go 文件：`app.go` 持有公开 API 与运行状态，bootstrap、execute、lifecycle、shutdown、task、settings、process、host 与 startup_report 各自按职责占一个文件，共同操作同一个 `App`。它们内聚为一个包，正是因为共享那份状态；内部按职责拆文件，但不按 `stage_1.go`、`stage_2.go` 这种时间编号形成伪模块。

这里曾经拆过一次，又拆回来了，值得写下为什么。早先的布局把根包压缩成 `app.go` / `doc.go` / `run.go` 三个文件的公开 façade，把上面那些文件放进 `internal/runtime`，理由是「单向可见性」。**这条理由没有兑现**：实测 `go list -deps` 显示根包与 `internal/runtime` 的依赖闭包是逐字相同的 74 个第三方包，边界没有隔离任何依赖；`web` module 根本不 import 根包（它自己的 `arch_test.go` 明令禁止），所以也不存在被这条边界挡住的反向依赖。边界唯一真正兑现的性质是「运行时新增一个首字母大写的标识符不会泄漏成公开 API」——而这一条现在由 `tests/architecture` 的导出清单守卫（§9 约束 15）直接钉住，不需要一个包边界来换。

代价则是具体的：一个逐字转发的 wrapper 类型（根 `App` 是 `struct{ impl *appruntime.App }`，`Execute` 原样转发）、四个只为跨越边界而存在的导出符号、以及同一个 codebase 里两个都叫 `App` 的类型。**同理适用于 `internal/startupreport`**：它的存在理由写的是「让渲染函数不必构造 App 就能测试」，但包边界并不提供这个性质——没有任何东西强制包 X 的测试必须构造 X 的主类型，今天 `startup_report_test.go` 就在根包里直接构造 `*container.Container` 而不碰 `newApp`。那条边界唯一可测量的效果，是逼着 `bare` 和 `fakeHost` 两个 fixture 从已有位置复制一份，因为测试 helper 不跨包。它表达的职责划分是真的，现在作为规则活在同一个文件里：所有 `render*` 函数返回字符串、不做 I/O。

### 2.2 名字直接表达领域能力

- 使用 `plugin`、`config`、`web`、`grpc` 等可直接解释的能力名词；
- package 表达稳定职责，具体主类型表达角色：`web.Server`、`grpc.Server`，而不是把二者粘成 `ginserver`、`grpcserver`；
- 不建立 `core`、`kernel`、`engine`、`common`、`utils`、`helper`、`manager` 这类无业务边界的杂物包；
- 不为缩短几个字符使用 `conf`、`mwchain`、`resp`、`errs` 等不必要缩写；
- `web` 明确表示“Gin-backed Web 应用运行栈”，不是虚构的传输无关 HTTP 抽象；Gin 是当前实现和公开 Handler 契约的一部分，但不是包的职责名；
- 若未来确有第二种 Web 引擎，再基于真实公共契约拆出 `web/gin` 等实现；现在不为假想替换预建层次；
- 不预建泛化的 `rpc` 包。只有两个以上实现真的共享稳定契约后，才抽取公共协议层。

### 2.3 一个概念只有一个 canonical owner

- 应用门面、应用运行状态、生命周期与进程适配都归根包（同一份 `App` 状态，不再拆成两个包，见 §2.1）；
- 插件契约归 `plugin`；
- 配置环境归 `config`；
- Web server、路由和中间件归 `web`；
- gRPC server、service 和 interceptor 归 `grpc`。

不能同时长期维护 `xbc.Router` 与 `web.Router` 两套入口。兼容别名只能是有删除期限的迁移手段，不能成为目标架构。

### 2.4 可选重依赖用 module 隔离

Spring 的模块是独立 artifact；只移动 Go package、却让根 `go.mod` 永久携带所有可选依赖，只完成了一半隔离。

- 核心 module：`github.com/xbcio/xbc`；
- Web 插件 module：`github.com/xbcio/xbc/web`；
- gRPC 插件 module：`github.com/xbcio/xbc/grpc`；
- 仓库开发使用 `go.work` 联调；发布与依赖版本各自可控；
- 子 module 的 `go.mod` 只能依赖**已经真实发布**的 core 版本，不提交指向本地目录的 `replace`；对应 tag 尚不存在时先不写该 `require`，禁止用 `v0.0.0`、伪造 pseudo-version、预发布或其他占位版本假装可发布；
- 发布顺序固定为 core 在前、各可选 module 在后。core 使用真实 `vX.Y.Z` tag，嵌套 module 使用如 `web/vX.Y.Z`、`grpc/vX.Y.Z` 的路径前缀 tag；只有上游 tag 已推送且可被 `GOWORK=off` 解析后，下游才能添加相应 `require`；
- 可选运行栈、management、数据、注册发现和消息系统集成达到稳定形态后，均按独立 module 发布。

`go.work` 只是仓库开发工具，不是消费者解析依赖的前提。示例也必须使用独立 module；否则 `go mod tidy` 会因示例 import 而把 `web`、`grpc` 等可选 module 重新写进 core 的 `go.mod`。这样纯后台任务或纯 gRPC 服务依赖 core 时，不会下载或编译 Gin；Web 服务也不会被迫携带 gRPC。

---

## 3. 当前布局（已落地）

`management/`、`integration/` 只是仓库级分组目录，不创建同名 Go package；`web` 是一等能力 module；任何能力没有实现时都不创建目录。

`grpc`、`management/health`、`integration/*` 在下面都只占一行注释：在真正实现之前列出它们的文件级清单，本身就是 §2.2 禁止的“为假想结构预建层次”。它们的职责边界（§4）和 API 归属（§7）仍然写明，因为那约束的是**将来实现时不许越界**，而不是现在就该落盘的目录。当前实际落盘的 module 只有三个：core、`web`、`examples`。下树精确列出根目录全部 12 个生产 Go 文件、主要生产 owner 与架构守卫；普通 `*_test.go` 仅为压缩篇幅而省略。

```text
xbc/
├── .gitignore                     本地配置、运行日志与工具产物忽略规则
├── go.mod / go.sum                core module 清单与依赖校验和
├── go.work / go.work.sum          core / web / examples 本地联调及 workspace 校验和
├── app.go                         App：公开句柄与运行状态合一；New / Option / WithDefinitions
├── bootstrap.go                   配置、日志、settings、task/container 引导
├── doc.go                         根包总览、入口与协议无关边界
├── execute.go                     显式 args/context 执行与退出码
├── host.go                        RuntimeHost 的唯一生产实现 hostAdapter
├── lifecycle.go                   Init / migrate / 两阶段 readiness 编排
├── process.go                     os.Args、signal、log.Sync、os.Exit 的唯一 owner
├── run.go                         零参数 Run 的薄进程入口；委托 process.go
├── settings.go                    私有 core settings
├── shutdown.go                    有界逆序 unwind 与错误聚合
├── startup_report.go              启动报告：渲染（吃 Instance 吐字符串）与输出时机
├── task.go                        托管任务、critical failure 与关机状态
│
├── plugin/                        稳定、协议无关的插件 SPI
│   ├── plugin.go                  marker Plugin / 可选 Base
│   ├── identity.go                Key / Identity / 实例名规则
│   ├── definition.go              Definition / Factory / Cardinality / Activation
│   ├── lifecycle.go               Configurable / Initializer / Runner / TrafficOpener / Closer ...
│   ├── dependency.go              Dep / Ref / Deps / Need / Opt / Offer / RefTo / Key.Ref()
│   ├── context.go                 Context：Key、config.View、日志、任务
│   ├── host.go                    RuntimeHost 窄端口与 Context 构造
│   ├── registry.go                Provide / Get / GetNamed
│   ├── extension.go               Extension[T] / Extensions[T]
│   ├── arch_test.go               SPI 依赖闭包与 catalog 纯净性守卫
│   └── catalog/
│       └── catalog.go             Catalog / Snapshot / New / Declare / Freeze
│
├── config/                        外部化配置能力
│   ├── environment.go             View；Environment 的 Get / Exists / Sub / Bind
│   ├── view.go                    Scope：实例相对路径只读视图与递归防御复制
│   ├── source.go                  file / profile / ENV / override 优先级
│   ├── bind.go                    schema 驱动绑定与默认值
│   ├── schema.go                  统一字段 walker、YAML path 与严格未知字段检查
│   ├── validate.go                配置校验及聚合错误
│   └── arch_test.go               config 不反向依赖根包或 log 的守卫
│
├── log/                           独立日志与 trace 门面
├── topology/                      stdlib-only 的稳定拓扑排序能力
│   └── graph.go                   AddHardEdge / AddSoftEdge / Direction / Miss / errors
│
├── internal/
│   ├── cli/
│   │   └── command.go             Command / ParseArgs / WantsMigration(bool)
│   ├── container/                 应用私有实例、装配、依赖、注册表、初始化状态
│   │   ├── container.go           聚合状态与窄入口
│   │   ├── instance.go            Definition 展开后的应用私有实例
│   │   ├── expand.go              Activation 与静态单/多实例展开
│   │   ├── bind.go                Assemble 第二阶段：插件配置绑定与校验错误聚合
│   │   ├── callback.go            Factory/ConfigPtr 等扩展回调的 panic 与返回值边界
│   │   ├── declarations.go        Dependencies/Provides 合并、校验与单次缓存
│   │   ├── resolve.go             依赖/产物选择与拓扑序
│   │   ├── registry.go            (type, instance) 值存储
│   │   ├── initialize.go          注入与产物收割；不调用插件 Init
│   │   ├── scan.go                Plugin 零方法宽契约到 inject 严格契约的适配
│   │   └── inject/
│   │       └── scan.go            struct tag 扫描、写入与收割
│
├── tests/
│   ├── architecture/
│   │   └── architecture_test.go   全仓依赖、布局、module 与 API 形状守卫
│   └── integration/
│       └── public_api_test.go     外部包视角锁定公开 API 与 App 方法集合
│
├── docs/
│   └── configuration.md           配置格式；可执行样例只保留在 quickstart
│
├── web/                           独立 module；Gin-backed Web 插件
│   ├── go.mod
│   ├── plugin.go                  无副作用 Definition()；普通 import 不注册
│   ├── autoload/register.go       显式 import-time 默认目录注册
│   ├── server.go                  Server：Runner + TrafficOpener + Closer
│   ├── config.go                  addr / base_path / read/write timeout
│   ├── extension.go               Route/Middleware/Catalog capabilities
│   ├── router.go                  Router / RouteInfo / RouteCatalog / CurrentRoute
│   ├── middleware.go              Middleware / Phase
│   ├── order.go                   Phase 分组与组内拓扑排序
│   ├── report.go                  路由和中间件启动报告
│   └── arch_test.go               web 不直接 import 根包/internal 的守卫
│
├── examples/                      独立示例 module，不污染 core go.mod
│   ├── go.mod                     module github.com/xbcio/xbc/examples
│   └── quickstart/                blank import web/autoload + 业务插件后调用 xbc.Run()
│       ├── main.go                import 列表和一行 xbc.Run()
│       ├── application.yml        唯一可执行配置样例
│       ├── arch_test.go            示例不直接 import core internal 的守卫
│       └── internal/greeter/      不嵌入 Base 的 marker Plugin 路由示例
│
└── （将来）grpc/、management/health/、integration/<technology>/
    本次不创建目录、go.mod 或占位 API
```

### 3.1 为什么入口是零参数 `Run()`

应用开发者的主路径只有 `xbc.Run()`。插件选择放在 import（版本由 `go.mod` 记录），启停和实例参数放在配置，启动顺序交给依赖图；不再把同一事实重复写进 `app.Register(...)`。需要测试或嵌入时使用 `xbc.New(options...)`，`Option` 只配置宿主行为或通过 `WithDefinitions` 替换私有定义集，不能再作为另一条普通插件注册通道。

根包也不提供暗中固定 Web/management 组合的 `Default()`：零参数 `Run()` 的“默认”只是装配当前二进制已经链接的插件，不代表框架强选某种服务形态。

### 3.2 为什么没有 `errs/` 和 `resp/`

前置设计中的 `Error.Status`、JSON `Response`、`Paged` 和 handler 适配器都是 HTTP 语义，不能放进协议无关 core：

- HTTP response/problem 归 `web` 或其子包；
- gRPC 使用 `codes.Code` / `status.Status` 的映射；
- 跨协议业务错误只有在 HTTP 与 gRPC 的映射规则共同稳定后，才另行设计如 `fault` 的中立契约。

现在预建 `errs`/`resp` 只会把 HTTP 偏见永久带进 core。

### 3.3 自动发现不是任意 `init()` 副作用

插件 module 暴露的是不可变定义和延迟工厂，而不是 live instance。`Definition` 的稳定字段是 `Key`、`Factory`、`Instances`、`Activation`；其中 `Instances` 的零值 `SingleInstance` 是默认策略：

```go
func Definition() plugin.Definition {
    return plugin.Definition{
        Key:        "web",
        Factory:    func() plugin.Plugin { return new(Server) },
        Instances:  plugin.SingleInstance, // 可省略：零值即单实例
        Activation: plugin.Always,
    }
}
```

普通 `web` import 只提供类型、能力接口和上面的无副作用 `Definition()`，**不会**改写默认 Catalog。只有可执行程序显式 blank import 的 `web/autoload` 才执行注册：

```go
// package web/autoload
func init() {
    catalog.Declare(web.Definition())
}
```

应用自己的业务插件也可以在其包内直接 `Declare`，但这个 `init()` 同样只能记录静态定义。可复用实现若既要支持私有 Catalog 又要支持默认装配，应采用“实现包返回 Definition、独立 autoload 包负责 Declare”的分层，不能让普通 import 偷偷改变进程状态。

唯一插件身份是 `Definition.Key` / `plugin.Key`。`plugin.Plugin` 只是 marker：

```go
type Plugin interface{}
```

它不要求 `Name()`，也不从 Go 类型或包路径推导身份；因此同一实现类型可以安全支撑多个不同 Definition。`plugin.Base` 只是可选便利设施，为选择嵌入它的实现保存 Context 和 Key；`Base.Name()`、`Context.Name()` 只是把 Key 转为文本供日志/展示使用，不是新的身份来源。不嵌入 Base、只实现所需 capability 的 struct 同样是合法插件。插件依赖只通过稳定 Key 构造：`plugin.RefTo(key)` 或 `key.Ref()`，不提供从实例或具体类型反推身份的入口。

`Definition.Instances`（类型为 `plugin.Cardinality`）是单/多实例形态的唯一来源：需要从 `plugins.<key>.<instance>` 展开的定义静态声明 `plugin.MultipleInstances`。运行时不再通过构造对象后探测 `MultiInstance()` 改变 Definition 形态；因此在调用 Factory 前就能确定是否调用以及调用次数。

这套机制必须满足以下硬约束：

1. `Declare` 只能记录 Key、工厂、静态实例形态和静态启用策略；不得读取配置、连接外部系统、启动 goroutine 或监听端口；
2. 未传 `WithDefinitions` 的第一次 `xbc.New()`（包级 `xbc.Run()` 走的也是这条路径）原子地冻结默认 Catalog；`xbc.New(xbc.WithDefinitions(snapshot))` 直接使用私有 Snapshot，不读取也不冻结默认 Catalog。Catalog 一旦冻结便不可增删，重复 Key、非法 Key 和非法定义在装配前统一报错；
3. Catalog 声明顺序没有语义；冻结时按 `Definition.Key` 稳定排序后再构建依赖图，结果不得依赖 Go 的 `init()` 顺序；
4. Factory 只为当前 App 创建新实例，每个启用实例恰好调用一次；禁用的 Definition/instance 不得调用 Factory，返回 nil 也属于契约错误；
5. 全局只保存 Definition，不保存 live plugin、配置、连接或运行状态；根包的白盒/行为测试使用私有 `catalog.New()`，`Freeze()` 后直接调用未导出的 `newApp(snapshot)`，不修改默认 Catalog；`tests/integration` 则以外部包视角通过 `xbc.New(xbc.WithDefinitions(snapshot))` 验证公开 API；
6. 插件构造函数只建立零副作用对象；所有 I/O 仍延后到 `Init`/`Start`，失败走统一回滚。

### 3.3.1 冻结的类型化：`Snapshot` 是唯一能进入装配的形态

上面第 2、5 条如果只写在文档里，实现时一定会退化成「`Freeze()` 设一个 bool，然后照样把可变的 `*Catalog` 传进去」。要让「只有冻结后的定义集能装配」成为**编译期事实**而不是运行期约定，冻结必须产出一个新类型：

```go
package catalog

// Catalog is a mutable set of plugin definitions. Declare writes into it;
// Freeze closes it and yields the immutable Snapshot assembly consumes.
type Catalog struct{ /* ... */ }

func New() *Catalog
func (c *Catalog) Declare(d plugin.Definition)

// Freeze closes the catalog and validates every definition it holds:
// duplicate keys, invalid keys, nil Factory and invalid static metadata. It
// is idempotent -- calling it
// again returns the same Snapshot and the same error.
func (c *Catalog) Freeze() (Snapshot, error)

// Snapshot is an immutable, deterministically ordered set of definitions.
// Its zero value is the valid empty snapshot. There is no way to mutate one;
// every non-empty Snapshot is produced by Freeze.
type Snapshot struct{ /* unexported slice */ }

func (s Snapshot) Len() int
func (s Snapshot) Definitions() []plugin.Definition  // defensive copy, sorted by Key
func (s Snapshot) Lookup(plugin.Key) (plugin.Definition, bool)

// Declare writes into the process-wide default catalog. Explicit registration
// or autoload packages call this from init(). Panics after the default catalog
// has been frozen.
func Declare(d plugin.Definition)

// Freeze freezes the process-wide default catalog. Idempotent.
func Freeze() (Snapshot, error)
```

配套的根包签名也随之钉死：

```go
package xbc

// New freezes and validates the selected definition set without constructing
// plugins or doing I/O. Catalog validation and malformed options are returned as
// errors; an intentionally empty private Snapshot remains constructible, and
// Execute decides whether that command has anything useful to run.
func New(opts ...Option) (*App, error)

// WithDefinitions replaces the definition set this App assembles from. It
// takes a frozen Snapshot -- not a *Catalog, not a []Definition -- so there
// is no signature through which a mutable or unvalidated definition set can
// reach assembly at all.
func WithDefinitions(s catalog.Snapshot) Option
```

`WithDefinitions` 只收 `Snapshot` 这一点是刻意的：如果它收 `*Catalog`，调用方就能在 `New` 返回之后继续 `Declare`，「冻结后不可增删」立刻变成一句空话；如果它收 `[]plugin.Definition`，重名与 nil Factory 的校验就得在装配阶段重做一遍，于是校验逻辑有了两个副本。零值 Snapshot 被明确约定为合法空集；除此以外，非空 Snapshot 只能由 `Freeze` 产生，因此「已冻结、已校验、顺序确定」由类型与构造路径共同保证。

私有 Snapshot 绕过默认 Catalog 是应用级运行状态隔离的基础：`catalog.New()` 建一个私有目录、`Declare` 若干假插件、`Freeze()` 得到 Snapshot、`xbc.New(xbc.WithDefinitions(snap))`。这样不同 App 不共享插件实例、Context、值注册表或 task runtime，也不会把私有定义写回默认 Catalog。但这**不是完整进程隔离**：环境变量、默认 Catalog 本身、全局日志、OS signal 以及 Gin 等协议库 globals 仍可能共享；需要隔离这些设施时应使用不同进程，测试也不得因私有 Snapshot 就假定所有进程级状态都可无条件并行修改。

`Activation` 是 Definition 自身的稳定元数据，不由入口来源决定。协议运行栈和无配置业务插件通常声明 `Always`；数据库、缓存等基础设施可声明 `Configured("plugins.<key>")`。无论哪种策略，显式 `enabled: false` 优先级最高。禁止提供任意回调读取环境或其他插件，否则“发现阶段”会再次变成一套隐藏容器。


**`Factory` 顺带消灭了 `clonePrototype`。** 旧实现注册的是**已经构造好的实例**：`Register` 收下一个插件对象，展开出多个实例时只能用反射造零值（旧 `stage_expand.go` 的 `clonePrototype`），并由裁定 R7 规定「只有显式 `Register` 且恰好展开出一个实例时才可复用构造好的原型」，多实例时还要额外打一条「构造参数不会带到各实例上，请确认插件零值可用」的警告。

这一整套补丁存在的唯一原因，是注册的是**实例**而不是**构造方式**。`Definition.Factory` 从根上取消了这个前提：每个实例都由工厂新建，不存在可复用的原型，也就不存在「什么时候可以复用原型」这个问题。R7 及其警告、`clonePrototype` 及其反射零值构造整块删除，不需要等价替代——这是 Definition 方案最具体的一项收益，比“消除双注册路径”更容易验证。

**R6（孤儿配置节）已保留，判据按 Key 重写。** `checkOrphanSections` 把「`plugins.<key>` 有配置节却没有对应 Definition Key」判为致命启动错误，专治「写了配置、忘了 import、启动后静默什么也没发生」。`Activation` 让「是否启用」完全由 Definition 加配置决定，因此一个拼错的配置节必须在装配期被明确指出。

它的落点是 `internal/container` 的 expand（§5 生命周期第 2 步）——只有那里同时看得到冻结后的 Catalog 快照和 `config.Environment`。需要改写的是「对应插件」的定义：判据必须比对**冻结快照里的全部 Definition key**，包括 `Activation` 求值为 false、因而没有展开出任何实例的那些。若误用「已展开实例」作为比对基准，`plugins.gorm.enabled: false` 会因为 gorm 没展开而被判成孤儿节，把一个完全正常的关闭动作变成启动失败。

这条要两个方向都钉住：拼错的节名必须致命失败；`enabled: false` 的正确节名必须正常启动。只钉前者的话，上面那个误判基准可以照样通过测试。

普通应用无需接触 `catalog` API；那是插件 module 作者和测试设施的低层 SPI。应用看到的是 import 与 `xbc.Run()`：

```go
import (
    _ "example.com/order-service/internal/order"

    "github.com/xbcio/xbc"
    _ "github.com/xbcio/xbc/web/autoload"
)

func main() { xbc.Run() }
```

这与旧设计的“双注册路径”不同：没有 `App.Register`，所有插件都来自同一种 Definition，启用语义由 Definition + 配置决定，而不是由“它从哪个 API 进来”决定。若某个插件只应在出现配置节时启用，应在 Definition 中声明统一的 `Activation` 条件；不能另造手工注册特例。

代价也必须写清：Go 无法凭 `go.mod` 自动执行未被 import 的包，所以至少需要一个 blank import，或者由应用自己的聚合包统一 import。若未来提供代码生成器，它只能生成这些 import/Definition 声明，不能引入第二套运行时发现协议。对 xbc 这种以插件快速装配为核心的产品，这个受约束的编译期 Catalog 比让每个应用手写实例清单更符合定位。

### 3.4 “分布式/微服务”不是一个包

微服务是部署和组合形态，不是可验证的代码职责，因此不建立 `microservice`、`distributed` 或 `starter` 万能目录。能力按真实 owner 拆分：请求承载归 `web` / `grpc`，协议无关运维聚合归按需建立的 `management/*`，注册发现、配置中心、数据库、缓存和消息系统归对应 `integration/<technology>`，业务编排留在应用自己的插件中。只有多个实现已经证明存在稳定公共契约时，才抽取诸如 discovery 或 messaging 的中立 API。

---

## 4. 每个包的职责边界

| 包/module | 负责 | 明确不负责 |
|---|---|---|
| 根包 `xbc` | 稳定公开 API（`App` / `New` / `Option` / `WithDefinitions` / `App.Execute` / `Run`）**以及**单个 App 的可变状态、bootstrap/execute、生命周期、托管任务、有界关机、私有 settings、RuntimeHost 适配、启动报告的渲染与输出；`process.go` 唯一持有 os.Args/signal/log.Sync/os.Exit | 默认 Catalog 的可变声明、插件实现、Web/gRPC server、业务集成；新增任何导出符号（导出清单由守卫冻结） |
| `plugin` | Plugin/Definition SPI、生命周期 capability（含 `Runner` / `TrafficOpener`）、依赖声明、上下文、值注册表访问、扩展点查询 | 默认目录状态、具体协议类型、容器排序实现、健康端点聚合 |
| `plugin/catalog` | Definition 声明、冻结、校验、确定性 `Snapshot` 及私有测试目录 | 创建 live plugin、读取配置、执行生命周期 |
| `config` | 配置源合并、profile、ENV、绑定、默认值、校验、只读查询 | CLI 子命令、日志初始化、任何具体业务/协议配置节的语义 |
| `log` | Logger/trace 门面与默认实现 | 配置加载、应用生命周期、插件发现、HTTP middleware |
| `internal/container` | Definition 实例展开、插件配置绑定、依赖解析与拓扑序、注入/产物收割、值注册表和初始化状态 | 调用插件 `Init`、生命周期编排/回滚、路由、server、命令解析、进程信号；这些生命周期动作由根包协调 |
| `topology` | 稳定拓扑排序和 cycle/missing 诊断 | Plugin、Middleware 等领域类型 |
| `internal/container/inject` | 反射 tag 机械能力 | 插件生命周期和错误策略 |
| `internal/cli` | 命令行解析：子命令、共享 flag、`XBC_PROFILE` 回退、三个迁移授权的合并 | 加载配置、认识插件、认识根包私有 `settings`（`WantsMigration` 只收 `autoMigrate bool`） |
| `tests/architecture` | 测试态全仓架构守卫，固定根包文件清单与导出清单、进程设施 owner、依赖方向、module manifest 与 API shape | 生产运行逻辑或可复用测试框架 |
| `tests/integration` | 以外部消费者视角验证公开 API 与 `App` 的反射方法集合 | 穿透 internal 做白盒验证 |
| `web`（独立 module） | Gin-backed `Server`（`Runner` + `TrafficOpener` + `Closer`）、路由、不可变路由目录、Web middleware、`plugins.web.*` 配置与自己那段报告 | 核心插件解析、gRPC、服务发现、进程级 shutdown 预算 |
| `examples`（独立 module） | 可运行示例，验证「普通用户视角」的 import 与入口 | 被 core 或 `web` 反向依赖 |

下面三项**本次不创建目录、不创建 `go.mod`、不创建占位 API**，只记录未来归属，避免实现时重新讨论：

| 未来包/module | 负责 | 明确不负责 |
|---|---|---|
| `grpc` | gRPC `Server`、service 注册、interceptor、gRPC 配置 | Web 路由和 Gin 类型 |
| `management/health` | 可选、协议无关的 health contributor 契约与聚合 | 默认强制启用、HTTP/gRPC 端点和具体基础设施检查 |
| `integration/*` | 一项具体基础设施或平台集成 | 通用容器职责和其他无关技术 |

`topology.Graph` 只提供意图明确的两种边：`AddHardEdge(from, to)` 记录一条要求两端最终都存在的边；两个 `Add*Edge` 方法本身都不返回错误，完整端点检查统一延迟到 `Sort()`。硬边缺端点时 `Sort()` 返回 `*MissingNodeError`；`AddSoftEdge(from, to)` 的可选端点缺失不阻断排序，而在 `Sort` 结果中以 `Miss` 报告。`Miss.Dir` 使用类型化 `Direction`（`After` / `Before`），不再用裸字符串或一个含义模糊的 `AddEdge(..., hard bool)`。

命令行解析不放进 `config`：`migrate`、`doctor` 是应用命令，`--config`/`--profile` 只是命令参数中的一部分，把整个解析器叫配置会掩盖真实职责。它最终落在 `internal/cli`，因为它是一个自足的域：只读 `os.Getenv` 与 `flag`，返回一个值，不知道根包存在。根包持有解析出来的 `Command` 当作纯数据，自己决定拿它做什么。

核心配置模型**留在根包并保持未导出**：`settings` 只承载 `xbc.shutdown_timeout` 与 `xbc.auto_migrate`，不是 `xbc.Settings` 公开 API——它现在和公开的 `App` 同处一个包，只靠首字母小写保持私有，由 §9 约束 15 的导出清单守卫钉住。`internal/cli.Command.WantsMigration` 只接收 `autoMigrate bool`，避免 cli 反向认识根包，也准确表达三份迁移授权中“部署配置”这一份本来就只是一个布尔。

启动报告的渲染与输出**不拆包**，只拆文件内的规则。曾经拆出过 `internal/startupreport`，理由是「渲染职责与测试隔离」；渲染职责是真的，测试隔离是假的（见 §2.1）。现在 `startup_report.go` 一个文件同时持有两半，规则写在文件头：所有 `render*` 函数返回字符串、不做 I/O，往哪个 logger 写、哪几段该出现只由 `(*App).report` 决定。

启动报告留在拥有数据的模块：core 在根包 `startup_report.go` 报告核心状态，`web` 模块报告路由/中间件。不要建立一个读取所有内部状态的 `startup` 万能包——core 的报告只认识 core 自己的 `Instance`，跨不到 `web` 那边去。

**多段启动报告是这次解耦的真实代价，必须承认而不是回避。** 重构前的旧实现按 `assembleHTTP` → `printStartupLog` → `startRunners` 输出一整块启动日志；当前落地已把 middleware 排序、路由注册和 Web 报告移进 `web.Server.Start`（§5.1）。因此核心报告在 Init/migrate 完成后、所有 `Runner.Start` 之前输出，`web` 报告在 Web 的 `Start` 内输出；将来同时启用 gRPC 时还会有第三段，各段之间可能夹着拓扑序中其他插件自己的 Start 输出。

接受这个代价的理由是：**报告的连贯性不值得用「core 反向认识每个运行栈的内部状态」去换。** 要把三段合成一段，core 就必须持有一个所有运行栈都往里写的报告缓冲区，那正是上面刚否决掉的 `startup` 万能包，只是换了个名字。

缓解手段限定在渲染层，不动数据归属：

- 各段共用同一套表头、缩进和字段宽度，读起来是同一份报告的连续章节，而不是三种格式拼在一起；
- 各段带稳定段标题（如 `xbc: 装配完成`、`web: 路由与中间件`），日志采集端可按标题聚合；
- core 的报告固定排在**所有 `Start` 之前**，保证第一段永远是「这个二进制装配出了什么」，读者据此就能预期后面还会出现哪几段。

明确不做的是：**不给 core 加「等所有运行栈报告完再统一输出」的屏障。** 那会把整份启动日志推迟到最后一个 Runner 之后，恰好丢掉启动日志最有价值的场景——某个插件的 `Start` 挂住时，前面的报告应该已经打出来了，而不是跟着一起卡在缓冲区里。

`HealthChecker` 也不放进稳定 core SPI：健康状态的聚合维度和结果模型属于可选运维能力。需求稳定后由 `management/health` 定义 `Contributor` 并通过 `plugin.Extensions` 聚合；HTTP、gRPC health checking protocol 或独立管理端口由单独适配器暴露。这样基础设施只实现一份协议无关检查，core 和运行栈都不需要反向认识它。

---

## 5. 协议无关生命周期

生命周期分成“可嵌入应用执行”和“进程适配”两层。公开 `App.Execute(ctx, args) (int, error)` 与包级 `Run` 共用同一条内部执行流程，它负责：

```text
0.  parse explicit args
1.  load environment，初始化日志，绑定私有 xbc settings，建立 task runtime/container
2.  evaluate activation and expand enabled instances
3.  bind and validate plugin config
4.  resolve dependencies and stable order
5.  doctor：只报告装配结果后返回；否则继续
6.  inject, initialize, harvest provided values
7.  migrate（仅 migrate 命令或显式启用）
8.  migrate 子命令：完整 unwind 后返回；否则继续
9.  输出 core 报告
10. readiness phase 1：按拓扑序串行 Runner.Start（prepare / bind）
11. readiness phase 2：按拓扑序串行 TrafficOpener.OpenTraffic（开放流量）
12. assert liveness，然后等待 caller context / process adapter / critical failure 请求停止
13. bounded unwind in reverse dependency order
```

选定 Definition 集在 `New` 返回前已经冻结：未传私有 Snapshot 时由 `New` 冻结默认 Catalog，传入 `WithDefinitions` 时则直接接收调用方已经冻结的 Snapshot。这个阶段早于 `Execute`，不会调用 Factory 或执行 I/O。核心只认识通用生命周期接口，不再硬编码“组装中间件、注册路由、启动 HTTP”三个 Web 阶段。`Runner.Start` 按拓扑序**串行**调用并必须快速返回；监听循环、consumer loop 等长任务通过 `Context.GoCritical` 托管。串行换来确定的依赖启动顺序和精确的“已启动集合”。

`App.Execute(ctx, args) (int, error)` 只使用调用方明确传入的参数和 context：它不读取 `os.Args`，不订阅 OS signal，不调用 `log.Sync`，也不调用 `os.Exit`。每个 App 是单次执行对象，`Execute`（包括包级 `Run` 进入的内部同一执行路径）至多成功进入一次，重复调用返回 code 1 和错误。包级 `Run()` 才是进程入口：`run.go` 创建 App，再委托 `process.go` 注册 SIGINT/SIGTERM、进入共用执行流程、输出诊断、flush 全局日志并按退出码终止进程。这样嵌入者保留进程控制权，普通 `main` 仍只需一行 `xbc.Run()`。

### 5.0 进程信号只属于包级 `Run`，且早于第一个 Init

旧实现直到 HTTP serve 阶段才调用 `signal.Notify`，使耗时 Init 期间的 Ctrl-C 可能直接终止进程，已经初始化的资源来不及统一 Stop。最终实现把唯一的 OS-signal 订阅放在根包 `process.go` 的 `executeWithSignals`：它在进入内部执行流程之前完成注册，所以肯定早于第一个插件 Init/Start。`New` 在此之前只冻结静态 Definition，不创建插件或资源。

`App.Execute` **从不**注册 signal；嵌入调用方通过取消传入的 context 请求停止。两条入口最终都调用同一个 `requestStop`/unwind 路径，但进程设施的 owner 不混淆：signal、`os.Args`、`log.Sync`、`os.Exit` 只属于根包的 `process.go`，caller context 只属于 `Execute`。这条在 runtime 并入根包后靠守卫维持：`process.go` 不再有包边界围着，同包兄弟文件伸手就能拿到包级 `osExit`。

启动期中断语义固定如下：

- `*plugin.Context` 实现标准 `context.Context`：`Deadline`/`Value` 继承 `App.Execute` 的 caller context，`Done`/`Err` 同时反映 caller 取消、进程 signal 与 critical stop；`requestStop` 首先关闭这份 execution-scoped lifecycle context；
- Init/Migrate/Start/OpenTraffic 仍在 Execute goroutine 上同步调用。可能阻塞的 hook **必须协作取消**：监听 `ctx.Done()` / `ctx.Err()`，或把 `ctx` 继续传给 context-aware 依赖；收到停止后尽快返回；
- 框架不会把 hook 丢进 detached goroutine 试图强制中断。若 hook 拒绝协作，框架无法安全抢占它；相应地 unwind 也不会越过尚未返回的 hook，`Stop` 绝不与同一实例仍在运行的启动 hook 并发；
- Init 中收到 signal/context/critical：协作 hook 返回后不再执行后续插件，并回滚所有已 Init 成功的实例；Start/OpenTraffic 同理停止后续启动，所有已 Init 成功的实例仍按逆拓扑序 unwind；
- signal、caller context、critical 与阶段自身错误复用同一套清理实现，不复制回滚逻辑；步骤之间仍检查停止状态，防止已经返回的 hook 之后继续启动；
- **启动尚未完成时**收到 signal 或 caller context，返回 code 1；critical 与任意启动失败同样返回 1；参数用法错误返回 2；
- **完成启动并进入等待后**，signal 或 caller context 属于主动停止，unwind 成功返回 0；critical 或 unwind 失败返回 1。

这里的可取消性是“同步 hook + 标准 context 的协作取消”，不是框架抢占 goroutine。同步调用何时真正返回仍由插件负责，但 Context 不再只允许在步骤之间观察停止。

### 5.0.1 动态 callback 与严格 typed config 边界

插件提供的 callback 都是不可信扩展代码，必须在 framework-owned 边界同步调用并统一校验：`Factory`、`ConfigPtr`、`Dependencies`、`Provides` 以及 `Init` / `Migrate` / `Start` / `OpenTraffic` 等 hook 的 panic 都要 recover 成普通错误，诊断至少包含 Definition key、实例、callback/hook 名与 stack。Factory 返回 nil（包括 interface 中的 typed nil）、`ConfigPtr()` 不是非 nil struct 指针、`Dep` / `Ref` 的 Type、Key、instance 或 Optional 等字段非法，都在装配边界失败，不能让非法值进入反射、拓扑图或注册表后才 panic。生命周期 callback 失败统一进入 abort/unwind；callback 保持同步，确保 `Stop` 不与尚未返回的 hook 竞态。

插件配置采用严格 typed schema：实现 `Configurable` 时，`ConfigPtr()` 必须返回非 nil 的 struct 指针；已声明的 typed section 出现未知字段必须报出完整相对/绝对路径，不能静默忽略拼写错误后回落到默认值。`enabled` 等框架保留键由 container 通过窄 `AllowedKeys` 排除，不能迫使每个插件在自己的 config struct 重复声明，也不能借 allowlist 隐藏其他未知字段。`app.*` 以及配置类型显式声明的 map/interface 边界属于自由格式区域，可以保持开放；其余 struct、struct slice 和 map value 按 schema 递归严格检查。

schema 生成、default/ENV 路径、unknown-field 检查、回写与 validation 必须共享同一套字段 walker 和 YAML path 映射，避免同一字段在不同阶段得到不同名字。受支持的嵌套指针按需分配；无法安全统一解释的指针层级、递归 struct 或非法 `yaml:",inline"` 形态必须明确报错，不能 panic，也不能退化成宽松绑定。

### 5.1 协议无关的两阶段 readiness

单阶段 `Start` 有一个无法用排序修补的缺陷：**A 一 `Start` 就开始收流量，而它依赖的 B 可能还没 `Start`**。拓扑序只能保证「B 在 A 之前」，管不了「A 之后还有 C 没起来」——一个 Web server 排在拓扑序中间时，它开始接受请求的那一刻，排在它后面的消息消费者、定时任务、二级缓存全都还没启动。压测和滚动发布里，这表现为进程刚起来的头几十毫秒返回 5xx 或读到空缓存。

修法不是给 Web 特判「你最后启动」——那要求 core 认识 `web` 这个名字，正是本设计要消灭的东西；也不是靠名称排序或让用户手写 `After`，那是把框架的时序缺陷转嫁给每个应用。修法是把启动拆成两个**全局屏障**分隔的阶段：

```go
// Runner is readiness phase 1: acquire resources and bind endpoints.
// Start must return quickly; long-lived loops go through Context.GoCritical.
// After Start returns, the plugin must be fully prepared but must NOT yet
// accept or produce traffic.
type Runner interface{ Start(ctx *Context) error }

// TrafficOpener is readiness phase 2: begin accepting traffic. Core calls it
// only after every Runner in the application has completed Start
// successfully, so a plugin that opens here can rely on the whole
// application being prepared -- not just its own dependencies.
type TrafficOpener interface{ OpenTraffic(ctx *Context) error }
```

语义固定：

1. core 先按拓扑序串行调用**全部** `Runner.Start`；任一失败即中止并 unwind，不进入 phase 2；
2. 全部 `Start` 成功后，core 才按拓扑序串行调用**全部** `TrafficOpener.OpenTraffic`；
3. 两个接口互相独立：只实现 `Runner` 的插件（如一次性预热）合法，只实现 `TrafficOpener` 的插件也合法；
4. core 对这两个接口**只做类型断言**，不认识 `web`、`grpc` 或任何插件名，不做名称排序，不维护特例表；
5. `OpenTraffic` 同样必须快速返回，长任务仍走 `GoCritical`。

`web.Server` 的落点因此是明确的：`Start` 里 `net.Listen`（端口已 bind、`Addr()` 可读回、连接会进内核 accept 队列但无人 accept），`OpenTraffic` 里才把 `http.Server.Serve(listener)` 交给 `ctx.GoCritical`。gRPC 将来同理：`Start` 建 listener 并注册 service，`OpenTraffic` 调 `grpc.Server.Serve`。core 两边都不需要知道。

**为什么 bind 放 phase 1 而不是 phase 2**：bind 是最容易失败的一步（端口占用、权限不足、地址非法），而它失败时我们希望**没有任何插件已经在对外服务**。放在 phase 1 意味着端口冲突在任何流量开放之前就把启动打回；放在 phase 2 则意味着前面的插件可能已经开始消费消息，然后才发现 Web 端口起不来，unwind 要撤销已经产生的副作用。

**代价必须写明**：`Start` 返回到 `OpenTraffic` 被调用之间，端口是 bind 状态但无人 accept。客户端此时连接会被内核放进 backlog 队列而不是被拒绝，看起来像“连上了但没响应”。这个窗口的长度等于「其余插件 `Start` 的总耗时 + 前面插件 `OpenTraffic` 的耗时」，因为 `Start`/`OpenTraffic` 都被要求快速返回，这个窗口是毫秒级的，用它换掉「启动期 5xx」是划算的。真正需要“未就绪就明确拒绝”的场景应由 readiness 探针负责，那是将来 `management/health` 的职责，不是 core 的。

### 5.2 存活能力校验：宁可启动失败，不要永久空等

两阶段 readiness 完成后会进入等待。如果这个二进制**根本没有任何长期存活能力**——Catalog 是空的，或者所有启用的插件里没有一个实现 `Runner`/`TrafficOpener`、task runtime 也从未接纳过任务——那么这次阻塞不是“在服务”，而是一个什么都不做、只等着被停止的空进程。它最典型的成因恰恰是配置或 import 出了错：忘了 `_ "github.com/xbcio/xbc/web/autoload"`，或者配置把唯一的运行栈 `enabled: false` 了。让它静默挂起，等于把一个启动错误伪装成一个健康的常驻进程。

因此 core 在进入等待之前做一次显式断言：

- **冻结后的 Catalog 快照为空**（一个 Definition 都没有）→ 立即启动失败，提示大概率漏了 blank import；
- readiness 完成后，若**没有任何实例实现 `Runner` 或 `TrafficOpener`，且 task runtime 从未接纳任务** → 启动失败，错误里列出当前装配出的实例清单，让人一眼看出「装是装上了，但没有一个能跑的」。

托管任务算存活能力，是为了不误伤合法形态：一个纯后台任务型应用可以完全不实现 `Runner`，只在 `Init` 里 `ctx.GoCritical` 起一个消费循环——那是正常的常驻服务，必须放行。判据因此是「有能力长期运行」，不是「实现了某个特定接口」。

`doctor` 和 `migrate` 子命令不受这条约束：它们本来就是跑完即退的一次性命令，永远不会进入常驻等待。

### 5.3 关机：一个永远阻塞的 Stop 不得拖死整个进程

信号、critical failure 或部分启动失败时，核心对**已经 Init 成功**的插件做一次统一 unwind，并为每个插件维护独立的托管任务 context。框架按逆拓扑序处理每个插件：先在该插件任务仍可运行时调用一次 `Closer.Stop`，随后取消该插件的任务 context，并在本轮统一 shutdown deadline 内等待其托管任务退出，再处理它依赖的下一个插件。没有 `Closer` 的插件直接取消并等待；若某一步耗尽总 deadline，立即强制取消所有尚未取消的插件 context，然后继续尝试其余 `Stop`，但不再无限等待。

`Stop` 必须用传入的剩余 deadline 主动完成自己的 graceful stop，不能反过来等待框架先取消同一个插件 context。`web.Server` 因而在 `Stop` 内停止接收并 drain，consumer 在 `Stop` 内停止拉取；core 不需要识别任何协议。每个 `Closer` 最多调用一次，panic、超时与多个 Stop 错误聚合记录，但不能阻断后续依赖的清理。

**但「必须用 deadline 主动裁剪」是一条对插件作者的要求，不是一条框架能依赖的保证。** 一个写错的 `Stop`（`select {}`、忘了 select `ctx.Done()` 的 channel 接收、一次没有超时的同步网络调用）会让 `closer.Stop(ctx)` 这行调用**永远不返回**。旧实现里这等于整个进程挂死在关机路径上，`shutdown_timeout` 完全不起作用——因为超时只体现在传给插件的 context 上，而框架自己是在**同步等这次调用返回**。运维承诺的「进程 N 秒内消失」被一个插件单方面作废了。

因此 core 的 unwind 必须自己有界，不能把有界性外包给插件。四条机制合起来保证 `shutdown` 一定在有限时间内返回：

1. **受控 goroutine**：每次 `Stop` 都在独立 goroutine 里调用，主流程 `select` 该 goroutine 的完成 channel 与共享 deadline 两者，**先到先算**。deadline 先到时主流程继续处理下一个插件，那个卡住的 goroutine 被**故意泄漏**——这是清醒的取舍，见下；
2. **panic recover**：受控 goroutine 内 `recover`，把 panic 转成一条错误记入聚合，绝不让它穿透到框架的 goroutine 栈上炸掉整个进程；
3. **共享 deadline**：整轮 unwind 共用一个 `shutdown_timeout` 预算，而不是每插件一份，理由见下；
4. **错误聚合**：`Stop` 返回的 error、panic、超时三类结果全部收进一个聚合错误，一次性上报，不因为任何一类而中断后续插件的清理。

**泄漏一个卡死的 goroutine 是有意的。** Go 没有安全终止 goroutine 的手段；能选的只有「泄漏它，进程按时退出」和「等它，进程永不退出」。进程马上就要 exit 了，泄漏的代价是零；不泄漏的代价是运维承诺失效。所以这里不是「暂时先这样」，是最终答案。

**shutdown deadline 是共享预算，这是一个有代价的选择。** 上面的"本轮统一 deadline"意味着逆拓扑序上靠前的插件先花预算，靠后的插件只能拿到剩余量。极端情况下第一个插件耗尽全部预算，后面每个插件的 `Stop` 都拿到一个已过期的 context，只能立即返回——**一个迟钝的插件有能力剥夺它所有下游依赖的 graceful stop 机会**。

这是相对重构前旧实现（全局 `wg.Wait()` 无超时 + 逆序 `rollback`）的一次权衡反转：旧行为可能挂死但不会连累别人，当前行为保证有界但会传播损失。选共享预算而不是"每插件均分子预算"的理由是：`shutdown_timeout` 是运维对**整个进程**多久必须消失的承诺，均分子预算会让实际总时长变成 `N × 子预算`，破坏这个承诺，而 N 随插件数量变化、运维无法预估。代价则通过两条约束控制：

- `Stop` 收到的 context 已经带着剩余 deadline，插件有义务据此裁剪自己的 drain 时长，而不是无视 context 干满；
- 预算耗尽时框架不是放弃清理，而是降级为"强制 cancel 全部 + 继续调用剩余 `Stop` 但不等待"，保证每个 `Closer` 仍被调用一次。

若将来出现"某个插件反复吃光预算"的实际案例，再考虑给单个插件加上限（如 `min(剩余, shutdown_timeout/2)`），而不是现在就引入一个没有真实数据支撑的分配策略。

**`shutdown_timeout` 属于 core，不属于任何运行栈。** 它回答的是「这个**进程**多久必须消失」，而不是「HTTP 连接 drain 多久」。旧实现把它放在 `server.shutdown_timeout`，等于让 Web 配置节决定一个 gRPC-only 或纯任务型应用的关机预算——那个应用连 `server` 节都不该有。新落点是核心配置节 `xbc.shutdown_timeout`（默认 `30s`），与 `xbc.auto_migrate` 同属 core 自己的 `xbc.*` 命名空间。`web` 不再拥有自己的 shutdown 配置项：它的 drain 时长来自 core 通过 `Stop(ctx)` 传进去的剩余 deadline，这正是 §5.3 要求每个插件遵守的那条约束。

### 5.4 托管任务组：先关准入，再 cancel/wait

托管任务组的关闭有一个经典竞态：`sync.WaitGroup` 的 `Add` 与 `Wait` 并发调用是未定义行为。关机流程走到 `wg.Wait()` 的同时，某个还在运行的托管 goroutine（或某个正在返回的 `Stop`）调用了 `ctx.Go`，就会命中 `WaitGroup misuse: Add called concurrently with Wait` 的 panic，或者更糟——`Wait` 提前返回，框架以为任务都退干净了，实际上刚提交的那个还在跑，随后 `Stop` 把它脚下的资源撤了。

修法是给任务组一个显式的**准入开关**，关机三步走严格分开：

1. **关闭准入**（一次性、幂等）：此后任何 `Go`/`GoCritical` 都不再启动 goroutine；
2. **cancel** 对应的任务 context；
3. **wait** 已准入的任务退出（带 §5.3 的共享 deadline）。

只有第 1 步完成之后才允许进入第 2、3 步，于是 `Add` 与 `Wait` 之间有了一个 happens-before 屏障，竞态在结构上消失，而不是靠“没人会在关机时提交任务”的约定。

**准入关闭后提交的任务必须被明确拒绝，不能静默丢弃。** 静默丢弃会让一个「关机时顺手起个 goroutine 上报最终状态」的插件以为自己上报了。`Go`/`GoCritical` 本身没有错误返回（它们在 `Init` 里被调用，调用点通常不检查返回值），所以拒绝的表达方式是**一条 WARN 日志**，写明插件身份和「任务组已关闭，该任务未启动」。同时提供 `Context.TasksAccepted() bool` 供确实需要分支的插件查询。

### 5.5 `GoCritical` 的“意外返回”使用应用级关机状态

**`GoCritical` 的“意外返回”判据必须是应用级的，不能用插件自己的 context。** 重构前的 `goroutine.go` 用 App 级 `runCtx` 是否取消（`expected = a.runCtx.Err() != nil`）判断，但实际效果仍与下面的逐插件 unwind 顺序**直接冲突**：

关机流程要求"先在该插件任务仍可运行时调用一次 `Stop`，随后才取消该插件的任务 context"。于是一个 consumer 插件在 `Stop` 内停止拉取 → 它的托管 goroutine 正常返回 → 但此刻该插件的任务 context **尚未取消** → 判据认为"没人要求它停，它却停了" → 触发一次虚假的 critical failure，还会把退出码置为 1。一次完全正常的关机会被报成异常关机。

当前 `taskRuntime` 已以原子 `shuttingDown` 表示**应用级“关机已开始”**：`closeAdmission` 在进入 unwind 的第一时刻置位，早于任何 `Closer.Stop` 调用；信号、critical 和启动失败都汇入同一个 unwind 路径。插件自己的任务 context 只负责“通知这一个插件的托管任务退出”，不再兼任“这次返回是否合法”的判据。

这条必须有钉子测试：一个在 `Stop` 内主动让自己的 `GoCritical` goroutine 返回的假插件，走完整关机流程后退出码必须是 0，且不得产生 critical 日志。

### 5.6 运行栈如何发现贡献者

`plugin` 提供只读、确定顺序并保留来源身份的扩展点查询：

```go
type Identity struct {
    Plugin   Key
    Instance string
}

type Extension[T any] struct {
    Identity Identity
    Value    T
}

func Extensions[T any](ctx *Context) ([]Extension[T], error)
```

语义必须固定：

- 只返回已启用且已完成 `Init` 的插件实例；
- 按核心依赖拓扑序返回，返回 slice 是快照；
- `Identity` 是不可变值，用于限定中间件名、统计路由归属和诊断，不暴露别的插件 Context；
- `T` 通常是某个运行栈定义的 capability interface；传入具体类型返回错误；
- 容器通过已初始化插件实例对 `T` 做 assignability 判断，不要求业务插件预先向值注册表登记自己；
- 它查询的是插件能力，不与 `Get[T]` 查询“插件产物值”混为一谈；
- 仅允许在全部插件完成 `Init` 后调用；Init 阶段调用返回错误，不能给出“只初始化到当前为止”的不完整快照。

`web.Server` 在自己的 `Start` 中查询并传播错误：

```go
// 这段代码在 web 包内部，因此 capability 接口不带 web. 前缀。
middlewares, err := plugin.Extensions[MiddlewareProvider](ctx)
if err != nil { return err }
routes, err := plugin.Extensions[RouteProvider](ctx)
if err != nil { return err }
consumers, err := plugin.Extensions[RouteCatalogConsumer](ctx)
if err != nil { return err }
```

运行栈用每个 `Extension.Identity` 生成稳定限定名和启动报告，调用能力时使用 `Extension.Value`；不得为获得来源信息下探 `internal/container`。

然后自行完成 middleware 排序、路由注册、路由冻结、监听和 Web 报告。路由冻结后的接口固定为：

```go
type RouteCatalog interface {
    All() []RouteInfo
    Lookup(method, path string) (RouteInfo, bool)
}

type RouteCatalogConsumer interface {
    RoutesReady(routes RouteCatalog) error
}
```

`All` 返回防御性副本，`Lookup` 返回值而非内部指针。请求期统一调用 `web.CurrentRoute(*gin.Context) (RouteInfo, bool)`：运行栈最外层的内部 middleware 将匹配结果写入 Gin context，该查询不依赖 `plugin.Context`，也不暴露可变路由表。

`grpc.Server` 同理查询 `ServiceProvider` 与 interceptor provider。由于所有插件的 `Init` 都先于任何 `Runner.Start`，运行栈看到的是完整且稳定的扩展集合。

这使一个业务插件可以同时实现多种能力，而核心不需要知道这些接口存在：

```go
type Plugin struct{ plugin.Base }

func (p *Plugin) RegisterRoutes(r *web.Router) { /* ... */ }
func (p *Plugin) RegisterServices(r grpc.ServiceRegistrar) { /* ... */ }

// 编译期接口断言让方法拼写错误立即暴露，而不是运行时静默缺少能力。
var _ web.RouteProvider = (*Plugin)(nil)
var _ grpc.ServiceProvider = (*Plugin)(nil)
```

### 5.7 `plugin.Context` 必须保持中立

`plugin.Context` 只暴露：

- Definition Key 与实例名；`Key()` 是身份 API，`Name()` 只是 Key 的文本便利表示；
- `Config() config.View`，而不是可绑定、可变的 `*config.Environment`；
- 预绑定 `log.Logger`；
- `Go` / `GoCritical` / `TasksAccepted`；
- `Provide` / `Get` 与 `Extensions` 所需的窄端口。

`Context.Config()` 返回的是**该实例作用域内**的 `config.View`，不是全局 Environment：单实例根为 `plugins.<key>`，多实例根为 `plugins.<key>.<instance>`。插件使用相对路径读取，例如 `ctx.Config().Get("addr")`、`Sub("tls")`；空路径表示本实例配置根。返回的 view 不能通过 `..`、绝对路径或重新拼接前缀越过作用域，因此不能读取其他插件、其他实例或 `xbc.*` / `log.*` 等 core owner 配置。

`config.View` 只有 `Get`、`Exists`、`Sub`。`Get`/`Sub` 在每次读取时递归复制 map 与 slice（包括嵌套集合），因此插件修改返回值不会污染 Environment 或其他插件之后的读取；`Bind` 只留给框架和配置 owner，不从 Context 暴露。`*plugin.Context` 同时实现标准 `context.Context` 的四个方法，使同步启动 hook 能协作观察 execution-scoped 取消，但这不会给插件增加宿主或协议访问权。现有 `Context.Route` / `Context.Routes` 删除：请求期查询归 `web.CurrentRoute`，全量查询归 `web.RouteCatalog`，否则只写 gRPC 的插件也会被迫依赖 Gin。

### 5.8 `Context` 与容器通过 `RuntimeHost` 端口连接

`plugin.Context` 不保存 `*xbc.App`。由于 Go 包边界，`plugin` 导出最小的 `RuntimeHost` 协作端口，根包的 `hostAdapter` 把调用转发给 `internal/container` 和根包的托管任务运行时。`RuntimeHost` 是框架装配 Context 的低层 SPI，不是普通插件获取完整宿主或替换 App 的入口：

```text
plugin.Context ──► plugin.RuntimeHost ◄── xbc.hostAdapter ──► internal/container / task runtime
```

`RuntimeHost` 只承载 lookup/provide/extensions/task 等机械能力，不得出现 Router、Gin、gRPC 或完整 App。它是全篇唯一一个“框架实现、SPI 定义”的反向接口，也最容易膨胀成 God interface，因此方法集固定为：

```go
package plugin

// RuntimeHost 由框架实现、plugin 包消费；普通插件不会直接持有它。
type RuntimeHost interface {
    ProvideValue(typ reflect.Type, instance string, v any)
    LookupValue(typ reflect.Type, instance string) (any, error)
    InitializedPlugins() ([]Extension[any], error)
    GoManaged(id Identity, fn func(context.Context), critical bool)
}
```

四个方法，没有第五个。新增方法的前提是证明它无法由现有四个组合得出；“这样写更方便”不是理由。`InitializedPlugins` 虽返回多个实例，仍不泄漏 Context、绑定配置、Deps、provided values、init 状态或任何 `internal/container.Instance` 字段。`plugin` 只用动态值做 capability assignability 判断，容器内部结构不进入公开 API。`NewRuntimeContext`、`BindRuntimeContext` 与 `BindLifecycleContext` 都是框架装配入口：前两者在实例展开时构造 Context 并按需绑定 Base，后者在 assembly 完成后、首个启动 hook 前绑定 execution-scoped context；普通插件只接收 Context，不调用这些入口。插件未嵌入 Base 时 `BindRuntimeContext` 返回 false，这是合法形态而不是错误。

架构守卫（§9 第 7 条）验证的正是这条边界：运行栈只能通过 `plugin.Extensions` 拿到实例值，任何可选 module 都不得直接 import `internal/container` 读取 Instance。

---

## 6. 依赖方向

箭头表示“可以 import”：

```text
config                       （与 log 互不依赖）
log                          （与 config 互不依赖）

plugin ───────────────────► config, log
plugin/catalog ────────────► plugin

topology                              （stdlib only）
internal/container/inject             （stdlib only）
internal/cli                          （stdlib only）
internal/container ────────► plugin, plugin/catalog, config, log, topology, internal/container/inject

xbc(root) ─────────────────► plugin, plugin/catalog, config, log, topology, internal/cli, internal/container

web ───────────────────────► plugin, config, log, topology, gin
web/autoload ──────────────► web, plugin/catalog
grpc（将来）───────────────► plugin, config, log, google.golang.org/grpc
management/* ──────────────► plugin, config, log + 自己的能力依赖
integration/* ─────────────► plugin, config, log + 自己的技术依赖
```

硬约束：

1. 根包除标准库外只直接 import `plugin`、`plugin/catalog`、`config`、`log`、`topology`、`internal/cli` 与 `internal/container`；尤其不得 import `web`、`grpc`、`management/*`、`integration/*`、Gin 或 Google gRPC。根包同时是公开 API 与运行内核，"是否公开" 不再由包边界保证，只差一个首字母大小写，因此导出符号清单由 §9.2 的守卫单独冻结；
2. `plugin` 不得 import 根包、`plugin/catalog`、`internal/*`、`web` 或 `grpc`；`plugin/catalog` 只向下依赖 `plugin`；
3. `config` 与 `log` 彼此独立，且都不得 import 根包、plugin、`web` 或 `grpc`；
4. `internal/container` 只消费冻结后的 Definition，不得读取默认 Catalog 的可变状态，也不得 import 根包或运行栈；
5. 普通 `web` 实现包不依赖 `plugin/catalog`，只暴露无副作用 `Definition()`；只有显式 `web/autoload` 依赖 catalog 并注册默认 Definition。将来的 grpc/management/integration 也应采用“实现包 + 按需 autoload”的同一分层，不得反向要求 core 了解具体实现；
6. core 下层库包与可选运行栈不得为了类型别名反向 import 根包；canonical 类型必须从 owner 包导入。应用可执行入口当然可以 import 根包并调用 `xbc.Run()`，这正是公开门面的用途；
7. core 的 `go list -deps github.com/xbcio/xbc` 中不得出现 Gin、Google gRPC 或任一可选 module；
8. **任何 `internal/*` 子包都不得 import 根包**。根包 import `internal/cli` 与 `internal/container`；反向那条边一旦出现就是 import cycle，Go 编译期直接拒绝——所以这条不需要额外守卫，但需要写下来：它约束的是**设计动作**，即「为了少改一处调用点，把根包的类型引下来」这种念头。正确的做法是收窄参数（`WantsMigration(autoMigrate bool)` 就是这么来的，见 §4），而不是让下层去认识上层的类型。

`config.Environment` 只保存和绑定配置值，不持有 `log.Config` 字段，也不调用 `log.Init`。根包的 `bootstrap.go` 从环境的 `log` 路径绑定出 `log.Config` 后再显式初始化日志，避免一个“读取配置”函数产生全局副作用。Web 也不得绕过 owner 去读 `log.level`：Gin mode 由 `ctx.Log().Enabled(log.DebugLevel)` 这类 logger capability 决定；debug 可输出时使用 Gin debug mode，否则使用 release mode。这样 Web 只依赖日志门面，不对 core 拥有的配置键建立 import graph 看不见的运行时耦合。保持 `log` 作为单包是当前迁移范围；若其 tracing/OTel 能力未来独立演进，再依据真实消费者拆成 sibling package，不能为目录对称提前拆包。

---

## 7. 公开 API 归属

| API | canonical owner |
|---|---|
| `App` / `New(...Option) (*App, error)` / `Run()` / `(*App).Execute(context.Context, []string) (int, error)` / `Option` / `WithDefinitions` | `xbc` |
| 私有核心配置 `settings` 与 `xbc.*`（`shutdown_timeout` / `auto_migrate`） | 根包 `settings.go`（不导出 `Settings`） |
| marker `Plugin` / 可选 `Base` / `Context` / `RuntimeHost` / `Identity` / `Extension[T]`；框架装配 API `NewRuntimeContext` / `BindRuntimeContext` / `BindLifecycleContext` | `plugin` |
| `Key` / `Definition` / `Factory` / `Cardinality` / `SingleInstance` / `MultipleInstances` / `Activation` | `plugin` |
| `Catalog` / `Snapshot` / `New` / `Declare` / `Freeze` / `Snapshot.Lookup` | `plugin/catalog` |
| 通用 lifecycle capability：`Configurable` / `Initializer` / `Migrator` / `Runner` / `TrafficOpener` / `Closer` / `Declarer` / `Provider` | `plugin` |
| `Dep` / `Ref` / `Need` / `Opt` / `Offer` / `RefTo` / `Key.Ref()` | `plugin` |
| `Provide` / `Get` / `GetNamed` / `MustGet` / `MustGetNamed` / `Extensions` | `plugin` |
| `Environment` / 只读 `View` / `Options` / source options / `Validate` | `config` |
| `Logger` / `Trace` | `log` |
| `Graph` / `AddHardEdge` / `AddSoftEdge` / `Direction` / `After` / `Before` / `Miss` / topology errors | `topology` |
| `Server` / `Router` / `RouteInfo` / `RouteCatalog` / `CurrentRoute` | `web` |
| HTTP `Middleware` / `Phase` / `RouteProvider` / `MiddlewareProvider` / `RouteCatalogConsumer` | `web` |
| HTTP server config（`addr` / `base_path` / `read_timeout` / `write_timeout`） | `web` |
| `Server` / gRPC service/interceptor capability / gRPC server config | `grpc`（实现时建立） |
| health contributor/聚合契约 | `management/health`（实现时建立） |

`Plugin` 不承担身份方法；身份由 `Key` 单点拥有。`Base`、`Context.Name()` 等便利 API 不改变这一点。`Definition.Instances` 是 cardinality 的唯一入口，依赖引用只接受 Key；这三条避免身份、实例形态和依赖边在运行时对象上出现第二来源。

`HealthChecker` 已从核心 SPI 中**删除**，不保留、不迁移：健康状态的聚合维度和结果模型属于可选运维能力，在 `management/health` 存在之前，core 里一个没有任何消费者的 `HealthChecker` 接口只会诱导插件去实现一个永远不会被调用的方法。启动报告里的 `health` 能力标记一并删除。

`config.Config` 改为 `config.Environment`，避免 `config.Config` 重复命名，也准确表达“多配置源合并后的运行环境”。仓库当前没有发布 tag，v1 前直接删除根包 `Config`，不建立第二个长期入口。

根包的 `ServerConfig` 整体拆解：`addr` / `base_path` / `read_timeout` / `write_timeout` 归 `web` 的 `plugins.web.*` 配置节，`shutdown_timeout` / `auto_migrate` 归 core 的 `xbc.*` 配置节。旧的 `server.*` 顶层节**不再保留兼容读取**——保留它意味着 core 必须认识一个 Web 语义的配置节名，正是本设计要消灭的耦合。保留字顶层节因此变为 `xbc.`、`log.`、`plugins.`、`app.`。

根包也不永久重导出 `Plugin`、`Base`、`Dep`、`Context` 等 SPI alias；插件作者直接 import canonical owner `plugin`。alias 虽然不会引入 Gin，也会制造两套文档入口并模糊 owner。当前无发布 tag，直接迁移；若未来已有外部用户，兼容 alias 必须标记 Deprecated、写明删除版本，不能进入目标布局。

HTTP 类型更不能在根包保留 alias：只要 `xbc.Router` alias 到 `web.Router`，core 就必须重新依赖 Gin，直接破坏本设计最重要的边界。若已有外部用户，则通过明确的 breaking release 和迁移文档处理，而不是永久污染 core。

---

## 8. 现有文件迁移映射

| 旧文件/目录 | 最终落点 | 说明 |
|---|---|---|
| `xbc.go` | 根 `app.go` / `run.go` / `process.go` / `execute.go` / `host.go`；`plugin/definition.go`、`plugin/catalog/catalog.go`；`internal/container/instance.go` | 公开 API 与可变运行状态同在根包 `App`，按职责拆文件；不再保留 `xbc.go` 大文件 |
| `plugin.go` | `plugin/plugin.go` + `plugin/identity.go` + `plugin/definition.go` + `plugin/lifecycle.go` | Plugin 改为 marker，Key 是唯一身份，Base 可选；Web capability 不进入 core，`HealthChecker` 删除 |
| `deps.go` | `plugin/dependency.go` | 依赖引用统一为 `RefTo` / `Key.Ref()` |
| `context.go` | `plugin/context.go` + `plugin/host.go` + `web.CurrentRoute` / `RouteCatalog` | Context 仅见 `config.View`；框架反向端口命名为 `RuntimeHost`；Route/Routes 移出 core |
| `registry.go` | `internal/container/registry.go` + `plugin/{registry,extension}.go` | 值存储和扩展索引实现 internal，泛型门面及来源 Identity 归 plugin |
| `config.go` | `config/*` + 根 `settings.go` + `web/config.go` | Environment/View 归 config；私有 `settings` 绑定 `xbc.*`；ServerConfig 其余字段归 web；不兼容读取旧 `server.*` |
| `cli.go` | `internal/cli/command.go` | stdlib-only 命令域；runtime 只消费 `Command`，`WantsMigration` 只收 bool |
| `stage_expand.go` | `internal/container/expand.go` | 删除 `clonePrototype`/R7；按 Definition Key 检查孤儿配置；按 `Definition.Instances` 静态展开 |
| `stage_resolve.go` | `internal/container/resolve.go` | 依赖、产物选择和 topology 排序的容器职责 |
| `stage_config.go` | `config/bind.go` + `internal/container` 调用 | 绑定机制与插件实例遍历分开 |
| `stage_init.go` | `internal/container/initialize.go` + 根 `lifecycle.go` | 注入/收割归容器，跨实例 Init 编排和回滚边界归根包 |
| `stage_run.go` | 根 `execute.go` / `lifecycle.go` / `shutdown.go` / `process.go`；`web/server.go` | migrate/readiness/unwind 按职责拆分；HTTP 整体迁出；`signal.Notify` 唯一落点是根 `process.go`，不在 lifecycle/Execute 内 |
| `router.go` / `middleware.go` / `mwchain.go` | `web/router.go` / `web/middleware.go` / `web/order.go` | Gin 专属能力全部归独立 web module |
| `startuplog.go`、旧 `report.go` / `internal/report/*` | 根 `startup_report.go` + `web/report.go` | core 的渲染与输出时机同在一个文件，靠「render* 不做 I/O」这条规则分开；各运行栈只报告自己拥有的数据 |
| `goroutine.go` | 根 `task.go` | 托管任务、准入开关、critical escalation 与应用级关机标志 |
| `internal/inject/*` | `internal/container/inject/*` | 注入反射只服务容器，作为其实现细节下沉 |
| `internal/conf/*` | `config/{source,bind,validate}.go` | 同一配置能力内聚，不保留缩写包 |
| `internal/graph/*` | `topology/graph.go` | 升级为 core/web 共享的 stdlib-only 稳定叶子包，最终 API 为 hard/soft edge + typed Direction |

关键变化不是“把 17 个文件平均搬到几个目录”，而是让共享同一份 `App` 状态的核心运行时按职责拆成十二个根文件、由导出清单守卫替代包边界，并让核心生命周期与协议运行栈彻底解耦。

### 8.1 测试怎么跟着搬

测试跟随 canonical owner：运行时白盒测试与 core 报告测试位于根包；expand/resolve/initialize/registry/inject 的行为测试位于 `internal/container` 及其 `inject` 子包；middleware/route/server 测试位于 `web` module。公开 API 由 `tests/integration/public_api_test.go` 以外部包视角验证；全仓布局守卫位于 `tests/architecture`。测试 helper 在各自包内最小复制，不建立跨 module 的 internal fixtures。

Web 启动同步按最终两阶段 readiness 验证：`web.Server.Start` 同步完成 `net.Listen`，返回时 `Addr()` 已可读但尚未 accept；`OpenTraffic` 才把 `Serve` 交给 `GoCritical`。需要自定义 listener 的同包测试通过 `web/export_test.go` 暴露未导出钩子，不为测试扩大生产 API。

OS signal 与可嵌入执行必须分开测试：普通生命周期测试调用 `App.Execute` 并取消 caller context，断言它不读取 `os.Args`、不退出进程；只有 `process_test.go` 通过 `executeWithSignals` 驱动真实 SIGINT/SIGTERM，验证订阅早于 Init 且结束后解除注册。未导出的 `ready` channel 只用于观察“已完成启动并进入 wait”，不承载“signal 已注册”的公开语义。

身份测试只围绕 `Definition.Key`；旧的包路径推名、`deriveName` 和共享 `internal/testplugins` 已删除。Catalog 测试钉住重复/非法 Key、按 Key 排序、冻结幂等和快照防御复制；Factory 测试钉住每启用实例恰好一次、禁用零次及 nil 返回失败；config 测试钉住 View 对 map/slice 的递归防御复制。

架构守卫也按 module 分布，并按所问问题选择判据：`tests/architecture/architecture_test.go` 对 Gin、`web`、`examples` 及 owner 反向依赖等方向规则读取直接 import，并额外用依赖闭包证明生产 core 不会经传递依赖带入 Gin、gRPC 或任一可选运行栈；叶子纯净性和 config/log 独立性同样读取依赖闭包；`plugin` 另有 SPI 依赖闭包守卫，`web` 与 `examples` 各自读取直接 import，防止可选 module 绕过 owner 读取 core internal。全仓守卫还直接检查 `go.mod` 不含 Gin、gRPC 或可选运行栈 module，用 AST 检查 container 不调用默认 Catalog，并严格限定根包的十二个 canonical 生产文件与六个导出符号、进程设施只落在根 `process.go`、topology 最终 API 形状以及子 module 无 replace/伪版本；plugin 守卫锁定 marker `Plugin`、`Key` / `Cardinality` 与 `Context.Config() config.View` 的最终 SPI。普通 import `web` 无注册副作用、显式 `web/autoload` 才注册的语义由行为测试覆盖，不再用重复的 import 文本断言。`web` 的方向守卫扫描 module 内全部 package（包括 `web/autoload`）；子 module 发布清单从 `go.work` 自动发现。根 module 的一次 `go test ./...` 不会覆盖嵌套 module，CI 必须逐 module 执行。

---

## 9. 架构守卫

下列是需要持续守住的完整架构规则；其中涉及尚未创建的 gRPC/management/integration module 的部分，在对应 module 真正落地时再增加可执行守卫，不能为了让测试“有对象可扫”而提前创建空目录：

1. **Core 纯净性**：根包依赖闭包禁止 Gin、Google gRPC、`web`、`grpc`、`management/*`、`integration/*`；
2. **SPI 纯净性**：`plugin` 禁止根包、catalog、internal、`web` 与 `grpc`；`plugin/catalog` 只依赖 `plugin` 和标准库；
3. **配置/日志独立性**：`config` 与 `log` 的依赖闭包互不包含对方；
4. **叶子纯净性**：`topology`、`internal/container/inject`、`internal/cli` 只允许标准库；
5. **Container 方向**：只允许 `plugin`、`plugin/catalog`、`config`、`log`、`topology`、`internal/container/inject` 的必要依赖；catalog 只用于接收 `Snapshot` 类型，容器不得调用包级 `catalog.Declare`/`catalog.Freeze` 读取默认 Catalog，也不得依赖根包或运行栈；
6. **运行栈独立性**：`web` module 不得依赖将来的 `grpc` module，反之亦然；
7. **无横向偷读**：运行栈通过 `plugin.Extensions` 消费能力，不读取 `internal/container.Instance`；
8. **发现确定性**：全局仅保存 Definition；只有未传 `WithDefinitions` 的第一次 `New`（包级 `Run` 走同一路径）会冻结默认 Catalog，私有 Snapshot 则在传入前已由私有 Catalog 冻结，且不会读取或冻结默认 Catalog；两条路径都按 Key 确定顺序，禁止全局 live instance 和 `App.Register`；
9. **module 验收**：CI 分别进入 core、web 及 examples module 执行格式化、vet、test；core `go.mod` 不得 require 任一可选 module；子 module 不得含 `replace`，对 xbc module 的 `require` 只有在真实 tag 存在后才能写入，禁止 `v0.0.0`、pseudo-version、预发布和占位版本。发布清单守卫必须显式读取仓库根 `go.work`，不能继承调用者 cwd 或 `GOWORK`（包括 `GOWORK=off`）；
10. **可选 module 不得直接 import core internal**：`web`、`examples`（以及将来的 `grpc`、`management/*`、`integration/*`）不得出现对 `github.com/xbcio/xbc/internal/...` 的**直接 import**。Go 的 internal 可见性按目录树而非 module 边界判定，这些 module 在语法上完全能 import 进去，所以这条**只能**靠守卫拦住，不能靠自觉（见 §8.1）。该断言必须写在各 module 自己的 `arch_test.go` 里，core 的 `go list` 看不到它们；
11. **web 不得从根包拿 alias**：`web` 的直接 import 里不得出现根包 `github.com/xbcio/xbc`。canonical 类型必须从 owner 包（`plugin`、`config`、`log`、`topology`）导入。这条防的是「重新长出一套根包 alias」——一旦 `web` 依赖根包，§7 里刚删掉的 alias 就会以「反正 web 已经依赖根包了」的名义回来；
12. **迁移产物与职责 owner 稳定**：旧 `internal/inject`、`internal/report`、根 `xbc.go` / `report.go` 与 `stage_*.go` 不得复活；根生产 Go 文件严格且仅允许 `app.go`、`doc.go`、`run.go`，runtime 生产文件严格限定为 §3 的十个 canonical 文件；
13. **进程适配唯一 owner**：根包与全部 `internal/**` 生产代码中的 `os.Args`、`os.Exit`、`os.Stderr`、`signal.Notify/NotifyContext`、`log.Sync` 以及包级 `osExit` 只允许出现在根包的 `process.go`，且该文件必须实际拥有这些入口，避免守卫空跑。这条在 runtime 并入根包后**变重要了**：`process.go` 不再被独立包围起来，它现在只是十二个根文件之一，同包兄弟伸手就能拿到包级 `osExit`；
14. **最终 API 形状**：`plugin.Plugin` 是零方法 marker，`Definition.Key` / `Instances` 分别为 `Key` / `Cardinality`，旧 `Definition.Name` 不得复活，`Context.Config()` 精确返回 `config.View`，`RuntimeHost` 保持 §5.8 的四个方法；topology 只暴露 `AddHardEdge(string,string)` / `AddSoftEdge(string,string)` 与 typed `Direction`（`After` / `Before`），旧 `AddEdge(..., hard bool)` 不得复活。
15. **根包导出清单冻结**：根包只允许导出 `App`、`App.Execute`、`New`、`Option`、`Run`、`WithDefinitions`。新增导出必须是对这份清单的明确修改，而不是重命名的副作用。

当前已落地的自动化覆盖必须按真实实现理解，而不能把上面的完整规则清单误写成“每条都已经用同一种 `go list` 断言实现”：

- `tests/architecture/architecture_test.go` 从测试文件位置锚定仓库根，扫描生产代码、包内测试和外部测试的直接 imports，拦截 core → Gin、core → `web`、根包 → `examples`，以及 `plugin` / `config` / `log` / `topology` 向根包或 internal 的已声明反向边；另检查 core 生产依赖闭包与根 `go.mod`，拦截经传递依赖带入或即使暂无源码引用仍残留的 Gin、gRPC 和可选运行栈；
- `plugin/arch_test.go` 用生产依赖闭包验证 SPI 不带入根包、internal、Gin 或 gRPC，并验证 `plugin/catalog` 没有引入 `plugin` 闭包之外的新第三方依赖；`config/arch_test.go` 与全仓守卫共同覆盖 config/log 的相互独立；
- 全仓守卫用生产依赖闭包检查 `topology`、`internal/container/inject`、`internal/cli` 没有第三方依赖，并用 AST 精确禁止 container 调用默认 Catalog 的包级 `Declare` / `Freeze`；另用严格文件 allowlist、import-aware AST 与反射守住旧路径、根包的十二个 canonical 文件、根包导出清单、进程设施 owner、topology API 和四方法 `RuntimeHost`。发布清单通过 `runtime.Caller` 锚定 `architecture_test.go` 所在仓库根，把绝对 `go.work` / `go.mod` 路径显式交给 `go work edit -json` / `go mod edit -json`，因此从其他 cwd 启动或外部设置 `GOWORK=off` 时仍检查 workspace 中全部子 module；
- `plugin/arch_test.go` 还用反射锁定最终 SPI 形状，不冻结 Definition 的完整字段数；`web/arch_test.go` 遍历 module 中全部 package，直接禁止其源码与测试 import 根包或 core internal；`web/side_effect_test.go` 与 `web/autoload/register_test.go` 分别验证普通 import 无注册副作用和显式 autoload 注册；`examples/quickstart/arch_test.go` 遍历 examples module，直接禁止示例 import core internal。尚未实现的 module 没有也不应有对应测试。

### 9.1 判据必须匹配问题：直接 import、依赖闭包、`go.mod` 与 AST

第 10、11 条极易写错成闭包检查，而闭包检查在这里是**必然误报**的：

- `examples` 必须 import 根包（它要调 `xbc.Run()`），根包再使用 `internal/container`。于是 `go list -deps github.com/xbcio/xbc/examples/quickstart` 的输出里**一定**含有 `github.com/xbcio/xbc/internal/container`。用闭包判据，这条守卫在写下的第一天就是红的，而它拦截的东西（examples 自己写 `import ".../internal/container"`）根本没发生。
- 同理，`web` 的闭包里会不会出现根包取决于它依赖的包将来怎么变，用闭包判据等于让守卫的成败依赖无关的第三方。

**传递依赖 internal 是合法的**——那正是 internal 包存在的意义：通过 owner 包的公开 API 间接使用。**直接 import 才是违规**——那是绕过 owner 包、把别人的内部结构焊死在自己身上。

因此这两条守卫读的是 `go list -json` 的 `Imports`、`TestImports` 与 `XTestImports`（本包生产代码、包内测试和外部测试真实写下的 import 行），**不是** `Deps`（整个传递闭包）。实现上逐包读取：

```sh
go list -json ./...   # 逐包取 .Imports/.TestImports/.XTestImports，不取 .Deps
```

反过来，真正询问「这个包最终会不会把某个东西编译进来」的纯净性断言必须读取 `Deps`。当前 `plugin` 的协议无关性、`topology` / `internal/container/inject` / `internal/cli` 的零第三方依赖，以及 config/log 的相互独立都使用依赖闭包。不是所有第 1~7 条都属于这一类：现有 core 对 Gin、`web`、`examples` 和 owner 反向依赖的检查表达的是源文件方向规则，读取三种直接 import；core 是否残留 Gin、gRPC 或可选运行栈 module 则直接检查 `go.mod`，因为没有源文件引用的陈旧 `require` 不会出现在任何 import 图里。

还有一类约束根本不是“有没有 import”：`internal/container` 合法依赖 `plugin/catalog` 的 `Snapshot` 类型，但不得调用指向进程级默认目录的包级 `catalog.Declare` / `catalog.Freeze`。现有守卫解析生产源码 AST，只匹配真实调用表达式，既允许所需类型依赖，也避免注释或字符串造成误报。**这些判据不能统一成一个看起来更简单的写法；应先说清要拦的是传递纯净性、直接越界、module 声明还是特定 API 调用，再选择证据。**

建议 CI 验收：

```sh
# core
gofmt -l .
go vet ./...
go test ./... -count=1

# 每个已存在的独立 module
(cd web && gofmt -l . && go vet ./... && go test ./... -count=1)
(cd examples && gofmt -l . && go vet ./... && go test ./... -count=1)
```

根 module 的 `./...` 不会覆盖嵌套 module，因此 CI 必须显式枚举，或从 `go work edit -json` 的 `Use` 列表生成矩阵；不能把一次根目录绿灯误当成整仓验收。发布任务还需在禁用 `go.work` 的环境中执行 `go mod tidy` / `go test`，证明每个子 module 只依赖已发布版本；在 core 尚未打真实 tag 之前这一步必然失败，属于预期，发布顺序固定为 core → web → examples。此时相关 `require` 应暂缺，**不得用本地 `replace`、`v0.0.0`、伪造 pseudo-version 或其他占位版本把它糊过去**；只有上游 tag 已真实发布并可解析后才添加 require。

---

## 10. 已完成的落地顺序与持续验收

以下第 1~9 项是本次迁移的完成记录，不是尚待执行的规划；保留顺序是为了说明边界如何在可验证状态下逐步建立：

1. **已建立叶子包**：`topology` 从 `internal/graph` 升级为零第三方依赖的稳定叶子；`config` 合并 `internal/conf` 与原根 `config.go` 的配置源、绑定、回写和校验能力，并导出 `Environment` / 只读 `View`，为后续迁移提供稳定地基；
2. **已统一发现入口**：建立 `plugin.Definition` 与可冻结的 `catalog.Catalog` / `Snapshot`，实现零参数 `Run()`、`New(...Option)` 和 `WithDefinitions`；删除 `App.Register`、包级 `Register`、全局 live instance 列表与 `source` 双路径，并用测试锁定重复/非法定义、声明顺序无关、冻结后不可变、禁用不调用 Factory 和 App 级运行状态隔离；
3. **已建立协议无关 SPI**：落地 `plugin` 包，包括 marker `Plugin`、可选 `Base`、`Key`、`Definition.Instances` / `Cardinality`、`Runner` / `TrafficOpener` / `RuntimeHost` / `Identity` / `Extensions[T]`，同时断开 config/log 相互依赖；
4. **已收拢容器与运行时实现**：实例展开、配置绑定、依赖解析、registry、inject/harvest 与初始化状态进入 `internal/container`，反射注入下沉到 `internal/container/inject`；删除 `clonePrototype` 与裁定 R7，并把 R6 改为冻结 Snapshot 的 Definition Key 与配置节求差。插件 `Initializer.Init` 的调用、跨实例生命周期编排和失败回滚归 `lifecycle.go` / `shutdown.go`，没有塞进 container；
5. **已重做任务组与关机流程**：根包落地准入开关式 task group（§5.4）和有界 unwind（受控 goroutine、panic recover、共享 deadline、错误聚合），再迁移依赖这套语义的 `web.Server.Stop`；`GoCritical` 的“意外返回”判据已改为应用级 `shuttingDown`（§5.5），避免把正常 Stop 误报为 critical failure；
6. **已分离进程适配并实现两阶段 readiness**：根 `run.go` 只构造 App 并委托，`process.go` 在执行前注册 signal，`App.Execute` 保持可嵌入且无进程副作用；`Runner.Start` → 屏障 → `TrafficOpener.OpenTraffic` 串行编排与存活能力校验已经落地（§5.0~§5.2）；
7. **已抽离 Web 运行栈**：Router、Middleware、HTTP server 与 HTTP report 迁入独立 `web` module，根包删除 Gin/HTTP 字段和接口；`web.Server.Start` 返回时已 bind 且 `Addr()` 可读，`OpenTraffic` 才开始服务。普通 `web` import 保持无注册副作用，显式 `web/autoload` 才声明默认 Definition；
8. **已建立 module 与守卫边界**：`web` 与 `examples` 各有独立 `go.mod`，仓库根以 `go.work` 联调，子 module 不写本地 `replace`，上游真实 tag 发布前也不写伪 require；全仓守卫位于 `tests/architecture`，各能力/module 的局部 `arch_test.go` 共同落实 §9 的直接 import、依赖闭包、`go.mod`、AST 与最终 API 形状约束；
9. **已完成文件收尾**：删除 `stage_*`、`internal/conf`、`internal/graph`、旧 `internal/inject`、旧 `internal/report`、`mwchain`、根包 HTTP 类型和临时 alias；外部 API 测试迁入 `tests/integration`，配置样例只保留 `examples/quickstart/application.yml`；
10. **已撤销三个伪边界**（2026-08-28）：`internal/runtime`、`internal/startupreport` 并入根包，`internal/architecture` 迁到 `tests/architecture`。前两者的存在理由经实测不成立（依赖闭包逐字相同、包边界不提供「测试无需构造主类型」这个性质），详见 §2.1；`internal/architecture` 的守卫本身有效——它上次真的抓到过 `scan.go` 的漂移——但它是零生产文件的 `package architecture_test`，放在语义为「实现」的 `internal/` 下不对，而 `tests/integration` 早已存在。`internal/cli` **保留**：§9 约束 4 的「零第三方依赖叶子」只能在包粒度上用 `go list -deps` 表达，根包有 74 个第三方依赖，并进去等于静默作废这条守卫。合并后根包导出符号仍是 6 个，与合并前完全一致；
11. **实现 gRPC 时继续复验边界**：若加入 gRPC 必须修改 core 才能完成，说明 SPI 仍泄漏协议概念，应先修边界而不是继续加条件分支。

后续变更仍必须通过对应 module 的 `gofmt`、`go vet`、`go test`（含 `-race`）和架构守卫；第 11 项是对未来运行栈的持续验收条件。

**发布顺序固定为 core → `web` → `examples`。** 在 core 打出并推送真实 tag 之前，去掉 `go.work` 的构建必然失败（`web` 找不到 `github.com/xbcio/xbc` 的已发布版本），这是预期结果，不是缺陷；此时不要添加 core require。**不得用本地 `replace`、`v0.0.0`、伪造 pseudo-version、预发布或其他占位版本把它糊过去**——这些写法都不能证明外部用户可以独立 `go get`。只有真实 tag 可解析后，才按顺序添加下游 require 并执行 `GOWORK=off` 验证。

---

## 11. 明确否决的方案

| 方案 | 否决原因 |
|---|---|
| 根包继续持有 Router/Middleware/http.Server | 把产品永久锁定成 Web 框架，gRPC 只能成为第二套特例阶段 |
| `plugin/` 同时装 SPI、Gin 类型和所有实现 | 核心契约被具体协议污染，插件树最终成为无法治理的市场目录 |
| core SPI 固化 `HealthChecker`/health response | 运维探针是可选聚合能力，契约应由 `management/health` 拥有，暴露应由协议适配器拥有 |
| 新建 `kernel` / `engine` / `core` | 名字只说明“很重要”，没有可验证职责 |
| 按十个 stage 建包或文件 | 时间顺序不是模块边界，跨阶段状态会被迫导出 |
| 根包 `Default()` 默认装配 Web/management | 对 gRPC、任务型和混合应用不存在统一正确默认值 |
| `App.Register` 或把 live plugin 塞进全局列表 | 重复写装配清单，产生可变时序，并制造跨 App 状态泄漏 |
| 注册已构造好的插件实例而非工厂 | 多实例只能靠反射造零值，被迫引入「何时可复用原型」的裁定和「构造参数会丢」的警告（§3.3） |
| `init()` 直接创建连接/启动任务 | 包初始化无错误通道、无配置上下文，也无法参与统一回滚；init 只允许声明 Definition |
| 用根包 alias 永久兼容 HTTP 类型 | alias 仍产生真实 import 依赖，无法实现 core 纯净性 |
| 一开始抽象统一 Transport/RPC 接口 | HTTP 路由与 gRPC service 尚无稳定公共模型，过早抽象只会制造最低公分母 |
| 所有可选实现共用 core `go.mod` | package 看似隔离，依赖下载、升级和安全扫描仍全部耦合 |
| 在 core 建报告缓冲区聚合多段启动日志 | 换个名字重建 `startup` 万能包，让 core 反向认识每个运行栈的内部状态；还会把日志推迟到最后一个 Runner 之后（§4） |
| 本次先建空的 `grpc/`、`management/health/`、`integration/*` 目录占位 | 空目录和占位 API 是没有使用者的契约，只会在真正实现时被推翻；目录布局的说服力来自能跑通的代码，不是提前挖好的坑 |
| core 按名字识别 `web`/`grpc` 插件来决定何时开放流量 | 那是把协议知识写死进内核，第三个运行栈出现时只能加第三个 `if`；两阶段 readiness 用类型断言 `TrafficOpener` 表达同一件事，且对未知运行栈天然成立（§5.1） |
| 把启动 hook 丢进 detached goroutine 并试图由框架强制中断 | Go 无法安全抢占任意插件代码，还会让 unwind 的 `Stop` 与未返回 hook 并发；正确方案是同步 hook + 实现 `context.Context` 的 `plugin.Context` 协作取消，并在步骤之间补充停止检查（§5.0） |
| `shutdown_timeout` 继续留在 `server.*` 配置节 | 让 Web 配置节决定一个 gRPC-only 或纯任务型应用的关机预算，而那个应用根本不该有 `server` 节（§5.3） |
| 关机时给每个插件各自一份 `shutdown_timeout` | 预算是「这个进程多久必须消失」，N 个插件各等一份就是 N 倍进程寿命；deadline 必须共享（§5.3） |
| 用 `context.WithTimeout` + `Stop(ctx)` 就算实现了有界关机 | `Stop` 收到 ctx 却选择不看，框架仍然会永远阻塞在那一行；有界性只能由调用方的受控 goroutine + select 保证，必要时故意泄漏卡住的 goroutine（§5.3） |
| task group 直接 `cancel` 后 `wg.Wait()` | 与 `wg.Add` 存在竞态：关机瞬间提交的任务可能在 `Wait` 返回后才 `Add`；必须先关准入再 cancel/wait（§5.4） |
| 架构守卫用「依赖闭包不含 internal」判定外部 module | `examples` import 根包、根包又依赖 `internal/container`，闭包判据第一天就是红的；跨 module 的 internal 守卫必须看**直接 import**（§9.1） |
| 给子 module 写本地 `replace`、`v0.0.0`、伪造 pseudo-version 或占位 require 让发布演练变绿 | 那不是通过验证，是取消验证：真实上游 tag 尚未发布时应暂缺 require；tag 可解析后再添加并用 `GOWORK=off` 验证（§10） |
| 没有任何 Runner/托管任务时静默 `select{}` 等下去 | 一个不会响应任何请求的进程装作健康活着，是最难排查的一类故障；必须启动失败并列出装配了什么（§5.2） |

这套布局的判断标准只有一个：**看到 import path 就知道它属于应用内核、插件契约、Web、gRPC，还是某项外部集成；普通应用入口只有 `xbc.Run()`，删除任一可选插件后 core 仍能独立编译和运行。**
