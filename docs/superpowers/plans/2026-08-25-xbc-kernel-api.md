# xbc 内核 API 契约（Plan 2 唯一签名来源）

> 这份文件是 `docs/superpowers/plans/2026-08-25-xbc-kernel.md` 的附属契约。
> 计划里任何 task 用到的类型、函数、方法签名，**以本文件为准**。
> 两处不一致时改 task，不改本文件。

## 0. 包与依赖方向

```
github.com/xbcio/xbc                 根包：全部公开 API
  ├─ imports github.com/xbcio/xbc/log            (已完成，不改)
  ├─ imports github.com/xbcio/xbc/internal/graph
  ├─ imports github.com/xbcio/xbc/internal/conf
  ├─ imports github.com/xbcio/xbc/internal/inject
  └─ imports github.com/gin-gonic/gin

internal/graph   只 import 标准库
internal/conf    只 import 标准库 + koanf + validator
internal/inject  只 import 标准库 + reflect
```

三个 `internal/` 包**都不认识任何 xbc 类型**，入参出参只用标准库类型、`reflect` 类型和自己定义的类型。

---

## 1. `plugin.go`

```go
// Plugin is the only interface a plugin must implement.
type Plugin interface {
	Name() string
}

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
type Closer interface{ Stop(ctx context.Context) error }
type HealthChecker interface{ Health(ctx context.Context) error }

// Base is the convenience accessor set, the Name() auto-derivation carrier,
// and the anchor the framework uses to hand a plugin its Context.
type Base struct {
	ctx  *Context
	name string
}

func (b *Base) Ctx() *Context   // nil before stage 5
func (b *Base) Log() log.Logger
func (b *Base) Name() string    // returns the framework-derived name
func (b *Base) base() *Base     // unexported anchor, satisfies baseAnchor

type baseAnchor interface{ base() *Base }

// deriveName reflects a plugin value's concrete package path and returns its
// last segment, skipping a trailing major-version element ("/v2").
func deriveName(p Plugin) (string, error)

// bindBase wires ctx and name into an embedded Base. Returns false when the
// plugin does not embed Base -- legal, just means no convenience accessors.
func bindBase(p Plugin, ctx *Context, name string) bool

// nameReserved reports whether s contains a character the framework reserves.
// Reserved: '.' (middleware qualification), '[' ']' (instance display),
// whitespace, and any character outside [a-z0-9_-].
//
// It does NOT lowercase first: an uppercase letter is rejected, not folded.
// Folding would let "Gorm" and "gorm" silently collide into one plugin name,
// and the conflict would surface as a confusing duplicate-registration panic
// far from the typo that caused it.
func validateName(s string) error
```

`deriveName` 的规则：`reflect.TypeOf(p)` 取到 `*jwt.Plugin` → `Elem().PkgPath()` = `github.com/xbcio/xbc/plugins/jwt` → 末段 `jwt`。末段形如 `v` + 纯数字时取前一段。非指针、非结构体指针、匿名类型 → error。

---

## 2. `xbc.go`

```go
// source records which registration path a plugin arrived by. The two paths
// carry different intent strength, so they get different enable rules (§6.4).
type source int

const (
	sourceImport   source = iota // blank import + init(): capability is available
	sourceRegister               // app.Register(): an explicit statement of intent
)

// entry is one registered plugin prototype, before expansion.
type entry struct {
	proto Plugin
	name  string
	src   source
	multi bool
}

// Register registers a plugin from a package init(). Enabled only when a
// matching config section exists.
func Register(p Plugin)

type App struct { /* unexported */ }

func New() *App
func (a *App) Register(p ...Plugin) *App
func (a *App) Run()
func (a *App) run(args []string) (exitCode int, err error) // testable core

// instance is one expanded plugin instance -- the unit everything after
// stage 2 operates on.
type instance struct {
	plugin   Plugin
	name     string // plugin name, e.g. "gorm"
	instance string // instance name, e.g. "default" / "readonly"
	src      source
	ctx      *Context
	fields   []inject.FieldSpec
	deps     Deps
	provides []Dep
	inited   bool
}

// id returns the graph node id: "gorm" for a single-instance plugin,
// "gorm[readonly]" for a named instance other than default.
func (i *instance) id() string

// label returns the display form used in startup logs and error copy.
// Same as id(), except default instances of multi-instance plugins render
// as "gorm[default]" so the reader can tell multi from single.
func (i *instance) label() string
```

