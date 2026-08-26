# xbc — 包结构重构设计

- **日期**：2026-08-26
- **仓库**：`git@github.com:xbcio/xbc.git`
- **Module**：`github.com/xbcio/xbc`
- **状态**：设计定稿，待实现
- **前置文档**：`2026-08-23-xbc-plugin-framework-design.md`（本文档修订其 §10 目录结构）

---

## 1. 问题

根包现有 **17 个非测试文件、3202 行**，全部平铺在 `package xbc`：

| 文件 | 行数 | 职责 |
|---|---|---|
| `stage_resolve.go` | 449 | 依赖合并 + 拓扑排序 |
| `xbc.go` | 331 | App / instance 结构 + New / Register / Run |
| `stage_run.go` | 308 | 阶段 6-10 全挤在一个文件 |
| `stage_expand.go` | 286 | entry → instance 展开 |
| `registry.go` | 239 | (type, instance) 值存储 |
| `plugin.go` | 212 | Plugin 接口族 + Base + 命名校验 |
| `config.go` | 206 | 配置模型 + 阶段 1 加载 |
| `startuplog.go` | 204 | 启动日志渲染 |
| `stage_init.go` | 185 | 注入 → Init → 收割 → 回滚 |
| `mwchain.go` | 143 | 中间件 Phase 分组 + 组内拓扑排序 |
| `router.go` | 138 | Router / RouteInfo |
| `goroutine.go` | 116 | 托管协程与致命信号 |
| `deps.go` | 107 | Dep / Ref 构造糖 |
| `context.go` | 88 | 插件运行时句柄 |
| `middleware.go` | 73 | Middleware / Phase |
| `stage_config.go` | 63 | 绑定 + 校验 ConfigPtr() |
| `cli.go` | 54 | 命令行参数解析 |

三个具体后果：

1. **架构不可见**。目录树读不出这个框架由哪几个子系统构成，只能读出「有 17 个文件」。
2. **第三方插件契约埋在实现里**。`plugin/` 生态要求插件作者只依赖一个稳定的窄接口面，而现在 `Plugin` 接口与十阶段编排、拓扑排序、启动日志渲染同处一包——契约与实现无法分别演进。
3. **切分维度选错**。5 个 `stage_*.go`（1291 行，占 40%）按流水线的**时间轴**切分。时间轴是运行时属性，应当写进 `doc.go`，不应编码进文件名。

---

## 2. 命名法则

本次重构的命名与划分依据取自 Spring Framework / Spring Boot 的模块组织，三条法则：

**法则一：包名是名词，且是包内头号类型的名字。**
`spring-beans` 管 `Bean`，`spring-context` 管 `ApplicationContext`，`spring-core` / `spring-aop` / `spring-web` / `spring-test` 同理——**全部是「这个包管的那个东西」，没有一个是「这个包做的事」**。

动词命名的包（`assemble` / `boot` / `observe`）天然装得下任何东西，因而必然退化成杂烩桶。此前被否决的 `kernel` 命名正是这一病根的极端形态。

**法则二：只有存在独立用户的东西，才配当顶层公开包。**
`spring-beans` 可以脱离 `spring-context` 单独使用，所以它是模块。反之，**没有独立用户的实现细节不应提升为公开 API**——它应当进 `internal/`，以保留将来自由重构的余地。

**法则三：不用「我框架的一个部件」来命名。**
Spring 没有 `spring-kernel`、没有 `spring-engine`。这类名字只表达「它在框架内部」，不表达「它是什么」。

**边界**：借的是命名法则与模块划分原则，不是 Java 词汇表。`bean` / `factory` 在 Go 生态无人使用，`registry.Registry` 是典型 stutter，均不直搬。

---

## 3. 模块映射

