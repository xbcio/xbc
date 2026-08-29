# xbc — 面向多运行栈的包布局设计

- **日期**：2026-08-26
- **仓库**：`git@github.com:xbcio/xbc.git`
- **Core module**：`github.com/xbcio/xbc`
- **状态**：已实现（2026-08-28：`internal/runtime`、`internal/container`、`internal/cli` 分别迁移为根级 `runtime`、`assembly`、`cli`；`xbc` 根包收敛为六符号薄门面）
- **定位**：通过插件快速构建 Web、RPC/gRPC、分布式服务与微服务
- **优先级**：在产品定位、插件发现与装配机制、通用生命周期、health 归属及包/module 边界上取代 `2026-08-23-xbc-plugin-framework-design.md` 的冲突结论

---

## 0. 最终结论

`xbc` **不是一个以 Gin 为内核的 Web 框架**，而是一个与传输协议无关的插件应用运行时：

- 核心只负责插件发现、配置绑定、依赖解析、初始化、启动、托管任务和逆序关闭；
- `transport/web`、`transport/grpc` 以及未来其他协议能力都是可选插件；
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
    _ "github.com/xbcio/xbc/transport/web/autoload"
)

func main() {
    xbc.Run()
}
```

这里的 import 是 Go 编译期依赖选择，不是运行时扫描：被链接的业务注册包或显式 `autoload` 包声明“有哪些插件可用”，配置决定实例与启停，依赖图决定顺序。注册动作只登记无副作用的 Definition/Factory，不得在 `init()` 中创建连接、启动任务或监听端口；普通实现包（例如 `web`）本身保持无注册副作用。

本次落地范围只包含 core、`transport/web` 与 `examples` 三个 module。`transport/grpc`、`management/health`、`integration/*` 在真正实现前**不创建目录、不创建空 `go.mod`、不创建占位 API**——本文档里凡是提到它们的地方，约束的都是“将来实现时不许越界”，不是现在就该落盘的结构。多加一个 `_ "github.com/xbcio/xbc/transport/grpc"` 的 import 行属于将来时，不属于当前入口。

---

## 1. 从 Spring / Spring Boot 借什么

借鉴的是**稳定能力分层与可选模块隔离**，不是 Java 的目录名和自动扫描机制。

| Spring / Spring Boot | 核心思想 | xbc 对应 |
|---|---|---|
| `spring-core` | 与上层框架无关的基础机制 | `plugin/ordering`、`assembly/inject` |
| `spring-beans` | 定义、实例化、依赖解析与注入 | `assembly` |
| `spring-context` | 组件上下文与统一生命周期 | `runtime` + `plugin.Context` |
| `SpringApplication` | 应用启动门面 | 根包 `xbc.Run()`、`xbc.New(options...)` |
| `Environment` | 外部化配置、profile、配置源 | `config.Environment` |
| `spring-web*` | 可选 Web 运行栈，不污染 core/context | `transport/web` |
| gRPC / messaging starter | 引入可选能力及其默认装配 | 独立 `transport/grpc` 等插件 module |
| auto-configuration | 从可用定义按配置装配 | `plugin/catalog` + 插件定义的启用条件 |
| actuator | 可选运维能力与协议适配分离 | 按需建立 `management/health`，HTTP/gRPC 暴露仍由各自适配器负责 |
| test modules | 面向扩展作者的测试支持 | 将来的 `plugintest`，有真实用户后再建立 |

三项不能错误类比：

1. **starter 不是实现包。** Spring Boot starter 主要是依赖聚合描述；Go 中最接近它的是“import 一个自声明 Definition 的插件 module + 默认配置”，不能把所有插件实现统称为 starter。
2. **`Opt[T]()` 不是 `@ConditionalOnMissingBean`。** 前者表示依赖不存在也能运行；后者表示满足缺失条件时才注册另一个组件，语义不同。xbc 当前没有后者的直接等价物。
3. **actuator 不是默认 Web 内核。** 它是显式选择的运维模块，不应由通用 `xbc.New()` 暗中加入。

Go 没有可扫描 classpath，因此不伪造 `@ComponentScan`。xbc 采用更窄的**编译期目录发现**：链接进二进制的业务注册包或显式 `autoload` 包在 `init()` 中仅声明 `plugin.Definition`，`xbc.Run()` 再按环境配置创建应用私有实例。可复用实现包可以只提供无副作用的 `Definition()`，由调用方选择私有 Catalog 或 `autoload`。它借鉴 Spring Boot 的“依赖可用 → 条件装配”，但不扫描源码、不加载 `.so`，也不允许 `init()` 执行运行时 I/O。当前落地已经删除 `App.Register`、全局 live instance 列表以及显式/隐式双路径。

### 1.1 从 Kubernetes / Hertz 借什么，不照搬什么

Kubernetes 值得借鉴的是**明确 owner、薄入口、单向依赖和可执行的架构守卫**：命令入口不承载领域实现，稳定 API 由固定包拥有，装配细节不能泄漏给可选模块。xbc 因而让根包只做应用门面，让 `runtime`、`assembly`、`cli` 分别拥有运行编排、实例装配和命令解析；`plugin`（含 `plugin/ordering`）、`config`、`log` 继续拥有各自领域。当前仓库远小于 Kubernetes，因此不机械建立庞大的 `/cmd`、`/pkg`、`staging` 或多层 `internal` 树。

把原 `internal/runtime`、`internal/container`、`internal/cli` 迁移为根级 `runtime/`、`assembly/`、`cli/`，不等于把实现展平进 `package xbc`。这三个包都有可独立描述、可验证的依赖边界；根 `xbc` 只保留 `App`、`New`、`Run` 等六个应用入口。`runtime`、`assembly`、`cli` 是有意公开的低层能力包：高级嵌入方可以直接使用，普通应用优先使用根门面。它们仍是各自领域的唯一 owner；架构守卫禁止下层 SPI 和可选运行栈反向依赖它们，并要求仓库示例保持推荐的门面用法。

Hertz 值得借鉴的是**运行时、工具/生成器、协议实现和可选集成分离**。xbc 把协议无关实现留在 core module 的 `runtime` / `assembly` 等包，把 Gin-backed 实现放进独立 `transport/web` module；将来若有生成器，也应独立于运行时。package 边界表达 core 内部职责，module 边界负责阻断 Gin/gRPC 等重依赖，两者解决不同问题。普通 `transport/web` import 必须无注册副作用，只有显式 blank import `transport/web/autoload` 才写入默认 Catalog。没有照搬的是以 HTTP engine 为中心的包树——xbc 还要承载 gRPC、consumer 和纯任务应用，因此 HTTP 类型不能成为根包或 `runtime` 的骨架。

---

## 2. 包与模块划分原则

### 2.1 包是依赖边界，不是文件收纳盒

只有同时满足下列条件，才新建或保留包：

1. 有一句独立、稳定的职责描述；
2. 有清晰的允许依赖与禁止依赖；
3. 类型之间的内聚大于它们与调用方的内聚；
4. 拆分收益足以覆盖跨包调用和必要导出符号的成本。

代码行数、文件数和“看起来对称”都不是拆包理由。本次保留的四个能力边界都满足上述条件：

- `runtime` 拥有单个 App 的状态、bootstrap/execute、生命周期、任务、关机、进程适配和启动报告；
- `assembly` 把冻结的 Definition Snapshot 装配成实例、依赖顺序和值注册表；
- `cli` 是只依赖标准库的命令解析域，输入 args，输出纯数据；
- `assembly/inject` 是只理解 reflect 与 struct tag 的机械叶子，不认识任何 xbc 类型。

根目录的 `package xbc` 则是薄门面：`App` 包装 `*runtime.App`，`Option` 复用 `runtime.Option`，`New`、`Run`、`WithDefinitions` 和 `App.Execute` 只做委托。这样普通应用拥有稳定、窄小的首选入口，运行时实现又不会与根命名空间混成一个巨型包。`runtime`、`assembly`、`cli` 的导出符号是各自 owner 的低层公开 API，而不是私有协作细节；架构测试冻结根包六个导出符号，并对低层包的关键边界和 API 形状建立守卫。

`runtime` 的启动报告保留在 `runtime/startup_report.go`。纯渲染函数与输出编排可以在同包分工并直接针对 `*assembly.Container` 做白盒测试，不需要再建立 `internal/startupreport` 转发层。

### 2.2 名字直接表达领域能力

- 使用 `plugin`、`config`、`web`、`grpc` 等可直接解释的能力名词；`transport/` 只为协议运行栈提供仓库级命名空间；
- package 表达稳定职责，具体主类型表达角色：`web.Server`、`grpc.Server`，而不是把二者粘成 `ginserver`、`grpcserver`；
- 不建立 `core`、`kernel`、`engine`、`common`、`utils`、`helper`、`manager` 这类无业务边界的杂物包；
- 不为缩短几个字符使用 `conf`、`mwchain`、`resp`、`errs` 等不必要缩写；
- `web` 明确表示“Gin-backed Web 应用运行栈”，不是虚构的传输无关 HTTP 抽象；Gin 是当前实现和公开 Handler 契约的一部分，但不是包的职责名；
- 若未来确有第二种 Web 引擎，再基于真实公共契约拆出 `transport/web/gin` 等实现；现在不为假想替换预建层次；
- 不预建泛化的 `rpc` 包。只有两个以上实现真的共享稳定契约后，才抽取公共协议层。

### 2.3 一个概念只有一个 canonical owner

- 应用入口和面向使用者的稳定 `App` API 归根包 `xbc`；
- 运行状态、生命周期、托管任务、关机与进程适配归 `runtime`；
- 实例展开、配置绑定、依赖解析、注入编排和值注册表归 `assembly`；
- 反射 tag 扫描与字段读写归 `assembly/inject`；
- 命令解析归 `cli`；
- 插件契约归 `plugin`，Definition 目录归 `plugin/catalog`，排序归 `plugin/ordering`；
- 配置环境归 `config`，日志与 trace 门面归 `log`；
- Web server、路由和中间件归 `transport/web`，将来的 gRPC 能力归 `transport/grpc`。

不能同时长期维护 `xbc.Router` 与 `web.Router`，也不能让 `runtime`、`assembly`、`cli` 各自再复制一套插件或配置模型。兼容别名只能是有删除期限的迁移手段，不能成为目标架构。

### 2.4 可选重依赖用 module 隔离

Spring 的模块是独立 artifact；只移动 Go package、却让根 `go.mod` 永久携带所有可选依赖，只完成了一半隔离。

- 核心 module：`github.com/xbcio/xbc`；
- `transport/` 是仓库级协议运行栈命名空间，不提供共享 Go package 或 module；
- Web 插件 module：`github.com/xbcio/xbc/transport/web`；
- gRPC 插件 module：`github.com/xbcio/xbc/transport/grpc`（真正实现时再创建）；
- 仓库开发使用 `go.work` 联调；发布与依赖版本各自可控；
- 子 module 的 `go.mod` 只能依赖**已经真实发布**的 core 版本，不提交指向本地目录的 `replace`；对应 tag 尚不存在时先不写该 `require`，禁止用 `v0.0.0`、伪造 pseudo-version、预发布或其他占位版本假装可发布；
- 发布顺序固定为 core 在前、各可选 module 在后。core 使用真实 `vX.Y.Z` tag，嵌套 module 使用如 `transport/web/vX.Y.Z`、`transport/grpc/vX.Y.Z` 的路径前缀 tag；只有上游 tag 已推送且可被 `GOWORK=off` 解析后，下游才能添加相应 `require`；
- 可选运行栈、management、数据、注册发现和消息系统集成达到稳定形态后，均按独立 module 发布。

`go.work` 只是仓库开发工具，不是消费者解析依赖的前提。示例也必须使用独立 module；否则 `go mod tidy` 会因示例 import 而把 `transport/web`、`transport/grpc` 等可选 module 重新写进 core 的 `go.mod`。这样纯后台任务或纯 gRPC 服务依赖 core 时，不会下载或编译 Gin；Web 服务也不会被迫携带 gRPC。

---

## 3. 当前布局（已落地）

`transport/`、`management/`、`integration/` 是仓库级分组，不创建同名 Go package；任何具体能力没有实现时都不创建占位 module。当前实际落盘的 module 只有 core、`transport/web`、`examples`。下树列出主要生产 owner；普通 `*_test.go` 省略：

```text
xbc/
├── .gitignore
├── go.mod / go.sum                core module 清单与依赖校验和
├── go.work / go.work.sum          core / transport/web / examples 本地联调
├── xbc.go                         package xbc：六符号应用门面与总览
│
├── runtime/                       App 状态、bootstrap/execute、生命周期与进程适配
│   ├── app.go / run.go
│   ├── bootstrap.go / execute.go
│   ├── host.go / lifecycle.go
│   ├── process.go                 os.Args、signal、log.Sync、os.Exit 的唯一 owner
│   └── settings.go / shutdown.go / startup_report.go / task.go
├── assembly/                     Definition 实例装配、绑定、解析、初始化与 registry
│   └── inject/                    stdlib-only 的反射 tag 扫描与字段操作
├── cli/                           stdlib-only 的命令解析
├── plugin/                        稳定、协议无关的插件 SPI
│   ├── catalog/                   Definition Catalog 与冻结 Snapshot
│   └── ordering/                  stdlib-only 的共享拓扑排序能力
├── config/                        外部化配置、绑定、校验与只读 View
├── log/                           独立日志与 trace 门面
├── tests/
│   ├── architecture/              全仓依赖、布局、module 与 API 形状守卫
│   └── integration/               外部包视角的根门面 API 守卫
├── docs/                          设计与使用文档
├── transport/                     协议运行栈命名空间；无共享 package/module
│   └── web/                       独立 module；Gin-backed Web 插件
├── examples/                      独立 module；可运行消费方示例
└── （将来）transport/grpc/、management/health/、integration/<technology>/
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

普通 `transport/web` import 只提供类型、能力接口和上面的无副作用 `Definition()`，**不会**改写默认 Catalog。只有可执行程序显式 blank import 的 `transport/web/autoload` 才执行注册：

```go
// package transport/web/autoload
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

它的落点是根包 `assembly_expand.go` 的 expand（§5 生命周期第 2 步）——只有那里同时看得到冻结后的 Catalog 快照和 `config.Environment`。需要改写的是「对应插件」的定义：判据必须比对**冻结快照里的全部 Definition key**，包括 `Activation` 求值为 false、因而没有展开出任何实例的那些。若误用「已展开实例」作为比对基准，`plugins.gorm.enabled: false` 会因为 gorm 没展开而被判成孤儿节，把一个完全正常的关闭动作变成启动失败。

这条要两个方向都钉住：拼错的节名必须致命失败；`enabled: false` 的正确节名必须正常启动。只钉前者的话，上面那个误判基准可以照样通过测试。

普通应用无需接触 `catalog` API；那是插件 module 作者和测试设施的低层 SPI。应用看到的是 import 与 `xbc.Run()`：

```go
import (
    _ "example.com/order-service/internal/order"

    "github.com/xbcio/xbc"
    _ "github.com/xbcio/xbc/transport/web/autoload"
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
| 根包 `xbc` | 稳定应用门面：`App`、`App.Execute`、`New`、`Option`、`Run`、`WithDefinitions`；把调用委托给 `runtime` | 保存装配状态、实现生命周期、命令解析、容器细节、插件实现、Web/gRPC 类型；除冻结清单外新增导出符号 |
| `runtime` | 单个 App 的状态、Definition 集选择、bootstrap/execute、生命周期、托管任务、有界关机、私有 settings、`RuntimeHost` 适配、启动报告；`process.go` 唯一持有 os.Args/signal/log.Sync/os.Exit | 实例装配算法、反射字段扫描、具体协议 server、业务集成 |
| `assembly` | 把调用方提供的冻结 `catalog.Snapshot` 展开为实例；配置绑定、声明扫描、依赖解析、初始化、产物收割和值 registry | 读取/冻结进程默认 Catalog、命令解析、进程信号、任务调度、协议实现 |
| `assembly/inject` | 扫描 `xbc` struct tag，描述和读写反射字段 | Plugin、Context、配置、日志、生命周期；标准库之外的依赖 |
| `cli` | 解析显式 args、profile 环境变量和迁移授权，返回 `Command` 纯数据 | 配置加载、插件发现、装配、日志初始化；标准库之外的依赖 |
| `plugin` | Plugin/Definition SPI、生命周期 capability、依赖声明、Context、值注册访问和扩展点查询 | 默认目录状态、运行时/容器实现、具体协议类型、健康端点聚合 |
| `plugin/catalog` | Definition 声明、冻结、校验、确定性 `Snapshot` 及私有测试目录 | 创建 live plugin、读取配置、执行生命周期 |
| `config` | 配置源合并、profile、ENV、绑定、默认值、校验、只读查询 | CLI 子命令、日志初始化、具体业务/协议配置节语义 |
| `log` | Logger/trace 门面与默认实现 | 配置加载、应用生命周期、插件发现、HTTP middleware |
| `plugin/ordering` | 为插件及其贡献项提供稳定排序和 cycle/missing 诊断 | Plugin、Middleware 等领域类型；标准库之外的依赖 |
| `tests/architecture` | 测试态全仓守卫：依赖方向、canonical path、根门面、进程 owner、module manifest 与 API shape | 生产运行逻辑或可复用测试框架 |
| `tests/integration` | 以外部消费者视角验证根门面 API | 穿透低层 owner 做白盒验证 |
| `web`（独立 module） | Gin-backed `Server`、路由、不可变路由目录、Web middleware、`plugins.web.*` 配置和自己的报告 | core 装配、gRPC、服务发现、进程级 shutdown 预算；直接依赖根门面或低层 core owner |
| `examples`（独立 module） | 可运行消费方，验证普通用户使用根门面、plugin owner 与可选 module | 被 core/web 反向依赖；绕过门面直接调用 runtime/assembly/cli |

下面三项本次不创建目录、`go.mod` 或占位 API，只记录未来归属：

| 未来包/module | 负责 | 明确不负责 |
|---|---|---|
| `grpc` | gRPC `Server`、service 注册、interceptor、gRPC 配置 | Web 路由和 Gin 类型 |
| `management/health` | 可选、协议无关的 health contributor 契约与聚合 | 默认强制启用、HTTP/gRPC 端点和具体基础设施检查 |
| `integration/*` | 一项具体基础设施或平台集成 | 通用容器职责和其他无关技术 |

`plugin/ordering.Graph` 只提供意图明确的两种边：`AddHardEdge(from, to)` 要求两端最终都存在，缺端点时 `Sort()` 返回 `*MissingNodeError`；`AddSoftEdge(from, to)` 的可选端点缺失不阻断排序，而在结果中以带 typed `Direction`（`After` / `Before`）的 `Miss` 报告。不恢复含义模糊的 `AddEdge(..., hard bool)`。

命令行解析不放进 `config`，也不展平到根包。`cli.ParseArgs` 只读显式参数、`os.Getenv` 与标准库 `flag`，返回 `cli.Command`；`Command.WantsMigration` 只接收 `autoMigrate bool`，配置加载和执行决策留给 `runtime`。`cli` 的导出是公开的低层命令解析能力；普通应用仍优先使用 `xbc`，自定义宿主或进程适配器可以直接使用 `cli`。

核心 settings 位于 `runtime/settings.go` 并保持未导出，只承载 `xbc.shutdown_timeout` 与 `xbc.auto_migrate`，不是 `xbc.Settings` 或 `runtime.Settings` 公开 API。

启动报告留在拥有数据的模块：`runtime/startup_report.go` 报告 core 装配状态，`transport/web/startup_report.go` 报告路由与中间件。不要建立读取所有模块内部状态的 `startup` 万能包。多段报告是解耦的真实代价：core 报告在 Init/migrate 完成后、所有 `Runner.Start` 之前输出；各运行栈在自己的 `Start` 中输出。各段共用格式和稳定标题，但不为拼成一段而让 runtime 反向认识具体运行栈，也不增加“等全部运行栈报告后再输出”的屏障。

`HealthChecker` 不进入稳定 core SPI。需求稳定后由 `management/health` 定义 contributor 并通过 `plugin.Extensions` 聚合；HTTP、gRPC health protocol 或独立管理端口由各自适配器暴露。

## 5. 协议无关生命周期

生命周期分成“可嵌入应用执行”和“进程适配”两层。公开 `App.Execute(ctx, args) (int, error)` 与包级 `Run` 共用同一条内部执行流程，它负责：

```text
0.  parse explicit args
1.  load environment，初始化日志，绑定私有 xbc settings，建立 task runtime/assembly
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

`xbc.App.Execute(ctx, args)` 只把调用委托给 `runtime.App.Execute`；后者仅使用调用方明确传入的参数和 context，不读取 `os.Args`，不订阅 OS signal，不调用 `log.Sync`，也不调用 `os.Exit`。每个 runtime App 是单次执行对象，重复调用返回 code 1 和错误。包级 `xbc.Run()` 是进程入口：根 `xbc.go` 委托 `runtime.Run()`，再由 `runtime/process.go` 注册 SIGINT/SIGTERM、进入共用执行流程、输出诊断、flush 全局日志并按退出码终止进程。这样嵌入者保留进程控制权，普通 `main` 仍只需一行 `xbc.Run()`。

### 5.0 进程信号只属于包级 `Run`，且早于第一个 Init

旧实现直到 HTTP serve 阶段才调用 `signal.Notify`，使耗时 Init 期间的 Ctrl-C 可能直接终止进程，已经初始化的资源来不及统一 Stop。最终实现把唯一的 OS-signal 订阅放在 `runtime/process.go` 的 `executeWithSignals`：它在进入内部执行流程之前完成注册，所以肯定早于第一个插件 Init/Start。`New` 在此之前只冻结静态 Definition，不创建插件或资源。

`App.Execute` **从不**注册 signal；嵌入调用方通过取消传入的 context 请求停止。两条入口最终都调用同一个 `requestStop`/unwind 路径，但进程设施的 owner 不混淆：signal、`os.Args`、`log.Sync`、`os.Exit` 只属于 `runtime/process.go`，caller context 只属于 `Execute`。架构守卫扫描 `runtime` 的全部生产文件，防止 owner 漂移。

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

插件配置采用严格 typed schema：实现 `Configurable` 时，`ConfigPtr()` 必须返回非 nil 的 struct 指针；已声明的 typed section 出现未知字段必须报出完整相对/绝对路径，不能静默忽略拼写错误后回落到默认值。`enabled` 等框架保留键由 assembly 通过窄 `AllowedKeys` 排除，不能迫使每个插件在自己的 config struct 重复声明，也不能借 allowlist 隐藏其他未知字段。`app.*` 以及配置类型显式声明的 map/interface 边界属于自由格式区域，可以保持开放；其余 struct、struct slice 和 map value 按 schema 递归严格检查。

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

两阶段 readiness 完成后会进入等待。如果这个二进制**根本没有任何长期存活能力**——Catalog 是空的，或者所有启用的插件里没有一个实现 `Runner`/`TrafficOpener`、task runtime 也从未接纳过任务——那么这次阻塞不是“在服务”，而是一个什么都不做、只等着被停止的空进程。它最典型的成因恰恰是配置或 import 出了错：忘了 `_ "github.com/xbcio/xbc/transport/web/autoload"`，或者配置把唯一的运行栈 `enabled: false` 了。让它静默挂起，等于把一个启动错误伪装成一个健康的常驻进程。

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

运行栈用每个 `Extension.Identity` 生成稳定限定名和启动报告，调用能力时使用 `Extension.Value`；不得为获得来源信息绕过 `plugin.Extensions` 依赖根包私有装配结构。

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

`plugin.Context` 不保存 `*xbc.App` 或 `*runtime.App`。`plugin` 导出最小的 `RuntimeHost` 协作端口，`runtime.hostAdapter` 把调用转发给 `*assembly.Container` 与 runtime 自己的托管任务状态。这样依赖仍然向下：plugin 定义端口，runtime 实现端口，plugin 不需要反向 import 上层能力 owner。

```text
plugin.Context ──► plugin.RuntimeHost ◄── runtime.hostAdapter
                                             ├──► assembly.Container
                                             └──► runtime task state
```

`RuntimeHost` 只承载 lookup/provide/extensions/task 等机械能力，不得出现 Router、Gin、gRPC 或完整 App。它是全篇唯一一个“框架实现、SPI 定义”的反向接口，也最容易膨胀成 God interface，因此方法集固定为：

```go
package plugin

type RuntimeHost interface {
    ProvideValue(typ reflect.Type, instance string, v any)
    LookupValue(typ reflect.Type, instance string) (any, error)
    InitializedPlugins() ([]Extension[any], error)
    GoManaged(id Identity, fn func(context.Context), critical bool)
}
```

四个方法，没有第五个。`InitializedPlugins` 返回的是窄化后的 `Extension[any]` 快照，不泄漏绑定配置、依赖图、registry 或初始化状态。`NewRuntimeContext`、`BindRuntimeContext` 与 `BindLifecycleContext` 是框架装配入口；普通插件只接收 Context，不调用这些入口。

架构守卫验证两层边界：`plugin` 不得反向依赖 `runtime` / `assembly` / `cli`，`web` 等可选 module 也不得直接 import 这些低层公开 owner。运行栈只能通过 `plugin.Extensions` 消费能力；普通应用则通过只有六个符号的根门面启动或嵌入 runtime，高级宿主可以直接选择低层 API。

## 6. 依赖方向

箭头表示“可以 import”；未列出的横向或反向边默认不允许：

```text
config                           （与 log 互不依赖）
log                              （与 config 互不依赖）
plugin/ordering                  （stdlib only）
assembly/inject                 （stdlib only）
cli                              （stdlib only）

plugin ────────────────────────► config, log
plugin/catalog ────────────────► plugin
assembly ─────────────────────► config, log, plugin, plugin/catalog,
                                 plugin/ordering, assembly/inject
runtime ───────────────────────► cli, config, assembly, log, plugin,
                                 plugin/catalog, plugin/ordering
xbc(root facade) ──────────────► runtime, plugin/catalog

web ───────────────────────────► plugin, plugin/ordering, config, log, gin
transport/web/autoload ───────► transport/web, plugin/catalog
grpc（将来）───────────────────► plugin, config, log, google.golang.org/grpc
management/* ──────────────────► plugin, config, log + 自己的能力依赖
integration/* ─────────────────► plugin, config, log + 自己的技术依赖
```

硬约束：

1. 根包是薄门面，只直接依赖 `runtime` 与 `plugin/catalog`；它不得承载运行时或装配实现，也不得 import 任一可选运行栈；
2. `runtime` 拥有 App 状态和执行编排，可以依赖 `cli`、`assembly` 及基础 owner，但不得反向依赖根门面或具体协议 module；
3. `assembly` 只消费调用方交付的冻结 Snapshot，可以依赖基础 owner 与 `assembly/inject`，不得依赖 `runtime`、`cli`、根门面或默认 Catalog 的可变入口；
4. `plugin` 不得 import 根门面、`plugin/catalog`、`runtime`、`assembly`、`cli`、`internal/*`、`web` 或 `grpc`；`plugin/catalog` 只向下依赖 `plugin`；
5. `config` 与 `log` 彼此独立，且不得依赖根门面、plugin 或上层能力包；`plugin/ordering`、`assembly/inject`、`cli` 的生产闭包必须 stdlib-only；
6. 普通 `transport/web` 实现包不依赖 `plugin/catalog`，只暴露无副作用 `Definition()`；只有显式 `transport/web/autoload` 注册默认 Definition。未来 module 采用同一分层；
7. 可选运行栈不得为了便利直接 import 根门面、`runtime`、`assembly`、`cli` 或 core internal；canonical 类型从 `plugin`、`config`、`log`、`plugin/ordering` 等 owner 导入；
8. 普通应用可执行入口应 import 根包并调用 `xbc.Run()`；高级嵌入方可以有意使用 runtime/assembly/cli 的低层公开 API，但仓库 quickstart 保持推荐的门面用法；
9. core 的生产依赖闭包与根 `go.mod` 都不得包含 Gin、Google gRPC 或任一可选 module。

`config.Environment` 只保存和绑定配置值，不持有 `log.Config`，也不调用 `log.Init`。`runtime/bootstrap.go` 从环境的 `log` 路径绑定出 `log.Config` 后再显式初始化日志，避免“读取配置”暗含全局副作用。Web 也不得绕过 owner 去读 `log.level`：Gin mode 根据 logger capability 决定，而不是认识 runtime 拥有的配置键。

## 7. 公开 API 归属

| API | canonical owner |
|---|---|
| 面向应用的 `App` / `New(...Option)` / `Run()` / `(*App).Execute(...)` / `Option` / `WithDefinitions` | 根包 `xbc`（稳定六符号门面） |
| App 实现、执行流程、进程入口与 runtime options | `runtime`（低层公开 API；面向高级嵌入与自定义进程适配） |
| 私有核心 `settings` 与 `xbc.*`（`shutdown_timeout` / `auto_migrate`） | `runtime/settings.go`（不导出 `Settings`） |
| `Command` / `ParseArgs` / `Command.WantsMigration` | `cli` |
| `Container` / `Options` / `Instance` 及装配方法 | `assembly` |
| `FieldSpec` / `Kind` / `Scan` / `Set` / `Value` / `IsZero` | `assembly/inject` |
| marker `Plugin` / 可选 `Base` / `Context` / `RuntimeHost` / `Identity` / `Extension[T]`；Context 装配 API | `plugin` |
| `Key` / `Definition` / `Factory` / `Cardinality` / activation | `plugin` |
| `Catalog` / `Snapshot` / `New` / `Declare` / `Freeze` / `Snapshot.Lookup` | `plugin/catalog` |
| 通用 lifecycle capability、依赖引用、Provide/Get/Extensions | `plugin` |
| `Environment` / 只读 `View` / `Options` / source options / `Validate` | `config` |
| `Logger` / `Trace` | `log` |
| `Graph` / hard-soft edge / `Direction` / `Miss` / ordering errors | `plugin/ordering` |
| `Server` / `Router` / route catalog / current route / HTTP middleware capability | `web` |
| HTTP server config（`addr` / `base_path` / timeouts） | `web` |
| gRPC server、service/interceptor capability 与配置 | `grpc`（实现时建立） |
| health contributor/聚合契约 | `management/health`（实现时建立） |

根门面是普通应用的首选兼容面；`runtime` / `assembly` / `cli` 则是有意可导入的低层公开 API，不应再描述为私有实现或仅限包间协作。它们的导出符号必须有清晰文档；在首个正式版本前若要调整，也必须作为明确的公开 API 变更同步更新调用方、测试和文档。若应用只需常规启动或嵌入，仍应优先 import `xbc`；需要新增高层稳定能力时，先决定是否应进入根门面，再显式更新六符号守卫。

`Plugin` 不承担身份方法；身份由 `Key` 单点拥有。`Definition.Instances` 是 cardinality 的唯一入口，依赖引用只接受 Key，避免身份、实例形态和依赖边出现第二来源。`HealthChecker` 已从核心 SPI 删除，等 `management/health` 有真实使用者时再定义。

`config.Config` 改为 `config.Environment`。根包不重导出 `Plugin`、`Base`、`Dep`、`Context` 等 SPI alias，插件作者直接 import `plugin`；HTTP 类型更不能在根包保留 alias，否则 core 会重新依赖 Gin。旧 `ServerConfig` 已拆成 `web` 的 `plugins.web.*` 与 runtime 的 `xbc.*`，不兼容读取旧 `server.*` 顶层节。

## 8. 现有文件迁移映射

| 旧文件/目录 | 最终落点 | 说明 |
|---|---|---|
| 旧单体 `xbc.go` 与根运行时文件 | 重建后的根 `xbc.go` 薄门面；`runtime/{app,run,bootstrap,execute,host,lifecycle,...}.go`；`plugin/definition.go`、`plugin/catalog/catalog.go` | 应用入口与运行实现分离，不展平为一个 package |
| `plugin.go` | `plugin/{plugin,identity,definition,lifecycle}.go` | Plugin 改为 marker，Key 是唯一身份；Web capability 不进入 core |
| `deps.go` | `plugin/dependency.go` | 依赖引用统一为 `RefTo` / `Key.Ref()` |
| `context.go` | `plugin/{context,host}.go` + `web.CurrentRoute` / `RouteCatalog` | Context 只见只读配置与窄 RuntimeHost；Route/Routes 移出 core |
| `registry.go`、旧 `internal/container/registry.go` | `assembly/registry.go` + `plugin/{registry,extension}.go` | 值存储归装配 owner，泛型门面及来源 Identity 归 plugin |
| `config.go` | `config/*` + `runtime/settings.go` + `transport/web/config.go` | Environment/View、core settings 与 Web config 各归 owner |
| `cli.go`、旧 `internal/cli/*` | `cli/command.go` | 保留命令包边界并整体提升到仓库根；生产闭包 stdlib-only |
| `stage_expand.go`、旧 `internal/container/expand.go` | `assembly/expand.go` | 按 Definition Key 检查孤儿配置，按 cardinality 静态展开 |
| `stage_resolve.go`、旧 `internal/container/resolve.go` | `assembly/resolve.go` | 依赖、产物选择和拓扑排序归装配 owner |
| `stage_config.go`、旧 `internal/container/bind.go` | `config/bind.go` + `assembly/bind.go` | 通用绑定机制与实例遍历分开 |
| `stage_init.go`、旧 `internal/container/initialize.go` | `assembly/initialize.go` + `runtime/lifecycle.go` | 单实例注入/收割与跨实例生命周期编排分包协作 |
| 旧 `internal/inject/*`、`internal/container/inject/*` | `assembly/inject/scan.go` | 反射叶子随装配 owner 迁移并保持 stdlib-only |
| `stage_run.go`、旧 `internal/runtime/*` | `runtime/{execute,lifecycle,shutdown,process}.go` + `transport/web/server.go` | 通用 lifecycle 归 runtime，HTTP 整体迁出，signal 唯一落在 runtime/process.go |
| `router.go` / `middleware.go` / `mwchain.go` | `transport/web/{router,middleware,middleware_order}.go` | Gin 专属能力全部归独立 transport/web module |
| `startuplog.go`、旧 report 包 | `runtime/startup_report.go` + `transport/web/startup_report.go` | 每个 owner 只报告自己的数据，core render 保持纯函数 |
| `goroutine.go` | `runtime/task.go` | 托管任务、准入开关、critical escalation 与关机状态 |
| `internal/conf/*` | `config/{source,bind,validate}.go` | 同一配置能力内聚，不保留缩写包 |
| `internal/graph/*`、旧根 `topology/` | `plugin/ordering/graph.go` | 归入 plugin owner 的 stdlib-only 排序叶子 |

关键变化是**提升目录而不破坏包边界**：`runtime/`、`assembly/`、`assembly/inject/`、`cli/` 成为仓库根下的 canonical package；`package xbc` 只留下应用门面。旧 `internal/runtime`、`internal/container`、`internal/cli` 不与新路径并存。

### 8.1 测试怎么跟着搬

测试跟随 owner：生命周期、进程、任务和 core 报告白盒测试位于 `runtime/`；expand/resolve/initialize/registry 位于 `assembly/`；反射扫描位于 `assembly/inject/`；命令解析位于 `cli/`；Web 测试位于 `transport/web` module。公开根门面由 `tests/integration/public_api_test.go` 以外部包视角验证，全仓布局守卫位于 `tests/architecture`。

OS signal 与可嵌入执行分开测试：普通生命周期测试调用 `runtime.App.Execute` 并取消 caller context；只有 `runtime/process_test.go` 驱动真实 SIGINT/SIGTERM。Web 继续按两阶段 readiness 验证：`Start` 完成 bind，`OpenTraffic` 才开始 Serve。

架构守卫按问题选择证据：直接 import 用 `Imports/TestImports/XTestImports`，传递纯净性用 `Deps`，发布边界读 `go.mod` / `go.work`，默认 Catalog 和进程 owner 用 AST。根 module 的 `go test ./...` 不覆盖嵌套 module，CI 必须逐 module 执行。

## 9. 架构守卫

下列规则持续有效；涉及尚未创建的 module 时，等实现落地再增加可执行守卫：

1. **Core 纯净性**：core module 的生产闭包禁止 Gin、Google gRPC、`transport/*`、`management/*`、`integration/*`；
2. **SPI 纯净性**：`plugin` 禁止根门面、catalog、`runtime`、`assembly`、`cli`、internal 与 `transport/*`；`plugin/catalog` 只依赖 `plugin` 和标准库；
3. **配置/日志独立性**：`config` 与 `log` 的依赖闭包互不包含对方，也不得向上依赖 plugin 或上层能力包；
4. **叶子纯净性**：`plugin/ordering`、`assembly/inject`、`cli` 只允许标准库；
5. **装配隔离**：`assembly` 可以接收 `plugin/catalog.Snapshot`，但不得调用包级 `catalog.Declare` / `catalog.Freeze`；只有 `runtime` 选择定义集，assembly 只装配输入；
6. **运行栈独立性**：`transport/web` 不得依赖将来的 `transport/grpc`，反之亦然；
7. **无横向偷读**：可选运行栈通过 `plugin.Extensions` 消费能力，不得直接 import 根门面、`runtime`、`assembly`、`cli` 或 core internal 读取装配状态；
8. **发现确定性**：全局只保存 Definition；只有未传 `WithDefinitions` 的 `New` 冻结默认 Catalog，私有 Snapshot 不读取默认 Catalog；禁止全局 live instance 和 `App.Register`；
9. **module 验收**：CI 分别验证 core、transport/web、examples；core `go.mod` 不得 require 可选 module；子 module 不得含本地 `replace` 或未发布占位版本；
10. **仓库内分层不倒置**：`transport/web` 等可选实现不得反向依赖根门面或 runtime/assembly/cli；`examples` 保持只使用推荐的根门面。该仓库守卫不禁止外部高级宿主有意使用这些低层公开 API；
11. **Web 不得依赖根门面**：`transport/web` 的直接 import 里不得出现 `github.com/xbcio/xbc`。canonical 类型从 `plugin`、`plugin/ordering`、`config`、`log` 导入；
12. **canonical path 稳定**：`runtime/`、`assembly/`、`assembly/inject/`、`cli/`、`transport/web/` 必须存在；旧 `internal/runtime`、`internal/container`、`internal/cli`、`internal/inject`、`internal/report`、`internal/startupreport`、`internal/architecture`、`topology/` 与根级 `web/` 不得复活；`transport/` 根下不得出现 `.go` 文件或 `go.mod`；
13. **进程适配唯一 owner**：`runtime` 生产代码中的 `os.Args`、`os.Exit`、`os.Stderr`、signal、`log.Sync` 和包级 `osExit` 只允许出现在 `runtime/process.go`；
14. **关键 API 形状**：锁定 Plugin marker、Definition identity、Context Config、四方法 RuntimeHost 与 ordering hard/soft edge 形状；
15. **根门面冻结**：根包只允许导出 `App`、`App.Execute`、`New`、`Option`、`Run`、`WithDefinitions`。

当前自动化覆盖：

- `tests/architecture/architecture_test.go` 检查 core 可选依赖、下层反向 import、三个 stdlib-only 叶子、config/log 独立、assembly 不读取默认 Catalog、canonical/retired path、`transport/` 纯命名空间、六符号根门面、`runtime/process.go` owner、workspace manifest 与关键 API shape；
- `plugin/arch_test.go` 用生产依赖闭包禁止根门面、runtime/assembly/cli、internal 和协议栈，并约束第三方依赖增量；
- `transport/web/arch_test.go` 遍历 transport/web module 全部 package，禁止直接 import 根门面、runtime/assembly/cli 和 core internal；
- `examples/quickstart/arch_test.go` 遍历 examples module，禁止示例绕过根门面直接依赖 runtime/assembly/cli 或 core internal；
- 行为测试分别验证普通 `transport/web` import 无注册副作用、显式 `transport/web/autoload` 才注册。

### 9.1 判据必须匹配问题：直接 import、依赖闭包、`go.mod` 与 AST

“消费者是否亲自越界”必须检查直接 import，不能检查闭包。`examples` 合法 import 根门面，而根门面会合法地继续依赖 `runtime`，runtime 又依赖 `assembly` / `cli`；因此 examples 的 `Deps` 必然包含这些低层 owner。闭包无法区分“通过门面合法间接使用”与“示例源码直接选择低层 API”。`transport/web` 的方向规则同理读取每个 package 的 `Imports`、`TestImports`、`XTestImports`。

反过来，“最终会不会把某个依赖编译进来”必须读 `Deps`。plugin 协议无关性、三个叶子的 stdlib-only、config/log 独立性和 core 可选栈纯净性都属于闭包问题。没有源码引用的陈旧 `require` 不会出现在 import 图里，所以 module 纯净性还要直接解析 `go.mod`。

assembly 合法使用 `catalog.Snapshot` 类型，但不得调用默认 Catalog 的 `Declare` / `Freeze`；这不是普通 import 规则，守卫解析 `assembly/` 生产源码 AST 并匹配真实调用表达式。进程设施 owner 同样用 import-aware AST 扫描 `runtime/`。

CI 与本地统一从仓库根执行：

```sh
make check      # gofmt 检查，并逐 workspace module 执行 go vet / go test
make test-race  # 逐 workspace module 执行 race test
```

`scripts/for-each-module` 从 `go.work` 动态发现 module。发布时还需在禁用 workspace 的环境验证已发布依赖；core 尚无真实 tag 时，下游独立解析失败是预期，不得用本地 `replace`、`v0.0.0` 或伪版本掩盖。

## 10. 已完成的落地顺序与持续验收

以下是已完成迁移记录，不是待执行计划：

1. **建立基础 owner 与叶子**：配置归 `config`，排序归 `plugin/ordering`，反射扫描归 `assembly/inject`，命令解析归 `cli`；后三者生产闭包 stdlib-only；
2. **统一发现入口**：建立 Definition 与可冻结 Catalog/Snapshot，实现零参数 `Run()`、`New(...Option)`、`WithDefinitions`，删除 `App.Register` 和全局 live instance；
3. **建立协议无关 SPI**：落地 marker Plugin、Base、Key、Cardinality、生命周期 capability、RuntimeHost 与 Extensions，并断开 config/log 相互依赖；
4. **保留 assembly 边界**：实例展开、绑定、依赖解析、registry、inject/harvest 与初始化状态归 `assembly`，跨实例生命周期编排归 `runtime`；
5. **重做 runtime 任务与关机**：准入开关式 task group、有界 unwind、panic recover、共享 deadline 和应用级 shuttingDown 已落地；
6. **分离进程适配并实现两阶段 readiness**：根 `xbc.Run` 委托 `runtime.Run`，OS 设施集中在 `runtime/process.go`；`App.Execute` 保持可嵌入；
7. **抽离 Web 运行栈**：Router、Middleware、HTTP server 与报告迁入独立 `transport/web` module，普通 import 无副作用，显式 autoload 才声明默认 Definition；
8. **建立 module 与守卫边界**：transport/web 与 examples 各自持有独立 `go.mod`，根 `go.work` 联调，测试覆盖直接 import、闭包、manifest、AST 与 API shape；
9. **完成旧路径收尾**：删除旧 stage、conf/graph/report/mwchain、根 HTTP alias 和临时兼容面，排序由旧 `topology/` 迁到 `plugin/ordering/`；
10. **提升能力 owner 而不展平**（2026-08-28）：`internal/runtime`、`internal/container`、`internal/cli` 连同子目录提升为根级 `runtime/`、`assembly/`、`cli/`；`assembly/inject/` 随 owner 提升；根 `package xbc` 仅保留薄门面；
11. **未来 gRPC 继续复验边界**：若实现 gRPC 必须修改具体 runtime/assembly 逻辑才能接入，先检查 SPI 是否泄漏协议概念，而不是继续加特例。

后续变更仍必须通过对应 module 的 `gofmt`、`go vet`、`go test`（含 `-race`）和架构守卫。

**发布顺序固定为 core → `transport/web` → `examples`。** 在 core 推送真实 tag 前，禁用 `go.work` 的下游构建失败是预期；不得用本地 `replace`、`v0.0.0`、伪造 pseudo-version、预发布或其他占位版本糊过去。

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
| 本次先建空的 `transport/grpc/`、`management/health/`、`integration/*` 目录占位 | 空目录和占位 API 是没有使用者的契约，只会在真正实现时被推翻；目录布局的说服力来自能跑通的代码，不是提前挖好的坑 |
| core 按名字识别 `web`/`grpc` 插件来决定何时开放流量 | 那是把协议知识写死进内核，第三个运行栈出现时只能加第三个 `if`；两阶段 readiness 用类型断言 `TrafficOpener` 表达同一件事，且对未知运行栈天然成立（§5.1） |
| 把启动 hook 丢进 detached goroutine 并试图由框架强制中断 | Go 无法安全抢占任意插件代码，还会让 unwind 的 `Stop` 与未返回 hook 并发；正确方案是同步 hook + 实现 `context.Context` 的 `plugin.Context` 协作取消，并在步骤之间补充停止检查（§5.0） |
| `shutdown_timeout` 继续留在 `server.*` 配置节 | 让 Web 配置节决定一个 gRPC-only 或纯任务型应用的关机预算，而那个应用根本不该有 `server` 节（§5.3） |
| 关机时给每个插件各自一份 `shutdown_timeout` | 预算是「这个进程多久必须消失」，N 个插件各等一份就是 N 倍进程寿命；deadline 必须共享（§5.3） |
| 用 `context.WithTimeout` + `Stop(ctx)` 就算实现了有界关机 | `Stop` 收到 ctx 却选择不看，框架仍然会永远阻塞在那一行；有界性只能由调用方的受控 goroutine + select 保证，必要时故意泄漏卡住的 goroutine（§5.3） |
| task group 直接 `cancel` 后 `wg.Wait()` | 与 `wg.Add` 存在竞态：关机瞬间提交的任务可能在 `Wait` 返回后才 `Add`；必须先关准入再 cancel/wait（§5.4） |
| 架构守卫用「依赖闭包不含 internal」判定外部 module | 公开 owner 可以合法使用私有实现，下游闭包因此可能包含 internal；跨 module 守卫要拦的是消费者自己越界，必须看**直接 import**（§9.1） |
| 给子 module 写本地 `replace`、`v0.0.0`、伪造 pseudo-version 或占位 require 让发布演练变绿 | 那不是通过验证，是取消验证：真实上游 tag 尚未发布时应暂缺 require；tag 可解析后再添加并用 `GOWORK=off` 验证（§10） |
| 没有任何 Runner/托管任务时静默 `select{}` 等下去 | 一个不会响应任何请求的进程装作健康活着，是最难排查的一类故障；必须启动失败并列出装配了什么（§5.2） |

这套布局的判断标准只有一个：**看到 import path 就知道它属于应用内核、插件契约、Web、gRPC，还是某项外部集成；普通应用入口只有 `xbc.Run()`，删除任一可选插件后 core 仍能独立编译和运行。**