`defaultInstance = "default"` 是包级常量。

`App` 与 `instance` 的**全部字段由 Task 1 一次性声明**（裁决 R12），后续 task 只读写、不新增、不改名。五个例外因为类型当时还不存在，由各自的 task 用 `Edit` 追加：`App.cfg *Config`（Task 6）、`App.registry *registry`（Task 4）、`App.softMisses []graph.Miss` 与 `App.middlewareChain []mwEntry`（Task 14）、`instance.fields []inject.FieldSpec`（Task 10）。

---

## 3. `deps.go`

```go
type Dep struct {
	Type     reflect.Type
	Instance string // "" means default
	Optional bool
}

func Need[T any]() Dep
func NeedNamed[T any](instance string) Dep
func Opt[T any]() Dep
func Offer[T any]() Dep

type Ref struct {
	typ      reflect.Type
	instance string // "" means any instance
}

func RefOf[T Plugin]() Ref
func (r Ref) Instance(name string) Ref

type Deps struct {
	Types   []Dep    // hard: I need an instance of this type
	Plugins []Ref    // hard: this plugin must be present
	After   []string // soft ordering preference
	Before  []string
}

// typeOf works for both concrete and interface T.
func typeOf[T any]() reflect.Type // reflect.TypeOf((*T)(nil)).Elem()

// String renders a Dep for error copy: "*gorm.DB" or "*gorm.DB[readonly]".
func (d Dep) String() string
func (r Ref) String() string
```

`Dep.Instance == ""` 与 `"default"` 在查表时等价，统一由 `normInstance(s string) string` 归一（空串 → `"default"`）。

---

## 4. `internal/graph`

```go
package graph

// Graph is a stable topological sorter. Node insertion order is the tiebreak
// among nodes that no edge separates, so the same input always sorts the same.
type Graph struct { /* unexported */ }

func New() *Graph

// AddNode is idempotent. The first call fixes the node's insertion index.
func (g *Graph) AddNode(id string)

// AddEdge declares that from must be ordered before to.
//
// hard=true: both endpoints must exist; a missing endpoint is an error at
// Sort time. hard=false: a missing endpoint is dropped and reported as a Miss.
func (g *Graph) AddEdge(from, to string, hard bool)

// Sort returns nodes in dependency order together with every soft edge that
// referenced a node that does not exist.
func (g *Graph) Sort() (order []string, misses []Miss, err error)

// Miss is a soft edge whose other endpoint was never registered.
type Miss struct {
	Node string // the node that declared the constraint
	Ref  string // the name it referenced, which does not exist
	Dir  string // "after" or "before"
}

// CycleError reports a dependency cycle with the full path, first node repeated
// at the end: user -> order -> payment -> user
type CycleError struct{ Path []string }

func (e *CycleError) Error() string

// MissingNodeError reports a hard edge pointing at a node that does not exist.
type MissingNodeError struct{ From, To string }

func (e *MissingNodeError) Error() string
```

排序算法：Kahn + 以插入序为键的最小堆，保证同层稳定。环路径靠残余子图上的 DFS 还原。

多个环共存时，`Sort` 报的是 **DFS 按插入序第一个撞到的那个环**——只报一个，不是全部。
这个选择是确定性的：DFS 的根从插入序取，不经过任何 map 迭代，所以同一份输入两次运行
必然报同一个环（已实测：双环图两种插入序各跑 60 次，结果各自恒定）。这点对上层重要——
用户拿着报错去改配置，改完重跑如果报出另一个环，那是**还有第二个环**，不是随机漂移。


---

## 5. `registry.go`

```go
type registryKey struct {
	typ      reflect.Type
	instance string
}

type registry struct {
	mu    sync.RWMutex
	m     map[registryKey]any
	order []registryKey // stable iteration, for deterministic error copy
}

func newRegistry() *registry
func (r *registry) put(typ reflect.Type, instance string, v any)
func (r *registry) lookup(want reflect.Type, instance string) (any, error)
func (r *registry) concreteTypes(instance string) []reflect.Type

// Provide registers v under the calling plugin's own instance name.
func Provide[T any](ctx *Context, v T)

func Get[T any](ctx *Context) (T, bool)
func GetNamed[T any](ctx *Context, name string) (T, bool)
func MustGet[T any](ctx *Context) T
func MustGetNamed[T any](ctx *Context, name string) T
```

