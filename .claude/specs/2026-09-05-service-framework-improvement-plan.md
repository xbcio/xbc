# XBC 可装配服务框架改进方案

- **日期**：2026-09-05
- **状态**：proposed
- **范围**：先稳定 Web 与 Core，再完成发布闭环，实现真实 gRPC 运行栈，最后进入微服务能力
- **关联设计**：
  - [XBC 插件框架设计](../../docs/superpowers/specs/2026-08-23-xbc-plugin-framework-design.md)
  - [XBC 包布局设计](../../docs/superpowers/specs/2026-08-26-xbc-package-layout-design.md)

## 1. 目标摘要

XBC 的目标不是成为以 Gin 或 gRPC 为内核的单一协议框架，而是成为一个通过静态 Go Plugin 显式装配应用的服务运行时和产品化能力集合。应用在 composition root 中选择 Bundle，即可构建：

- 开箱即用的 Web 服务；
- 开箱即用的 gRPC 服务；
- 只运行 worker、scheduler 或 integration 的后台服务；
- 明确选择后同时承载多种运行栈的服务。

Core 只拥有 Definition、依赖规划、严格配置、资源所有权、生命周期、托管任务、流量门和确定性关机。Web 与 gRPC 分别拥有自己的路由/服务注册、middleware/interceptor、协议错误、健康暴露和监听器实现。只有被两个真实运行栈共同证明稳定的 identity、security、health、telemetry 等能力，才抽到协议无关 owner。

这里的 Plugin 是编译期链接并由 immutable Definition 描述的 Go 组件，不是 `.so`、运行时下载、热插拔或 package scanning。配置只能激活已由 Bundle 组合的 Definition，不能发现未编译进程序的代码。

## 2. 完成定义

达到阶段性产品目标时，应同时满足以下条件。

### 2.1 开发体验

- 最小应用只需一个清晰的 composition root，不需要复制框架内部装配代码。
- Web 或 gRPC Starter 在少量配置下提供可运行的生产基线。
- 所有部署相关配置均可由 ENV 表达，包括插件激活和有明确语法的多实例选择。
- `doctor` 在不创建连接、goroutine、listener 或修改进程全局状态的前提下，报告最终插件图、实例、配置来源和禁用原因。

### 2.2 运行时正确性

- 依赖、contract、配置、认证策略和执行顺序问题在开放流量前失败。
- 构造失败完整回滚，资源有且只有一个 owner。
- 正常关机严格按依赖逆序执行；超时和不遵守 context 的 Hook 有明确、可测试且不夸大的降级语义。
- Web 与 gRPC 共享同一个应用生命周期和 traffic gate，但互不依赖对方的类型。
- 认证只产生一个可信 Principal，授权、租户和审计只消费认证结果，不各自重新解释凭据。

### 2.3 发布与消费

- 每个可发布 module 都能在 `GOWORK=off`、`-mod=readonly` 下独立构建和测试。
- sibling module 使用真实版本，不依赖本地 `replace`、`v0.0.0` 或未发布伪版本。
- 仓库外临时 consumer 能只依赖已发布 tag 构建并运行最小 Web/gRPC 服务。
- CI、发布顺序和兼容矩阵覆盖全部 module。

### 2.4 运维基线

- 默认具备结构化日志、request/correlation ID、错误边界、liveness、readiness、draining 和优雅关机。
- 安全相关能力默认拒绝不明确状态，管理端点不会无意暴露到公网。
- metrics、tracing 和健康状态可被不同运行栈适配，不产生全局 registry/provider 污染。

## 3. 当前基线与主要差距

当前实现已有可靠方向：canonical Definition、side-effect-free Bundle、typed Inputs/contracts、Plan/Construct 分离、构造回滚、traffic gate、托管任务、Web 路由冻结、中间件排序和较清晰的 module owner。

以下差距阻止其成为可直接交付的服务框架：

