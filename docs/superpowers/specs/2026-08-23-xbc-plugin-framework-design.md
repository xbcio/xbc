# xbc — 插件化 Go Web 框架设计

- **日期**：2026-08-23
- **仓库**：`git@github.com:xbcio/xbc.git`
- **Module**：`github.com/xbcio/xbc`
- **状态**：设计定稿，待实现

---

## 1. 目标

基于 Gin 构建一个开箱即用的 Web 框架，**以插件体系为核心**——框架内核只负责装配与请求契约，其余一切能力（数据库、缓存、认证、限流、定时任务、业务模块）均以插件形式接入。

衡量设计是否成功的标准：

1. 新增一个能力，**不需要修改框架任何一行代码**——写一个插件，import 进来即可
2. 关闭一个能力，**不需要重新编译**——注释掉配置节即可
3. 插件之间的依赖关系是**声明式的**，启动顺序与关闭顺序由框架推导，不靠约定
4. 配置写错、依赖缺失、顺序成环，**在启动装配阶段就报出可执行的错误**，不漏到运行时

### 1.1 非目标

以下明确不在本次范围内，列出以防范围蔓延：

- **CLI 脚手架与代码生成器**（类似 hertz 的 `hz`、go-zero 的 `goctl`）。以可运行的 `examples/` 作为脚手架替代，`cp -r` 即用
- **配置热重载**。它会污染所有插件接口（每个插件都得处理「配置变了怎么办」），收益却只对网关类产品明显。留到插件接口稳定后再评估
- **运行时动态加载插件**（`.so` / WASM / 子进程 RPC）。插件为编译期静态注册，产出单一二进制
- **微服务治理**（服务注册发现、熔断、分布式事务）

---

## 2. 核心决策

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 插件载入方式 | **编译期静态注册**，单一二进制 | 零运行时开销、类型安全、可调试。Go plugin `.so` 限制过多（版本必须完全一致、不支持交叉编译、无法卸载） |
| 2 | 插件承载能力 | 基础设施 / HTTP 中间件 / 业务模块 / 后台任务，**四类全支持** | 四类各有一个首批插件压测接口设计 |
| 3 | 依赖获取 | **双层并存**：显式类型注册表打底 + struct tag 注入糖衣 | tag 省样板并让框架自动推导依赖图；`MustGet` 兜住动态场景 |
| 4 | 配置绑定 | **约定式自动绑定 + 支持多实例** | 插件只声明 Config 结构体，不写读配置代码；多数据源/读写分离是真实需求 |
| 5 | 仓库形态 | **单仓单 module** | 开发发版最简。二进制体积不受影响（Go 链接器只打包实际 import 的包），代价是 `go.sum` 大、CVE 扫描噪音、依赖版本冲突面大。目录按 `plugins/<name>/` 切干净，将来可拆 |
| 6 | 工具链 | **不做 CLI**，纯框架库 + examples | 先把插件接口设计对 |
| 7 | 插件接口形态 | **窄接口组合为底，额外提供可选 `Base`** | 插件只实现关心的接口；新增扩展点对既有插件零影响 |
| 8 | 排序机制 | **声明式**：`Phase` 粗锚点 + `After`/`Before` 细调，无魔数 | `OrderRateLimit + 50` 是魔数，第三方之间会撞，且表达不出真实意图 |
| 9 | 依赖表达 | 硬依赖（`Types` / `Plugins`）与软顺序（`After` / `Before`）**分离** | 二者语义不同：硬依赖缺失应中止启动，软顺序缺失应静默忽略 |
| 10 | 插件名引用 | 硬依赖用**类型引用**（编译期安全），软依赖用字符串 | 判据：能用类型引用的是硬依赖；必须用字符串的是软依赖 |
| 11 | 内核边界 | `recovery` / `requestid` / `accesslog` **内建，非插件** | 它们是框架契约本身，关掉会让统一响应的 `traceId` 和日志契约同时失效 |
| 12 | 配置命名空间 | 插件配置**保留 `plugins.` 前缀** | 与「配置节不存在则不启用」构成闭环，使「有配置但无插件」可被诊断 |
| 13 | HTTP 错误表达 | **语义化状态码** + body 携带业务码 | 网关熔断、Prometheus 告警、CDN 策略可直接用状态码，无需解包 |

---

## 3. 架构总览

```
┌──────────────────────────────────────────────────────────┐
│  xbc 内核                                                 │
│                                                          │
│  装配引擎          HTTP 骨架         请求契约              │
│  ├ 配置加载/绑定    ├ gin 封装        ├ recovery           │
│  ├ 拓扑排序         ├ Router 元数据   ├ requestid          │
│  ├ 依赖解析/注入    ├ 优雅关闭        ├ accesslog          │
│  └ 生命周期管理     └ healthz         └ 统一响应/错误       │
│                                                          │
│  内核依赖：gin + koanf + validator + slog                 │
└──────────────────────────────────────────────────────────┘
                            ▲
                            │ 实现可选接口接入
                            │
┌──────────────────────────────────────────────────────────┐
│  插件层                                                   │
│  gorm  redis  cors  ratelimit  jwt  cron  ...业务模块     │
└──────────────────────────────────────────────────────────┘
```