| Spring / Spring Boot | 职责 | xbc 对应 |
|---|---|---|
| `spring-core` | 基础工具，不承诺给业务用 | `internal/{graph, inject, conf}` |
| `spring-beans`（`BeanFactory`） | Bean 定义、注册、实例化、依赖注入 | `internal/container/` |
| `spring-context`（`ApplicationContext`） | 容器生命周期、`refresh()` | 根包 `lifecycle.go` |
| `spring-boot`（`SpringApplication`） | 一行启动 | 根包 `xbc.go` |
| `spring-boot-autoconfigure` | 条件装配 | `Default()` 注册的内建插件 |
| `spring-boot-starter-*` | 依赖聚合 + 自动配置 + 默认值 | `plugin/gorm`、`plugin/redis` … |
| `spring-boot-starter-actuator` | 健康 / 信息端点 | `plugin/actuator/` |
| `spring-boot-test` | 测试支持 | `plugin/xbctest/` |
| `core.env`（`Environment`） | 外部化配置与配置源 | `config/` |
| `@ConditionalOnMissingBean` | 条件装配 | `Opt()` 软依赖 + enable 规则 |

Spring 把 **IoC 机制（beans）与容器生命周期（context）分成两个模块**，这与本设计中 `internal/container/` 与根包 `lifecycle.go` 的分法同构。

**不可移植的部分**：Spring 依赖注解 + 反射 + classpath 扫描完成自动装配，Go 没有等价机制。xbc 保持编译期显式注册 + struct tag 注入，不引入 `@ComponentScan` 式的隐式发现。

---

## 4. 目标结构

```
xbc/
├── xbc.go            App / New / Default / Register / Run
├── lifecycle.go      十阶段编排
├── goroutine.go      托管协程与致命信号
├── option.go         Option 装配选项
├── alias.go          契约类型别名重导出
├── doc.go            架构导航（十阶段时间轴写在这里）
│
├── plugin/           第三方插件作者的唯一入口
│   ├── plugin.go         Plugin + 12 个可选扩展点 + Base
│   ├── dep.go            Dep / Ref / Need / NeedNamed / Opt / Offer / RefOf
│   ├── context.go        Context（插件侧只读视图）
│   ├── router.go         Router / RouteInfo
│   ├── middleware.go     Middleware / Phase
│   ├── xbctest/          插件契约测试套件
│   ├── recovery/         ┐
│   ├── trace/            ├ 内建，Default() 注册
│   ├── accesslog/        │
│   ├── actuator/         ┘
│   ├── gorm/  redis/  cors/  ratelimit/  jwt/  cron/
│
├── config/           配置模型 / 加载 / 校验 / profile / 命令行参数
├── log/              已有，零框架依赖
├── errs/             零依赖：Error + 预置错误码
├── resp/             零依赖：Response / Paged
│
├── internal/
│   ├── container/    实例展开 + 依赖解析 + 注册表 + 注入
│   ├── startup/      启动报告渲染
│   ├── mwchain/      中间件 Phase 分组与组内排序
│   ├── graph/        通用拓扑排序器
│   ├── inject/       tag 扫描、注入与产物收割
│   ├── conf/         koanf 加载、profile、ENV 映射
│   └── testplugins/  测试用假插件
│
└── examples/
```

顶层公开包 5 个（`plugin` / `config` / `log` / `errs` / `resp`）加根包，每个都有明确的独立用户。

---