| 优先级 | 差距 | 影响 |
| --- | --- | --- |
| P0 | Web route auth policy 未接入统一认证执行器 | 多认证方式会形成错误的 AND 语义，Principal 依赖顺序没有生效 |
| P0 | shutdown 超时后 Stop 实际进入顺序不确定 | 依赖资源可能以错误顺序关闭，race 测试已复现 |
| P0 | 统一配置视图缺少 ENV 层，activation 只看 file/profile/override | 纯 ENV 部署无法启用插件；ENV 仅能覆盖已知 schema 叶子，不能激活插件 |
| P0 | 未知顶层配置未被拒绝，orphan 检查也不覆盖自定义 `ConfigPath` | 配置拼写错误会静默回退默认值 |
| P0 | 子 module 依赖 `go.work` 才能解析 | 仓库外用户无法可靠消费和发布 |
| P0 | gRPC 尚未实现 | 当前只能交付 Web 运行栈 |
| P1 | Prelude 只组合组件，不提供默认启用的产品基线 | “开箱即用”仍要求复制较长配置 |
| P1 | Quickstart 仅覆盖浅层 Web 场景 | 未证明认证授权、基础设施、观测和关闭的真实组合 |
| P1 | 部分 health/telemetry owner 与 Web 绑定 | 加入第二运行栈时可能复制状态或产生反向依赖 |
| P1 | Gin、日志等进程全局状态仍有修改 | 嵌入、多 App 和并行测试隔离不足 |
| P1 | `doctor` 会初始化 logger，且只输出装配顺序 | 违反只读约束，且缺少配置来源与禁用原因，无法在启动前判断部署问题 |
| P1 | 重构后的 runtime 可靠性回归矩阵偏薄 | signal、并发 Execute、任务准入和泄漏风险可能回归 |

## 4. 实施原则

1. **先修语义，再加能力**：认证、关机、配置和发布闭环完成前，不开始微服务扩展。
2. **结果约束优先于当前目录**：允许有意 breaking change，但必须同步更新 caller、测试、文档、示例和架构守卫。
3. **不伪造通用抽象**：Web middleware 和 gRPC interceptor 各自建模；只有真实重复且语义一致时才上移 contract。
4. **安全默认拒绝**：未知配置、缺失认证策略、无 owner 的配置和不完整依赖都必须 fail fast。
5. **一个能力一个 owner**：避免 parallel registry、重复 route/service directory 或多个 Principal 来源。
6. **组合显式、默认产品化**：Bundle 保持无副作用；Starter/Profile 可选择一组明确、安全、可文档化的默认激活策略。
7. **可发布性是功能**：workspace 内通过不代表完成，必须验证脱离 `go.work` 的消费路径。
8. **每阶段独立可验收**：不得通过跳过 race、弱化架构守卫或增加兼容壳来关闭任务。

## 5. 阶段 0：冻结当前重构基线

在继续改 API 前，先让当前大规模 `assembly/runtime/cli` 内聚重构具备可比较基线。

### 工作项

