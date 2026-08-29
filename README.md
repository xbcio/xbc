# xbc

xbc 是一个与传输协议无关的 Go 插件应用运行时。核心只负责插件发现、配置绑定、依赖排序、生命周期、托管任务和逆序关闭；HTTP 等运行栈通过独立 module 接入，不进入 core 的依赖闭包。

当前仓库仍处于首个正式 tag 之前。仓库内开发由已提交的 `go.work` 联结三个 module；发布时则必须按 core → transport/web → examples 的顺序使用真实版本验证，不能依赖本地 `replace`。

## 目录与边界

| 路径 | 职责 |
| --- | --- |
| 根目录 | `xbc.Run`、`App`、`New` 等六符号稳定薄门面；实现委托给 `runtime` |
| `runtime/` | App 状态、启动执行、生命周期、托管任务、关机和进程适配 |
| `assembly/` | Definition 实例装配、配置绑定、依赖解析、初始化与值注册表 |
| `assembly/inject/` | 仅依赖标准库的反射 tag 扫描与字段操作 |
| `cli/` | 仅依赖标准库的命令解析 |
| `plugin/` | 插件 SPI、生命周期、依赖和扩展契约 |
| `plugin/catalog/` | 无副作用 Definition 目录及不可变快照 |
| `config/` | 分层配置、严格绑定和只读 View |
| `log/` | 可独立使用的日志与 trace 门面 |
| `plugin/ordering/` | 插件贡献项共享的稳定排序能力；仅依赖标准库 |
| `transport/` | 可选协议运行栈的仓库级命名空间；不提供共享 Go package |
| `transport/web/` | 独立 Go module；Gin-backed Web 插件 |
| `examples/` | 独立 Go module；可运行的消费方示例 |
| `tests/architecture/` | 依赖方向、API 和 module 边界守卫 |
| `tests/integration/` | 外部消费者视角的公开 API 测试 |

根级 package path 都是有意公开的。根包 `xbc` 是普通应用推荐使用的高层门面；`runtime`、`assembly`、`cli` 是面向高级嵌入、自定义装配和进程适配的低层公开 API，不是私有实现包。仓库内的下层 SPI、可选 transport 和 quickstart 仍遵守单向依赖，不反向绕过门面或领域 owner。

包是依赖边界，不是文件收纳盒。不要为目录整齐新增 `common`、`utils`、`pkg`，也不要在能力实现前创建空的 `transport/grpc`、`management` 或 `integration` module。完整设计见[包布局设计](docs/superpowers/specs/2026-08-26-xbc-package-layout-design.md)。

## 本地运行

要求 Go 1.25 或更高版本。从仓库根目录运行 quickstart：

```bash
go run ./examples/quickstart doctor --config examples/quickstart/application.yml
go run ./examples/quickstart --config examples/quickstart/application.yml
curl localhost:8080/api/v1/hello
```

配置命名空间、覆盖顺序和环境变量规则见[配置约定](docs/configuration.md)。

## 开发与验证

```bash
make check      # gofmt 检查 + 每个 workspace module 的 go vet/go test
make test-race  # 每个 workspace module 的 race test
make fmt        # 格式化全仓 Go 文件
```

不要只用根目录的 `go test ./...` 作为全仓验证：Go 的递归 package pattern 不会进入嵌套 module。`make check` 和 CI 会从 workspace 动态发现并逐个验证 core、transport/web、examples，以及未来加入 `go.work` 的 module。
