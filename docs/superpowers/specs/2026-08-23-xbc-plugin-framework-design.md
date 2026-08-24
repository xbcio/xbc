# xbc — 插件化 Go Web 框架设计

- **日期**：2026-08-23（2026-08-24 设计审阅修订 + 日志体系补全）
- **仓库**：`git@github.com:xbcio/xbc.git`
- **Module**：`github.com/xbcio/xbc`
- **状态**：设计定稿，待实现

> **修订说明（2026-08-24）**
>
> 一次批判性自审修掉了两个会让实现卡住的设计漏洞——产物的静态可知性（§5.7）与路由元数据的可见时机（§7.1）——并调整了三处默认值：显式注册默认启用（§6.4）、后台任务死亡策略（§5.2）、迁移默认关闭（§4.2）。
>
> 随后补全了两块此前缺席的设计：**插件复用**（§5.6 注册表接口匹配，让 B 依赖 A 时不必 import A 的第三方依赖）与**日志体系**（§8 SLF4J 式门面 + zap binding + OTel 链路，§9 零依赖子包）。日志的变参定为 **KV 结构化**，由 console/json 两种 encoder 分别渲染（§8.7），并提供 `TInfo(ctx, ...)` 语法糖压掉两段式调用。

---

## 1. 目标

基于 Gin 构建一个开箱即用的 Web 框架，**以插件体系为核心**——框架内核只负责装配与请求契约，其余一切能力（数据库、缓存、认证、限流、定时任务、业务模块）均以插件形式接入。

衡量设计是否成功的标准：

1. 新增一个能力，**不需要修改框架任何一行代码**——写一个插件，import 进来即可
2. 关闭一个能力，**不需要重新编译**——注释掉配置节或写一行 `enabled: false`
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
| 3 | 依赖获取 | **双层并存**：显式类型注册表打底 + struct tag 糖衣 | tag 省样板并让框架自动推导依赖图；`MustGet` 兜住动态场景 |
| 4 | 产物声明 | **`provide` tag 对称于 `inject`**，一次 tag 扫描得到依赖图两端 | 阶段 4 建图必须静态知道「谁产出什么」；运行时 `Provide` 来不及 |
| 5 | 配置绑定 | **约定式自动绑定 + 支持多实例** | 插件只声明 Config 结构体，不写读配置代码；多数据源/读写分离是真实需求 |
| 6 | 仓库形态 | **单仓单 module** | 开发发版最简。二进制体积不受影响（Go 链接器只打包实际 import 的包），代价是 `go.sum` 大、CVE 扫描噪音、依赖版本冲突面大。目录按 `plugins/<name>/` 切干净，将来可拆 |
| 7 | 工具链 | **不做 CLI**，纯框架库 + examples | 先把插件接口设计对 |
| 8 | 插件接口形态 | **窄接口组合为底，额外提供可选 `Base`** | 插件只实现关心的接口；新增扩展点对既有插件零影响 |
| 9 | 排序机制 | **声明式**：`Phase` 粗锚点 + `After`/`Before` 细调，无魔数 | `OrderRateLimit + 50` 是魔数，第三方之间会撞，且表达不出真实意图 |
| 10 | 依赖表达 | 硬依赖（`Types` / `Plugins`）与软顺序（`After` / `Before`）**分离** | 二者语义不同：硬依赖缺失应中止启动，软顺序缺失应静默忽略 |
| 11 | 插件名引用 | 硬依赖用**类型引用**（编译期安全），软依赖用字符串 | 判据：能用类型引用的是硬依赖；必须用字符串的是软依赖 |
| 12 | 内核边界 | `recovery` / `trace` / `accesslog` **内建，非插件** | 它们是框架契约本身，关掉会让统一响应的 `traceId` 和日志契约同时失效 |
| 13 | 配置命名空间 | 插件配置**保留 `plugins.` 前缀** | 与「配置节不存在则不启用」构成闭环，使「有配置但无插件」可被诊断 |
| 14 | 启用规则 | **显式 `Register` 默认启用**；blank import 需配置节 | 显式注册本身就是意图声明，业务插件不该被迫写空配置节 |
| 15 | 路由元数据可见时机 | **请求时查表**，不在中间件装载时查 | gin 要求 `Use` 早于路由注册，装载时路由表必然为空 |
| 16 | HTTP 错误表达 | **语义化状态码** + body 携带业务码 | 网关熔断、Prometheus 告警、CDN 策略可直接用状态码，无需解包 |
| 17 | 自动迁移 | **默认关闭**，`--migrate` / `migrate` 子命令显式触发 | 应用启动时自动改表结构是危险默认值 |
| 18 | 日志底座 | **SLF4J 式门面**：`log.Logger` 接口 + zap 默认 binding | 调用点零 zap 依赖、后端可换、测试可捕获；zap 生态撑得起日期滚动/多 sink/分级落盘，slog 的 handler 得自己补一遍 |
| 19 | 链路标识 | `request_id` 与 `trace_id` 是**同一个 128 位值的两种编码** | ULID 与 W3C trace_id 都是 128 bit，人读与机读兼得，无需二选一 |
| 20 | span 划分 | **业务显式开 span 并命名**，不是每请求一个随机值 | span_name 让日志能看出调用层次；未显式开时用路由模板兜底 |
| 21 | 零依赖子包 | `log` / `errs` / `resp` 独立成包，根包用**类型别名**重导出 | 让 domain 层能用错误码而不拖进 gin；使用方一个 import 的体验不变 |
| 22 | 注册表匹配 | 目标类型为**接口**时按可赋值性扫描 | 让「接口定义在消费方」这条 Go 惯例在插件系统里成立，B 不必 import A 的第三方依赖 |
| 23 | 日志变参语义 | **KV 结构化**，`TInfof` 提供 printf 版 | 拼进 msg 的字段检索不到；KV 是数据，console/json 只是两种渲染 |
| 24 | 日志格式归属 | **每个 sink 自带 format**，文件按后缀推导（`.log`/`.jsonl`） | 「终端 console + 文件 json」是最常见组合，全局单一 format 表达不了 |

---

## 3. 架构总览

```
┌──────────────────────────────────────────────────────────┐
│  xbc 内核                                                 │
│                                                          │
│  装配引擎          HTTP 骨架         请求契约              │
│  ├ 配置加载/绑定    ├ gin 封装        ├ recovery           │
│  ├ 拓扑排序         ├ Router 元数据   ├ trace（链路）      │
│  ├ 依赖解析/注入    ├ 优雅关闭        ├ accesslog          │
│  └ 生命周期管理     └ healthz         └ 统一响应/错误       │
│                                                          │
│  内核依赖：gin + koanf + validator                        │
└──────────────────────────────────────────────────────────┘
         │ 依赖                              ▲
         ▼                                   │ 实现可选接口接入
┌───────────────────────────┐   ┌──────────────────────────┐
│  零依赖子包（可独立使用）    │   │  插件层                   │
│  log/   门面 + zap 绑定     │   │  gorm  redis  cors        │
│  errs/  Error + 错误码     │   │  ratelimit  jwt  cron     │
│  resp/  Response / Paged  │   │  ...业务模块              │
└───────────────────────────┘   └──────────────────────────┘
         ▲
         │ 其他仓库可直接 import，不拖进 gin
```

**内核不认识任何具体插件**；插件也不互相 import（硬依赖除外，那是刻意为之的显性化）。二者通过三个协作面交互：

1. **类型注册表** —— 插件提供/消费类型实例（`*gorm.DB`），目标为接口时按可赋值性匹配
2. **路由元数据** —— 插件标注/查询路由属性（`.Public()` / `.Perm()`）
3. **依赖声明** —— 插件声明关系，框架推导顺序

