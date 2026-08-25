# xbc 内核实现计划（插件契约 / 配置 / 依赖解析 / 装配管线）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 `github.com/xbcio/xbc` 根包内核 —— 插件接口族与 `Base`、按 `(类型, 实例名)` 索引的注册表、koanf 配置体系（profile / ENV / default / validate / 多实例展开）、一次 tag 扫描得出两端的依赖图与拓扑排序，以及十阶段装配管线（含失败回滚、逆序关闭、`Go`/`GoCritical` 托管、`migrate` 与 `doctor` 子命令）。终点是 spec §13 第 4 步的验收线：**内核可用假插件跑通全部测试，不接触任何真实中间件**。

**Architecture:** 公开 API 全部集中在根包，插件与用户只 import 一个包。`internal/` 只放**不认识任何 xbc 类型**的三个纯逻辑包（拓扑排序、配置加载、tag 扫描），它们各自可独立测；一切需要触碰 `Plugin` / `Context` / `Base` 的装配逻辑留在根包（理由见「裁决 R1」）。依赖图的两端由同一次 tag 扫描得出：`inject` 给出「谁要」，`provide` 给出「谁给」，因此阶段 4 排序时产物归属是静态可知的。同一份拓扑排序器服务三处：插件 Init 顺序、中间件顺序、Stop 顺序（Init 顺序取逆）。

**Tech Stack:** Go 1.25+ / gin v1.12.0 / koanf v2.3.6 / validator v10.30.3 / 本仓库 `xbc/log`（已完成）/ testify v1.12.1

**Spec:** [docs/superpowers/specs/2026-08-23-xbc-plugin-framework-design.md](../specs/2026-08-23-xbc-plugin-framework-design.md) —— 本计划实现其 §2、§4、§5、§6、§10、§12 中的内核部分，即 §13 的第 1~4 步。

**不在本计划范围内（留给后续 plan）：** §7 HTTP 层（`Router` 链式元数据、冻结路由表的元数据查询、`resp` / `errs` 子包、内建 recovery/trace/accesslog、healthz）→ Plan 3；§11 首批插件 → Plan 4；examples 与 README → Plan 5。

---

## Global Constraints

以下约束对**每一个** task 隐式生效，不再逐条重复：

- **module**：`github.com/xbcio/xbc`，单仓单 module。本计划在既有 `go.mod` 上追加依赖，不新建 module。
- **go 指令**：保持 `go 1.25.0`。gin v1.12.0 与 validator v10.30.3 的下限都恰好是 1.25.0，无需抬高。
- **依赖版本锁死**：新增 `github.com/gin-gonic/gin v1.12.0`、`github.com/knadh/koanf/v2 v2.3.6`、`github.com/knadh/koanf/parsers/yaml v1.1.1`、`github.com/knadh/koanf/providers/file v1.2.1`、`github.com/knadh/koanf/providers/env/v2 v2.0.1`、`github.com/knadh/koanf/providers/confmap v1.0.1`、`github.com/go-playground/validator/v10 v10.30.3`。既有的 zap `v1.28.0`、lumberjack.v2 `v2.2.1`、otel `v1.45.0`、ulid/v2 `v2.1.2`、go-isatty `v0.0.24`、testify `v1.12.1` 一律不动。**不引入计划外的第三方依赖**，尤其不引入 cobra（子命令用标准库 `flag`，见 spec §4.2）。
- **依赖方向（硬约束）**：内核 → `log`，绝不反向。`log/` 下任何文件仍然一行都不能 import 根包或 `internal/*`（`log/nodeps_test.go` 已守住这条线，本计划不得削弱它）。
- **`internal/` 的洁癖（硬约束）**：`internal/graph`、`internal/conf`、`internal/inject` 三个包**不得 import 根包 `github.com/xbcio/xbc`**，也不得互相 import。它们的入参与返回值只能是标准库类型、`reflect` 类型和自己定义的类型。Task 15 有一条自动化测试守这条线。
- **测试包名**：根包测试用 `package xbc`（内部测试，要断言未导出的 `registry`、`expand`、`bindBase` 等）；`internal/*` 的测试用各自的包名。
- **内核只用假插件测**：本计划的任何测试都不得 import gorm / redis / jwt 等真实中间件，也不得依赖 Docker 或网络。
- **注释与文档语言**：**代码注释一律英文**；README、错误信息（含 `fmt.Errorf` 文案）、测试断言 message、测试数据字符串一律中文；标识符、tag、配置 key 保持英文。判断边界：**引号里的中文不动**，注释（`//` 与 `/* */` 之后）才转英文。
- **验收三件套**：每次提交前跑 `gofmt -l . && go vet ./... && go test ./... -count=1`，三条全绿才提交。注意是 `./...` 不是 `./`，本计划开始有多个包。
- **提交粒度**：每个 task 的每个 "Commit" 步骤都真的提交一次，不攒批。提交信息用中文。
- **不 push**：本计划全程只在本地 `main` 上提交，不执行 `git push`。

---

## 裁决（Rulings）

spec 有若干处自相矛盾或在实现层面走不通。以下裁决在写计划时已经作出，执行时按裁决走，**不要回头按 spec 原文实现**。每条都记了「错了的代价」。

### R1：`internal/assemble` 取消，十阶段管线留在根包

spec §10 把管线放在 `internal/assemble/`，同时把 `plugin.go` / `context.go` / `registry.go` 放在根包。管线要构造 `*Context`、要给 `Base` 回填名字、要调 `Plugin` 的可选接口 —— 它必须认识这些类型。于是 `internal/assemble` 得 import 根包，而根包又得 import 它才能跑起来：**import 循环**。

裁决：管线实现为根包的 `xbc.go` / `stage_*.go`。`internal/` 只保留 spec 中三个天然不认识 xbc 类型的包：`graph`（纯字符串节点的拓扑排序）、`conf`（koanf 加载与结构体绑定）、`inject`（`reflect` 层的 tag 扫描）。spec §10 的目录树是草图，import 循环是硬事实。

**错了的代价：** 根包文件数变多（多 5 个文件）。反过来若强行拆包，只能靠「根包注入回调进 internal」的间接层，读代码的人要多跳一跳才能看懂装配顺序 —— 这是更贵的代价。

### R2：ENV 覆盖改为「按 schema 反查」，不做 `_` → `.` 的字面替换

spec §6.1 写「大写、`_` 转 `.`」。这条规则在多词 key 上直接崩：`XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN` 会被译成 `plugins.gorm.default.max.open.conn`，而真实 key 是 `plugins.gorm.default.max_open_conn`。分隔符与词内下划线无法区分。

裁决：**ENV 覆盖由目标结构体的 schema 驱动，方向反过来**。绑定 `plugins.gorm.readonly` 到 `*gorm.Config` 时，先由 `yaml` tag 枚举出全部叶子路径（`plugins.gorm.readonly.dsn`、`plugins.gorm.readonly.max_open_conn`…），再对每条路径算出它的环境变量名（`strings.ToUpper` 后把 `.` 换成 `_`，加 `XBC_` 前缀）去 `os.LookupEnv`。用户看到的环境变量名与 spec 原文**完全一致**，歧义从根上消失。

代价与边界：ENV 只能覆盖**有结构体 schema 的配置**（`server.*`、`log.*`、`plugins.*`），覆盖不到 `app.*` 这种框架不解析的自由配置。这条边界必须写进 README。

**错了的代价：** 有人想用 `XBC_APP_FEATURE_X` 覆盖业务配置会发现不生效。可接受 —— `app.*` 的形状框架本来就不知道，字面替换在那里同样是错的。

### R3：`NeedNamed` 的签名以 spec 第 345 行为准

spec 第 345 行是 `func NeedNamed[T any](instance string) Dep`，第 555 行的用例 `xbc.NeedNamed[*gorm.DB](p.Cfg.Shard)` 与之一致；但第 485 行写成了 `xbc.NeedNamed(xbc.RefOf[Counter](), p.Cfg.CounterInstance)` —— 既与签名不符，又误用了 `RefOf`（后者约束是 `[T Plugin]`，用于插件引用，不是类型依赖）。

裁决：`NeedNamed[T any](instance string) Dep` 为准。§5.6 那个例子在计划里改写为 `xbc.NeedNamed[Counter](p.Cfg.CounterInstance)`。

**错了的代价：** 无。二比一，且另一个写法根本不能编译。

### R4：`Phase` 常量之间留出间隔

spec 第 596 行写 `PhaseRecover Phase = iota`（值 0,1,2,3,4，没有间隔），但同节「逃生舱口」段落又说「五个常量之间**刻意留出间隔**」。

裁决：`Phase = iota * 100`，即 0 / 100 / 200 / 300 / 400。`PhaseRecover - 1` 这个逃生舱口两种写法下都成立，但只有留间隔才对得上它自己的说明文字。

**错了的代价：** 无。数值本身不进配置文件也不进日志（日志打的是 `[security]` 这种名字）。

### R5：中间件名的前缀规则 —— 同名不加前缀

spec §5.8 说 `Name` 「不含 `.` 时自动加插件名前缀」，但 §4.4 的启动日志里 cors 插件的 cors 中间件显示为 `cors` 而不是 `cors.cors`。

裁决：`qualify(plugin, name)`：含 `.` → 原样；`name == plugin` → 就用 `plugin`；否则 → `plugin + "." + name`。这条规则能同时复现 spec 里 `cors`、`jwt.auth`、`ratelimit` 三个例子。§4.4 里 `myapp.audit` 那一行与 §5.8 的 `AuditPlugin` 对不上（一个插件不可能既叫 `audit` 又叫 `myapp`），按本规则它显示为 `audit`。

**错了的代价：** 别的插件写 `After: "cors.cors"` 会失配。但那是软约束，会进「软约束未命中」段落，一眼看得见。

### R6：`plugins.<key>` 有配置节但无对应插件 → **启动中止**，不是警告

spec §6.2 把这条呈现为「诊断」，措辞未说是否致命。

裁决：**致命**。这条诊断存在的全部理由就是治「写了配置、忘了 import、静默不生效」；如果它自己也是静默的（只打一行 warn），那个 bug 换个马甲照样漏过去。`server.` / `log.` / `app.` 三个命名空间豁免（前两个是框架保留，第三个框架不解析）。

**错了的代价：** 删插件时忘了删配置节会挡住启动。修法是删掉那一节，而错误信息会把节名和建议一起打出来。

### R7：多实例插件的实例克隆是「新建零值」，不是「拷贝原型」

`app.Register(user.New())` 注册的原型可能带构造参数状态；而多实例展开要产出 N 个独立实例。浅拷贝结构体会连锁地拷贝里面的 `sync.Mutex`（`go vet` 会报），深拷贝在通用场景下不可能正确。

裁决：
- 显式 `Register` 且最终只展开出 1 个实例 → **直接用原型本身**，构造参数完整保留（这是业务插件的主路径）。
- 其余情况（blank import 注册，或多实例展开出 ≥1 个实例）→ 每个实例走 `reflect.New(T)` 新建零值。
- 因此**多实例插件必须零值可用**，其状态只能来自配置。这条写进插件开发文档，后续由 `xbctest.Conform` 检查。

**错了的代价：** 有人写了个带构造参数的多实例插件，会发现参数丢了。展开时会打一条 warn 提示（见 Task 8），不是静默。

### R8：完全没有配置文件是合法的

三个查找位置（`--config` / `./application.yml` / `./configs/application.yml`）都不存在时，不报错，按空配置继续。显式 `Register` 的插件仍然启用并走默认值（§6.4 第一行规则），blank import 的插件一个都不启用。

**错了的代价：** 打错 `--config` 路径会静默按空配置跑。所以**显式传了 `--config` 但文件不存在时必须报错**，只有「三个位置都没找」才静默。

### R9：koanf 的 unmarshal tag 用 `yaml`

koanf 默认按 `koanf` tag 绑定结构体，而 spec §6.3 的插件 `Config` 用的是 `yaml:"dsn"`。全部绑定走 `UnmarshalWithConf` 并设 `Tag: "yaml"`。

### R10：`server.write_timeout` 是本计划补的字段

spec §6.2 列了 `read_timeout` 却没列 `write_timeout`。只有读超时没有写超时的 `http.Server` 留着慢速读取的攻击面。补上，默认 `30s`。§6.2 是示意性片段而非字段全集。

### R11：阶段 3 的三步顺序是「unmarshal → ENV 覆盖 → 补 default → validate」

spec §4 阶段 3 写「unmarshal → 填 default tag → 跑 validate」，没说 ENV 插在哪。ENV 优先级高于文件（§6.1），所以必须在补 default 之前生效，否则「文件里没写、ENV 里写了」的字段会先被 default 填掉。

补 default 时**只填「配置里根本没出现过这条路径」的字段**，不能用「字段是零值」当判据 —— 那样 yml 里显式写的 `enabled: false`、`max_retries: 0` 会被默认值悄悄改掉。因此补 default 的函数必须能查 koanf 的 key 集合与 ENV 命中集合。

### R12：`App` 与 `instance` 两个结构体由 Task 1 一次性声明到位，五个字段例外

Task 8 / 10 / 13 / 14 / 15 都写着「这些字段是 Task 1 的骨架预留的」，但 Task 1 原本只声明了 `mu` 与 `entries`，`type instance struct` 更是一个 task 都没声明过 —— 五个 task 都在消费一个不存在的类型。这不是文案问题：按原样执行，Task 8 第一个 `go build` 就会 `undefined: instance`。

裁决是让那句话变成真的：**Task 1 把 `App` 与 `instance` 的完整字段集一次性写好**，包括阶段 6~10 才会用到的 `router` / `httpServer` / `ready` 等。Go 允许结构体字段声明后暂时无人读写（不像未使用的变量和 import），`go vet` 也不报，所以提前占位不付出任何代价，换来的是后面七个 task 不必各自猜字段名。

**五个字段例外，因为它们的类型在 Task 1 时还不存在**，只能由各自的 task 用 `Edit` 追加：

| 字段 | 类型定义在 | 由哪个 task 追加 |
|---|---|---|
| `App.cfg *Config` | `config.go`（Task 6） | Task 6 |
| `App.registry *registry` | `registry.go`（Task 4） | Task 4 |
| `App.softMisses []graph.Miss` | `internal/graph`（Task 3） | Task 14 |
| `App.middlewareChain []mwEntry` | `mwchain.go`（Task 11） | Task 14 |
| `instance.fields []inject.FieldSpec` | `internal/inject`（Task 7） | Task 10 |

**错了的代价**：把这五个也塞进 Task 1，Task 1 就会 import 一批当时还不存在的包，整个 task 编译不过；反过来，若不做这条裁决而维持原状，后面五个 task 各自按自己的想象补字段，同一个字段会出现 `order` / `sorted` / `initOrder` 三个名字，`stage_run.go` 与 `cli.go` 对不上。

---

## File Structure

| 文件 | 职责 | Task |
|---|---|---|
| `go.mod` / `go.sum` | 追加 gin / koanf / validator | 1 |
| `plugin.go` | `Plugin` + 12 个可选接口 + `Base` + 名字推导 | 1 |
| `xbc.go` | `App` / `instance` 完整字段集、包级 `Register`、`(*App).Register`、来源标记（Task 1 一次性声明，4/6/10/14 各追加一个类型当时还不存在的字段，裁决 R12） | 1, 4, 6, 10, 14 |
| `context.go` | `Context`（Task 1 立骨架，4 补注册表访问，12 补托管 goroutine） | 1, 4, 12 |
| `middleware.go` | `Middleware` / `Phase`（Task 1 立类型，11 补排序） | 1, 11 |
| `router.go` | `Router` / `RouteInfo` 最小形态（Plan 3 补链式元数据） | 1, 14 |
| `deps.go` | `Deps` / `Dep` / `Ref` / `Need` / `NeedNamed` / `Opt` / `Offer` / `RefOf` | 2 |
| `internal/graph/graph.go` | 通用稳定拓扑排序器 + 环路径 + 软约束未命中 | 3 |
| `registry.go` | `(类型, 实例名)` 注册表、接口可赋值性匹配、最接近类型诊断 | 4 |
| `internal/conf/load.go` | 文件查找、profile 合并、koanf 装配 | 5 |
| `internal/conf/bind.go` | schema 驱动的 ENV 覆盖 + key 集合感知的 default 填充 | 5 |
| `internal/conf/validate.go` | validator 调用 + 中文错误文案 + yaml 路径还原 | 6 |
| `config.go` | 根包 `Config` / `ServerConfig` + `log` 段接线 + 阶段 1 `loadConfig` | 6 |
| `internal/inject/scan.go` | `xbc:"inject"` / `xbc:"provide"` tag 扫描 | 7 |
| `stage_expand.go` | 阶段 2：多实例展开 + 启用规则 + 孤儿配置节诊断 | 8 |
| `stage_config.go` | 阶段 3：逐实例绑定配置 + 错误聚合 | 9 |
| `stage_resolve.go` | 阶段 4：扫 tag、合并依赖图两端、产物索引、报错文案、拓扑排序 | 10 |
| `mwchain.go` | 中间件排序：Phase 硬边界 + 组内软序 + 跨 Phase 反向检测 | 11 |
| `goroutine.go` | `Context.Go` / `GoCritical` + 托管生命周期 | 12 |
| `stage_init.go` | 阶段 5：注入 → Init → 收割 → 校验 → 失败回滚 | 13 |
| `stage_run.go` | 阶段 6~10：Migrate / AssembleHTTP / Start / Serve / Shutdown | 14 |
| `cli.go` | `flag` 解析、`migrate` / `doctor` 子命令、启动日志 | 15 |
| `application.example.yml` | 配置样例 | 15 |

拆分依据：三个 `internal/` 包是**纯逻辑、无 xbc 类型**的独立可测单元，各自一个 task；根包里按十阶段的自然边界切 `stage_*.go`，每个阶段一个 task，因为每个阶段都有自己独立的一组错误文案要测。

---

### Task 1: go.mod 依赖 + 插件接口族 + `Base` + 名字推导 + 两条注册路径

**Files:**
- Modify: `go.mod`、`go.sum`
- Create: `plugin.go`、`xbc.go`、`context.go`、`middleware.go`、`router.go`
- Create（测试夹具，非测试文件）: `internal/testplugins/jwt/jwt.go`、`internal/testplugins/foo/v2/foo.go`
- Test: `plugin_test.go`、`register_test.go`

**Interfaces:**
- Consumes：已完成的 `github.com/xbcio/xbc/log`（`log.Logger`、`log.L()`）
- Produces：
  - `type Plugin interface{ Name() string }`
  - 12 个可选能力接口：`Configurable`、`MultiInstancer`、`Declarer`、`Provider`、`Initializer`、`Migrator`、`MiddlewareProvider`、`RouteProvider`、`PostRouter`、`Runner`、`Closer`、`HealthChecker`（签名逐字见下方实现）
  - `type Dep struct{ Type reflect.Type; Instance string; Optional bool }`、`type Ref struct{ typ reflect.Type; instance string }`、`type Deps struct{ Types []Dep; Plugins []Ref; After, Before []string }`（本 task 只声明形状，构造函数留给 Task 2）
  - `type Base struct{ ctx *Context; name string }` + `(*Base) Ctx() *Context`、`(*Base) Log() log.Logger`、`(*Base) Name() string`、`(*Base) base() *Base`
  - `type baseAnchor interface{ base() *Base }`
  - `func deriveName(p Plugin) (string, error)`
  - `func bindBase(p Plugin, ctx *Context, name string) bool`
  - `func validateName(s string) error`
  - `type source int`，常量 `sourceImport`、`sourceRegister`
  - `const defaultInstance = "default"`
  - `type entry struct{ proto Plugin; name string; src source; multi bool }`
  - `func Register(p Plugin)`（包级）
  - `type App struct{...}` —— **完整字段集，见 Step 5，裁决 R12**。后续七个 task 全部按这些字段名读写，不再各自新增
  - `type instance struct{...}` —— **同样在 Step 5 一次性声明完整**（`id()` / `label()` 两个方法留给 Task 8，因为它们的正确性依赖 `expand()` 产出的实例形状）
  - `func New() *App`、`func (a *App) Register(p ...Plugin) *App`、`func (a *App) Run()`
  - `type Context struct{ app *App; name, instance string; logger log.Logger }` + `Log()`/`Name()`/`Instance()`（其余字段与方法留给 Task 4/6/12）
  - `type Phase int` + `PhaseRecover`/`PhaseObserve`/`PhaseSecurity`/`PhaseAuth`/`PhaseBusiness`
  - `type Middleware struct{ Name string; Phase Phase; After, Before []string; Handler gin.HandlerFunc }`
  - `type RouteInfo struct{ Method, Path string }`
  - `type Router struct{ engine *gin.Engine; group *gin.RouterGroup; basePath string; routes *[]RouteInfo; frozen *bool }`

- [ ] **Step 1: 追加依赖**

```bash
cd /Users/10097292/Desktop/caffe/xbcio/xbc
go get github.com/gin-gonic/gin@v1.12.0
go get github.com/knadh/koanf/v2@v2.3.6
go get github.com/knadh/koanf/parsers/yaml@v1.1.1
go get github.com/knadh/koanf/providers/file@v1.2.1
go get github.com/knadh/koanf/providers/env/v2@v2.0.1
go get github.com/knadh/koanf/providers/confmap@v1.0.1
go get github.com/go-playground/validator/v10@v10.30.3
```

不要在这之后跑 `go mod tidy`。这一步只追加依赖，还没有任何 `.go` 文件 import 它们；`go mod tidy` 会按「谁被真正 import」来判断一个 require 是否该留在 `go.mod` 里，koanf 系列和 validator 现在谁都没 import，会被直接整段删掉（已用一次性草稿验证过：`go get` 完六个 koanf/validator 包后立刻 `go mod tidy`，`go.sum` 里 `koanf` 字样清零）。所以本 task 只 `go get`，不 `tidy`。

确认 `go.mod` 的第一个 `require` 块里出现 `github.com/gin-gonic/gin v1.12.0`（`gin` 已经被 `middleware.go`/`router.go` import，会落在不带 `// indirect` 的直接依赖块）；第二个 `require` 块（`// indirect`）里出现 `github.com/go-playground/validator/v10 v10.30.3`、`github.com/knadh/koanf/v2 v2.3.6`、`github.com/knadh/koanf/parsers/yaml v1.1.1`、`github.com/knadh/koanf/providers/file v1.2.1`、`github.com/knadh/koanf/providers/env/v2 v2.0.1`、`github.com/knadh/koanf/providers/confmap v1.0.1`。这六个暂时挂在 `// indirect` 块完全正常——Go 工具链按「有没有代码 import 它」而不是「有没有手动 `go get` 过它」来分直接/间接两块，koanf 和 validator 真正被 import（`internal/conf`，Task 5/6/8）之后会自动挪到第一块，不需要手工搬。`go 1.25.0` 保持不变（gin 与 validator 的下限恰好都是 1.25.0）。

- [ ] **Step 2: 写测试夹具包**

`deriveName` 的核心行为是从插件值的包路径反射出插件名，这需要一个"真实的、有意义的多段包路径"才能测出东西——如果直接在本文件里定义一个类型，它的包路径末段永远是 `xbc`，测不出"取包路径末段"这条规则本身。

但本文件是 `package xbc`（根包内部测试，按全局约束，需要断言未导出的 `deriveName`/`bindBase`/`validateName` 等），这就撞上一条 Go 的硬限制：`package xbc` 的测试文件不能 import 一个反过来 import `xbc` 的包，哪�件事只在 `_test.go` 里发生也不行——Go 仍然判定为 import 循环。用一个独立的临时 module 验证过这条限制确实存在（`example.com/mod` 与其子包 `sub`：`sub` import `mod`，`mod` 内部测试 import `sub`，`go test` 报 `import cycle not allowed in test`）。

解法：两个夹具包放在 `internal/testplugins/` 下，**不 import `xbc`**，插件身份只靠结构性满足（有一个 `Name() string` 方法就够，不需要真的嵌入 `xbc.Base`）：

创建 `internal/testplugins/jwt/jwt.go`：

```go
// Package jwt is a minimal plugin fixture used only by xbc's own tests, to
// exercise deriveName's package-path parsing with a real, nested package
// path. It deliberately does NOT import github.com/xbcio/xbc (embedding
// xbc.Base would): xbc's internal ("package xbc") test files cannot import a
// package that itself imports xbc, because Go treats that as an import
// cycle even when the only path back to xbc runs through a _test.go file.
// Plugin identity only needs to be satisfied structurally -- a Name method
// is enough, no embedding required.
package jwt

// Plugin is a stand-in plugin type. Its Name method is never actually called
// by the tests that use this fixture -- they call xbc's unexported
// deriveName directly, which only inspects the type's package path via
// reflection and never invokes Name() at all.
type Plugin struct{}

func (p *Plugin) Name() string { return "" }
```

创建 `internal/testplugins/foo/v2/foo.go`：

```go
// Package foo, nested under a "v2" directory, is a minimal plugin fixture
// used only by xbc's own tests, to exercise deriveName's major-version-
// suffix stripping (".../foo/v2" -> "foo"). See the sibling jwt fixture for
// why this package doesn't import xbc.
package foo

// Plugin is a stand-in plugin type; see jwt.Plugin's comment for why a bare
// Name method is enough here.
type Plugin struct{}

func (p *Plugin) Name() string { return "" }
```

`foo` 包故意嵌套在一层名叫 `v2` 的目录下，专门用来测 `deriveName` 跳过 major-version 路径段这条规则——它的完整包路径是 `github.com/xbcio/xbc/internal/testplugins/foo/v2`，末段 `v2` 会被 `deriveName` 识别为版本号并跳过，取前一段 `foo`。

- [ ] **Step 3: 写失败测试**

创建 `plugin_test.go`：

```go
package xbc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	foo "github.com/xbcio/xbc/internal/testplugins/foo/v2"
	"github.com/xbcio/xbc/internal/testplugins/jwt"
)

// deriveName's package-path parsing is the one piece of this file that
// genuinely needs a real, nested package path to exercise properly -- but
// this test file is "package xbc" (an internal test, per the plan's global
// constraint), so any type defined right here would derive "xbc" itself,
// telling us nothing about path parsing. The two fixtures under
// internal/testplugins/ exist purely to give deriveName a realistic,
// multi-segment path to chew on; see their own doc comments for why they
// don't embed xbc.Base.
func TestDeriveNameFromPackagePath(t *testing.T) {
	name, err := deriveName(&jwt.Plugin{})
	require.NoError(t, err)
	assert.Equal(t, "jwt", name, "取包路径末段")
}

func TestDeriveNameStripsMajorVersionSuffix(t *testing.T) {
	name, err := deriveName(&foo.Plugin{})
	require.NoError(t, err)
	assert.Equal(t, "foo", name, "末段形如 v+数字时取前一段")
}

// notAPointerPlugin and nonStructPlugin don't need a realistic package path
// -- deriveName rejects them before it ever looks at PkgPath -- so they're
// defined right here instead of as fixtures.
type notAPointerPlugin struct{}

func (notAPointerPlugin) Name() string { return "value" }

type nonStructPlugin int

func (p *nonStructPlugin) Name() string { return "nonstruct" }

func TestDeriveNameRejectsNonPointer(t *testing.T) {
	_, err := deriveName(notAPointerPlugin{})
	assert.Error(t, err, "非指针插件不能自动推导名字")
}

func TestDeriveNameRejectsNonStructPointer(t *testing.T) {
	var p nonStructPlugin
	_, err := deriveName(&p)
	assert.Error(t, err, "指向非结构体的指针不能自动推导名字")
}

func TestValidateNameRejectsReservedCharacters(t *testing.T) {
	cases := []string{"a.b", "a[b]", "a]b", "a b", "ABC", "用户"}
	for _, s := range cases {
		assert.Error(t, validateName(s), "名字 %q 应当被拒绝", s)
	}
}

func TestValidateNameAcceptsPlainLowercase(t *testing.T) {
	for _, s := range []string{"jwt", "gorm_v2", "rate-limit", "a1"} {
		assert.NoError(t, validateName(s), "名字 %q 应当合法", s)
	}
}

func TestValidateNameRejectsEmpty(t *testing.T) {
	assert.Error(t, validateName(""), "空名字不合法")
}
```

创建 `register_test.go`：

```go
package xbc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCleanRegistry isolates a test from the package-level `registered`
// slice, which is process-global mutable state shared by every test in this
// binary (Go runs all tests in a package in one process). Without this, a
// plugin left behind by an earlier test would still be sitting in
// `registered` when New() seeds a later test's App, and two tests that both
// happen to derive the same name (very likely here, since every fixture in
// this file lives in the same package and therefore derives the same
// package-path name) would spuriously collide.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	old := registered
	registered = nil
	t.Cleanup(func() { registered = old })
}

// overriddenNamePlugin writes its own Name(), so it must win over whatever
// deriveName would have produced -- Go's method resolution never even calls
// Base's promoted Name() here.
type overriddenNamePlugin struct{ Base }

func (p *overriddenNamePlugin) Name() string { return "custom-name" }

// noBasePlugin doesn't embed Base at all. It's still a legal plugin: the
// Plugin interface only requires Name(), and it implements that itself.
type noBasePlugin struct{}

func (p *noBasePlugin) Name() string { return "standalone" }

// autoNamedPlugin embeds Base and never overrides Name(), so its name must
// come from deriveName. Its concrete type lives directly in this package
// (root package xbc), so the derived name is this package's own last path
// segment -- see TestDeriveNameFromPackagePath and
// TestDeriveNameStripsMajorVersionSuffix above for the "real nested package
// path" cases, which is why they use external fixtures instead of a type
// defined here.
type autoNamedPlugin struct{ Base }

// conflictPluginA and conflictPluginB both embed Base without overriding
// Name(), so both derive this package's name -- a deliberate, guaranteed
// collision used to test the conflict check itself.
type conflictPluginA struct{ Base }
type conflictPluginB struct{ Base }

// multiCapablePlugin implements MultiInstancer, so newEntry must record
// entry.multi = true for it.
type multiCapablePlugin struct{ Base }

func (p *multiCapablePlugin) MultiInstance() bool { return true }

func TestRegisterRespectsOverriddenName(t *testing.T) {
	app := New()
	app.Register(&overriddenNamePlugin{})
	require.Len(t, app.entries, 1)
	assert.Equal(t, "custom-name", app.entries[0].name,
		"插件自己实现的 Name() 必须遮蔽框架推导的名字")
	assert.Equal(t, sourceRegister, app.entries[0].src)
}

func TestRegisterAcceptsPluginWithoutBase(t *testing.T) {
	app := New()
	app.Register(&noBasePlugin{})
	require.Len(t, app.entries, 1)
	assert.Equal(t, "standalone", app.entries[0].name, "不嵌 Base 的插件照样合法")
}

func TestRegisterDerivesNameFromBaseWhenNotOverridden(t *testing.T) {
	app := New()
	app.Register(&autoNamedPlugin{})
	require.Len(t, app.entries, 1)
	assert.Equal(t, "xbc", app.entries[0].name,
		"嵌入 Base 且未覆盖 Name() 时，框架从包路径推导")
}

func TestPackageRegisterMarksSourceImport(t *testing.T) {
	withCleanRegistry(t)
	Register(&noBasePlugin{})
	require.Len(t, registered, 1)
	assert.Equal(t, sourceImport, registered[0].src, "包级 Register 必须标记为 sourceImport")
	assert.Equal(t, "standalone", registered[0].name)
}

func TestAppRegisterMarksSourceRegister(t *testing.T) {
	app := New()
	app.Register(&noBasePlugin{})
	require.Len(t, app.entries, 1)
	assert.Equal(t, sourceRegister, app.entries[0].src, "App.Register 必须标记为 sourceRegister")
}

func TestAppRegisterRejectsNameConflict(t *testing.T) {
	app := New()
	app.Register(&conflictPluginA{})
	assert.PanicsWithError(t,
		`xbc: 插件名 "xbc" 冲突：*xbc.conflictPluginA 与 *xbc.conflictPluginB 推导出同一个名字，请给其中一个显式实现 Name() 改名`,
		func() { app.Register(&conflictPluginB{}) },
		"两个插件推导出同名必须在 Register 时就报错",
	)
}

func TestPackageRegisterRejectsNameConflict(t *testing.T) {
	withCleanRegistry(t)
	Register(&conflictPluginA{})
	assert.PanicsWithError(t,
		`xbc: 插件名 "xbc" 冲突：*xbc.conflictPluginA 与 *xbc.conflictPluginB 推导出同一个名字，请给其中一个显式实现 Name() 改名`,
		func() { Register(&conflictPluginB{}) },
	)
}

func TestNewEntryDetectsMultiInstancer(t *testing.T) {
	app := New()
	app.Register(&multiCapablePlugin{})
	require.Len(t, app.entries, 1)
	assert.True(t, app.entries[0].multi,
		"实现 MultiInstancer 且返回 true 时 entry.multi 必须为 true")
}

func TestNewSeedsFromPackageLevelRegistrations(t *testing.T) {
	withCleanRegistry(t)
	Register(&noBasePlugin{})
	app := New()
	require.Len(t, app.entries, 1, "New() 必须把包级 Register 的插件带入 App")
	assert.Equal(t, sourceImport, app.entries[0].src)
}
```

- [ ] **Step 4: 跑测试确认失败**

```bash
go test . 2>&1 | head -20
```

Expected：编译失败。`register_test.go` 第一条报错是 `undefined: Base`（`plugin.go` 还不存在，`Base`/`sourceRegister`/`sourceImport`/`New`/`App` 全部未定义）；`plugin_test.go` 报 `undefined: deriveName`、`undefined: validateName`。两个文件加起来未定义符号超过编译器默认的报错上限，最后一行是 `too many errors`：

```
# github.com/xbcio/xbc [github.com/xbcio/xbc.test]
./register_test.go:28:35: undefined: Base
./register_test.go:45:30: undefined: Base
./register_test.go:50:30: undefined: Base
./register_test.go:51:30: undefined: Base
./register_test.go:55:33: undefined: Base
./plugin_test.go:22:15: undefined: deriveName
./plugin_test.go:28:15: undefined: deriveName
./plugin_test.go:45:12: undefined: deriveName
./plugin_test.go:51:12: undefined: deriveName
./plugin_test.go:58:19: undefined: validateName
./plugin_test.go:58:19: too many errors
FAIL	github.com/xbcio/xbc [build failed]
FAIL
```

- [ ] **Step 5: 写实现**

`plugin.go`、`xbc.go`、`context.go`、`middleware.go`、`router.go` 这五个文件互相引用（`plugin.go` 的接口族要用到 `Context`/`Middleware`/`Router`；`xbc.go` 要用到 `plugin.go` 的 `Base`/`deriveName`/`bindBase`/`validateName`），必须一起落地才能编译过，所以放在同一步。

创建 `plugin.go`：

```go
package xbc

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/xbcio/xbc/log"
)

// Plugin is the only interface a plugin must implement.
type Plugin interface {
	Name() string
}

// Dep, Ref and Deps are declared here -- ahead of deps.go (Task 2) -- purely
// because the Declarer and Provider interfaces immediately below reference
// them: an interface naming an undefined type fails to compile the moment
// this file lands, and plugin.go must build on its own by the end of this
// task. Task 2 adds the constructors (Need, NeedNamed, Opt, Offer, RefOf,
// Ref.Instance) and the String methods that operate on these same types --
// splitting "what shape is a dependency" from "how do you build one" across
// two commits without ever leaving the package non-building in between.
type Dep struct {
	Type     reflect.Type
	Instance string // "" means default
	Optional bool
}

type Ref struct {
	typ      reflect.Type
	instance string // "" means any instance
}

type Deps struct {
	Types   []Dep    // hard: I need an instance of this type
	Plugins []Ref    // hard: this plugin must be present
	After   []string // soft ordering preference
	Before  []string
}

// ── Optional capability interfaces: implement one, get its stage ──────────
type Configurable interface{ ConfigPtr() any }
type MultiInstancer interface{ MultiInstance() bool }
type Declarer interface{ Dependencies() Deps }
type Provider interface{ Provides() []Dep }
type Initializer interface{ Init(ctx *Context) error }
type Migrator interface{ Migrate(ctx *Context) error }
type MiddlewareProvider interface{ Middlewares() []Middleware }
type RouteProvider interface{ RegisterRoutes(r *Router) }
type PostRouter interface{ PostRoutes(ctx *Context) error }
type Runner interface{ Start(ctx *Context) error }
type Closer interface {
	Stop(ctx context.Context) error
}
type HealthChecker interface {
	Health(ctx context.Context) error
}

// Base is embedded by a plugin to get three things: convenience accessors
// (Ctx, Log), automatic Name() derivation, and the anchor bindBase uses to
// hand the plugin its Context. Embedding it is optional -- a plugin that
// writes Init(ctx) itself already has everything Base would give it, via the
// ctx parameter directly.
type Base struct {
	ctx  *Context
	name string
}

// Ctx returns the Context bound to this plugin. It is nil before stage 5
// (Init): Base itself gets a name bound earlier, at registration, purely to
// resolve Name(); the Context that carries the registry and config isn't
// built until Init.
func (b *Base) Ctx() *Context { return b.ctx }

// Log is a shortcut for Ctx().Log(). Safe to call before stage 5: it falls
// back to the global logger instead of dereferencing a nil Context.
func (b *Base) Log() log.Logger {
	if b.ctx == nil {
		return log.L()
	}
	return b.ctx.Log()
}

// Name returns the framework-derived (or later explicitly bound) name. A
// plugin that writes its own Name() method shadows this outright -- Go's
// method resolution never even reaches this one in that case.
func (b *Base) Name() string { return b.name }

// base is the unexported anchor bindBase uses to reach into an embedded Base
// without knowing the plugin's own concrete type. It stays unexported so it
// can only be satisfied by actually embedding Base, never by a plugin that
// happens to define its own base() method.
func (b *Base) base() *Base { return b }

// baseAnchor is implemented by *Base and, through Go's method promotion, by
// any plugin that embeds Base. bindBase type-asserts against it to find out
// whether there is a Base to wire up at all.
type baseAnchor interface{ base() *Base }

// deriveName reflects a plugin value's concrete package path and returns its
// last segment, skipping a trailing major-version element ("/v2").
//
// It requires p to be a pointer to a named struct type: a value receiver
// can't have its Base bound (bindBase needs a pointer to mutate), and an
// anonymous type has no meaningful package-path segment to take the name
// from. Both are rejected with an error that tells the plugin author to
// just implement Name() themselves.
func deriveName(p Plugin) (string, error) {
	t := reflect.TypeOf(p)
	if t == nil {
		return "", fmt.Errorf("xbc: 无法从 nil 值推导插件名")
	}
	if t.Kind() != reflect.Pointer {
		return "", fmt.Errorf("xbc: 插件类型 %s 不是指针，无法自动推导名字，请自行实现 Name()", t)
	}
	elem := t.Elem()
	if elem.Kind() != reflect.Struct {
		return "", fmt.Errorf("xbc: 插件类型 %s 指向的不是结构体，无法自动推导名字，请自行实现 Name()", t)
	}
	if elem.Name() == "" || elem.PkgPath() == "" {
		return "", fmt.Errorf("xbc: 插件类型 %s 是匿名类型，无法自动推导名字，请自行实现 Name()", t)
	}

	segs := strings.Split(elem.PkgPath(), "/")
	last := segs[len(segs)-1]
	if len(segs) >= 2 && isMajorVersionSegment(last) {
		last = segs[len(segs)-2]
	}
	return last, nil
}

// isMajorVersionSegment reports whether s looks like a Go major-version path
// element ("v2", "v10", ...). deriveName skips it and falls back to the
// preceding path segment, so github.com/x/foo/v2 still derives "foo".
func isMajorVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// bindBase wires ctx and name into an embedded Base. Returns false when the
// plugin does not embed Base -- legal, just means no convenience accessors.
func bindBase(p Plugin, ctx *Context, name string) bool {
	a, ok := p.(baseAnchor)
	if !ok {
		return false
	}
	b := a.base()
	b.ctx = ctx
	b.name = name
	return true
}

// validateName reports whether s contains a character the framework
// reserves, returning an error describing the first one found.
//
// Reserved: '.' (middleware qualification), '[' ']' (instance display),
// whitespace, and any character outside [a-z0-9_-]. This intentionally does
// NOT lowercase s first: an uppercase letter is rejected outright rather than
// silently folded, so "Gorm" and "gorm" can never collide by accident.
func validateName(s string) error {
	if s == "" {
		return fmt.Errorf("xbc: 插件名不能为空")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return fmt.Errorf("xbc: 插件名 %q 含非法字符 %q，只允许小写字母、数字、下划线、连字符", s, r)
		}
	}
	return nil
}
```

创建 `xbc.go`：

```go
package xbc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
)

// source records which registration path a plugin arrived by. The two paths
// carry different intent strength, so they get different enable rules (§6.4,
// implemented in Task 8's stage_expand.go).
type source int

const (
	sourceImport   source = iota // blank import + init(): capability is available
	sourceRegister               // app.Register(): an explicit statement of intent
)

// defaultInstance is the instance name used when a plugin doesn't ask for a
// specific one.
const defaultInstance = "default"

// entry is one registered plugin prototype, before expansion.
type entry struct {
	proto Plugin
	name  string
	src   source
	multi bool
}

var (
	registerMu sync.Mutex
	registered []entry
)

// Register registers a plugin from a package init(). Enabled only when a
// matching config section exists (Task 8).
//
// Register cannot return an error -- it exists to be called from init(),
// which has no error channel -- so a malformed plugin (an unresolvable name,
// or a name collision with an earlier registration) panics instead. This
// mirrors database/sql.Register: a mistake here explodes at program startup,
// inside the offending package's own init(), not silently later during a
// request.
func Register(p Plugin) {
	registerMu.Lock()
	defer registerMu.Unlock()

	e, err := newEntry(p, sourceImport)
	if err != nil {
		panic(err)
	}
	for _, existing := range registered {
		if existing.name == e.name {
			panic(fmt.Errorf(
				"xbc: 插件名 %q 冲突：%T 与 %T 推导出同一个名字，请给其中一个显式实现 Name() 改名",
				e.name, existing.proto, e.proto))
		}
	}
	registered = append(registered, e)
}

// App is the assembled application. Every exported method returns *App (or
// nothing), so main() reads top to bottom: New().Register(...).Run().
//
// Most of these fields are written by a later assembly stage, not here. They
// are declared up front so every stage agrees on one set of names -- a struct
// field costs nothing until something writes it, and go vet does not flag a
// field nobody reads yet.
type App struct {
	mu      sync.Mutex
	entries []entry

	// Stage 1-4 results.
	order   []*instance // topological order, written by resolve (stage 4)
	migrate bool        // whether stage 6 runs, decided by cli.go

	// HTTP, assembled by stage 7 and driven by stages 8-10.
	router      *Router
	httpServer  *http.Server
	listener    net.Listener
	ready       chan struct{}   // closed once the listener accepts
	routeCounts map[string]int  // plugin name -> routes it registered

	// Managed goroutine lifecycle, see goroutine.go (Task 12).
	runCtx         context.Context // handed to every ctx.Go / ctx.GoCritical callback
	cancel         context.CancelFunc
	wg             *sync.WaitGroup
	criticalCh     chan struct{} // closed once by triggerCritical
	criticalOnce   sync.Once
	criticalReason string

	exitCode int
}

// instance is one expanded plugin instance -- the unit every stage after
// expansion operates on. Expansion fills plugin/name/instance/src/ctx;
// resolve fills fields/deps/provides; initAll flips inited.
type instance struct {
	plugin   Plugin
	name     string // plugin name, e.g. "gorm"
	instance string // instance name, e.g. "default" / "readonly"
	src      source
	ctx      *Context
	deps     Deps
	provides []Dep
	inited   bool
}

// New creates an App seeded with every plugin registered via the
// package-level Register.
func New() *App {
	registerMu.Lock()
	seed := make([]entry, len(registered))
	copy(seed, registered)
	registerMu.Unlock()
	return &App{entries: seed}
}

// Register registers one or more plugins explicitly. Unlike the
// package-level Register (a capability announcement), this is a statement of
// intent: the plugin is enabled by default even without a matching config
// section (§6.4).
//
// Like the package-level Register, this has no error return: app.Register
// calls read as a flat list in main(), and a name collision here is exactly
// as much a programming error as a duplicate package-level Register, so it
// panics for the same reason.
func (a *App) Register(p ...Plugin) *App {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, one := range p {
		e, err := newEntry(one, sourceRegister)
		if err != nil {
			panic(err)
		}
		for _, existing := range a.entries {
			if existing.name == e.name {
				panic(fmt.Errorf(
					"xbc: 插件名 %q 冲突：%T 与 %T 推导出同一个名字，请给其中一个显式实现 Name() 改名",
					e.name, existing.proto, e.proto))
			}
		}
		a.entries = append(a.entries, e)
	}
	return a
}

// Run assembles and serves the application, exiting the process with the
// resulting code. The ten-stage pipeline (Task 8 onward) fills this in; it
// is intentionally left minimal here since none of those stages exist yet.
func (a *App) Run() {}

// newEntry resolves a plugin's name and wraps it into an entry.
//
// Name resolution order: a plugin's own Name() wins whenever it returns a
// non-empty value. That covers both a hand-written Name() (method shadowing
// beats Base's promoted method outright) and a Base-embedding plugin that
// was already bound by an earlier call. Only when Name() comes back empty --
// meaning Base is embedded but has never been bound -- does newEntry fall
// back to deriveName and bind it, so Base.Name() (and any later p.Name()
// call) reflects it from then on. A plugin that neither overrides Name() nor
// embeds Base can't legally reach the empty-string branch at all: the Plugin
// interface requires Name(), so such a plugin must already return something
// non-empty on its own.
func newEntry(p Plugin, src source) (entry, error) {
	name := p.Name()
	if name == "" {
		derived, err := deriveName(p)
		if err != nil {
			return entry{}, fmt.Errorf("xbc: 插件 %T 未实现 Name()，且无法自动推导名字：%w", p, err)
		}
		if !bindBase(p, nil, derived) {
			return entry{}, fmt.Errorf(
				"xbc: 插件 %T 的 Name() 返回空字符串，且未嵌入 xbc.Base，无法确定插件名", p)
		}
		name = p.Name()
	}
	if err := validateName(name); err != nil {
		return entry{}, err
	}

	e := entry{proto: p, name: name, src: src}
	if mi, ok := p.(MultiInstancer); ok {
		e.multi = mi.MultiInstance()
	}
	return e, nil
}
```

**`App` 和 `instance` 上还差五个字段，它们的类型现在还不存在（裁决 R12）。** 本 task 不要试图补上——`*Config`、`*registry`、`graph.Miss`、`mwEntry`、`inject.FieldSpec` 分别定义在 Task 6、4、3、11、7，现在写进去只会让 `xbc.go` import 一批不存在的包，整个 task 编译不过。它们由各自的 task 用 `Edit` 追加到这两个结构体里：

| 字段 | 追加进 | 由哪个 task 追加 |
|---|---|---|
| `cfg *Config` | `App` | Task 6 |
| `registry *registry` | `App` | Task 4 |
| `softMisses []graph.Miss` | `App` | Task 14 |
| `middlewareChain []mwEntry` | `App` | Task 14 |
| `fields []inject.FieldSpec` | `instance` | Task 10 |

反过来说，**上面已经写出来的字段，后面的 task 一个都不要重复声明**，也不要改名。Task 12 的 `goroutine.go` 会用到 `cancel` / `wg` / `criticalCh` / `criticalOnce` / `criticalReason`，Task 14 的 `stage_run.go` 会用到 `router` / `httpServer` / `listener` / `ready` / `routeCounts` / `exitCode`，Task 15 的 `cli.go` 会写 `migrate`——它们读到的就是这里定的这一套名字。

创建 `context.go`：

```go
package xbc

import "github.com/xbcio/xbc/log"

// Context is the per-instance handle a plugin receives from Init onward. It
// carries just enough identity (name/instance) to pre-bind the logger; the
// registry accessor, config accessor and managed-goroutine methods land in
// later tasks (Task 4, Task 6, Task 12 respectively).
type Context struct {
	app      *App
	name     string // plugin name
	instance string // instance name, always non-empty ("default" by default)
	logger   log.Logger
}

// Log returns a Logger pre-bound with this instance's identity. Task 4 wires
// up the actual field-binding (plugin=/instance=); for now it just hands back
// whatever logger the instance carries, falling back to the global logger so
// a Context built without one (e.g. in a unit test) never panics.
func (c *Context) Log() log.Logger {
	if c.logger == nil {
		return log.L()
	}
	return c.logger
}

// Name returns the plugin name this Context belongs to.
func (c *Context) Name() string { return c.name }

// Instance returns the instance name ("default" unless the plugin is
// multi-instance and configured otherwise).
func (c *Context) Instance() string { return c.instance }
```

创建 `middleware.go`：

```go
package xbc

import "github.com/gin-gonic/gin"

// Phase is a coarse, ordered anchor for middleware placement. It is a hard
// boundary: middleware in an earlier Phase always runs outside (before) all
// middleware in a later Phase, regardless of any After/Before preference.
type Phase int

// The five phases are spaced 100 apart, not a plain iota run, so a plugin
// that genuinely needs to sit outside PhaseRecover can write
// xbc.PhaseRecover-1 without colliding with a future inserted phase. This is
// documented as an escape hatch, not a named constant, deliberately: a
// middleware placed there runs outside panic recovery, and naming it would
// make it look like a normal, supported option.
const (
	PhaseRecover  Phase = iota * 100 // 0,   outermost: panic backstop
	PhaseObserve                     // 100, tracing, access log
	PhaseSecurity                    // 200, cors / ratelimit / replay defense
	PhaseAuth                        // 300, authentication and authorization
	PhaseBusiness                    // 400, business middleware
)

// Middleware is one entry a plugin contributes to the HTTP middleware chain.
// Task 11 (mwchain.go) adds the sorter that consumes this; this task only
// needs the shape to exist so MiddlewareProvider compiles.
type Middleware struct {
	Name    string // unique identifier other middleware can reference in After/Before
	Phase   Phase
	After   []string // soft ordering preference within the same Phase
	Before  []string
	Handler gin.HandlerFunc
}
```

创建 `router.go`：

```go
package xbc

import "github.com/gin-gonic/gin"

// RouteInfo is one entry in the frozen route table. Plan 2 freezes method and
// path only; Plan 3 adds the metadata fields (Public, Name, Doc).
type RouteInfo struct {
	Method string
	Path   string
}

// Router is the chainable route builder plugins receive in RegisterRoutes.
// Task 14 (stage_run.go) adds the Group/Handle/GET/... methods and the
// freeze-after-stage-7 behavior; this task only needs the shape to exist so
// RouteProvider compiles.
type Router struct {
	engine   *gin.Engine
	group    *gin.RouterGroup
	basePath string
	routes   *[]RouteInfo
	frozen   *bool
}
```

- [ ] **Step 6: 跑测试确认通过**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -v
```

Expected：`plugin_test.go` 与 `register_test.go` 里全部 16 个测试 PASS，`go vet`/`gofmt -l` 无输出。

- [ ] **Step 7: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
git add go.mod go.sum plugin.go xbc.go context.go middleware.go router.go plugin_test.go register_test.go internal/testplugins/jwt/jwt.go internal/testplugins/foo/v2/foo.go
git commit -m "feat(xbc): 插件接口族、Base 与名字推导、两条注册路径"
```

---

### Task 2: `deps.go` —— 依赖与产物的声明类型

**Files:**
- Create: `deps.go`
- Test: `deps_test.go`

**Interfaces:**
- Consumes：Task 1 的 `Plugin`、`Dep`、`Ref`、`Deps`、`defaultInstance`
- Produces：
  - `func typeOf[T any]() reflect.Type`
  - `func normInstance(s string) string`
  - `func Need[T any]() Dep`
  - `func NeedNamed[T any](instance string) Dep`
  - `func Opt[T any]() Dep`
  - `func Offer[T any]() Dep`
  - `func RefOf[T Plugin]() Ref`
  - `func (r Ref) Instance(name string) Ref`
  - `func (d Dep) String() string`
  - `func (r Ref) String() string`

`Dep`/`Ref`/`Deps` 的结构体形状已经在 Task 1 的 `plugin.go` 里落地（`Declarer`/`Provider` 接口需要它们才能编译），本 task 只补构造函数和 `String()`——"依赖长什么样"与"怎么造一个依赖"分两次提交，中间任何一次提交后包都能正常编译。

- [ ] **Step 1: 写失败测试**

创建 `deps_test.go`：

```go
package xbc

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// depsFixturePlugin exists purely so RefOf has a concrete Plugin type to
// capture; its behavior is never exercised.
type depsFixturePlugin struct{ Base }

func (p *depsFixturePlugin) Name() string { return "deps-fixture" }

// TestTypeOfDiffersFromNaiveReflectTypeOf locks in exactly why typeOf exists:
// calling reflect.TypeOf directly on an interface-typed value gives back the
// concrete type stored inside it, not the interface type itself.
func TestTypeOfDiffersFromNaiveReflectTypeOf(t *testing.T) {
	var r io.Reader = bytes.NewBufferString("x")
	naive := reflect.TypeOf(r)
	assert.NotEqual(t, reflect.Interface, naive.Kind(),
		"对接口值直接调用 reflect.TypeOf 拿到的是背后的具体类型，不是接口类型本身")
	assert.Equal(t, reflect.Interface, typeOf[io.Reader]().Kind(),
		"typeOf 绕开这个陷阱，稳定拿到接口类型")
}

func TestTypeOfCapturesInterfaceType(t *testing.T) {
	got := typeOf[io.Reader]()
	assert.Equal(t, reflect.Interface, got.Kind(), "io.Reader 是接口类型")
	assert.Equal(t, "io.Reader", got.String())
}

func TestTypeOfCapturesAnotherInterfaceType(t *testing.T) {
	got := typeOf[fmt.Stringer]()
	assert.Equal(t, reflect.Interface, got.Kind(), "fmt.Stringer 是接口类型")
	assert.Equal(t, "fmt.Stringer", got.String())
}

func TestTypeOfCapturesConcretePointerType(t *testing.T) {
	got := typeOf[*bytes.Buffer]()
	assert.Equal(t, reflect.Pointer, got.Kind(), "*bytes.Buffer 是具体类型")
	assert.Equal(t, "*bytes.Buffer", got.String())
}

func TestNormInstanceMapsEmptyToDefault(t *testing.T) {
	assert.Equal(t, "default", normInstance(""), `空串归一为 "default"`)
}

func TestNormInstanceLeavesDefaultUnchanged(t *testing.T) {
	assert.Equal(t, "default", normInstance("default"))
}

func TestNormInstanceLeavesNamedInstanceUnchanged(t *testing.T) {
	assert.Equal(t, "readonly", normInstance("readonly"))
}

func TestNeedProducesDefaultInstanceHardDep(t *testing.T) {
	d := Need[io.Reader]()
	assert.Equal(t, typeOf[io.Reader](), d.Type)
	assert.Equal(t, "", d.Instance)
	assert.False(t, d.Optional, "Need 产生的依赖不是可选的")
}

func TestNeedNamedSetsInstance(t *testing.T) {
	d := NeedNamed[io.Reader]("readonly")
	assert.Equal(t, typeOf[io.Reader](), d.Type)
	assert.Equal(t, "readonly", d.Instance)
	assert.False(t, d.Optional)
}

func TestOptSetsOptionalTrue(t *testing.T) {
	d := Opt[io.Reader]()
	assert.Equal(t, "", d.Instance)
	assert.True(t, d.Optional, "Opt 产生的依赖必须是可选的")
}

func TestOfferProducesNonOptionalDep(t *testing.T) {
	d := Offer[fmt.Stringer]()
	assert.Equal(t, typeOf[fmt.Stringer](), d.Type)
	assert.False(t, d.Optional, "Offer 描述的是产出，不存在可选一说")
}

func TestDepStringWithoutInstance(t *testing.T) {
	d := Need[*bytes.Buffer]()
	assert.Equal(t, "*bytes.Buffer", d.String())
}

func TestDepStringWithNamedInstance(t *testing.T) {
	d := NeedNamed[*bytes.Buffer]("readonly")
	assert.Equal(t, "*bytes.Buffer[readonly]", d.String())
}

func TestDepStringTreatsExplicitDefaultAsUnqualified(t *testing.T) {
	d := NeedNamed[*bytes.Buffer]("default")
	assert.Equal(t, "*bytes.Buffer", d.String(),
		`显式写 "default" 与不写等价，字符串表示不应带方括号`)
}

func TestRefOfCapturesConcretePluginType(t *testing.T) {
	r := RefOf[*depsFixturePlugin]()
	assert.Equal(t, typeOf[*depsFixturePlugin](), r.typ)
	assert.Equal(t, "", r.instance, `未调用 Instance 时默认为 "任意实例"`)
}

func TestRefInstanceSetsInstance(t *testing.T) {
	r := RefOf[*depsFixturePlugin]().Instance("readonly")
	assert.Equal(t, "readonly", r.instance)
}

func TestRefStringWithoutInstance(t *testing.T) {
	r := RefOf[*depsFixturePlugin]()
	assert.Equal(t, "*xbc.depsFixturePlugin", r.String())
}

func TestRefStringWithInstance(t *testing.T) {
	r := RefOf[*depsFixturePlugin]().Instance("readonly")
	assert.Equal(t, "*xbc.depsFixturePlugin[readonly]", r.String())
}
```

`RefOf` 的类型约束是 `[T Plugin]` 而不是 `[T any]`，这条**不写测试**：它是编译期拒绝，运行时没有任何行为可断言，写出来的只能是一个调 `t.Log` 的空壳，永远通过，还在覆盖率里冒充一个"测过了"的信号。理由写进 `RefOf` 自己的文档注释（见 Step 3），读者在用它的时候就能看见，比藏在测试文件里更管用。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test . 2>&1 | head -20
```

Expected：编译失败，第一批报错是 `undefined: typeOf`（`TestTypeOfDiffersFromNaiveReflectTypeOf` 用到），随后是 `undefined: normInstance`、`undefined: Need`：

```
# github.com/xbcio/xbc [github.com/xbcio/xbc.test]
./deps_test.go:27:37: undefined: typeOf
./deps_test.go:32:9: undefined: typeOf
./deps_test.go:38:9: undefined: typeOf
./deps_test.go:44:9: undefined: typeOf
./deps_test.go:50:29: undefined: normInstance
./deps_test.go:54:29: undefined: normInstance
./deps_test.go:58:30: undefined: normInstance
./deps_test.go:62:7: undefined: Need
./deps_test.go:63:18: undefined: typeOf
./deps_test.go:69:7: undefined: NeedNamed
./deps_test.go:69:7: too many errors
FAIL	github.com/xbcio/xbc [build failed]
FAIL
```

- [ ] **Step 3: 写实现**

创建 `deps.go`：

```go
package xbc

import "reflect"

// typeOf returns the reflect.Type for T itself, never for *T. Going through
// a *T pointer side-steps a specific trap: reflect.TypeOf(v) applied to a
// value of interface type T returns the *concrete* type stored inside the
// interface (or nil, for the zero value) -- never the interface type T
// itself. A *T value is never nil regardless of what T is, so TypeOf never
// has to guess, and .Elem() unwraps the pointer to hand back exactly T's own
// reflect.Type, whether T is an interface (io.Reader) or a concrete type
// (*bytes.Buffer).
func typeOf[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

// normInstance normalizes an instance name for comparison and display: ""
// and "default" mean the same thing everywhere in xbc (the zero-value
// instance), so a caller that holds either spelling can normalize once
// instead of special-casing both at every comparison site.
func normInstance(s string) string {
	if s == "" {
		return defaultInstance
	}
	return s
}

// Need declares a hard dependency on the default instance of T. Resolution
// (Task 9) fails the whole app if no plugin provides one.
func Need[T any]() Dep {
	return Dep{Type: typeOf[T]()}
}

// NeedNamed declares a hard dependency on a specific, non-default instance
// of T, e.g. NeedNamed[*gorm.DB]("readonly").
func NeedNamed[T any](instance string) Dep {
	return Dep{Type: typeOf[T](), Instance: instance}
}

// Opt declares a soft dependency: if the default instance of T exists it is
// wired up, but its absence is not an error.
func Opt[T any]() Dep {
	return Dep{Type: typeOf[T](), Optional: true}
}

// Offer declares that a plugin's Provider.Provides() produces an instance of
// T. It is spelled distinctly from Need -- even though the two build an
// identical Dep{Type: typeOf[T]()} value -- because the two lists that carry
// them (Deps.Types vs Provider's return value) are read in opposite
// directions: one says "resolve me one of these", the other says "I am the
// one you can resolve to". Sharing the constructor name would make code that
// mixes them up (declaring a need in Provides, say) read as correct when it
// isn't.
func Offer[T any]() Dep {
	return Dep{Type: typeOf[T]()}
}

// RefOf declares a hard dependency on another plugin, by identity rather
// than by any type it provides.
//
// T is constrained to Plugin, not any, because a Ref names one specific
// plugin by its own concrete type -- unlike Dep, which is routinely an
// interface (Need[io.Reader]() correctly means "whatever provides an
// io.Reader"), there is no such thing as "any plugin that happens to
// implement io.Reader" for a Ref to mean. RefOf[T any]() would let
// RefOf[string]() compile and produce a Ref that can never correspond to a
// real registration; RefOf[T Plugin]() turns that into a compile error
// instead, at no cost to any real call site, which always passes a concrete
// *SomePlugin type anyway.
func RefOf[T Plugin]() Ref {
	return Ref{typ: typeOf[T]()}
}

// Instance narrows a Ref to one specific instance of the referenced plugin.
// The zero value ("") means any instance of that plugin will do.
func (r Ref) Instance(name string) Ref {
	r.instance = name
	return r
}

// String renders a Dep for error copy, e.g. "*gorm.DB" or
// "*gorm.DB[readonly]". The bracket is omitted for the default instance --
// normInstance folds both "" and the explicit string "default" to the same
// unqualified rendering, so a caller who wrote NeedNamed[T]("default")
// instead of Need[T]() doesn't get a redundant "[default]" in every error
// message.
func (d Dep) String() string {
	s := d.Type.String()
	if inst := normInstance(d.Instance); inst != defaultInstance {
		s += "[" + inst + "]"
	}
	return s
}

// String renders a Ref the same way Dep does, e.g. "*jwt.Plugin[readonly]".
//
// Unlike Dep.String, this does NOT run r.instance through normInstance: a
// Ref's "" means "any instance is acceptable", which is a different thing
// from Dep's "" ("the default instance specifically") and must not be
// displayed as if the caller had asked for "default" by name.
func (r Ref) String() string {
	s := r.typ.String()
	if r.instance != "" {
		s += "[" + r.instance + "]"
	}
	return s
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
gofmt -l . && go vet ./... && go test . -run 'TestTypeOf|TestNormInstance|TestNeed|TestOpt|TestOffer|TestDepString|TestRef' -count=1 -v
```

Expected：`deps_test.go` 里全部 18 个测试 PASS，`go vet`/`gofmt -l` 无输出。

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
git add deps.go deps_test.go
git commit -m "feat(xbc): 依赖与产出的声明类型（Need/NeedNamed/Opt/Offer/RefOf）"
```

---

### Task 3: `internal/graph` —— 通用稳定拓扑排序器

**Files:**
- Create: `internal/graph/graph.go`
- Test: `internal/graph/graph_test.go`

**Interfaces:**
- Consumes：无（本包不认识任何 xbc 类型，是独立可测的纯逻辑包）
- Produces：
  - `type Graph struct{ ... }`（内部字段不导出）
  - `func New() *Graph`
  - `func (g *Graph) AddNode(id string)`
  - `func (g *Graph) AddEdge(from, to string, hard bool)`
  - `func (g *Graph) Sort() (order []string, misses []Miss, err error)`
  - `type Miss struct{ Node, Ref, Dir string }`
  - `type CycleError struct{ Path []string }` + `func (e *CycleError) Error() string`
  - `type MissingNodeError struct{ From, To string }` + `func (e *MissingNodeError) Error() string`

这个包故意不 import、不提及任何 xbc 类型：节点和边只是纯字符串。同一个排序器要在 xbc 里被复用至少三处——插件初始化顺序（Task 9 的 resolve 阶段）、同一 Phase 内的中间件排序（Task 11 的 `orderMiddlewares`）、优雅关闭时的 `Stop()` 顺序（就是初始化顺序的逆序）——这三处对"什么是一个节点"完全没有共识（插件实例 id、限定后的中间件名……）。让这个包认识 `Dep`/`Ref`/`Plugin` 只会把它的正确性绑死在 xbc 自身的演进上，却什么都换不来：每个调用方反正都要先把自己的领域对象转成字符串 id 才能用 `Graph`，字符串这道边界不花一分钱，换来的是一个永远能独立编译、独立测试的包。

- [ ] **Step 1: 写失败测试**

创建 `internal/graph/graph_test.go`：

```go
package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSortOnEmptyGraph(t *testing.T) {
	g := New()
	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Empty(t, order)
	assert.Empty(t, misses)
}

func TestSortLinearChain(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("c")
	g.AddEdge("a", "b", true)
	g.AddEdge("b", "c", true)

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"a", "b", "c"}, order)
}

// TestSortSameLayerStability repeats the same unrelated-nodes graph 100
// times to lock in that Sort's insertion-order tiebreak is not an accident
// of map iteration -- three nodes with no edges between them at all must
// come back in AddNode order, every single time.
func TestSortSameLayerStability(t *testing.T) {
	for i := 0; i < 100; i++ {
		g := New()
		g.AddNode("z")
		g.AddNode("m")
		g.AddNode("a")

		order, misses, err := g.Sort()
		require.NoError(t, err)
		assert.Empty(t, misses)
		assert.Equal(t, []string{"z", "m", "a"}, order,
			"三个互不相关的节点必须严格按插入顺序排出，第 %d 次", i)
	}
}

// TestSortDiamondDependency covers a > b, a > c, b > d, c > d: b and c
// become eligible at the same time once a is emitted, and must come out in
// their own insertion order (b before c) before d, which needs both.
func TestSortDiamondDependency(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("c")
	g.AddNode("d")
	g.AddEdge("a", "b", true)
	g.AddEdge("a", "c", true)
	g.AddEdge("b", "d", true)
	g.AddEdge("c", "d", true)

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"a", "b", "c", "d"}, order)
}

func TestSortDetectsSelfLoop(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddEdge("a", "a", true)

	_, _, err := g.Sort()
	require.Error(t, err)
	var cycleErr *CycleError
	require.ErrorAs(t, err, &cycleErr)
	assert.Equal(t, []string{"a", "a"}, cycleErr.Path)
}

func TestSortDetectsTwoNodeMutualCycle(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddEdge("a", "b", true)
	g.AddEdge("b", "a", true)

	_, _, err := g.Sort()
	require.Error(t, err)
	var cycleErr *CycleError
	require.ErrorAs(t, err, &cycleErr)
	assert.Equal(t, []string{"a", "b", "a"}, cycleErr.Path)
}

// TestSortDetectsThreeNodeCycleWithExactPath locks the exact path and exact
// rendered error text, matching the shape the kernel spec's own error copy
// uses for a plugin dependency cycle (user -> order -> payment -> user).
func TestSortDetectsThreeNodeCycleWithExactPath(t *testing.T) {
	g := New()
	g.AddNode("user")
	g.AddNode("order")
	g.AddNode("payment")
	g.AddEdge("user", "order", true)
	g.AddEdge("order", "payment", true)
	g.AddEdge("payment", "user", true)

	order, misses, err := g.Sort()
	require.Error(t, err)
	assert.Nil(t, order)
	assert.Nil(t, misses)

	var cycleErr *CycleError
	require.ErrorAs(t, err, &cycleErr)
	assert.Equal(t, []string{"user", "order", "payment", "user"}, cycleErr.Path)
	assert.Equal(t, "graph: 存在环 user → order → payment → user", cycleErr.Error())
}

func TestSortReportsSoftMissWithoutError(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddEdge("a", "ghost", false)

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, order)
	require.Len(t, misses, 1)
	assert.Equal(t, Miss{Node: "a", Ref: "ghost", Dir: "before"}, misses[0])
}

func TestSortReportsSoftMissFromMissingFromSide(t *testing.T) {
	g := New()
	g.AddNode("b")
	g.AddEdge("ghost", "b", false)

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, order)
	require.Len(t, misses, 1)
	assert.Equal(t, Miss{Node: "b", Ref: "ghost", Dir: "after"}, misses[0])
}

func TestSortReturnsMissingNodeErrorForHardEdge(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddEdge("a", "ghost", true)

	order, misses, err := g.Sort()
	assert.Nil(t, order)
	assert.Nil(t, misses)
	require.Error(t, err)

	var missingErr *MissingNodeError
	require.ErrorAs(t, err, &missingErr)
	assert.Equal(t, "a", missingErr.From)
	assert.Equal(t, "ghost", missingErr.To)
	assert.Equal(t, "graph: 硬依赖引用了不存在的节点：a → ghost", missingErr.Error())
}

func TestAddNodeIsIdempotent(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("a") // must not move a's insertion index

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"a", "b"}, order,
		"重复 AddNode 不能改变节点的插入序")
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/graph/... 2>&1 | head -20
```

Expected：编译失败，`internal/graph/graph.go` 还不存在，报 `undefined: New`、`undefined: CycleError`：

```
# github.com/xbcio/xbc/internal/graph [github.com/xbcio/xbc/internal/graph.test]
internal/graph/graph_test.go:11:7: undefined: New
internal/graph/graph_test.go:19:7: undefined: New
internal/graph/graph_test.go:38:8: undefined: New
internal/graph/graph_test.go:55:7: undefined: New
internal/graph/graph_test.go:72:7: undefined: New
internal/graph/graph_test.go:78:16: undefined: CycleError
internal/graph/graph_test.go:84:7: undefined: New
internal/graph/graph_test.go:92:16: undefined: CycleError
internal/graph/graph_test.go:101:7: undefined: New
internal/graph/graph_test.go:114:16: undefined: CycleError
internal/graph/graph_test.go:114:16: too many errors
FAIL	github.com/xbcio/xbc/internal/graph [build failed]
FAIL
```

- [ ] **Step 3: 写实现**

创建 `internal/graph/graph.go`：

```go
// Package graph implements a generic, stable topological sorter.
//
// This package deliberately does not import, mention, or know about any xbc
// type: nodes and edges are plain strings, nothing more. The same sorter is
// reused for at least three unrelated orderings inside xbc -- plugin
// initialization (Task 9's resolve stage), middleware placement within a
// Phase (Task 11's orderMiddlewares), and Stop() ordering during graceful
// shutdown (the reverse of the same order) -- and none of those callers
// agree on what a "node" even is (a plugin instance id, a qualified
// middleware name, ...). Teaching this package about Dep, Ref, or Plugin
// would tie its correctness to xbc's own evolution for zero benefit: every
// caller already has to turn its own domain objects into id strings before
// it can build a Graph anyway, so the string boundary costs nothing and
// buys a package that compiles and tests on its own, forever.
package graph

import (
	"container/heap"
	"fmt"
	"strings"
)

// edge is one declared ordering constraint, kept exactly as AddEdge received
// it. Validation (does each endpoint exist?) is deferred to Sort, not done
// at AddEdge time, so callers are free to add all their edges before all
// their nodes if that's more convenient -- Sort is the only place that needs
// a complete picture.
type edge struct {
	from, to string
	hard     bool
}

// Graph is a stable topological sorter. Node insertion order is the tiebreak
// among nodes that no edge separates, so the same input always sorts the
// same, run after run.
type Graph struct {
	order []string // node ids, in the order AddNode first saw them
	seen  map[string]bool
	index map[string]int // id -> position in order, for the stable tiebreak
	edges []edge
}

// New creates an empty Graph.
func New() *Graph {
	return &Graph{
		seen:  make(map[string]bool),
		index: make(map[string]int),
	}
}

// AddNode registers a node. It is idempotent: the first call fixes the
// node's insertion index (and therefore its tiebreak priority); later calls
// with the same id are no-ops.
func (g *Graph) AddNode(id string) {
	if g.seen[id] {
		return
	}
	g.seen[id] = true
	g.index[id] = len(g.order)
	g.order = append(g.order, id)
}

// AddEdge declares that from must be ordered before to.
//
// hard=true: both endpoints must exist by the time Sort runs; a missing one
// produces a *MissingNodeError and aborts the sort entirely -- a hard edge
// is a promise about the shape of the graph, and a broken promise is a bug
// in the caller, not something to route around.
//
// hard=false: a missing endpoint is not an error. The edge is silently
// dropped from the ordering and reported back as a Miss, because a soft
// edge is only ever a preference ("if this other thing exists, go near it"),
// and half of that preference not existing is completely unremarkable --
// think an After/Before naming a middleware that was never registered.
func (g *Graph) AddEdge(from, to string, hard bool) {
	g.edges = append(g.edges, edge{from: from, to: to, hard: hard})
}

// Miss is a soft edge whose other endpoint was never registered as a node.
type Miss struct {
	Node string // the node that declared the constraint
	Ref  string // the name it referenced, which does not exist
	Dir  string // "after" or "before"
}

// CycleError reports a dependency cycle with the full path, the starting
// node repeated at the end: {"user", "order", "payment", "user"}.
type CycleError struct{ Path []string }

func (e *CycleError) Error() string {
	return "graph: 存在环 " + strings.Join(e.Path, " → ")
}

// MissingNodeError reports a hard edge pointing at a node that does not
// exist. Both From and To are the edge's own endpoints as declared, even
// though only one of them is necessarily the missing one -- the caller
// already has both values on hand and can tell at a glance which side it
// forgot to AddNode.
type MissingNodeError struct{ From, To string }

func (e *MissingNodeError) Error() string {
	return fmt.Sprintf("graph: 硬依赖引用了不存在的节点：%s → %s", e.From, e.To)
}

// Sort returns nodes in dependency order together with every soft edge that
// referenced a node that does not exist.
//
// Algorithm: Kahn's algorithm (repeatedly emit a node with no remaining
// incoming edges, then decrement its successors' counts), with one twist --
// whenever more than one node is eligible to be emitted at the same time,
// the one that was AddNode'd earliest wins. That tiebreak is implemented as
// a container/heap min-heap keyed by insertion index rather than a plain
// FIFO queue, because eligibility doesn't arrive in insertion order: a node
// inserted first can easily become eligible after one inserted later (it
// might have more incoming edges to clear first), so the pool of
// "currently eligible" nodes needs to be kept sorted by index at all times,
// not just enqueued in the order they become eligible.
func (g *Graph) Sort() ([]string, []Miss, error) {
	adj := make(map[string][]string, len(g.order))
	indeg := make(map[string]int, len(g.order))
	for _, id := range g.order {
		indeg[id] = 0
	}

	var misses []Miss

	for _, e := range g.edges {
		fromOK := g.seen[e.from]
		toOK := g.seen[e.to]

		if fromOK && toOK {
			adj[e.from] = append(adj[e.from], e.to)
			indeg[e.to]++
			continue
		}

		if e.hard {
			return nil, nil, &MissingNodeError{From: e.from, To: e.to}
		}

		if m, ok := missFor(e, fromOK, toOK); ok {
			misses = append(misses, m)
		}
	}

	h := &idHeap{index: g.index}
	for _, id := range g.order {
		if indeg[id] == 0 {
			heap.Push(h, id)
		}
	}

	order := make([]string, 0, len(g.order))
	for h.Len() > 0 {
		id := heap.Pop(h).(string)
		order = append(order, id)
		for _, next := range adj[id] {
			indeg[next]--
			if indeg[next] == 0 {
				heap.Push(h, next)
			}
		}
	}

	if len(order) < len(g.order) {
		remaining := make(map[string]bool, len(g.order)-len(order))
		for _, id := range g.order {
			remaining[id] = true
		}
		for _, id := range order {
			delete(remaining, id)
		}
		return nil, nil, &CycleError{Path: findCycle(g.order, remaining, adj)}
	}

	return order, misses, nil
}

// missFor decides which endpoint of a soft edge with at least one missing
// side "declared the constraint" and which side is the dangling reference.
//
// The convention: whichever endpoint DOES exist is the node that declared
// the constraint (it's the only one that could have -- the other one isn't
// even in the graph to have declared anything), and the direction word
// describes what it said about the missing side. from exists, to missing:
// from said "I go before to", so Dir is "before". to exists, from missing:
// to said "I go after from", so Dir is "after".
func missFor(e edge, fromOK, toOK bool) (Miss, bool) {
	switch {
	case fromOK && !toOK:
		return Miss{Node: e.from, Ref: e.to, Dir: "before"}, true
	case !fromOK && toOK:
		return Miss{Node: e.to, Ref: e.from, Dir: "after"}, true
	default:
		// Neither endpoint exists, so there is no real node to attribute
		// the miss to; an edge between two nodes that both don't exist
		// carries no information worth reporting.
		return Miss{}, false
	}
}

// findCycle runs a DFS over the subgraph induced by remaining (the nodes
// Kahn's algorithm never managed to emit, because they're stuck in a cycle
// or depend on one) and returns the first cycle it finds, as a full path
// with the starting node repeated at the end.
//
// insertionOrder (rather than ranging over the remaining map directly) is
// what makes the result deterministic: map iteration order is randomized by
// Go itself, so picking DFS roots and successors straight from a map would
// make Sort's error message flip between equally-valid cycles from one run
// to the next on the very same graph.
func findCycle(insertionOrder []string, remaining map[string]bool, adj map[string][]string) []string {
	visited := make(map[string]bool, len(remaining))
	onStack := make(map[string]bool, len(remaining))
	var stack []string
	var found []string

	var visit func(id string) bool
	visit = func(id string) bool {
		visited[id] = true
		onStack[id] = true
		stack = append(stack, id)

		for _, next := range adj[id] {
			if !remaining[next] {
				continue
			}
			if onStack[next] {
				start := 0
				for i, s := range stack {
					if s == next {
						start = i
						break
					}
				}
				found = append(append([]string{}, stack[start:]...), next)
				return true
			}
			if !visited[next] {
				if visit(next) {
					return true
				}
			}
		}

		onStack[id] = false
		stack = stack[:len(stack)-1]
		return false
	}

	for _, id := range insertionOrder {
		if !remaining[id] {
			continue
		}
		if !visited[id] {
			if visit(id) {
				return found
			}
		}
	}
	return nil
}

// idHeap is a container/heap min-heap of node ids, ordered by each id's
// insertion index rather than by the id string itself -- Sort's stability
// guarantee is about insertion order, not lexical order.
type idHeap struct {
	ids   []string
	index map[string]int
}

func (h idHeap) Len() int           { return len(h.ids) }
func (h idHeap) Less(i, j int) bool { return h.index[h.ids[i]] < h.index[h.ids[j]] }
func (h idHeap) Swap(i, j int)      { h.ids[i], h.ids[j] = h.ids[j], h.ids[i] }
func (h *idHeap) Push(x any)        { h.ids = append(h.ids, x.(string)) }
func (h *idHeap) Pop() any {
	old := h.ids
	n := len(old)
	item := old[n-1]
	h.ids = old[:n-1]
	return item
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
gofmt -l . && go vet ./... && go test ./internal/graph/... -count=1 -v
```

Expected：`internal/graph/graph_test.go` 里全部 11 个测试 PASS，`go vet`/`gofmt -l` 无输出。

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
git add internal/graph/graph.go internal/graph/graph_test.go
git commit -m "feat(graph): 通用稳定拓扑排序器，Kahn + 插入序小顶堆 + 环路径还原"
```
### Task 4: registry.go —— 按 (类型, 实例名) 索引的注册表 + 接口可赋值性匹配

**Files:**
- Create: `registry.go`
- Modify: `context.go`（追加 `(*Context).registry()` 访问器）
- Modify: `xbc.go`（`App` 追加 `registry *registry` 字段并在 `New()` 里初始化——见 Step 4 的说明）
- Test: `registry_test.go`

**Interfaces:**

Consumes（来自其他 task，本 task 直接使用、不重新定义）：
- `type Context struct { app *App; name string; instance string; logger log.Logger }`（Task 1）
- `func (c *Context) Instance() string`（Task 1）
- `func typeOf[T any]() reflect.Type`（Task 2，`deps.go`）
- `func normInstance(s string) string`（Task 2，`deps.go`；空串归一为 `"default"`）
- `type App struct { /* unexported */ }`、`func New() *App`（Task 1，`xbc.go`）

Produces（本 task 定义，供后续 task 使用）：
```go
type registryKey struct {
	typ      reflect.Type
	instance string
}

type registry struct { /* see Step 3 */ }

func newRegistry() *registry
func (r *registry) put(typ reflect.Type, instance string, v any)
func (r *registry) lookup(want reflect.Type, instance string) (any, error)
func (r *registry) concreteTypes(instance string) []reflect.Type

type NotFoundError struct {
	Want     reflect.Type
	Instance string
	Closest  reflect.Type
	Missing  []string
}
type AmbiguousError struct {
	Want       reflect.Type
	Instance   string
	Candidates []reflect.Type
}

func Provide[T any](ctx *Context, v T)
func Get[T any](ctx *Context) (T, bool)
func GetNamed[T any](ctx *Context, name string) (T, bool)
func MustGet[T any](ctx *Context) T
func MustGetNamed[T any](ctx *Context, name string) T

func (c *Context) registry() *registry // appended to context.go
```

`registry` 是整个内核里唯一负责回答「这个类型、这个实例，谁能提供」的地方——阶段 4 的依赖解析（Task 10）和插件运行期互相取产物（`xbc.Get`/`xbc.MustGet`）走的是同一份存储，行为必须完全一致，所以只写一次、写在这里。

**关于 `lookup` 的三分支语义（API 契约 §5.6）：**

| `want` 的种类 | 行为 |
|---|---|
| 具体类型 | 精确命中 `(want, instance)`；未命中 → `*NotFoundError`，`Closest` 为 `nil`（具体类型不谈"最接近"，命中就是命中，不命中就是不命中，没有中间态） |
| 接口，先试精确命中 `(want, instance)` 本身（万一真的注册了一个接口类型的值） | 命中则直接返回，省掉扫描 |
| 接口，扫描同 `instance` 下全部已登记具体类型，取 `AssignableTo(want)` 的 | 命中 0 个 → `*NotFoundError` 带 `Closest`/`Missing`；命中 1 个 → 返回；命中 ≥2 个 → `*AmbiguousError` |

"最接近"的判据是方法名比对（不比签名）：对每个已登记的具体类型，数它有多少个方法名与 `want` 的方法名重合，取最多的；并列时取**先注册**的——这就是为什么 `registry` 要单独维护一个 `order []registryKey` 而不能只靠 `map` 的遍历顺序，Go 的 map 迭代顺序是随机的，同样的输入两次跑出来的错误文案里候选顺序会不一样，用户拿着报错文案去 issue 里搜都搜不出稳定复现。

**关于 `NotFoundError`/`AmbiguousError` 的 `Error()` 该写多细——一个必须现在讲清楚的边界：**

API 契约 §13 给的是"最终呈现给用户"的完整文案，例如：

```
xbc: 插件 ratelimit 需要 xbc_test.Counter，无任何插件提供
  最接近的是 *fake.HalfCounter，缺少方法：Expire
```

这行文案里的"插件 ratelimit"是**调用方**（阶段 4 依赖解析，Task 10）才知道的信息——`registry` 只知道"谁在找什么类型、什么实例"，不知道"是哪个插件在找"。所以本 task 里 `NotFoundError.Error()` / `AmbiguousError.Error()` 只产出不含插件名的、可独立测试的核心文案（例如 `xbc: 未找到类型 %s（实例 %q），最接近的是 %s，缺少方法：%s`），Task 10 拿到这个 error 后再包一层插件名前缀拼成契约里的最终样子。这不是偷懒少做一半，而是关注点分离：`registry` 不认识 `instance` 结构（Task 4 的输入类型），插件名这个信息本来就不该出现在 `registry.go` 里。

**关于为什么 Task 4 要碰 `xbc.go`（Global Constraints 的 File Structure 表只写了 `registry.go` 一个 Create 和 `context.go` 一个 Modify）：**

`Provide`/`Get` 等函数只拿得到 `*Context`，而 `Context` 的字段已经在 Task 1 钉死（`app *App; name string; instance string; logger log.Logger`），没有留注册表的槛位——注册表必须挂在 `*App` 上，靠 `ctx.app.registry` 这条链路才摸得到，否则同一个 `App` 下的两个 `Context`（不同插件、不同实例）会各自拿到一份注册表，`Provide` 在插件 A 里塞的东西，插件 B 完全看不到，整个依赖注入的前提就垮了。`App` 的字段在 API 契约里是 `/* unexported */`，没有被钉死，加一个字段不违反任何契约条款。这是"可编译、语义正确"优先于"严格只改列出的文件"的一次判断，写在这里是为了让读计划的人知道这不是漏看了文件列表，而是有意为之。

- [ ] **Step 1: 编写 registry_test.go（失败测试）**

  ```go
  // registry_test.go
  package xbc

  import (
  	"reflect"
  	"sync"
  	"testing"

  	"github.com/stretchr/testify/require"
  )

  // ---- everything in this file uses local fake types; no importing gorm/redis or any real middleware ----

  // Counter is a small two-method interface, exercising the interface-assignability matching path.
  type Counter interface {
  	Incr(key string) int64
  	Expire(key string, seconds int) error
  }

  type fakeDB struct{ name string }

  type counterA struct{ hits int64 }

  func (c *counterA) Incr(key string) int64               { c.hits++; return c.hits }
  func (c *counterA) Expire(key string, seconds int) error { return nil }

  type counterB struct{}

  func (c *counterB) Incr(key string) int64               { return 1 }
  func (c *counterB) Expire(key string, seconds int) error { return nil }

  // halfCounter implements only Incr, for testing the "zero hits, report the
  // closest candidate's missing method" diagnostic path.
  type halfCounter struct{}

  func (c *halfCounter) Incr(key string) int64 { return 0 }

  func newTestApp() *App {
  	return &App{registry: newRegistry()}
  }

  // newTestContext takes a shared app rather than building a fresh registry each
  // time -- otherwise tests for "can one instance see what another Provide'd"
  // would not exercise the real behavior at all.
  func newTestContext(app *App, instance string) *Context {
  	return &Context{app: app, name: "test", instance: instance}
  }

  func TestRegistryConcreteHitAndMiss(t *testing.T) {
  	r := newRegistry()
  	db := &fakeDB{name: "primary"}
  	dbType := reflect.TypeOf(db)
  	r.put(dbType, "default", db)

  	got, err := r.lookup(dbType, "default")
  	require.NoError(t, err, "已登记的具体类型应当能精确命中")
  	require.Same(t, db, got)

  	_, err = r.lookup(dbType, "readonly")
  	require.Error(t, err, "同类型换一个实例名应当未命中")
  	var nfe *NotFoundError
  	require.ErrorAs(t, err, &nfe)
  	require.Nil(t, nfe.Closest, "具体类型未命中没有\"最接近\"这个概念，Closest 必须是 nil")
  }

  func TestRegistryInstanceIsolation(t *testing.T) {
  	r := newRegistry()
  	def := &fakeDB{name: "default"}
  	ro := &fakeDB{name: "readonly"}
  	r.put(reflect.TypeOf(def), "default", def)
  	r.put(reflect.TypeOf(ro), "readonly", ro)

  	got, err := r.lookup(reflect.TypeOf(def), "default")
  	require.NoError(t, err)
  	require.Same(t, def, got, "default 实例下应取到 default 那份，不能串到 readonly")

  	got, err = r.lookup(reflect.TypeOf(ro), "readonly")
  	require.NoError(t, err)
  	require.Same(t, ro, got)
  }

  func TestRegistryLookupDoesNotNormalizeInstance(t *testing.T) {
  	// registry is the lowest-level store and does not normalize instance
  	// names itself -- an empty string and "default" are two distinct keys at
  	// this layer. Normalization is the job of the facade functions above it
  	// (Get/GetNamed/Provide, see the next test); this pins down the fact that
  	// registry's raw behavior never normalizes, so nobody later sneaks
  	// normalization into registry itself, which would turn the facade
  	// layer's normalization into duplicated or conflicting work.
  	r := newRegistry()
  	db := &fakeDB{name: "x"}
  	r.put(reflect.TypeOf(db), "default", db)

  	_, err := r.lookup(reflect.TypeOf(db), "")
  	require.Error(t, err, "registry 这一层不把空串等同于 default")
  }

  func TestEmptyInstanceEqualsDefaultThroughFacade(t *testing.T) {
  	app := newTestApp()
  	ctx := newTestContext(app, "default")
  	db := &fakeDB{name: "x"}
  	Provide(ctx, db)

  	got, ok := GetNamed[*fakeDB](ctx, "")
  	require.True(t, ok, "GetNamed 的空实例名经归一化后应等价于 default")
  	require.Same(t, db, got)

  	got2, ok2 := GetNamed[*fakeDB](ctx, "default")
  	require.True(t, ok2)
  	require.Same(t, db, got2)
  }

  func TestRegistryInterfaceUniqueHit(t *testing.T) {
  	r := newRegistry()
  	a := &counterA{}
  	r.put(reflect.TypeOf(a), "default", a)

  	want := reflect.TypeOf((*Counter)(nil)).Elem()
  	got, err := r.lookup(want, "default")
  	require.NoError(t, err, "唯一实现了 Counter 的具体类型应当被命中")
  	require.Same(t, a, got)
  }

  func TestRegistryInterfaceZeroHitReportsClosest(t *testing.T) {
  	r := newRegistry()
  	half := &halfCounter{}
  	r.put(reflect.TypeOf(half), "default", half)

  	want := reflect.TypeOf((*Counter)(nil)).Elem()
  	_, err := r.lookup(want, "default")
  	require.Error(t, err)
  	var nfe *NotFoundError
  	require.ErrorAs(t, err, &nfe)
  	require.Equal(t, reflect.TypeOf(half), nfe.Closest, "唯一候选即最接近的候选")
  	require.Equal(t, []string{"Expire"}, nfe.Missing, "halfCounter 只缺 Expire 这一个方法")
  }

  func TestRegistryInterfaceMultiHitReportsCandidatesInOrder(t *testing.T) {
  	r := newRegistry()
  	a := &counterA{}
  	b := &counterB{}
  	r.put(reflect.TypeOf(a), "default", a)
  	r.put(reflect.TypeOf(b), "default", b)

  	want := reflect.TypeOf((*Counter)(nil)).Elem()
  	_, err := r.lookup(want, "default")
  	require.Error(t, err)
  	var ae *AmbiguousError
  	require.ErrorAs(t, err, &ae)
  	require.Equal(t, []reflect.Type{reflect.TypeOf(a), reflect.TypeOf(b)}, ae.Candidates,
  		"候选顺序必须与登记顺序一致，否则报错文案在不同运行间会飘，用户没法拿着文案去复现")
  }

  func TestMustGetPanicsWithFullDiagnostics(t *testing.T) {
  	app := newTestApp()
  	ctx := newTestContext(app, "default")
  	half := &halfCounter{}
  	Provide(ctx, half)

  	defer func() {
  		r := recover()
  		require.NotNil(t, r, "MustGetNamed 在未命中时必须 panic，不能返回零值让调用者带着 nil 指针继续跑")
  		msg, ok := r.(string)
  		require.True(t, ok)
  		require.Contains(t, msg, "Expire", "panic 文案要带上缺失的方法名，否则排查者两眼一抹黑")
  	}()
  	MustGetNamed[Counter](ctx, "default")
  }

  func TestProvideGetRoundTrip(t *testing.T) {
  	app := newTestApp()
  	ctx := newTestContext(app, "default")
  	db := &fakeDB{name: "primary"}
  	Provide(ctx, db)

  	got, ok := Get[*fakeDB](ctx)
  	require.True(t, ok)
  	require.Same(t, db, got)
  }

  func TestProvideStoresInstanceFromCtx(t *testing.T) {
  	app := newTestApp()
  	roCtx := newTestContext(app, "readonly")
  	db := &fakeDB{name: "ro"}
  	Provide(roCtx, db)

  	defCtx := newTestContext(app, "default")
  	_, ok := Get[*fakeDB](defCtx)
  	require.False(t, ok, "Provide 落在 readonly 实例名下，default 实例看不到它")

  	got, ok := GetNamed[*fakeDB](defCtx, "readonly")
  	require.True(t, ok)
  	require.Same(t, db, got)
  }

  func TestRegistryConcurrentPutAndLookupDoesNotRace(t *testing.T) {
  	r := newRegistry()
  	var wg sync.WaitGroup
  	for i := 0; i < 50; i++ {
  		wg.Add(2)
  		go func() {
  			defer wg.Done()
  			r.put(reflect.TypeOf(&fakeDB{}), "default", &fakeDB{name: "x"})
  		}()
  		go func() {
  			defer wg.Done()
  			_, _ = r.lookup(reflect.TypeOf(&fakeDB{}), "default")
  		}()
  	}
  	wg.Wait()
  }
  ```

  多实例隔离、"接口零命中报诊断"、"接口多命中报候选顺序"、`MustGet` panic 文案、并发安全，这五点是 §5.6 表格逐行对应的行为，也是整个依赖注入子系统里最容易被"看起来能跑"糊弄过去的部分——不写测试，代码看起来完全正常，直到线上真的出现两个插件都提供了 `Counter` 接口的那一天才会炸。

- [ ] **Step 2: 运行测试，确认因缺少实现而失败**

  ```bash
  go test . -run TestRegistry -v
  ```

  预期失败原因：`registry.go` 还不存在，编译器报 `undefined: newRegistry`（以及 `undefined: NotFoundError`、`undefined: AmbiguousError`、`undefined: Provide` 等一连串未定义符号），`App{registry: newRegistry()}` 这行还会额外报 `unknown field registry in struct literal`——这是本 task 需要在 `xbc.go` 里补的字段，此刻它也理所当然不存在。

- [ ] **Step 3: 实现 registry.go**

  ```go
  // registry.go
  package xbc

  import (
  	"fmt"
  	"reflect"
  	"strings"
  	"sync"
  )

  // registryKey identifies one stored value by its concrete type and instance name.
  type registryKey struct {
  	typ      reflect.Type
  	instance string
  }

  // registry is the (type, instance) -> value store shared by every Context
  // that belongs to the same App. It never normalizes the instance argument --
  // that is the caller's job (see normInstance in deps.go), so this type can
  // be tested in isolation from the "" == "default" convention.
  type registry struct {
  	mu    sync.RWMutex
  	m     map[registryKey]any
  	order []registryKey // stable insertion order, for deterministic diagnostics
  }

  func newRegistry() *registry {
  	return &registry{m: make(map[registryKey]any)}
  }

  // put stores v under (typ, instance). Re-putting the same key overwrites the
  // value but does not change its position in order -- order only tracks the
  // first registration of each key.
  func (r *registry) put(typ reflect.Type, instance string, v any) {
  	r.mu.Lock()
  	defer r.mu.Unlock()

  	key := registryKey{typ: typ, instance: instance}
  	if _, exists := r.m[key]; !exists {
  		r.order = append(r.order, key)
  	}
  	r.m[key] = v
  }

  // lookup implements the three-branch semantics documented in the API
  // contract §5.6: exact hit for concrete types; for interface types, an
  // exact hit first, then a scan of every concrete type registered under the
  // same instance for AssignableTo(want).
  func (r *registry) lookup(want reflect.Type, instance string) (any, error) {
  	r.mu.RLock()
  	defer r.mu.RUnlock()

  	if v, ok := r.m[registryKey{typ: want, instance: instance}]; ok {
  		return v, nil
  	}

  	if want.Kind() != reflect.Interface {
  		return nil, &NotFoundError{Want: want, Instance: instance}
  	}

  	var candidates []reflect.Type
  	for _, key := range r.order {
  		if key.instance != instance {
  			continue
  		}
  		if key.typ.AssignableTo(want) {
  			candidates = append(candidates, key.typ)
  		}
  	}

  	switch len(candidates) {
  	case 0:
  		closest, missing := closestMatch(r.order, instance, want)
  		return nil, &NotFoundError{Want: want, Instance: instance, Closest: closest, Missing: missing}
  	case 1:
  		return r.m[registryKey{typ: candidates[0], instance: instance}], nil
  	default:
  		return nil, &AmbiguousError{Want: want, Instance: instance, Candidates: candidates}
  	}
  }

  // concreteTypes returns every concrete type registered under instance, in
  // registration order. Used by later stages (e.g. startup diagnostics) that
  // need to enumerate what an instance actually provides.
  func (r *registry) concreteTypes(instance string) []reflect.Type {
  	r.mu.RLock()
  	defer r.mu.RUnlock()

  	var out []reflect.Type
  	for _, key := range r.order {
  		if key.instance == instance {
  			out = append(out, key.typ)
  		}
  	}
  	return out
  }

  // closestMatch picks, among every concrete type registered under instance,
  // the one implementing the most methods of want by name (signature
  // mismatches still count as "missing"). Ties keep the earliest registered
  // type, which is why the scan walks order rather than the map.
  func closestMatch(order []registryKey, instance string, want reflect.Type) (reflect.Type, []string) {
  	var best reflect.Type
  	var bestMissing []string
  	bestScore := -1

  	for _, key := range order {
  		if key.instance != instance {
  			continue
  		}
  		score, missing := methodScore(key.typ, want)
  		if score > bestScore {
  			bestScore = score
  			best = key.typ
  			bestMissing = missing
  		}
  	}
  	return best, bestMissing
  }

  // methodScore counts how many of want's methods candidate also has, by name only.
  func methodScore(candidate, want reflect.Type) (score int, missing []string) {
  	for i := 0; i < want.NumMethod(); i++ {
  		name := want.Method(i).Name
  		if _, ok := candidate.MethodByName(name); ok {
  			score++
  		} else {
  			missing = append(missing, name)
  		}
  	}
  	return score, missing
  }

  // NotFoundError is returned when lookup finds nothing for (Want, Instance).
  // Closest is nil for a concrete Want (there is no "closest" concept when the
  // match is exact-or-nothing) and also nil when the instance has nothing
  // registered at all.
  //
  // Error renders the reusable core message only -- it does not know which
  // plugin was asking, so it does not carry a plugin name. The stage that
  // resolves dependencies (stage_resolve.go) wraps this with the plugin name
  // to produce the exact copy in the API contract §13.
  type NotFoundError struct {
  	Want     reflect.Type
  	Instance string
  	Closest  reflect.Type
  	Missing  []string
  }

  func (e *NotFoundError) Error() string {
  	if e.Closest == nil {
  		return fmt.Sprintf("xbc: 未找到类型 %s（实例 %q），该实例下未登记任何类型", e.Want, e.Instance)
  	}
  	return fmt.Sprintf("xbc: 未找到类型 %s（实例 %q），最接近的是 %s，缺少方法：%s",
  		e.Want, e.Instance, e.Closest, strings.Join(e.Missing, ", "))
  }

  // AmbiguousError is returned when an interface Want matches more than one
  // registered concrete type under the same instance. Candidates preserves
  // registration order.
  type AmbiguousError struct {
  	Want       reflect.Type
  	Instance   string
  	Candidates []reflect.Type
  }

  func (e *AmbiguousError) Error() string {
  	names := make([]string, len(e.Candidates))
  	for i, c := range e.Candidates {
  		names[i] = c.String()
  	}
  	return fmt.Sprintf("xbc: 类型 %s（实例 %q）匹配到 %d 个候选：%s",
  		e.Want, e.Instance, len(e.Candidates), strings.Join(names, ", "))
  }

  // Provide registers v under the calling plugin's own instance name.
  //
  // T can be instantiated with an interface type, but reflect.TypeOf(v) always
  // yields v's dynamic concrete type -- the registry only ever stores concrete
  // types, matching lookup's assumption that r.order enumerates concrete types.
  func Provide[T any](ctx *Context, v T) {
  	ctx.registry().put(reflect.TypeOf(v), ctx.Instance(), v)
  }

  // Get looks up the default instance of T. It is GetNamed with an empty name.
  func Get[T any](ctx *Context) (T, bool) {
  	return GetNamed[T](ctx, "")
  }

  // GetNamed looks up T under the given instance name ("" means default).
  func GetNamed[T any](ctx *Context, name string) (T, bool) {
  	var zero T
  	want := typeOf[T]()
  	v, err := ctx.registry().lookup(want, normInstance(name))
  	if err != nil {
  		return zero, false
  	}
  	tv, ok := v.(T)
  	if !ok {
  		return zero, false
  	}
  	return tv, true
  }

  // MustGet is MustGetNamed with an empty name.
  func MustGet[T any](ctx *Context) T {
  	return MustGetNamed[T](ctx, "")
  }

  // MustGetNamed looks up T under the given instance name and panics with the
  // full diagnostic (including any Closest/Missing/Candidates detail) when it
  // is not found or ambiguous.
  func MustGetNamed[T any](ctx *Context, name string) T {
  	want := typeOf[T]()
  	inst := normInstance(name)
  	v, err := ctx.registry().lookup(want, inst)
  	if err != nil {
  		panic(err.Error())
  	}
  	tv, ok := v.(T)
  	if !ok {
  		panic(fmt.Sprintf("xbc: 类型断言失败：注册表中 %s（实例 %q）的值无法转换为 %s", reflect.TypeOf(v), inst, want))
  	}
  	return tv
  }
  ```

  `time.Duration` 那类"底层类型陷阱"在这里不存在，但有一个同类的坑：`Provide[T any](ctx *Context, v T)` 如果被显式实例化成接口类型调用（例如 `xbc.Provide[Counter](ctx, a)`），`reflect.TypeOf(v)` 拿到的仍然是 `v` 装箱后的动态类型（`*counterA`），不是 `Counter` 本身——`reflect.TypeOf` 的参数类型是 `any`，`v` 传进去的时候已经完成了到 `any` 的装箱转换。这正是我们想要的行为：注册表永远按具体类型存，`lookup` 对接口类型做的 `AssignableTo` 扫描才有意义。

- [ ] **Step 4: 接入 context.go 与 xbc.go**

  在 `context.go` 里追加一个方法（放在 `Instance()`/`Name()` 附近即可）：

  ```go
  // registry returns the App-wide (type, instance) store this Context's App owns.
  // Unexported: Provide/Get/GetNamed/MustGet/MustGetNamed are the public seam.
  func (c *Context) registry() *registry {
  	return c.app.registry
  }
  ```

  在 `xbc.go` 的 `App` 结构体定义里追加一个字段：

  ```go
  type App struct {
  	// ...Task 1's existing fields stay unchanged...
  	registry *registry
  }
  ```

  并在 `New()` 现有的字段初始化列表里追加一行：

  ```go
  func New() *App {
  	return &App{
  		// ...Task 1's existing initialization stays unchanged...
  		registry: newRegistry(),
  	}
  }
  ```

  这两处都是纯追加，不删改 Task 1 已经写好的任何一行。

- [ ] **Step 5: 运行测试确认全部通过**

  ```bash
  go test . -run TestRegistry -v
  go test . -run TestProvide -v
  go test . -run TestMustGet -v
  go test -race . -run TestRegistryConcurrentPutAndLookupDoesNotRace
  ```

- [ ] **Step 6: 提交**

  ```bash
  gofmt -l . && go vet ./... && go test ./... -count=1
  git add registry.go registry_test.go context.go xbc.go
  git commit -m "feat(xbc): 实现按 (类型, 实例名) 索引的注册表与接口匹配"
  ```

---

### Task 5: internal/conf 加载与绑定 —— 文件查找、profile 合并、schema 驱动的 ENV 覆盖、key 集合感知的 default 填充

**Files:**
- Create: `internal/conf/load.go`
- Create: `internal/conf/bind.go`
- Test: `internal/conf/load_test.go`
- Test: `internal/conf/bind_test.go`

**Interfaces:**

Consumes：仅标准库 + `github.com/knadh/koanf/v2` + `github.com/knadh/koanf/parsers/yaml` + `github.com/knadh/koanf/providers/file` + `github.com/knadh/koanf/providers/confmap`。`internal/conf` 是"洁癖包"（Global Constraints），不认识任何 xbc 类型，本 task 的两个文件都不 import 根包 `github.com/xbcio/xbc`。

Produces：
```go
package conf

type Options struct {
	File      string
	Profile   string
	EnvPrefix string
	Overrides map[string]any
}

func Load(opts Options) (*koanf.Koanf, error)

type Leaf struct {
	Path  string
	Index []int
	Type  reflect.Type
}

func Leaves(root string, out any) []Leaf
func EnvName(prefix, path string) string
func Bind(k *koanf.Koanf, path string, out any, envPrefix string) error
```

`Load` 与 `Bind` 分别对应 spec §6.1 的两条职责：前者决定"配置从哪些文件、以什么顺序进到内存里的 `*koanf.Koanf` 树"；后者决定"内存里那棵树怎么落到某个插件的 `Config` 结构体字段上"。二者之间只通过 `*koanf.Koanf` 这一个值交接，`internal/conf` 包内没有全局状态，可以在同一个进程里多次调用（多实例插件每个实例都要独立 `Bind` 一次）。

**`Load` 的查找与合并顺序（裁决 R8）：**

1. 若 `Options.File` 非空，只认这一个路径；不存在就是错误（这是唯一一种"没找到配置文件"会报错的情况）。
2. 否则依次探测 `./application.yml`、`./configs/application.yml`；两个都没有 → 合法的空配置，不报错。
3. 找到主文件后，若 `Options.Profile` 非空，探测同目录下的 `<主文件名>-<profile><后缀>`（例如 `application.yml` + `profile=prod` → `application-prod.yml`）；这个 profile 文件不存在时**静默跳过**——profile 叠加从一开始就是可选的锦上添花，不是"用户显式承诺了它一定存在"的东西，这点与主文件的 `--config` 语义不同，不能套用同一条报错规则。
4. 最后应用 `Options.Overrides`（预期是扁平的 `"server.addr"` 这类点号路径 → 值的 map，与 CLI `--set` 风格的 flag 天然对应），用 `confmap.Provider(overrides, ".")` 加载，`delim` 传 `"."` 是因为这批 key 是扁平的，需要 `confmap` 内部 `Unflatten` 成嵌套结构后再并入 `koanf` 树。

同一个 `*koanf.Koanf` 实例连续 `Load` 两次（主文件、profile 文件）是安全的——koanf 底层对同 key 采用"后者覆盖前者的叶子值，其余深度合并"的语义，不需要额外调用 `Merge`。

**`Bind` 的三步链（裁决 R2、R9、R11）：**

1. `k.UnmarshalWithConf(path, out, koanf.UnmarshalConf{Tag: "yaml"})`——tag 用 `yaml` 不用 koanf 默认的 `koanf`（裁决 R9），因为插件 `Config` 结构体全部用 `yaml:"..."` 打标。
2. 用 `Leaves` 枚举 `out` 的 schema，对每条叶子路径反查环境变量名（`EnvName`），命中就用 `setScalar` 写进对应字段——这是裁决 R2 的核心：不做 `_` → `.` 的字面替换，因为 `max_open_conn` 这种多词 key 在字面替换下无法与分隔符区分。
3. 对**没被文件覆盖、也没被 ENV 覆盖**的叶子路径（`!k.Exists(path) && !envSet[path]`），填 `default` tag——裁决 R11 强调的"key 集合感知"：判据是"这条路径有没有出现过"，不是"字段是不是零值"，否则 `enabled: false`、`max_retries: 0` 这类显式写了零值的配置会被 default 悄悄改掉。

- [ ] **Step 1: 编写 load_test.go 与 bind_test.go（失败测试）**

  ```go
  // internal/conf/load_test.go
  package conf

  import (
  	"os"
  	"path/filepath"
  	"testing"

  	"github.com/stretchr/testify/require"
  )

  func writeYAML(t *testing.T, path, content string) {
  	t.Helper()
  	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
  	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
  }

  func TestLoadFindsExplicitConfigFile(t *testing.T) {
  	dir := t.TempDir()
  	t.Chdir(dir)
  	p := filepath.Join(dir, "custom.yml")
  	writeYAML(t, p, "server:\n  addr: \":9000\"\n")

  	k, err := Load(Options{File: p})
  	require.NoError(t, err)
  	require.Equal(t, ":9000", k.String("server.addr"))
  }

  func TestLoadFindsApplicationYMLInCurrentDir(t *testing.T) {
  	dir := t.TempDir()
  	t.Chdir(dir)
  	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":9001\"\n")

  	k, err := Load(Options{})
  	require.NoError(t, err)
  	require.Equal(t, ":9001", k.String("server.addr"))
  }

  func TestLoadFindsConfigsApplicationYML(t *testing.T) {
  	dir := t.TempDir()
  	t.Chdir(dir)
  	writeYAML(t, filepath.Join(dir, "configs", "application.yml"), "server:\n  addr: \":9002\"\n")

  	k, err := Load(Options{})
  	require.NoError(t, err)
  	require.Equal(t, ":9002", k.String("server.addr"))
  }

  func TestLoadExplicitFileMissingIsError(t *testing.T) {
  	dir := t.TempDir()
  	t.Chdir(dir)

  	_, err := Load(Options{File: filepath.Join(dir, "not-there.yml")})
  	require.Error(t, err, "显式指定的 --config 路径不存在必须报错（裁决 R8），不能静默按空配置跑")
  }

  func TestLoadWithNoFileAnywhereIsNotAnError(t *testing.T) {
  	dir := t.TempDir()
  	t.Chdir(dir)

  	k, err := Load(Options{})
  	require.NoError(t, err, "三个位置都没有配置文件时，按空配置继续是合法的（裁决 R8）")
  	require.False(t, k.Exists("server.addr"))
  }

  func TestLoadMergesProfileOverlay(t *testing.T) {
  	dir := t.TempDir()
  	t.Chdir(dir)
  	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n  base_path: /\n")
  	writeYAML(t, filepath.Join(dir, "application-prod.yml"), "server:\n  addr: \":80\"\n")

  	k, err := Load(Options{Profile: "prod"})
  	require.NoError(t, err)
  	require.Equal(t, ":80", k.String("server.addr"), "profile 里同 key 应当覆盖主文件")
  	require.Equal(t, "/", k.String("server.base_path"), "profile 没提到的 key 应当保留主文件的值")
  }

  func TestLoadProfileMissingSiblingIsSilentlySkipped(t *testing.T) {
  	dir := t.TempDir()
  	t.Chdir(dir)
  	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n")

  	k, err := Load(Options{Profile: "does-not-exist"})
  	require.NoError(t, err, "profile 叠加文件缺失不是错误，只有主文件的显式路径缺失才是错误")
  	require.Equal(t, ":8080", k.String("server.addr"))
  }

  func TestLoadOverridesWinOverEverything(t *testing.T) {
  	dir := t.TempDir()
  	t.Chdir(dir)
  	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n")
  	writeYAML(t, filepath.Join(dir, "application-prod.yml"), "server:\n  addr: \":80\"\n")

  	k, err := Load(Options{
  		Profile:   "prod",
  		Overrides: map[string]any{"server.addr": ":9999"},
  	})
  	require.NoError(t, err)
  	require.Equal(t, ":9999", k.String("server.addr"), "flag 覆盖必须是最高优先级")
  }
  ```

  ```go
  // internal/conf/bind_test.go
  package conf

  import (
  	"reflect"
  	"testing"
  	"time"

  	"github.com/knadh/koanf/providers/confmap"
  	"github.com/knadh/koanf/v2"
  	"github.com/stretchr/testify/require"
  )

  // gormLikeConfig mimics the shape of a real plugin Config, deliberately
  // carrying a multi-word key to pin down ruling R2 (ENV overrides cannot
  // rely on a literal `_` -> `.` replacement).
  type gormLikeConfig struct {
  	DSN         string        `yaml:"dsn"`
  	MaxOpenConn int           `yaml:"max_open_conn" default:"10"`
  	ConnTimeout time.Duration `yaml:"conn_timeout"  default:"5s"`
  	Enabled     bool          `yaml:"enabled"        default:"true"`
  }

  type tlsLeaf struct {
  	Enabled bool `yaml:"enabled" default:"false"`
  }

  type serverLeaf struct {
  	Addr string  `yaml:"addr" default:":8080"`
  	TLS  tlsLeaf `yaml:"tls"`
  }

  type nestedRoot struct {
  	Server serverLeaf `yaml:"server"`
  }

  type badDefaultConfig struct {
  	Timeout time.Duration `yaml:"timeout" default:"not-a-duration"`
  }

  // koanfFrom builds a koanf tree directly from a nested map, bypassing the
  // filesystem -- bind_test.go tests Bind, not Load, so there is no need to
  // write a temporary YAML file just to grow a tree. An empty delim means
  // data is already nested.
  func koanfFrom(t *testing.T, data map[string]any) *koanf.Koanf {
  	t.Helper()
  	k := koanf.New(".")
  	require.NoError(t, k.Load(confmap.Provider(data, ""), nil))
  	return k
  }

  func TestBindOverlaysEnvForMultiWordKey(t *testing.T) {
  	k := koanfFrom(t, map[string]any{
  		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "file-value"}}},
  	})
  	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "42")

  	var cfg gormLikeConfig
  	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))

  	require.Equal(t, 42, cfg.MaxOpenConn,
  		"多词 key max_open_conn 必须靠 schema 反查命中，字面 _ -> . 替换会把它错译成 max.open.conn")
  }

  func TestBindOverlaysEnvForDuration(t *testing.T) {
  	k := koanfFrom(t, map[string]any{
  		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "x"}}},
  	})
  	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_CONN_TIMEOUT", "15s")

  	var cfg gormLikeConfig
  	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))
  	require.Equal(t, 15*time.Second, cfg.ConnTimeout)
  }

  func TestBindFillsDefaultsForUnsetLeaves(t *testing.T) {
  	k := koanfFrom(t, map[string]any{
  		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "x"}}},
  	})

  	var cfg gormLikeConfig
  	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))

  	require.Equal(t, 10, cfg.MaxOpenConn, "文件和 ENV 都没设，应当落到 default tag")
  	require.Equal(t, 5*time.Second, cfg.ConnTimeout)
  	require.True(t, cfg.Enabled)
  }

  func TestBindExplicitFalseIsNotOverriddenByDefaultTrue(t *testing.T) {
  	k := koanfFrom(t, map[string]any{
  		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{
  			"dsn":     "x",
  			"enabled": false,
  		}}},
  	})

  	var cfg gormLikeConfig
  	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))

  	require.False(t, cfg.Enabled,
  		"yml 里显式写的 enabled: false 不能被 default:\"true\" 悄悄改掉——补 default 必须判 key 是否出现过，不能判字段是否为零值")
  }

  func TestBindDefaultParseFailureIsError(t *testing.T) {
  	k := koanf.New(".")
  	var cfg badDefaultConfig
  	err := Bind(k, "", &cfg, "XBC_")
  	require.Error(t, err, "default tag 本身写错了，必须在绑定阶段就暴露，不能拖到运行期才炸")
  }

  func TestLeavesWalksNestedStruct(t *testing.T) {
  	var cfg nestedRoot
  	leaves := Leaves("", &cfg)

  	paths := make([]string, len(leaves))
  	for i, l := range leaves {
  		paths[i] = l.Path
  	}
  	require.Contains(t, paths, "server.addr")
  	require.Contains(t, paths, "server.tls.enabled")
  }

  func TestEnvName(t *testing.T) {
  	require.Equal(t, "XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN",
  		EnvName("XBC_", "plugins.gorm.default.max_open_conn"))
  }

  func TestSetScalarParsesDurationNotAsInt(t *testing.T) {
  	var d time.Duration
  	v := reflect.ValueOf(&d).Elem()
  	require.NoError(t, setScalar(v, v.Type(), "1h"))
  	require.Equal(t, time.Hour, d,
  		"time.Duration 底层是 int64，判断顺序反了会把 1h 当整数解析失败")
  }

  func TestPriorityChainDefaultFileProfileEnvOverrides(t *testing.T) {
  	// The three tiers each use a different key, avoiding a collision with
  	// the next "known limitation" test's key.
  	k := koanfFrom(t, map[string]any{
  		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{
  			"dsn": "from-file",
  		}}},
  	})
  	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "99")

  	var cfg gormLikeConfig
  	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))

  	require.Equal(t, "from-file", cfg.DSN, "文件设置的字段应该生效")
  	require.Equal(t, 99, cfg.MaxOpenConn, "ENV 设置的字段应该覆盖 default")
  	require.Equal(t, 5*time.Second, cfg.ConnTimeout, "两者都没设的字段落到 default")
  }

  func TestBindEnvBeatsOverridesOnSameKeyKnownLimitation(t *testing.T) {
  	// Known structural limitation: by the time Bind runs, the file, profile,
  	// and Load-stage Overrides have already been flattened into one koanf
  	// tree, so Bind cannot tell which layer a given value originally came
  	// from. The global priority is default < file < profile < ENV < flag,
  	// but within the three-step algorithm the Bind contract specifies
  	// (unmarshal -> ENV -> default), ENV always overrides whatever value is
  	// already in the koanf tree, with no way to tell whether that value came
  	// from flag Overrides. This test pins down the real behavior: on the
  	// same key, ENV beats Overrides -- this is not a globally-true priority
  	// rule, only a boundary of this one Bind step.
  	t.Chdir(t.TempDir())
  	k, err := Load(Options{Overrides: map[string]any{"plugins.gorm.default.max_open_conn": 7}})
  	require.NoError(t, err)
  	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "77")

  	var cfg gormLikeConfig
  	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))
  	require.Equal(t, 77, cfg.MaxOpenConn, "已知限制：ENV 与 flag Overrides 撞在同一个 key 上时，ENV 赢")
  }
  ```

- [ ] **Step 2: 运行测试，确认因缺少实现而失败**

  ```bash
  go test ./internal/conf/... -v
  ```

  预期失败原因：`load.go`、`bind.go` 都还不存在，编译器报 `undefined: Load`、`undefined: Bind`、`undefined: Leaves`、`undefined: EnvName`、`undefined: setScalar`——整个 `internal/conf` 包目前只有测试文件，没有任何被测代码。

- [ ] **Step 3: 实现 internal/conf/load.go**

  ```go
  // internal/conf/load.go
  package conf

  import (
  	"errors"
  	"fmt"
  	"os"
  	"path/filepath"

  	"github.com/knadh/koanf/parsers/yaml"
  	"github.com/knadh/koanf/providers/confmap"
  	"github.com/knadh/koanf/providers/file"
  	"github.com/knadh/koanf/v2"
  )

  // Options carries every knob stage 1 (loadConfig) needs to locate and
  // assemble the configuration tree.
  type Options struct {
  	File      string         // --config; when non-empty the file MUST exist
  	Profile   string         // --profile or XBC_PROFILE
  	EnvPrefix string         // "XBC_"; threaded through to later Bind calls by the caller, unused here
  	Overrides map[string]any // flat dotted-path -> value, e.g. from --set flags
  }

  // Load searches for the config file, merges the profile overlay, applies
  // Overrides, and returns the koanf instance. Missing files are only an error
  // when Options.File named one explicitly (ruling R8).
  func Load(opts Options) (*koanf.Koanf, error) {
  	k := koanf.New(".")

  	basePath, err := locateBaseFile(opts.File)
  	if err != nil {
  		return nil, err
  	}

  	if basePath != "" {
  		if err := k.Load(file.Provider(basePath), yaml.Parser()); err != nil {
  			return nil, fmt.Errorf("xbc: 读取配置文件 %s 失败：%w", basePath, err)
  		}

  		if opts.Profile != "" {
  			profilePath := profileSibling(basePath, opts.Profile)
  			if _, statErr := os.Stat(profilePath); statErr == nil {
  				if err := k.Load(file.Provider(profilePath), yaml.Parser()); err != nil {
  					return nil, fmt.Errorf("xbc: 读取 profile 配置文件 %s 失败：%w", profilePath, err)
  				}
  			}
  			// A missing profile file is silently skipped: it is an optional
  			// overlay, not an explicit promise like --config.
  		}
  	}

  	if len(opts.Overrides) > 0 {
  		if err := k.Load(confmap.Provider(opts.Overrides, "."), nil); err != nil {
  			return nil, fmt.Errorf("xbc: 应用配置覆盖失败：%w", err)
  		}
  	}

  	return k, nil
  }

  // profileSibling derives "application-prod.yml" from "application.yml" + "prod".
  func profileSibling(basePath, profile string) string {
  	dir := filepath.Dir(basePath)
  	ext := filepath.Ext(basePath)
  	name := filepath.Base(basePath)
  	name = name[:len(name)-len(ext)]
  	return filepath.Join(dir, name+"-"+profile+ext)
  }

  // locateBaseFile resolves the main config file path, or "" when none of the
  // three lookup locations has one and that is legal (ruling R8).
  func locateBaseFile(explicitPath string) (string, error) {
  	if explicitPath != "" {
  		if _, err := os.Stat(explicitPath); err != nil {
  			if errors.Is(err, os.ErrNotExist) {
  				return "", fmt.Errorf("xbc: 指定的配置文件 %s 不存在", explicitPath)
  			}
  			return "", fmt.Errorf("xbc: 无法访问配置文件 %s：%w", explicitPath, err)
  		}
  		return explicitPath, nil
  	}

  	for _, candidate := range []string{"application.yml", filepath.Join("configs", "application.yml")} {
  		if _, err := os.Stat(candidate); err == nil {
  			return candidate, nil
  		}
  	}
  	return "", nil
  }
  ```

- [ ] **Step 4: 实现 internal/conf/bind.go**

  ```go
  // internal/conf/bind.go
  package conf

  import (
  	"fmt"
  	"os"
  	"reflect"
  	"strconv"
  	"strings"
  	"time"

  	"github.com/knadh/koanf/v2"
  )

  // Leaf is one scalar leaf in a struct's yaml-tag schema.
  type Leaf struct {
  	Path  string       // e.g. "plugins.gorm.default.max_open_conn"
  	Index []int        // reflect field index path from the struct root
  	Type  reflect.Type
  }

  // Leaves walks out's yaml-tag schema and returns every leaf, with Path
  // rooted at root (root == "" means unrooted, paths start at the struct itself).
  func Leaves(root string, out any) []Leaf {
  	t := reflect.TypeOf(out)
  	for t.Kind() == reflect.Ptr {
  		t = t.Elem()
  	}
  	var leaves []Leaf
  	walkLeaves(root, t, nil, &leaves)
  	return leaves
  }

  // walkLeaves recurses only into plain structs (time.Time is treated as a
  // scalar -- it has no yaml-tagged fields of its own that this scheme cares about).
  func walkLeaves(prefix string, t reflect.Type, index []int, out *[]Leaf) {
  	for i := 0; i < t.NumField(); i++ {
  		f := t.Field(i)
  		if f.PkgPath != "" {
  			continue // unexported field: mapstructure/yaml can't reach it either
  		}
  		name := yamlTagName(f)
  		if name == "-" {
  			continue
  		}

  		path := prefix
  		if name != "" {
  			if path != "" {
  				path += "." + name
  			} else {
  				path = name
  			}
  		}

  		fieldIndex := append(append([]int{}, index...), i)

  		ft := f.Type
  		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}) {
  			walkLeaves(path, ft, fieldIndex, out)
  			continue
  		}
  		*out = append(*out, Leaf{Path: path, Index: fieldIndex, Type: ft})
  	}
  }

  func yamlTagName(f reflect.StructField) string {
  	tag := f.Tag.Get("yaml")
  	if tag == "" {
  		return f.Name
  	}
  	name := strings.Split(tag, ",")[0]
  	if name == "" {
  		return f.Name
  	}
  	return name
  }

  // EnvName maps a config path to its environment variable name:
  // "plugins.gorm.default.dsn" -> "XBC_PLUGINS_GORM_DEFAULT_DSN".
  func EnvName(prefix, path string) string {
  	return prefix + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
  }

  // Bind performs the stage-3 chain for one subtree:
  //   1. unmarshal k's subtree at path into out (mapstructure, Tag "yaml")
  //   2. overlay ENV, driven by out's own yaml-tag schema (ruling R2)
  //   3. fill `default:"..."` tags on leaves that neither the file nor ENV set
  //
  // It does not validate.
  func Bind(k *koanf.Koanf, path string, out any, envPrefix string) error {
  	if err := k.UnmarshalWithConf(path, out, koanf.UnmarshalConf{Tag: "yaml"}); err != nil {
  		return fmt.Errorf("xbc: 绑定配置节 %s 失败：%w", displayPath(path), err)
  	}

  	leaves := Leaves(path, out)
  	v := reflect.ValueOf(out).Elem()

  	envSet := make(map[string]bool, len(leaves))
  	for _, leaf := range leaves {
  		envName := EnvName(envPrefix, leaf.Path)
  		s, ok := os.LookupEnv(envName)
  		if !ok {
  			continue
  		}
  		envSet[leaf.Path] = true
  		fv := fieldByIndex(v, leaf.Index)
  		if err := setScalar(fv, leaf.Type, s); err != nil {
  			return fmt.Errorf("xbc: 环境变量 %s 的值 %q 无法解析为 %s：%w", envName, s, leaf.Type, err)
  		}
  	}

  	for _, leaf := range leaves {
  		if envSet[leaf.Path] || k.Exists(leaf.Path) {
  			continue
  		}
  		defTag, ok := defaultTag(reflect.TypeOf(out), leaf.Index)
  		if !ok {
  			continue
  		}
  		fv := fieldByIndex(v, leaf.Index)
  		if err := setScalar(fv, leaf.Type, defTag); err != nil {
  			return fmt.Errorf("xbc: 字段 %s 的 default tag %q 无法解析为 %s：%w", leaf.Path, defTag, leaf.Type, err)
  		}
  	}

  	return nil
  }

  func displayPath(path string) string {
  	if path == "" {
  		return "(root)"
  	}
  	return path
  }

  func fieldByIndex(v reflect.Value, index []int) reflect.Value {
  	for _, i := range index {
  		v = v.Field(i)
  	}
  	return v
  }

  // defaultTag reads the `default:"..."` tag off the field at the end of index,
  // walking through intermediate struct types along the way.
  func defaultTag(t reflect.Type, index []int) (string, bool) {
  	for t.Kind() == reflect.Ptr {
  		t = t.Elem()
  	}
  	for depth, i := range index {
  		f := t.Field(i)
  		if depth == len(index)-1 {
  			tag := f.Tag.Get("default")
  			return tag, tag != ""
  		}
  		t = f.Type
  		for t.Kind() == reflect.Ptr {
  			t = t.Elem()
  		}
  	}
  	return "", false
  }

  // setScalar parses s according to typ and stores it into v.
  //
  // time.Duration must be checked before the generic int64 case -- it is
  // itself backed by int64, and checking order the other way around would try
  // to parse "1h" as a base-10 integer and fail.
  func setScalar(v reflect.Value, typ reflect.Type, s string) error {
  	switch {
  	case typ == reflect.TypeOf(time.Duration(0)):
  		d, err := time.ParseDuration(s)
  		if err != nil {
  			return err
  		}
  		v.SetInt(int64(d))
  		return nil
  	case typ.Kind() == reflect.String:
  		v.SetString(s)
  		return nil
  	case typ.Kind() == reflect.Bool:
  		b, err := strconv.ParseBool(s)
  		if err != nil {
  			return err
  		}
  		v.SetBool(b)
  		return nil
  	case typ.Kind() >= reflect.Int && typ.Kind() <= reflect.Int64:
  		n, err := strconv.ParseInt(s, 10, 64)
  		if err != nil {
  			return err
  		}
  		v.SetInt(n)
  		return nil
  	case typ.Kind() >= reflect.Uint && typ.Kind() <= reflect.Uint64:
  		n, err := strconv.ParseUint(s, 10, 64)
  		if err != nil {
  			return err
  		}
  		v.SetUint(n)
  		return nil
  	case typ.Kind() == reflect.Float32 || typ.Kind() == reflect.Float64:
  		f, err := strconv.ParseFloat(s, 64)
  		if err != nil {
  			return err
  		}
  		v.SetFloat(f)
  		return nil
  	case typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.String:
  		parts := strings.Split(s, ",")
  		for i, p := range parts {
  			parts[i] = strings.TrimSpace(p)
  		}
  		v.Set(reflect.ValueOf(parts))
  		return nil
  	default:
  		return fmt.Errorf("不支持的标量类型 %s", typ)
  	}
  }
  ```

  `Load` 用的 `confmap.Provider(opts.Overrides, ".")` 和 `Bind` 内部完全不相关——`Bind` 从来不直接摸 `Options`，它只认 `*koanf.Koanf` 和目标结构体，这也是为什么 `Options.EnvPrefix` 这个字段在 `Load` 里完全不用：它是留给调用方（stage_config.go，Task 9）在拿到 `Options` 之后，逐个 `Bind` 调用时原样传下去的参数，`Load` 自己不需要知道 ENV 前缀是什么。

- [ ] **Step 5: 运行验收三件套确认全部通过**

  ```bash
  gofmt -l . && go vet ./... && go test ./... -count=1
  ```

- [ ] **Step 6: 提交**

  ```bash
  git add internal/conf/load.go internal/conf/bind.go internal/conf/load_test.go internal/conf/bind_test.go
  git commit -m "feat(conf): 实现配置文件查找、profile 合并与 schema 驱动的 ENV/default 绑定"
  ```

---

### Task 6: internal/conf/validate.go + 根包 config.go —— 校验文案与框架配置

**Files:**
- Create: `internal/conf/validate.go`
- Create: `config.go`
- Modify: `xbc.go`（`App` 追加 `cfg *Config` 字段——裁决 R12，见 Step 4 末尾）
- Test: `internal/conf/validate_test.go`
- Test: `config_test.go`

**Interfaces:**

Consumes：
- `github.com/go-playground/validator/v10`
- `github.com/xbcio/xbc/internal/conf`（`conf.Bind`，根包 `config.go` 用它实现 `Unmarshal`；`conf.Load` 与 `conf.Options`（Task 5），`loadConfig` 用它们完成阶段 1 的文件查找与 profile 合并）
- `github.com/xbcio/xbc/log`（`log.Config`、`log.DefaultConfig()`、`(*log.Config).Normalize()`、`log.Init(cfg) error`）
- `Leaves`、`yamlTagName`（Task 5，同一个 `internal/conf` 包内，`validate.go` 直接调用未导出函数，不必重新实现）

Produces：
```go
package conf

func Validate(out any, path string) error

type ValidationError struct{ Lines []string }

func (e *ValidationError) Error() string
func (e *ValidationError) Append(other *ValidationError)
```
```go
package xbc

type Config struct {
	Server ServerConfig `yaml:"server"`
	Log    log.Config   `yaml:"log"`
	k *koanf.Koanf
}

type ServerConfig struct {
	Addr            string        `yaml:"addr"             default:":8080"`
	BasePath        string        `yaml:"base_path"        default:"/"`
	ReadTimeout     time.Duration `yaml:"read_timeout"     default:"10s"`
	WriteTimeout    time.Duration `yaml:"write_timeout"    default:"30s"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"30s"`
	AutoMigrate     bool          `yaml:"auto_migrate"`
}

func (c *Config) Unmarshal(path string, out any) error
func (c *Config) Get(path string) any
func (c *Config) Exists(path string) bool
func (c *Config) Sub(path string) map[string]any

// bindLog binds the "log" subtree and brings the global logger up.
func bindLog(k *koanf.Koanf, envPrefix string) (log.Config, error)

// loadConfig is assembly stage 1. Writes App.cfg.
func (a *App) loadConfig(opts conf.Options) error
```

**本 task 落地阶段 1 `loadConfig`。** `kernel-api.md` §12 把十个阶段函数按 1~10 编号，其中阶段 2~10 各自有专属 task（8/9/10/13/14），只有阶段 1 没有——因为它做的三件事全在 `config.go` 的辖区内（`conf.Load`、`bindLog`、绑定并校验 `server` 段），拆出去反而要把 `defaultEnvPrefix`、`bindLog`、`Config.k` 三个未导出符号跨文件用。Task 15 的 `run()` 第一行调的就是它。

**`ServerConfig.WriteTimeout` 是裁决 R10 补的字段**——spec §6.2 只示意性地列了 `read_timeout`，只读不写超时的 `http.Server` 留有慢速读取攻击面，本 task 补上并给默认值 `30s`。

**校验文案的路径还原（`yamlPath`）：** `validator` 报错时给的是 Go 字段名（`fe.StructNamespace()`，形如 `"ServerConfig.Addr"`），但错误文案要按 spec §13 的样子给 yaml 路径（`"server.addr"`）。`yamlPath` 沿着与 `Leaves`/`walkLeaves` 同一套 schema，把 `StructNamespace` 的每一段 Go 字段名换成对应的 `yaml` tag 名——两套代码认的是同一份 struct 定义，字段名到 tag 名的映射规则也必须是同一个函数（`yamlTagName`），否则两边一旦不同步，报错路径就会和真实配置文件里的路径对不上。

**`ValidationError.Append` 与懒对齐：** `Lines` 存的是 `"<path>\t<message>"`，宽度对齐（让所有报错的分隔符列对整齐，参照 spec §13 样例里 `dsn` 和 `addr` 两行的对齐效果）不在写入时算，而是在 `Error()` 被调用的那一刻，对当时整个 `Lines` 切片重新算一次最大路径宽度——这样多个插件各自校验出的 `*ValidationError` 用 `Append` 合并之后，最终打印出来的对齐仍然是正确的，不会出现"先算的两行对齐了，后 append 进来的第三行更长，把前两行的对齐弄乱了却没人重新算"的问题。

- [ ] **Step 1: 编写 validate_test.go 与 config_test.go（失败测试）**

  ```go
  // internal/conf/validate_test.go
  package conf

  import (
  	"testing"

  	"github.com/stretchr/testify/require"
  )

  type gormValidateConfig struct {
  	DSN string `yaml:"dsn" validate:"required"`
  }

  type redisValidateConfig struct {
  	Addr string `yaml:"addr" validate:"required,hostname_port"`
  }

  type poolLeaf struct {
  	MaxOpenConn int `yaml:"max_open_conn" validate:"min=1,max=100"`
  }

  type nestedValidateConfig struct {
  	Pool poolLeaf `yaml:"pool"`
  }

  type oneofValidateConfig struct {
  	Mode string `yaml:"mode" validate:"oneof=daily size"`
  }

  type fallbackValidateConfig struct {
  	Ratio float64 `yaml:"ratio" validate:"gt=0,lt=1"`
  }

  type multiFieldConfig struct {
  	DSN  string `yaml:"dsn" validate:"required"`
  	Addr string `yaml:"addr" validate:"required,hostname_port"`
  }

  func TestValidateRequiredMessage(t *testing.T) {
  	cfg := gormValidateConfig{}
  	err := Validate(&cfg, "plugins.gorm.readonly")
  	require.Error(t, err)
  	require.Contains(t, err.Error(), "plugins.gorm.readonly.dsn")
  	require.Contains(t, err.Error(), "必填项缺失")
  }

  func TestValidateHostnamePortMessageIncludesActualValue(t *testing.T) {
  	cfg := redisValidateConfig{Addr: "127.0.0.1"}
  	err := Validate(&cfg, "plugins.redis.default")
  	require.Error(t, err)
  	require.Contains(t, err.Error(), `不是合法的 host:port —— 得到 "127.0.0.1"`)
  }

  func TestValidateUnknownTagFallsBackToGenericMessage(t *testing.T) {
  	cfg := fallbackValidateConfig{Ratio: 5}
  	err := Validate(&cfg, "plugins.sampler")
  	require.Error(t, err)
  	require.Contains(t, err.Error(), "未通过校验规则")
  }

  func TestValidateOneofMessage(t *testing.T) {
  	cfg := oneofValidateConfig{Mode: "weekly"}
  	err := Validate(&cfg, "log.file")
  	require.Error(t, err)
  	require.Contains(t, err.Error(), "必须是")
  	require.Contains(t, err.Error(), `"weekly"`)
  }

  func TestValidateNestedStructPath(t *testing.T) {
  	cfg := nestedValidateConfig{Pool: poolLeaf{MaxOpenConn: 0}}
  	err := Validate(&cfg, "plugins.gorm.default")
  	require.Error(t, err)
  	require.Contains(t, err.Error(), "plugins.gorm.default.pool.max_open_conn")
  }

  func TestValidateMultipleViolationsOrderedAndAligned(t *testing.T) {
  	cfg := multiFieldConfig{}
  	err := Validate(&cfg, "plugins.gorm.readonly")
  	require.Error(t, err)
  	var ve *ValidationError
  	require.ErrorAs(t, err, &ve)
  	require.Len(t, ve.Lines, 2)
  	require.Contains(t, err.Error(), "plugins.gorm.readonly.dsn")
  	require.Contains(t, err.Error(), "plugins.gorm.readonly.addr")
  }

  func TestValidationErrorAppendMergesAndRealigns(t *testing.T) {
  	cfg1 := gormValidateConfig{}
  	err1 := Validate(&cfg1, "plugins.gorm.readonly")
  	var e1 *ValidationError
  	require.ErrorAs(t, err1, &e1)

  	cfg2 := redisValidateConfig{Addr: "127.0.0.1"}
  	err2 := Validate(&cfg2, "plugins.redis.default")
  	var e2 *ValidationError
  	require.ErrorAs(t, err2, &e2)

  	e1.Append(e2)
  	require.Len(t, e1.Lines, 2)
  	require.Contains(t, e1.Error(), "plugins.gorm.readonly.dsn")
  	require.Contains(t, e1.Error(), "plugins.redis.default.addr")
  }

  func TestValidatePassesWithNoViolations(t *testing.T) {
  	cfg := gormValidateConfig{DSN: "user:pass@/db"}
  	err := Validate(&cfg, "plugins.gorm.default")
  	require.NoError(t, err)
  }
  ```

  ```go
  // config_test.go
  package xbc

  import (
  	"testing"
  	"time"

  	"github.com/knadh/koanf/providers/confmap"
  	"github.com/knadh/koanf/v2"
  	"github.com/stretchr/testify/require"

  	"github.com/xbcio/xbc/log"
  )

  func TestServerConfigDefaults(t *testing.T) {
  	c := &Config{k: koanf.New(".")}
  	var sc ServerConfig
  	require.NoError(t, c.Unmarshal("server", &sc))

  	require.Equal(t, ":8080", sc.Addr)
  	require.Equal(t, "/", sc.BasePath)
  	require.Equal(t, 10*time.Second, sc.ReadTimeout)
  	require.Equal(t, 30*time.Second, sc.WriteTimeout, "裁决 R10：write_timeout 是本计划补的字段，默认 30s")
  	require.Equal(t, 30*time.Second, sc.ShutdownTimeout)
  }

  func TestConfigUnmarshalAppliesXBCEnvPrefix(t *testing.T) {
  	t.Setenv("XBC_SERVER_ADDR", ":9999")
  	c := &Config{k: koanf.New(".")}

  	var sc ServerConfig
  	require.NoError(t, c.Unmarshal("server", &sc))
  	require.Equal(t, ":9999", sc.Addr)
  }

  func TestConfigUnmarshalOnUninitializedConfigIsError(t *testing.T) {
  	var c *Config
  	var sc ServerConfig
  	err := c.Unmarshal("server", &sc)
  	require.Error(t, err, "Config 还没被 loadConfig 初始化时调用 Unmarshal 必须报错，不能拿着 nil 的 koanf 去 panic")
  }

  func TestConfigGetExistsSub(t *testing.T) {
  	k := koanf.New(".")
  	require.NoError(t, k.Load(confmap.Provider(map[string]any{
  		"app": map[string]any{"feature_x": true},
  	}, ""), nil))
  	c := &Config{k: k}

  	require.True(t, c.Exists("app.feature_x"))
  	require.Equal(t, true, c.Get("app.feature_x"))
  	require.Equal(t, map[string]any{"feature_x": true}, c.Sub("app"))
  }

  func TestConfigSubOnAbsentPathReturnsNil(t *testing.T) {
  	c := &Config{k: koanf.New(".")}
  	require.Nil(t, c.Sub("does.not.exist"))
  }

  func TestConfigSubOnScalarPathReturnsNil(t *testing.T) {
  	k := koanf.New(".")
  	require.NoError(t, k.Load(confmap.Provider(map[string]any{"server": map[string]any{"addr": ":8080"}}, ""), nil))
  	c := &Config{k: k}
  	require.Nil(t, c.Sub("server.addr"), "标量路径不是 map，Sub 应返回 nil 而不是 panic")
  }

  func TestBindLogFromYAMLInitializesLogger(t *testing.T) {
  	k := koanf.New(".")
  	require.NoError(t, k.Load(confmap.Provider(map[string]any{
  		"log": map[string]any{"level": "warn"},
  	}, ""), nil))

  	bound, err := bindLog(k, defaultEnvPrefix)
  	require.NoError(t, err)
  	require.Equal(t, "warn", bound.Level, "bindLog 要把绑定后的 log.Config 交回去，供 Config.Log 保存")
  	require.False(t, log.L().Enabled(log.InfoLevel), "level: warn 生效后，Info 级别应当被关闭")
  	require.True(t, log.L().Enabled(log.WarnLevel))
  }

  func TestLoadConfigWithoutFileUsesDefaults(t *testing.T) {
  	a := &App{}
  	require.NoError(t, a.loadConfig(conf.Options{EnvPrefix: defaultEnvPrefix}))
  	require.NotNil(t, a.cfg, "loadConfig 必须把结果挂到 App.cfg 上，后续九个阶段全靠它")
  	require.Equal(t, ":8080", a.cfg.Server.Addr, "没有配置文件时 server 段应当全走 default tag")
  	require.Equal(t, 30*time.Second, a.cfg.Server.WriteTimeout)
  	require.True(t, a.cfg.Exists("server.addr"), "Config.k 必须被填上，否则 Exists/Sub 全瞎")
  }

  func TestLoadConfigRejectsMissingExplicitFile(t *testing.T) {
  	a := &App{}
  	err := a.loadConfig(conf.Options{File: "不存在的.yml", EnvPrefix: defaultEnvPrefix})
  	require.Error(t, err, "显式 --config 指到不存在的文件必须报错（裁决 R8）")
  }

  func TestLoadConfigReportsServerValidationError(t *testing.T) {
  	dir := t.TempDir()
  	path := filepath.Join(dir, "application.yml")
  	require.NoError(t, os.WriteFile(path, []byte("server:\n  read_timeout: 不是时长\n"), 0o600))

  	a := &App{}
  	err := a.loadConfig(conf.Options{File: path, EnvPrefix: defaultEnvPrefix})
  	require.Error(t, err, "server 段绑定失败要一路冒泡到 loadConfig 的调用方")
  }
  ```

  测试文件顶部相应补上 `os`、`path/filepath`、`time` 与 `github.com/xbcio/xbc/internal/conf` 的 import。

- [ ] **Step 2: 运行测试，确认因缺少实现而失败**

  ```bash
  go test ./internal/conf/... -run TestValidate -v
  go test . -run TestConfig -v
  go test . -run TestServerConfigDefaults -v
  go test . -run TestBindLogFromYAMLInitializesLogger -v
  go test . -run TestLoadConfig -v
  ```

  预期失败原因：`internal/conf/validate.go` 不存在，`internal/conf` 包报 `undefined: Validate`、`undefined: ValidationError`；根包的 `config.go` 不存在，报 `undefined: Config`、`undefined: ServerConfig`、`undefined: bindLog`、`undefined: (*App).loadConfig`、`a.cfg undefined`、`unknown field k in struct literal`。

- [ ] **Step 3: 实现 internal/conf/validate.go**

  ```go
  // internal/conf/validate.go
  package conf

  import (
  	"fmt"
  	"reflect"
  	"strconv"
  	"strings"

  	"github.com/go-playground/validator/v10"
  )

  // ValidationError carries one line per violation, already rendered but not
  // yet column-aligned -- alignment happens lazily in Error() so that Append
  // (merging violations collected from several plugins) still produces a
  // correctly aligned block.
  type ValidationError struct {
  	Lines []string // each entry is "<path>\t<message>"
  }

  func (e *ValidationError) Error() string {
  	if len(e.Lines) == 0 {
  		return "xbc: 配置错误"
  	}

  	paths := make([]string, len(e.Lines))
  	msgs := make([]string, len(e.Lines))
  	width := 0
  	for i, line := range e.Lines {
  		parts := strings.SplitN(line, "\t", 2)
  		paths[i] = parts[0]
  		if len(parts) > 1 {
  			msgs[i] = parts[1]
  		}
  		if n := len([]rune(parts[0])); n > width {
  			width = n
  		}
  	}

  	var b strings.Builder
  	b.WriteString("xbc: 配置错误")
  	for i := range paths {
  		pad := width - len([]rune(paths[i])) + 2
  		b.WriteString("\n  ")
  		b.WriteString(paths[i])
  		b.WriteString(strings.Repeat(" ", pad))
  		b.WriteString(msgs[i])
  	}
  	return b.String()
  }

  // Append merges other's violations into e. Alignment is recomputed on the
  // next Error() call, over the full merged Lines.
  func (e *ValidationError) Append(other *ValidationError) {
  	if other == nil {
  		return
  	}
  	e.Lines = append(e.Lines, other.Lines...)
  }

  // Validate runs go-playground/validator over out and renders every violation
  // as a Chinese line prefixed with its full config path (path + the field's
  // own yaml-tag path, dot-joined).
  func Validate(out any, path string) error {
  	v := validator.New()
  	err := v.Struct(out)
  	if err == nil {
  		return nil
  	}

  	verrs, ok := err.(validator.ValidationErrors)
  	if !ok {
  		return fmt.Errorf("xbc: 配置校验失败：%w", err)
  	}

  	root := reflect.TypeOf(out)
  	ve := &ValidationError{}
  	for _, fe := range verrs {
  		p := joinPath(path, yamlPath(root, fe.StructNamespace()))
  		ve.Lines = append(ve.Lines, p+"\t"+renderViolation(fe))
  	}
  	return ve
  }

  func joinPath(path, sub string) string {
  	switch {
  	case path == "":
  		return sub
  	case sub == "":
  		return path
  	default:
  		return path + "." + sub
  	}
  }

  // yamlPath translates validator's Go-field StructNamespace (e.g.
  // "GormConfig.Pool.MaxOpenConn") into the yaml-tag path (e.g.
  // "pool.max_open_conn") by walking the same struct schema Leaves/walkLeaves
  // uses -- both must agree on the field-name-to-tag-name mapping, or the
  // rendered error path won't match what's actually in the config file.
  func yamlPath(root reflect.Type, structNamespace string) string {
  	for root.Kind() == reflect.Ptr {
  		root = root.Elem()
  	}

  	segments := strings.Split(structNamespace, ".")
  	if len(segments) <= 1 {
  		return ""
  	}
  	segments = segments[1:] // drop the leading struct type name

  	t := root
  	out := make([]string, 0, len(segments))
  	for _, seg := range segments {
  		name := seg
  		if idx := strings.IndexByte(seg, '['); idx >= 0 {
  			name = seg[:idx] // strip a slice/map index suffix, e.g. "Items[0]"
  		}
  		f, ok := t.FieldByName(name)
  		if !ok {
  			return strings.ToLower(strings.Join(segments, "."))
  		}
  		out = append(out, yamlTagName(f))

  		ft := f.Type
  		for ft.Kind() == reflect.Ptr {
  			ft = ft.Elem()
  		}
  		t = ft
  	}
  	return strings.Join(out, ".")
  }

  func renderViolation(fe validator.FieldError) string {
  	switch fe.Tag() {
  	case "required":
  		return "必填项缺失"
  	case "min":
  		return fmt.Sprintf("不能小于 %s，得到 %s", fe.Param(), quoteValue(fe.Value()))
  	case "max":
  		return fmt.Sprintf("不能大于 %s，得到 %s", fe.Param(), quoteValue(fe.Value()))
  	case "hostname_port":
  		return fmt.Sprintf("不是合法的 host:port —— 得到 %s", quoteValue(fe.Value()))
  	case "oneof":
  		return fmt.Sprintf("必须是 %s 之一，得到 %s", fe.Param(), quoteValue(fe.Value()))
  	default:
  		return fmt.Sprintf("未通过校验规则 %q，得到 %s", fe.Tag(), quoteValue(fe.Value()))
  	}
  }

  func quoteValue(v any) string {
  	return strconv.Quote(fmt.Sprint(v))
  }
  ```

  `renderViolation` 里 `hostname_port` 分支的措辞是逐字对齐 API 契约 §13 的样例（`不是合法的 host:port —— 得到 "127.0.0.1"`），`required` 分支同理（`必填项缺失`）——这两条不是"看起来差不多就行"，是测试要断言的逐字文案，必须原样照抄，不能按自己的措辞习惯改写。

- [ ] **Step 4: 实现根包 config.go**

  ```go
  // config.go
  package xbc

  import (
  	"fmt"
  	"time"

  	"github.com/knadh/koanf/v2"

  	"github.com/xbcio/xbc/internal/conf"
  	"github.com/xbcio/xbc/log"
  )

  const defaultEnvPrefix = "XBC_"

  // Config is the root configuration surface handed to every plugin via
  // Context.Config(). Server and Log are bound eagerly during stage 1;
  // everything else (plugins.*, app.*) is reached through k via Get/Exists/Sub.
  type Config struct {
  	Server ServerConfig `yaml:"server"`
  	Log    log.Config   `yaml:"log"`

  	k *koanf.Koanf
  }

  // ServerConfig configures the HTTP server assembled in stage 7.
  //
  // WriteTimeout is not in the spec's illustrative §6.2 snippet -- it is added
  // by this plan (ruling R10). A read-only timeout leaves the write side open
  // to a slow-write style attack surface.
  type ServerConfig struct {
  	Addr            string        `yaml:"addr"             default:":8080"`
  	BasePath        string        `yaml:"base_path"        default:"/"`
  	ReadTimeout     time.Duration `yaml:"read_timeout"     default:"10s"`
  	WriteTimeout    time.Duration `yaml:"write_timeout"    default:"30s"`
  	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"30s"`
  	AutoMigrate     bool          `yaml:"auto_migrate"`
  }

  // Unmarshal binds a config subtree into out, applying the full stage-3 chain
  // (unmarshal -> ENV -> default). It does NOT run validate -- callers that
  // need validation call conf.Validate separately (see stage_config.go, Task 9).
  func (c *Config) Unmarshal(path string, out any) error {
  	if c == nil || c.k == nil {
  		return fmt.Errorf("xbc: 配置尚未加载，无法绑定 %s", displayConfigPath(path))
  	}
  	return conf.Bind(c.k, path, out, defaultEnvPrefix)
  }

  func displayConfigPath(path string) string {
  	if path == "" {
  		return "(root)"
  	}
  	return path
  }

  // Get returns the raw value at path, or nil when absent.
  func (c *Config) Get(path string) any {
  	if c == nil || c.k == nil {
  		return nil
  	}
  	return c.k.Get(path)
  }

  // Exists reports whether path was set by any of the loaded layers.
  func (c *Config) Exists(path string) bool {
  	if c == nil || c.k == nil {
  		return false
  	}
  	return c.k.Exists(path)
  }

  // Sub returns the map at path, or nil when path is absent or not a map.
  func (c *Config) Sub(path string) map[string]any {
  	if c == nil || c.k == nil {
  		return nil
  	}
  	m, ok := c.k.Get(path).(map[string]any)
  	if !ok {
  		return nil
  	}
  	return m
  }

  // bindLog binds the "log" subtree onto a fresh log.DefaultConfig(), wires it
  // into the global logger, and hands the bound config back so loadConfig can
  // keep it on Config.Log for anything that wants to read what log is running.
  //
  // Ordering constraint: Bind must run BEFORE log.Init. Bind is the only step
  // that applies the file/ENV/default chain (including ruling R2's schema-driven
  // ENV overlay); log.Init only normalizes and assembles zap cores from
  // whatever Config it is handed -- it has no idea a "log" config section, ENV
  // vars, or defaults even exist. Calling log.Init first and Bind second would
  // silently discard every override Bind was supposed to apply.
  func bindLog(k *koanf.Koanf, envPrefix string) (log.Config, error) {
  	cfg := log.DefaultConfig()
  	if err := conf.Bind(k, "log", &cfg, envPrefix); err != nil {
  		return cfg, fmt.Errorf("xbc: 绑定 log 配置失败：%w", err)
  	}
  	if err := log.Init(cfg); err != nil {
  		return cfg, fmt.Errorf("xbc: 初始化日志失败：%w", err)
  	}
  	return cfg, nil
  }

  // loadConfig is assembly stage 1: locate and merge the config sources, bring
  // the logger up, bind and validate the framework's own server section, and
  // park the result on App for the nine stages that follow.
  //
  // Log comes up before server is bound on purpose. Everything after this line
  // -- including the error paths of the very next statement -- wants a working
  // logger, and the log section is the one section that can be bound without
  // anything else already being in place.
  func (a *App) loadConfig(opts conf.Options) error {
  	if opts.EnvPrefix == "" {
  		opts.EnvPrefix = defaultEnvPrefix
  	}
  	k, err := conf.Load(opts)
  	if err != nil {
  		return err
  	}

  	cfg := &Config{k: k}
  	if cfg.Log, err = bindLog(k, opts.EnvPrefix); err != nil {
  		return err
  	}
  	if err := conf.Bind(k, "server", &cfg.Server, opts.EnvPrefix); err != nil {
  		return fmt.Errorf("xbc: 绑定 server 配置失败：%w", err)
  	}
  	if err := conf.Validate(&cfg.Server, "server"); err != nil {
  		return err
  	}

  	a.cfg = cfg
  	return nil
  }
  ```

  `bindLog` 没有出现在 `Config` 的方法集里，是一个包级函数——它服务的是 `loadConfig` 里"配置刚加载完、日志系统还没起来"这个特定时间点，不是长期挂在 `Config` 上供插件调用的 API，插件拿到的日志入口始终是 `Context.Log()`（Task 1 已定），不是 `Config`。

  `loadConfig` 是 `kernel-api.md` §12 十个阶段函数里的第一个，也是唯一一个不接 `[]*instance` 的——它跑在展开之前，此时还没有实例。它落在 `config.go` 而不是单独开一个 `stage_config.go`，是因为它做的三件事（`conf.Load`、`bindLog`、绑定 `server` 段）全都只碰 `Config` 自己的字段，跟 `config.go` 里其余代码共享同一套未导出符号（`defaultEnvPrefix`、`bindLog`、`Config.k`）；拆到别的文件只会让读者为了看懂一个函数在两个文件之间来回跳。注意 `stage_config.go`（Task 9）是**阶段 3** `bindConfigs` 的家，逐实例绑定插件配置，跟这里不是一回事，别混。

  最后用 `Edit` 往 `xbc.go` 的 `type App struct { ... }` 里补上 `cfg` 字段（裁决 R12 里挂在本 task 名下的那一个），加在 `entries` 那一行后面：

  ```go
  	// cfg is the merged configuration, filled by stage 1's loadConfig and
  	// read by every stage after it. It stays nil until then, so anything
  	// that touches it before stage 1 is a pipeline-ordering bug, not a
  	// missing nil check.
  	cfg *Config
  ```

  `Config` 就定义在同一个包里，这一步不需要给 `xbc.go` 新增任何 import。

- [ ] **Step 5: 运行验收三件套确认全部通过**

  ```bash
  gofmt -l . && go vet ./... && go test ./... -count=1
  ```

- [ ] **Step 6: 提交**

  ```bash
  git add internal/conf/validate.go internal/conf/validate_test.go config.go config_test.go xbc.go
  git commit -m "feat(conf): 实现校验中文文案与路径还原，接入根包 Config/ServerConfig 与阶段 1 loadConfig"
  ```
### Task 7: `internal/inject` —— 一次 tag 扫描得出依赖图两端

**Files:**
- Create: `internal/inject/scan.go`
- Test: `internal/inject/scan_test.go`

**Interfaces:**
- Consumes: 无（`internal/inject` 是叶子包，只 import 标准库 `fmt`/`reflect`/`strings`；硬约束——不得 import 根包 `github.com/xbcio/xbc`，也不得 import 其他 `internal/*`）
- Produces:
  - `type Kind int`，常量 `KindInject Kind = iota`、`KindProvide`
  - `type FieldSpec struct { Index int; Name string; Type reflect.Type; Instance string; Optional bool; Kind Kind }`
  - `func Scan(v any) ([]FieldSpec, error)`
  - `func Set(v any, spec FieldSpec, val any) error`
  - `func IsZero(v any, spec FieldSpec) (bool, error)`
  - `func Value(v any, spec FieldSpec) (any, error)`

**这个包是整个内核的支点。** 依赖图有两端：谁**要**什么，谁**给**什么。两端都必须在阶段 4 静态可知，否则连不出边。「要什么」好办——`inject` tag 是声明式的，字段类型摆在那里，反射一扫就有。**「给什么」才是难点**：如果产物只在 `Init` 里运行时调 `Provide`，阶段 4 排序的时候这次调用根本还没发生，框架无从知道 `*gorm.DB` 出自谁手，拓扑排序也就无法给它连边。于是产物也必须走同一套声明方式——`xbc:"provide"` 与 `xbc:"inject"` 语法完全对称，一次 `Scan` 同时扫出依赖图的两端，插件作者不需要为「谁提供什么」多写一个方法。这也是为什么 `FieldSpec.Kind` 只有两个值：这个包不关心「怎么用」这个值，只负责把「一个字段是要还是给」这件事从 tag 里读出来，交给根包的阶段 4/5 去连图、去收割。

**未导出字段带 `xbc` tag 必须报错，不能静默跳过。** `reflect.Value.Field(i)` 拿到的未导出字段值 `CanSet()` 恒为 `false`，`Set` 从物理上设不进去。如果 `Scan` 对这种字段选择「安静跳过」，插件作者会以为自己写对了 tag（毕竟没报错），实际上这个字段永远拿不到注入值，等到运行时用到它才会现出 nil——那时候上下文早就丢了，排查成本比在扫描阶段直接报错高一个量级。「静默跳过」在这里等于埋雷，所以未导出字段带 tag 是唯一一个「格式合法但仍然报错」的分支。

**只扫顶层导出字段，不递归进嵌套结构体。** 一是因为 `xbc.Base` 作为匿名嵌入字段本来就没有 `xbc` tag，天然落进「无 tag 跳过」分支，不需要特殊处理；二是递归扫描会让「一个字段的归属」变得模糊——如果插件在自己的业务结构体里也用了 `xbc` 这个 tag 名字（哪怕是无意的），递归扫描会把八字不相关的内层字段也纳入依赖图,这种隐式行为很难在阅出错时定位。只扫顶层，语义边界清楚：`xbc` tag 只对直属插件结构体的字段生效。

**`Set` 用 `AssignableTo` 做类型检查而不是直接赋值 panic。** 装配管线要跑一整条流水线，注入的值来自注册表在运行时按类型匹配出来的 `any`；如果类型不匹配就 panic，一个插件的接线错误会直接打爆整个启动流程,错误信息还是一条毫无上下文的 runtime panic。检查后返回 `error`，上层能把「插件 X 的字段 Y 类型不匹配」这种有意义的信息包进统一的错误链路。

- [ ] **Step 1: 写失败测试**

创建 `internal/inject/scan_test.go`：

```go
package inject

import (
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBase stands in for xbc.Base without importing the root package --
// internal/inject must never know that type exists. It is anonymous and
// carries no xbc tag, so Scan must let it fall through the "no tag" branch
// exactly like the real Base does.
type fakeBase struct{ name string }

type fourFormsPlugin struct {
	fakeBase
	Default  *int `xbc:"inject"`
	Named    *int `xbc:"inject,name=readonly"`
	Optional *int `xbc:"inject,optional"`
	Both     *int `xbc:"inject,name=ro,optional"`
	BothRev  *int `xbc:"inject,optional,name=ro"`
	Untagged *int
	Skipped  *int `xbc:"-"`
}

func TestScanFourInjectForms(t *testing.T) {
	specs, err := Scan(&fourFormsPlugin{})
	require.NoError(t, err)

	byName := make(map[string]FieldSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}

	require.Contains(t, byName, "Default")
	assert.Equal(t, KindInject, byName["Default"].Kind)
	assert.Equal(t, "", byName["Default"].Instance, "裸 inject 的实例名是空串，代表 default")
	assert.False(t, byName["Default"].Optional)

	require.Contains(t, byName, "Named")
	assert.Equal(t, "readonly", byName["Named"].Instance)

	require.Contains(t, byName, "Optional")
	assert.True(t, byName["Optional"].Optional)

	require.Contains(t, byName, "Both")
	assert.Equal(t, "ro", byName["Both"].Instance)
	assert.True(t, byName["Both"].Optional)

	require.Contains(t, byName, "BothRev")
	assert.Equal(t, "ro", byName["BothRev"].Instance, "选项顺序不应影响解析结果")
	assert.True(t, byName["BothRev"].Optional)

	assert.NotContains(t, byName, "Untagged", "无 tag 字段必须被跳过")
	assert.NotContains(t, byName, "Skipped", `xbc:"-" 必须被跳过`)
	assert.NotContains(t, byName, "fakeBase", "嵌入的匿名字段没有 xbc tag，必须被跳过")
}

type provideOnlyPlugin struct {
	fakeBase
	Svc *int `xbc:"provide"`
}

func TestScanProvide(t *testing.T) {
	specs, err := Scan(&provideOnlyPlugin{})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, KindProvide, specs[0].Kind)
	assert.Equal(t, "", specs[0].Instance, "产物的实例名恒为空，由插件自己的实例名决定，不能来自 tag")
}

type mixedPlugin struct {
	fakeBase
	DB  *int `xbc:"inject"`
	Svc *int `xbc:"provide"`
}

func TestScanMixedInjectAndProvide(t *testing.T) {
	specs, err := Scan(&mixedPlugin{})
	require.NoError(t, err)
	require.Len(t, specs, 2)

	kinds := make(map[string]Kind, 2)
	for _, s := range specs {
		kinds[s.Name] = s.Kind
	}
	assert.Equal(t, KindInject, kinds["DB"])
	assert.Equal(t, KindProvide, kinds["Svc"])
}

func TestScanIndexAddressesFieldDirectly(t *testing.T) {
	p := &mixedPlugin{}
	specs, err := Scan(p)
	require.NoError(t, err)

	rt := reflect.TypeOf(p).Elem()
	for _, s := range specs {
		assert.Equal(t, s.Name, rt.Field(s.Index).Name, "Index 必须能直接定位到同一个字段，不需要二次查找")
	}
}

type provideNamedPlugin struct {
	X *int `xbc:"provide,name=x"`
}

func TestScanProvideWithNameErrors(t *testing.T) {
	_, err := Scan(&provideNamedPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不能指定 name", "产物实例名来自插件自己，不能由 tag 指定")
}

type provideOptionalPlugin struct {
	X *int `xbc:"provide,optional"`
}

func TestScanProvideWithOptionalErrors(t *testing.T) {
	_, err := Scan(&provideOptionalPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "没有可选一说")
}

type unknownActionPlugin struct {
	X *int `xbc:"injct"`
}

func TestScanUnknownActionErrorsAndListsValidOnes(t *testing.T) {
	_, err := Scan(&unknownActionPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"injct"`)
	assert.Contains(t, err.Error(), "inject")
	assert.Contains(t, err.Error(), "provide")
}

type unknownOptionPlugin struct {
	X *int `xbc:"inject,nmae=x"`
}

func TestScanUnknownOptionErrors(t *testing.T) {
	_, err := Scan(&unknownOptionPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nmae")
}

type unexportedTaggedPlugin struct {
	x int `xbc:"inject"` //nolint:unused // deliberately an unexported field, to test the error-reporting branch
}

func TestScanUnexportedFieldWithTagErrors(t *testing.T) {
	_, err := Scan(&unexportedTaggedPlugin{})
	require.Error(t, err, "未导出字段带 xbc tag 必须报错——反射设不进去，静默跳过等于埋雷")
	assert.Contains(t, err.Error(), "未导出")
}

func TestScanRejectsNonPointer(t *testing.T) {
	_, err := Scan(mixedPlugin{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "指针")
}

func TestScanRejectsNilPointer(t *testing.T) {
	var p *mixedPlugin
	_, err := Scan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestScanRejectsPointerToNonStruct(t *testing.T) {
	n := 1
	_, err := Scan(&n)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "结构体")
}

type setPlugin struct {
	DB   *int
	Name string
}

func TestSetAssignsCompatibleValue(t *testing.T) {
	p := &setPlugin{}
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}
	n := 42

	require.NoError(t, Set(p, spec, &n))
	assert.Equal(t, &n, p.DB)
}

func TestSetRejectsIncompatibleType(t *testing.T) {
	p := &setPlugin{}
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}

	err := Set(p, spec, "不是 *int")
	require.Error(t, err, "类型不匹配必须报错而不是 panic")
	assert.Contains(t, err.Error(), "类型不匹配")
}

func TestSetAcceptsNilForNilableKinds(t *testing.T) {
	p := &setPlugin{DB: new(int)}
	spec := FieldSpec{Index: 0, Name: "DB", Type: reflect.TypeOf((*int)(nil))}

	require.NoError(t, Set(p, spec, nil))
	assert.Nil(t, p.DB)
}

type zeroCheckPlugin struct {
	Ptr    *int
	Iface  error
	Slice  []string
	Struct struct{ A int }
}

func TestIsZeroCoversFourKinds(t *testing.T) {
	p := &zeroCheckPlugin{}
	specs := []FieldSpec{
		{Index: 0, Name: "Ptr", Type: reflect.TypeOf((*int)(nil))},
		{Index: 1, Name: "Iface", Type: reflect.TypeOf((*error)(nil)).Elem()},
		{Index: 2, Name: "Slice", Type: reflect.TypeOf([]string(nil))},
		{Index: 3, Name: "Struct", Type: reflect.TypeOf(struct{ A int }{})},
	}

	for _, s := range specs {
		zero, err := IsZero(p, s)
		require.NoError(t, err)
		assert.True(t, zero, "字段 %s 初始必须是零值", s.Name)
	}

	n := 1
	p.Ptr = &n
	p.Iface = errors.New("x")
	p.Slice = []string{"a"}
	p.Struct.A = 1

	for _, s := range specs {
		zero, err := IsZero(p, s)
		require.NoError(t, err)
		assert.False(t, zero, "字段 %s 赋值后不应仍是零值", s.Name)
	}
}

func TestValueReturnsCurrentValue(t *testing.T) {
	p := &setPlugin{Name: "cors"}
	spec := FieldSpec{Index: 1, Name: "Name", Type: reflect.TypeOf("")}

	v, err := Value(p, spec)
	require.NoError(t, err)
	assert.Equal(t, "cors", v)
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/inject/... -v
```

Expected: 编译失败，`undefined: Scan` / `undefined: FieldSpec` / `undefined: KindInject` 等——`scan.go` 还不存在。

- [ ] **Step 3: 写实现**

创建 `internal/inject/scan.go`：

```go
// Package inject scans xbc struct tags off a plugin's fields via reflection.
//
// It knows nothing about xbc types -- it only understands reflect.Type,
// reflect.Value, and its own FieldSpec. This keeps the package independently
// testable and lets the root package build the dependency graph on top of it
// without creating an import cycle.
package inject

import (
	"fmt"
	"reflect"
	"strings"
)

// Kind distinguishes the two ends of the dependency graph a tagged field
// describes: what a plugin wants (KindInject) versus what it produces
// (KindProvide). A single Scan call over one plugin's fields yields both
// ends at once, which is exactly why the graph can be built statically
// before any plugin's Init has run.
type Kind int

const (
	KindInject Kind = iota
	KindProvide
)

// FieldSpec describes one xbc-tagged field found on a plugin struct.
type FieldSpec struct {
	Index    int          // top-level field index; usable directly with reflect.Value.Field
	Name     string       // Go field name, for error copy
	Type     reflect.Type // field type, used for registry lookups and Set's type check
	Instance string       // "" means default; always "" for KindProvide
	Optional bool         // only meaningful for KindInject
	Kind     Kind
}

// Scan reads the xbc struct tags off a plugin value.
//
// Only exported top-level fields are considered; embedded structs (e.g. a
// plugin's embedded Base) are not recursed into -- an embedded field with no
// xbc tag of its own simply falls through the "no tag" branch below, just
// like any other untagged field.
//
// v must be a non-nil pointer to a struct.
func Scan(v any) ([]FieldSpec, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("inject: Scan 需要指向结构体的指针，收到 %T", v)
	}
	if rv.IsNil() {
		return nil, fmt.Errorf("inject: Scan 收到 nil 指针")
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return nil, fmt.Errorf("inject: Scan 需要指向结构体的指针，收到指向 %s 的指针", elem.Kind())
	}

	t := elem.Type()
	var specs []FieldSpec
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		tagVal, ok := f.Tag.Lookup("xbc")
		if !ok {
			// No tag: not our business. This is exactly the branch an
			// embedded Base (or any other untagged field) falls into.
			continue
		}
		if !f.IsExported() {
			// Tag.Lookup works regardless of exportedness, but reflect can
			// never Set an unexported field. Skipping silently here would
			// mean the tag is a lie the author never finds out about until
			// something downstream sees an unexplained nil.
			return nil, fmt.Errorf("inject: 字段 %s 未导出但带有 xbc tag，反射无法为它赋值", f.Name)
		}
		if tagVal == "-" {
			continue
		}

		spec, err := parseTag(f, tagVal)
		if err != nil {
			return nil, err
		}
		spec.Index = i
		specs = append(specs, spec)
	}
	return specs, nil
}

// parseTag parses one field's xbc tag value, e.g. "inject,name=ro,optional".
func parseTag(f reflect.StructField, tag string) (FieldSpec, error) {
	parts := strings.Split(tag, ",")
	action := parts[0]

	spec := FieldSpec{Name: f.Name, Type: f.Type}
	switch action {
	case "inject":
		spec.Kind = KindInject
	case "provide":
		spec.Kind = KindProvide
	default:
		return FieldSpec{}, fmt.Errorf(
			"inject: 字段 %s 的 xbc tag 动作 %q 未知，合法取值：inject、provide、-", f.Name, action)
	}

	for _, opt := range parts[1:] {
		key, val, hasVal := strings.Cut(opt, "=")
		switch key {
		case "name":
			if !hasVal {
				return FieldSpec{}, fmt.Errorf(
					"inject: 字段 %s 的 xbc tag 选项 %q 缺少值，期望 name=<实例名>", f.Name, opt)
			}
			if spec.Kind == KindProvide {
				return FieldSpec{}, fmt.Errorf(
					"inject: 字段 %s 是 provide，不能指定 name —— 产物实例名来自插件自己，不能由 tag 指定", f.Name)
			}
			spec.Instance = val
		case "optional":
			if hasVal {
				return FieldSpec{}, fmt.Errorf(
					"inject: 字段 %s 的 xbc tag 选项 %q 不接受值", f.Name, opt)
			}
			if spec.Kind == KindProvide {
				return FieldSpec{}, fmt.Errorf(
					"inject: 字段 %s 是 provide，产物没有可选一说，optional 无意义", f.Name)
			}
			spec.Optional = true
		default:
			return FieldSpec{}, fmt.Errorf(
				"inject: 字段 %s 的 xbc tag 选项 %q 未知，合法取值：name、optional", f.Name, key)
		}
	}
	return spec, nil
}

// Set assigns val to the field described by spec on v.
//
// The assignment is type-checked first via AssignableTo: an incompatible val
// reports an error instead of panicking, because a single misbehaving plugin
// panicking here would take down the whole assembly pipeline for everyone
// else riding in the same process.
func Set(v any, spec FieldSpec, val any) error {
	fv, err := fieldValue(v, spec)
	if err != nil {
		return err
	}
	if !fv.CanSet() {
		return fmt.Errorf("inject: 字段 %s 不可设置", spec.Name)
	}

	rv := reflect.ValueOf(val)
	if !rv.IsValid() {
		// val is an untyped nil; only legal for field types that can hold nil.
		switch spec.Type.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
			fv.Set(reflect.Zero(spec.Type))
			return nil
		default:
			return fmt.Errorf("inject: 字段 %s 类型 %s 不能被赋值为 nil", spec.Name, spec.Type)
		}
	}
	if !rv.Type().AssignableTo(spec.Type) {
		return fmt.Errorf("inject: 字段 %s 类型不匹配：期望 %s，得到 %s", spec.Name, spec.Type, rv.Type())
	}
	fv.Set(rv)
	return nil
}

// IsZero reports whether the field described by spec is still its zero value.
func IsZero(v any, spec FieldSpec) (bool, error) {
	fv, err := fieldValue(v, spec)
	if err != nil {
		return false, err
	}
	return fv.IsZero(), nil
}

// Value returns the current value of the field described by spec.
func Value(v any, spec FieldSpec) (any, error) {
	fv, err := fieldValue(v, spec)
	if err != nil {
		return nil, err
	}
	return fv.Interface(), nil
}

// fieldValue resolves spec.Index against v. Set/IsZero/Value can be called
// independently of Scan (as the tests above do, building FieldSpec literals
// by hand), so this cannot assume v was ever passed to Scan -- it re-checks
// the same pointer-to-struct shape Scan enforces.
func fieldValue(v any, spec FieldSpec) (reflect.Value, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return reflect.Value{}, fmt.Errorf("inject: 需要指向结构体的指针，收到 %T", v)
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("inject: 需要指向结构体的指针，收到指向 %s 的指针", elem.Kind())
	}
	if spec.Index < 0 || spec.Index >= elem.NumField() {
		return reflect.Value{}, fmt.Errorf("inject: 字段索引 %d 超出范围", spec.Index)
	}
	return elem.Field(spec.Index), nil
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/inject/... -v
```

Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
git add internal/inject/scan.go internal/inject/scan_test.go
git commit -m "feat(inject): 一次 tag 扫描得出依赖图两端，未导出字段带 tag 直接报错"
```

---

### Task 8: 阶段 2 Expand —— 多实例展开 + 两条注册路径的启用规则 + 孤儿配置节诊断

**Files:**
- Create: `stage_expand.go`
- Test: `stage_expand_test.go`

**Interfaces:**
- Consumes:
  - `Plugin` / `Configurable` / `MultiInstancer` / `Base` / `bindBase`（`plugin.go`，Task 1）
  - `entry` / `source` / `sourceImport` / `sourceRegister` / `App`（`xbc.go`，Task 1）—— `App.entries []entry` 是 Task 1 合并包级 pending `Register` 列表与显式 `(*App).Register` 调用后的结果；`App.cfg *Config` 由 Task 6 追加进结构体、阶段 1 `loadConfig` 写入。**这两个字段名不是本 task 可以自定的**，按裁决 R12 它们是全局约定，读到不一致要回头查是不是前面的 task 少写了，不要就地改名
  - `instance` 结构体定义（`xbc.go`，Task 1）—— 本 task 补充它的 `id()` / `label()` 方法，因为这两个方法的正确性依赖 `expand()` 产出的实例形状，写在一起才有的测
  - `defaultInstance` 包级常量（`xbc.go`，Task 1）
  - `Context`（`context.go` 骨架，Task 1）
  - `Config.Exists` / `Config.Get` / `Config.Sub`（`config.go`，Task 6）
  - `log.L()` / `log.Logger.With`（`xbc/log`，已完成）
- Produces:
  - `func (a *App) expand() ([]*instance, error)`
  - `func (i *instance) id() string`
  - `func (i *instance) label() string`
  - `func isMultiInstance(p Plugin) bool`（unexported 辅助函数，Task 9 会复用）

**启用规则（spec §6.4）分两条路径的理由。** `app.Register(user.New())` 已经是一次明确的意图声明——业务插件恰恰最常见的形态是没有任何配置就该跑起来，还要求在 yml 里补一个空的 `plugins.user: {}` 才肯装配纯属仪式。而 blank import（`_ "xbc/plugins/redis"`）表达的意图强度完全不同：它只是说「这个能力现在可用」，装不装配交给配置决定。这条规则的价值不是理论上的整洁，而是运维上的真实收益——**临时停掉一个基础设施插件只需要注释掉配置文件里的一段 yaml，不用改代码重新编译发布**。框架能分辨这两条路径是因为它们天然带着来源标记（`init()` 走包级 `Register`，`app.Register()` 走实例方法），来源在 `entry.src` 里从注册那一刻就定了，`expand()` 只是照着这张表读。

| 注册方式 | 配置节 | 结果 |
|---|---|---|
| `app.Register(p)` | 不存在 | **启用**，全走默认值 |
| `app.Register(p)` | 存在 | 启用 |
| `app.Register(p)` | `enabled: false` | 关闭 |
| blank import | 不存在 | **不启用** |
| blank import | 存在（哪怕空 `{}`） | 启用 |
| blank import | `enabled: false` | 关闭 |

**多实例由插件显式声明，不自动探测。** 如果框架看到 `plugins.gorm` 下有个子节点长得像一个映射就当它是一个实例，那当某个插件的 `Config` 结构体恰好有个字段自己就叫 `default`（比如一个连接池插件想给"默认策略"起名 `default`）时，探测规则立刻会把这个字段错认成实例名，装配彻底走歪。显式声明（`MultiInstancer.MultiInstance() bool`）把这件事从"猜"变成"读"，没有歧义。

**孤儿配置节致命，不是警告（裁决 R6）。** 这条诊断存在的全部理由就是治「写了配置、忘了 import、静默不生效」这个最常见的翻车场景；如果诊断本身只打一行 warn 就放你过去，那这个 bug 换了个马甲照样能溜过去——用户一样会在生产环境里发现某个配置写了却没生效,只是现在多了一行淹没在日志里的警告。所以这里选择启动中止，而不是"温柔提醒"。诊断只扫描 `plugins.*` 命名空间下的键；`server.` / `log.` / `app.` 三个顶层命名空间是保留字（前两个框架自己解析，第三个框架完全不碰），从来不会被当成候选插件名——它们跟 `plugins.*` 根本不是同一层级，天然豁免，不需要额外的排除列表。

**R7 的克隆规则：显式 Register 且最终恰好展开 1 个实例才复用原型，其余一律 `reflect.New` 出零值。** `app.Register(user.New())` 传进来的原型可能带着构造参数状态（一个已经算好的哈希盐、一个预热过的连接池配置），这是业务插件最常见也最合理的写法。但多实例展开要产出 N 个彼此独立的实例——浅拷贝结构体会把内部嵌的 `sync.Mutex` 之类的东西也一起拷贝走，`go vet` 会当场报错；深拷贝在通用反射场景下没有正确定义（要拷贝到哪一层？slice 里的指针要不要跟着深拷贝？没有通用答案）。于是规则收紧成：只有「不需要复制」的那一种情况——显式注册且最终只有一个实例——才可以直接把原型交出去；其余场景，多实例插件必须自己保证零值可用，状态只能来自配置。显式 Register 的多实例插件如果展开出 >1 个实例，框架要打一条 warn，不能悄悄把构造参数丢了却什么都不说。

**`id()` / `label()` 的定义与潜在撞车问题。** 按契约：`id()` 对 default 实例省略实例名（`"gorm"`），只有非 default 的具名实例才带方括号（`"gorm[readonly]"`）；`label()` 在此基础上，对多实例插件的 default 实例额外显式标出（`"gorm[default]"`），方便阅读启动日志时一眼分清"这是单实例插件"还是"这是多实例插件的默认实例"。这意味着单实例插件的 `id()` 与多实例插件 default 实例的 `id()` **算出来是同一个字符串**（都是裸插件名）——但这不是一次新的撞车：图节点 id 唯一性的前提本来就是"插件名唯一"，这个前提不管有没有多实例都必须成立，`id()` 省略 default 只是不引入*额外*的撞车面，没有让本来安全的东西变得不安全。真正需要防的是"两个不同 entry 用了同一个插件名"这种非法状态本身——本 task 在 `expand()` 里加一条防御性检查：发现重复插件名直接报错，绝不允许它借着 `id()` 的省略规则悄悄合并成拓扑图里的同一个节点（那样两条本该独立的边会被错误地画到一起，是一个极难排查的隐蔽 bug）。`label()` 需要知道某个实例所属的插件是否为多实例，不必在 `instance` 结构体上加字段——直接对 `i.plugin` 做一次 `MultiInstancer` 类型断言即可，这也是 `isMultiInstance` 存在的原因。

- [ ] **Step 1: 写失败测试**

创建 `stage_expand_test.go`：

```go
package xbc

import (
	"testing"

	koanf "github.com/knadh/koanf/v2"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestConfig builds a *Config directly from a nested map, bypassing file
// I/O entirely -- stage_expand tests only care about the shape of the config
// tree, not about how it got loaded.
func newTestConfig(t *testing.T, data map[string]any) *Config {
	t.Helper()
	k := koanf.New(".")
	if data != nil {
		// delim="" tells confmap the input is already nested, not flat --
		// passing "." here would make it try to re-split top-level keys.
		require.NoError(t, k.Load(confmap.Provider(data, ""), nil))
	}
	return &Config{k: k}
}

// fakeSinglePlugin is a minimal single-instance plugin: it does not implement
// MultiInstancer.
type fakeSinglePlugin struct {
	Base
}

// fakeMultiPlugin declares itself multi-instance.
type fakeMultiPlugin struct {
	Base
}

func (p *fakeMultiPlugin) MultiInstance() bool { return true }

func TestExpandEnableRuleMatrix(t *testing.T) {
	cases := []struct {
		name    string
		src     source
		data    map[string]any // nil means the "plugins" namespace is entirely absent
		wantLen int
	}{
		{
			"register 无配置节 → 启用",
			sourceRegister, nil, 1,
		},
		{
			"register 配置节存在 → 启用",
			sourceRegister,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"x": 1}}},
			1,
		},
		{
			"register enabled:false → 关闭",
			sourceRegister,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"enabled": false}}},
			0,
		},
		{
			"blank import 无配置节 → 不启用",
			sourceImport, nil, 0,
		},
		{
			"blank import 配置节存在（哪怕空）→ 启用",
			sourceImport,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{}}},
			1,
		},
		{
			"blank import enabled:false → 关闭",
			sourceImport,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"enabled": false}}},
			0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{cfg: newTestConfig(t, tc.data)}
			a.entries = []entry{{proto: &fakeSinglePlugin{}, name: "demo", src: tc.src}}

			insts, err := a.expand()
			require.NoError(t, err)
			assert.Len(t, insts, tc.wantLen)
		})
	}
}

func TestExpandMultiInstanceExpandsNamedInstances(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b"},
			},
		},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 2)

	names := map[string]bool{}
	for _, inst := range insts {
		names[inst.instance] = true
		assert.Equal(t, "gorm", inst.name)
	}
	assert.True(t, names["default"])
	assert.True(t, names["readonly"])
}

func TestExpandMultiInstanceMissingSectionDiffersByRegistrationPath(t *testing.T) {
	a := &App{cfg: newTestConfig(t, nil)}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceRegister, multi: true}}
	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1, "显式 Register 且配置节缺失 → 展开出一个 default 实例")
	assert.Equal(t, defaultInstance, insts[0].instance)

	b := &App{cfg: newTestConfig(t, nil)}
	b.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}
	insts2, err := b.expand()
	require.NoError(t, err)
	assert.Empty(t, insts2, "blank import 且配置节缺失 → 不启用")
}

func TestExpandMultiInstanceSingleInstanceDisabled(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b", "enabled": false},
			},
		},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Equal(t, "default", insts[0].instance, "单独一个实例被 enabled:false 关掉，不影响别的实例")
}

func TestExpandMultiInstancePluginLevelDisabledSkipsAll(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"enabled":  false,
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b"},
			},
		},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	assert.Empty(t, insts, "插件级 enabled:false 必须关掉全部实例")
}

func TestExpandMultiInstanceNonMapKeyErrors(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":     map[string]any{"dsn": "a"},
				"max_retries": 3, // a scalar but not "enabled" -- invalid
			},
		},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	_, err := a.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `多实例插件 gorm 的配置节下 "max_retries" 不是实例（实例配置必须是映射）`)
}

func TestExpandOrphanSectionAbortsWithExactMessage(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"kafka": map[string]any{"brokers": "x"},
		},
	})}

	_, err := a.expand()
	require.Error(t, err)
	assert.Equal(t,
		"xbc: plugins.kafka 有配置但无对应插件\n  → 是否忘了 import github.com/xbcio/xbc/plugins/kafka？",
		err.Error())
}

func TestExpandOrphanSectionsAreAllListedAtOnce(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"kafka": map[string]any{"brokers": "x"},
			"mq":    map[string]any{"addr": "y"},
		},
	})}

	_, err := a.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.kafka 有配置但无对应插件", "多个孤儿节要一次全部列出，不能报一个就退")
	assert.Contains(t, err.Error(), "plugins.mq 有配置但无对应插件")
}

func TestExpandReservedNamespacesAreNeverTreatedAsOrphans(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"server":  map[string]any{"addr": ":8080"},
		"log":     map[string]any{"level": "info"},
		"app":     map[string]any{"feature_x": true},
		"plugins": map[string]any{},
	})}

	insts, err := a.expand()
	require.NoError(t, err, "server/log/app 三个命名空间必须豁免于孤儿诊断")
	assert.Empty(t, insts)
}

func TestExpandDuplicatePluginNameErrors(t *testing.T) {
	a := &App{cfg: newTestConfig(t, nil)}
	a.entries = []entry{
		{proto: &fakeSinglePlugin{}, name: "dup", src: sourceRegister},
		{proto: &fakeSinglePlugin{}, name: "dup", src: sourceRegister},
	}
	_, err := a.expand()
	require.Error(t, err, "重复插件名必须报错，否则会在图节点 id 上悄悄合并成同一个节点")
	assert.Contains(t, err.Error(), "dup")
}

func TestExpandR7ReusesPrototypeForSingleRegisteredInstance(t *testing.T) {
	proto := &fakeSinglePlugin{}
	a := &App{cfg: newTestConfig(t, nil)}
	a.entries = []entry{{proto: proto, name: "cors", src: sourceRegister}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Same(t, proto, insts[0].plugin, "显式 Register 且只展开 1 个实例必须复用原型本身，保留构造参数")
}

func TestExpandR7BuildsFreshZeroValueForBlankImport(t *testing.T) {
	proto := &fakeSinglePlugin{}
	a := &App{cfg: newTestConfig(t, map[string]any{"plugins": map[string]any{"cors": map[string]any{}}})}
	a.entries = []entry{{proto: proto, name: "cors", src: sourceImport}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.NotSame(t, proto, insts[0].plugin, "blank import 必须新建零值，不能复用包级 Register 时构造的那个原型")
}

func TestExpandR7MultiInstanceRegisteredWithMultipleInstancesSkipsPrototype(t *testing.T) {
	proto := &fakeMultiPlugin{}
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{},
				"readonly": map[string]any{},
			},
		},
	})}
	a.entries = []entry{{proto: proto, name: "gorm", src: sourceRegister, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 2)
	for _, inst := range insts {
		assert.NotSame(t, proto, inst.plugin,
			"展开出 >1 个实例时任何一个实例都不能复用原型——构造参数没法同时分给两份")
	}
}

func TestInstanceIDAndLabel(t *testing.T) {
	single := &instance{name: "cors", instance: defaultInstance, plugin: &fakeSinglePlugin{}}
	assert.Equal(t, "cors", single.id())
	assert.Equal(t, "cors", single.label(), "单实例插件的 default 实例，label 与 id 一致，不加 [default]")

	multiDefault := &instance{name: "gorm", instance: defaultInstance, plugin: &fakeMultiPlugin{}}
	assert.Equal(t, "gorm", multiDefault.id(), "id() 里 default 被省略")
	assert.Equal(t, "gorm[default]", multiDefault.label(), "label() 要把多实例插件的 default 显式标出来")

	named := &instance{name: "gorm", instance: "readonly", plugin: &fakeMultiPlugin{}}
	assert.Equal(t, "gorm[readonly]", named.id())
	assert.Equal(t, "gorm[readonly]", named.label())
}

func TestExpandWiresContextAndBase(t *testing.T) {
	a := &App{cfg: newTestConfig(t, nil)}
	a.entries = []entry{{proto: &fakeSinglePlugin{}, name: "cors", src: sourceRegister}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)

	p := insts[0].plugin.(*fakeSinglePlugin)
	assert.Equal(t, "cors", p.Name(), "bindBase 必须把框架推导的名字回填进 Base")
	assert.NotNil(t, p.Ctx(), "bindBase 必须把 Context 回填进 Base")
}

func TestExpandMultiInstanceContextCarriesInstanceName(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"readonly": map[string]any{}}},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Equal(t, "readonly", insts[0].ctx.Instance())
	assert.Equal(t, "gorm", insts[0].ctx.Name())
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test . -run TestExpand -v
```

Expected: 编译失败，`undefined: (*App).expand` —— `stage_expand.go` 还不存在（这一步同时会暴露 `App`/`entry`/`instance`/`Context` 是否已按契约就位；如果 Task 1 尚未提交，先补齐再回到这里）。

- [ ] **Step 3: 写实现**

创建 `stage_expand.go`：

```go
package xbc

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/xbcio/xbc/log"
)

// expand is stage 2 of the assembly pipeline: it turns registered plugin
// prototypes (entries) into plugin instances, applying the enable-rule
// matrix (spec §6.4) and the multi-instance expansion shape (spec §6.5).
func (a *App) expand() ([]*instance, error) {
	seen := make(map[string]struct{}, len(a.entries))
	var out []*instance

	for _, e := range a.entries {
		// A duplicate plugin name is illegal regardless of multi/single --
		// letting it through would let two unrelated entries collapse onto
		// the same graph node id in stage 4 (see the id()/label() doc above).
		if _, dup := seen[e.name]; dup {
			return nil, fmt.Errorf("xbc: 插件名 %s 重复注册", e.name)
		}
		seen[e.name] = struct{}{}

		insts, err := a.expandEntry(e)
		if err != nil {
			return nil, err
		}
		out = append(out, insts...)
	}

	if err := a.checkOrphanSections(seen); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *App) expandEntry(e entry) ([]*instance, error) {
	if e.multi {
		return a.expandMulti(e)
	}
	return a.expandSingle(e)
}

// expandSingle handles a plugin that did not declare MultiInstancer. It
// always produces at most one instance, at defaultInstance.
func (a *App) expandSingle(e entry) ([]*instance, error) {
	path := "plugins." + e.name
	enabled, err := a.sectionEnabled(path, a.cfg.Exists(path), e.src)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}

	plugin := e.proto
	if e.src != sourceRegister {
		// Blank-import registrations never carry meaningful constructor
		// state (there was no call site to pass arguments), so there is no
		// reason to hold on to the package-level prototype -- always start
		// from a fresh zero value.
		plugin = clonePrototype(e.proto)
	}
	return []*instance{a.newInstance(plugin, e.name, defaultInstance, e.src)}, nil
}

// expandMulti handles a plugin that declared MultiInstance() == true.
func (a *App) expandMulti(e entry) ([]*instance, error) {
	path := "plugins." + e.name
	section := a.cfg.Sub(path)

	if section == nil {
		if e.src != sourceRegister {
			return nil, nil
		}
		return []*instance{a.newInstance(e.proto, e.name, defaultInstance, e.src)}, nil
	}

	pluginEnabled := true
	var names []string
	for k, v := range section {
		if _, ok := v.(map[string]any); ok {
			names = append(names, k)
			continue
		}
		if k != "enabled" {
			return nil, fmt.Errorf(
				"xbc: 多实例插件 %s 的配置节下 %q 不是实例（实例配置必须是映射）", e.name, k)
		}
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("xbc: %s.enabled 必须是布尔值，得到 %v", path, v)
		}
		pluginEnabled = b
	}
	if !pluginEnabled {
		return nil, nil
	}
	if len(names) == 0 {
		// The section exists (even as {}) but names no instances -- per the
		// single-instance enable rule extended to the multi-instance shape,
		// existence alone (regardless of src) means "enabled", so we fall
		// back to a lone default instance.
		names = []string{defaultInstance}
	}
	sort.Strings(names) // deterministic regardless of map iteration order

	var enabledNames []string
	for _, name := range names {
		sub, _ := section[name].(map[string]any)
		instEnabled := true
		if v, ok := sub["enabled"]; ok {
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("xbc: %s.%s.enabled 必须是布尔值，得到 %v", path, name, v)
			}
			instEnabled = b
		}
		if instEnabled {
			enabledNames = append(enabledNames, name)
		}
	}

	// Ruling R7: only an explicit Register that ends up with exactly one
	// enabled instance may reuse the constructor-supplied prototype.
	reusePrototype := e.src == sourceRegister && len(enabledNames) == 1
	if e.src == sourceRegister && len(enabledNames) > 1 {
		log.L().Warn(
			"多实例插件通过 app.Register 注册且展开出多个实例，构造参数不会带到各实例上，请确认插件零值可用",
			"plugin", e.name, "instances", len(enabledNames))
	}

	out := make([]*instance, 0, len(enabledNames))
	for _, name := range enabledNames {
		p := e.proto
		if !reusePrototype {
			p = clonePrototype(e.proto)
		}
		out = append(out, a.newInstance(p, e.name, name, e.src))
	}
	return out, nil
}

// sectionEnabled applies the enable-rule matrix (spec §6.4) for a config path
// that either exists or does not:
//
//	register + section absent  -> enabled (defaults)
//	register + section present -> enabled unless explicitly disabled
//	import   + section absent  -> disabled
//	import   + section present -> enabled unless explicitly disabled
func (a *App) sectionEnabled(path string, exists bool, src source) (bool, error) {
	if !exists {
		return src == sourceRegister, nil
	}
	v := a.cfg.Get(path + ".enabled")
	if v == nil {
		return true, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("xbc: %s.enabled 必须是布尔值，得到 %v", path, v)
	}
	return b, nil
}

// clonePrototype builds a fresh zero-value plugin of the same concrete type
// as proto. Ruling R7: a shallow copy would drag along embedded
// synchronization primitives (sync.Mutex and friends, which go vet flags),
// and there is no generically correct definition of a deep copy -- so any
// plugin instance that isn't the sole, explicitly-registered one must start
// from zero and get all of its state from configuration.
func clonePrototype(proto Plugin) Plugin {
	t := reflect.TypeOf(proto).Elem()
	return reflect.New(t).Interface().(Plugin)
}

// isMultiInstance reports whether p declared itself multi-instance. It is a
// plain type assertion rather than a stored flag on *instance, because the
// concrete plugin value is already sitting right there -- adding a
// duplicate bit of state would just be one more thing that could drift out
// of sync with the truth.
func isMultiInstance(p Plugin) bool {
	mi, ok := p.(MultiInstancer)
	return ok && mi.MultiInstance()
}

// newInstance builds one *instance together with its own *Context, and wires
// ctx/name back into the plugin's embedded Base (if any) so that
// Base.Ctx()/Log()/Name() work correctly from this point onward.
func (a *App) newInstance(p Plugin, name, inst string, src source) *instance {
	logger := log.L().With("plugin", name)
	if isMultiInstance(p) {
		logger = logger.With("instance", inst)
	}
	ctx := &Context{app: a, name: name, instance: inst, logger: logger}
	bindBase(p, ctx, name)
	return &instance{plugin: p, name: name, instance: inst, src: src, ctx: ctx}
}

// id returns the graph node id: "gorm" for a single-instance plugin,
// "gorm[readonly]" for a named instance other than default. The default
// instance never gets a bracket, whether the owning plugin is multi-instance
// or not -- that asymmetry is label()'s job, not id()'s.
func (i *instance) id() string {
	if i.instance == defaultInstance {
		return i.name
	}
	return i.name + "[" + i.instance + "]"
}

// label is the display form used in startup logs and error copy. It is the
// same as id(), except the default instance of a multi-instance plugin
// renders as "gorm[default]" so a reader can tell "single-instance plugin"
// apart from "multi-instance plugin's default instance" at a glance.
func (i *instance) label() string {
	if i.instance != defaultInstance {
		return i.id()
	}
	if isMultiInstance(i.plugin) {
		return i.name + "[" + defaultInstance + "]"
	}
	return i.name
}

// checkOrphanSections implements ruling R6: a plugins.<key> section with no
// matching registered plugin is a fatal startup error, not a warning. The
// entire point of this diagnostic is to catch "wrote config, forgot the
// import, silently did nothing" -- if the diagnostic itself failed silently
// too, that exact bug would slip through wearing a different hat.
//
// Only the plugins.* namespace is scanned. server./log./app. are reserved
// top-level namespaces handled elsewhere in the pipeline and are never
// treated as candidate plugin sections.
func (a *App) checkOrphanSections(registered map[string]struct{}) error {
	section := a.cfg.Sub("plugins")
	if section == nil {
		return nil
	}

	var orphans []string
	for k := range section {
		if _, ok := registered[k]; !ok {
			orphans = append(orphans, k)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	sort.Strings(orphans)

	var b strings.Builder
	for i, name := range orphans {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "xbc: plugins.%s 有配置但无对应插件\n  → 是否忘了 import github.com/xbcio/xbc/plugins/%s？", name, name)
	}
	return errors.New(b.String())
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test . -run TestExpand -v
go test . -run TestInstanceIDAndLabel -v
```

Expected: 全部 PASS。特别确认 `TestExpandOrphanSectionAbortsWithExactMessage` 的文案逐字匹配，`TestExpandR7MultiInstanceRegisteredWithMultipleInstancesSkipsPrototype` 里两个实例都不是原型本身。

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
git add stage_expand.go stage_expand_test.go
git commit -m "feat(kernel): 阶段 2 Expand，六种启用组合 + 多实例展开 + 孤儿配置节致命诊断"
```

---

### Task 9: 阶段 3 BindConfig —— 逐实例绑定配置、`enabled` 隐式注入、错误聚合

**Files:**
- Create: `stage_config.go`
- Test: `stage_config_test.go`

**Interfaces:**
- Consumes:
  - `instance` / `defaultInstance` / `isMultiInstance`（`stage_expand.go`，Task 8）
  - `Configurable`（`plugin.go`，Task 1）
  - `Config.Unmarshal`（`config.go`，Task 6）
  - `conf.Validate` / `conf.ValidationError`（`internal/conf`，Task 6）
  - `Base`（`plugin.go`，Task 1，测试用假插件里用到）
- Produces:
  - `func (i *instance) configPath() string`
  - `func (a *App) bindConfigs(insts []*instance) error`

**为什么 Expand 必须早于 BindConfig（spec §4.1(c)）。** 先展开出实例、再绑配置，一条 DSN 写错才能精确报告到 `plugins.gorm.readonly.dsn`，而不是含糊的 `plugins.gorm`——展开这一步已经把"两个数据源"拆成了两个独立的校验单元，绑定与报错都可以就着实例名精确定位，不需要在事后猜是哪个实例出的错。反过来，如果颠倒顺序先绑配置再展开，绑定阶段还不知道会展开出几个实例，连"往哪个字段绑"都说不清楚。

**错误聚合，不是遇到第一个就退。** 配置错误往往成群出现——一段复制粘贴的 yaml 块里同一个笔误可能重复了两次,一次改完重启一次只能修一条,对着日志改五次配置才能启动成功，这种迭代成本没有必要。本 task 把每个实例校验出的错误行汇总进同一个 `*conf.ValidationError`，一次全部打出来。

**`enabled` 字段要不要在绑定前从配置子树里摘掉？** 已读 koanf v2.3.6 源码验证：`(*koanf.Koanf).UnmarshalWithConf` 在调用方不显式传 `DecoderConfig` 时，会用下面这段默认值（`koanf.go:266-274`）：

```go
if c.DecoderConfig == nil {
    c.DecoderConfig = &mapstructure.DecoderConfig{
        DecodeHook: mapstructure.ComposeDecodeHookFunc(
            mapstructure.StringToTimeDurationHookFunc(),
            textUnmarshalerHookFunc()),
        Metadata:         nil,
        WeaklyTypedInput: true,
    }
}
```

这里没有设置 `ErrorUnused`，其零值是 `false`——mapstructure 在 `ErrorUnused=false` 时，对目标结构体没有声明的输入 key 会**静默忽略**，不报错、不 panic。`enabled` 对插件的 `Config` 结构体来说就是这样一个"多余的输入 key"：它由框架在阶段 2 读取和消费，插件的 `Config` 结构体从来不需要声明它，绑定时 mapstructure 自动跳过，没有任何冲突。所以**本 task 不需要在绑定前摘除 `enabled`**——这不是偷懒，是验证过底层库行为之后确认这一步没有必要做。如果未来 `Config.Unmarshal` 改成显式传 `DecoderConfig{ErrorUnused: true}`（比如为了在别的场景抓"配置里有拼写错误的字段名"），到那时才需要在 `bindConfigs` 里补一次摘除逻辑，并把这个理由从这里搬过去。

**绑定顺序取 `expand()` 返回的原始顺序（也就是注册顺序）**，不额外排序——阶段 4 的拓扑序此时还没算出来，用一个跟运行无关、但可预测、不随 map 迭代顺序抖动的顺序，能保证多插件同时出错时错误行的先后顺序是确定的，测试不会 flaky。

- [ ] **Step 1: 写失败测试**

创建 `stage_config_test.go`：

```go
package xbc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type demoConfig struct {
	DSN string `yaml:"dsn"           validate:"required"`
	Max int    `yaml:"max_open_conn" default:"5"`
}

type demoConfigurablePlugin struct {
	Base
	Cfg demoConfig
}

func (p *demoConfigurablePlugin) ConfigPtr() any { return &p.Cfg }

type multiConfigurablePlugin struct {
	Base
	Cfg demoConfig
}

func (p *multiConfigurablePlugin) MultiInstance() bool { return true }
func (p *multiConfigurablePlugin) ConfigPtr() any      { return &p.Cfg }

// noConfigPlugin does not implement Configurable at all.
type noConfigPlugin struct{ Base }

func TestBindConfigsSingleInstance(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"demo": map[string]any{"dsn": "root:pwd@tcp(127.0.0.1:3306)/app"},
		},
	})}
	p := &demoConfigurablePlugin{}
	inst := &instance{plugin: p, name: "demo", instance: defaultInstance}

	require.NoError(t, a.bindConfigs([]*instance{inst}))
	assert.Equal(t, "root:pwd@tcp(127.0.0.1:3306)/app", p.Cfg.DSN)
	assert.Equal(t, 5, p.Cfg.Max, "default tag 在这一步生效")
}

func TestBindConfigsMultiInstanceIsolatesEachInstance(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{"dsn": "a", "max_open_conn": 10},
				"readonly": map[string]any{"dsn": "b"},
			},
		},
	})}
	pDefault := &multiConfigurablePlugin{}
	pReadonly := &multiConfigurablePlugin{}
	insts := []*instance{
		{plugin: pDefault, name: "gorm", instance: "default"},
		{plugin: pReadonly, name: "gorm", instance: "readonly"},
	}

	require.NoError(t, a.bindConfigs(insts))
	assert.Equal(t, "a", pDefault.Cfg.DSN)
	assert.Equal(t, 10, pDefault.Cfg.Max)
	assert.Equal(t, "b", pReadonly.Cfg.DSN)
	assert.Equal(t, 5, pReadonly.Cfg.Max, "readonly 没写 max_open_conn，必须各自取默认值，互不影响")
}

func TestBindConfigsRequiredFieldMissingReportsPreciseInstancePath(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{"readonly": map[string]any{}},
		},
	})}
	inst := &instance{plugin: &multiConfigurablePlugin{}, name: "gorm", instance: "readonly"}

	err := a.bindConfigs([]*instance{inst})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.gorm.readonly.dsn",
		"错误必须精确指到具体实例的字段路径，不能只说 plugins.gorm")
}

func TestBindConfigsAggregatesErrorsAcrossPlugins(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"demo": map[string]any{},
			"gorm": map[string]any{"readonly": map[string]any{}},
		},
	})}
	insts := []*instance{
		{plugin: &demoConfigurablePlugin{}, name: "demo", instance: defaultInstance},
		{plugin: &multiConfigurablePlugin{}, name: "gorm", instance: "readonly"},
	}

	err := a.bindConfigs(insts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.demo.dsn",
		"两个插件各错一条，必须一次全部报出，不能第一个错就退")
	assert.Contains(t, err.Error(), "plugins.gorm.readonly.dsn")
}

func TestBindConfigsSkipsPluginsWithoutConfigurable(t *testing.T) {
	a := &App{cfg: newTestConfig(t, nil)}
	inst := &instance{plugin: &noConfigPlugin{}, name: "plain", instance: defaultInstance}

	assert.NoError(t, a.bindConfigs([]*instance{inst}),
		"没实现 Configurable 的插件必须被跳过，不能因为找不到 ConfigPtr 报错")
}

func TestBindConfigsEnvOverride(t *testing.T) {
	t.Setenv("XBC_PLUGINS_DEMO_DSN", "env-dsn")
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{"demo": map[string]any{"dsn": "file-dsn"}},
	})}
	p := &demoConfigurablePlugin{}
	inst := &instance{plugin: p, name: "demo", instance: defaultInstance}

	require.NoError(t, a.bindConfigs([]*instance{inst}))
	assert.Equal(t, "env-dsn", p.Cfg.DSN, "ENV 覆盖必须在这一步生效，串起 Task 5 的 conf.Bind")
}

func TestBindConfigsIgnoresEnabledKeyAsPlainExtraField(t *testing.T) {
	// Stage 2 (expand, Task 8) is the one that interprets "enabled" and drops
	// disabled instances before stage 3 ever runs -- bindConfigs must never
	// see a disabled instance in insts. This test only proves the mechanical
	// side: an "enabled" key left in a config subtree does not break
	// unmarshalling, because mapstructure silently ignores fields the target
	// struct doesn't declare (verified against koanf v2.3.6 source above).
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{"demo": map[string]any{"dsn": "x", "enabled": true}},
	})}
	p := &demoConfigurablePlugin{}
	inst := &instance{plugin: p, name: "demo", instance: defaultInstance}

	require.NoError(t, a.bindConfigs([]*instance{inst}))
	assert.Equal(t, "x", p.Cfg.DSN)
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test . -run TestBindConfigs -v
```

Expected: 编译失败，`undefined: (*App).bindConfigs`——`stage_config.go` 还不存在。

- [ ] **Step 3: 写实现**

创建 `stage_config.go`：

```go
package xbc

import (
	"errors"
	"fmt"

	"github.com/xbcio/xbc/internal/conf"
)

// configPath computes the config subtree path for one instance, following
// the shape spec §6.5 defines: plugins.<name> for single-instance plugins,
// plugins.<name>.<instance> for multi-instance ones.
func (i *instance) configPath() string {
	if isMultiInstance(i.plugin) {
		return "plugins." + i.name + "." + i.instance
	}
	return "plugins." + i.name
}

// bindConfigs is stage 3 of the assembly pipeline: for every instance that
// implements Configurable, it binds that instance's config subtree into
// ConfigPtr() and validates it.
//
// Validation errors are aggregated across every instance instead of
// returning on the first failure -- config mistakes tend to arrive in
// clusters, and stopping at the first one means restarting the process once
// per mistake instead of fixing them all in one pass.
//
// Instances are processed in the order expand() returned them (registration
// order), so that when several instances fail at once, the resulting error
// lines are in a deterministic order and tests never flake on map iteration
// order.
func (a *App) bindConfigs(insts []*instance) error {
	var lines []string

	for _, inst := range insts {
		cfgable, ok := inst.plugin.(Configurable)
		if !ok {
			continue
		}

		ptr := cfgable.ConfigPtr()
		path := inst.configPath()

		if err := a.cfg.Unmarshal(path, ptr); err != nil {
			return fmt.Errorf("xbc: 绑定配置 %s 失败：%w", path, err)
		}

		if err := conf.Validate(ptr, path); err != nil {
			var verr *conf.ValidationError
			if errors.As(err, &verr) {
				lines = append(lines, verr.Lines...)
				continue
			}
			return err
		}
	}

	if len(lines) > 0 {
		return &conf.ValidationError{Lines: lines}
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test . -run TestBindConfigs -v
```

Expected: 全部 PASS。特别确认 `TestBindConfigsAggregatesErrorsAcrossPlugins` 同时包含两条错误路径，`TestBindConfigsEnvOverride` 证明 ENV 优先级高于文件。

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
git add stage_config.go stage_config_test.go
git commit -m "feat(kernel): 阶段 3 BindConfig，逐实例绑定 + 错误聚合"
```
### Task 10: 阶段 4 Resolve —— 合并依赖图两端、拓扑排序

**Files:**
- Create: `stage_resolve.go`
- Modify: `xbc.go`（`instance` 追加 `fields []inject.FieldSpec` 字段——裁决 R12，见 Step 3 开头）
- Test: `stage_resolve_test.go`

**Interfaces:**
- Consumes:
  - `deps.go`（Task 2）：`type Dep struct{ Type reflect.Type; Instance string; Optional bool }`、`func (d Dep) String() string`、`type Ref struct{ typ reflect.Type; instance string }`、`func (r Ref) String() string`、`func Need[T any]() Dep`、`func NeedNamed[T any](instance string) Dep`、`func Opt[T any]() Dep`、`func Offer[T any]() Dep`、`func RefOf[T Plugin]() Ref`、`func (r Ref) Instance(name string) Ref`、`type Deps struct{ Types []Dep; Plugins []Ref; After, Before []string }`、`func normInstance(s string) string`
  - `plugin.go`（Task 1）：`type Plugin interface{ Name() string }`、`type Declarer interface{ Dependencies() Deps }`、`type Provider interface{ Provides() []Dep }`
  - `xbc.go`（Task 1）：`type instance struct{ plugin Plugin; name, instance string; src source; ctx *Context; deps Deps; provides []Dep; inited bool }`、`func (i *instance) id() string`、`func (i *instance) label() string` —— **`fields []inject.FieldSpec` 这一个字段由本 task 用 `Edit` 追加**（`inject.FieldSpec` 到 Task 7 才存在，Task 1 写不了，裁决 R12），追加位置见 Step 3 开头
  - `registry.go`（Task 4）：`type registryKey struct{ typ reflect.Type; instance string }`
  - `internal/graph`（Task 3）：`func New() *Graph`、`func (g *Graph) AddNode(id string)`、`func (g *Graph) AddEdge(from, to string, hard bool)`、`func (g *Graph) Sort() (order []string, misses []Miss, err error)`、`type Miss struct{ Node, Ref, Dir string }`、`type CycleError struct{ Path []string }`、`func (e *CycleError) Error() string`、`type MissingNodeError struct{ From, To string }`
  - `internal/inject`（Task 7）：`type FieldSpec struct{ Index int; Name string; Type reflect.Type; Instance string; Optional bool; Kind Kind }`、`type Kind int`、`const KindInject, KindProvide`、`func Scan(v any) ([]FieldSpec, error)`
- Produces:
  - `func (a *App) resolve(insts []*instance) ([]*instance, []graph.Miss, error)`
  - `func mergeDeps(inst *instance) Deps`（合并 tag 扫描出的 `inject` 字段与 `Declarer.Dependencies()`）
  - `func mergeProvides(inst *instance) []Dep`（合并 tag 扫描出的 `provide` 字段与 `Provider.Provides()`）
  - `type product struct{ typ reflect.Type; owner *instance }`（产物索引的一条记录）
  - `func pkgName(typ reflect.Type) string`、`func isMajorVersionSegment(s string) bool`（「是否忘了 import」提示与具名实例缺失提示共用的包名猜测）
  - `func zeroPluginOf(t reflect.Type) Plugin`、`func refPluginName(r Ref) string`（从 `Ref.typ` 反射出插件名，供 `Deps.Plugins` 报错用）
  - `func matchInterface(products []product, want reflect.Type) []product`、`func closestMatch(allProducts []product, want reflect.Type) (reflect.Type, []string)`、`func missingMethods(typ, iface reflect.Type) []string`、`func methodSignatureMatches(concrete, iface reflect.Method) bool`

---

**为什么 tag 与 `Dependencies()` 两种写法要在这一步合并，而不是二选一。** tag 只能写编译期字面量：`xbc:"inject,name=readonly"` 里的 `readonly` 必须在写代码的那一刻就固定下来。但 spec §5.6「通用插件依赖多实例基础设施」给出的场景是实例名来自配置——`ratelimit` 插件用哪个 redis 实例是 `plugins.ratelimit.counter_instance` 说了算，不可能提前写进 tag。这种场景只能落在 `Dependencies()` 里读 `p.Cfg.CounterInstance`，而这个字段在阶段 3（`bindConfigs`）已经绑定完毕，所以阶段 4 调用 `Dependencies()` 时它是静态可知的——不是运行时才知道的动态值，只是「写在方法里而不是写在 tag 里」的静态值。这就是两种声明方式并存的正当理由：**没有一种写法能覆盖另一种覆盖的场景**，tag 写不出配置驱动的实例名，`Dependencies()` 写起来又比 tag 啰嗦得多，二者互补而非冗余。`mergeDeps` 与 `mergeProvides` 就是把两路声明拢成一张图的地方——插件作者可以只用 tag、只用方法、或者混着用（`AuditPlugin` 那个例子：`DB` 字段走 tag 自动进 `Deps.Types`，`Dependencies()` 只补 tag 表达不了的 `Plugins`），阶段 4 之后一视同仁，不区分「这条边是从哪种写法来的」。

**为什么产物冲突、依赖缺失、成环要分成三种不同的失败形态。** 产物冲突（两个插件声明同一个 `(类型, 实例名)`）和依赖缺失是「图还没建完整就已经能判定出错」的静态问题，二者一起收集进同一份「依赖检查失败」报告——不像 `graph.Sort()` 的成环检测，那必须先有一张完整、边都连好的图才能跑 Kahn 算法。所以本文件的顺序是：先扫描全部实例算出每个实例的合并依赖 `Deps` 与合并产物 `[]Dep`（写回 `inst.deps` / `inst.provides`，供阶段 5 收割校验复用，省得重新扫一遍 tag），期间发现产物冲突就记一条；然后逐实例解析 `Deps.Types` 与 `Deps.Plugins`，命中就连边，缺失且非软约束就记一条错误；**全部实例扫完之后才决定要不要提前返回**——一次性把所有问题倒给用户，而不是发现第一个就 `return`。只有硬依赖检查全部通过，才把 `After`/`Before` 连成软边，交给 `graph.Sort()`；这时才可能出现成环，用 `errors.As` 从 `Sort()` 的返回值里把 `*graph.CycleError` 挑出来，套上 `xbc: 依赖成环` 的外壳重新渲染。

---

- [ ] **Step 1: 写失败的测试 —— 假插件族与第一批场景（线性排序 / tag 与 Dependencies() 混用 / Optional 缺失）**

这一步的假插件全部直接定义在 `stage_resolve_test.go` 里（`package xbc`），不新开子包、不 import 任何真实中间件。测试里手工构造 `*instance`（不经过 `Register`/`expand`），因为阶段 4 的输入契约就是 `[]*instance`——单测它不需要先跑完阶段 1~3。`fields` 用真实的 `inject.Scan` 扫出来（不是手填 `FieldSpec`），这样 tag 语法上的任何改动都会在这层测试里立刻暴露，而不是被手填的假数据掩盖。

夹具里这次扫描跟 `resolve` 的 pass 0 会扫出同样的结果——`resolve` 随后把 `fields` 覆盖成自己扫的那一份，两者逐字相同，覆盖不改变任何断言。之所以夹具里还留着，是因为它顺带守住了一条：如果哪天有人把 pass 0 从 `resolve` 里删掉，夹具扫出来的 `fields` 会让测试**继续通过**——所以下面专门有一个 `TestResolveScansTagsItself` 用不带 `fields` 的裸 `*instance` 调 `resolve`，钉死「生产代码自己会扫」这件事，不靠夹具兜底。

创建 `stage_resolve_test.go`：

```go
package xbc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/internal/inject"
)

// ---- fake product types (marker structs, never provided by any real middleware) ----

type svcA struct{}
type svcB struct{}
type svcC struct{}

// ---- fake plugin family: each carries only the minimal fields its test needs ----

// fakeProducerA provides *svcA via tag only.
type fakeProducerA struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeProducerA) Name() string { return "producer-a" }

// fakeMiddle injects *svcA via tag and provides *svcB via tag -- used to chain into a linear pipeline.
type fakeMiddle struct {
	In  *svcA `xbc:"inject"`
	Out *svcB `xbc:"provide"`
}

func (p *fakeMiddle) Name() string { return "middle" }

// fakeConsumer injects *svcB via tag only -- the end of the chain.
type fakeConsumer struct {
	In *svcB `xbc:"inject"`
}

func (p *fakeConsumer) Name() string { return "consumer" }

// fakeJWT stands in for a "jwt plugin" -- it does nothing beyond Name(), and
// exists purely as a target for Deps.Plugins' Ref matching. It produces no
// type at all; it satisfies a hard dependency purely by "being present".
type fakeJWT struct{}

func (p *fakeJWT) Name() string { return "jwt" }

// fakeAudit uses both a tag (inject *svcA) and Dependencies()
// (RefOf[*fakeJWT]) -- verifying that stage 4 merges both styles into the
// same graph, so they can be freely mixed.
type fakeAudit struct {
	DB *svcA `xbc:"inject"`
}

func (p *fakeAudit) Name() string { return "audit" }
func (p *fakeAudit) Dependencies() Deps {
	return Deps{Plugins: []Ref{RefOf[*fakeJWT]()}}
}

// fakeOptionalConsumer's field carries optional -- missing it should neither error nor add a graph edge.
type fakeOptionalConsumer struct {
	Cache *svcC `xbc:"inject,optional"`
}

func (p *fakeOptionalConsumer) Name() string { return "opt-consumer" }

// fakeBadTag's tag action is misspelled, so inject.Scan is bound to error --
// this verifies that resolve's pass 0 propagates the scan error all the way
// out, instead of swallowing it and carrying on with empty fields.
type fakeBadTag struct {
	In *svcA `xbc:"injct"`
}

func (p *fakeBadTag) Name() string { return "bad" }

// newInst hand-assembles an *instance, bypassing Register/expand and
// feeding it straight to resolve(). fields is scanned with the real
// inject.Scan; see this Step's explanation for why.
func newInst(t *testing.T, name, instanceName string, p Plugin) *instance {
	t.Helper()
	fields, err := inject.Scan(p)
	require.NoError(t, err, "插件 %s 的 tag 扫描不应失败", name)
	return &instance{plugin: p, name: name, instance: instanceName, fields: fields}
}

// TestResolveScansTagsItself calls resolve with a bare *instance that has no
// fields, pinning down that "scanning tags is resolve's own job" -- every
// other test goes through newInst, whose fixture has already filled in
// fields, so those tests would still pass even if resolve's pass 0 were
// deleted.
func TestResolveScansTagsItself(t *testing.T) {
	a := &App{}
	// A bare *instance: fields is not pre-filled, forcing resolve to scan it
	// itself. producer produces *svcA, middle consumes *svcA -- this edge exists
	// only in the tags; Dependencies() says nothing about it at all.
	producer := &instance{plugin: &fakeProducerA{}, name: "producer-a", instance: defaultInstance}
	middle := &instance{plugin: &fakeMiddle{}, name: "middle", instance: defaultInstance}

	order, _, err := a.resolve([]*instance{middle, producer})
	require.NoError(t, err, "resolve 必须自己扫出 inject tag，不能依赖调用方预先填好 fields")
	require.NotEmpty(t, middle.fields, "resolve 返回后 fields 应当已被填上")
	require.Len(t, order, 2)
	assert.Equal(t, []string{"producer-a", "middle"}, idsOf(order),
		"扫出来的 inject 边要真的进图：产出方排在消费方之前")
}

func TestResolveRejectsMalformedTag(t *testing.T) {
	a := &App{}
	bad := &instance{plugin: &fakeBadTag{}, name: "bad", instance: defaultInstance}

	_, _, err := a.resolve([]*instance{bad})
	require.Error(t, err, "tag 写错必须在阶段 4 就报出来")
	assert.Contains(t, err.Error(), "xbc: 插件 bad 的 xbc tag 有误",
		"错误文案要点名是哪个实例的 tag 有问题")
}

func TestResolve_LinearOrder(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "consumer", defaultInstance, &fakeConsumer{})
	middle := newInst(t, "middle", defaultInstance, &fakeMiddle{})
	producer := newInst(t, "producer-a", defaultInstance, &fakeProducerA{})

	// Input order is shuffled: the sorted result must not depend on the caller's order, only on the dependency graph itself.
	order, misses, err := a.resolve([]*instance{consumer, middle, producer})
	require.NoError(t, err, "线性依赖链不应报错")
	assert.Empty(t, misses, "没有声明任何软约束，misses 必须是空的")
	require.Len(t, order, 3)
	assert.Equal(t, []string{"producer-a", "middle", "consumer"}, idsOf(order),
		"producer 产出 svcA，middle 消费 svcA 产出 svcB，consumer 消费 svcB —— 拓扑序必须是这个顺序")
}

func TestResolve_MergeTagAndDependencies(t *testing.T) {
	a := &App{}
	jwt := newInst(t, "jwt", defaultInstance, &fakeJWT{})
	producer := newInst(t, "producer-a", defaultInstance, &fakeProducerA{})
	audit := newInst(t, "audit", defaultInstance, &fakeAudit{})

	order, misses, err := a.resolve([]*instance{audit, jwt, producer})
	require.NoError(t, err, "tag 的 svcA 依赖与 Dependencies() 的 jwt 依赖应当都被满足")
	assert.Empty(t, misses)
	// audit must sort after both jwt and producer-a -- both edges must take effect, and this assertion fails if either one is missing.
	assert.Equal(t, "audit", order[len(order)-1].id())
}

func TestResolve_OptionalMissingLeavesZero(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "opt-consumer", defaultInstance, &fakeOptionalConsumer{})

	order, misses, err := a.resolve([]*instance{consumer})
	require.NoError(t, err, "optional 依赖缺失不应报错")
	assert.Empty(t, misses, "optional 硬依赖缺失不算软约束未命中，不进 misses")
	require.Len(t, order, 1)

	zero, err := inject.IsZero(consumer.plugin, consumer.fields[0])
	require.NoError(t, err)
	assert.True(t, zero, "optional 缺失时字段必须留零值，框架不能塞任何东西进去")
}

// idsOf renders a topological order into a slice of id strings, so assert.Equal can compare order directly.
func idsOf(insts []*instance) []string {
	ids := make([]string, len(insts))
	for i, inst := range insts {
		ids[i] = inst.id()
	}
	return ids
}

```

继续在同一个文件里追加下列类型与测试，覆盖「具体类型缺失」「具名实例缺失」两种报错文案。断言用 `assert.Contains` 而不是整串 `assert.Equal`——契约第 13 节「逐字」约束的是**固定的中文模板片段**（"无任何插件提供"、"是否忘了 import ..."、"当前只有"、"下添加 ... 实例" 这些不会因为测试用的假类型名不同而变化的部分），插入的类型名/插件名本身在真实场景是 `*redis.Client`、`gorm` 这样的具体标识符，本测试里换成假类型完全等价，逐字比对整串反而会把「假类型名恰好不等于契约示例里的名字」误判成失败。

```go
// fakeDBConsumer injects *svcC via tag (default instance); no plugin provides *svcC.
type fakeDBConsumer struct {
	DB *svcC `xbc:"inject"`
}

func (p *fakeDBConsumer) Name() string { return "user" }

// fakeNamedConsumer declares a named-instance dependency via Dependencies()
// -- since NeedNamed's instance name is a literal here, it could equally be
// expressed with the tag's name= option; Dependencies() is chosen here
// simply to verify this style also merges correctly into the graph, as an
// equivalent path to the tag.
type fakeNamedConsumer struct{}

func (p *fakeNamedConsumer) Name() string { return "report" }
func (p *fakeNamedConsumer) Dependencies() Deps {
	return Deps{Types: []Dep{NeedNamed[*svcC]("readonly")}}
}

// fakeNamedProducer provides *svcC under whatever instance name the test gives it.
type fakeNamedProducer struct {
	Out *svcC `xbc:"provide"`
}

func (p *fakeNamedProducer) Name() string { return "gorm" }

func TestResolve_ConcreteTypeMissing(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "user", defaultInstance, &fakeDBConsumer{})

	_, _, err := a.resolve([]*instance{consumer})
	require.Error(t, err, "无人提供 *svcC，必须启动中止")
	assert.Contains(t, err.Error(), "xbc: 依赖检查失败")
	assert.Contains(t, err.Error(), "插件 user 需要")
	assert.Contains(t, err.Error(), "无任何插件提供")
	assert.Contains(t, err.Error(), "是否忘了 import github.com/xbcio/xbc/plugins/")
}

func TestResolve_NamedInstanceMissing(t *testing.T) {
	a := &App{}
	report := newInst(t, "report", defaultInstance, &fakeNamedConsumer{})
	gormDefault := newInst(t, "gorm", defaultInstance, &fakeNamedProducer{})
	gormCache := newInst(t, "gorm", "cache", &fakeNamedProducer{})

	_, _, err := a.resolve([]*instance{report, gormDefault, gormCache})
	require.Error(t, err, "只有 default/cache 实例，没有 readonly")
	assert.Contains(t, err.Error(), "插件 report 依赖")
	assert.Contains(t, err.Error(), "[readonly]")
	assert.Contains(t, err.Error(), "当前只有")
	// The "当前只有" list must enumerate both existing instances, not just the first.
	assert.Contains(t, err.Error(), "[default]")
	assert.Contains(t, err.Error(), "[cache]")
	assert.Contains(t, err.Error(), "下添加 readonly 实例")
}

```

继续追加 `Deps.Plugins`（`Ref`）两种缺失场景的测试：整个插件都没启用，与启用了但缺了收窄的那个实例。

```go
// fakeRefConsumer hard-depends on the *fakeJWT plugin being present (without narrowing to an instance).
type fakeRefConsumer struct{}

func (p *fakeRefConsumer) Name() string { return "audit" }
func (p *fakeRefConsumer) Dependencies() Deps {
	return Deps{Plugins: []Ref{RefOf[*fakeJWT]()}}
}

// fakeNarrowedRefConsumer narrows to *fakeJWT's "readonly" instance.
type fakeNarrowedRefConsumer struct{}

func (p *fakeNarrowedRefConsumer) Name() string { return "audit" }
func (p *fakeNarrowedRefConsumer) Dependencies() Deps {
	return Deps{Plugins: []Ref{RefOf[*fakeJWT]().Instance("readonly")}}
}

func TestResolve_RefMissing(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "audit", defaultInstance, &fakeRefConsumer{})

	_, _, err := a.resolve([]*instance{consumer})
	require.Error(t, err, "jwt 一个实例都没启用")
	// The plugin name here goes through deriveName's package-path inference
	// (fakeJWT lives in package xbc alongside this test, so the derived name is
	// necessarily "xbc" rather than "jwt" as in a real scenario) -- the assertion
	// only pins down the fixed template fragment from the contract, and never
	// compares the interpolated package name literally; see the note at the top
	// of Step 1 for why.
	assert.Contains(t, err.Error(), "插件 audit 依赖插件")
	assert.Contains(t, err.Error(), "未启用")
	assert.Contains(t, err.Error(), "在 application.yml 中添加 plugins.")
	assert.Contains(t, err.Error(), "配置节")
}

func TestResolve_RefInstanceNarrowedMissing(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "audit", defaultInstance, &fakeNarrowedRefConsumer{})
	jwtDefault := newInst(t, "jwt", defaultInstance, &fakeJWT{})

	_, _, err := a.resolve([]*instance{consumer, jwtDefault})
	require.Error(t, err, "jwt 启用了，但只有 default 实例，没有 readonly")
	assert.Contains(t, err.Error(), "插件 audit 依赖插件")
	assert.Contains(t, err.Error(), "[readonly]")
	assert.Contains(t, err.Error(), "当前只有")
	assert.Contains(t, err.Error(), "[default]")
	assert.Contains(t, err.Error(), "下添加 readonly 实例")
}

```

继续追加接口匹配三种命中数（1 / 0 / >1）的测试。`Counter` 接口定义在消费方——这正是 spec §5.6 要示范的"接口定义在消费方"套路，只是这里的"消费方"与"提供方"全在同一个测试文件里，免去 import 真实 redis 的麻烦。

```go
// Counter is an example of an interface defined on the consumer side: the consumer only cares about this interface, not who implements it.
type Counter interface {
	Incr() int64
	Expire() error
}

// fullCounterA / fullCounterB both fully implement Counter -- used to manufacture a "multiple candidates" ambiguity.
type fullCounterA struct{}

func (*fullCounterA) Incr() int64   { return 0 }
func (*fullCounterA) Expire() error { return nil }

type fullCounterB struct{}

func (*fullCounterB) Incr() int64   { return 0 }
func (*fullCounterB) Expire() error { return nil }

// halfCounter implements only Incr, missing Expire -- used to manufacture "zero hits, but with a closest candidate".
type halfCounter struct{}

func (*halfCounter) Incr() int64 { return 0 }

type fakeCounterConsumer struct {
	C Counter `xbc:"inject"`
}

func (p *fakeCounterConsumer) Name() string { return "ratelimit" }

// fakeCounterProviderConcrete declares, via Provides(), that it produces the
// concrete type *fullCounterA. It must go through Provides() (not a tag):
// tag scanning picks up a field's statically declared type, so if the field
// were declared as the interface Counter, the product index would store the
// interface itself, unable to express "which concrete implementation was
// actually produced" -- a product must be a concrete type, with interfaces
// appearing only on the dependency side. That is the other half of §5.6's
// "interface defined on the consumer side".
type fakeCounterProviderConcrete struct{}

func (p *fakeCounterProviderConcrete) Name() string { return "provider" }
func (p *fakeCounterProviderConcrete) Provides() []Dep {
	return []Dep{Offer[*fullCounterA]()}
}

func TestResolve_InterfaceSingleMatch(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	producer := newInst(t, "provider", defaultInstance, &fakeCounterProviderConcrete{})

	order, misses, err := a.resolve([]*instance{consumer, producer})
	require.NoError(t, err, "唯一命中应当直接连边成功")
	assert.Empty(t, misses)
	assert.Equal(t, "ratelimit", order[len(order)-1].id())
}

func TestResolve_InterfaceZeroMatchWithClosest(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	producer := newInst(t, "half-provider", defaultInstance, &fakeHalfCounterProvider{})

	_, _, err := a.resolve([]*instance{consumer, producer})
	require.Error(t, err, "halfCounter 没有 Expire 方法，不满足 Counter")
	assert.Contains(t, err.Error(), "插件 ratelimit 需要")
	assert.Contains(t, err.Error(), "无任何插件提供")
	assert.Contains(t, err.Error(), "最接近的是")
	assert.Contains(t, err.Error(), "halfCounter")
	assert.Contains(t, err.Error(), "缺少方法：Expire")
}

type fakeHalfCounterProvider struct{}

func (p *fakeHalfCounterProvider) Name() string { return "half-provider" }
func (p *fakeHalfCounterProvider) Provides() []Dep {
	return []Dep{Offer[*halfCounter]()}
}

func TestResolve_InterfaceAmbiguous(t *testing.T) {
	a := &App{}
	consumer := newInst(t, "ratelimit", defaultInstance, &fakeCounterConsumer{})
	pa := newInst(t, "provider-a", defaultInstance, &fakeCounterProviderConcreteA{})
	pb := newInst(t, "provider-b", defaultInstance, &fakeCounterProviderConcreteB{})

	_, _, err := a.resolve([]*instance{consumer, pa, pb})
	require.Error(t, err, "两个候选都满足 Counter，框架不能瞎猜")
	assert.Contains(t, err.Error(), "插件 ratelimit 需要")
	assert.Contains(t, err.Error(), "有 2 个候选")
	assert.Contains(t, err.Error(), "fullCounterA")
	assert.Contains(t, err.Error(), "fullCounterB")
	assert.Contains(t, err.Error(), `用 xbc:"inject,name=xxx" 指定实例消歧`)
}

type fakeCounterProviderConcreteA struct{}

func (p *fakeCounterProviderConcreteA) Name() string    { return "provider-a" }
func (p *fakeCounterProviderConcreteA) Provides() []Dep { return []Dep{Offer[*fullCounterA]()} }

type fakeCounterProviderConcreteB struct{}

func (p *fakeCounterProviderConcreteB) Name() string    { return "provider-b" }
func (p *fakeCounterProviderConcreteB) Provides() []Dep { return []Dep{Offer[*fullCounterB]()} }

```

继续追加产物冲突、软约束（命中改变顺序 / 未命中不中止）、成环、多条错误一次报完、`provide` tag 与 `Provides()` 合并这六个场景。

```go
// fakeConflictProducer and fakeConflictProducerAlt both declare producing *svcA[default] -- a conflict.
type fakeConflictProducer struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeConflictProducer) Name() string { return "conflict-a" }

type fakeConflictProducerAlt struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeConflictProducerAlt) Name() string { return "conflict-b" }

func TestResolve_ProductConflict(t *testing.T) {
	a := &App{}
	p1 := newInst(t, "conflict-a", defaultInstance, &fakeConflictProducer{})
	p2 := newInst(t, "conflict-b", defaultInstance, &fakeConflictProducerAlt{})

	_, _, err := a.resolve([]*instance{p1, p2})
	require.Error(t, err, "两个插件的同一实例名都产出 *svcA，必须报冲突")
	assert.Contains(t, err.Error(), "conflict-a")
	assert.Contains(t, err.Error(), "conflict-b")
	assert.Contains(t, err.Error(), "都声明产出")
}

// fakeSoftAfter has no hard dependency at all; it only declares "if tracing exists, sort me after it".
type fakeSoftAfter struct{}

func (p *fakeSoftAfter) Name() string { return "business" }
func (p *fakeSoftAfter) Dependencies() Deps {
	return Deps{After: []string{"tracing"}}
}

type fakeTracing struct{}

func (p *fakeTracing) Name() string { return "tracing" }

func TestResolve_SoftConstraintOrdering(t *testing.T) {
	a := &App{}
	// business is deliberately passed in before tracing: if After has no effect, insertion order would keep business ahead.
	business := newInst(t, "business", defaultInstance, &fakeSoftAfter{})
	tracing := newInst(t, "tracing", defaultInstance, &fakeTracing{})

	order, misses, err := a.resolve([]*instance{business, tracing})
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"tracing", "business"}, idsOf(order),
		"After 软约束命中，必须把 tracing 排到 business 前面")
}

// fakeSoftMiss declares After on a plugin name that does not exist at all -- this must not abort startup.
type fakeSoftMiss struct{}

func (p *fakeSoftMiss) Name() string { return "z-plugin" }
func (p *fakeSoftMiss) Dependencies() Deps {
	return Deps{After: []string{"ghost"}}
}

func TestResolve_SoftConstraintMissNotFatal(t *testing.T) {
	a := &App{}
	z := newInst(t, "z-plugin", defaultInstance, &fakeSoftMiss{})

	order, misses, err := a.resolve([]*instance{z})
	require.NoError(t, err, "软约束引用的名字不存在，只记 miss，不能中止启动")
	require.Len(t, order, 1)
	require.Len(t, misses, 1)
	assert.Equal(t, graph.Miss{Node: "z-plugin", Ref: "ghost", Dir: "after"}, misses[0])
}

// fakeCycleA/B/C hold hands via Dependencies() to form a cycle: a needs b, b needs c, c needs a.
type fakeCycleA struct{}

func (p *fakeCycleA) Name() string       { return "user" }
func (p *fakeCycleA) Dependencies() Deps { return Deps{Plugins: []Ref{RefOf[*fakeCycleC]()}} }

type fakeCycleB struct{}

func (p *fakeCycleB) Name() string       { return "order" }
func (p *fakeCycleB) Dependencies() Deps { return Deps{Plugins: []Ref{RefOf[*fakeCycleA]()}} }

type fakeCycleC struct{}

func (p *fakeCycleC) Name() string       { return "payment" }
func (p *fakeCycleC) Dependencies() Deps { return Deps{Plugins: []Ref{RefOf[*fakeCycleB]()}} }

func TestResolve_Cycle(t *testing.T) {
	a := &App{}
	ca := newInst(t, "user", defaultInstance, &fakeCycleA{})
	cb := newInst(t, "order", defaultInstance, &fakeCycleB{})
	cc := newInst(t, "payment", defaultInstance, &fakeCycleC{})

	_, _, err := a.resolve([]*instance{ca, cb, cc})
	require.Error(t, err, "user → payment → order → user 是一个环")
	assert.Contains(t, err.Error(), "xbc: 依赖成环")
	assert.Contains(t, err.Error(), "→")
}

func TestResolve_MultipleErrorsReportedTogether(t *testing.T) {
	a := &App{}
	// Two unrelated misses: user is missing *svcC, audit is missing the jwt
	// plugin. A single resolve call must report both, not return early just
	// because it hit the first one.
	userConsumer := newInst(t, "user", defaultInstance, &fakeDBConsumer{})
	auditConsumer := newInst(t, "audit", defaultInstance, &fakeRefConsumer{})

	_, _, err := a.resolve([]*instance{userConsumer, auditConsumer})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "插件 user 需要")
	assert.Contains(t, err.Error(), "插件 audit 依赖插件")
	assert.Contains(t, err.Error(), "未启用")
}

// fakeDualProvider uses both a tag (provide *svcA) and Provides() (an extra
// offer of *svcB) -- verifying that stage 4 also merges both styles on the
// product side, not just on the dependency side.
type fakeDualProvider struct {
	Out *svcA `xbc:"provide"`
}

func (p *fakeDualProvider) Name() string    { return "dual" }
func (p *fakeDualProvider) Provides() []Dep { return []Dep{Offer[*svcB]()} }

func TestResolve_ProvideTagAndProvidesMerge(t *testing.T) {
	a := &App{}
	dual := newInst(t, "dual", defaultInstance, &fakeDualProvider{})
	consumer := newInst(t, "consumer", defaultInstance, &fakeConsumer{}) // injects *svcB via tag

	order, misses, err := a.resolve([]*instance{consumer, dual})
	require.NoError(t, err, "tag 声明的 *svcA 与 Provides() 声明的 *svcB 都要生效")
	assert.Empty(t, misses)
	assert.Equal(t, []string{"dual", "consumer"}, idsOf(order))
}
```

---

- [ ] **Step 2: 跑测试确认失败**

```bash
cd /Users/10097292/Desktop/caffe/xbcio/xbc
go test ./ -run TestResolve -v
```

Expected: 编译失败，`./stage_resolve_test.go:XX:XX: a.resolve undefined (type *App has no field or method resolve)`。这是本 task 唯一要新增的方法，`instance`、`Deps`、`Dep`、`Ref`、`Need`/`NeedNamed`/`Opt`/`Offer`/`RefOf`、`graph.Miss`、`inject.Scan`/`inject.IsZero` 全部来自更早的 task，此刻应该都能编译通过——如果报的是这些符号 undefined，说明前置 task 没做完，要先回头补，而不是在这里补丁式地补定义。

---

- [ ] **Step 3: 写实现**

先用 `Edit` 往 `xbc.go` 的 `type instance struct { ... }` 里补一个字段（裁决 R12 里挂在本 task 名下的那一个），加在 `ctx` 那一行后面：

```go
	// fields is the xbc-tag scan of this instance's plugin, filled by
	// resolve's pass 0. Stage 5 injects into these fields and harvests the
	// provide-tagged ones back out, so both stages read the same scan
	// instead of each doing their own -- one scan, one source of truth for
	// what the plugin's tags actually said.
	fields []inject.FieldSpec
```

`xbc.go` 顶部相应补上 `"github.com/xbcio/xbc/internal/inject"` 这一行 import。这个字段之所以现在才加而不是 Task 1 就位，是因为 `inject.FieldSpec` 到 Task 7 才存在。

然后按「扫 tag → 合并 → 建产物索引 → 解硬依赖 → 一次性报错 → 连软边 → 拓扑排序」的顺序组织，与本 task 正文说明的顺序一致。名字猜测（`pkgName`）与插件名反射（`refPluginName`）两个辅助函数单独放在文件末尾，因为它们只服务于错误文案，跟排序算法本身无关，混在一起会让 `resolve` 主体不好读。

创建 `stage_resolve.go`：

```go
package xbc

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/internal/inject"
)

// product is one declared output: a concrete type paired with the instance
// that declared it. Built once per resolve() call from every instance's
// merged provide-side; backs both the exact (type, instance) index and the
// interface-assignability scan.
type product struct {
	typ   reflect.Type
	owner *instance
}

// mergeDeps combines the two dependency-declaration paths from spec §5.7:
// the "inject" tags scanned into inst.fields by pass 0 above, and the
// optional Declarer.Dependencies() method. A plugin may use either, or mix
// both on the same instance (AuditPlugin in the spec: DB field via tag,
// Plugins via Dependencies()) -- this function is where the two paths become
// one Deps value, so everything downstream treats them identically.
func mergeDeps(inst *instance) Deps {
	var out Deps
	for _, fs := range inst.fields {
		if fs.Kind != inject.KindInject {
			continue
		}
		out.Types = append(out.Types, Dep{Type: fs.Type, Instance: fs.Instance, Optional: fs.Optional})
	}
	if d, ok := inst.plugin.(Declarer); ok {
		explicit := d.Dependencies()
		out.Types = append(out.Types, explicit.Types...)
		out.Plugins = append(out.Plugins, explicit.Plugins...)
		out.After = append(out.After, explicit.After...)
		out.Before = append(out.Before, explicit.Before...)
	}
	return out
}

// mergeProvides mirrors mergeDeps on the producing side of the graph:
// "provide" tags plus the optional Provider.Provides() method. A provide
// tag's FieldSpec.Instance is always "" (inject.Scan rejects "name=" on a
// provide tag, per the API contract) -- the produced instance name is always
// the owning instance's own name, decided later by the caller, never by the
// tag itself. That is why this function never reads fs.Instance.
func mergeProvides(inst *instance) []Dep {
	var out []Dep
	for _, fs := range inst.fields {
		if fs.Kind != inject.KindProvide {
			continue
		}
		out = append(out, Dep{Type: fs.Type})
	}
	if p, ok := inst.plugin.(Provider); ok {
		out = append(out, p.Provides()...)
	}
	return out
}

// resolve is stage 4. It merges both ends of the dependency graph, builds
// the product index, wires every hard and soft edge, and returns the
// topological order. Every problem that would block startup is collected
// and reported together as one "xbc: 依赖检查失败" batch -- fixing the first
// line reported should not just uncover a second one on the next run.
func (a *App) resolve(insts []*instance) ([]*instance, []graph.Miss, error) {
	g := graph.New()
	byID := make(map[string]*instance, len(insts))
	byPluginName := make(map[string][]*instance, len(insts))
	for _, inst := range insts {
		g.AddNode(inst.id())
		byID[inst.id()] = inst
		byPluginName[inst.name] = append(byPluginName[inst.name], inst)
	}

	// Pass 0: scan xbc tags. This is the only place production code fills
	// inst.fields -- stage 2 builds instances but has no business reading
	// tags, and stage 5 needs the scan to have already happened so it can
	// inject and harvest without re-scanning. A malformed tag aborts
	// immediately rather than joining the errLines batch: the batch exists
	// to report every *configuration* problem at once, and a tag typo is a
	// compile-time-shaped mistake whose own message already says exactly
	// which field is wrong.
	for _, inst := range insts {
		fs, err := inject.Scan(inst.plugin)
		if err != nil {
			return nil, nil, fmt.Errorf("xbc: 插件 %s 的 xbc tag 有误：%w", inst.label(), err)
		}
		inst.fields = fs
	}

	// Pass 1: merge tag + explicit declarations once per instance, caching
	// the result on the instance itself so stage 5's harvest-and-verify step
	// does not need to re-scan tags or re-call Dependencies()/Provides().
	for _, inst := range insts {
		inst.deps = mergeDeps(inst)
		inst.provides = mergeProvides(inst)
	}

	// Pass 2: build the product index; a (type, instance) collision is a
	// startup-blocking error just like a missing dependency, so it goes into
	// the same errLines batch instead of aborting immediately.
	produced := make(map[registryKey]*instance)
	var allProducts []product
	productsByInstance := make(map[string][]product)
	var errLines []string
	for _, inst := range insts {
		instName := normInstance(inst.instance)
		for _, dep := range inst.provides {
			key := registryKey{typ: dep.Type, instance: instName}
			if other, ok := produced[key]; ok {
				if other == inst {
					// Same instance declared the same product twice (e.g. once
					// via tag, once via Provides()) -- harmless duplicate, not
					// a conflict between two different plugins.
					continue
				}
				errLines = append(errLines, fmt.Sprintf(
					"  插件 %s 与插件 %s 都声明产出 %s\n    → 产出同一类型、同一实例名的插件只能有一个",
					other.label(), inst.label(), dep.String()))
				continue
			}
			produced[key] = inst
			p := product{typ: dep.Type, owner: inst}
			allProducts = append(allProducts, p)
			productsByInstance[instName] = append(productsByInstance[instName], p)
		}
	}

	// Pass 3: resolve Deps.Types (concrete or interface) against the index.
	for _, inst := range insts {
		for _, dep := range inst.deps.Types {
			if line := resolveTypeDep(g, inst, dep, produced, allProducts, productsByInstance); line != "" {
				errLines = append(errLines, line)
			}
		}
	}

	// Pass 4: resolve Deps.Plugins (Ref) -- presence, optionally narrowed to
	// one named instance.
	for _, inst := range insts {
		for _, ref := range inst.deps.Plugins {
			if line := resolveRef(g, inst, ref, insts); line != "" {
				errLines = append(errLines, line)
			}
		}
	}

	if len(errLines) > 0 {
		return nil, nil, fmt.Errorf("xbc: 依赖检查失败\n%s", strings.Join(errLines, "\n"))
	}

	// Pass 5: only once every hard requirement is satisfiable do we wire the
	// soft After/Before edges. After/Before name a *plugin*, and one plugin
	// name can fan out to several instances (multi-instance plugin) -- that
	// fan-out has to happen here, in Go, before anything reaches the graph,
	// because graph.AddEdge only ever sees one literal node id per call and
	// has no notion of "this name means N nodes". A name that matches no
	// instance at all becomes a Miss we build ourselves, not something we
	// hand to the graph package to detect.
	var misses []graph.Miss
	for _, inst := range insts {
		for _, name := range inst.deps.After {
			targets, ok := byPluginName[name]
			if !ok {
				misses = append(misses, graph.Miss{Node: inst.id(), Ref: name, Dir: "after"})
				continue
			}
			for _, target := range targets {
				g.AddEdge(target.id(), inst.id(), false)
			}
		}
		for _, name := range inst.deps.Before {
			targets, ok := byPluginName[name]
			if !ok {
				misses = append(misses, graph.Miss{Node: inst.id(), Ref: name, Dir: "before"})
				continue
			}
			for _, target := range targets {
				g.AddEdge(inst.id(), target.id(), false)
			}
		}
	}

	order, sortMisses, err := g.Sort()
	if err != nil {
		var cycle *graph.CycleError
		if errors.As(err, &cycle) {
			return nil, nil, fmt.Errorf("xbc: 依赖成环\n  %s", strings.Join(cycle.Path, " → "))
		}
		// Every hard edge we added above only ever names an endpoint we had
		// already confirmed exists, so a *graph.MissingNodeError here would
		// mean a bug in this file, not a user-facing configuration mistake.
		return nil, nil, err
	}
	misses = append(misses, sortMisses...)

	sorted := make([]*instance, len(order))
	for i, id := range order {
		sorted[i] = byID[id]
	}
	return sorted, misses, nil
}

// resolveTypeDep resolves one Dep from Deps.Types against the product
// index. It adds a hard edge and returns "" on success (including the
// "optional and absent" case, which is success too), or returns one
// already-indented, ready-to-join error line otherwise.
func resolveTypeDep(g *graph.Graph, inst *instance, dep Dep, produced map[registryKey]*instance, allProducts []product, productsByInstance map[string][]product) string {
	instName := normInstance(dep.Instance)

	if dep.Type.Kind() == reflect.Interface {
		candidates := matchInterface(productsByInstance[instName], dep.Type)
		switch len(candidates) {
		case 1:
			g.AddEdge(candidates[0].owner.id(), inst.id(), true)
			return ""
		case 0:
			if dep.Optional {
				return ""
			}
			closest, missing := closestMatch(allProducts, dep.Type)
			if closest == nil {
				return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    → 是否忘了 import 提供该类型的插件包",
					inst.label(), dep.Type.String())
			}
			return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    最接近的是 %s，缺少方法：%s",
				inst.label(), dep.Type.String(), closest.String(), strings.Join(missing, "、"))
		default:
			var b strings.Builder
			fmt.Fprintf(&b, "  插件 %s 需要 %s，有 %d 个候选\n", inst.label(), dep.Type.String(), len(candidates))
			for _, c := range candidates {
				fmt.Fprintf(&b, "    %s[%s]\n", c.typ.String(), instName)
			}
			b.WriteString(`    → 用 xbc:"inject,name=xxx" 指定实例消歧`)
			return b.String()
		}
	}

	key := registryKey{typ: dep.Type, instance: instName}
	if owner, ok := produced[key]; ok {
		g.AddEdge(owner.id(), inst.id(), true)
		return ""
	}
	if dep.Optional {
		return ""
	}

	// Same concrete type exists, just not under the instance name asked
	// for -- list every instance that does produce it, so the fix is
	// "add that instance", not "guess what's wrong".
	var alt []string
	for _, p := range allProducts {
		if p.typ == dep.Type {
			alt = append(alt, fmt.Sprintf("%s[%s]", pkgName(dep.Type), normInstance(p.owner.instance)))
		}
	}
	if len(alt) == 0 {
		return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    → 是否忘了 import github.com/xbcio/xbc/plugins/%s",
			inst.label(), dep.Type.String(), pkgName(dep.Type))
	}
	return fmt.Sprintf("  插件 %s 依赖 %s[%s]，当前只有 %s\n    → 在 plugins.%s 下添加 %s 实例",
		inst.label(), pkgName(dep.Type), instName, strings.Join(alt, "、"), pkgName(dep.Type), instName)
}

// resolveRef resolves one Ref from Deps.Plugins: is a matching plugin type
// present, and -- if the Ref was narrowed with .Instance -- present under
// that exact instance name. Matching is by reflect.Type of the concrete
// plugin value, never by a hand-written string: a typo'd RefOf[*Foo]() fails
// to compile, whereas a typo'd plugin-name string would silently never match.
func resolveRef(g *graph.Graph, inst *instance, ref Ref, insts []*instance) string {
	var matches []*instance
	var sameType []*instance
	for _, other := range insts {
		if reflect.TypeOf(other.plugin) != ref.typ {
			continue
		}
		sameType = append(sameType, other)
		if ref.instance != "" && other.instance != normInstance(ref.instance) {
			continue
		}
		matches = append(matches, other)
	}
	if len(matches) > 0 {
		for _, m := range matches {
			g.AddEdge(m.id(), inst.id(), true)
		}
		return ""
	}

	name := refPluginName(ref)
	if ref.instance == "" || len(sameType) == 0 {
		return fmt.Sprintf("  插件 %s 依赖插件 %s，但 %s 未启用\n    → 在 application.yml 中添加 plugins.%s 配置节",
			inst.label(), name, name, name)
	}

	alt := make([]string, 0, len(sameType))
	for _, other := range sameType {
		alt = append(alt, fmt.Sprintf("%s[%s]", name, normInstance(other.instance)))
	}
	return fmt.Sprintf("  插件 %s 依赖插件 %s[%s]，当前只有 %s\n    → 在 plugins.%s 下添加 %s 实例",
		inst.label(), name, normInstance(ref.instance), strings.Join(alt, "、"), name, normInstance(ref.instance))
}

// matchInterface scans products already scoped to one instance name and
// returns every one whose concrete type is assignable to want. Scoping by
// instance mirrors registry.lookup's own semantics (§5.6) -- the static
// check at this stage and the runtime lookup at stage 5+ read the same way
// to a plugin author, because they run the same rule.
func matchInterface(products []product, want reflect.Type) []product {
	var out []product
	for _, p := range products {
		if p.typ.AssignableTo(want) {
			out = append(out, p)
		}
	}
	return out
}

// closestMatch finds, among every declared product in the whole app
// (deliberately not scoped to one instance -- this is a diagnostic aid, not
// a candidate list), the concrete type implementing the most of want's
// methods by name, and reports which method names it is still missing. Ties
// keep the first-declared type, since allProducts is already in
// registration order.
func closestMatch(allProducts []product, want reflect.Type) (reflect.Type, []string) {
	var bestType reflect.Type
	var bestMissing []string
	bestScore := -1
	for _, p := range allProducts {
		missing := missingMethods(p.typ, want)
		score := want.NumMethod() - len(missing)
		if score > bestScore {
			bestScore = score
			bestType = p.typ
			bestMissing = missing
		}
	}
	return bestType, bestMissing
}

// missingMethods lists the methods of iface that typ's method set lacks --
// by name, but a name match with a mismatched signature counts as missing
// too (spec §5.6 registry semantics: "按方法名比对，签名不符也算缺").
func missingMethods(typ, iface reflect.Type) []string {
	var missing []string
	for i := 0; i < iface.NumMethod(); i++ {
		want := iface.Method(i)
		m, ok := typ.MethodByName(want.Name)
		if !ok || !methodSignatureMatches(m, want) {
			missing = append(missing, want.Name)
		}
	}
	return missing
}

// methodSignatureMatches compares a concrete type's method -- whose Type
// includes the receiver as an implicit first input, per reflect's rule for
// any non-interface type -- against an interface method's signature, which
// has no receiver at all. The off-by-one is why this helper exists instead
// of a plain reflect.Type equality check.
func methodSignatureMatches(concrete, iface reflect.Method) bool {
	ct := concrete.Type
	if ct.NumIn() < 1 {
		return false
	}
	if ct.NumIn()-1 != iface.Type.NumIn() || ct.NumOut() != iface.Type.NumOut() {
		return false
	}
	for i := 0; i < iface.Type.NumIn(); i++ {
		if ct.In(i+1) != iface.Type.In(i) {
			return false
		}
	}
	for i := 0; i < iface.Type.NumOut(); i++ {
		if ct.Out(i) != iface.Type.Out(i) {
			return false
		}
	}
	return true
}

// isMajorVersionSegment reports whether s looks like a Go module major
// version path element ("v2", "v10", ...). Mirrors the rule plugin.go's
// deriveName uses, so a versioned plugin package and a versioned dependency
// package guess the same way.
func isMajorVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// pkgName guesses a human name for typ from its package path -- dereferencing
// pointers first, since every product in this framework is provided as a
// pointer type. It backs two purely best-effort hints: "did you forget to
// import" on a fully-missing type, and the plugin-name-shaped prefix used in
// "当前只有 x[y]" when the type exists under a different instance. Neither
// hint is load-bearing; both are just where to look first.
func pkgName(typ reflect.Type) string {
	t := typ
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	pkgPath := t.PkgPath()
	if pkgPath == "" {
		return t.String()
	}
	segs := strings.Split(pkgPath, "/")
	last := segs[len(segs)-1]
	if len(segs) >= 2 && isMajorVersionSegment(last) {
		last = segs[len(segs)-2]
	}
	return last
}

// zeroPluginOf builds a zero-value Plugin of the given concrete type so we
// can name a plugin that was never registered at all -- t is always a
// pointer-to-struct in practice (RefOf[T Plugin]() requires T to satisfy
// Plugin, and every plugin in this framework is a pointer receiver), but the
// non-pointer branch is kept for defensiveness rather than assuming that
// convention is enforced elsewhere.
func zeroPluginOf(t reflect.Type) Plugin {
	if t.Kind() == reflect.Ptr {
		return reflect.New(t.Elem()).Interface().(Plugin)
	}
	return reflect.New(t).Elem().Interface().(Plugin)
}

// refPluginName recovers a Ref's plugin name purely from its type, with no
// instance required -- this is what makes the "但 x 未启用" message possible
// even when x was never registered at all. It deliberately mirrors
// deriveName's package-path rule rather than calling zeroPluginOf(t).Name():
// Name() can be overridden to anything (spec §5.4a, "想覆盖就自己写"), but the
// config section a user is told to add is always keyed by the package-
// derived name, not by whatever Name() happens to return.
func refPluginName(r Ref) string {
	name, err := deriveName(zeroPluginOf(r.typ))
	if err != nil {
		return pkgName(r.typ)
	}
	return name
}
```

---

- [ ] **Step 4: 跑测试确认通过**

```bash
cd /Users/10097292/Desktop/caffe/xbcio/xbc
go test ./ -run TestResolve -v
```

Expected: 18 个测试全部 PASS（16 个 `TestResolve_*` 加上 `TestResolveScansTagsItself` 与 `TestResolveRejectsMalformedTag`）。重点复核四处容易假通过的地方：

0. `TestResolveScansTagsItself` 是唯一一个不经 `newInst`、直接构造裸 `*instance` 的测试——它专门盯着 pass 0。其余测试的 `fields` 都由夹具预先扫好，把 pass 0 从 `resolve` 里删掉它们照样全绿，只有这一个会红。看到它失败先去看 pass 0 还在不在，别去改夹具。

1. `TestResolve_NamedInstanceMissing` 与 `TestResolve_RefInstanceNarrowedMissing` 的 "当前只有" 列表必须列出**全部**已存在的实例，不是只列第一个——如果实现里 `alt` 切片用了 `if len(alt) == 0` 之类的提前 `break`，这两个测试会在只注册一个实例的变体下假通过，多实例变体才会把 bug 暴露出来（本 task 两个测试都特意注册了 ≥2 个实例，就是为了堵住这条假通过路径）。
2. `TestResolve_MultipleErrorsReportedTogether` 断言两条错误**同时**出现在一个 `error` 里；如果实现在扫到第一条错误后就提前 `return`，这个测试会失败在第二个 `assert.Contains` 上，而不是编译不过——容易被当成"顺手加个 assert 没通过"忽略掉，要按报错信息去核对 `errLines` 是否真的收集了两条。
3. `TestResolve_Cycle` 只断言了 `"→"` 和 `"xbc: 依赖成环"` 两个子串，没有钉死具体路径顺序——`graph.CycleError.Path` 从哪个节点开始视 `internal/graph`（Task 3）的 DFS 实现而定，本 task 不应该也不需要对着一个具体起点断言，那是在测别人的实现细节。

---

- [ ] **Step 5: Commit**

```bash
cd /Users/10097292/Desktop/caffe/xbcio/xbc
gofmt -l . && go vet ./... && go test ./... -count=1
git add stage_resolve.go stage_resolve_test.go xbc.go
git commit -m "feat: 阶段 4 依赖解析——合并依赖图两端、注册表索引、拓扑排序"
```

三件套必须全绿才提交：`gofmt -l .` 空输出，`go vet ./...` 无输出，`go test ./... -count=1` 全绿（不止 `TestResolve_*`，因为 `stage_resolve.go` 新增的 `mergeDeps`/`mergeProvides` 会把 `inst.deps`/`inst.provides` 从零值变成非零值，之前 task 里任何断言过这两个字段是零值的测试都可能因此翻车——如果真翻车，说明那些测试原本就在偷偷假设阶段 4 不存在，得一起修，不能靠跳过阶段 4 的测试蒙混过关）。

### Task 11: `mwchain.go` —— 中间件排序（Phase 硬边界 + 组内软序 + 跨 Phase 反向检测）

**Files:**
- Create: `mwchain.go`
- Modify: `middleware.go`（补 `Phase.String()`、`qualify`）
- Test: `mwchain_test.go`

**Interfaces:**
- Consumes（前置 task 的产物，本 task 不重复实现，只引用）：
  - `type Phase int`，常量 `PhaseRecover Phase = iota * 100`、`PhaseObserve`、`PhaseSecurity`、`PhaseAuth`、`PhaseBusiness`（Task 1，`middleware.go`，裁决 R4 已经把间隔定死在骨架里，本 task 不改常量本身）
  - `type Middleware struct{ Name string; Phase Phase; After, Before []string; Handler gin.HandlerFunc }`（Task 1，`middleware.go`）
  - `package graph`：`func New() *Graph`、`func (g *Graph) AddNode(id string)`、`func (g *Graph) AddEdge(from, to string, hard bool)`、`func (g *Graph) Sort() (order []string, misses []Miss, err error)`、`type Miss struct{ Node, Ref, Dir string }`、`type CycleError struct{ Path []string }`（Task 3，`internal/graph`）
- Produces:
  - `func (p Phase) String() string`（`middleware.go`）—— `"recover"` / `"observe"` / `"security"` / `"auth"` / `"business"`，越界值渲染为 `"phase(N)"`
  - `func qualify(plugin, name string) string`（`middleware.go`）—— 裁决 R5
  - `type mwEntry struct{ Middleware; qname string; plugin string }`（`mwchain.go`）
  - `func orderMiddlewares(entries []mwEntry) (ordered []mwEntry, misses []graph.Miss, err error)`（`mwchain.go`）
  - `type PhaseConflictError struct{ From, To string; FromPhase, ToPhase Phase; Dir string }` 及 `func (e *PhaseConflictError) Error() string`（`mwchain.go`）

**为什么 `Phase` 是硬边界，而不是把 `After`/`Before` 一股脑丢进一张全局图排序。** 五个 `Phase` 常量本身就是一份粗粒度的、经过设计的顺序声明——recover 必须包住一切，业务中间件必须在认证之后——这份顺序不该被插件作者的一个笔误推翻。如果把全部 `After`/`Before` 边和 `Phase` 混在同一张图里跑拓扑排序，一条跨 Phase 的软约束不管方向对不对，都会被排序算法「消化」掉，出来的链路可能把业务中间件排到 recover 前面而不报任何错——这就是 spec 自己点名的「错误的洋葱」。所以排序分两层：先按 `Phase` 分组（组间顺序只由 `Phase` 的数值决定，不受任何 `After`/`Before` 影响），组内才用 `internal/graph` 跑真正的拓扑排序。跨 Phase 的 `After`/`Before` 只做一件事——**核对方向**：方向与 `Phase` 顺序天然一致就是冗余声明（比如业务中间件写 `After` 一个认证中间件，反正业务本来就在认证之后），直接丢弃；方向相反就是插件作者对两个中间件的相对位置有着与框架结论矛盾的预期，唯一诚实的做法是中止启动并把矛盾的两端都打印出来，而不是悄悄选一个方向替它做决定。

**为什么 R4 的间隔本身在这份代码里不需要被用到，但仍然要裁决。** `PhaseRecover - 1` 这个逃生舱口能不能用，纯粹取决于 `Phase` 底层是不是 `iota`（无间隔时 `PhaseRecover - 1 == -1`，凑巧可用但完全是意外）还是 `iota * 100`（有意留出的区间，`-1` 落在区间内，是「刻意」的一部分）。本 task 不给这个值起名字、不导出常量、不在 `orderMiddlewares` 里做任何特殊分支——它能工作只是因为 `Phase` 是普通的 `int`，任何整数都能塞进 `Middleware.Phase` 字段。文档里必须点破这一点：写 `xbc.PhaseRecover - 1` 的中间件會被排到 recover 之前，framework 自带的 panic 兜底盖不住它，它里面的 panic 会直接打挂进程。不给它一个体面的名字，就是不让它看起来像一个正常选项。

**为什么 `qualify` 之后重名要报错，而不是「后一个覆盖前一个」或者「都留着，谁也别报」。** `After`/`Before` 是纯字符串引用，框架分不清 `After: "cors"` 指的是哪一个 `cors`——如果两个不相干的中间件在 `qualify` 之后撞成了同一个 `qname`，那么任何指向它的软约束从此刻开始就是在两个候选之间掷骰子，且这个骰子在每次启动、每次代码改动后都可能摇出不同结果。宁可在装配阶段就报错拒绝启动，也不要把一个不确定性留到运行时的某次请求里才现出原形。

**同一套排序器服务三处，这也是 `internal/graph` 存在的理由。** 插件 `Init` 顺序（硬依赖 ∪ 软 `After`/`Before`）、中间件顺序（`Phase` + 软 `After`/`Before`）、`Stop` 顺序（`Init` 顺序取逆）——三处都是「一组节点、若干软/硬边、需要一个可复现的拓扑序」，框架里只写一份排序算法（`internal/graph.Graph`），`mwchain.go` 不重新发明一套只服务中间件的排序逻辑，只是在 `Phase` 分组之后把组内的排序工作原样甩给它。

- [ ] **Step 1: 写失败测试**

创建 `mwchain_test.go`：

```go
package xbc

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/graph"
)

// newFixtureEntry builds an mwEntry the way the assembly stage would: a raw
// Middleware plus the plugin name that qualify needs. The Handler is a bare
// placeholder -- this task never dispatches an HTTP request, it only orders
// entries by name.
func newFixtureEntry(plugin, name string, phase Phase, after, before []string) mwEntry {
	return mwEntry{
		Middleware: Middleware{
			Name:    name,
			Phase:   phase,
			After:   after,
			Before:  before,
			Handler: func(*gin.Context) {},
		},
		qname:  qualify(plugin, name),
		plugin: plugin,
	}
}

func qnames(entries []mwEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.qname
	}
	return out
}

func TestPhaseStringKnownAndOutOfRange(t *testing.T) {
	cases := []struct {
		phase Phase
		want  string
	}{
		{PhaseRecover, "recover"},
		{PhaseObserve, "observe"},
		{PhaseSecurity, "security"},
		{PhaseAuth, "auth"},
		{PhaseBusiness, "business"},
		{Phase(150), "phase(150)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.phase.String(), "Phase(%d) 的字符串形式", int(c.phase))
	}
}

// In spec §4.4's startup log, the cors plugin's cors middleware and the
// ratelimit plugin's ratelimit middleware both render as a bare, unprefixed
// name -- two real examples of the "same name gets no prefix" rule.
func TestQualifySameNameAsPluginIsNotPrefixed(t *testing.T) {
	assert.Equal(t, "cors", qualify("cors", "cors"),
		"spec §4.4 例子：cors 插件的 cors 中间件不应显示成 cors.cors")
	assert.Equal(t, "ratelimit", qualify("ratelimit", "ratelimit"),
		"spec §4.4 例子：ratelimit 插件的 ratelimit 中间件同理")
}

// In spec §4.4's startup log, the jwt plugin's auth middleware renders as
// jwt.auth -- a real example of the "a different name gets the plugin
// prefix" rule.
func TestQualifyDifferentNameGetsPluginPrefix(t *testing.T) {
	assert.Equal(t, "jwt.auth", qualify("jwt", "auth"),
		"spec §4.4 例子：jwt 插件的 auth 中间件应显示为 jwt.auth")
}

// spec gives no real example for a name that already contains a dot, so a
// synthetic case verifies it: once a name has already been qualified (no
// matter by whom), qualify must pass it through unchanged, never adding
// another layer of plugin-name prefix.
func TestQualifyDottedNamePassesThroughUnchanged(t *testing.T) {
	assert.Equal(t, "jwt.auth", qualify("audit", "jwt.auth"),
		"名字里已经带点号，说明调用方已经完成过一次限定，不能被再套一层插件名前缀")
}

func TestOrderMiddlewaresGroupsByPhaseAscendingRegardlessOfRegistrationOrder(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("business", "business", PhaseBusiness, nil, nil),
		newFixtureEntry("recover", "recover", PhaseRecover, nil, nil),
		newFixtureEntry("security", "security", PhaseSecurity, nil, nil),
		newFixtureEntry("auth", "auth", PhaseAuth, nil, nil),
		newFixtureEntry("observe", "observe", PhaseObserve, nil, nil),
	}

	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"recover", "observe", "security", "auth", "business"}, qnames(ordered),
		"Phase 是硬边界，最终顺序必须按 Phase 升序排列，与注册顺序无关")
}

func TestOrderMiddlewaresWithinGroupRegistrationOrderIsStableAcrossManyRuns(t *testing.T) {
	build := func() []mwEntry {
		return []mwEntry{
			newFixtureEntry("c", "c", PhaseSecurity, nil, nil),
			newFixtureEntry("a", "a", PhaseSecurity, nil, nil),
			newFixtureEntry("b", "b", PhaseSecurity, nil, nil),
		}
	}
	want := []string{"c", "a", "b"}
	for i := 0; i < 100; i++ {
		ordered, misses, err := orderMiddlewares(build())
		require.NoError(t, err)
		assert.Empty(t, misses)
		assert.Equal(t, want, qnames(ordered), "第 %d 次运行：组内无约束的中间件必须按注册顺序稳定排列", i)
	}
}

func TestOrderMiddlewaresWithinGroupAfterConstraintTakesEffect(t *testing.T) {
	// Registration order is ratelimit first, cors second; if After had no
	// effect, the stable sort would keep this registration order. Once
	// ratelimit declares After=cors, the result must be reversed.
	entries := []mwEntry{
		newFixtureEntry("ratelimit", "ratelimit", PhaseSecurity, []string{"cors"}, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"cors", "ratelimit"}, qnames(ordered),
		"ratelimit 声明 After=cors，即便注册顺序是 ratelimit 在前，排序结果也必须把 cors 排到前面")
}

func TestOrderMiddlewaresWithinGroupBeforeConstraintTakesEffect(t *testing.T) {
	// Registration order is ratelimit first, cors second; cors declares
	// Before=ratelimit, so the result must move cors ahead of ratelimit,
	// opposite to registration order.
	entries := []mwEntry{
		newFixtureEntry("ratelimit", "ratelimit", PhaseSecurity, nil, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, []string{"ratelimit"}),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"cors", "ratelimit"}, qnames(ordered),
		"cors 声明 Before=ratelimit，即便注册顺序是 ratelimit 在前，排序结果也必须把 cors 排到前面")
}

// spec §5.8's audit example: audit (PhaseBusiness) declares After:
// "jwt.auth" (PhaseAuth). 300 < 400, so jwt.auth already sorts before audit;
// this cross-phase constraint agrees with Phase order and is a redundant
// declaration, which must be silently ignored -- no error, and it must not
// land in misses either.
func TestOrderMiddlewaresCrossPhaseConsistentConstraintIsRedundantAndIgnored(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, []string{"jwt.auth"}, nil),
		newFixtureEntry("jwt", "auth", PhaseAuth, nil, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "audit(business) After jwt.auth(auth) 与 Phase 顺序一致，是冗余约束，不应中止启动")
	assert.Empty(t, misses)
	assert.Equal(t, []string{"jwt.auth", "audit"}, qnames(ordered))
}

// The reverse case: a PhaseSecurity middleware declares After: "jwt.auth"
// (PhaseAuth). security(200) sorts before auth(300), but After demands
// jwt.auth sort before it -- the direction contradicts Phase order, so
// startup must abort and print both middleware names and both Phase names.
func TestOrderMiddlewaresCrossPhaseReversedConstraintAbortsWithPhaseConflictError(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("early", "check", PhaseSecurity, []string{"jwt.auth"}, nil),
		newFixtureEntry("jwt", "auth", PhaseAuth, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "early.check(security) After jwt.auth(auth) 与 Phase 顺序相反，必须中止启动")

	var conflict *PhaseConflictError
	require.ErrorAs(t, err, &conflict, "冲突必须能还原成 *PhaseConflictError，供上层区分于其他失败原因")
	assert.Equal(t, "early.check", conflict.From)
	assert.Equal(t, "jwt.auth", conflict.To)
	assert.Equal(t, PhaseSecurity, conflict.FromPhase)
	assert.Equal(t, PhaseAuth, conflict.ToPhase)
	assert.Contains(t, err.Error(), "early.check", "错误信息必须包含声明约束的中间件名")
	assert.Contains(t, err.Error(), "jwt.auth", "错误信息必须包含被引用的中间件名")
	assert.Contains(t, err.Error(), "security", "错误信息必须包含声明方的 Phase 名")
	assert.Contains(t, err.Error(), "auth", "错误信息必须包含被引用方的 Phase 名")
}

func TestOrderMiddlewaresMissingReferenceIsRecordedAsMiss(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, []string{"tracing"}, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "引用不存在的名字是软约束未命中，不应中止启动")
	require.Len(t, misses, 1)
	assert.Equal(t, graph.Miss{Node: "audit", Ref: "tracing", Dir: "after"}, misses[0])
	assert.Equal(t, []string{"audit"}, qnames(ordered))
}

func TestOrderMiddlewaresDuplicateQualifiedNameFails(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "限定后重名，After/Before 靠名字引用会失去唯一性，必须报错")
	assert.Contains(t, err.Error(), "cors")
}

func TestOrderMiddlewaresCycleWithinGroupFails(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("a", "a", PhaseSecurity, []string{"b"}, nil),
		newFixtureEntry("b", "b", PhaseSecurity, []string{"a"}, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "a After b 且 b After a，组内出现环，必须报错")

	var cycleErr *graph.CycleError
	assert.ErrorAs(t, err, &cycleErr, "组内成环应能还原成 internal/graph 的 CycleError")
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test . -run 'TestPhaseString|TestQualify|TestOrderMiddlewares' -v
```

Expected: 编译失败。`middleware.go` 目前只有 `Phase`/`Middleware` 骨架，没有 `String` 方法、没有 `qualify`；`mwchain.go` 还不存在。编译器会报出一串 `undefined`：`undefined: qualify`、`undefined: mwEntry`、`undefined: orderMiddlewares`、`undefined: PhaseConflictError`，以及 `c.phase.String undefined (type Phase has no field or method String)`。

- [ ] **Step 3: 补 `middleware.go`**

在 `middleware.go` 已有的 `import` 块里补上 `"fmt"` 与 `"strings"`（`gin` 那一行不动），并在文件末尾追加：

```go
// String renders the phase name used in startup logs and error copy. An
// out-of-range value -- reached only via the PhaseRecover-1 escape hatch
// described in the design doc, or a stray literal -- falls back to
// "phase(N)" instead of an empty string, so it is always safe to print.
func (p Phase) String() string {
	switch p {
	case PhaseRecover:
		return "recover"
	case PhaseObserve:
		return "observe"
	case PhaseSecurity:
		return "security"
	case PhaseAuth:
		return "auth"
	case PhaseBusiness:
		return "business"
	default:
		return fmt.Sprintf("phase(%d)", int(p))
	}
}

// qualify applies ruling R5. A name that already contains "." is assumed to
// be pre-qualified and passes through unchanged; a name identical to its own
// plugin's name is left bare (so a plugin named "cors" registering a
// middleware named "cors" doesn't render as "cors.cors"); everything else
// gets the plugin name prefixed so names stay unique across plugins.
func qualify(plugin, name string) string {
	if strings.Contains(name, ".") {
		return name
	}
	if name == plugin {
		return plugin
	}
	return plugin + "." + name
}
```

`qualify` 之所以只看 `name` 本身要不要拼前缀，完全不检查 `plugin` 参数的形状——这个函数不负责验证插件名合法性，那是 `validateName`（`plugin.go`，Task 1）的职责。两个函数各管一段，`qualify` 只管「怎么拼」。

- [ ] **Step 4: 写 `mwchain.go` 实现**

创建 `mwchain.go`：

```go
package xbc

import (
	"fmt"
	"sort"

	"github.com/xbcio/xbc/internal/graph"
)

// mwEntry is one middleware after name qualification. qname is the graph
// node id and the identity After/Before reference; plugin is kept for error
// copy that needs to name the owning plugin, not just the middleware.
type mwEntry struct {
	Middleware
	qname  string
	plugin string
}

// PhaseConflictError reports a cross-phase After/Before constraint whose
// direction contradicts Phase order. Phase is a hard boundary (spec §5.8
// rule 1): the only two ways to honor a constraint that fights it are to
// silently reorder the phases (the "wrong onion" the design doc explicitly
// rejects) or to abort. This type carries both middleware names and both
// phase names so the startup log can point at the exact contradiction.
type PhaseConflictError struct {
	From, To           string
	FromPhase, ToPhase Phase
	Dir                string // "after" or "before"
}

func (e *PhaseConflictError) Error() string {
	return fmt.Sprintf(
		"xbc: 中间件 %s（阶段 %s）声明 %s=%q（阶段 %s），与 Phase 先后顺序矛盾，无法排出一致的中间件链",
		e.From, e.FromPhase, e.Dir, e.To, e.ToPhase,
	)
}

// orderMiddlewares groups entries by Phase -- a hard boundary, so the coarse
// position of every entry is fixed before any After/Before is even looked
// at -- sorts each group internally with internal/graph (the same sorter
// stage 4 uses for plugin Init order and Stop order), then concatenates the
// groups in ascending Phase order.
//
// A cross-phase After/Before that names a real, resolvable middleware is
// only ever checked for direction, never turned into a sort edge: adding it
// as an edge would let a single soft constraint silently override the Phase
// boundary. A same-direction cross-phase constraint is redundant (Phase
// order already satisfies it) and is dropped; an opposite-direction one
// aborts with *PhaseConflictError. A reference to a name that was never
// registered at all -- same phase or not -- is reported as a graph.Miss and
// otherwise ignored, matching the soft-edge semantics of internal/graph.
func orderMiddlewares(entries []mwEntry) (ordered []mwEntry, misses []graph.Miss, err error) {
	byName := make(map[string]*mwEntry, len(entries))
	for i := range entries {
		e := &entries[i]
		if _, dup := byName[e.qname]; dup {
			return nil, nil, fmt.Errorf(
				"xbc: 中间件名 %q 重复注册，After/Before 靠名字引用，重名会让引用变成掷骰子", e.qname)
		}
		byName[e.qname] = e
	}

	phaseGraphs := make(map[Phase]*graph.Graph)
	var phaseOrder []Phase
	graphFor := func(ph Phase) *graph.Graph {
		g, ok := phaseGraphs[ph]
		if !ok {
			g = graph.New()
			phaseGraphs[ph] = g
			phaseOrder = append(phaseOrder, ph)
		}
		return g
	}

	// Register every node before any edge, in original registration order,
	// so the stable tiebreak inside internal/graph reflects registration
	// order rather than the order edges happen to be discovered in below.
	for _, e := range entries {
		graphFor(e.Phase).AddNode(e.qname)
	}

	for i := range entries {
		e := &entries[i]
		for _, ref := range e.After {
			other, ok := byName[ref]
			if !ok {
				misses = append(misses, graph.Miss{Node: e.qname, Ref: ref, Dir: "after"})
				continue
			}
			switch {
			case other.Phase == e.Phase:
				// Same group: a real ordering edge for the intra-group sort below.
				graphFor(e.Phase).AddEdge(other.qname, e.qname, false)
			case other.Phase > e.Phase:
				// e wants "other" before it, but other's Phase already
				// places it after e -- direction contradicts Phase order.
				return nil, nil, &PhaseConflictError{
					From: e.qname, To: other.qname,
					FromPhase: e.Phase, ToPhase: other.Phase, Dir: "after",
				}
			}
			// other.Phase < e.Phase: Phase order already puts other first;
			// the constraint is redundant, and is dropped here.
		}
		for _, ref := range e.Before {
			other, ok := byName[ref]
			if !ok {
				misses = append(misses, graph.Miss{Node: e.qname, Ref: ref, Dir: "before"})
				continue
			}
			switch {
			case other.Phase == e.Phase:
				graphFor(e.Phase).AddEdge(e.qname, other.qname, false)
			case other.Phase < e.Phase:
				// e wants to come before "other", but other's Phase
				// already places it before e -- direction contradicts.
				return nil, nil, &PhaseConflictError{
					From: e.qname, To: other.qname,
					FromPhase: e.Phase, ToPhase: other.Phase, Dir: "before",
				}
			}
			// other.Phase > e.Phase: Phase order already puts e first;
			// the constraint is redundant, and is dropped here.
		}
	}

	sort.Slice(phaseOrder, func(i, j int) bool { return phaseOrder[i] < phaseOrder[j] })

	ordered = make([]mwEntry, 0, len(entries))
	for _, ph := range phaseOrder {
		// Every node and edge in this graph belongs to entries already
		// known to exist in byName, so Sort's own miss-reporting never
		// fires here; a non-nil err can only be a cycle within this phase.
		order, _, sortErr := phaseGraphs[ph].Sort()
		if sortErr != nil {
			return nil, nil, fmt.Errorf("xbc: 阶段 %s 内的中间件排序失败: %w", ph, sortErr)
		}
		for _, name := range order {
			ordered = append(ordered, *byName[name])
		}
	}
	return ordered, misses, nil
}
```

- [ ] **Step 5: 跑测试确认通过**

```bash
go test . -run 'TestPhaseString|TestQualify|TestOrderMiddlewares' -v
go test ./... -count=1
```

Expected: 全部 PASS，其中 `TestOrderMiddlewaresWithinGroupRegistrationOrderIsStableAcrossManyRuns` 内部循环 100 次全部命中同一个顺序。

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
git add middleware.go mwchain.go mwchain_test.go
git commit -m "feat(xbc): mwchain 中间件排序——Phase 硬边界、组内软序、跨 Phase 反向检测"
```
### Task 12: `goroutine.go` —— `Context.Go` / `Context.GoCritical` 托管 goroutine

**Files:**
- Create: `goroutine.go`
- Modify: `context.go`（补 `Go` / `GoCritical` 两个方法体；Task 1 只留了 `Context` 骨架，`kernel-api.md` §6 明确把这两个方法记在 `context.go` 名下）
- Verify: `xbc.go`（**只核对，不新增**。托管 goroutine 用到的七个字段——`runCtx` / `cancel` / `wg` / `criticalCh` / `criticalOnce` / `criticalReason` / `exitCode`——已由 Task 1 一次性声明在 `App` 里，见裁决 R12。本 task 只写它们的读写代码，再新增一遍就是 `duplicate field`，`go build` 直接红）
- Test: `goroutine_test.go`

**Interfaces:**
- Consumes（前置 task 的产物，本 task 不重复实现，只引用）：
  - `type Context struct{ app *App; name, instance string; logger log.Logger }` 及其已有方法 `func (c *Context) Log() log.Logger`、`func (c *Context) Name() string`、`func (c *Context) Instance() string`（Task 1，`context.go` 骨架）
  - `App` 的骨架字段（Task 1，`xbc.go`）。**强调**：托管 goroutine 用到的七个字段已由 Task 1 声明完毕，本 task 只读写、不新增。四个跨 task 共用的字段——`cancel context.CancelFunc`、`wg *sync.WaitGroup`、`criticalCh chan struct{}`、`exitCode int`——`stage_run.go`（Task 14）会直接在它自己的测试里赋值使用，名字与类型必须原样沿用（尤其是 `wg` 是指针、`criticalCh` 是不带数据的 `chan struct{}`），不能按自己的设想另起一套（比如 `wg sync.WaitGroup` 值类型或 `critical chan criticalEvent` 带原因的缓冲 channel）——那样 `stage_run.go` 会编译不过，两份产物就对不上
  - `log.L() log.Logger`、`log.Logger` 接口：`Debug/Info/Warn/Error(msg string, kv ...any)`、`With(kv ...any) Logger`、`Enabled(lv Level) bool`（`log` 包已完成）
- Produces:
  - `func (a *App) initGoroutines()` —— `wg`、`runCtx`、`cancel`、`criticalCh` 四个字段唯一的构造点（`cli.go`，Task 15，会在阶段 5 Init 之前调用它；本 task 不实现 `cli.go`，只把这个函数准备好）
  - `App` 已声明字段的首个消费点（**不新增声明**）：`runCtx context.Context`、`criticalOnce sync.Once`、`criticalReason string`
  - `func (c *Context) Go(fn func(context.Context))`
  - `func (c *Context) GoCritical(fn func(context.Context))`
  - `func (a *App) goManaged(ctx *Context, fn func(context.Context), critical bool)`
  - `func (a *App) triggerCritical(reason string)`

**这个 task 到底在防什么。** `Runner.Start` 必须快速返回，长期循环交给 `ctx.Go` / `ctx.GoCritical` 托管（spec §5.2）。但"panic 恢复、记日志、继续"这条兜底逻辑，对所有后台任务一刀切是错的。一个 Kafka consumer 的消费循环 panic 之后，如果框架的反应是"恢复、记一行 error、goroutine 静静地结束"，进程本身完全正常：还在运行、`/healthz` 还返回 200、k8s 的存活探针看不出任何异常。但这个 consumer 已经死了——**消息从此静静地堆积**，没有人在读。这比进程直接崩溃糟糕得多：崩溃会被 k8s 立刻重启、被告警立刻捕获；一个"看起来健康但核心循环已经死透"的僵尸进程不会触发任何东西，直到下游的积压严重到有人去查监控为止。

所以后台任务不能用同一套兜底策略，要拆成两种语义，判据是一句话：**这个 goroutine 死了，应用还算不算健康？** 不算，就用 `GoCritical`；算，就用 `Go`。

| | `Go` | `GoCritical` |
|---|---|---|
| panic | 恢复，记 error 日志，goroutine 结束 | 恢复，记 error 日志，**触发 shutdown** |
| 正常返回（非 shutdown 触发） | 视为任务完成，无事发生 | 视为**意外退出**，触发 shutdown |
| shutdown 期间返回 | 正常，计入等待组 | 正常，计入等待组 |

`GoCritical` 触发的关闭不是 `os.Exit`，而是走**完整的阶段 10**（`stage_run.go` 的 `shutdown`，Task 14）：正在处理的 HTTP 请求照样排空，其他插件照样按逆拓扑序 `Stop`，只是最终退出码是 `1`——让编排系统知道这是一次异常退出，而不是一次干净关闭。本 task 的职责就到"点燃这根引线"为止：`triggerCritical` 关掉 `criticalCh`、取消 `runCtx`；`stage_run.go` 的 `serve()` 已经在 `select` 里等着 `<-a.criticalCh`，接到信号后调用 `a.shutdown("critical")`，那里面才是设 `a.exitCode = 1` 的地方。两段代码通过 `criticalCh` 这个信号做接缝，谁都不用知道对方的实现细节。

**关于"可测性不碰 `os.Exit`"这条设计怎么落地。** `kernel-api.md` 把 `func (a *App) run(args []string) (exitCode int, err error)` 列为整条流水线的可测核心，`Run()` 只做 `os.Exit(a.run(os.Args[1:]))` 这一层皮。但 `run` 本身要串起全部十个阶段，得等 `cli.go`（Task 15）落地，而本 task 在执行顺序上排在 `stage_init.go`（Task 13）、`stage_run.go`（Task 14）之前——写这段代码时，仓库里还没有 `run`，也还没有 `serve`/`shutdown` 可以调。所以本 task 没有、也不需要整条流水线来测：它只对外承诺一件事——`triggerCritical` 被调用之后，`criticalCh` 关闭且 `runCtx` 被取消，这正是 `stage_run.go` 的 `serve()` 拿去驱动 `a.shutdown("critical")`、进而把 `a.exitCode` 置 `1` 的那个信号源（Task 14 已经有一条端到端测试 `TestGoCriticalTriggersFullShutdownAndExitCodeOne` 覆盖了"信号 → 完整关闭 → 退出码 1"这条链路，只是它测试里是手写 `close(a.criticalCh)` 模拟触发，并没有真的调用本 task 的 `triggerCritical`）。本 task 的测试因此验证到"信号被正确点燃"这一层就足够、也只能到这一层，并在测试里显式断言这层契约（`criticalCh` 已关闭、`runCtx.Err() != nil`），把它和 Task 14 那条测试的边界画清楚，避免日后有人以为本 task 该测退出码却无从下手。

**为什么 `criticalCh` 是 `chan struct{}` 关闭一次，不是带原因的缓冲 channel。** 关闭一个 channel 天然是"广播"语义——`serve()` 的 `select` 只需要知道"出事了"，不需要知道原因，原因只用于日志。但两个 `GoCritical` goroutine 可能几乎同时炸掉，第二次 `close` 会 panic（对已关闭的 channel 再关一次是运行时错误），所以必须用 `sync.Once` 守住"只关一次"。至于"非阻塞"：这里防的不是"给 `criticalCh` 发信号会不会卡住"（`close` 从不阻塞），而是防两个并发的失败 goroutine 互相拖累——`sync.Once.Do` 保证第二个调用者最多等第一个调用者跑完那几行（记原因、打日志、`close`）就返回，不会被挂在一个没人接收的 channel 发送上无限等待。这是"非阻塞"这个词在这里的真实含义，跟"channel 是否带缓冲"无关。

**面板测试用的假 Logger。** `log/zap_test.go` 里的 `fakeBackend` 只是把 `Nop()` 包了一层壳，用来测试 `ZapProvider` 类型断言失败的路径，并不记录任何调用，对本 task 没用；`log/mask_test.go`、`log/sugar_test.go` 等测试用的 `observer.ObservedLogs` 绑定的是 zap 的真实后端，跨包不可见也不必要——本 task 只需要一个满足 `log.Logger` 六个方法、并发安全地记录 `Error` 调用的假实现，`goroutine_test.go` 里自己写一个不到 20 行的 `recordingLogger`，没有可复用的现成件。

- [ ] **Step 1: 写失败的测试（上）—— 假 Logger 与测试夹具**

先在 `goroutine_test.go` 里搭好本 task 全部测试共用的部分：假 `log.Logger`、测试用的 `App`/`Context` 构造器。这一步先写这些，不写具体测试函数体，下一步再续。

创建 `goroutine_test.go`：

```go
package xbc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
)

// recordingLogger is a minimal log.Logger fake that records every call
// under a mutex. None of this package's existing test helpers fit: the log
// package's own fakes either discard everything (zap_test.go's fakeBackend)
// or bind to a real zap backend via zaptest/observer, which isn't visible
// outside the log package. Managed-goroutine tests only need to assert
// "an error was logged" and inspect its fields, so a small local recorder
// is simpler than reaching for either.
type logRecord struct {
	level log.Level
	msg   string
	kv    []any
}

type recordingLogger struct {
	mu      *sync.Mutex
	records *[]logRecord
}

func newRecordingLogger() *recordingLogger {
	return &recordingLogger{mu: &sync.Mutex{}, records: &[]logRecord{}}
}

func (l *recordingLogger) record(lv log.Level, msg string, kv []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.records = append(*l.records, logRecord{level: lv, msg: msg, kv: kv})
}

func (l *recordingLogger) Debug(msg string, kv ...any) { l.record(log.DebugLevel, msg, kv) }
func (l *recordingLogger) Info(msg string, kv ...any)  { l.record(log.InfoLevel, msg, kv) }
func (l *recordingLogger) Warn(msg string, kv ...any)  { l.record(log.WarnLevel, msg, kv) }
func (l *recordingLogger) Error(msg string, kv ...any) { l.record(log.ErrorLevel, msg, kv) }
func (l *recordingLogger) With(...any) log.Logger      { return l }
func (l *recordingLogger) Enabled(log.Level) bool      { return true }

func (l *recordingLogger) errorCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range *l.records {
		if r.level == log.ErrorLevel {
			n++
		}
	}
	return n
}

// newGoroutineTestApp builds a bare App with only the machinery Go/
// GoCritical touch -- cfg/registry/order etc. belong to other stages and
// are deliberately left nil, since this task's tests never reach them.
func newGoroutineTestApp(t *testing.T) *App {
	t.Helper()
	a := &App{}
	a.initGoroutines()
	return a
}

// newGoroutineTestContext wires a Context to a into a app for goroutine
// tests, with logger swapped for a recordingLogger so tests can assert on
// what got logged without touching the real log backend.
func newGoroutineTestContext(a *App, name string) (*Context, *recordingLogger) {
	rl := newRecordingLogger()
	ctx := &Context{app: a, name: name, instance: "default", logger: rl}
	return ctx, rl
}
```

- [ ] **Step 2: 跑测试确认失败（上）**

```bash
go test . -run 'TestGoroutinePlaceholder' -v
```

Expected: 编译失败，`undefined: (*App).initGoroutines`——`goroutine.go` 还不存在，`newGoroutineTestApp` 引用不到它。这一步的目的只是确认夹具本身能暴露出正确的缺口，具体测试函数下一步才写。

- [ ] **Step 3: 写失败的测试（下）—— 具体测试函数**

在 `goroutine_test.go` 末尾追加以下测试函数（`Step 1` 的 import 块已经够用，不用改）。全部同步只靠 channel / `sync.WaitGroup`，`time.After` 只在两处出现，且只当"死锁兜底"用——真正判断"事情发生没有"永远是 channel 接收或 `errorCount()`，`time.After` 触发只意味着测试本身挂了，不参与断言的正确性判断。

```go
// ---- Go: panic recovered but the application keeps running ----

func TestGoPanicRecoveredAppKeepsRunningAndLogsError(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "cron")

	ctx.Go(func(context.Context) {
		panic("模拟 cron 任务 panic")
	})

	a.wg.Wait()
	assert.Equal(t, 1, rl.errorCount(), "panic 必须被恢复并记一条 error 日志")
	select {
	case <-a.criticalCh:
		t.Fatal("Go 的 panic 不能触发 critical 关闭")
	default:
	}
	assert.Nil(t, a.runCtx.Err(), "Go 的 panic 不能取消 runCtx")
}

// ---- Go: a normal return is a non-event ----

func TestGoNormalReturnTriggersNothing(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "refresh")

	ctx.Go(func(context.Context) {})

	a.wg.Wait()
	assert.Equal(t, 0, rl.errorCount(), "正常返回不应该记任何 error 日志")
	select {
	case <-a.criticalCh:
		t.Fatal("Go 正常返回不能触发 critical 关闭")
	default:
	}
}

// ---- Go: cancel arrives during shutdown, and is waited on by the WaitGroup ----

func TestGoObservesShutdownCancelAndIsWaitedOn(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, _ := newGoroutineTestContext(a, "consumer")

	sawDone := make(chan struct{})
	ctx.Go(func(c context.Context) {
		<-c.Done()
		close(sawDone)
	})

	a.cancel()
	<-sawDone // fn must actually observe the cancel signal to reach here, not exit by coincidence

	waitReturned := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(waitReturned)
	}()
	select {
	case <-waitReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("wg.Wait 应该在托管 goroutine 观察到 cancel 并返回后完成")
	}
}

// ---- GoCritical: panic triggers shutdown ----

func TestGoCriticalPanicTriggersShutdown(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "consumer")

	ctx.GoCritical(func(context.Context) {
		panic("模拟 consumer panic")
	})

	<-a.criticalCh // closed means triggered
	a.wg.Wait()
	assert.Equal(t, 1, rl.errorCount(), "panic 必须被恢复并记一条 error 日志")
	assert.NotEmpty(t, a.criticalReason, "触发原因必须被记录")
	require.Error(t, a.runCtx.Err(), "触发 critical 必须取消 runCtx，这正是 stage_run.go 的 shutdown 依赖的信号")
}

// ---- GoCritical: an early normal return also triggers shutdown ----

func TestGoCriticalEarlyReturnTriggersShutdown(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx, rl := newGoroutineTestContext(a, "gateway")

	ctx.GoCritical(func(context.Context) {})

	<-a.criticalCh
	a.wg.Wait()
	assert.Equal(t, 0, rl.errorCount(), "提前正常返回不是 panic，不应该记 error 日志，但仍要触发关闭")
	assert.Contains(t, a.criticalReason, "意外提前返回")
	require.Error(t, a.runCtx.Err())
}

// ---- GoCritical: returning during shutdown does not re-trigger ----

func TestGoCriticalReturnDuringShutdownDoesNotRetrigger(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctxA, _ := newGoroutineTestContext(a, "consumer-a")
	ctxB, rlB := newGoroutineTestContext(a, "consumer-b")

	started := make(chan struct{})
	ctxB.GoCritical(func(c context.Context) {
		close(started)
		<-c.Done() // returns only once consumer-a's panic cancels runCtx
	})
	<-started

	ctxA.GoCritical(func(context.Context) {
		panic("模拟 consumer-a panic")
	})

	a.wg.Wait() // both managed goroutines must return cleanly: no deadlock, and no panic from a second close
	assert.Equal(t, "插件 consumer-a 的托管 goroutine panic: 模拟 consumer-a panic", a.criticalReason,
		"只保留第一个触发原因，consumer-b 的返回不能覆盖它")
	assert.Equal(t, 0, rlB.errorCount(), "consumer-b 是响应 shutdown 的正常返回，不是 panic，不应该记 error 日志")
}

// ---- triggerCritical's signal contract: stage_run.go relies on it for exit code 1 ----
//
// This task sits before stage_init.go / stage_run.go in execution order, and
// the repo does not yet have a complete testable run(), so verification only
// goes as far as "the signal was correctly lit": criticalCh closed, runCtx
// cancelled. The code that actually reads those two signals, drives the full
// stage 10, and sets a.exitCode to 1 is stage_run.go's serve()/shutdown()
// (Task 14's TestGoCriticalTriggersFullShutdownAndExitCodeOne covers that
// part, except its test hand-writes close(a.criticalCh) to simulate the
// trigger, without actually calling triggerCritical).
func TestTriggerCriticalSignalIsWhatStage9ReadsForExitCodeOne(t *testing.T) {
	a := newGoroutineTestApp(t)
	a.triggerCritical("手工触发，验证契约")

	_, stillOpen := <-a.criticalCh
	assert.False(t, stillOpen, "criticalCh 必须已关闭——stage_run.go 的 serve() 在 select 里等的正是这个信号")
	require.Error(t, a.runCtx.Err(),
		"runCtx 必须已取消——真正把 exitCode 置 1 的是 stage_run.go 的 shutdown(\"critical\")，本 task 只负责点燃信号")
}

// ---- Concurrent criticals: keep only the first reason, and never block each other ----

func TestTriggerCriticalConcurrentFailuresKeepOnlyFirstReasonAndDoNotBlock(t *testing.T) {
	a := newGoroutineTestApp(t)
	ctx1, rl1 := newGoroutineTestContext(a, "worker-1")
	ctx2, rl2 := newGoroutineTestContext(a, "worker-2")

	release := make(chan struct{})
	ctx1.GoCritical(func(context.Context) {
		<-release
		panic("worker-1 炸了")
	})
	ctx2.GoCritical(func(context.Context) {
		<-release
		panic("worker-2 炸了")
	})
	close(release) // makes the two panics happen as close to simultaneously as possible

	waitReturned := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(waitReturned)
	}()
	select {
	case <-waitReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("并发 critical 触发不能让任何一个托管 goroutine 卡死")
	}

	assert.Equal(t, 1, rl1.errorCount())
	assert.Equal(t, 1, rl2.errorCount(), "各自的 panic 仍然各自记一条 error 日志，触发 shutdown 这个动作才只认第一个")
	assert.True(t,
		a.criticalReason == "插件 worker-1 的托管 goroutine panic: worker-1 炸了" ||
			a.criticalReason == "插件 worker-2 的托管 goroutine panic: worker-2 炸了",
		"必须恰好保留其中一个原因，不能是空、也不能是两个拼接在一起")
}

// ---- The WaitGroup really does wait for every managed goroutine to return before returning itself ----

func TestWaitGroupBlocksUntilAllManagedGoroutinesReturn(t *testing.T) {
	a := newGoroutineTestApp(t)
	const n = 5
	release := make(chan struct{})
	var mu sync.Mutex
	var finished []int

	for i := 0; i < n; i++ {
		ctx, _ := newGoroutineTestContext(a, "worker")
		i := i
		ctx.Go(func(context.Context) {
			<-release
			mu.Lock()
			finished = append(finished, i)
			mu.Unlock()
		})
	}

	waitReturned := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(waitReturned)
	}()

	close(release)
	select {
	case <-waitReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("释放全部 goroutine 后 wg.Wait 必须返回")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, finished, n, "wg.Wait 返回时必须是全部托管 goroutine 都已经跑完，一个都不能少")
}
```

- [ ] **Step 4: 跑测试确认失败**

```bash
go test . -run 'TestGoPanic|TestGoNormal|TestGoObserves|TestGoCritical|TestTriggerCritical|TestWaitGroupBlocks' -v
```

Expected: 编译失败，`undefined: (*App).initGoroutines`（以及 `ctx.Go`、`ctx.GoCritical`、`a.criticalCh`、`a.runCtx`、`a.criticalReason` 一连串未定义符号）——`goroutine.go` 还没有创建，`Context.Go`/`GoCritical` 也还没有方法体，`App` 也还没有这几个字段。

- [ ] **Step 5: 写实现（一）—— `goroutine.go`**

创建 `goroutine.go`：

```go
package xbc

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/xbcio/xbc/log"
)

// initGoroutines wires up the App-level machinery Context.Go and
// Context.GoCritical depend on: a WaitGroup the shutdown path waits on, a
// cancellable root context every managed goroutine receives, and a signal
// channel stage 9's serve() (stage_run.go) selects on to learn a
// GoCritical failure happened.
//
// cli.go calls this once, before stage 5 (Init) runs -- a plugin's Init
// already receives a live *Context and is free to call ctx.Go from inside
// Init itself, so this machinery must exist before initAll runs, not just
// before startRunners.
func (a *App) initGoroutines() {
	a.wg = &sync.WaitGroup{}
	a.runCtx, a.cancel = context.WithCancel(context.Background())
	a.criticalCh = make(chan struct{})
}

// goManaged is the single engine behind Context.Go and Context.GoCritical
// (their method bodies live in context.go and just forward here -- see
// Step 6). The two are the same mechanism -- spawn, recover, wait --
// differing only in what happens after a panic or an unrequested return,
// so splitting them into separate implementations would just be the same
// code twice with one branch flipped.
//
// Go: a panic inside fn is recovered and logged; the goroutine simply
// ends. A normal return is treated as "the task finished", nothing more --
// the right default for one-shot work like a cache refresh or a metrics
// push, where "it stopped" is not itself bad news.
//
// GoCritical: for goroutines whose death means the application is no
// longer doing its job even though the process is still up -- a Kafka
// consumer, a long-lived gateway connection, a data sync loop. Both a
// panic and an unprompted normal return are treated as the application
// having failed, and trigger a full stage-10 shutdown (not os.Exit):
// in-flight HTTP requests still drain, every other plugin still gets
// Stop() in reverse topological order, and the process exits with code 1
// so the surrounding orchestrator knows this was not a clean stop.
func (a *App) goManaged(ctx *Context, fn func(context.Context), critical bool) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		// expected records whether the eventual return (if fn doesn't
		// panic) happened because shutdown was already underway. It is
		// only read by the deferred recover below, in the same goroutine,
		// after fn has returned -- no concurrent access, no race.
		expected := false

		defer func() {
			if r := recover(); r != nil {
				ctx.Log().Error("xbc: 托管 goroutine panic，已恢复",
					"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
				if critical {
					a.triggerCritical(fmt.Sprintf("插件 %s 的托管 goroutine panic: %v", ctx.Name(), r))
				}
				return
			}
			if critical && !expected {
				a.triggerCritical(fmt.Sprintf("插件 %s 的托管 goroutine 意外提前返回", ctx.Name()))
			}
		}()

		fn(a.runCtx)
		// Reaching here means fn returned without panicking. If runCtx is
		// already cancelled, this return was requested (shutdown is
		// already in flight) and is therefore expected, whatever the
		// reason for the cancellation was. If it isn't cancelled yet,
		// nobody asked this goroutine to stop -- for GoCritical that is
		// exactly the "consumer silently stopped consuming" failure this
		// whole mechanism exists to catch.
		expected = a.runCtx.Err() != nil
	}()
}

// triggerCritical fires the application-wide shutdown alarm. It runs from
// arbitrary, possibly concurrent goroutines, so firing it must itself be
// race-safe and must never leave a caller stuck: criticalOnce guarantees
// exactly one caller does the actual work (recording the reason, logging
// it, closing criticalCh), while every other concurrent caller's Do call
// returns as soon as that tiny critical section finishes -- closing a
// channel twice panics, so collapsing concurrent triggers into one is not
// optional, it is the only thing standing between a double panic-in-a-
// panic and a clean shutdown.
//
// "Non-blocking" here does not mean "channel send with a buffer of 1" --
// closing a channel never blocks regardless of buffering. What it actually
// guards against is a goroutine getting wedged forever waiting on another
// goroutine that will never look at what it sent. Once's guarded function
// always runs to completion and returns quickly and unconditionally, so no
// caller of triggerCritical can get stuck on it.
func (a *App) triggerCritical(reason string) {
	a.criticalOnce.Do(func() {
		a.criticalReason = reason
		log.L().Error("xbc: 收到 critical 信号，即将触发应用关闭", "reason", reason)
		close(a.criticalCh)
	})
	if a.cancel != nil {
		a.cancel()
	}
}
```

- [ ] **Step 6: 写实现（二）—— `context.go` 补 `Go` / `GoCritical` 方法体**

`context.go` 里 Task 1 留下的 `Context` 骨架只有字段和 `Log`/`Config`/`Instance`/`Name`/`Route`/`Routes`，没有 `Go`/`GoCritical`（`kernel-api.md` §6 把这两个方法列在 `context.go` 名下，实现留给本 task）。用 `Edit` 在文件末尾追加：

```go
// Go hands fn to the framework as a managed background goroutine.
// See goroutine.go for the full semantics of Go vs GoCritical.
func (c *Context) Go(fn func(context.Context)) {
	c.app.goManaged(c, fn, false)
}

// GoCritical is Go's stricter twin: a panic or an unprompted return
// triggers a full application shutdown. See goroutine.go for the full
// semantics of Go vs GoCritical.
func (c *Context) GoCritical(fn func(context.Context)) {
	c.app.goManaged(c, fn, true)
}
```

若 `context.go` 顶部尚未 `import "context"`（`Route`/`Routes` 只用到 `*gin.Context`，未必已经引入标准库 `context`），补上这一行 import。这两个方法只做一行转发，真正的引擎（`goManaged`/`triggerCritical`/`initGoroutines`）都在 `goroutine.go`——`kernel-api.md` §6 把 `Go`/`GoCritical` 记在 `context.go` 名下，是"这是 `Context` 对外暴露的方法表"的意思，不是要求实现也写在那个文件里；把状态机搬到 `goroutine.go` 只是让读者不用在两个文件之间跳着找同一套逻辑。如果 Task 1 的 `context.go` 骨架里已经占了这两个方法的空函数体（比如占位 `panic("TODO")`），直接替换方法体为上面的转发代码，不要留两份定义导致重复声明报错。

- [ ] **Step 7: 写实现（三）—— 核对 `xbc.go` 的 `App` 字段**

**这一步不新增字段，只核对。** 按裁决 R12，`App` 的完整字段集在 Task 1 就一次性声明到位了，本 task 用到的七个字段都在里面。打开 `xbc.go`，确认 `type App struct { ... }` 里逐字存在下面这些（注释是 Task 1 写的，措辞可能略有出入，**字段名与类型必须逐字一致**）：

```go
	// -- managed-goroutine bookkeeping (goroutine.go, Task 12) --

	// runCtx is the context every ctx.Go / ctx.GoCritical callback
	// receives. It is cancelled once by cancel, either by a clean shutdown
	// (stage 10) or by triggerCritical -- managed goroutines never see two
	// different contexts across their lifetime.
	runCtx context.Context
	cancel context.CancelFunc

	// wg is waited on by stage 10's shutdown before it starts stopping
	// plugins in reverse topological order -- every managed goroutine must
	// have returned before Stop() runs, otherwise a goroutine could still
	// be mid-flight against a connection Stop is about to tear down.
	wg *sync.WaitGroup

	// criticalCh is closed exactly once, by triggerCritical, the moment a
	// GoCritical callback panics or returns unprompted. Stage 9's serve()
	// selects on it alongside the OS signal channel and its own Serve
	// error, so a GoCritical failure and an operator-sent SIGTERM go
	// through the exact same shutdown path.
	criticalCh chan struct{}

	// criticalOnce guards criticalCh against being closed twice when two
	// GoCritical callbacks fail concurrently -- closing an already-closed
	// channel panics, and only the first failure's reason matters anyway.
	criticalOnce sync.Once
	// criticalReason is the first reason passed to triggerCritical. It is
	// only written inside criticalOnce.Do, so no lock is needed to read it
	// once criticalCh has observably closed.
	criticalReason string

	// exitCode is read by cli.go's run() after serve() returns and handed
	// to os.Exit by Run(). It starts at 0 (clean exit) and is set to 1 by
	// stage_run.go's shutdown when the shutdown reason is "critical".
	exitCode int
```

**任何一个对不上就停下来先补齐**（照上面的字段名与类型加到 `App` 里，并确保 `xbc.go` 顶部 import 了 `context` 与 `sync`），再往下走。不要另起一套名字绕过去——`stage_run.go`（Task 14）读的就是这七个名字，改名等于把编译错误推给下一个 task。

- [ ] **Step 8: 跑测试确认通过**

```bash
go test . -run 'TestGoPanic|TestGoNormal|TestGoObserves|TestGoCritical|TestTriggerCritical|TestWaitGroupBlocks' -v
go test ./... -count=1
go test ./... -race -count=1
```

Expected: 全部 PASS，`-race` 不报数据竞争。本 task 里唯一跨 goroutine 共享的可变状态是 `a.criticalReason`（只在 `criticalOnce.Do` 内写一次）、`recordingLogger.records`（每次读写都在 `mu` 内）、以及测试里的 `finished` 切片（同样锁保护）——`-race` 若报警，八成是某个测试里往 `recordingLogger` 或 `finished` 写数据时漏了锁，检查对应测试的 `mu.Lock()`/`mu.Unlock()` 是否包住了全部访问点。

- [ ] **Step 9: gofmt / vet 三件套**

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
```

Expected: `gofmt -l .` 空输出（没有格式问题的文件），`go vet` 无输出，`go test` 全绿。三条全过才进入下一步。

- [ ] **Step 10: Commit**

```bash
git add goroutine.go goroutine_test.go context.go xbc.go
git commit -m "feat(xbc): Context.Go/GoCritical 托管 goroutine，GoCritical 触发完整 shutdown"
```

### Task 13: 阶段 5 Init —— 注入 → Init → 收割产物 → 校验 → 失败回滚

**Files:**
- Create: `stage_init.go`
- Test: `stage_init_test.go`

**Interfaces:**
- Consumes（前置 task 的产物，本 task 不重复实现，只引用）：
  - `type instance struct{ plugin Plugin; name, instance string; src source; ctx *Context; fields []inject.FieldSpec; deps Deps; provides []Dep; inited bool }` 及其 `(i *instance) id() string` / `(i *instance) label() string`（结构体本身在 Task 1，`fields` 由 Task 10 追加；`id()`/`label()` 在 Task 8；`fields`/`deps`/`provides` 三个字段的值都由 Task 10 的 `resolve` 填好，本 task 只读，**不要重新扫一遍 tag**）
  - `App` 的未导出字段 `cfg *Config`、`registry *registry`（结构体在 Task 1，这两个字段分别由 Task 6、Task 4 追加进去，裁决 R12）
  - `func newRegistry() *registry`、`func (r *registry) put(typ reflect.Type, instance string, v any)`、`func (r *registry) lookup(want reflect.Type, instance string) (any, error)`（Task 4）
  - `func normInstance(s string) string`、`type Dep struct{ Type reflect.Type; Instance string; Optional bool }`、`func Offer[T any]() Dep`、`func typeOf[T any]() reflect.Type`、`(Dep) String() string`（Task 2/3）
  - `func bindBase(p Plugin, ctx *Context, name string) bool`（Task 1，`plugin.go`）
  - `type Context struct{ app *App; name, instance string; logger log.Logger }`（Task 1 骨架，字段未导出但同包可直接构造）
  - `type Provider interface{ Provides() []Dep }`、`type Initializer interface{ Init(ctx *Context) error }`、`type Closer interface{ Stop(ctx context.Context) error }`、`type MultiInstancer interface{ MultiInstance() bool }`（Task 1，`plugin.go`）
  - `func Provide[T any](ctx *Context, v T)`（Task 4，`registry.go`，本 task 的测试夹具会调它，不是本 task 实现它）
  - `inject.Scan(v any) ([]inject.FieldSpec, error)`、`inject.Set(v any, spec inject.FieldSpec, val any) error`、`inject.IsZero(v any, spec inject.FieldSpec) (bool, error)`、`inject.Value(v any, spec inject.FieldSpec) (any, error)`、`inject.FieldSpec{Kind, Type, Instance, Optional, Name}`、`inject.KindInject`、`inject.KindProvide`（Task 7，`internal/inject`）
  - `log.L() log.Logger`（`log` 包已完成）
- Produces:
  - `func newContext(a *App, inst *instance) *Context` —— 阶段 5 是 `Context` 第一次被真正构造出来的地方（`Base.Ctx()` 的文档明确写着「阶段 5 之前是 nil」），所以构造它的活自然落在这个文件里，不是 `context.go` 的事。
  - `func (a *App) initAll(insts []*instance) error`
  - `func (a *App) rollback(insts []*instance)`
  - `func injectInstance(a *App, inst *instance) error`
  - `func harvestInstance(a *App, inst *instance) error`
  - `func validateManualProvides(a *App, inst *instance) error`
  - `func stopInstanceSafely(ctx context.Context, inst *instance)`

**为什么注入非 optional 缺失要在阶段 5 兜底报错，而不是信任阶段 4。** 阶段 4（`resolve`）靠**声明**做静态推断——它看 `inject`/`provide` tag 和 `Dependencies()`/`Provides()` 拼出一张图，从没跑过一行插件代码。阶段 5 看到的是**实际登记的注册表**——是运行时状态。这两者理应一致，但"理应"不是保证：如果哪天 `resolve` 的图算法和 `initAll` 的收割逻辑对"谁的产物叫什么"理解出现偏差（比如一个多实例插件的实例名归一化在两处走了不同的路径），阶段 4 会误判"这条边能连上"，实际到阶段 5 却查不到。这时框架必须炸给自己看，而不是把 nil 塞进字段、让下游插件在第一次查询时收到一个语焉不详的空指针 panic——那时调用栈早就离真正出错的地方十万八千里了。

- [ ] **Step 1: 写失败测试**

创建 `stage_init_test.go`：

```go
package xbc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/inject"
)

// fakeConn and fakeWidget stand in for what a real plugin (gorm, redis, ...)
// would provide: a pointer type whose zero value is nil, so the harvest-time
// zero check below has something meaningful to catch.
type fakeConn struct{ id string }
type fakeWidget struct{ n int }

func newTestApp(t *testing.T) *App {
	t.Helper()
	return &App{
		cfg:      &Config{Server: ServerConfig{ShutdownTimeout: 2 * time.Second}},
		registry: newRegistry(),
	}
}

func mustInstance(t *testing.T, p Plugin, name, instName string) *instance {
	t.Helper()
	fs, err := inject.Scan(p)
	require.NoError(t, err, "扫描插件 %s 的 tag 失败", name)
	return &instance{plugin: p, name: name, instance: instName, fields: fs}
}

// ---- plugin fixtures ----

type providerPlugin struct {
	Base
	Conn    *fakeConn `xbc:"provide"`
	forget  bool // Init "forgets" to set Conn -- harvest must catch this
	failErr error
}

func (p *providerPlugin) Name() string { return "gorm" }
func (p *providerPlugin) Init(ctx *Context) error {
	if p.failErr != nil {
		return p.failErr
	}
	if p.forget {
		return nil
	}
	p.Conn = &fakeConn{id: ctx.Instance()}
	return nil
}

type consumerPlugin struct {
	Base
	Conn *fakeConn `xbc:"inject"`
}

func (p *consumerPlugin) Name() string            { return "user" }
func (p *consumerPlugin) Init(ctx *Context) error { return nil }

type optionalConsumerPlugin struct {
	Base
	Conn *fakeConn `xbc:"inject,optional"`
}

func (p *optionalConsumerPlugin) Name() string { return "report" }

type namedConsumerPlugin struct {
	Base
	RO *fakeConn `xbc:"inject,name=readonly"`
}

func (p *namedConsumerPlugin) Name() string { return "audit" }

type manualProviderPlugin struct {
	Base
	registerOnInit bool
}

func (p *manualProviderPlugin) Name() string    { return "cache" }
func (p *manualProviderPlugin) Provides() []Dep { return []Dep{Offer[*fakeWidget]()} }
func (p *manualProviderPlugin) Init(ctx *Context) error {
	if p.registerOnInit {
		Provide(ctx, &fakeWidget{n: 1})
	}
	return nil
}

// provideOnlyPlugin deliberately does not implement Initializer -- its
// provide field is set by the constructor instead, the way a single-instance
// plugin registered via app.Register is allowed to under ruling R7.
type provideOnlyPlugin struct {
	Base
	Widget *fakeWidget `xbc:"provide"`
}

func (p *provideOnlyPlugin) Name() string { return "widget" }

type stoppablePlugin struct {
	Base
	name      string
	stopped   *[]string
	stopErr   error
	stopPanic bool
}

func (p *stoppablePlugin) Name() string        { return p.name }
func (p *stoppablePlugin) Init(*Context) error { return nil }
func (p *stoppablePlugin) Stop(context.Context) error {
	*p.stopped = append(*p.stopped, p.name)
	if p.stopPanic {
		panic("模拟 Stop panic：" + p.name)
	}
	return p.stopErr
}

// ---- tests ----

// Covers both "injecting the default instance" and "a provide tag being
// harvested with the downstream able to inject it": gorm[default] produces a
// connection, user depends on it, and both steps are verified chained
// together in one initAll call.
func TestProvideHarvestedAndInjectedToDownstreamDefaultInstance(t *testing.T) {
	a := newTestApp(t)
	prov := &providerPlugin{}
	provInst := mustInstance(t, prov, "gorm", "default")
	cons := &consumerPlugin{}
	consInst := mustInstance(t, cons, "user", "default")

	require.NoError(t, a.initAll([]*instance{provInst, consInst}))
	require.NotNil(t, cons.Conn, "user 应该拿到 gorm 产出的连接")
	assert.Equal(t, "default", cons.Conn.id)
}

func TestInjectNamedInstance(t *testing.T) {
	a := newTestApp(t)
	prov := &providerPlugin{}
	provInst := mustInstance(t, prov, "gorm", "readonly")
	cons := &namedConsumerPlugin{}
	consInst := mustInstance(t, cons, "audit", "default")

	require.NoError(t, a.initAll([]*instance{provInst, consInst}))
	require.NotNil(t, cons.RO)
	assert.Equal(t, "readonly", cons.RO.id)
}

func TestInjectOptionalMissingLeavesZeroValue(t *testing.T) {
	a := newTestApp(t)
	cons := &optionalConsumerPlugin{}
	consInst := mustInstance(t, cons, "report", "default")

	require.NoError(t, a.initAll([]*instance{consInst}))
	assert.Nil(t, cons.Conn, "可选依赖缺失时留零值，不报错")
}

// This is the fallback described in the task rationale: stage 4 should have
// already caught this, so seeing it here means the two stages disagree --
// that must fail loudly, not silently inject a nil.
func TestInjectRequiredMissingIsTreatedAsInternalError(t *testing.T) {
	a := newTestApp(t)
	cons := &consumerPlugin{}
	consInst := mustInstance(t, cons, "user", "default")

	err := a.initAll([]*instance{consInst})
	require.Error(t, err, "阶段 4 本该拦住这种缺失，阶段 5 兜底同样要报错，不能让 nil 溜过去")
	assert.Contains(t, err.Error(), "内部错误")
}

// The fake types in this test verify the error message's template and
// layout; the placeholders are filled with this test's own plugin labels
// and type names -- kernel tests are not allowed to import real gorm, so the
// literal string "*gorm.DB" can never appear.
func TestHarvestZeroValueErrorMessageVerbatim(t *testing.T) {
	a := newTestApp(t)
	prov := &providerPlugin{forget: true}
	provInst := mustInstance(t, prov, "gorm", "readonly")

	err := a.initAll([]*instance{provInst})
	require.Error(t, err)
	want := "xbc: 插件 gorm[readonly] 声明产出 *xbc.fakeConn，但 Init 后该字段仍为 nil\n" +
		"  → 检查 Init 中是否忘记给 Conn 字段赋值"
	assert.Equal(t, want, err.Error())
}

func TestProvidesDeclaredButNotManuallyRegisteredFails(t *testing.T) {
	a := newTestApp(t)
	p := &manualProviderPlugin{registerOnInit: false}
	inst := mustInstance(t, p, "cache", "default")

	err := a.initAll([]*instance{inst})
	require.Error(t, err, "Provides() 声明了产出，但 Init 里没有调用 xbc.Provide，必须报错")
	assert.Contains(t, err.Error(), "cache")
	assert.Contains(t, err.Error(), "xbc.Provide")
}

func TestProvidesWithManualProvideSucceeds(t *testing.T) {
	a := newTestApp(t)
	p := &manualProviderPlugin{registerOnInit: true}
	inst := mustInstance(t, p, "cache", "default")

	require.NoError(t, a.initAll([]*instance{inst}))
	got, err := a.registry.lookup(typeOf[*fakeWidget](), "default")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 1}, got)
}

func TestMultiInstanceProductKeysDoNotCollide(t *testing.T) {
	a := newTestApp(t)
	def := &providerPlugin{}
	ro := &providerPlugin{}
	defInst := mustInstance(t, def, "gorm", "default")
	roInst := mustInstance(t, ro, "gorm", "readonly")

	require.NoError(t, a.initAll([]*instance{defInst, roInst}))

	gotDef, err := a.registry.lookup(typeOf[*fakeConn](), "default")
	require.NoError(t, err)
	assert.Equal(t, "default", gotDef.(*fakeConn).id, "default 实例的产物必须能按 default 键取到")

	gotRO, err := a.registry.lookup(typeOf[*fakeConn](), "readonly")
	require.NoError(t, err)
	assert.Equal(t, "readonly", gotRO.(*fakeConn).id, "readonly 实例的产物必须能按 readonly 键取到，不能串成 default 的")
}

func TestRollbackStopsInReverseOrderOnInitFailure(t *testing.T) {
	a := newTestApp(t)
	var stopped []string

	aPlugin := &stoppablePlugin{name: "a", stopped: &stopped}
	bPlugin := &stoppablePlugin{name: "b", stopped: &stopped}
	cPlugin := &providerPlugin{failErr: errors.New("模拟 c 初始化失败")}

	aInst := mustInstance(t, aPlugin, "a", "default")
	bInst := mustInstance(t, bPlugin, "b", "default")
	cInst := mustInstance(t, cPlugin, "gorm", "default")

	err := a.initAll([]*instance{aInst, bInst, cInst})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "模拟 c 初始化失败")
	assert.Equal(t, []string{"b", "a"}, stopped, "已成功 Init 的插件必须按拓扑序的逆序 Stop")
	assert.False(t, cInst.inited, "c 自己 Init 失败，不能标记为已初始化")
}

func TestRollbackStopErrorAndPanicDoNotMaskOriginalError(t *testing.T) {
	a := newTestApp(t)
	var stopped []string

	aPlugin := &stoppablePlugin{name: "a", stopped: &stopped, stopErr: errors.New("a 的 Stop 自己也炸了")}
	bPlugin := &stoppablePlugin{name: "b", stopped: &stopped, stopPanic: true}
	cPlugin := &providerPlugin{failErr: errors.New("原始错误：模拟初始化失败")}

	aInst := mustInstance(t, aPlugin, "a", "default")
	bInst := mustInstance(t, bPlugin, "b", "default")
	cInst := mustInstance(t, cPlugin, "gorm", "default")

	var err error
	assert.NotPanics(t, func() {
		err = a.initAll([]*instance{aInst, bInst, cInst})
	}, "回滚中一个插件的 Stop panic 不能让整个回滚流程崩掉")
	require.Error(t, err)
	assert.Equal(t, "xbc: 插件 gorm 初始化失败: 原始错误：模拟初始化失败", err.Error(),
		"回滚阶段任何 Stop 错误或 panic 都不能覆盖最初触发回滚的错误")
	assert.Equal(t, []string{"b", "a"}, stopped, "b 的 panic 不能挡住 a 也被 Stop")
}

func TestSkipsInitWhenNotImplementedButStillHarvests(t *testing.T) {
	a := newTestApp(t)
	p := &provideOnlyPlugin{Widget: &fakeWidget{n: 7}}
	inst := mustInstance(t, p, "widget", "default")

	require.NoError(t, a.initAll([]*instance{inst}))
	got, err := a.registry.lookup(typeOf[*fakeWidget](), "default")
	require.NoError(t, err)
	assert.Equal(t, &fakeWidget{n: 7}, got)
	assert.True(t, inst.inited, "没有 Init 方法不代表初始化失败，仍应视为已完成，允许后续被 Stop")
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test . -run 'TestProvideHarvested|TestInject|TestHarvest|TestProvides|TestMultiInstanceProduct|TestRollback|TestSkipsInit' -v
```

Expected: 编译失败，`undefined: newTestApp`（`newTestApp` 引用了尚不存在的 `a.initAll`，编译器会先在 `a.initAll([]*instance{...})` 处报 `(*App).initAll undefined`）。

- [ ] **Step 3: 写实现**

创建 `stage_init.go`：

```go
package xbc

import (
	"context"
	"fmt"

	"github.com/xbcio/xbc/internal/inject"
	"github.com/xbcio/xbc/log"
)

// newContext mints the *Context handed to a plugin instance at stage 5.
// This is the first point in the pipeline where Context becomes real --
// Base.Ctx() is documented as nil before stage 5 for exactly this reason,
// so building it here (rather than in context.go) keeps that guarantee
// anchored to the one place that actually flips it.
func newContext(a *App, inst *instance) *Context {
	logger := log.L().With("plugin", inst.name)
	if mi, ok := inst.plugin.(MultiInstancer); ok && mi.MultiInstance() {
		logger = logger.With("instance", inst.instance)
	}
	return &Context{app: a, name: inst.name, instance: normInstance(inst.instance), logger: logger}
}

// initAll drives stage 5. For every instance, in topological order: inject
// its dependencies, call Init if implemented, harvest its declared
// products, then verify every product it claimed via Provides() actually
// landed in the registry. Any failure rolls back every instance that has
// already completed Init successfully, in reverse order, and returns the
// original error untouched -- rollback must never replace the error that
// caused it.
func (a *App) initAll(insts []*instance) error {
	for _, inst := range insts {
		ctx := newContext(a, inst)
		inst.ctx = ctx
		bindBase(inst.plugin, ctx, inst.name)

		if err := injectInstance(a, inst); err != nil {
			a.rollback(insts)
			return err
		}

		if initer, ok := inst.plugin.(Initializer); ok {
			if err := initer.Init(ctx); err != nil {
				a.rollback(insts)
				return fmt.Errorf("xbc: 插件 %s 初始化失败: %w", inst.label(), err)
			}
		}
		// Reaching this line means either Init succeeded, or the plugin
		// never implemented Initializer in the first place -- in both
		// cases there is nothing pending that would make a later Stop
		// call unsafe, so the instance is eligible for rollback/shutdown.
		inst.inited = true

		if err := harvestInstance(a, inst); err != nil {
			a.rollback(insts)
			return err
		}

		if err := validateManualProvides(a, inst); err != nil {
			a.rollback(insts)
			return err
		}
	}
	return nil
}

// injectInstance fills every xbc:"inject" field on inst from the registry.
//
// A non-optional miss here is a framework bug, not a user error: stage 4
// (resolve) is supposed to have already turned every hard dependency into a
// graph edge and aborted the boot if it couldn't be satisfied. If stage 5
// still can't find it, resolve's static analysis and the registry's runtime
// state have drifted apart -- crashing loudly beats letting a nil slip
// through to whichever plugin queries it first, far from where the real
// mistake was made.
func injectInstance(a *App, inst *instance) error {
	for _, spec := range inst.fields {
		if spec.Kind != inject.KindInject {
			continue
		}
		val, err := a.registry.lookup(spec.Type, normInstance(spec.Instance))
		if err != nil {
			if spec.Optional {
				continue
			}
			dep := Dep{Type: spec.Type, Instance: spec.Instance}
			return fmt.Errorf("xbc: 内部错误——插件 %s 注入 %s 失败，阶段 4 本应拦住这个缺失: %w",
				inst.label(), dep.String(), err)
		}
		if err := inject.Set(inst.plugin, spec, val); err != nil {
			return fmt.Errorf("xbc: 插件 %s 字段 %s 注入失败: %w", inst.label(), spec.Name, err)
		}
	}
	return nil
}

// harvestInstance reads every xbc:"provide" field back out of inst after
// Init has returned, and registers it under (field type, plugin's own
// instance name). A field that is still zero means Init forgot to set it --
// catching that here, instead of leaving it to surface as a nil-pointer
// panic in some unrelated downstream plugin's first query, is the entire
// point of this step.
func harvestInstance(a *App, inst *instance) error {
	for _, spec := range inst.fields {
		if spec.Kind != inject.KindProvide {
			continue
		}
		zero, err := inject.IsZero(inst.plugin, spec)
		if err != nil {
			return fmt.Errorf("xbc: 插件 %s 收割字段 %s 失败: %w", inst.label(), spec.Name, err)
		}
		if zero {
			return fmt.Errorf("xbc: 插件 %s 声明产出 %s，但 Init 后该字段仍为 nil\n  → 检查 Init 中是否忘记给 %s 字段赋值",
				inst.label(), spec.Type.String(), spec.Name)
		}
		val, err := inject.Value(inst.plugin, spec)
		if err != nil {
			return fmt.Errorf("xbc: 插件 %s 读取字段 %s 失败: %w", inst.label(), spec.Name, err)
		}
		a.registry.put(spec.Type, normInstance(inst.instance), val)
	}
	return nil
}

// validateManualProvides checks the other half of Provider: types declared
// via Provides() (as opposed to a "provide" tag) that the plugin was
// supposed to register itself with a manual xbc.Provide call inside Init.
// A declaration with nothing behind it is exactly as dangerous as a zero
// "provide" field, so it gets the same fail-fast treatment.
func validateManualProvides(a *App, inst *instance) error {
	provider, ok := inst.plugin.(Provider)
	if !ok {
		return nil
	}
	for _, dep := range provider.Provides() {
		if _, err := a.registry.lookup(dep.Type, normInstance(inst.instance)); err != nil {
			return fmt.Errorf("xbc: 插件 %s 的 Provides() 声明产出 %s，但 Init 中没有调用 xbc.Provide 登记该类型",
				inst.label(), dep.Type.String())
		}
	}
	return nil
}

// rollback stops every instance whose Init has already completed
// successfully, in reverse topological order -- dependents before their
// dependencies, mirroring shutdown's ordering (spec §4.1 constraint a). A Stop
// error or panic is logged and swallowed: the original failure that
// triggered the rollback is the one the user needs to see, and letting a
// second failure stomp on it would hide the real cause.
func (a *App) rollback(insts []*instance) {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
	defer cancel()

	for i := len(insts) - 1; i >= 0; i-- {
		inst := insts[i]
		if !inst.inited {
			continue
		}
		stopInstanceSafely(ctx, inst)
	}
}

// stopInstanceSafely calls Stop if the plugin implements Closer, recovering
// from a panic and logging any error instead of propagating it -- rollback
// must run to completion for every remaining instance no matter what one
// misbehaving Stop does.
func stopInstanceSafely(ctx context.Context, inst *instance) {
	closer, ok := inst.plugin.(Closer)
	if !ok {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			inst.ctx.Log().Error("回滚时 Stop panic，已忽略并继续", "plugin", inst.label(), "panic", r)
		}
	}()
	if err := closer.Stop(ctx); err != nil {
		inst.ctx.Log().Error("回滚时 Stop 失败，已忽略并继续", "plugin", inst.label(), "error", err)
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test . -run 'TestProvideHarvested|TestInject|TestHarvest|TestProvides|TestMultiInstanceProduct|TestRollback|TestSkipsInit' -v
go test ./... -count=1
```

Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add stage_init.go stage_init_test.go
git commit -m "feat(xbc): 阶段 5 Init——注入/Init/收割/校验与失败回滚"
```

---

### Task 14: 阶段 6~10 —— Migrate / AssembleHTTP / Start / Serve / Shutdown

**Files:**
- Create: `stage_run.go`
- Modify: `xbc.go`（`App` 追加 `softMisses []graph.Miss` 与 `middlewareChain []mwEntry` 两个字段——裁决 R12，见 Step 3 开头）
- Modify: `router.go`（Task 1 只留了类型定义与方法签名，本 task 补冻结判断与路由记录两部分方法体）
- Test: `stage_run_test.go`

**Interfaces:**
- Consumes:
  - `stage_init.go` 的 `newContext`、`(*App).initAll`、`(*App).rollback`（Task 13，直接复用，不重复实现）
  - `stage_init_test.go` 的测试夹具 `newTestApp`、`mustInstance`（Task 13，本 task 的测试文件直接复用，不重复定义）
  - `App` 的未导出字段：`order []*instance`（Task 10 的 `resolve` 写入，`kernel-api.md` §12 明确约定这个字段名）、`migrate bool`（Task 15 的 `cli.go` 根据 `--migrate` / `migrate` 子命令 / `server.auto_migrate` 算出，本 task 只读）、`router *Router`、`httpServer *http.Server`、`listener net.Listener`、`ready chan struct{}`、`routeCounts map[string]int`、`criticalCh chan struct{}`、`runCtx context.Context`、`cancel context.CancelFunc`、`wg *sync.WaitGroup`、`exitCode int`（**这十二个是 Task 1 按裁决 R12 一次性声明好的，直接用，不要重复声明**；`runCtx`/`cancel`/`wg`/`criticalCh` 由 Task 12 的 `goroutine.go` 负责写入与消费；`router`/`httpServer`/`listener`/`ready`/`routeCounts` 本 task 负责写入，Task 15 的启动日志只读，不重复计算）
  - `App` 的另两个未导出字段 `softMisses []graph.Miss` 与 `middlewareChain []mwEntry` **由本 task 用 `Edit` 追加进 `type App struct`**——它们的类型分别来自 `internal/graph`（Task 3）与 `mwchain.go`（Task 11），在 Task 1 时还不存在，所以按裁决 R12 延后到这里（见 Step 3 的具体位置）
  - `type Middleware struct{...}`、`type mwEntry struct{...}`、`func qualify(plugin, name string) string`、`func orderMiddlewares(entries []mwEntry) (ordered []mwEntry, misses []graph.Miss, err error)`（Task 11，`mwchain.go`）
  - `type Migrator interface{ Migrate(ctx *Context) error }`、`MiddlewareProvider`、`RouteProvider`、`PostRouter`、`Runner`、`Closer`（Task 1，`plugin.go`）
  - `log.L() log.Logger`、`log.Level`、`log.DebugLevel`、`log.ParseLevel`（`log` 包已完成）
- Produces:
  - `func (a *App) migrateAll(insts []*instance) error` —— 阶段 6
  - `func (a *App) assembleHTTP(insts []*instance) error` —— 阶段 7
  - `func (a *App) startRunners(insts []*instance) error` —— 阶段 8
  - `func (a *App) serve() error` —— 阶段 9
  - `func (a *App) shutdown(reason string) error` —— 阶段 10
  - `type ginLogWriter struct{ logger log.Logger; level log.Level }` 及其 `Write`
  - `func setGinMode(level string)`
  - `router.go`：`func newRouter(engine *gin.Engine, basePath string) *Router`、`func (r *Router) freeze()`、补全的 `Group` / `Handle` / `GET` / `POST` / `PUT` / `DELETE` / `PATCH`
  - 副作用：`assembleHTTP` 把排好序的中间件链写进 `a.middlewareChain`，把每个插件注册的路由数写进 `a.routeCounts[inst.id()]` —— 这两份数据 Task 15 的启动日志原样读取，不重新计算一遍

**阶段 6 为什么独立而不是并进 Init。** 迁移是有副作用的写操作——建表、加索引、改列类型。把它绑在启动路径上，意味着每一次滚动重启、每一次金丝雀发布都在改表结构：多副本并发跑 DDL，回滚镜像时旧版本代码又把表结构改回去。独立成阶段之后，迁移要么是 CI/CD 里一次性的 `migrate` 子命令，要么是显式的 `--migrate`，危险的事情从「顺手做了」变成「说了才做」。

**阶段 7 的时序约束（spec §4.1 约束 g）。** gin 要求 `engine.Use()` 必须先于路由注册，否则中间件不生效；而路由元数据是 `RegisterRoutes` 执行时才产生的。两条约束叠在一起，结论是**中间件装载的那一刻，路由表必然是空的**——所以中间件不能在装载期读路由元数据，只能在请求时通过 `ctx.Route(gc)` 查当前请求命中的那一条。需要全量路由表的插件（swagger、casbin 权限点同步）没法在装载期拿到，只能挪到路由表冻结之后，这正是 `PostRouter` 存在的理由。

**Plan 2/Plan 3 的接缝画在这里。** `router.go` 里的 `Router` 现在只有 `Method` + `Path` 两个字段的最小路由表；`.Public()`、`.Name()`、`.Doc()` 这些链式元数据是 Plan 3 的事——`RegisterRoutes(r *xbc.Router)` 的签名已经稳定，Plan 3 要做的只是让 `r.GET(...)` 返回一个带得住链式调用的类型，不改这个签名。Plan 3 的实现者读到这里应该知道：往 `RouteInfo` 加字段、往 `Handle` 的返回值加类型，都不碰这段代码已经锁死的方法签名。

- [ ] **Step 1: 写失败测试**

创建 `stage_run_test.go`：

```go
package xbc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { gin.SetMode(gin.TestMode) }

// ---- plugin fixtures ----

type migratorPlugin struct {
	Base
	name     string
	migrated *bool
}

func (p *migratorPlugin) Name() string { return p.name }
func (p *migratorPlugin) Migrate(ctx *Context) error {
	*p.migrated = true
	return nil
}

type mwPlugin struct {
	Base
	name  string
	order *[]string
	phase Phase
	after []string
}

func (p *mwPlugin) Name() string { return p.name }
func (p *mwPlugin) Middlewares() []Middleware {
	return []Middleware{{
		Name:  p.name,
		Phase: p.phase,
		After: p.after,
		Handler: func(c *gin.Context) {
			*p.order = append(*p.order, p.name)
			c.Next()
		},
	}}
}

type routePlugin struct {
	Base
	handler gin.HandlerFunc
}

func (p *routePlugin) Name() string { return "demo" }
func (p *routePlugin) RegisterRoutes(r *Router) {
	r.GET("/ping", p.handler)
}

type postRouterPlugin struct {
	Base
	seen *[]RouteInfo
}

func (p *postRouterPlugin) Name() string { return "swagger" }
func (p *postRouterPlugin) PostRoutes(ctx *Context) error {
	*p.seen = append(*p.seen, ctx.Routes()...)
	return nil
}

type runnerPlugin struct {
	Base
	name     string
	startErr error
	stopped  *[]string
}

func (p *runnerPlugin) Name() string             { return p.name }
func (p *runnerPlugin) Init(*Context) error      { return nil }
func (p *runnerPlugin) Start(ctx *Context) error { return p.startErr }
func (p *runnerPlugin) Stop(context.Context) error {
	*p.stopped = append(*p.stopped, p.name)
	return nil
}

type closerPlugin struct {
	Base
	name    string
	stopped *[]string
	delay   time.Duration
}

func (p *closerPlugin) Name() string        { return p.name }
func (p *closerPlugin) Init(*Context) error { return nil }
func (p *closerPlugin) Stop(ctx context.Context) error {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	*p.stopped = append(*p.stopped, p.name)
	return nil
}

// ---- assembly helpers ----

func newAssembleTestApp(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	a.cfg.Server.BasePath = "/"
	return a
}

// newServeTestApp wires initAll + assembleHTTP and a real :0 listener, ready
// for serve()/shutdown() -- everything a test needs to drive stage 9/10
// without touching time.Sleep for synchronization.
func newServeTestApp(t *testing.T, insts []*instance) *App {
	t.Helper()
	a := newAssembleTestApp(t)
	a.cfg.Server.ShutdownTimeout = 2 * time.Second
	a.cancel = func() {}
	a.wg = &sync.WaitGroup{}
	a.criticalCh = make(chan struct{})
	a.ready = make(chan struct{})
	a.order = insts

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a.listener = ln

	require.NoError(t, a.initAll(insts))
	require.NoError(t, a.assembleHTTP(insts))
	return a
}

// ---- stage 6: Migrate ----

func TestMigrateAllDefaultDoesNotRun(t *testing.T) {
	a := newTestApp(t)
	var ran bool
	inst := mustInstance(t, &migratorPlugin{name: "user", migrated: &ran}, "user", "default")
	require.NoError(t, a.migrateAll([]*instance{inst}))
	assert.False(t, ran, "默认不迁移")
}

func TestMigrateAllRunsWhenRequested(t *testing.T) {
	a := newTestApp(t)
	a.migrate = true
	var ran bool
	inst := mustInstance(t, &migratorPlugin{name: "user", migrated: &ran}, "user", "default")
	require.NoError(t, a.migrateAll([]*instance{inst}))
	assert.True(t, ran, "--migrate / migrate 子命令 / server.auto_migrate 命中任一个都要跑迁移")
}

// The fact that "the migrate subcommand exits as soon as it finishes
// (stages 1-6, then exit, no HTTP)" is cli.go's subcommand dispatch logic,
// verified end-to-end in Task 15's cli_test.go; this test only covers
// migrateAll's own on/off behavior.

// ---- stage 7: AssembleHTTP ----

func TestAssembleHTTPMiddlewareRunsInPhaseOrderOnARealRequest(t *testing.T) {
	a := newAssembleTestApp(t)
	var order []string

	recoverMW := &mwPlugin{name: "recover", order: &order, phase: PhaseRecover}
	observeMW := &mwPlugin{name: "observe", order: &order, phase: PhaseObserve}
	businessMW := &mwPlugin{name: "audit", order: &order, phase: PhaseBusiness}
	route := &routePlugin{handler: func(c *gin.Context) { c.Status(http.StatusOK) }}

	// Registration order is deliberately shuffled; the assertion checks that sorting only looks at Phase, not registration order.
	insts := []*instance{
		mustInstance(t, businessMW, "audit", "default"),
		mustInstance(t, route, "demo", "default"),
		mustInstance(t, recoverMW, "recover", "default"),
		mustInstance(t, observeMW, "observe", "default"),
	}
	for _, inst := range insts {
		inst.ctx = newContext(a, inst)
	}

	require.NoError(t, a.assembleHTTP(insts))

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	a.router.engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"recover", "observe", "audit"}, order,
		"中间件必须按 Phase 顺序套上，与插件注册顺序无关")
}

func TestRouterHandleAfterFreezePanics(t *testing.T) {
	a := newAssembleTestApp(t)
	require.NoError(t, a.assembleHTTP(nil))

	assert.PanicsWithValue(t, "xbc: 路由表已在阶段 7 冻结，PostRoutes 里不能再加路由", func() {
		a.router.GET("/late", func(c *gin.Context) {})
	})
}

func TestPostRoutesSeesFullFrozenRouteTable(t *testing.T) {
	a := newAssembleTestApp(t)
	var seen []RouteInfo
	route := &routePlugin{handler: func(c *gin.Context) {}}
	post := &postRouterPlugin{seen: &seen}

	insts := []*instance{
		mustInstance(t, route, "demo", "default"),
		mustInstance(t, post, "swagger", "default"),
	}
	for _, inst := range insts {
		inst.ctx = newContext(a, inst)
	}

	require.NoError(t, a.assembleHTTP(insts))
	require.Len(t, seen, 1, "PostRoutes 执行时路由表必须已经装满")
	assert.Equal(t, "/ping", seen[0].Path)
}

// ---- stage 8: Start ----

func TestStartRunnersFailureTriggersRollbackInReverseOrder(t *testing.T) {
	a := newTestApp(t)
	a.cancel = func() {}
	a.wg = &sync.WaitGroup{}
	var stopped []string

	good := &runnerPlugin{name: "cron", stopped: &stopped}
	bad := &runnerPlugin{name: "consumer", startErr: errors.New("模拟启动失败"), stopped: &stopped}

	goodInst := mustInstance(t, good, "cron", "default")
	badInst := mustInstance(t, bad, "consumer", "default")
	insts := []*instance{goodInst, badInst}
	require.NoError(t, a.initAll(insts))

	err := a.startRunners(insts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "模拟启动失败")
	assert.Equal(t, []string{"consumer", "cron"}, stopped,
		"启动失败要回滚全部已初始化的插件，按 insts 的逆序：consumer 排在后面先被 Stop")
}

// ---- stage 9/10: Serve / Shutdown ----

func TestShutdownStopsInReverseTopologicalOrder(t *testing.T) {
	a := newTestApp(t)
	a.cancel = func() {}
	a.wg = &sync.WaitGroup{}

	var stopped []string
	gormPlugin := &closerPlugin{name: "gorm", stopped: &stopped}
	userPlugin := &closerPlugin{name: "user", stopped: &stopped} // user depends on gorm -> sorts after it
	insts := []*instance{
		mustInstance(t, gormPlugin, "gorm", "default"),
		mustInstance(t, userPlugin, "user", "default"),
	}
	require.NoError(t, a.initAll(insts))
	a.order = insts

	require.NoError(t, a.shutdown("test"))
	assert.Equal(t, []string{"user", "gorm"}, stopped, "user 依赖 gorm，关闭必须先停 user 再停 gorm")
}

func TestInFlightRequestIsDrainedBeforeShutdownCompletes(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	route := &routePlugin{handler: func(c *gin.Context) {
		close(reached)
		<-release
		c.Status(http.StatusOK)
	}}
	insts := []*instance{mustInstance(t, route, "demo", "default")}
	a := newServeTestApp(t, insts)

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serve() }()
	<-a.ready

	addr := a.listener.Addr().String()
	reqDone := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/ping")
		require.NoError(t, err)
		reqDone <- resp.StatusCode
	}()
	<-reached // the request has already entered the handler and is stuck there

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- a.shutdown("test") }()

	// Instead of sleeping to wait for "shutdown should be waiting on the
	// in-flight request", release the handler directly and let channel ordering
	// converge: the request must finish first, and only then should shutdown
	// return.
	close(release)

	assert.Equal(t, http.StatusOK, <-reqDone, "in-flight 请求必须正常跑完，不能被 shutdown 打断")
	require.NoError(t, <-shutdownDone)
	require.NoError(t, <-serveDone)
}

func TestShutdownTimeoutForceKillsStuckConnection(t *testing.T) {
	block := make(chan struct{})
	reached := make(chan struct{})
	route := &routePlugin{handler: func(c *gin.Context) {
		close(reached)
		<-block // deliberately never released, simulating a connection that ignores the shutdown signal
	}}
	insts := []*instance{mustInstance(t, route, "demo", "default")}
	a := newServeTestApp(t, insts)
	a.cfg.Server.ShutdownTimeout = 200 * time.Millisecond

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serve() }()
	<-a.ready

	addr := a.listener.Addr().String()
	go func() { _, _ = http.Get("http://" + addr + "/ping") }()
	<-reached

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- a.shutdown("test") }()

	select {
	case err := <-shutdownErr:
		assert.NoError(t, err, "超时后应该强杀退出，不能无限期等一个不肯放手的连接")
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown 应该在 shutdown_timeout 后强杀，不应该卡住")
	}
	close(block)
	<-serveDone
}

func TestGoCriticalTriggersFullShutdownAndExitCodeOne(t *testing.T) {
	var stopped []string
	closer := &closerPlugin{name: "gorm", stopped: &stopped}
	insts := []*instance{mustInstance(t, closer, "gorm", "default")}
	a := newServeTestApp(t, insts)

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serve() }()
	<-a.ready

	close(a.criticalCh) // simulates a GoCritical trigger

	require.NoError(t, <-serveDone)
	assert.Equal(t, []string{"gorm"}, stopped, "GoCritical 触发的关闭仍要走完整阶段 10，其他插件照样被 Stop")
	assert.Equal(t, 1, a.exitCode, "GoCritical 触发的退出码必须是 1")
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test . -run 'TestMigrateAll|TestAssembleHTTP|TestRouterHandle|TestPostRoutes|TestStartRunners|TestShutdown|TestInFlight|TestGoCritical' -v
```

Expected: 编译失败，`undefined: a.migrateAll`（以及 `newRouter`、`a.assembleHTTP` 等一连串未定义符号，因为 `stage_run.go` 还不存在，`router.go` 里的方法体也还没补全）。

- [ ] **Step 3: 写实现**

先用 `Edit` 往 `xbc.go` 的 `type App struct { ... }` 里补两个字段（裁决 R12 里唯二挂在本 task 名下的延后字段），加在 `routeCounts` 那一行后面：

```go
	// softMisses collects every soft ordering constraint (After/Before)
	// that named something which does not exist -- from stage 4's plugin
	// sort and from this stage's middleware sort alike. cli.go's startup
	// log renders them as one warning block; they are never fatal, since a
	// preference that points at an absent plugin is a stale preference,
	// not a broken dependency.
	softMisses []graph.Miss

	// middlewareChain is the ordered chain assembled by stage 7. It is kept
	// on App so cli.go's startup log can print the final order without
	// re-running orderMiddlewares -- printing a second, independently
	// computed order would be a chance for the log to disagree with what
	// gin actually runs.
	middlewareChain []mwEntry
```

`xbc.go` 顶部相应补上 `"github.com/xbcio/xbc/internal/graph"` 这一行 import（`mwEntry` 是根包自己的类型，不需要 import）。这两个字段之所以现在才加而不是 Task 1 就位，是因为 `graph.Miss` 与 `mwEntry` 分别到 Task 3 和 Task 11 才存在——Task 1 写它们会 import 到不存在的包。

然后创建 `stage_run.go`：

```go
package xbc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/log"
)

// migrateAll drives stage 6. It runs only when a.migrate is true, set by
// cli.go from --migrate / the migrate subcommand / server.auto_migrate --
// migration is a side-effecting write operation, and binding it to every
// boot would mean every rolling restart silently touches the schema.
func (a *App) migrateAll(insts []*instance) error {
	if !a.migrate {
		return nil
	}
	for _, inst := range insts {
		migrator, ok := inst.plugin.(Migrator)
		if !ok {
			continue
		}
		if err := migrator.Migrate(inst.ctx); err != nil {
			return fmt.Errorf("xbc: 插件 %s 迁移失败: %w", inst.label(), err)
		}
	}
	return nil
}

// ginLogWriter adapts an xbc log.Logger to io.Writer so gin's own startup
// banner and internal warnings land in the same structured log stream as
// everything else, instead of a second, unstructured format fighting for
// stdout.
type ginLogWriter struct {
	logger log.Logger
	level  log.Level
}

func (w ginLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		if w.level == log.ErrorLevel {
			w.logger.Error(msg)
		} else {
			w.logger.Info(msg)
		}
	}
	return len(p), nil
}

// setGinMode derives gin's run mode from log.level. Debug logging implies
// gin's own debug mode (route dump, warnings); anything quieter gets gin's
// release mode -- gin's debug banner is noisy in production logs and
// duplicates what xbc's own startup log already prints.
func setGinMode(level string) {
	lvl, err := log.ParseLevel(level)
	if err == nil && lvl == log.DebugLevel {
		gin.SetMode(gin.DebugMode)
		return
	}
	gin.SetMode(gin.ReleaseMode)
}

// assembleHTTP drives stage 7: load middleware in Phase order, register
// routes, freeze the route table, then run PostRoutes. Per spec §4.1
// constraint g, middleware is loaded before any route exists (gin requires
// engine.Use before route registration), so middleware must never read
// route metadata at load time -- only at request time via ctx.Route.
func (a *App) assembleHTTP(insts []*instance) error {
	setGinMode(a.cfg.Log.Level)
	gin.DefaultWriter = ginLogWriter{logger: log.L(), level: log.InfoLevel}
	gin.DefaultErrorWriter = ginLogWriter{logger: log.L(), level: log.ErrorLevel}

	engine := gin.New()
	router := newRouter(engine, a.cfg.Server.BasePath)
	a.router = router

	var entries []mwEntry
	for _, inst := range insts {
		mp, ok := inst.plugin.(MiddlewareProvider)
		if !ok {
			continue
		}
		for _, mw := range mp.Middlewares() {
			entries = append(entries, mwEntry{Middleware: mw, qname: qualify(inst.name, mw.Name), plugin: inst.name})
		}
	}
	ordered, misses, err := orderMiddlewares(entries)
	if err != nil {
		return err
	}
	a.softMisses = append(a.softMisses, misses...)
	a.middlewareChain = ordered // Task 15's startup log renders this section directly
	for _, e := range ordered {
		engine.Use(e.Handler)
	}

	if a.routeCounts == nil {
		a.routeCounts = make(map[string]int)
	}
	for _, inst := range insts {
		rp, ok := inst.plugin.(RouteProvider)
		if !ok {
			continue
		}
		before := len(*router.routes)
		rp.RegisterRoutes(router)
		// RouteInfo carries no owner field (Plan 2's minimal form, §11), so a
		// per-plugin route count can only be recovered by diffing the frozen
		// table's length around the one call that plugin makes -- Task 15's
		// startup log needs this count for the "routes(N)" capability tag.
		a.routeCounts[inst.id()] = len(*router.routes) - before
	}

	router.freeze()

	for _, inst := range insts {
		pr, ok := inst.plugin.(PostRouter)
		if !ok {
			continue
		}
		if err := pr.PostRoutes(inst.ctx); err != nil {
			return fmt.Errorf("xbc: 插件 %s 的 PostRoutes 失败: %w", inst.label(), err)
		}
	}
	return nil
}

// startRunners drives stage 8: concurrently start every Runner. Start must
// return quickly -- long-running loops go through ctx.Go / ctx.GoCritical.
// errgroup would pull in a dependency for something a WaitGroup + a Mutex
// already do; any Start failure rolls back everything already initialized.
func (a *App) startRunners(insts []*instance) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, inst := range insts {
		runner, ok := inst.plugin.(Runner)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(inst *instance, runner Runner) {
			defer wg.Done()
			if err := runner.Start(inst.ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("xbc: 插件 %s 启动失败: %w", inst.label(), err))
				mu.Unlock()
			}
		}(inst, runner)
	}
	wg.Wait()

	if len(errs) == 0 {
		return nil
	}
	if a.cancel != nil {
		a.cancel()
	}
	if a.wg != nil {
		a.wg.Wait()
	}
	a.rollback(insts)
	return errors.Join(errs...)
}

// serve drives stage 9: block running the HTTP server until one of three
// signals arrives -- an OS interrupt, a GoCritical failure, or Serve's own
// error -- then hands off to shutdown. a.listener lets tests pass a :0
// listener and pick their own port; a.ready is closed once Serve has
// actually started accepting, so a test never has to guess with a sleep.
func (a *App) serve() error {
	if a.listener == nil {
		ln, err := net.Listen("tcp", a.cfg.Server.Addr)
		if err != nil {
			return fmt.Errorf("xbc: 监听 %s 失败: %w", a.cfg.Server.Addr, err)
		}
		a.listener = ln
	}
	a.httpServer = &http.Server{
		Handler:      a.router.engine,
		ReadTimeout:  a.cfg.Server.ReadTimeout,
		WriteTimeout: a.cfg.Server.WriteTimeout,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.httpServer.Serve(a.listener) }()

	if a.ready != nil {
		close(a.ready)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		return a.shutdown("signal")
	case <-a.criticalCh:
		return a.shutdown("critical")
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

// shutdown drives stage 10: stop accepting new requests, drain in-flight
// ones, cancel every managed goroutine and wait for them, stop every
// initialized plugin in reverse topological order (spec §4.1 constraint a:
// dependents before their dependencies), then force-kill anything still
// stuck past shutdown_timeout. reason distinguishes a clean signal-triggered
// shutdown from a GoCritical-triggered one, which additionally sets the
// process exit code to 1.
func (a *App) shutdown(reason string) error {
	log.L().Info("xbc: 开始关闭", "reason", reason)

	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
	defer cancel()

	if a.httpServer != nil {
		if err := a.httpServer.Shutdown(ctx); err != nil {
			log.L().Error("xbc: 优雅关闭超时，强制关闭", "error", err)
			_ = a.httpServer.Close()
		}
	}

	if a.cancel != nil {
		a.cancel()
	}
	if a.wg != nil {
		a.wg.Wait()
	}

	a.rollback(a.order)

	if reason == "critical" {
		a.exitCode = 1
	}
	return nil
}
```

用 `Edit` 把 `router.go` 里 Task 1 留下的空方法体替换为完整实现（若 Task 1 已按下面的签名占位，直接替换方法体；文件最终内容如下）：

```go
package xbc

import (
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
)

// RouteInfo is one entry in the frozen route table. Plan 3 adds the
// metadata fields (Public, Name, Doc); Plan 2 freezes method and path only.
type RouteInfo struct {
	Method string
	Path   string
}

// Router wraps a *gin.RouterGroup with route-table recording and a freeze
// switch. routes and frozen are pointers so every Router returned by Group
// shares the same underlying slice/flag as the root -- freezing the root
// freezes every group derived from it too.
type Router struct {
	engine   *gin.Engine
	group    *gin.RouterGroup
	basePath string
	routes   *[]RouteInfo
	frozen   *bool
}

// newRouter is called exactly once, at stage 7, the first time AssembleHTTP
// needs somewhere to register routes.
func newRouter(engine *gin.Engine, basePath string) *Router {
	routes := make([]RouteInfo, 0, 16)
	frozen := false
	return &Router{
		engine:   engine,
		group:    engine.Group(basePath),
		basePath: basePath,
		routes:   &routes,
		frozen:   &frozen,
	}
}

// freeze locks the route table. Called once, right after every plugin's
// RegisterRoutes has run and before any PostRoutes runs.
func (r *Router) freeze() { *r.frozen = true }

// Group returns a sub-router rooted at relativePath, sharing this router's
// route table and freeze flag.
func (r *Router) Group(relativePath string) *Router {
	return &Router{
		engine:   r.engine,
		group:    r.group.Group(relativePath),
		basePath: r.basePath,
		routes:   r.routes,
		frozen:   r.frozen,
	}
}

// Handle registers a route and records it in the route table. Calling it
// after freeze panics -- PostRoutes runs after the route table is supposed
// to be complete, so a plugin adding a route there is a programming
// mistake, not a runtime condition worth recovering from.
func (r *Router) Handle(method, relativePath string, h ...gin.HandlerFunc) {
	if *r.frozen {
		panic("xbc: 路由表已在阶段 7 冻结，PostRoutes 里不能再加路由")
	}
	r.group.Handle(method, relativePath, h...)
	*r.routes = append(*r.routes, RouteInfo{
		Method: method,
		Path:   path.Join(r.group.BasePath(), relativePath),
	})
}

func (r *Router) GET(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodGet, relativePath, h...)
}

func (r *Router) POST(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodPost, relativePath, h...)
}

func (r *Router) PUT(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodPut, relativePath, h...)
}

func (r *Router) DELETE(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodDelete, relativePath, h...)
}

func (r *Router) PATCH(relativePath string, h ...gin.HandlerFunc) {
	r.Handle(http.MethodPatch, relativePath, h...)
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test . -run 'TestMigrateAll|TestAssembleHTTP|TestRouterHandle|TestPostRoutes|TestStartRunners|TestShutdown|TestInFlight|TestGoCritical' -v
go test ./... -count=1
go test ./... -race -count=1
```

Expected: 全部 PASS，`-race` 也不报数据竞争（`startRunners` 的 `errs`/`stopped` 全部经 mutex 或 channel 保护）。

- [ ] **Step 5: Commit**

```bash
git add stage_run.go router.go stage_run_test.go
git commit -m "feat(xbc): 阶段 6~10——Migrate/AssembleHTTP/Start/Serve/Shutdown"
```

---

### Task 15: `cli.go` —— flag 解析、子命令、启动日志、配置样例、`internal/` 洁癖守卫

**Files:**
- Create: `cli.go`、`startuplog.go`、`application.example.yml`、`arch_test.go`
- Modify: `xbc.go`（Task 1 只留了 `Run()` / `run()` 的签名和空实现，本 task 补方法体，把入口接上前 14 个 task 攒出来的全部阶段函数）
- Test: `cli_test.go`、`startuplog_test.go`

**Interfaces:**
- Consumes:
  - 阶段函数 `loadConfig`（Task 6，`config.go`）/ `expand` / `bindConfigs` / `resolve` / `initAll` / `migrateAll` / `assembleHTTP` / `startRunners` / `serve` / `rollback`（Task 8/9/10/13/14，签名以 `kernel-api.md` §12 为准，具体落在哪个文件不影响本 task 调用）
  - `App` 的未导出字段：`cfg *Config`（Task 6 追加，阶段 1 `loadConfig` 写入）、`order []*instance`、`migrate bool`（本 task 负责写入）、`softMisses []graph.Miss`、`middlewareChain []mwEntry`、`routeCounts map[string]int`（Task 14 写入，本 task 只读）、`router` / `httpServer` / `listener` / `ready` / `criticalCh` / `exitCode`（Task 12/14 写入，本 task 不碰，只是它们要在 `run()` 里被间接驱动到）。除 `cfg` / `softMisses` / `middlewareChain` 三个是后续 task 追加进结构体的之外，其余都是 Task 1 一次性声明好的（裁决 R12），本 task 一个都不新增
  - `internal/conf.Options{ File, Profile, EnvPrefix, Overrides }`（Task 5，`internal/conf/load.go`）
  - `internal/graph.Miss{ Node, Ref, Dir }`（Task 3，`internal/graph/graph.go`）
  - `type Middleware struct{...}`、`type mwEntry struct{ Middleware; qname, plugin string }`、`type Phase int` 及 `(Phase).String()`（Task 1 / 11，`middleware.go` / `mwchain.go`）
  - `Plugin` 与全部 12 个可选接口：`Configurable`、`MultiInstancer`、`Declarer`、`Provider`、`Initializer`、`Migrator`、`MiddlewareProvider`、`RouteProvider`、`PostRouter`、`Runner`、`Closer`、`HealthChecker`（Task 1，`plugin.go`）
  - `Dep`、`(Dep).String()`、`Deps{ Types, Plugins, After, Before }`、`Need[T any]() Dep`、`Offer[T any]() Dep`（Task 2，`deps.go`）
  - `(*instance).id()`、`(*instance).label()`、`type source int` 及 `sourceImport` / `sourceRegister`（Task 1，`xbc.go`）
  - `log.L() log.Logger`、`log.SetLogger`、`log.Nop()`、`log.Logger` 接口、`log.Level`（`log` 包已完成）
  - 测试夹具 `fakeConn`（Task 13，`stage_init_test.go`，`startuplog_test.go` 直接复用，不重复定义一个一样的假连接类型）
- Produces:
  - `type cliOptions struct{ subcommand, config, profile string; migrate bool }`
  - `func parseArgs(args []string) (cliOptions, error)`
  - `func (a *App) Run()` —— 方法体（`xbc.go`）
  - `func (a *App) run(args []string) (exitCode int, err error)` —— 方法体（`xbc.go`），可测试的核心，不碰 `os.Exit`
  - `func (a *App) printStartupLog(order []*instance)`
  - `func buildPluginTable(a *App, order []*instance) string`
  - `func capabilityTokens(a *App, inst *instance) []string`
  - `func depsAndProvidesSuffix(inst *instance) string`
  - `func renderMiddlewareChain(chain []mwEntry) string`
  - `func renderSoftMisses(misses []graph.Miss) string`
  - `func renderMigrationNotice(a *App, order []*instance) string`
  - `application.example.yml`（配置样例，`server` / `log` / `plugins` / `app` 四段）
  - `TestInternalPackagesDoNotImportRootOrEachOther`（`arch_test.go`，`internal/` 洁癖守卫的另外一半；`log/` 那一半已经在 `log/integration_test.go` 的 `TestLogPackageHasNoFrameworkDependency` 里守住了，本 task 不重复）

这是最后一个 task，把前 14 个 task 攒出来的全部阶段函数接成一个真正能跑的程序。三件事分开做：`cli.go` 管「进程从命令行到跑哪几个阶段」，`startuplog.go` 管「把装配结果讲清楚给人看」，`arch_test.go` 管「架构约束不会被下一个不知情的改动悄悄破坏」。

`cli.go` 只用标准库 `flag`——spec §4.2 明确不要 cobra，理由是 xbc 的子命令只有 `migrate` / `doctor` 两个，外加几个全局 flag，`flag.FlagSet` 完全够用，多引入一个命令行框架换来的只是学习成本。`flag` 包要求所有 flag 出现在位置参数之前才能正确解析，而 xbc 的子命令固定是第一个不以 `-` 开头的参数，所以 `parseArgs` 先把它摘出来，剩下的交给 `fs.Parse`。

子命令到阶段的映射表（spec §4.2，`run()` 直接照这张表写分支）：

| 命令 | 执行的阶段 |
|---|---|
| `./myapp` | 1~5, 7~9，不迁移 |
| `./myapp --config ./prod.yml --profile prod` | 同上，只是配置来源不同 |
| `./myapp --migrate` | 1~9，阶段 6 也跑 |
| `./myapp migrate` | 1~6，然后退出，不起 HTTP |
| `./myapp doctor` | 1~4，然后退出，不建立任何连接（不调 `initAll`） |

`--profile` 优先于 `XBC_PROFILE`：`parseArgs` 里先解析 flag，`opts.profile` 仍为空时才去查环境变量——flag 显式传的意图总是压过环境的隐式设置，这条和裁决 R8「显式传参的意图更强」是同一个道理。

`run()` 里 `migrate` 子命令跑完会调一次 `a.rollback(order)` 再退出：spec 的阶段表只写了「1~6 然后退出」，没写要不要 Stop 已经 Init 好的插件，但如果不 Stop，`migrate` 子命令退出时连接池、文件描述符全部原地泄漏，一次性进程也要讲究干净退出，这是本 task 在裁决之外做的一个不言自明的选择。

`doctor` 子命令在阶段 4 之后直接调 `printStartupLog` 再退出，全程不碰 `initAll`——`doctor` 存在的目的就是「看看装配结果对不对」而不建立任何真实连接，这也是它和 `migrate`/正常启动最本质的区别，`TestRunSubcommandsReachExpectedStages` 里专门断言它的调用轨迹里没有 `"init"`。

`startuplog.go` 实现 spec §4.4 的四段式启动日志：插件表（label + 能力列 + requires/provides）、中间件链、软约束未命中、迁移未执行提示。能力列的顺序按 §5.1 接口表的阶段顺序排：`config < init < migrate < middleware < routes(N) < runner < health < stop`。这里要指出 spec §4.4 示例里的一处内部矛盾：`user` 那一行写的是 `routes(12) migrate`，routes 排在 migrate 前面；但同一个例子里 `gorm`、`jwt`、`cron` 三行都严格按接口表的阶段顺序排列，`user` 是唯一的例外。本 task 统一按接口表顺序实现，把 `user` 那一行的顺序视为文档笔误，不当作规则——这类处理方式和计划里已经写下的裁决 R1~R11 是同一种态度：spec 是设计意图的记录，不是不可更改的圣经，出现自相矛盾时选内部一致的那一种。另外 spec §4.4 里 jwt 那一行带的「consumes 路由元数据」是对 jwt 语义层行为的旁注，不是某个可选接口能推出的通用能力，内核层面没有对应的接口断言可以驱动这一列，本 task 不实现它，只输出可以从接口断言与 `Deps`/`Provides` 推出的信息。

「迁移未执行」提示里的数字，spec 原文写的是「12 个模型待检查」——「模型」是 gorm 插件自己的领域概念（它知道自己注册了多少个 model），内核不认识 gorm，也不该认识。本 task 退一步，数「实现了 `Migrator` 接口的插件数」,语义对等（都是「有多少东西声明了要迁移但没有迁移」），但主体从「模型」换成了「插件」,这个换算写进 `renderMigrationNotice` 的文档注释里，免得以后有人对着 spec 原文找不到「12」这个数字从哪来。

插件表里每一行的能力列、requires/provides 列都要按**实际内容宽度**对齐，不能写死列宽——不同项目里插件名长度、能力组合千差万别，硬编码的宽度换个项目就错位。显式 `Register` 的实例（`inst.src == sourceRegister`）额外加一行 `↑ 显式 Register` 标记，对齐到 requires/provides 列的起始位置。

`arch_test.go` 只补「internal/ 不认识根包，也不互相认识」这一半的自动化守卫；「`log/` 不认识根包」那一半已经在 `log/integration_test.go` 的 `TestLogPackageHasNoFrameworkDependency` 里实现了（用 `go list -test -deps`，不是字符串 grep），global constraints 里那句「`log/nodeps_test.go` 已经守住这条线」实际指的就是这个测试，只是文件名和位置记错了——它其实活在 `log/integration_test.go` 里而不是单独的 `log/nodeps_test.go`。本 task 不重复实现，只是在这里把这件事讲清楚，避免以后有人真的去找一个不存在的文件。`internal/graph` 已经先天不 import 根包（它是最先写的三个包之一，当时根包还不存在),这条守卫的价值在于**未来**——防止某次改动图省事直接在 `internal/graph` 里引用了一个 `xbc.Dep`。用 `go list -test -deps` 而不是字符串 grep 的理由和 `log` 包那条测试的注释一样：grep 会被注释里提到的包名、字符串常量里恰好出现的路径污染，`go list` 走的是真实的编译期 import 图，不会有假阳性也不会漏掉只出现在 `_test.go` 里的 import。

- [ ] **Step 1: 为三份实现一次性写好失败测试**

`cli_test.go`：

```go
package xbc

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kyaml "github.com/knadh/koanf/parsers/yaml"
)

// ---- lifecycle tracker: records method call order, so assertions about which stage was reached need no real middleware ----

type lifecycleTracker struct {
	mu    sync.Mutex
	calls []string
}

func (t *lifecycleTracker) record(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, s)
}

func (t *lifecycleTracker) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.calls...)
}

type trackedPlugin struct {
	Base
	tracker *lifecycleTracker
}

func newTrackedPlugin(tr *lifecycleTracker) *trackedPlugin { return &trackedPlugin{tracker: tr} }

func (p *trackedPlugin) Name() string { return "tracked" }
func (p *trackedPlugin) Init(ctx *Context) error {
	p.tracker.record("init")
	return nil
}
func (p *trackedPlugin) Migrate(ctx *Context) error {
	p.tracker.record("migrate")
	return nil
}
func (p *trackedPlugin) Start(ctx *Context) error {
	p.tracker.record("start")
	return nil
}
func (p *trackedPlugin) RegisterRoutes(r *Router) {
	p.tracker.record("registerRoutes")
}
func (p *trackedPlugin) Stop(ctx context.Context) error {
	p.tracker.record("stop")
	return nil
}

// newRunnableTestApp prepares an App that can reach serve() and be stopped
// early from outside: a :0 listener plus ready/criticalCh channels are
// pre-wired, so once a test receives ready it can close criticalCh to make
// serve() return immediately, with no need to guess timing via time.Sleep.
func newRunnableTestApp(t *testing.T, p Plugin) *App {
	t.Helper()
	a := &App{}
	a.ready = make(chan struct{})
	a.criticalCh = make(chan struct{})
	a.cancel = func() {}
	a.wg = &sync.WaitGroup{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a.listener = ln
	a.Register(p)
	return a
}

func TestRunNormalBootReachesServeAndShutsDownOnGoCritical(t *testing.T) {
	track := &lifecycleTracker{}
	a := newRunnableTestApp(t, newTrackedPlugin(track))

	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := a.run(nil)
		done <- struct {
			code int
			err  error
		}{code, err}
	}()

	<-a.ready
	close(a.criticalCh)
	result := <-done

	require.NoError(t, result.err)
	assert.Equal(t, 1, result.code, "GoCritical 触发的关闭退出码固定为 1")
	assert.Equal(t, []string{"init", "start", "registerRoutes", "stop"}, track.snapshot(),
		"正常启动跑阶段 1~5,7~9，不该出现 migrate")
}

func TestRunWithMigrateFlagAlsoRunsStageSix(t *testing.T) {
	track := &lifecycleTracker{}
	a := newRunnableTestApp(t, newTrackedPlugin(track))

	done := make(chan int, 1)
	go func() {
		code, _ := a.run([]string{"--migrate"})
		done <- code
	}()
	<-a.ready
	close(a.criticalCh)
	<-done

	assert.Equal(t, []string{"init", "migrate", "start", "registerRoutes", "stop"}, track.snapshot(),
		"--migrate 补跑阶段 6")
}

func TestRunSubcommandsReachExpectedStagesAndExit(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantCalls []string
	}{
		{
			name:      "doctor 只到阶段 4，不调用任何 Init",
			args:      []string{"doctor"},
			wantCalls: nil,
		},
		{
			name:      "migrate 子命令跑到阶段 6 然后退出，不起 HTTP",
			args:      []string{"migrate"},
			wantCalls: []string{"init", "migrate", "stop"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			track := &lifecycleTracker{}
			a := &App{}
			a.Register(newTrackedPlugin(track))

			code, err := a.run(tc.args)
			require.NoError(t, err)
			assert.Equal(t, 0, code)
			assert.Equal(t, tc.wantCalls, track.snapshot(), tc.name)
		})
	}
}

func TestRunUnknownSubcommandReportsErrorAndDoesNotBoot(t *testing.T) {
	track := &lifecycleTracker{}
	a := &App{}
	a.Register(newTrackedPlugin(track))

	code, err := a.run([]string{"launch"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未知子命令")
	assert.Equal(t, 2, code, "命令行用法错误固定退出码 2")
	assert.Nil(t, track.snapshot(), "命令行都没解析成功，不该跑到任何阶段")
}

func TestRunConfigFileMissingIsFatalWhenExplicitlyNamed(t *testing.T) {
	a := &App{}
	_, err := a.run([]string{"--config", "/tmp/xbc-plan-does-not-exist-ever.yml"})
	require.Error(t, err, "裁决 R8：显式传了 --config 但文件不存在必须报错，不能静默按空配置跑")
}

func TestParseArgsProfileFlagTakesPriorityOverEnv(t *testing.T) {
	t.Setenv("XBC_PROFILE", "from-env")
	opts, err := parseArgs(nil)
	require.NoError(t, err)
	assert.Equal(t, "from-env", opts.profile, "没传 --profile 时落回 XBC_PROFILE")

	opts, err = parseArgs([]string{"--profile", "from-flag"})
	require.NoError(t, err)
	assert.Equal(t, "from-flag", opts.profile, "--profile 优先于 XBC_PROFILE")
}

func TestParseArgsUnknownFlagReportsErrorAndPrintsUsage(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	_, parseErr := parseArgs([]string{"--not-a-real-flag"})
	w.Close()
	os.Stderr = orig

	out, _ := io.ReadAll(r)
	assert.Error(t, parseErr, "未知 flag 必须报错")
	assert.Contains(t, string(out), "Usage", "未知 flag 必须连带打印 usage")
}

func TestParseArgsExtraPositionalArgIsAnError(t *testing.T) {
	_, err := parseArgs([]string{"--config", "x.yml", "extra-arg"})
	assert.Error(t, err, "flag 之后不该再有多余的位置参数")
}

func TestApplicationExampleYmlIsValidYAML(t *testing.T) {
	raw, err := os.ReadFile("application.example.yml")
	require.NoError(t, err, "样例配置文件必须存在于仓库根目录")

	// Uses koanf's own yaml parser (a dependency this plan has already locked
	// in), instead of pulling in an out-of-plan third-party package such as
	// gopkg.in/yaml.v3. This only does the generic check of "valid YAML with all
	// four sections present" -- fields under plugins other than server/log belong
	// to each Plan 4 plugin's own Config; kernel tests do not know gorm/redis/
	// jwt, and do not do schema-level validation here.
	doc, err := kyaml.Parser().Unmarshal(raw)
	require.NoError(t, err, "样例配置必须是合法 YAML")

	for _, section := range []string{"server", "log", "plugins", "app"} {
		_, ok := doc[section]
		assert.True(t, ok, "样例配置缺少 %s 段", section)
	}
}
```

`startuplog_test.go`：

```go
package xbc

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/log"
)

// ---- capturingLogger: records Info calls verbatim for assertions, without actually writing to a file ----

type capturingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturingLogger) Debug(msg string, kv ...any) { c.add(msg) }
func (c *capturingLogger) Info(msg string, kv ...any)  { c.add(msg) }
func (c *capturingLogger) Warn(msg string, kv ...any)  { c.add(msg) }
func (c *capturingLogger) Error(msg string, kv ...any) { c.add(msg) }
func (c *capturingLogger) With(kv ...any) log.Logger    { return c }
func (c *capturingLogger) Enabled(log.Level) bool       { return true }

func (c *capturingLogger) add(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, msg)
}

func (c *capturingLogger) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// ---- plugin fixtures: each implements only the small set of interfaces its test needs ----

type tableInitPlugin struct{ Base }

func (tableInitPlugin) Name() string               { return "gorm" }
func (tableInitPlugin) Init(*Context) error        { return nil }
func (tableInitPlugin) Stop(context.Context) error { return nil }

type tableRoutePlugin struct{ Base }

func (tableRoutePlugin) Name() string           { return "user" }
func (tableRoutePlugin) Migrate(*Context) error { return nil }
func (tableRoutePlugin) RegisterRoutes(*Router) {}

type capAllPlugin struct{ Base }

func (capAllPlugin) Name() string                { return "all" }
func (capAllPlugin) ConfigPtr() any               { return &struct{}{} }
func (capAllPlugin) Init(*Context) error          { return nil }
func (capAllPlugin) Migrate(*Context) error       { return nil }
func (capAllPlugin) Middlewares() []Middleware    { return nil }
func (capAllPlugin) RegisterRoutes(*Router)       {}
func (capAllPlugin) Start(*Context) error         { return nil }
func (capAllPlugin) Health(context.Context) error { return nil }
func (capAllPlugin) Stop(context.Context) error   { return nil }

func TestCapabilityTokensCoverAllInterfaceCombinations(t *testing.T) {
	a := &App{routeCounts: map[string]int{"all": 3}}
	inst := &instance{plugin: capAllPlugin{}, name: "all", instance: "default"}
	assert.Equal(t,
		[]string{"config", "init", "migrate", "middleware", "routes(3)", "runner", "health", "stop"},
		capabilityTokens(a, inst))
}

func TestCapabilityTokensOnlyListsImplementedInterfaces(t *testing.T) {
	inst := &instance{plugin: tableInitPlugin{}, name: "gorm", instance: "default"}
	assert.Equal(t, []string{"init", "stop"}, capabilityTokens(&App{}, inst))
}

func TestBuildPluginTableAlignsColumnsByActualContentWidth(t *testing.T) {
	gormInst := &instance{
		plugin:   tableInitPlugin{},
		name:     "gorm",
		instance: "default",
		src:      sourceImport,
		provides: []Dep{Offer[*fakeConn]()},
	}
	userInst := &instance{
		plugin:   tableRoutePlugin{},
		name:     "user",
		instance: "default",
		src:      sourceRegister,
		deps:     Deps{Types: []Dep{Need[*fakeConn]()}},
	}
	a := &App{routeCounts: map[string]int{"user": 12}}

	got := buildPluginTable(a, []*instance{gormInst, userInst})
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 4, "标题行 + gorm 一行 + user 一行 + user 的『显式 Register』标记行")

	assert.Equal(t, "xbc: 装配完成，2 个插件实例", lines[0])

	// "gorm"/"user" are both 4 characters; "migrate routes(12)" is the longest
	// capability column across the two rows, at 19 characters -- these two
	// numbers are exactly what the column width should compute to, not a number
	// this test picked by hand.
	const labelWidth, capsWidth = 4, 19
	wantGorm := fmtRow(labelWidth, "gorm", capsWidth, "init stop", "provides *xbc.fakeConn")
	wantUser := fmtRow(labelWidth, "user", capsWidth, "migrate routes(12)", "requires *xbc.fakeConn")
	assert.Equal(t, wantGorm, lines[1])
	assert.Equal(t, wantUser, lines[2])

	wantMark := strings.Repeat(" ", 2+labelWidth+2+capsWidth+2) + "↑ 显式 Register"
	assert.Equal(t, wantMark, lines[3], "user 是显式 Register，必须标出来")
}

// fmtRow assembles the expected value with exactly the same format string as
// buildPluginTable, so what the test locks down is the rule "column width is
// computed from actual content" itself, not a hand-counted number of spaces
// from one particular run.
func fmtRow(labelWidth int, label string, capsWidth int, caps, suffix string) string {
	return fmtSprintfRow(labelWidth, label, capsWidth, caps, suffix)
}

func TestRenderMiddlewareChainFormatsPhaseAndSoftOrder(t *testing.T) {
	chain := []mwEntry{
		{Middleware: Middleware{Name: "recovery", Phase: PhaseRecover}, qname: "xbc.recovery", plugin: "xbc"},
		{Middleware: Middleware{Name: "cors", Phase: PhaseSecurity}, qname: "cors", plugin: "cors"},
		{Middleware: Middleware{Name: "ratelimit", Phase: PhaseSecurity, After: []string{"cors"}}, qname: "ratelimit", plugin: "ratelimit"},
	}
	got := renderMiddlewareChain(chain)
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 4)
	assert.Equal(t, "xbc: 中间件链（3）", lines[0])
	assert.Contains(t, lines[1], "xbc.recovery")
	assert.Contains(t, lines[1], "[recover]")
	assert.Contains(t, lines[3], "ratelimit")
	assert.Contains(t, lines[3], "after=cors")
}

func TestRenderSoftMissesFormat(t *testing.T) {
	misses := []graph.Miss{{Node: "audit", Ref: "tracing", Dir: "after"}}
	got := renderSoftMisses(misses)
	want := "xbc: 软约束未命中（不影响启动）\n" +
		"  audit.After = \"tracing\" —— 无此插件，忽略\n" +
		"    → 拼写错误？还是忘了启用 plugins.tracing？"
	assert.Equal(t, want, got)
}

func TestRenderMigrationNoticeCountsMigratorsWhenNotMigrated(t *testing.T) {
	a := &App{migrate: false}
	insts := []*instance{
		{plugin: tableRoutePlugin{}, name: "user", instance: "default"},
		{plugin: tableInitPlugin{}, name: "gorm", instance: "default"},
	}
	got := renderMigrationNotice(a, insts)
	assert.Equal(t, "xbc: 迁移未执行（1 个插件声明了 Migrate，待检查）\n  → 需要迁移请使用 ./myapp migrate 或 --migrate", got)
}

func TestRenderMigrationNoticeEmptyWhenAlreadyMigrated(t *testing.T) {
	a := &App{migrate: true}
	insts := []*instance{{plugin: tableRoutePlugin{}, name: "user", instance: "default"}}
	assert.Empty(t, renderMigrationNotice(a, insts))
}

func TestRenderMigrationNoticeEmptyWhenNoMigrator(t *testing.T) {
	a := &App{migrate: false}
	insts := []*instance{{plugin: tableInitPlugin{}, name: "gorm", instance: "default"}}
	assert.Empty(t, renderMigrationNotice(a, insts))
}

func TestPrintStartupLogEmitsAllFourSections(t *testing.T) {
	cap := &capturingLogger{}
	log.SetLogger(cap)
	defer log.SetLogger(log.Nop())

	a := &App{
		softMisses:      []graph.Miss{{Node: "audit", Ref: "tracing", Dir: "after"}},
		middlewareChain: []mwEntry{{Middleware: Middleware{Name: "cors", Phase: PhaseSecurity}, qname: "cors"}},
	}
	insts := []*instance{{plugin: tableRoutePlugin{}, name: "user", instance: "default"}}

	a.printStartupLog(insts)

	lines := cap.snapshot()
	require.Len(t, lines, 4, "装配完成表、中间件链、软约束未命中、迁移未执行——四段缺一不可")
	assert.Contains(t, lines[0], "xbc: 装配完成")
	assert.Contains(t, lines[1], "xbc: 中间件链")
	assert.Contains(t, lines[2], "xbc: 软约束未命中")
	assert.Contains(t, lines[3], "xbc: 迁移未执行")
}
```

`arch_test.go`：

```go
package xbc

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInternalPackagesDoNotImportRootOrEachOther is the other half of the
// automated guard for Global Constraints' hard rule that internal/ stays
// pure -- the log/ half is already implemented in log/integration_test.go's
// TestLogPackageHasNoFrameworkDependency.
//
// Uses go list -test -deps instead of a string grep: grep gets polluted by
// package names that happen to appear in comments or string constants,
// producing false positives or false negatives; go list walks the real
// compile-time import graph, and -test makes sure it does not miss an import
// that only appears in a _test.go file.
func TestInternalPackagesDoNotImportRootOrEachOther(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过依赖方向检查")
	}
	pkgs := []string{
		"github.com/xbcio/xbc/internal/graph",
		"github.com/xbcio/xbc/internal/conf",
		"github.com/xbcio/xbc/internal/inject",
	}
	for _, pkg := range pkgs {
		out, err := exec.Command("go", "list", "-test", "-deps", pkg).Output()
		require.NoError(t, err, "go list -deps %s 失败", pkg)

		for _, line := range strings.Split(string(out), "\n") {
			dep := strings.TrimSpace(line)
			if dep == "" || isInternalPackageItself(dep, pkg) {
				continue
			}
			assert.NotEqual(t, "github.com/xbcio/xbc", dep,
				"%s 不得 import 根包，否则会跟根包 import internal/* 形成循环", pkg)
			for _, other := range pkgs {
				if other == pkg {
					continue
				}
				assert.False(t, dep == other, "%s 不得 import 兄弟包 %s，三个 internal 包彼此独立", pkg, other)
			}
		}
	}
}

// isInternalPackageItself filters out the synthetic entries "go list -test
// -deps" produces for the package under test itself: the bare package name,
// the test-instrumented variant "pkg [pkg.test]", and the compiled test
// binary "pkg.test".
func isInternalPackageItself(dep, pkg string) bool {
	switch {
	case dep == pkg, dep == pkg+".test":
		return true
	case strings.HasPrefix(dep, pkg+" ["), strings.HasPrefix(dep, pkg+"_test"):
		return true
	default:
		return false
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test . -run 'TestRun|TestParseArgs|TestApplicationExampleYml|TestCapabilityTokens|TestBuildPluginTable|TestRenderMiddleware|TestRenderSoftMisses|TestRenderMigrationNotice|TestPrintStartupLog|TestInternalPackages' -v
```

Expected: 编译失败，`undefined: parseArgs`（`cli.go` 还不存在），级联报出 `undefined: cliOptions`、`undefined: buildPluginTable`、`undefined: renderMiddlewareChain`、`a.middlewareChain`/`a.routeCounts`/`a.migrate` 字段未定义等一串符号缺失；`application.example.yml` 尚不存在会让 `TestApplicationExampleYmlIsValidYAML` 报 `open application.example.yml: no such file or directory`。

- [ ] **Step 3: 写最小完整实现**

`cli.go`：

```go
package xbc

import (
	"flag"
	"fmt"
	"os"
)

// cliOptions is the parsed command line: an optional subcommand plus the
// flags every subcommand shares.
type cliOptions struct {
	subcommand string // "" (normal boot) / "migrate" / "doctor"
	config     string
	profile    string
	migrate    bool
}

// parseArgs parses args with the standard library flag package -- spec §4.2
// deliberately excludes cobra, since xbc only ever has two subcommands and a
// handful of flags. The subcommand, if present, must be the first
// non-flag argument; flag requires every flag to precede positional
// arguments, so it is peeled off before handing the rest to fs.Parse.
func parseArgs(args []string) (cliOptions, error) {
	var opts cliOptions
	fs := flag.NewFlagSet("xbc", flag.ContinueOnError)
	fs.StringVar(&opts.config, "config", "", "配置文件路径")
	fs.StringVar(&opts.profile, "profile", "", "配置 profile（未设置时读取 XBC_PROFILE）")
	fs.BoolVar(&opts.migrate, "migrate", false, "启动前先跑一次迁移")

	rest := args
	if len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
		switch rest[0] {
		case "migrate", "doctor":
			opts.subcommand = rest[0]
			rest = rest[1:]
		default:
			fs.Usage()
			return opts, fmt.Errorf("xbc: 未知子命令 %q，可选 migrate/doctor，或不带子命令直接启动", rest[0])
		}
	}

	if err := fs.Parse(rest); err != nil {
		return opts, err // the flag package has already printed usage to fs.Output() (os.Stderr by default)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return opts, fmt.Errorf("xbc: 未知参数 %v", fs.Args())
	}

	if opts.profile == "" {
		opts.profile = os.Getenv("XBC_PROFILE")
	}
	return opts, nil
}
```

`xbc.go` 补 `Run()` / `run()` 方法体：

```go
// Run is the process entry point: main() calls this and nothing else. It
// parses os.Args, drives the pipeline, prints a fatal error to stderr on
// failure, and exits with the resulting code.
func (a *App) Run() {
	code, err := a.run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

// run is the testable core behind Run: it never touches os.Exit, so tests
// can call it directly and assert on the returned exit code without forking
// a subprocess. Exit code 2 marks a command-line usage error (flag package's
// own convention); 1 marks a runtime/pipeline failure; 0 is success.
func (a *App) run(args []string) (exitCode int, err error) {
	opts, err := parseArgs(args)
	if err != nil {
		return 2, err
	}
	a.migrate = opts.migrate || opts.subcommand == "migrate"

	if err := a.loadConfig(conf.Options{File: opts.config, Profile: opts.profile, EnvPrefix: "XBC_"}); err != nil {
		return 1, err
	}
	if !a.migrate && a.cfg.Server.AutoMigrate {
		a.migrate = true
	}

	insts, err := a.expand()
	if err != nil {
		return 1, err
	}
	if err := a.bindConfigs(insts); err != nil {
		return 1, err
	}
	order, misses, err := a.resolve(insts)
	if err != nil {
		return 1, err
	}
	a.order = order
	a.softMisses = append(a.softMisses, misses...)

	if opts.subcommand == "doctor" {
		a.printStartupLog(order) // reports only the assembly result, without establishing any connection
		return 0, nil
	}

	if err := a.initAll(order); err != nil {
		return 1, err
	}

	if err := a.migrateAll(order); err != nil {
		return 1, err
	}

	if opts.subcommand == "migrate" {
		a.rollback(order) // a one-shot process; even exiting right after finishing must Stop cleanly, leaving no lingering connections
		return 0, nil
	}

	if err := a.assembleHTTP(order); err != nil {
		return 1, err
	}
	a.printStartupLog(order)

	if err := a.startRunners(order); err != nil {
		return 1, err
	}

	if err := a.serve(); err != nil {
		return 1, err
	}
	if a.exitCode != 0 {
		return a.exitCode, nil
	}
	return 0, nil
}
```

（`xbc.go` 顶部的 `import` 块要加上 `"flag"` 间接依赖用到的 `"os"`、`"fmt"`，以及 `"github.com/xbcio/xbc/internal/conf"`——如果这些 import 已经因为其他 task 存在就不用重复加。）

`startuplog.go`：

```go
package xbc

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/log"
)

// printStartupLog renders spec §4.4's four sections: the assembled plugin
// table, the middleware chain, unresolved soft constraints, and (when
// migration did not run) a reminder of how to run it. A narrow-interface
// framework silently skips a plugin whose method name is misspelled -- the
// only backstop against that is spelling out exactly what got wired up.
func (a *App) printStartupLog(order []*instance) {
	l := log.L()
	l.Info(buildPluginTable(a, order))
	if len(a.middlewareChain) > 0 {
		l.Info(renderMiddlewareChain(a.middlewareChain))
	}
	if len(a.softMisses) > 0 {
		l.Info(renderSoftMisses(a.softMisses))
	}
	if note := renderMigrationNotice(a, order); note != "" {
		l.Info(note)
	}
}

// capabilityTokens lists a plugin instance's optional-interface capabilities
// in the fixed order laid out by spec §5.1's interface table: config, init,
// migrate, middleware, routes(N), runner, health, stop. (Spec §4.4's own
// example swaps routes(N) and migrate for exactly one row -- "user" -- while
// every other row in that same example follows this order; that swap is
// treated as a documentation slip, not a rule to reproduce.)
func capabilityTokens(a *App, inst *instance) []string {
	var tokens []string
	if _, ok := inst.plugin.(Configurable); ok {
		tokens = append(tokens, "config")
	}
	if _, ok := inst.plugin.(Initializer); ok {
		tokens = append(tokens, "init")
	}
	if _, ok := inst.plugin.(Migrator); ok {
		tokens = append(tokens, "migrate")
	}
	if _, ok := inst.plugin.(MiddlewareProvider); ok {
		tokens = append(tokens, "middleware")
	}
	if _, ok := inst.plugin.(RouteProvider); ok {
		tokens = append(tokens, fmt.Sprintf("routes(%d)", a.routeCounts[inst.id()]))
	}
	if _, ok := inst.plugin.(Runner); ok {
		tokens = append(tokens, "runner")
	}
	if _, ok := inst.plugin.(HealthChecker); ok {
		tokens = append(tokens, "health")
	}
	if _, ok := inst.plugin.(Closer); ok {
		tokens = append(tokens, "stop")
	}
	return tokens
}

// depsAndProvidesSuffix renders an instance's hard type dependencies and
// declared products for the trailing column of the plugin table.
func depsAndProvidesSuffix(inst *instance) string {
	var parts []string
	if len(inst.deps.Types) > 0 {
		var ts []string
		for _, d := range inst.deps.Types {
			ts = append(ts, d.String())
		}
		parts = append(parts, "requires "+strings.Join(ts, " "))
	}
	if len(inst.provides) > 0 {
		var ts []string
		for _, d := range inst.provides {
			ts = append(ts, d.String())
		}
		parts = append(parts, "provides "+strings.Join(ts, " "))
	}
	return strings.Join(parts, "  ")
}

// fmtSprintfRow renders one plugin-table row with the given column widths.
// Pulling this into its own function keeps buildPluginTable's row-building
// and its test's expected-value construction using the exact same format
// string, so the test locks in "width is computed from content" rather than
// a hand-counted number of spaces.
func fmtSprintfRow(labelWidth int, label string, capsWidth int, caps, suffix string) string {
	return strings.TrimRight(fmt.Sprintf("  %-*s  %-*s  %s", labelWidth, label, capsWidth, caps, suffix), " ")
}

// buildPluginTable renders the "assembled" section: one row per instance,
// columns aligned to the actual content width of this run -- hardcoding a
// width would misalign the moment a plugin name or capability list is
// longer or shorter than whatever the author happened to test with.
func buildPluginTable(a *App, order []*instance) string {
	type row struct {
		label    string
		caps     string
		suffix   string
		explicit bool
	}
	rows := make([]row, len(order))
	labelWidth, capsWidth := 0, 0
	for i, inst := range order {
		r := row{
			label:    inst.label(),
			caps:     strings.Join(capabilityTokens(a, inst), " "),
			suffix:   depsAndProvidesSuffix(inst),
			explicit: inst.src == sourceRegister,
		}
		rows[i] = r
		if len(r.label) > labelWidth {
			labelWidth = len(r.label)
		}
		if len(r.caps) > capsWidth {
			capsWidth = len(r.caps)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "xbc: 装配完成，%d 个插件实例", len(order))
	for _, r := range rows {
		b.WriteString("\n")
		b.WriteString(fmtSprintfRow(labelWidth, r.label, capsWidth, r.caps, r.suffix))
		if r.explicit {
			b.WriteString("\n")
			b.WriteString(strings.Repeat(" ", 2+labelWidth+2+capsWidth+2))
			b.WriteString("↑ 显式 Register")
		}
	}
	return b.String()
}

// renderMiddlewareChain renders the ordered middleware section, one line per
// entry, with soft-order hints (after=/before=) appended when declared.
func renderMiddlewareChain(chain []mwEntry) string {
	nameWidth := 0
	for _, e := range chain {
		if l := len(e.qname); l > nameWidth {
			nameWidth = l
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "xbc: 中间件链（%d）", len(chain))
	for i, e := range chain {
		line := fmt.Sprintf("  %d. %-*s  [%s]", i+1, nameWidth, e.qname, e.Phase.String())
		if len(e.After) > 0 {
			line += "  after=" + strings.Join(e.After, ",")
		}
		if len(e.Before) > 0 {
			line += "  before=" + strings.Join(e.Before, ",")
		}
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

// renderSoftMisses renders every soft ordering constraint that pointed at a
// plugin which does not exist -- harmless to startup, but silent typos in
// After/Before are exactly the kind of bug nobody notices without this line.
func renderSoftMisses(misses []graph.Miss) string {
	var b strings.Builder
	b.WriteString("xbc: 软约束未命中（不影响启动）")
	for _, m := range misses {
		dir := "After"
		if m.Dir == "before" {
			dir = "Before"
		}
		fmt.Fprintf(&b, "\n  %s.%s = %q —— 无此插件，忽略\n    → 拼写错误？还是忘了启用 plugins.%s？", m.Node, dir, m.Ref, m.Ref)
	}
	return b.String()
}

// renderMigrationNotice reports how many instances declared Migrate but had
// it skipped this run. Spec §4.4's example counts "12 个模型" -- a concept
// gorm itself owns (it knows how many models it registered); the kernel does
// not know what a "model" is. The count here is the number of Migrator-
// implementing instances instead, which is the same warning ("something
// declared it needs migrating and didn't get it") stated in a vocabulary the
// kernel actually has.
func renderMigrationNotice(a *App, order []*instance) string {
	if a.migrate {
		return ""
	}
	n := 0
	for _, inst := range order {
		if _, ok := inst.plugin.(Migrator); ok {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("xbc: 迁移未执行（%d 个插件声明了 Migrate，待检查）\n  → 需要迁移请使用 ./myapp migrate 或 --migrate", n)
}
```

`application.example.yml`：

```yaml
# application.example.yml —— 配置样例，复制为 application.yml 后按需修改。
# 四个顶级命名空间：server / log 是框架保留字段；plugins 是插件配置区；
# app 是业务自由区，框架不解析，也不做 ENV 覆盖（裁决 R2 的边界）。

server:
  addr: :8080                 # 监听地址
  base_path: /api/v1           # 根路由前缀，所有 RegisterRoutes 注册的路径都挂在它下面
  read_timeout: 10s            # 读超时，防止慢速请求头占满连接
  write_timeout: 30s           # 写超时，同上，防止慢速写入占满连接（裁决 R10 补的字段）
  shutdown_timeout: 30s        # 优雅关闭的最长等待时间，超时后强杀还在处理的连接
  auto_migrate: false          # 默认关闭迁移；开发环境可以在 application-dev.yml 里覆盖成 true

log:
  level: info                  # debug/info/warn/error
  caller: true                 # 是否记录调用位置
  console:
    enabled: true               # 终端人读，带色对齐
  file:
    enabled: true
    path: logs/app.log.jsonl    # 后缀决定渲染格式：.log -> console，.jsonl -> json

plugins:
  gorm:
    default:
      dsn: ${MYSQL_DSN}                    # 敏感信息走 ENV 插值，绝不写真实连接串
      max_open_conn: 20
      max_idle_conn: 10
      conn_max_life: 1h
    readonly:
      dsn: ${MYSQL_READONLY_DSN}
      max_open_conn: 50

  redis:
    default:
      addr: 127.0.0.1:6379
      password: ${REDIS_PASSWORD}          # 同样走 ENV，配置文件里不出现明文密码

  cors:
    allow_origins:
      - https://example.com

  jwt:
    secret: ${JWT_SECRET}                  # 绝不写真实密钥；本地开发用 .env 或 shell 导出这个变量

  cron:
    enabled: false                         # 一行关掉整个插件，不用改代码重新编译

app:
  # 业务自定义配置，框架不解析
  feature_x: true
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test . -run 'TestRun|TestParseArgs|TestApplicationExampleYml|TestCapabilityTokens|TestBuildPluginTable|TestRenderMiddleware|TestRenderSoftMisses|TestRenderMigrationNotice|TestPrintStartupLog|TestInternalPackages' -v
```

Expected: 全部 PASS。

（人工验证 `arch_test.go` 的守卫真的守得住，不进自动化，留痕在这里）：临时在 `internal/graph/graph.go` 顶部加一行 `import _ "github.com/xbcio/xbc"`，重跑 `go test -run TestInternalPackagesDoNotImportRootOrEachOther .`。因为根包本身已经 import `internal/graph`，这一行会立刻造成真正的 Go import cycle，`go list -test -deps` 会直接报 `import cycle not allowed`，测试在 `require.NoError(t, err, ...)` 那一行就炸——比资产断言更早更硬地拦下，这是符合预期的失败方式。验证完撤掉这行 import。再验证「互相 import」那一半：临时在 `internal/conf` 里随便一个文件加一行 `import _ "github.com/xbcio/xbc/internal/graph"`——`conf` 和 `graph` 之间没有反向依赖，这次不会有 import cycle，`go list` 能正常返回，测试会在 `assert.False(t, dep == other, ...)` 这一行断言失败，文案里带上 `internal/conf` 和 `internal/graph` 两个包名。验证完同样撤掉。两种破坏方式分别失败在不同的行，说明守卫的两个判断——「不认根包」与「不认兄弟包」——都真的在起作用，不是同一行代码凑巧同时满足了两条断言。

- [ ] **Step 5: 跑完整验收三件套 + 覆盖率，然后 Commit**

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
go test ./... -race -count=1
go test ./... -cover
```

Expected：`gofmt -l .` 空输出；`go vet` 无报错；两轮 `go test` 全部 PASS，`-race` 不报数据竞争；`-cover` 打出根包与三个 `internal/*` 包各自的覆盖率百分比——把这次实际跑出来的数字填进下面的提交信息（不要照抄示例数字，以本地这次真实输出为准）。

```bash
git add cli.go startuplog.go application.example.yml arch_test.go xbc.go cli_test.go startuplog_test.go
git commit -m "feat(xbc): CLI 入口——flag 子命令/启动日志/配置样例/internal 洁癖守卫（覆盖率 xx.x%）"
```

十五个 task 至此全部完成：spec §13 第 1~4 步的验收线——「内核可用假插件跑通全部测试，不接触任何真实中间件」——在本 task 的 `go test ./... -race -count=1` 这一步兑现。