**内核不认识任何具体插件**；插件也不互相 import（硬依赖除外，那是刻意为之的显性化）。二者通过三个协作面交互：

1. **类型注册表** —— 插件提供/消费类型实例（`*gorm.DB`）
2. **路由元数据** —— 插件标注/查询路由属性（`.Public()` / `.Perm()`）
3. **依赖声明** —— 插件声明关系，框架推导顺序

---

## 4. 装配管线

框架启动是一条**分阶段流水线**。阶段边界清晰，插件作者才知道「我该在哪一步做什么」。

```
编译期
  Register ················ init() 或 app.Register() 把插件实例塞进待装配列表
                            仅收集，不执行任何逻辑

运行期
  1. LoadConfig ··········· application.yml + profile + ENV + flag，此时无插件被初始化
  2. Expand ··············· 读 plugins.<name>[.<instance>]，按具名实例克隆插件
                            enabled:false / 无配置节的直接剔除
                            —— 本步把「插件类型」变成「插件实例」
  3. BindConfig ·········· unmarshal → 填 default tag → 跑 validate
  4. Resolve ············· 扫 inject tag + 读 Dependencies()，合并成依赖图，拓扑排序
                            检测环 / 缺失硬依赖 → 启动中止
  5. Init ················ 按拓扑序，逐插件：Inject → Init → Provide
  6. Migrate ············· 全部 Init 完成后，按拓扑序调用 Migrator
  7. AssembleHTTP ········ 收集全部 Middleware 排序装载 → 调 RegisterRoutes
  8. Start ··············· 并发拉起所有 Runner
  9. Serve ··············· HTTP server 阻塞运行
                                  ↓ SIGINT / SIGTERM
 10. Shutdown ··········· 停收新请求 → 等 in-flight 排空 → 停 Runner
                          → 逆拓扑序调 Stop() → 超时兜底强杀
```

### 4.1 关键约束

**(a) 逆拓扑序关闭。** `user` 插件依赖 `gorm`，关闭时必须先停 `user` 再停 `gorm`，否则 `user` 手里正在跑的事务会被拔掉连接。关闭顺序 = 启动顺序取逆，由框架保证。

**(b) 启动失败必须回滚。** 阶段 5~8 任一步出错，对**已经 Init 成功的插件**逆序调用 `Stop()` 再退出。否则 MySQL 连上了、Redis 连失败，进程退出时那些连接泄漏在服务端。

**(c) Expand 早于 BindConfig。** 先展开实例再绑配置，「两个数据源」才是两个独立的校验单元——其中一个 DSN 写错，报错能精确到 `plugins.gorm.readonly.dsn`。

**(d) 阶段 5 的注入时机。** 拓扑序保证依赖已 Init 完毕，所以对插件 X 执行 `Inject` 时，X 的所有依赖必然已经 `Provide` 过成品。

**(e) Migrate 独立成阶段 6。** 迁移可能需要多个插件同时就绪，所以放在全部 `Init` 完成之后统一执行，而非混在阶段 5 里。

### 4.2 子命令映射

应用二进制自带子命令，用标准库 `flag` 实现，**不引入 cobra**：

| 命令 | 执行阶段 | 用途 |
|---|---|---|
| `./myapp` | 1~9 | 正常启动 |
| `./myapp --config ./prod.yml --profile prod` | 1~9 | 指定配置 |
| `./myapp --skip-migration` | 1~5, 7~9 | 生产环境跳过迁移 |
| `./myapp migrate` | 1~6 后退出 | 只跑迁移，不起 HTTP |
| `./myapp doctor` | 1~4 后退出 | 打印装配计划，不建任何连接 |

`doctor` 是白送的——阶段 1~4 本就不产生副作用，跑完打印依赖图和中间件链即可，用于 CI 中校验配置。

### 4.3 用户侧入口

```go
package main

import (
    "github.com/xbcio/xbc"
    _ "github.com/xbcio/xbc/plugins/gorm"    // 副作用注册
    _ "github.com/xbcio/xbc/plugins/redis"
    _ "github.com/xbcio/xbc/plugins/jwt"
    "myapp/plugins/user"
)

func main() {
    app := xbc.New()
    app.Register(user.New())   // 业务插件显式注册
    app.Run()
}
```

框架自带插件靠 blank import 注册；业务插件显式 `Register`——**不强制业务代码用 `init()` 副作用**，因为业务插件常需要构造参数。

### 4.4 启动日志

装配结果必须摊开打印。这是对「窄接口方法名拼错 → 框架静默跳过」的兜底：