左下角的三个子包是**框架的下游而非上游**：它们零框架依赖，既服务于内核，也能被任何其他仓库单独 import。详见 §9。

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
                            enabled:false / 未启用的直接剔除（启用规则见 6.4）
                            —— 本步把「插件类型」变成「插件实例」
  3. BindConfig ·········· unmarshal → 填 default tag → 跑 validate
  4. Resolve ············· 扫 inject/provide tag + 读 Dependencies()/Provides()
                            合并成依赖图，拓扑排序
                            检测环 / 缺失硬依赖 → 启动中止
  5. Init ················ 按拓扑序，逐插件：Inject → Init → 收割产物
                            收割后校验「声明的产物」与「实际登记的产物」一致
  6. Migrate ············· 仅在显式请求时执行（--migrate / migrate 子命令）
                            全部 Init 完成后，按拓扑序调用 Migrator
  7. AssembleHTTP ········ 收集全部 Middleware 排序装载 → 调 RegisterRoutes
                            → 冻结路由表 → 调 PostRoutes
  8. Start ··············· 并发拉起所有 Runner
  9. Serve ··············· HTTP server 阻塞运行
                                  ↓ SIGINT / SIGTERM / GoCritical 触发
 10. Shutdown ··········· 停收新请求 → 等 in-flight 排空 → 停 Runner
                          → 逆拓扑序调 Stop() → 超时兜底强杀
```

### 4.1 关键约束

**(a) 逆拓扑序关闭。** `user` 插件依赖 `gorm`，关闭时必须先停 `user` 再停 `gorm`，否则 `user` 手里正在跑的事务会被拔掉连接。关闭顺序 = 启动顺序取逆，由框架保证。

**(b) 启动失败必须回滚。** 阶段 5~8 任一步出错，对**已经 Init 成功的插件**逆序调用 `Stop()` 再退出。否则 MySQL 连上了、Redis 连失败，进程退出时那些连接泄漏在服务端。

**(c) Expand 早于 BindConfig。** 先展开实例再绑配置，「两个数据源」才是两个独立的校验单元——其中一个 DSN 写错，报错能精确到 `plugins.gorm.readonly.dsn`。

**(d) 阶段 4 必须静态知道产物归属。** 拓扑排序要连边，就得回答「谁产出 `*gorm.DB`」。产物靠运行时 `Provide` 登记的话，阶段 4 时它还没发生——所以产物**必须声明**（`provide` tag 或 `Provides()`），见 5.7。

**(e) 阶段 5 的注入与收割时机。** 拓扑序保证依赖已 Init 完毕，所以对插件 X 执行 `Inject` 时，X 的所有依赖必然已经产出成品。`Init` 返回后框架立刻**收割**——读取 `provide` 字段登记进注册表，并比对声明与实际：声明了却是零值 → 报错中止，避免下游拿到 nil。

**(f) Migrate 独立成阶段 6。** 迁移可能需要多个插件同时就绪，所以放在全部 `Init` 完成之后统一执行，而非混在阶段 5 里。**默认不执行**——理由见 4.2。

**(g) 阶段 7 结束才有完整路由表。** 中间件必须先于路由注册装载（gin 的硬性要求），因此中间件**装载时看不到任何路由**。需要读路由元数据的中间件在**请求时**查表；需要全量路由表的插件（swagger、casbin 权限点同步）实现 `PostRouter`，在路由表冻结后被调用。

### 4.2 子命令映射

应用二进制自带子命令，用标准库 `flag` 实现，**不引入 cobra**：

| 命令 | 执行阶段 | 用途 |
|---|---|---|
| `./myapp` | 1~5, 7~9 | 正常启动，**不跑迁移** |
| `./myapp --config ./prod.yml --profile prod` | 1~5, 7~9 | 指定配置 |
| `./myapp --migrate` | 1~9 | 启动前先跑迁移 |
| `./myapp migrate` | 1~6 后退出 | 只跑迁移，不起 HTTP |
| `./myapp doctor` | 1~4 后退出 | 打印装配计划，不建任何连接 |

**为什么迁移默认关闭。** 应用一启动就自动 `AutoMigrate` 改表结构，是个危险默认值：滚动发布时多副本会并发改表，回滚镜像时旧版本会把表结构改回去，而生产库的 DDL 本该是一次受控操作。翻过来之后，迁移要么走独立的 `migrate` 子命令（CI/CD 里一个单独的 job），要么显式加 `--migrate`。

开发期嫌麻烦，在 profile 里开：

```yaml
# application-dev.yml
server:
  auto_migrate: true
```

危险的事情要显式，而 profile 正是承载「这个环境可以宽松一点」的地方。`doctor` 是白送的——阶段 1~4 本就不产生副作用，跑完打印依赖图和中间件链即可，用于 CI 中校验配置。

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

两条注册路径的**启用规则不同**：`app.Register()` 是一次意图声明，默认启用；blank import 只表示「这个能力可用」，要不要装配由配置说了算。详见 6.4。

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
                                                  ↑ 显式 Register

xbc: 中间件链（7）
  1. xbc.recovery      [recover]
  2. xbc.trace         [observe]
  3. xbc.accesslog     [observe]
  4. cors              [security]
  5. ratelimit         [security]  after=cors
  6. jwt.auth          [auth]
  7. myapp.audit       [business]  after=jwt.auth

xbc: 软约束未命中（不影响启动）
  audit.After = "tracing" —— 无此插件，忽略
    → 拼写错误？还是忘了启用 plugins.tracing？

xbc: 迁移未执行（12 个模型待检查）
  → 需要迁移请使用 ./myapp migrate 或 --migrate
```

最后一段是刻意加的：迁移改成默认关闭后，「我改了 model 但表没变」会成为新的常见困惑，启动日志必须主动说明。

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
type Provider           interface { Provides() []Dep }
type Initializer        interface { Init(ctx *Context) error }
type Migrator           interface { Migrate(ctx *Context) error }
type MiddlewareProvider interface { Middlewares() []Middleware }
type RouteProvider      interface { RegisterRoutes(r *Router) }
type PostRouter         interface { PostRoutes(ctx *Context) error }
type Runner             interface { Start(ctx *Context) error }
type Closer             interface { Stop(ctx context.Context) error }
type HealthChecker      interface { Health(ctx context.Context) error }
```

| 接口 | 调用阶段 | 典型实现者 |
|---|---|---|
| `Configurable` | 3 BindConfig | 几乎所有插件 |
| `MultiInstancer` | 2 Expand | gorm、redis |
| `Declarer` | 4 Resolve（与 tag 合并） | 有硬依赖或顺序要求的插件 |
| `Provider` | 4 Resolve（与 tag 合并） | 不用 `provide` tag 的产出型插件 |
| `Initializer` | 5 Init | 基础设施类 |
| `Migrator` | 6 Migrate | 业务模块 |
| `MiddlewareProvider` | 7 AssembleHTTP | 中间件类 |
| `RouteProvider` | 7 AssembleHTTP | 业务模块 |
| `PostRouter` | 7 路由表冻结后 | swagger、casbin 权限点同步 |
| `Runner` | 8 Start | cron、consumer |
| `Closer` | 10 Shutdown（逆拓扑序） | 持有资源的插件 |
| `HealthChecker` | `/healthz` 被访问时 | 基础设施类 |

**窄接口方案下不需要空实现**，所以 `Base` 的职责与「胖接口 + Base」模式完全不同——见 5.4。

### 5.2 `Runner` 语义：非阻塞 + 托管 goroutine

`Start` **必须快速返回**。长期循环通过 `ctx.Go` / `ctx.GoCritical` 交给框架托管，框架负责 panic 恢复、shutdown 时 cancel、退出前等待：

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

#### 后台任务死掉之后

「panic 恢复、记日志、继续」对所有后台任务一刀切是错的。一个 Kafka consumer 挂掉之后，进程还在、`/healthz` 还返回 200、k8s 觉得一切正常——**消息静静地堆积**。这比进程崩溃糟得多：崩溃会被立刻发现，僵尸不会。

所以拆成两个语义：

```go
func (c *Context) Go(fn func(context.Context))          // panic 恢复 → 记日志 → 应用继续
func (c *Context) GoCritical(fn func(context.Context))  // panic 或提前返回 → 触发整个应用 shutdown
```

| | `Go` | `GoCritical` |
|---|---|---|
| panic | 恢复，记 error 日志，goroutine 结束 | 恢复，记 error 日志，**触发 shutdown** |
| 正常返回（非 shutdown 触发） | 视为任务完成，无事发生 | 视为**意外退出**，触发 shutdown |
| shutdown 期间返回 | 正常，计入等待组 | 正常，计入等待组 |
| 适用 | cron 单次任务、缓存刷新、指标上报 | consumer、长连接网关、数据同步 |

`GoCritical` 触发的关闭走**完整的阶段 10**，不是 `os.Exit`——in-flight 请求照样排空，其他插件照样逆序 `Stop`。退出码 `1`，让编排系统知道这是异常退出。

判据：**这个 goroutine 死了，应用还算不算健康？** 不算，就用 `GoCritical`。

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
func Offer[T any]() Dep                       // 产出（用于 Provides，见 5.7）

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

func (c *Context) Log() log.Logger               // 门面接口，自动带 plugin=gorm instance=readonly
func (c *Context) Config() *Config
func (c *Context) Instance() string             // "default" / "readonly"
func (c *Context) Go(fn func(context.Context))          // 托管 goroutine
func (c *Context) GoCritical(fn func(context.Context))  // 死亡即触发 shutdown
func (c *Context) Route(gc *gin.Context) *RouteInfo     // 请求时查当前路由元数据
func (c *Context) Routes() []RouteInfo                  // 全量路由表（阶段 7 之后才非空）

type Base struct{ ctx *Context }
func (b *Base) Ctx() *Context    { return b.ctx }
func (b *Base) Log() log.Logger  { return b.ctx.Log() }
func (b *Base) Name() string     // 从包路径自动推导
```