- 先把工作区中 `assembly`/`runtime`/`cli` 迁入 `internal/*` 的重构提交为一个基线 commit。在此之前没有可比较的锚点，下面的 inventory 与失败分类都无法与任何东西对比。
- 保存当前 module、package、公开 API 和插件 inventory。
- 恢复迁移前已有但在重构中删除的契约覆盖，重点是 `internal/pluginmodel`、`internal/assembly`、`internal/runtime`。本阶段只做 parity 恢复；矩阵的目标范围见 [§6.4](#64-恢复-runtime-可靠性矩阵)，补齐工作留在阶段 1。
- 将当前已知失败分类为：真实回归、已决定的设计变化、尚未决定的契约冲突。
- 确保 architecture/integration guards 描述当前意图，而不是暂时迁移形态。

### 验收

- `make fmt`、`make check` 通过。
- 除已登记的 shutdown 契约用例外，关键 runtime race 测试通过。
- 测试覆盖不低于基线 commit 之前：以迁移前的测试清单和逐包覆盖率为基准做 diff，`internal/pluginmodel`、`internal/assembly`、`internal/runtime` 三个包不得低于其迁移前对应包。仅靠 `make check` 变绿不构成通过——重构期间删除测试同样会让它变绿。
- Quickstart doctor 和启动 smoke test 通过。
- 没有为迁移保留旧公开入口或兼容 wrapper。

## 6. 阶段 1：关闭 P0 正确性问题

### 6.1 统一 Web 认证执行链

#### 目标语义

- Route auth policy 只有三种状态：明确 Public、明确 Accepts(schemes)、未声明时的 restrictive default。
- 多 scheme 是按路由声明顺序或冻结后的确定顺序执行的 OR 选择，不是多个 middleware 串联形成的 AND。
- credential 结果至少区分 absent、malformed、rejected、authenticated；malformed 不得降级尝试更宽松路径。
- 一个请求最多发布一次最终 Principal。
- authorization、tenant、audit 等后置能力只能读取 Principal，不直接解析原始凭据。

#### 工作项

- 使用现有协议中立 `security.Manager` 作为认证协调 owner，补齐 Web adapter/dispatcher。该 Manager 目前只有测试调用者，尚未被任何运行栈验证过；接线过程中调整它的 API 属于预期结果，不算返工。
- 让 frozen RouteInfo 的 `AuthPolicy.Schemes()` 成为运行时真实输入。
- 由统一 dispatcher 提供 canonical `authentication-middleware` 顺序节点。
- JWT、API Key、Session 改为贡献 authenticator/credential source，而不是各自强制认证整个请求。
- 让确实依赖 Principal 的 tenant、Casbin、audit 等声明 `RequiresPrincipal`。
- 定义 duplicate scheme、challenge、多 credential、anonymous fallback 和错误映射。

#### 验收

- Public route 在认证插件存在时仍可匿名访问。
- restrictive default 在未配置允许策略时拒绝匿名。
- JWT/API Key/Session 任一被路由接受且成功即可通过，其他缺失 scheme 不会额外返回 401。
- malformed credential 不会退化到另一种更宽松凭据。
- tenant、Casbin、audit 在认证后执行，并读取同一 Principal。
- doctor 能在监听前拒绝未知 scheme、重复 scheme 和缺少 dispatcher 的 Principal consumer。
- doctor 在启动前列出所有落入 restrictive default 的路由。restrictive default 会让现存未声明 `Auth` 的路由从可访问变为拒绝，升级现有应用必须能在不发流量的情况下看到影响面。
- 增加真实组合 E2E 和 race 测试。

### 6.2 明确并修复 shutdown 契约

#### 决策

“严格逆序完成”“硬超时返回”“即使 Hook 永久阻塞也尝试所有后续 Hook”三者不能同时无条件保证。当前实现的具体来源是：`Unwind` 的调用顺序始终是确定逆序，但共享预算耗尽后，`StopBounded` 会立刻放弃等待并返回，导致已经拉起的 Stop goroutine 与后续 Stop 并发执行，函数体的实际进入顺序交由调度器决定。

本方案就此做出裁定，不再留待后续决定：

- 在所有 Hook 遵守 context 的正常路径中，Stop 严格串行逆序完成。
- 所有 Hook 共享同一个 shutdown deadline，不按插件数量叠加超时。
- Hook 忽略 context 属于插件契约违规；runtime 必须有界返回并报告未完成 Identity。
- **deadline 之后不再启动任何新的 Stop。** 剩余 Identity 直接记入 shutdown report 的 `not-attempted`，被放弃的 Hook 记为 `abandoned`。

这样做的代价是超时后放弃 best-effort 清理，换来的是“严格逆序”在所有路径下都成立，且验收条件可判定、测试不依赖 goroutine 调度。若日后确需在超时后继续 best-effort，必须同时改 lifecycle API，让 Hook 能显式确认进入停止阶段，并把顺序断言改为集合断言——不得靠调度偶然性维持现有断言。

#### 工作项

- 增加可观测的 shutdown report：开始顺序、完成顺序、超时、panic、`abandoned` 与 `not-attempted` Identity。
- 正常路径保持严格逆序，不为每个 Hook 无条件并发启动；deadline 触发后停止拉起新的 Stop goroutine。
- 对 panic、重复 Stop、部分 Init、task join 和 listener drain 使用同一 shared budget 语义。
- 清理与公开设计冲突的测试名称或断言，但不得删除行为覆盖。

#### 验收

- 正常停止在高并发和 `-race -count` 下始终严格逆序。
- stuck Stop 不会让进程无限挂起，报告包含准确 Identity。
- deadline 之后没有新的 Stop 被调用，剩余 Identity 在报告中标记为 `not-attempted`；顺序断言不依赖 goroutine 调度结果。
- 构造回滚、正常 shutdown、critical task shutdown 和 signal shutdown 使用一致的资源所有权规则。

### 6.3 统一配置存在性、激活和绑定

#### 目标流水线

1. 收集 composition root 中所有 Definition 和 schema；
2. 合并 defaults、文件、profile、override 与 ENV 到统一配置视图；
3. 基于统一视图决定 activation 和实例展开；
4. 执行 strict bind、validation、Prepare 与 Plan；
5. 在 Construct 前输出脱敏后的来源和决策诊断。

配置依然不能发现未组合代码，但所有配置来源必须具有相同的 activation 能力。

#### 工作项

- 给统一配置视图补上 ENV 层。当前 ENV 根本没有参与配置合并，只在 Bind 时按已知 schema 叶子逐个查询，因此这不是调整判定顺序，而是补一层配置来源。
- 增加不依赖 schema 的 ENV 前缀枚举通道，用于发现实例名。ENV 变量名由 schema 叶子推导，而多实例展开发生在 activation 之后，静态 schema 无法推导出多实例的变量名；没有这条通道，纯 ENV 多实例无法实现。
- 定义 ENV-only 单实例和多实例语法；无法无歧义表达的结构必须给出明确错误。
- 建立顶层 owner：框架保留根、所有 Definition 的 `ConfigPath`、以及显式声明的应用 freeform 根。
- 默认拒绝未被 owner 声明的顶层键；若保留 `app`，只让该节点 freeform。
- 让 orphan configuration 检查覆盖使用自定义 `ConfigPath` 的插件。当前实现只扫 `plugins` 子树且只计入使用约定路径的插件，自定义路径既不参与 orphan 判定，其同级拼写错误也无人认领。
- doctor 展示 activation 的来源，不输出 secret 值。

#### 验收

- 只设置 ENV 即可激活并配置 `WhenConfigured` 插件。
- 文件、profile、override、ENV 的优先级有表驱动测试。
- 多实例可仅由约定 ENV 表达，或在不支持时产生可操作错误，不能静默丢失。
- `wbe` 等未知根键失败并指出路径；合法 `app.*` 按声明策略处理。
- secret 不出现在 doctor、错误和启动报告中。

### 6.4 恢复 runtime 可靠性矩阵

本节是可靠性矩阵的唯一权威清单。阶段 0 只负责恢复迁移前已有的那部分覆盖，本节负责补齐其余，两处不再各写一半。矩阵中与 shutdown 契约同源的条目（Stop panic/error、drain、泄漏）归 [§13](#13-首批可执行任务切分) 的 shutdown owner，其余归 reliability owner。

至少覆盖：

- 并发/重复 Execute；
- process signal 与程序化 shutdown 竞争；
- managed task admission、critical task failure、cancel/join；
- Init/Start/Stop panic 和 error；
- traffic gate 开闭时序；
- autoload 与 composition freeze；
- retained Context/BuildContext 失效；
- goroutine、listener 和临时资源泄漏；
- Web server drain 与 timeout。

### 阶段门禁

```sh
make fmt
make check
make test
make test-race
go test -race -count=20 -run 'Shutdown|Unwind|Order' ./internal/runtime/... ./internal/assembly/... ./transport/web/...
go run ./examples/quickstart doctor --config examples/quickstart/application.yml
go run ./examples/quickstart --config examples/quickstart/application.yml
```

顺序敏感的用例必须带 `-count`。`make check` 与 `make test` 使用 `-count=1`，而顺序断言的失败是间歇性的，单次运行可能自己变绿——只跑这两个命令等于放弃了对该类回归的检测，也会架空[实施原则](#4-实施原则)第 8 条。

`./transport/web/...` 是后续加入门禁包列表的：Web 优雅停机的排空与超时用例属于门禁要防的同一类间歇性回归，而该列表是在这些用例存在之前定下的。凡是主题为停机顺序或排空的用例，命名必须能被 `Shutdown|Unwind|Order` 选中，否则它对保护自己的门禁不可见。

阶段 1 未全部通过，不进入 gRPC 或微服务实现。

## 7. 阶段 2：交付真正的 Web Starter

### 7.1 Bundle、Prelude 与 Starter 分工

- `Bundle`：显式提供一项能力，不产生副作用。
- `Prelude`：组合 canonical Definition，不篡改成员 activation，保持现有设计语义。
- `Starter/Profile`：产品层入口，选择一组有文档、安全且经过集成验证的默认启用策略。

具体 API 名称在实现前根据 caller 选择，但不能改变 Prelude 的无副作用和不强制激活约束。

### 7.2 Web 生产基线

默认基线应覆盖：

- recovery 和稳定 Problem Details；
- request ID/correlation；
- 结构化 access log；
- security headers；
- request/header/body limit 与 server timeout；
- liveness、readiness、draining；
- deterministic graceful shutdown。

CORS、认证策略、业务 envelope、Swagger、pprof、具体 exporter 和外部基础设施仍由应用明确选择。pprof 与管理能力默认关闭或只允许安全的 management listener。

### 7.3 运行形态

- 明确是否支持原生 TLS；若只支持代理终止 TLS，写成部署约束。
- 评估独立 management listener，使 health/metrics/pprof 与业务端口隔离。
- 避免修改 Gin default writer/mode 等可替代的进程全局状态；无法避免的状态要明确单进程单 App 约束。
- `doctor` 等只读命令不得提前初始化 logger 或其他进程级资源。

### 7.4 示例与验收

新增可运行的 production-style Web example，至少组合：

- Public 与 authenticated route；
- 两种可替代认证方式和授权；
- 一个协议无关 integration；
- metrics/tracing hook；
- readiness/drain/shutdown。

验收：

- 最小 main 保持只负责 Bundle 选择和 `Execute`。
- 通过 YAML 和纯 ENV 两种方式启动。
- 提供 Dockerfile、容器 smoke test 和部署说明。
- SIGTERM 时先关闭 readiness/traffic，再 drain 请求，最后逆序释放依赖。
- 管理端点不会默认暴露到非预期接口。

## 8. 阶段 3：完成 module 发布闭环

### 8.1 版本策略

首批发布采用同一 release train，降低首个稳定版本前的组合矩阵；每个 module 仍按自身 module path 使用合法 tag。

**首批 tag 走 `v0.x`。** Core 中的协议无关契约（security、health、telemetry）在阶段 4 结束前不承诺兼容性。这一条是必需的：本方案第 1 节和 [§9.2](#92-先由真实实现验证的扩展面) 都规定“只有被两个真实运行栈共同证明稳定的能力才抽到协议无关 owner”，而本阶段发布时只有 Web 一个运行栈。如果首批就发 `v1`，阶段 3 的“可消费产品”承诺与阶段 4 的“实现后才抽象”原则必然对撞，而 [§8.2](#82-工作项) 又禁止覆盖既有 tag。阶段 4 完成、两个运行栈共同验证过共享契约之后，才评估 `v1`。

发布顺序遵循依赖拓扑：

```text
Core
→ transport/web 与协议无关基础 integration
→ Web heavy integration 与组合 integration
→ examples/consumer verification
```

如后续确有独立节奏需求，再基于兼容数据拆分版本，不提前制造多版本矩阵。

### 8.2 工作项

- 为每个 child module 写入真实 sibling `require` 和完整 `go.sum`。
- 保持 publishable manifests 无 local `replace`、placeholder 或 synthetic pseudo-version。
- 增加按拓扑生成/校验 release manifest 的工具。
- CI 对每个 module 执行 `GOWORK=off`、`-mod=readonly` 测试。
- 创建仓库外临时 consumer，只使用已发布 tag；不得通过仓库 `go.work` 或源码路径补救。
- 增加 module compatibility/BOM 文档和升级说明。

### 验收

- 所有 module 独立通过：
  ```sh
  GOWORK=off go test -mod=readonly ./...
  ```
- 外部 consumer 可构建、启动、访问 Web route 并完成优雅关机。
- 从空 module cache 复现成功。
- 架构守卫不再允许“首个 tag 前缺少 sibling requirement”的例外。
- release job 不直接覆盖或 force push 既有 tag。

## 9. 阶段 4：实现真实 gRPC 运行栈

只有阶段 1～3 的契约稳定后，才创建 `transport/grpc`。不先创建空目录、manifest 或占位接口。

### 9.1 所有权

`transport/grpc` 独立拥有：

- gRPC server 与 listener；
- service contributor/registrar；
- unary interceptor 顺序；
- stream interceptor 顺序；
- status/error mapping；
- reflection adapter；
- standard gRPC health adapter；
- graceful stop 与强制停止降级。

它可以依赖 Core/plugin 和被证明协议无关的 security/health/telemetry contract，但不能依赖 `transport/web`。Web 也不能反向依赖 gRPC。

### 9.2 先由真实实现验证的扩展面

- Service 必须在 listener 开放前完成收集、冲突检测和冻结。
- Interceptor order 与构造 dependency graph 分离，但都必须确定、可诊断、可拒绝环。
- Unary 与 stream 分别建模，不假定两者具有相同能力。
- 认证复用统一 security 语义，但凭据提取和 gRPC status/challenge 由 gRPC adapter 负责。
- traffic gate、managed tasks 和 app shutdown 复用 Core，不按 key 在 Core 中识别 gRPC。

只有实现 Web/gRPC adapter 后确认存在相同状态 owner，才考虑抽出共享 health registry、service identity 或 telemetry provider；暴露方式继续由各 transport 所有。

### 9.3 Starter 与示例

提供独立 gRPC Starter 和 Quickstart，覆盖：

- protobuf 生成约定及可复现工具版本；
- 一个 unary 与一个 stream service；
- auth、recovery、logging、metrics/tracing；
- standard health 和可选 reflection；
- readiness、graceful stop 和超时降级。

再增加 Web + gRPC 同进程集成测试，证明：

- 两个 transport 可独立选择，也可显式共存；
- 任一 transport 启动失败不会开放另一个 transport 的流量；
- shutdown 先关闭全局 traffic/readiness，再分别 drain，最后逆序释放共享 integration；
- 不存在 route/service registry 或 Principal 的平行事实源。

### 阶段验收

- gRPC module 有真实 consumer、测试和文档后才进入 `go.work` 和发布拓扑。
- unary/stream interceptor 顺序、重复 service、未知 auth scheme 和 listener failure 均在测试中覆盖。
- gRPC Quickstart 可用纯 ENV 启动。
- 外部 consumer 使用已发布 tag 通过构建和 smoke test。
- `make check`、`make test-race` 覆盖新 module。

## 10. 阶段 5：微服务 Profile

微服务不是新容器或第二套 Plugin 模型，而是对已稳定能力的部署 Profile。只从真实部署需求增加组件。

### 优先能力

1. service identity、instance identity 和版本元数据；
2. 独立 management plane；
3. readiness、draining 与终止宽限期协同；
4. outbound deadline、trace propagation 和有界重试；
5. transport-specific client metrics 与连接生命周期；
6. secret/config provider；
7. 多副本 migration、cron、outbox 等任务的 leader/lease 策略；
8. 镜像、Kubernetes/Helm 模板、SBOM 和漏洞扫描；
9. release BOM 与兼容矩阵。

### 后置能力

以下能力只有存在实际 provider/registry 和至少一个 consumer 时才设计 SPI：

- service discovery/registration；
- client-side load balancing；
- circuit breaker policy；
- dynamic config watch；
- service mesh adapter；
- control plane。

禁止为了“未来微服务”把这些接口提前放入 Core。

### 微服务验收场景

至少用一个多副本示例证明：

- rolling update 中 readiness/draining 正确；
- 请求和 trace 可跨 Web/gRPC 边界关联；
- retry 只对满足幂等条件的调用生效，并受总 deadline 限制；
- migration/cron/outbox 不会在所有副本无协调竞争执行；
- 一个实例失联后 lease 能安全接管，旧 owner 不能破坏新 owner 状态；
- 配置和 secret 不进入日志、doctor 或错误响应。

## 11. CI 与质量门禁

最终 CI 分为四层：

1. **package 层**：单元、配置、错误路径和并发测试；
2. **workspace 层**：`make fmt`、`make check`、`make test`、`make test-race`；
3. **进程层**：doctor、启动、流量、signal、drain、泄漏 smoke；
4. **发布层**：`GOWORK=off -mod=readonly`、空缓存、外部 consumer、发布拓扑。

安全和生命周期相关改动必须带组合测试，不能仅依赖 package mock。任何阶段都不得通过以下方式变绿：

- skip 或删除失败的 architecture/race test；
- 放宽未知配置；
- 增加隐式 global registry；
- 在 child module 添加 local replace；
- 用兼容 wrapper 保留已决定删除的 API；
- 只验证根 module 的 `go test ./...`。

## 12. 推荐执行顺序

```text
0. 冻结当前重构基线
   ↓
1. 认证闭环 + shutdown 契约 + ENV/严格配置 + runtime race
   ↓
2. Web Starter + production-style example
   ↓
3. 真实 module tag + detached consumer
   ↓
4. transport/grpc + gRPC Starter + dual-transport E2E
   ↓
5. 基于真实部署的 microservice Profile
```

阶段 1 是最高优先级。认证链和 shutdown 是正确性问题；ENV activation 和严格配置是部署可靠性问题。它们未完成前，增加更多插件只会扩大不稳定表面积。阶段 3 是“可用仓库”变成“可消费产品”的边界。gRPC 和微服务能力必须建立在这些闭环之上。

## 13. 首批可执行任务切分

为降低并行修改冲突，第一轮按互不重叠的 owner 切分：

1. **Security/Web auth**：冻结 auth policy 语义，接入 dispatcher，迁移 authenticator，增加组合 E2E。
2. **Runtime shutdown**：契约裁定已由 [§6.2](#62-明确并修复-shutdown-契约) 给出，直接改 `StopBounded`/`Unwind`/Execute 与 shutdown report。独占 shutdown 路径，并连带拥有 [§6.4](#64-恢复-runtime-可靠性矩阵) 中与之同源的用例：Stop panic/error、Web server drain 与 timeout、goroutine/listener 泄漏。
3. **Configuration**：实现统一 source/activation 视图、ENV 层与前缀枚举、顶层 owner 和 ENV-only tests。
4. **Reliability guards**：`internal/pluginmodel` 覆盖，以及 [§6.4](#64-恢复-runtime-可靠性矩阵) 中不属于 shutdown 路径的部分——并发/重复 Execute、signal 竞争、任务准入与 cancel/join、traffic gate 时序、autoload 与 composition freeze、retained Context 失效，外加 process smoke。不改 shutdown 实现。
5. **Release audit**：只生成 module DAG、缺失 requirements 和首批 tag 计划；在阶段 1 稳定前不发布。

每项变更只修改其 owner 与必要 caller；完成后统一运行 workspace-wide validation。认证、shutdown 和配置三个 P0 不应在同一个大提交中混改。