```
xbc: 装配完成，6 个插件实例
  gorm[default]   config init health stop         provides *gorm.DB
  gorm[readonly]  config init health stop         provides *gorm.DB[readonly]
  redis[default]  config init health stop         provides *redis.Client
  jwt             config middleware               consumes 路由元数据
  cron            config init runner stop         requires *gorm.DB
  user            routes(12) migrate              requires *gorm.DB *redis.Client

xbc: 中间件链（7）
  1. xbc.recovery      [recover]
  2. xbc.requestid     [observe]
  3. xbc.accesslog     [observe]
  4. cors              [security]
  5. ratelimit         [security]  after=cors
  6. jwt.auth          [auth]
  7. myapp.audit       [business]  after=jwt.auth

xbc: 软约束未命中（不影响启动）
  audit.After = "tracing" —— 无此插件，忽略
    → 拼写错误？还是忘了启用 plugins.tracing？
```

---

## 5. 插件契约

### 5.1 接口全集

```go
package xbc

// ── 唯一必须实现的 ──────────────────────────────────────
type Plugin interface {
    Name() string
}

// ── 可选能力：实现哪个，就获得哪个能力 ────────────────────
type Configurable       interface { ConfigPtr() any }
type MultiInstancer     interface { MultiInstance() bool }
type Declarer           interface { Dependencies() Deps }
type Initializer        interface { Init(ctx *Context) error }
type Migrator           interface { Migrate(ctx *Context) error }
type MiddlewareProvider interface { Middlewares() []Middleware }
type RouteProvider      interface { RegisterRoutes(r *Router) }
type Runner             interface { Start(ctx *Context) error }
type Closer             interface { Stop(ctx context.Context) error }
type HealthChecker      interface { Health(ctx context.Context) error }
```

| 接口 | 调用阶段 | 典型实现者 |
|---|---|---|
| `Configurable` | 3 BindConfig | 几乎所有插件 |
| `MultiInstancer` | 2 Expand | gorm、redis |
| `Declarer` | 4 Resolve（与 tag 合并） | 有硬依赖或顺序要求的插件 |
| `Initializer` | 5 Init | 基础设施类 |
| `Migrator` | 6 Migrate | 业务模块 |
| `MiddlewareProvider` | 7 AssembleHTTP | 中间件类 |
| `RouteProvider` | 7 AssembleHTTP | 业务模块 |
| `Runner` | 8 Start | cron、consumer |
| `Closer` | 10 Shutdown（逆拓扑序） | 持有资源的插件 |
| `HealthChecker` | `/healthz` 被访问时 | 基础设施类 |

**窄接口方案下不需要空实现**，所以 `Base` 的职责与「胖接口 + Base」模式完全不同——见 5.4。

### 5.2 `Runner` 语义：非阻塞 + 托管 goroutine

`Start` **必须快速返回**。长期循环通过 `ctx.Go` 交给框架托管，框架负责 panic 恢复、shutdown 时 cancel、退出前等待：

```go
func (p *CronPlugin) Start(ctx *xbc.Context) error {
    p.c.Start()
    ctx.Go(func(c context.Context) {   // 托管：panic 不会打挂进程
        <-c.Done()
        <-p.c.Stop().Done()            // 等待在途任务跑完
    })
    return nil
}
```

`Start`/`Stop` 语义对称，插件作者不必自己管 `WaitGroup`。

### 5.3 依赖声明

四种关系是同一张依赖图上的不同边，聚合成一个方法而非散成四个接口：

```go
type Deps struct {
    Types   []Dep     // 类型依赖（硬）：我要拿到 *gorm.DB
    Plugins []Ref     // 插件依赖（硬）：jwt 必须启用，哪怕它不给我任何东西
    After   []string  // 顺序偏好（软）：有它就排我前面，没有拉倒
    Before  []string  // 顺序偏好（软）
}
```

| 关系 | 表达什么 | 缺失时 | 影响拓扑序 |
|---|---|---|---|
| `Types` | 我需要**某类型的实例** | **启动中止** | 是 |
| `Plugins` | 我需要**某插件在场** | **启动中止** | 是 |
| `After` / `Before` | 我**偏好**的相对位置 | 静默忽略（记入软约束未命中日志） | 是 |

```go
type Dep struct {
    Type     reflect.Type
    Instance string    // "" 表示 default
    Optional bool
}

func Need[T any]() Dep                        // 必需，default 实例
func NeedNamed[T any](instance string) Dep    // 必需，具名实例
func Opt[T any]() Dep                         // 可选

type Ref struct {
    typ      reflect.Type
    instance string    // "" 表示任一实例
}

func RefOf[T Plugin]() Ref                    // 编译期安全的插件引用
func (r Ref) Instance(name string) Ref        // 收窄到具名实例
```

`Plugins` 支持两种粒度：

```go
xbc.RefOf[*gorm.Plugin]()                     // gorm 至少一个实例启用即可
xbc.RefOf[*gorm.Plugin]().Instance("readonly") // 必须有 readonly 实例
```

用法：