`Log()` 返回**接口**而非具体类型（§8.3）——插件因此不依赖 zap，宿主换后端时插件不用重编写。

`Base` 的职责是**便利访问器 + `Name()` 自动推导 + tag 锚点**，不是空实现载体。不嵌 `Base` 照样是合法插件——`Init(ctx)` 参数里什么都有。

### 5.6 注册表：键 = (类型, 实例名)

```go
func Provide[T any](ctx *Context, v T)                     // 用插件自己的实例名登记
func Get[T any](ctx *Context) (T, bool)                    // 取 default
func GetNamed[T any](ctx *Context, name string) (T, bool)
func MustGet[T any](ctx *Context) T
func MustGetNamed[T any](ctx *Context, name string) T
```

`gorm[default]` 与 `gorm[readonly]` 是同一份插件代码的两个实例，登记时框架自动用**自己的实例名**做键——插件代码完全不需要感知自己是第几个实例。

#### 目标为接口时按可赋值性匹配

插件复用（B 依赖 A，如「限流插件要用 redis」）有个绕不开的问题：若 B 写 `RDB *redis.Client`，B 就**硬 import 了 go-redis**——A 的第三方依赖传染给了 B 的每一个使用者。

Go 的解法是「接口定义在消费方」，但这要求注册表能按接口取值。所以：**取值目标为接口类型时，注册表扫描已登记的具体类型，返回可赋值的那个。**

```go
// plugins/ratelimit —— 不 import go-redis
type Counter interface {
    Incr(ctx context.Context, key string) (int64, error)
    Expire(ctx context.Context, key string, d time.Duration) error
}

type Plugin struct {
    xbc.Base
    C Counter `xbc:"inject"`     // 注册表扫描：谁能赋给 Counter？
}
```

匹配规则与报错：

| 命中数 | 行为 |
|---|---|
| 1 | 注入 |
| 0 | 硬依赖报错，列出注册表里**方法最接近**的类型与差哪几个方法 |
| >1 | **报错**，列出全部候选，要求用 `xbc:"inject,name=xxx"` 消歧 |

歧义不猜、直接报错——两个 redis 实例都满足 `Counter` 时，框架无从知道限流该用哪个，猜错比报错更难查。

> **现实约束，必须写进插件开发文档。** `*redis.Client` 的 `Incr` 返回 `*redis.IntCmd` 而非 `(int64, error)`，**不会天然满足**上面这个 `Counter`。想让接口匹配成立，redis 插件得额外 provide 一个适配过的类型。
>
> 这不是设计缺陷，而是 Go 生态的普遍事实：**第三方 SDK 的方法签名不是为「消费方定义接口」准备的**。因此首批插件里凡是要被其他插件复用的（redis 尤其），都要显式设计一组窄接口并 provide 其实现——这是插件作者的责任，框架只保证匹配机制成立。

#### 通用插件依赖多实例基础设施

限流要用哪个 redis 实例？答案不写死在代码里，走配置：

```yaml
plugins:
  ratelimit:
    counter_instance: cache      # 用 redis[cache] 而非 redis[default]
```

插件据此在 `Dependencies()` 里返回具名依赖——**实例名来自配置，依赖声明在阶段 4 仍是静态可知的**（配置在阶段 3 已绑定）：

```go
func (p *Plugin) Dependencies() xbc.Deps {
    return xbc.Deps{Types: []xbc.Dep{
        xbc.NeedNamed(xbc.RefOf[Counter](), p.Cfg.CounterInstance),
    }}
}
```

tag 只能写字面量，所以**依赖实例名可配置的场景必须走 `Dependencies()`**，这是两种声明方式并存的正当理由，不是冗余。

### 5.7 依赖与产物：一次 tag 扫描，得到依赖图两端

依赖图有两端：谁**要**什么，谁**给**什么。两端都必须在阶段 4 静态可知，否则连不出边。

要什么好办——`inject` tag 或 `Deps.Types` 都是声明式的。**给什么才是难点**：如果产物只在 `Init` 里运行时 `Provide`，阶段 4 排序时根本不知道 `*gorm.DB` 出自谁手，拓扑排序无从谈起。

所以产物也要声明，且用与 `inject` 完全对称的形式：

```go
type GormPlugin struct {
    xbc.Base
    Cfg Config
    DB  *gorm.DB `xbc:"provide"`     // 阶段 4：静态可知本插件产出 *gorm.DB
}

func (p *GormPlugin) Init(ctx *xbc.Context) error {
    db, err := gorm.Open(mysql.Open(p.Cfg.DSN))
    if err != nil {
        return err
    }
    p.DB = db                         // 阶段 5：Init 返回后框架自动收割并登记
    return nil
}
```

一次 tag 扫描同时得到依赖图的两端，插件作者一个额外方法都不用写。多实例天然成立——收割时框架用插件自己的实例名做键。

**收割后校验。** `Init` 成功返回但 `provide` 字段仍是零值 → 立刻报错中止：

```
xbc: 插件 gorm[readonly] 声明产出 *gorm.DB，但 Init 后该字段仍为 nil
  → 检查 Init 中是否忘记给 DB 字段赋值
```

不校验的话，这个 nil 会一路漏到下游插件的第一次查询，那时栈已经离现场很远了。

**不用 tag 的走显式层**，与 `inject` 的双层结构完全对称：

```go
func (p *GormPlugin) Provides() []xbc.Dep { return []xbc.Dep{xbc.Offer[*gorm.DB]()} }
func (p *GormPlugin) Init(ctx *xbc.Context) error {
    // ...
    xbc.Provide(ctx, db)              // 手动登记
    return nil
}
```

`Offer[T]()` 与 `Need[T]()` 是同一个 `Dep` 类型的两个构造器，只是站在边的两端。声明与实际登记不一致同样会在阶段 5 末尾被抓出来。

#### 四种写法的完整对照

```go
// 写法一：tag —— 覆盖 95% 的场景，框架据此自动建图，无需写任何声明方法
type UserPlugin struct {
    xbc.Base
    DB    *gorm.DB      `xbc:"inject"`
    RoDB  *gorm.DB      `xbc:"inject,name=readonly"`
    Cache *redis.Client `xbc:"inject,optional"`   // 缺失留零值，不报错
    Svc   *UserService  `xbc:"provide"`           // 本插件对外产出
}

// 写法二：显式 —— 动态场景，如实例名来自配置，tag 是静态的写不出来
func (p *ShardPlugin) Dependencies() xbc.Deps {
    return xbc.Deps{Types: []xbc.Dep{xbc.NeedNamed[*gorm.DB](p.Cfg.Shard)}}
}
func (p *ShardPlugin) Init(ctx *xbc.Context) error {
    p.db = xbc.MustGetNamed[*gorm.DB](ctx, p.Cfg.Shard)
    return nil
}
```

tag 语法：

| tag | 含义 |
|---|---|
| `xbc:"inject"` | 注入 default 实例，缺失则启动中止 |
| `xbc:"inject,name=readonly"` | 注入具名实例 |
| `xbc:"inject,optional"` | 缺失留零值，不报错 |
| `xbc:"provide"` | 声明产出，`Init` 后由框架收割登记 |

均只作用于**导出字段**。两种写法在阶段 4 合并成同一张依赖图，可自由混用：

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
    PhaseObserve                // 可观测                 ← 内建 trace、accesslog
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