## 5. 关键决策

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | `App` 的归属 | **留在根包** | `SpringApplication` 就在最顶层的 `org.springframework.boot`。放进子包再在根包起别名是纯间接层，且会产生 `xbc.App` 与 `boot.App` 两个同名类型。用户写 `xbc.New()` / `xbc.Default()`，与 gin / echo / fiber 一致 |
| 2 | 注册表的可见性 | **降级为 `internal/container/`** | 插件通过 `Context` 取依赖，不直接碰注册表——没有独立用户（法则二） |
| 3 | `registry` / `expand` / `resolve` 的合并 | **合成一个 `internal/container/`** | 对应 Spring `DefaultListableBeanFactory` 一个类承担的三件事：定义注册、实例化、依赖解析。三者共享 `instance` 类型，拆开会迫使该类型跨包导出 |
| 4 | 可观测能力的归属 | **拆解**：端点 → `plugin/actuator/`，启动报告渲染 → `internal/startup/` | Actuator 在 Spring Boot 中本就是 starter 而非核心模块。此前的 `observe/` 包内容单薄且名为动词，不成立 |
| 5 | `plugin` 与 `plugins` | **合并为 `plugin/` 子树** | import 路径自洽：契约 `xbc/plugin`，实现 `xbc/plugin/gorm`。插件相关的一切在同一棵子树下 |
| 6 | 内建中间件的形态 | **就是插件**，由 `Default()` 注册 | 走与用户插件完全相同的注册路径，无特殊通道。等价于 Spring Boot 的自动配置类也只是普通 `@Configuration` |
| 7 | `Router` / `Middleware` 类型的归属 | **`plugin/`** | 它们出现在契约签名里（`RegisterRoutes(r *Router)` / `Middlewares() []Middleware`）。若下沉到别处会形成 `plugin → 该包 → plugin` 的循环 |
| 8 | 命令行参数的归属 | **`config/`** | 对应 Spring 的 `SimpleCommandLinePropertySource`——命令行是一种配置源 |
| 9 | 十阶段的文件命名 | **不再按阶段编号命名文件** | 时间轴是运行时属性，写进 `doc.go`。5 个 `stage_*.go` 合并进 `lifecycle.go` 与 `internal/container/` |
| 10 | 最小装配入口的命名 | **`New()`**，不用 `Base()` | `Base` 已是插件基类（`plugin.Base`），同名两义 |

### 5.1 `New()` 与 `Default()`

```go
func New(opts ...Option) *App      // 最小内核，不注册任何插件
func Default(opts ...Option) *App  // New() + 注册 recovery / trace / accesslog / actuator
```

`Default()` 与 `New()` 的差别有干净定义：**前者等于后者加上几个内建插件的常规注册调用**。这同时废止了前置文档 §10 的 `internal/httpx/`——内建中间件不再是特例实现，而是插件。

---

## 6. 文件搬迁映射

| 现文件 | 行数 | 去向 |
|---|---|---|
| `plugin.go` | 212 | `plugin/plugin.go` |
| `deps.go` | 107 | `plugin/dep.go` |
| `context.go` | 88 | `plugin/context.go` |
| `router.go` | 138 | `plugin/router.go` |
| `middleware.go` | 73 | `plugin/middleware.go` |
| | **618** | **→ `plugin/`** |
| `config.go` | 206 | `config/config.go` |
| `cli.go` | 54 | `config/args.go` |
| | **260** | **→ `config/`** |
| `registry.go` | 239 | `internal/container/registry.go` |
| `stage_expand.go` | 286 | `internal/container/expand.go` |
| `stage_resolve.go` | 449 | `internal/container/resolve.go` |
| | **974** | **→ `internal/container/`** |
| `startuplog.go` | 204 | `internal/startup/` |
| `mwchain.go` | 143 | `internal/mwchain/` |
| `xbc.go` | 331 | 根包 `xbc.go` |
| `stage_config.go` | 63 | ┐ |
| `stage_init.go` | 185 | ├ 根包 `lifecycle.go` |
| `stage_run.go` | 308 | ┘ |
| `goroutine.go` | 116 | 根包 `goroutine.go` |
| | **1003** | **→ 根包** |

合计 3202 行，与现状对平。根包从 17 个文件降至 6 个、约 1250 行（含新增的 `option.go` / `alias.go` / `doc.go`）。

---

## 7. 依赖方向

叶子层（无框架内依赖）：

```
log/        errs/        resp/        internal/graph/   internal/inject/   internal/conf/
```

上层依赖方向（箭头表示 import）：