```go
func (p *AuditPlugin) Dependencies() xbc.Deps {
    return xbc.Deps{
        Plugins: []xbc.Ref{xbc.RefOf[*jwt.Plugin]()},   // 硬：拿不到用户身份就没法审计
        After:   []string{"tracing"},                    // 软：有链路追踪就排它后面
    }
}

func (p *ReportPlugin) Dependencies() xbc.Deps {
    return xbc.Deps{
        Types: []xbc.Dep{xbc.NeedNamed[*gorm.DB]("readonly")},  // 报表只走只读库
    }
}
```

### 5.4 消灭手写插件名

三类名字，处理方式不同：

**(a) 插件自身的 `Name()` —— 自动推导。** 嵌 `xbc.Base` 后，框架在 `Register` 时反射取具体类型的包路径末段：

```go
// github.com/xbcio/xbc/plugins/jwt
type Plugin struct{ xbc.Base }        // Name() 自动 = "jwt"
func (p *Plugin) Name() string { return "jwt-v2" }   // 想覆盖就自己写，方法遮蔽
```

**(b) 硬依赖 —— 类型引用，编译期安全。** `RefOf[*jwt.Plugin]()` 拼错即编译失败、改名 IDE 自动跟随、可跳转可查引用。引入 import 不是副作用而是把隐性依赖显性化——audit 既然硬依赖 jwt，`go mod graph` 里就该看见这条边。

**(c) 软依赖 —— 只能是字符串，这是它的本质。** 若为了写 `After` 而 import 对方，对方就被编译进二进制了，**软依赖当场变成硬依赖**。所以保留字符串是正确的，不是妥协。拼错的兜底靠 `doctor` 与启动日志中的「软约束未命中」段落。

判据：**能用类型引用的 = 硬依赖；必须用字符串的 = 软依赖。**

**(d) 实例名 —— 必然是字符串。** `xbc:"inject,name=readonly"` 里的 `readonly` 是用户在 yml 里现取的名字，框架不可能预置常量。写错会在阶段 4 直接报「gorm 只有 default 实例」，不会漏到运行时。

### 5.5 `Context` 与 `Base`

```go
type Context struct { /* app / registry / config / logger */ }

func (c *Context) Log() *slog.Logger            // 自动带 plugin=gorm instance=readonly
func (c *Context) Config() *Config
func (c *Context) Instance() string             // "default" / "readonly"
func (c *Context) Go(fn func(context.Context))  // 托管 goroutine
func (c *Context) Routes() []RouteInfo          // 查询路由元数据

type Base struct{ ctx *Context }
func (b *Base) Ctx() *Context     { return b.ctx }
func (b *Base) Log() *slog.Logger { return b.ctx.Log() }
func (b *Base) Name() string      // 从包路径自动推导
```

`Base` 的职责是**便利访问器 + `Name()` 自动推导 + tag 注入锚点**，不是空实现载体。不嵌 `Base` 照样是合法插件——`Init(ctx)` 参数里什么都有。

### 5.6 注册表：键 = (类型, 实例名)

```go
func Provide[T any](ctx *Context, v T)                     // 用插件自己的实例名登记
func Get[T any](ctx *Context) (T, bool)                    // 取 default
func GetNamed[T any](ctx *Context, name string) (T, bool)
func MustGet[T any](ctx *Context) T
func MustGetNamed[T any](ctx *Context, name string) T
```

`gorm[default]` 与 `gorm[readonly]` 是同一份插件代码的两个实例，各自 `Provide(ctx, db)` 时框架自动用**自己的实例名**做键——插件代码完全不需要感知自己是第几个实例。

### 5.7 两种注入写法并存

```go
// 写法一：tag —— 框架据此自动推导依赖图，无需写 Dependencies
type UserPlugin struct {
    xbc.Base
    DB    *gorm.DB      `xbc:"inject"`
    RoDB  *gorm.DB      `xbc:"inject,name=readonly"`
    Cache *redis.Client `xbc:"inject,optional"`   // 缺失留零值，不报错
}

// 写法二：显式 —— 动态场景，如实例名来自配置
func (p *ShardPlugin) Dependencies() xbc.Deps {
    return xbc.Deps{Types: []xbc.Dep{xbc.NeedNamed[*gorm.DB](p.Cfg.Shard)}}
}
func (p *ShardPlugin) Init(ctx *xbc.Context) error {
    p.db = xbc.MustGetNamed[*gorm.DB](ctx, p.Cfg.Shard)
    return nil
}
```

tag 语法：`xbc:"inject[,name=<实例名>][,optional]"`，作用于导出字段。两种写法在阶段 4 合并成同一张依赖图，可混用：

```go
type AuditPlugin struct {
    xbc.Base
    DB *gorm.DB `xbc:"inject"`                        // → 自动进 Deps.Types
}
func (p *AuditPlugin) Dependencies() xbc.Deps {
    return xbc.Deps{Plugins: []xbc.Ref{xbc.RefOf[*jwt.Plugin]()}}  // 只补 tag 表达不了的
}
```

### 5.8 排序：声明式，无魔数