`lookup` 的语义（§5.6）：

| `want` 的种类 | 行为 |
|---|---|
| 具体类型 | 精确命中 `(want, instance)`；未命中 → `*NotFoundError` |
| 接口类型 | 先试精确命中；否则扫描同 `instance` 下全部已登记的具体类型，取 `AssignableTo(want)` 的 |
| 接口，命中 1 个 | 返回 |
| 接口，命中 0 个 | `*NotFoundError`，`Closest` 字段带「方法最接近的类型 + 缺哪几个方法」 |
| 接口，命中 >1 个 | `*AmbiguousError`，列出全部候选 |

```go
type NotFoundError struct {
	Want     reflect.Type
	Instance string
	Closest  reflect.Type // nil when the registry is empty
	Missing  []string     // method names Closest lacks
}

type AmbiguousError struct {
	Want       reflect.Type
	Instance   string
	Candidates []reflect.Type
}
```

「最接近」的判据：对每个已登记具体类型，数它实现了 `want` 的多少个方法（按方法名比对，签名不符也算缺），取最多的那个；并列时取先登记的。

---

## 6. `context.go`

```go
type Context struct {
	app      *App
	name     string // plugin name
	instance string // instance name, always non-empty ("default" by default)
	logger   log.Logger
}

func (c *Context) Log() log.Logger // pre-bound with plugin= and instance=
func (c *Context) Config() *Config
func (c *Context) Instance() string
func (c *Context) Name() string

func (c *Context) Go(fn func(context.Context))
func (c *Context) GoCritical(fn func(context.Context))

// Route / Routes are the Plan 3 seam. Plan 2 implements them against the
// frozen minimal route table (method + path only).
func (c *Context) Route(gc *gin.Context) *RouteInfo
func (c *Context) Routes() []RouteInfo
```

`Log()` 的字段：单实例插件只带 `plugin=<name>`；多实例插件带 `plugin=<name> instance=<instance>`。

---

## 7. `config.go`

```go
type Config struct {
	Server ServerConfig `yaml:"server"`
	Log    log.Config   `yaml:"log"`

	k *koanf.Koanf // raw access for app.* and plugin sections
}

type ServerConfig struct {
	Addr            string        `yaml:"addr"             default:":8080"`
	BasePath        string        `yaml:"base_path"        default:"/"`
	ReadTimeout     time.Duration `yaml:"read_timeout"     default:"10s"`
	WriteTimeout    time.Duration `yaml:"write_timeout"    default:"30s"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"30s"`
	AutoMigrate     bool          `yaml:"auto_migrate"`
}

// Unmarshal binds a config subtree into out, applying the full stage-3 chain
// (file -> ENV -> default -> nothing else). It does NOT run validate.
func (c *Config) Unmarshal(path string, out any) error

func (c *Config) Get(path string) any
func (c *Config) Exists(path string) bool
func (c *Config) Sub(path string) map[string]any // nil when absent or not a map
```

---

## 8. `internal/conf`

```go
package conf

type Options struct {
	File      string // --config; when non-empty the file MUST exist
	Profile   string // --profile or XBC_PROFILE
	EnvPrefix string // "XBC_"
	Overrides map[string]any
}

// Load searches for the config file, merges the profile overlay, applies
// Overrides, and returns the koanf instance. Missing files are only an error
// when Options.File named one explicitly (ruling R8).
func Load(opts Options) (*koanf.Koanf, error)

// Bind performs the stage-3 chain for one subtree:
//   1. unmarshal k's subtree at path into out (mapstructure, Tag "yaml")
//   2. overlay ENV, driven by out's own yaml-tag schema (ruling R2)
//   3. fill `default:"..."` tags on leaves that neither the file nor ENV set
//
// It does not validate. envPrefix is normally "XBC_".
func Bind(k *koanf.Koanf, path string, out any, envPrefix string) error

// EnvName maps a config path to its environment variable name:
// "plugins.gorm.default.dsn" -> "XBC_PLUGINS_GORM_DEFAULT_DSN"
func EnvName(prefix, path string) string

// Leaves walks a struct's yaml-tag schema and returns every leaf path,
// relative to root, together with the reflect path to set it.
func Leaves(root string, out any) []Leaf

type Leaf struct {
	Path  string // "plugins.gorm.default.max_open_conn"
	Index []int  // reflect field index path
	Type  reflect.Type
}

// Validate runs go-playground/validator over out and renders every violation
// as a Chinese line prefixed with its full config path.
func Validate(out any, path string) error

// ValidationError carries one line per violation, already rendered.
type ValidationError struct{ Lines []string }

func (e *ValidationError) Error() string
```