**逃生舱口。** `Phase` 底层是 `int`，五个常量之间刻意留出间隔。真要插到 `PhaseRecover` 之前（比如连接级埋点，需要早于 panic 兜底），可以写 `xbc.PhaseRecover - 1`。这是逃生舱口不是推荐用法——落到 recovery 外面意味着这个中间件里的 panic 无人接管，会直接打挂进程。文档里写明存在，但不给它命名常量，避免它看起来像一个正常选项。

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
  auto_migrate: false        # 默认关闭，见 4.2

log:                         # 框架保留，完整形态见 8.6
  level: info
  console: {enabled: true}   # 终端人读，带色对齐
  file:                      # 文件后缀决定格式：.log → console，.jsonl → json
    enabled: true
    path: logs/app.log.jsonl

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

启用与否取决于**注册方式**——两条注册路径表达的意图强度不同：

| 注册方式 | 配置节 | 结果 |
|---|---|---|
| `app.Register(p)` 显式注册 | 不存在 | **启用**，全走默认值 |
| `app.Register(p)` 显式注册 | 存在 | 启用 |
| `app.Register(p)` 显式注册 | `enabled: false` | 关闭 |
| blank import + `init()` | 不存在 | **不启用** |
| blank import + `init()` | 存在（哪怕空 `{}`） | 启用 |
| blank import + `init()` | `enabled: false` | 关闭 |

**为什么要分。** `app.Register(user.New())` 已经是一次明确的意图声明——还要求在 yml 里补一个空的 `plugins.user: {}` 才肯装配，纯属仪式。而业务插件恰恰是最常没有配置的那一类。

blank import 则不同：`_ "xbc/plugins/redis"` 只表示「这个能力可用」，装不装配由配置说了算。这条规则的价值在于**临时停掉一个基础设施插件只需注释配置，不用改代码重新编译**。

框架能区分这两条路径——`init()` 走包级默认列表，`app.Register()` 走实例方法，两个入口天然带着来源标记。启动日志会标出显式注册的插件（见 4.4），让「为什么这个插件启用了」永远有据可查。

`enabled` 字段由框架隐式注入，插件 `Config` 结构体不需要声明它。

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
    Path       string      // 路由模板，如 /api/v1/users/:id
    Name       string
    Public     bool
    Perm       string
    Idempotent bool
}
```

#### 元数据什么时候能看见

这里有个绕不开的时序：**gin 要求 `engine.Use()` 必须早于路由注册**，否则中间件不生效。而路由元数据是 `RegisterRoutes` 执行时才产生的。两者相加的结论是——

> **中间件装载的那一刻，路由表必然是空的。**

`jwt` 若在 `Middlewares()` 里读白名单，读到的永远是空表。所以元数据**在请求时查，不在装载时查**：

```go
func (p *JWTPlugin) handle(c *gin.Context) {
    if info := p.Ctx().Route(c); info != nil && info.Public {
        c.Next()
        return
    }
    // ...验证 token
}
```

`c.FullPath()` 返回的正是注册时的路由模板，与 `RouteInfo.Path` 天然对齐。框架在阶段 7 末尾把路由表编成 `map[method+path]*RouteInfo` 并**冻结**，请求时 O(1) 命中，无锁无反射。

时序问题就此消失：中间件装载时不碰路由表，第一个请求到达时表早已完整。

**需要全量路由表的插件走 `PostRouter`。** swagger 要遍历所有路由生成文档，casbin 要把 `Perm()` 同步成权限点——它们需要的是启动期的一次性全量扫描，不是每请求查询：

```go
func (p *SwaggerPlugin) PostRoutes(ctx *xbc.Context) error {
    return p.gen(ctx.Routes())   // 此时路由表已冻结且完整
}
```

`Routes()` 在阶段 7 之前调用返回空切片而非报错——它是个查询方法，不是断言。真正的保护在于 `PostRoutes` 的调用时机由框架控制，插件作者不需要自己判断「现在能不能读」。

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

`H` 在写响应前必须检查 `c.Writer.Written()`：中间件可能已经 `Abort` 并写过响应体，此时再写一次会拼出一个畸形的 JSON（两个 body 首尾相接），而且 HTTP 状态码已经发出去了改不了。已写过就只记日志，不再写。

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

## 8. 日志与链路追踪

`github.com/xbcio/xbc/log` —— **零框架依赖**，只依赖 `zap` + `lumberjack` + OTel 的 **API 包**。硬约束：**这个包一行都不能 import xbc 根包**，否则「其他仓库直接用」就变成拖进整个框架。

结构是 SLF4J 式的两层：**`Logger` 接口是契约，zap 是默认 binding**（§8.3）。调用点只碰接口，后端可整体替换。

选 zap 而非 slog 做默认 binding：日期滚动、多 sink、分级落盘、采样这些能力 zap 生态已经成熟，slog 的 handler 得自己补一遍；且 logger 要被其他仓库直接 import，标准库那点抽象反而不够。

**ID 类型与传播协议直接用 OpenTelemetry**，不自己造：

| 用途 | 用什么 | 为什么不自己写 |
|---|---|---|
| ID 类型 | `trace.TraceID` `trace.SpanID` | 编码、校验、全零检查都已处理好 |
| header 解析/注入 | `propagation.TraceContext` | `traceparent`/`tracestate` 的规范实现 |
| 上下文载体 | `trace.SpanContext` | 接 OTel SDK 时天然互通，无需转换 |

依赖的是 `go.opentelemetry.io/otel` + `go.opentelemetry.io/otel/trace` 两个 **API-only** module，不含 SDK、exporter、collector——很轻。真要导出 span 到 Jaeger/Tempo 时才引 SDK，见 8.7。

### 8.1 链路标识模型

| 字段 | 生成 | 跨服务传播 | 用途 |
|---|---|---|---|
| `request_id` | 每次 HTTP 请求，ULID | ❌ | 人读的请求号，带时间前缀 |
| `trace_id` | 无上游时由 `request_id` 构造；有上游则继承 | ✅ `traceparent` | 串整条链；**回给客户端做报障号** |
| `span_id` | 每个 span 生成；可自定义 | ✅ 作为下游的 parent | 定位链上具体节点 |
| `span_name` | 业务显式命名；未开 span 时取路由模板 | ❌ | 看出当前在哪个操作里 |
| `parent_span_id` | 上游传入或父 span | — | 只在 span 结束日志里输出 |

**request_id 与 trace_id 是同一个 128 位值的两种编码。** ULID 是 128 bit，`trace.TraceID` 是 `[16]byte`——同样 128 bit，直接构造，不必二选一：

```go
var tid trace.TraceID = trace.TraceID(ulid.Bytes())   // 零转换成本
```

```
request_id  01J8XQZ7K3M4N5P6Q7R8S9T0V1          Crockford Base32，26 字符，人读、可排序
trace_id    01926f7e8c83a4d5b6c7d8e9fa0b1c2d    trace.TraceID.String()，32 hex
```

ULID 前 48 bit 是毫秒时间戳，天然非全零，满足 OTel 对 `TraceID.IsValid()` 的要求。

有上游 `traceparent` 时二者**不同**——`trace_id` 继承上游，`request_id` 仍是本服务本次请求的号。这正是它们该不同的场合。

### 8.2 链路怎么串

传播走 OTel 的 `propagation.TraceContext`，即 **W3C Trace Context** 标准：

```
traceparent: 00-{trace_id:32hex}-{parent_span_id:16hex}-{flags:2hex}
```

```
       网关                     订单服务                    库存服务
         │                         │                          │
生成 trace_id ────traceparent────► 继承 trace_id ──────────► 继承 trace_id
  span: a1…                parent=a1…, span: b2…      parent=b2…, span: c3…
  request_id: R1                request_id: R2             request_id: R3
         │                         │                          │
    ─────┴─────────────────────────┴──────────────────────────┴─────
     trace_id 全程一致 → 一个 ID 捞出整条链的全部日志
```

解析与注入都交给 OTel 的 propagator，我们不碰 header 字符串：

```go
prop := propagation.TraceContext{}
sc := prop.Extract(ctx, propagation.HeaderCarrier(req.Header))   // 入站
prop.Inject(ctx, propagation.HeaderCarrier(outReq.Header))       // 出站
```

### 8.3 门面接口：调用方不 import zap

参考 SLF4J 的**门面 + 可插拔后端**：`Logger` 是接口，zap 是默认绑定。

```go
package log   // 零框架依赖