```go
type Middleware struct {
    Name    string           // 唯一标识，供他人引用；不含 "." 时自动加插件名前缀
    Phase   Phase            // 粗粒度锚点
    After   []string         // 细粒度微调（软）
    Before  []string
    Handler gin.HandlerFunc
}

type Phase int
const (
    PhaseRecover  Phase = iota  // 最外层，panic 兜底      ← 内建 recovery
    PhaseObserve                // 可观测                 ← 内建 requestid、accesslog
    PhaseSecurity               // cors / ratelimit / 防重放
    PhaseAuth                   // 认证鉴权
    PhaseBusiness               // 业务中间件
)
```

内建的三个中间件也走同一套 `Phase` 机制装载，只是不可拔除——机制统一，没有第二条路径。

**排序规则：**

1. `Phase` 是**硬边界**，先按 Phase 分组
2. 组内按 `After`/`Before` 拓扑排序；无约束者按注册顺序稳定排列（保证可复现）
3. 引用不存在的名字 → **忽略**（软约束），记入「软约束未命中」日志
4. 跨 Phase 约束方向与 Phase 顺序一致 → 冗余但无害；**方向相反 → 启动中止报错**，而不是静默给出错误的洋葱

```go
// 绝大多数插件：只声明阶段
func (p *CORSPlugin) Middlewares() []xbc.Middleware {
    return []xbc.Middleware{{Name: "cors", Phase: xbc.PhaseSecurity, Handler: p.handle}}
}

// 需要精细控制：审计要拿到 JWT 解出的身份
func (p *AuditPlugin) Middlewares() []xbc.Middleware {
    return []xbc.Middleware{{
        Name: "audit", Phase: xbc.PhaseBusiness,
        After: []string{"jwt.auth"}, Handler: p.handle,
    }}
}
```

同一套拓扑排序器服务三处：**插件 Init 顺序**（硬依赖 ∪ 软顺序）、**中间件顺序**（Phase + 软顺序）、**Stop 顺序**（Init 顺序取逆）。框架内只有一份排序实现。

### 5.9 硬依赖缺失的报错

报错必须直接指出修法：

```
xbc: 依赖检查失败
  插件 audit 依赖插件 jwt，但 jwt 未启用
    → 在 application.yml 中添加 plugins.jwt 配置节
  插件 report 依赖 gorm[readonly]，当前只有 gorm[default]
    → 在 plugins.gorm 下添加 readonly 实例
  插件 user 需要 *redis.Client，无任何插件提供
    → 是否忘了 import github.com/xbcio/xbc/plugins/redis
```

环检测需指出环上的完整路径：

```
xbc: 依赖成环
  user → order → payment → user
```

---

## 6. 配置体系

### 6.1 来源与优先级

```
内建默认值 < application.yml < application-{profile}.yml < 环境变量 < 命令行 flag
```

- 文件查找序：`--config` 指定 → `./application.yml` → `./configs/application.yml`
- profile 由 `XBC_PROFILE` 或 `--profile` 指定，同名 key 覆盖基础文件，其余合并
- 环境变量映射：`XBC_PLUGINS_GORM_DEFAULT_DSN` → `plugins.gorm.default.dsn`（大写、`_` 转 `.`）
- 底座用 **koanf**——比 viper 轻，无隐式全局态

### 6.2 命名空间

```yaml
server:                      # 框架保留
  addr: :8080
  base_path: /api/v1
  read_timeout: 10s
  shutdown_timeout: 30s

log:                         # 框架保留
  level: info
  format: json               # json | console

plugins:                     # 插件命名空间
  gorm:
    default:
      dsn: root:pwd@tcp(127.0.0.1:3306)/app?parseTime=true
    readonly:
      dsn: root:pwd@tcp(10.0.0.2:3306)/app?parseTime=true
      max_open_conn: 50
  redis:
    default:
      addr: 127.0.0.1:6379
  cors:
    allow_origins: ["https://example.com"]
  jwt:
    secret: ${JWT_SECRET}    # 支持 ENV 插值
  cron:
    enabled: false           # 一行关掉

app:                         # 业务配置，框架不解析
  feature_x: true
```

保留 `plugins.` 前缀，使「有配置节但无对应插件」可被诊断：

```
xbc: plugins.kafka 有配置但无对应插件
  → 是否忘了 import github.com/xbcio/xbc/plugins/kafka？
```

这条诊断与「配置节不存在 → 插件不启用」构成闭环，专治最常见的翻车场景：**写了配置、忘了 import、静默不生效**。

### 6.3 声明即绑定

插件只声明结构体，**不写任何读配置的代码**：

```go
type Config struct {
    DSN           string        `yaml:"dsn"             validate:"required"`
    MaxOpenConn   int           `yaml:"max_open_conn"   default:"20"`
    MaxIdleConn   int           `yaml:"max_idle_conn"   default:"10"`
    ConnMaxLife   time.Duration `yaml:"conn_max_life"   default:"1h"`
    SlowThreshold time.Duration `yaml:"slow_threshold"  default:"200ms"`
}

func (p *GormPlugin) ConfigPtr() any { return &p.Cfg }
```