`Bind` 内部的标量解析（ENV 值与 `default` tag 值共用一个解析器）：

```go
// setScalar parses s according to typ and stores it into v.
// Supported: string, bool, all int/uint widths, float32/64,
// time.Duration (via time.ParseDuration), []string (comma separated).
func setScalar(v reflect.Value, typ reflect.Type, s string) error
```

`time.Duration` 必须在 int64 之前判断 —— 它底层就是 int64，判断顺序反了会把 `"1h"` 当整数解析失败。

---

## 9. `internal/inject`

```go
package inject

type Kind int

const (
	KindInject Kind = iota
	KindProvide
)

type FieldSpec struct {
	Index    int // top-level field index
	Name     string
	Type     reflect.Type
	Instance string // "" means default; always "" for KindProvide
	Optional bool   // only meaningful for KindInject
	Kind     Kind
}

// Scan reads the xbc struct tags off a plugin value. v must be a non-nil
// pointer to a struct.
//
// Only exported top-level fields are considered; embedded structs are not
// recursed into.
func Scan(v any) ([]FieldSpec, error)

// Set assigns val to the field described by spec on v.
func Set(v any, spec FieldSpec, val any) error

// IsZero reports whether the field described by spec is still its zero value.
func IsZero(v any, spec FieldSpec) (bool, error)

// Value returns the current value of the field described by spec.
func Value(v any, spec FieldSpec) (any, error)
```

tag 语法与报错：

| tag | 结果 |
|---|---|
| `xbc:"inject"` | `Kind=KindInject, Instance="", Optional=false` |
| `xbc:"inject,name=readonly"` | `Instance="readonly"` |
| `xbc:"inject,optional"` | `Optional=true` |
| `xbc:"inject,name=ro,optional"` | 两者并存，顺序不限 |
| `xbc:"provide"` | `Kind=KindProvide` |
| `xbc:"-"` | 跳过 |
| `xbc:"provide,name=x"` | **error**：产物实例名来自插件自己，不能由 tag 指定 |
| `xbc:"provide,optional"` | **error**：产物没有可选一说 |
| `xbc:"injct"` | **error**：未知动作，列出合法取值 |
| `xbc:"inject,nmae=x"` | **error**：未知选项 |
| 未导出字段带 `xbc` tag | **error**：反射设不进去，静默跳过等于埋雷 |

---

## 10. `middleware.go` 与 `mwchain.go`

```go
type Phase int

const (
	PhaseRecover  Phase = iota * 100 // 0   outermost, panic backstop
	PhaseObserve                     // 100 tracing, access log
	PhaseSecurity                    // 200 cors / ratelimit / replay defense
	PhaseAuth                        // 300 authentication and authorization
	PhaseBusiness                    // 400 business middleware
)

func (p Phase) String() string // "recover" / "observe" / ... / "phase(150)"

type Middleware struct {
	Name    string
	Phase   Phase
	After   []string
	Before  []string
	Handler gin.HandlerFunc
}

// qualify applies ruling R5.
func qualify(plugin, name string) string

// mwEntry is one middleware after name qualification.
type mwEntry struct {
	Middleware
	qname  string
	plugin string
}

// orderMiddlewares groups by Phase, sorts within each group, then concatenates.
// A cross-phase constraint pointing backwards against Phase order aborts.
func orderMiddlewares(entries []mwEntry) (ordered []mwEntry, misses []graph.Miss, err error)

// PhaseConflictError reports a cross-phase constraint that contradicts Phase order.
type PhaseConflictError struct {
	From, To       string
	FromPhase, ToPhase Phase
	Dir            string // "after" or "before"
}
```

---

## 11. `router.go`（Plan 2 的最小形态）