// ── 门面：业务代码与插件依赖这个，不 import zap ──────────
type Logger interface {
    Debug(msg string, kv ...any)
    Info(msg string, kv ...any)
    Warn(msg string, kv ...any)
    Error(msg string, kv ...any)

    With(kv ...any) Logger              // 派生子 logger
    Enabled(lv Level) bool              // 避免昂贵的字段构造
}

// 可选接口：后端若是 zap，就能拿到强类型入口（同插件契约的窄接口组合）
type ZapProvider interface{ Zap() *zap.Logger }

func Zap(ctx context.Context) (*zap.Logger, bool)   // 逃生舱口，后端非 zap 时 ok=false

// ── 后端绑定 ────────────────────────────────────────────
func Init(cfg Config) error             // 装配默认的 zap 实现
func SetLogger(l Logger)                // 换成自己的实现（SLF4J 的 binding）
func L() Logger                         // 全局
func Sync() error                       // 退出前刷盘

// ── 链路：字段绑进 context，业务代码只传 ctx ─────────────
func Ctx(ctx context.Context) Logger    // 取不到则返回 L()，不 panic
func NewContext(ctx context.Context, l Logger) context.Context

// ── span ───────────────────────────────────────────────
func Span(ctx context.Context, name string) (context.Context, func())
func SpanWith(ctx context.Context, name string, opts ...SpanOption) (context.Context, func())
func SpanID(id trace.SpanID) SpanOption // 自定义 span_id

// ── 链路上下文：内嵌 OTel 的 SpanContext，天然互操作 ──────
type Trace struct {
    trace.SpanContext            // TraceID / SpanID / TraceFlags / Remote
    ParentSpanID trace.SpanID
    SpanName     string
    RequestID    string          // ULID，本框架扩展字段
}

func TraceFrom(ctx context.Context) Trace
func WithTrace(ctx context.Context, t Trace) context.Context
func (t Trace) Fork(name string) Trace   // 新 span，继承 trace_id
```

**为什么门面用 `...any` 的 KV 而不是 `...zap.Field`：** 若签名里出现 `zap.Field`，每个调用点都得 `import "go.uber.org/zap"`，门面就白做了——业务代码依然绑死在 zap 上。KV 风格让调用点干干净净：

```go
log.Ctx(ctx).Info("订单创建", "order_id", id, "amount", amt)
```

**性能税用 `log.Zap()` 找补。** zap 的 Sugar 层为便利付出了一点分配开销，热路径（每请求几十条日志的场景）可以直接下探到强类型：

```go
if zl, ok := log.Zap(ctx); ok {
    zl.Info("hot path", zap.String("k", v))   // 零分配
}
```

这条 API 承认了一个事实：**门面的价值在调用点数量，不在性能**。几千个普通调用点受益于零 zap 依赖，少数几个热点用逃生舱口——比反过来强。

`Zap()` 走可选接口而非门面方法，是因为**接口不该逼着每个实现都产出 zap logger**——换成 zerolog 后端时，`Zap() *zap.Logger` 这个方法根本无从实现。同插件契约里 `Configurable`/`Runner` 的处理：能力是可选的，用类型断言问。

`Enabled()` 是给昂贵字段用的，避免无谓构造：

```go
if l := log.Ctx(ctx); l.Enabled(log.DebugLevel) {
    l.Debug("请求详情", "dump", expensiveDump())
}
```

**接口里刻意没有 `Fatal`。** 库代码不该有权决定进程退出——`Fatal` 只保留在具体实现上，由 `main` 显式调用。

#### 门面换来的三件事

| 收益 | 场景 |
|---|---|
| 调用方零 zap 依赖 | 业务代码、domain 层、插件都只 import `xbc/log` |
| 后端可替换 | 其他仓库已有日志体系 → `log.SetLogger(自己的实现)` |
| 测试可捕获 | `log.SetLogger(testLogger)` 后直接断言日志内容 |

插件作者写 `Logger` 接口而非 `*zap.Logger`，插件就能跑在任何后端上——这跟 §5.6 的「接口定义在消费方」是同一条原则的应用。

#### 门面与 binding 不分包

SLF4J 在 Java 里必须分成 `slf4j-api` + `slf4j-logback` 两个 jar，是因为它要调和 log4j / commons-logging / JUL 的历史三国杀。**xbc 没有这个包袱**，所以门面接口与 zap binding**同放在 `log/` 一个包里**：

- zap 自身传递依赖极少（`zapcore` + `multierr`），import 进来不污染
- 分包会让 99% 只想用默认后端的人多 import 一个包、多写一行装配，为 1% 的换后端场景买单
- 换后端的人本来就要写实现代码，多一次 `SetLogger` 调用不算负担

真正要紧的约束——**调用点不出现 zap**——已经由 KV 签名达成了，不需要靠分包来保证。

`Ctx(ctx)` 取的是**已绑好字段的 logger**（存在 context 里），不是每次现构造——O(1)，无反射。

#### T 系列：`.Ctx(ctx)` 的语法糖

`log.Ctx(ctx).Info(...)` 每天要写几百遍，两段式调用是纯粹的噪音。包级 T 函数把它压成一段：

```go
func TDebug(ctx context.Context, msg string, kv ...any)
func TInfo (ctx context.Context, msg string, kv ...any)
func TWarn (ctx context.Context, msg string, kv ...any)
func TError(ctx context.Context, msg string, kv ...any)

func TDebugf(ctx context.Context, format string, args ...any)
func TInfof (ctx context.Context, format string, args ...any)
func TWarnf (ctx context.Context, format string, args ...any)
func TErrorf(ctx context.Context, format string, args ...any)
```

**两条路径完全等价**，T 版就是转发：

```go
log.TInfo(ctx, "订单创建", "order_id", id)      // 等价于
log.Ctx(ctx).Info("订单创建", "order_id", id)
```

链式版留着不是为了兼容——`With` 派生、`Enabled` 预判这些场景仍然需要它：

```go
l := log.Ctx(ctx).With("order_id", id)   // 派生一次，后面反复用
l.Info("校验通过")
l.Info("库存锁定")
```

T 前缀转发到门面，所以 **`SetLogger` 换后端后 T 系列照常生效**——它不是绕过门面的后门。

> **实现约束：两个 CallerSkip 实例。** 包级 T 函数比门面方法多一层栈帧，共用一个 logger 会让 `caller` 指到 log 包内部。binding 里必须持有两个实例——门面方法用 `AddCallerSkip(1)`，包级函数用 `AddCallerSkip(2)`。这个坑不写进实现清单，第一次看到 `caller: log/zap.go:88` 时会查很久。

### 8.4 业务代码的样子

只传 ctx，链路字段自动带上：

```go
func (s *OrderService) Create(ctx context.Context, req Req) error {
    ctx, end := log.Span(ctx, "OrderService.Create")
    defer end()

    log.TInfo(ctx, "校验通过", "order_id", id, "amount", amt)
    return s.dao.Insert(ctx, o)   // 传 ctx 下去，dao 里可再开子 span
}
```

**一次调用，两种渲染。** 同一条日志进 json encoder 和 console encoder，出来的是同一份数据的两个视图：

```jsonc
// json —— 机读，进 ELK/Loki
{"level":"info","ts":"2026-08-24T10:23:45.123+08:00","caller":"order/service.go:42",
 "msg":"校验通过","trace_id":"01926f7e8c83a4d5b6c7d8e9fa0b1c2d",
 "span_id":"b2c3d4e5f6a7b8c9","span_name":"OrderService.Create",
 "request_id":"01J8XQZ7K3M4N5P6Q7R8S9T0V1","order_id":"1001","amount":99}
```

```
# console —— 人读，一行，对齐着色（见 8.7）
10:23:45.123 INFO  01926f7e order/service.go:42       校验通过  order_id=1001 amount=99
```

`order_id` 在两边都是**可检索的独立字段**——json 里能 `order_id:1001` 精确过滤，console 里能 `grep order_id=1001`。这是把它写成 KV 而不是拼进 msg 的全部理由。

`end()` 自动打一条耗时日志，嵌套 span 自动串父子：

```json
{"level":"info","msg":"span done","span_name":"OrderService.Create",
 "span_id":"b2c3d4e5f6a7b8c9","parent_span_id":"a1b2c3d4e5f6a7b8","duration_ms":42}