阶段 3：`unmarshal → 填 default → 跑 validate`，失败即中止，错误带完整路径：

```
xbc: 配置错误
  plugins.gorm.readonly.dsn   必填项缺失
  plugins.redis.default.addr  不是合法的 host:port —— 得到 "127.0.0.1"
```

### 6.4 启用规则

| 配置节 | 结果 |
|---|---|
| 不存在 | 插件**不启用**（import 了也不装配） |
| 存在（哪怕空 `{}`） | 启用，走默认值 |
| `enabled: false` | 显式关闭 |

「配置节不存在就不启用」的价值：临时停掉一个插件只需注释配置，**不用改代码重新编译**。`enabled` 字段由框架隐式注入，插件 `Config` 结构体不需要声明它。

### 6.5 多实例配置形状

由插件**显式声明**，不靠框架猜测：

```go
func (p *GormPlugin) MultiInstance() bool { return true }
```

| 声明 | 配置形状 |
|---|---|
| 实现 `MultiInstancer` 返回 `true` | `plugins.gorm.<实例名>.<字段>` |
| 未实现 | `plugins.cors.<字段>` |

不用自动探测的理由：若某插件 Config 恰好有个字段叫 `default`，探测就会错乱。

---

## 7. HTTP 层

### 7.1 路由注册：链式元数据

`RegisterRoutes` 接收 `*xbc.Router`（内嵌 `*gin.RouterGroup`，gin 原生方法全在），额外提供链式元数据：

```go
func (p *UserPlugin) RegisterRoutes(r *xbc.Router) {
    g := r.Group("/users")
    g.POST("/login", p.login).Public().Name("用户登录")
    g.GET("",        p.list).Perm("user:read")
    g.POST("",       p.create).Perm("user:write").Idempotent()
    g.DELETE("/:id", p.delete).Perm("user:delete")
}
```

**为什么值得做**：免鉴权白名单若写在 `plugins.jwt.exclude` 配置里，必然与代码脱节——新增一个登录接口忘了改配置，线上就是 401。`.Public()` 写在路由旁边，漏不掉。

元数据存在框架的路由表中，插件可查询——`jwt` 读 `Public()`，`casbin` 读 `Perm()`，`swagger` 读 `Name()`。**这是插件之间除类型注册表外的第二个协作面。** 配置里的 `exclude` 仍保留，作为运维侧的紧急覆盖手段。

```go
type RouteInfo struct {
    Method     string
    Path       string
    Name       string
    Public     bool
    Perm       string
    Idempotent bool
}
```

### 7.2 Handler 签名

```go
func H(fn func(c *gin.Context) (any, error)) gin.HandlerFunc
```

```go
func (p *UserPlugin) list(c *gin.Context) (any, error) {
    var req ListReq
    if err := c.ShouldBindQuery(&req); err != nil {
        return nil, xbc.ErrInvalidParam.Wrap(err)
    }
    users, total, err := p.svc.List(c, req)
    if err != nil {
        return nil, err              // 直接抛，框架统一处理
    }
    return xbc.Paged(users, total, req.Page, req.Size), nil
}
```

省掉每个 handler 里重复的 `c.JSON(...)`。原生 `gin.HandlerFunc` 也可直接注册，只是要自己写响应。

### 7.3 响应格式：语义化状态码

```
HTTP/1.1 200 OK
{ "code": "0", "data": {...}, "traceId": "01HQ8..." }

HTTP/1.1 404 Not Found
{ "code": "USER.NOT_FOUND", "message": "用户 42 不存在", "traceId": "01HQ8..." }
```

```go
type Response[T any] struct {
    Code    string `json:"code"`
    Message string `json:"message,omitempty"`
    Data    T      `json:"data,omitempty"`
    TraceID string `json:"traceId"`
}

type Paged[T any] struct {
    List  []T   `json:"list"`
    Total int64 `json:"total"`
    Page  int   `json:"page"`
    Size  int   `json:"size"`
}
```

选语义化状态码而非「一律 200」：网关熔断、Prometheus 告警、CDN 缓存策略都能直接用状态码，不必解包 body。

### 7.4 错误体系

```go
type Error struct {
    Code    string   // 业务码，分层命名："USER.NOT_FOUND"
    Message string   // 面向调用方
    Status  int      // HTTP 状态码
    cause   error    // 内部原因，只进日志，绝不出网
}

var (
    ErrInvalidParam = New("INVALID_PARAM", 400, "参数错误")
    ErrUnauthorized = New("UNAUTHORIZED",  401, "未认证")
    ErrForbidden    = New("FORBIDDEN",     403, "无权限")
    ErrNotFound     = New("NOT_FOUND",     404, "资源不存在")
    ErrConflict     = New("CONFLICT",      409, "状态冲突")
    ErrInternal     = New("INTERNAL",      500, "服务内部错误")
)

func (e *Error) WithMsg(f string, a ...any) *Error   // 返回副本，不改全局变量
func (e *Error) Wrap(cause error) *Error             // 挂内部原因
```