```
    plugin/  ──────────► log/
       │
       ▼
 internal/container/ ──► internal/{graph, inject}

 config/ ─────────────► internal/conf

 internal/mwchain/ ───► plugin/          （需要 Middleware / Phase 类型）
 internal/startup/ ───► log/             （纯渲染，输入为 DTO）

 根包 xbc ────────────► plugin/, config/, internal/{container, mwchain, startup}
```

约束：

- `plugin/` 只依赖 `log/`。不得 import `config/`、`internal/*` 或根包——`ConfigPtr() any` 使契约无需知道配置实现
- `config/` 不依赖 `plugin/`。配置加载与插件契约互不相识，二者仅在根包会合
- `internal/container/` import `plugin/`（需识别扩展点接口）与 `internal/{graph, inject}`
- `internal/startup/` 为纯渲染：由根包传入 DTO，**不得直接读取装配层内部状态**
- 任何包不得 import 根包

### 7.1 arch_test 规则升级

`arch_test.go:26-30` 现钉的规则是「三个 internal 包互不依赖」。新增的 `internal/container/` 需 import `internal/graph` 与 `internal/inject`，与该规则冲突。规则改为分层版：

- **叶子层**（`graph` / `inject` / `conf`）：互不依赖，且不依赖根包——维持现状
- **上层**（`container` / `startup` / `mwchain`）：可依赖叶子层与 `plugin/`，不得依赖根包，不得互相依赖

实施时同步修改 `arch_test.go`，保持 `go list -test -deps` 的检测方式不变。

---

## 8. 已识别的迁移风险

| 风险 | 说明 | 处置 |
|---|---|---|
| `App` 未导出字段跨包 | `App` 约 20 个未导出字段被原 5 个 `stage_*.go` 密集读写 | 编排逻辑全部留在根包，`App` 不跨包——本方案已规避 |
| `instance` 类型跨包 | `internal/container/` 需要它，根包 `lifecycle.go` 也需要 | 类型定义随 `container/` 迁移并导出为 `container.Instance`，字段按需导出。根包通过它驱动阶段 5-10 |
| `plugin.Context` 的反向依赖 | `Context` 需提供取依赖能力，而实现在 `internal/container/` | `Context` 持有窄接口，运行时由根包注入 `container` 实现（依赖倒置） |
| 测试代码换包 | 约 4900 行测试分布在 15 个 `*_test.go` | 随所测代码同步迁移，分步进行，每步跑验收三件套 |
| 第三方依赖足迹 | 插件作者 import `plugin/` 会拉入 gin + zap + otel + lumberjack | 可接受：`plugin/` 契约本就以 gin 为前提。Go 1.17+ module graph pruning 保证 koanf / validator / ulid / testify 不进第三方构建产物 |

---

## 9. 实施路径

分四步，每步独立提交并跑验收三件套（`gofmt -l . && go vet ./... && go test ./... -count=1`）：

1. **建立 `plugin/`**：迁移 5 个契约文件 + 对应测试，根包 `alias.go` 做类型别名重导出，保证既有测试与 `examples/` 不改一行
2. **建立 `config/`**：迁移配置模型与命令行解析
3. **建立 `internal/container/` 与 `internal/{startup, mwchain}`**：迁移注册表、展开、解析、渲染、中间件排序；同步升级 `arch_test.go`
4. **收尾**：根包合并 `stage_*.go` 为 `lifecycle.go`，补 `option.go` / `doc.go`，实现 `New()` / `Default()`

前三步为增量迁移，根包保持可编译；第四步清空遗留。

---

## 10. 非目标

- **不改变任何公开 API 的语义**。根包通过 `alias.go` 重导出契约类型，既有用法 `xbc.Plugin` / `xbc.Dep` 继续可用
- **不引入运行时反射式自动发现**。保持编译期显式 `Register()`
- **不拆分为多 module**。维持单仓单 module（前置文档决策 6）
- **不在本次实现 `plugin/actuator/` 与首批插件**。本次只确立结构；插件实现是后续独立任务
- **不改动 `log/`**。它已满足零框架依赖约束