```

**不显式开 span 也有 span_name。** 内建中间件用路由模板做根 span 名（`POST /api/v1/orders`），所以业务一个 `log.Span` 都不写，日志里也看得出这条是什么请求。显式开 span 是**增量收益**，不是使用门槛。

`span_id` 默认自动生成，`SpanID()` 选项可自定义——手写 span_id 是少数场景（如对齐外部系统的 ID），不该成为默认负担。

### 8.5 与 OTel SDK 互操作

用 OTel 的类型，最大的收益在这里：**接不接 SDK，业务代码一个字不改。**

`TraceFrom` 优先读 OTel 官方的 SpanContext，读不到才回退到 log 包自己维护的：

```go
func TraceFrom(ctx context.Context) Trace {
    if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
        return Trace{SpanContext: sc, ...}   // 用户接了 OTel SDK，直接用它的
    }
    return traceFromLocal(ctx)               // 没接，用轻量实现
}
```

于是两种模式自动切换：

| 模式 | `log.Span()` 行为 | 依赖 | span 去哪 |
|---|---|---|---|
| **默认** | 自己生成 span_id | API-only，很轻 | 只进日志 |
| `log.UseTracer(tracer)` 之后 | 委托给 OTel tracer 开真 span | 需引 OTel SDK | 日志 **+** Jaeger/Tempo |

一行配置从「日志链路」升级到「完整 tracing」，`log.Span(ctx, name)` 的调用点一个都不用动。这是不自己造 ID 类型换来的直接收益——自己造的话，这里得写一层双向转换，且永远有对不齐的风险。

### 8.6 配置：sink 各自决定格式

原来的「全局 `format` + `output` 列表」表达不了「终端要 console、文件要 json」这个最常见的诉求。改成**每个 sink 自带格式**：

```yaml
log:
  level: info
  caller: true
  stacktrace: error         # 该级别以上附堆栈

  console:
    enabled: true
    format: console         # console | json，默认 console
    color: auto             # auto | always | never

  file:
    enabled: true
    path: logs/app.log      # 后缀决定格式：.log → console，.jsonl → json
    format: ""              # 留空 = 按后缀推导；显式填写则覆盖推导
    rotate: daily           # daily | size
    max_size: 100           # MB，rotate=size 时生效
    max_age: 30             # 保留天数
    max_backups: 30
    compress: true
    error_path: logs/error.log   # error 级别单独落盘，格式同样按后缀推导

  sampling:                 # 高 QPS 防刷爆
    initial: 100
    thereafter: 100
  mask_fields: []           # 追加脱敏字段，内置黑名单始终生效
```

#### 文件后缀即格式声明

| 后缀 | 格式 | 意图 |
|---|---|---|
| `.log` | console | 人直接 `tail -f` 看的 |
| `.jsonl` | json | 采集器读的（JSON Lines，每行一个对象） |
| 其他 | console | 兜底 |

用 `.jsonl` 而非 `.json`，因为文件整体不是一个合法 JSON 值——`.json` 会让 `jq .` 直接报错，而 `.jsonl` 是这种「每行一个 JSON」布局的既有约定，`jq -c` 和主流采集器都认。

推导只是默认值，`format` 字段显式写了就以它为准——**约定优先，配置兜底**，跟框架其他地方的取向一致。

典型的三种组合：

```yaml
# 开发：只要终端，带色
console: {enabled: true}
file:    {enabled: false}

# 生产：终端给 k8s logs 看，文件给采集器
console: {enabled: true, format: json, color: never}
file:    {enabled: true, path: logs/app.log.jsonl}