业务侧就地声明错误码：

```go
var ErrUserNotFound = xbc.New("USER.NOT_FOUND", 404, "用户不存在")
return nil, ErrUserNotFound.WithMsg("用户 %d 不存在", id).Wrap(err)
```

#### 安全约束（强制）

> **[SEC-INFO] 兜底层强制脱敏。** 任何非 `*xbc.Error` 的 error（`gorm.ErrRecordNotFound`、driver 报错、panic 恢复值）在出网前一律替换为 `ErrInternal` 的固定文案，真实错误链与堆栈只写日志。否则 SQL 语句、DSN、服务器路径会顺着 error message 泄漏给调用方。`cause` 字段小写不可导出且不参与 JSON 序列化，从类型层面杜绝误传。

> **[SEC-INFO] 日志脱敏。** `accesslog` 对请求体/响应体默认不记录；如开启，必须过滤字段黑名单：`password`、`token`、`access_token`、`refresh_token`、`secret`、`private_key`、`id_card`、`bank_card`、`phone`。配置项 `log.body.enabled`（默认 `false`）与 `log.body.mask_fields`。

> **[SEC-INFO] 配置回显禁止。** `doctor` 与启动日志打印配置时，对匹配敏感字段名的值一律显示为 `***`，尤其是 `plugins.gorm.*.dsn`（含密码）与 `plugins.jwt.secret`。

### 7.5 参数校验

`validator/v10` + 中文翻译内建。绑定失败自动转 `ErrInvalidParam`，`Message` 取翻译后的第一条：

```json
{ "code": "INVALID_PARAM", "message": "邮箱 必须是一个有效的邮箱", "traceId": "01HQ..." }
```

### 7.6 健康检查

内建 `GET {base_path}/healthz`，聚合所有 `HealthChecker`：

```json
{
  "status": "degraded",
  "checks": {
    "gorm[default]":  {"status": "up",   "latency": "1.2ms"},
    "gorm[readonly]": {"status": "down", "error": "dial tcp 10.0.0.2:3306: timeout"},
    "redis[default]": {"status": "up",   "latency": "0.3ms"}
  }
}
```

全 up → 200；任一 down → 503。各 check 并发执行、单独超时，一个卡住不拖垮整个探针。

> **[SEC-INFO]** `healthz` 的 `error` 字段会暴露内网地址与端口。默认仅在 `log.level=debug` 时输出详情；生产模式只返回 `{"status": "down"}`。

---

## 8. 目录结构

单 module。**公开 API 全部集中在根包**，插件与用户只需 import 一个包；实现细节关进 `internal/`，内部重构不破坏兼容。

```
xbc/
├── go.mod                    module github.com/xbcio/xbc
├── go.sum
├── application.example.yml
├── README.md
│
├── xbc.go                    New / Register / Run
├── plugin.go                 Plugin + 全部可选接口 + Base
├── deps.go                   Deps / Dep / Ref / Need / NeedNamed / Opt / RefOf
├── context.go                Context + Go() 托管
├── registry.go               Provide / Get / GetNamed / MustGet / MustGetNamed
├── router.go                 Router 链式元数据 + RouteInfo 查询
├── middleware.go             Middleware / Phase
├── response.go               Response / Paged / H()
├── errors.go                 Error + 预置错误
│
├── internal/
│   ├── assemble/             十阶段装配管线、失败回滚、逆序关闭
│   ├── graph/                通用拓扑排序器（插件序 / 中间件序共用）
│   ├── inject/               tag 扫描与注入
│   ├── conf/                 koanf 加载、profile、ENV 映射、default + validate
│   └── httpx/                recovery / requestid / accesslog 内建实现
│
├── plugins/
│   ├── gorm/                 基础设施（多实例）
│   ├── redis/                基础设施（多实例）
│   ├── cors/                 中间件
│   ├── ratelimit/            中间件
│   ├── jwt/                  认证（消费路由元数据）
│   └── cron/                 后台任务
│
├── examples/
│   ├── minimal/              20 行跑起来
│   ├── multi-datasource/     读写分离
│   └── full/                 完整骨架，cp -r 即用
│
└── xbctest/                  插件契约测试套件（对外可用）
```

---

## 9. 首批插件

每个插件对应压测一类机制：

| 插件 | 实现的接口 | 验证的机制 |
|---|---|---|
| `gorm` | Configurable, MultiInstancer, Initializer, HealthChecker, Closer | **多实例 + 连接生命周期 + 逆序关闭** |
| `redis` | 同上 | 多实例键 `(type, instance)` 不串 |
| `cors` | Configurable, MiddlewareProvider | 最薄插件形态：2 个方法 |
| `ratelimit` | Configurable, MiddlewareProvider, Declarer | `After: cors` 软约束 |
| `jwt` | Configurable, MiddlewareProvider | **消费路由元数据**：读 `.Public()` 决定放行 |
| `cron` | Configurable, Initializer, Runner, Closer, Declarer | **`ctx.Go` 托管 + `RefOf` 硬依赖** |