```go
// RouteInfo is one entry in the frozen route table. Plan 3 adds the metadata
// fields (Public, Name, Doc); Plan 2 freezes method and path only.
type RouteInfo struct {
	Method string
	Path   string
}

type Router struct {
	engine   *gin.Engine
	group    *gin.RouterGroup
	basePath string
	routes   *[]RouteInfo
	frozen   *bool
}

func (r *Router) Group(path string) *Router
func (r *Router) Handle(method, path string, h ...gin.HandlerFunc)
func (r *Router) GET(path string, h ...gin.HandlerFunc)
func (r *Router) POST(path string, h ...gin.HandlerFunc)
func (r *Router) PUT(path string, h ...gin.HandlerFunc)
func (r *Router) DELETE(path string, h ...gin.HandlerFunc)
func (r *Router) PATCH(path string, h ...gin.HandlerFunc)
```

冻结后再调 `Handle` → panic，文案说明「路由表已在阶段 7 冻结，`PostRoutes` 里不能再加路由」。

---

## 12. 阶段函数（根包，`stage_*.go`）

```go
func (a *App) loadConfig(opts conf.Options) error                   // stage 1，落在 config.go
func (a *App) expand() ([]*instance, error)                         // stage 2
func (a *App) bindConfigs(insts []*instance) error                  // stage 3
func (a *App) resolve(insts []*instance) ([]*instance, []graph.Miss, error) // stage 4
func (a *App) initAll(insts []*instance) error                      // stage 5
func (a *App) migrateAll(insts []*instance) error                   // stage 6
func (a *App) assembleHTTP(insts []*instance) error                 // stage 7
func (a *App) startRunners(insts []*instance) error                 // stage 8
func (a *App) serve() error                                         // stage 9
func (a *App) shutdown(reason string) error                         // stage 10

// rollback stops every already-initialized instance in reverse topological
// order. Called when any of stages 5-8 fails.
func (a *App) rollback(insts []*instance)
```

`a.order` 保存阶段 4 算出的拓扑序（`[]*instance`），阶段 10 与 `rollback` 都取它的逆序。

阶段 2~10 各有专属的 `stage_*.go`；阶段 1 例外，它跟 `Config` 共享 `defaultEnvPrefix` / `bindLog` / `Config.k` 三个未导出符号，所以留在 `config.go` 里。`inst.fields` 由阶段 4 的 `resolve` 一次扫出（`inject.Scan`），阶段 5 直接读，不重扫。

---

## 13. 错误文案（逐字，测试要断言）

阶段 4 硬依赖缺失（spec §5.9）：

```
xbc: 依赖检查失败
  插件 audit 依赖插件 jwt，但 jwt 未启用
    → 在 application.yml 中添加 plugins.jwt 配置节
  插件 report 依赖 gorm[readonly]，当前只有 gorm[default]
    → 在 plugins.gorm 下添加 readonly 实例
  插件 user 需要 *redis.Client，无任何插件提供
    → 是否忘了 import github.com/xbcio/xbc/plugins/redis
```

成环：

```
xbc: 依赖成环
  user → order → payment → user
```

阶段 5 产物为零值（spec §5.7）：

```
xbc: 插件 gorm[readonly] 声明产出 *gorm.DB，但 Init 后该字段仍为 nil
  → 检查 Init 中是否忘记给 DB 字段赋值
```

阶段 3 配置错误（spec §6.3）：

```
xbc: 配置错误
  plugins.gorm.readonly.dsn   必填项缺失
  plugins.redis.default.addr  不是合法的 host:port —— 得到 "127.0.0.1"
```

孤儿配置节（spec §6.2，裁决 R6 定为致命）：

```
xbc: plugins.kafka 有配置但无对应插件
  → 是否忘了 import github.com/xbcio/xbc/plugins/kafka？
```

接口匹配歧义（spec §5.6）：

```
xbc: 插件 ratelimit 需要 xbc_test.Counter，有 2 个候选
  *fake.RedisA[default]
  *fake.RedisB[default]
  → 用 xbc:"inject,name=xxx" 指定实例消歧
```

接口匹配零命中：

```
xbc: 插件 ratelimit 需要 xbc_test.Counter，无任何插件提供
  最接近的是 *fake.HalfCounter，缺少方法：Expire
```

启动日志见 spec §4.4，逐字实现。