# 传统部署：终端人看，文件也人看
console: {enabled: true}
file:    {enabled: true, path: logs/app.log}
```

多 sink 用 `zapcore.NewTee` 组装，**同一条日志被各自的 encoder 渲染一遍**——KV 数据只构造一次，呈现分两路。

#### `rotate: daily` 是我们的实现

lumberjack 只按大小滚，按日期得自己换 writer（文件名模板 + 零点触发）。实现不复杂，但要写清这是**我们的实现**而非 lumberjack 能力，免得后来者去它文档里找。

滚动后的文件名保留原后缀，格式推导才不会漂移：`app.log.jsonl` → `app-2026-08-24.log.jsonl`。

### 8.7 console 渲染：对齐与着色

console encoder 的目标是**一条日志一行，扫一眼能定位**。参考 logback 的 pattern layout，固定宽度的字段对齐，变长的自然流动：

```
10:23:45.123 INFO  01926f7e order/service.go:42       校验通过  order_id=1001 amount=99
10:23:45.156 WARN  01926f7e order/service.go:58       库存不足  sku=A100 remain=0
10:23:45.201 ERROR 01926f7e payment/client.go:33      支付失败  err="connection refused"
10:23:45.203 INFO  01926f7e xbc/assemble.go:112       span done  span_name=OrderService.Create duration_ms=42
```

| 段 | 宽度 | 规则 |
|---|---|---|
| 时间 | 12，固定 | `15:04:05.000`。日期不打——console 是给当下看的，文件名已带日期 |
| level | 5，左对齐 | `INFO ` / `WARN ` / `ERROR` / `DEBUG`，按级着色 |
| trace | 8，固定 | trace_id 前 8 位。32 位全打会挤掉正文，前 8 位在单机排查里足够区分 |
| caller | 24，右对齐 | 超长从**左侧**截断加 `…`，保住文件名和行号——`…service/order/dao.go:88` |
| msg | 变长 | 后跟两个空格再接 KV |
| KV | 变长 | `key=value` 空格分隔，value 含空格时加引号 |

**为什么不强行对齐 msg。** msg 长度差异太大，补齐到最长的那条会浪费半屏横向空间。zerolog 的 console writer、charmbracelet/log 都是这个取法。

#### 着色

| 元素 | 色 |
|---|---|
| DEBUG / INFO / WARN / ERROR | 青 / 绿 / 黄 / 红 |
| 时间、trace、caller | 暗灰——它们是坐标，不是内容 |
| key | 青 |
| value | 默认色 |
| `err` 字段的 value | 红——出错时眼睛直接落上去 |

`color: auto` 的判定顺序，任一不满足就关掉：

1. `NO_COLOR` 环境变量未设置（[no-color.org](https://no-color.org) 的事实标准）
2. 输出目标是 TTY（重定向到文件或管道时不该混入 ANSI 转义码）
3. `TERM != dumb`

第 2 条最要紧：`./myapp > app.log` 之后文件里全是 `\033[32m` 是个很常见的翻车现场。

### 8.8 脱敏做在 encoder 层

> **[SEC-INFO] 字段脱敏不能靠调用方自觉。** 包一层 `zapcore.Encoder` 拦截字段名，命中黑名单直接替换为 `***`：

```go
log.TInfo(ctx, "登录", "password", pwd)
// json:    {"msg":"登录","password":"***"}
// console: 10:23:45.123 INFO  01926f7e auth/login.go:31  登录  password=***
```

内置黑名单（始终生效，不可关闭）：`password`、`token`、`ulp-token`、`access_token`、`refresh_token`、`secret`、`private_key`、`AK`、`SK`、`db_url`、`id_card`、`bank_card`、`phone`。`log.mask_fields` 只能**追加**不能移除。

**脱敏在 encoder 链的最外层**，所以对 console 和 json 两个 sink 同时生效——不会出现「json 里脱了、console 里没脱」这种半拉子状态。

放在 encoder 层而非调用点的理由：调用点有几千个，encoder 只有一个。**走 `log.Zap()` 逃生舱口的日志同样被拦截**——脱敏在 encoder，绕过 Sugar 层绕不过它。

这也划出了 `SetLogger` 的边界：换掉后端就意味着**脱敏也换成了对方的实现**。文档必须明说这一点，否则「我换了个 logger，密码就进日志了」会成为一个没人预料到的事故。

### 8.9 框架侧接入

| 时机 | 动作 |
|---|---|
| 阶段 1 之后 | `log.Init(cfg)` 装配默认 zap 后端，此时配置已加载 |
| 内建 `trace` 中间件 | `propagation.TraceContext` 解析入站 header → 建根 span → `WithTrace` |
| 插件 `ctx.Log()` | 返回 `log.Logger` 接口，自动附 `plugin=gorm instance=readonly` |
| 阶段 10 最末 | `log.Sync()` 刷盘，放在所有插件 `Stop` 之后 |

宿主想换后端，在 `app.Run()` 之前调 `log.SetLogger(自己的实现)`，`log.Init` 就跳过 zap 装配。此后框架与全部插件的日志都走宿主的实现——因为它们依赖的是接口。

原设计的 `requestid` 中间件扩展为 `trace` 中间件——职责从「生成一个 ID」变成「建立链路上下文」，位置仍在 `PhaseObserve`，仍不可拔除。

### 8.10 其他仓库怎么用

零框架依赖意味着它可以脱离 xbc 单独用：

```go
import "github.com/xbcio/xbc/log"

func main() {
    log.Init(log.Config{
        Level:   "info",
        Console: log.ConsoleConfig{Enabled: true},
        File:    log.FileConfig{Enabled: true, Path: "logs/app.log.jsonl"},
    })
    defer log.Sync()

    log.L().Info("独立使用，不需要 xbc 框架")
    log.TInfo(ctx, "带链路", "order_id", id)   // ctx 里有 trace 就自动带上
}
```

链路能力也是自带的——`log.Span` / `TraceFrom` 不依赖框架，任何 Go 服务都能用它串起 trace_id 与 span。

已有日志体系的仓库，用门面接管即可——`log.SetLogger` 之后，所有走 `log.Ctx()` / `log.L()` / T 系列的代码（包括 xbc 插件）都落到你的实现上：

```go
log.SetLogger(myLogger{})   // 实现 log.Logger 的四个日志方法 + With + Enabled
```

**当前作为子包而非独立 module**：外部仓库 import 它会在 `go.sum` 里拉进 xbc 的全部依赖，但**编译产物不受影响**（Go 链接器只打包实际用到的包），代价仅是 `go mod download` 慢与 CVE 扫描噪音。

留好了拆的余地：`log/` 目录自包含、零框架依赖，将来真被大量外部使用，加一个 `log/go.mod` 即可拆成独立 module，**import path 不变**（下游只需改 `go.mod` 的 require 行）。

---

## 9. 零依赖子包

`log` 不是特例，而是一条原则的应用：**纯数据类型与通用能力，不该拖着 gin 一起走。**

问题出在这里：

```go
// domain/order.go —— 一个纯粹的领域层，不该知道 HTTP 存在
import "github.com/xbcio/xbc"
var ErrOrderClosed = xbc.New("ORDER.CLOSED", 409, "订单已关闭")
```

这一行 import 把 **gin 拖进了 domain 层**。因为根包里 `router.go` import 了 gin，而 Go 的编译单位是包——用 `xbc.Error` 就等于依赖整个根包。

所以零依赖的部分拆出去，根包用**类型别名**重新导出：

```go
// xbc.go
type Error        = errs.Error           // 别名，不是新类型
type Response[T any] = resp.Response[T]
var  ErrNotFound  = errs.ErrNotFound
func New(code string, status int, msg string) *Error { return errs.New(code, status, msg) }
```

两条路径并存，且拿到的是**同一个类型**（别名保证，不会出现两个 `Error` 互不兼容）：

| 使用者 | 写法 | 传递依赖 |
|---|---|---|
| 写 handler 的人 | `xbc.ErrNotFound` | 整个根包（本来就要用 gin） |
| domain 层 / 其他仓库 | `errs.ErrNotFound` | **零** |

| 子包 | 内容 | 依赖 |
|---|---|---|
| `log/` | `Logger` 门面 + zap binding + 链路追踪 | zap、lumberjack、OTel API |
| `errs/` | `Error` + 预置错误码 | 无 |
| `resp/` | `Response` / `Paged` | 无 |

代价是三个小包加十几行别名，收益是框架能用在有分层洁癖的项目里。

---

## 10. 目录结构

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
├── deps.go                   Deps / Dep / Ref / Need / NeedNamed / Opt / Offer / RefOf
├── context.go                Context + Go / GoCritical 托管
├── registry.go               Provide / Get / GetNamed / MustGet / MustGetNamed
├── router.go                 Router 链式元数据 + RouteInfo + 冻结路由表查询
├── middleware.go             Middleware / Phase
├── response.go               resp 的类型别名重导出 + H()
├── errors.go                 errs 的类型别名重导出
│
├── log/                      零框架依赖，可脱离 xbc 单独用
│   ├── logger.go             Logger 门面接口 + Level + ZapProvider
│   ├── sugar.go              T 系列包级语法糖（TInfo / TInfof / ...）
│   ├── zap.go                默认 binding：zap 装配、SetLogger、L / Ctx
│   ├── console.go            console encoder：对齐、着色、TTY 探测
│   ├── trace.go              Trace（内嵌 OTel SpanContext）/ Span / Fork
│   ├── mask.go               脱敏 encoder：内置黑名单 + mask_fields
│   ├── rotate.go             daily 滚动（lumberjack 只按大小滚，日期得自己来）
│   └── config.go             Config + 默认值 + 后缀推导 format
│
├── errs/                     零依赖：Error + 预置错误码
├── resp/                     零依赖：Response / Paged
│
├── internal/
│   ├── assemble/             十阶段装配管线、失败回滚、逆序关闭
│   ├── graph/                通用拓扑排序器（插件序 / 中间件序共用）
│   ├── inject/               tag 扫描（inject + provide）、注入与产物收割
│   ├── conf/                 koanf 加载、profile、ENV 映射、default + validate
│   └── httpx/                recovery / trace / accesslog 内建实现
│
├── plugins/
│   ├── gorm/                 基础设施（多实例）
│   ├── redis/                基础设施（多实例）
│   ├── cors/                 中间件
│   ├── ratelimit/            中间件
│   ├── jwt/                  认证（请求时查路由元数据）
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

## 11. 首批插件

每个插件对应压测一类机制：

| 插件 | 实现的接口 | 验证的机制 |
|---|---|---|
| `gorm` | Configurable, MultiInstancer, Initializer, HealthChecker, Closer | **多实例 + `provide` tag 收割 + 连接生命周期 + 逆序关闭** |
| `redis` | 同上 | 多实例键 `(type, instance)` 不串 |
| `cors` | Configurable, MiddlewareProvider | 最薄插件形态：2 个方法 |
| `ratelimit` | Configurable, MiddlewareProvider, Declarer | `After: cors` 软约束 |
| `jwt` | Configurable, MiddlewareProvider | **请求时查路由元数据**：读 `.Public()` 决定放行 |
| `cron` | Configurable, Initializer, Runner, Closer, Declarer | **`ctx.Go` 托管 + `RefOf` 硬依赖** |

`jwt` 是最关键的一个——它验证「插件通过路由元数据协作」这条设计是否真的成立，且它的实现必须证明**请求时查表**这条路径在时序上确实走得通。

**第四类能力（业务模块）由 `examples/full` 中的 `user` 插件承载**，它实现 `RouteProvider` + `Migrator` + tag 注入，验证业务插件能否只靠 import 就在另一个项目里复用。它不进 `plugins/` 目录——那里只放通用能力。

`GoCritical` 与 `PostRouter` 在首批插件中没有实现者（cron 用 `Go` 就够，swagger 不在首批）。二者由内核的假插件测试覆盖，真实实现者留到后续插件——**接口先立住，避免将来加它们时要改内核**。

> **[SEC-BRUTE]** `jwt` 插件本身不含登录端点，故无暴力破解面。但 examples 中的登录示例必须包含失败次数限制说明，避免使用者照抄出一个无防护的登录接口。

> **[SEC-SESSION]** `jwt` 插件配置必须强制 `secret` 非空且长度 ≥ 32 字节（`validate:"required,min=32"`），token 必须设置过期时间，默认 `expire: 2h`。禁止提供默认 secret。

---

## 12. 测试策略

**内核用假插件测，不碰任何真实中间件。** 拓扑排序、环检测、缺失依赖报错、逆序关闭、失败回滚全是纯逻辑，一组 `fakePlugin` 即可覆盖：

```go
func TestShutdownReverseOrder(t *testing.T) {
    var stopped []string
    a := fake("a").onStop(func() { stopped = append(stopped, "a") })
    b := fake("b").needs(a).onStop(func() { stopped = append(stopped, "b") })
    app := xbc.New().Register(a, b)
    app.start(); app.stop()
    assert.Equal(t, []string{"b", "a"}, stopped)   // 依赖者先停
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
- 启用规则：显式 Register 无配置节仍启用 / blank import 无配置节不启用 / `enabled: false` 两条路径都关得掉
- tag 注入：必需 / 可选 / 具名 / 与 `Dependencies()` 合并
- **接口匹配：唯一实现命中 / 零实现报错含最接近类型 / 多实现报错列全部候选 / `name=` 消歧后命中**
- **产物：`provide` tag 被收割登记 / 声明了却留零值 → 报错 / 与 `Provides()` 合并 / 多实例产物键正确**
- **`GoCritical`：panic 触发完整 shutdown（其他插件仍逆序 Stop）、提前返回同样触发、退出码为 1**
- **`Go`：panic 恢复后应用继续、shutdown 时被 cancel 且计入等待组**
- 迁移：默认不执行 / `--migrate` 执行 / `migrate` 子命令跑完即退

**日志包独立测**，门面接口让它变得容易——`SetLogger(captureLogger)` 就能断言输出：

- 脱敏：内置黑名单命中 → `***`；`mask_fields` 追加生效；**尝试移除内置项无效**
- 脱敏对 `log.Zap()` 逃生舱口同样生效（证明拦截在 encoder 而非 Sugar 层）
- 脱敏对 console 与 json 两个 sink 同时生效
- **格式推导：`.log` → console / `.jsonl` → json / 显式 `format` 覆盖推导 / 滚动后后缀不漂移**
- **console 渲染：固定段宽度正确、caller 超长从左截断、KV value 含空格时加引号**
- **着色：`NO_COLOR` 置位则关 / 非 TTY 则关 / `color: always` 强开**
- **T 系列与链式等价：同参数产出同字段；caller 指向业务代码而非 log 包**
- 链路：入站 `traceparent` 被解析并沿用 / 无入站头时新建 / `Fork` 后 trace_id 不变而 span_id 变
- `Ctx()` 在无链路上下文时回落到 `L()`，不 panic
- `SetLogger` 后框架与插件的日志全部落到替换实现上
- `rotate: daily` 跨零点换文件（注入可控时钟，不靠 sleep）

**对外提供契约测试套件**，第三方插件作者引一行即可自检：

```go
func TestGormPluginConformance(t *testing.T) {
    xbctest.Conform(t, gorm.New())
}
```

`Conform` 把插件装进一个 mini app 跑一遍冒烟装配，覆盖：`Name()` 非空且不含保留字符、`ConfigPtr()` 返回指针且能绑定、声明的产物在 `Init` 后确实非零、`Stop` 幂等（连调两次不 panic）、`Middlewares()` 的 `Name` 不重复。

**它测不到的**：真实依赖能否解析（那取决于宿主应用装了哪些插件）、多实例展开是否符合预期（取决于宿主配置）。这两项只能在宿主侧用 `doctor` 验证——文档里必须写明边界，避免 `Conform` 通过被误读成「这插件在我的应用里一定能跑」。

**真实中间件走 `-tags=integration`**，默认 `go test ./...` 不需要任何 Docker。

---

## 13. 实施顺序

```
0. log 子包     Logger 门面 + T 系列糖 + zap binding + console/json encoder
                + 脱敏 + Trace/Span + daily 滚动
      ↑ 排在最前：内核自己就要用它，且它零框架依赖，可独立测完再往上盖
1. 内核骨架     Plugin 接口族 / Base / Context / Register / Name 自动推导
2. 配置         koanf 加载 + profile + ENV + default/validate 绑定 + 多实例展开
                + 两条注册路径的启用规则
3. 依赖解析     拓扑排序器 + tag 扫描（inject + provide）+ 注册表
                + Deps/Provides 合并 + 产物收割与校验 + 全部报错文案
4. 装配管线     十阶段 + 优雅关闭 + 失败回滚 + Go/GoCritical
                + doctor 子命令 + 启动日志
      ↑ 到此内核可用假插件跑通全部测试，未引入 gin 之外任何依赖
5. HTTP 骨架    Router 元数据 + 冻结路由表 + PostRoutes / 响应 / 错误
                / recovery+trace+accesslog / healthz
6. 插件         cors → jwt → gorm → redis → ratelimit → cron
      ↑ 顺序有讲究：cors 最薄先跑通形态，jwt 验证请求时查元数据，
        gorm 验证多实例与 provide 收割，cron 验证 Runner 与硬依赖
7. examples + README
```

第 1~4 步**完全不引入 gin 之外的任何依赖**，内核正确性在接触真实中间件之前就被测试锁死。

第 3 步是整个实现的重心——依赖图两端（`inject` 与 `provide`）必须同时立住，缺一端拓扑排序就无从谈起。

---

## 14. 已知取舍

| 取舍 | 代价 | 缓解 |
|---|---|---|
| 单仓单 module | `go.sum` 大、CVE 扫描噪音、与使用方依赖版本冲突面大 | 二进制体积不受影响；目录按 `plugins/<name>/` 切干净，将来可拆多 module |
| 窄接口组合 | 方法名拼错 → 框架静默跳过 | 启动日志打印每个插件被识别到的能力；`xbctest.Conform` 契约测试 |
| 软依赖用字符串 | 拼错不报错 | `doctor` 与启动日志的「软约束未命中」段落 |
| tag 反射 | 反射有性能与可读性成本 | 只在阶段 4~5 装配期执行一次，请求链路零反射 |
| 产物需显式声明 | 比「直接 Provide」多一行 tag | 换来阶段 4 可建图、可 `doctor`、可在启动期抓出 nil 产物——这是必要成本，不是可选糖 |
| 元数据请求时查 | 每请求一次 map 查找 | 冻结后无锁，O(1)，代价可忽略；换来时序上的绝对正确 |
| 迁移默认关闭 | 「改了 model 表没变」会成为新的常见困惑 | 启动日志主动打印「迁移未执行」与修法；dev profile 一行开启 |
| 日志门面用 KV 变参 | 编译期不校验键值配对，`TInfo(ctx,"m","k")` 落地成 dangling key | binding 侧检测奇数参并降级为 `!BADKEY` 字段（同 slog）；`go vet` 风格的 lint 规则可后补 |
| KV 与 gfa 的 `Infoln` 语义不同 | 从 gfa 迁移的 `TInfo(ctx,"支付",id,amt)` **编译通过但把 id 当 key**，偶数参时连 `!BADKEY` 都不触发 | 迁移指南单列一节：拼接语义一律改走 `TInfof`；examples 里只出现 KV 写法 |
| 同一条日志渲染两遍 | 双 sink 时 encoder 跑两次 | KV 数据只构造一次，重复的只是序列化；高 QPS 下靠 `sampling` 兜底 |
| `SetLogger` 换后端 | **脱敏一并换成对方的实现**，内置黑名单失效 | 文档在 `SetLogger` 处显式警示；`doctor` 检测到非默认后端时打印一行提示 |
| 不做热重载 | 改配置须重启 | 插件接口保持简单；如确有需要，待接口稳定后再评估 |
| 内建三中间件不可拔 | 违背「一切皆插件」的纯粹性 | 它们是请求契约本身，可拔会让 `traceId` 与日志契约同时失效 |
| 接口数量偏多（12 个） | 第一印象复杂，学习曲线陡 | README 首屏必须是「最小插件长什么样」——一个 `Name()` 加一个 `Middlewares()` 的 15 行 cors；其余接口按需查表 |

### 14.1 审阅中考虑过但未采纳的方案

| 方案 | 未采纳的理由 |
|---|---|
| 产物用 `Provides() []Dep` 单独声明（不做 tag） | 与 `inject` 不对称，且多一个方法。tag 方案让依赖图两端出自同一次扫描 |
| 中间件装载前先跑一遍「影子路由器」收集元数据 | 要求 `RegisterRoutes` 可幂等执行两次，这是个很难让插件作者遵守的隐性契约 |
| `Phase` 改为开放的字符串锚点 | 会退化成软约束，跨 Phase 反向依赖就没法在启动期抓出来了 |
| 合并 `Migrate` 进 `Init` 阶段 | 迁移常需多个插件同时就绪；且合并后 `migrate` 子命令没法只跑迁移不建 HTTP |
| 所有后台任务统一按 critical 处理 | cron 的单次任务失败不该拖垮整个应用，一刀切会制造大量误重启 |