`jwt` 是最关键的一个——它验证「插件通过路由元数据协作」这条设计是否真的成立。

**第四类能力（业务模块）由 `examples/full` 中的 `user` 插件承载**，它实现 `RouteProvider` + `Migrator` + tag 注入，验证业务插件能否只靠 import 就在另一个项目里复用。它不进 `plugins/` 目录——那里只放通用能力。

> **[SEC-BRUTE]** `jwt` 插件本身不含登录端点，故无暴力破解面。但 examples 中的登录示例必须包含失败次数限制说明，避免使用者照抄出一个无防护的登录接口。

> **[SEC-SESSION]** `jwt` 插件配置必须强制 `secret` 非空且长度 ≥ 32 字节（`validate:"required,min=32"`），token 必须设置过期时间，默认 `expire: 2h`。禁止提供默认 secret。

---

## 10. 测试策略

**内核用假插件测，不碰任何真实中间件。** 拓扑排序、环检测、缺失依赖报错、逆序关闭、失败回滚全是纯逻辑，一组 `fakePlugin` 即可覆盖：

```go
func TestShutdownReverseOrder(t *testing.T) {
    var log []string
    a := fake("a").onStop(func() { log = append(log, "a") })
    b := fake("b").needs(a).onStop(func() { log = append(log, "b") })
    app := xbc.New().Register(a, b)
    app.start(); app.stop()
    assert.Equal(t, []string{"b", "a"}, log)   // 依赖者先停
}
```

内核必须覆盖的用例清单：

- 拓扑序正确、同层按注册顺序稳定
- 依赖成环 → 报错含完整环路径
- 硬依赖缺失（类型 / 插件 / 具名实例）→ 各自报错文案正确
- 软约束未命中 → 不中止，且记入日志
- 跨 Phase 反向约束 → 启动中止
- `Init` 失败 → 已初始化插件按逆序 `Stop`
- 关闭逆拓扑序
- 多实例：两个实例的配置独立校验、注册表键不串
- 配置：默认值填充、validate 报错路径正确、ENV 覆盖、profile 合并
- `enabled: false` 与无配置节两种关闭路径
- tag 注入：必需 / 可选 / 具名 / 与 `Dependencies()` 合并

**对外提供契约测试套件**，第三方插件作者引一行即可自检：

```go
func TestGormPluginConformance(t *testing.T) {
    xbctest.Conform(t, gorm.New())   // 检查 Name 非空、配置可绑定、
}                                     // 声明的依赖可解析、Stop 幂等等
```

**真实中间件走 `-tags=integration`**，默认 `go test ./...` 不需要任何 Docker。

---

## 11. 实施顺序

```
1. 内核骨架     Plugin 接口族 / Base / Context / Register / Name 自动推导
2. 配置         koanf 加载 + profile + ENV + default/validate 绑定 + 多实例展开
3. 依赖解析     拓扑排序器 + tag 扫描 + 注册表 + Deps 合并 + 全部报错文案
4. 装配管线     十阶段 + 优雅关闭 + 失败回滚 + doctor 子命令 + 启动日志
      ↑ 到此内核可用假插件跑通全部测试，未引入 gin 之外任何依赖
5. HTTP 骨架    Router 元数据 / 响应 / 错误 / recovery+requestid+accesslog / healthz
6. 插件         cors → jwt → gorm → redis → ratelimit → cron
      ↑ 顺序有讲究：cors 最薄先跑通形态，jwt 验证元数据协作，
        gorm 验证多实例，cron 验证 Runner 与硬依赖
7. examples + README
```

第 1~4 步**完全不引入 gin 之外的任何依赖**，内核正确性在接触真实中间件之前就被测试锁死。

---

## 12. 已知取舍

| 取舍 | 代价 | 缓解 |
|---|---|---|
| 单仓单 module | `go.sum` 大、CVE 扫描噪音、与使用方依赖版本冲突面大 | 二进制体积不受影响；目录按 `plugins/<name>/` 切干净，将来可拆多 module |
| 窄接口组合 | 方法名拼错 → 框架静默跳过 | 启动日志打印每个插件被识别到的能力；`xbctest.Conform` 契约测试 |
| 软依赖用字符串 | 拼错不报错 | `doctor` 与启动日志的「软约束未命中」段落 |
| tag 注入用反射 | 反射有性能与可读性成本 | 只在阶段 4~5 装配期执行一次，请求链路零反射 |
| 不做热重载 | 改配置须重启 | 插件接口保持简单；如确有需要，待接口稳定后再评估 |
| 内建三中间件不可拔 | 违背「一切皆插件」的纯粹性 | 它们是请求契约本身，可拔会让 `traceId` 与日志契约同时失效 |
