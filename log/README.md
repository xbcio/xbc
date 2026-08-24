# xbc/log

结构化日志门面 + zap 默认实现。零框架依赖，可脱离 xbc 单独用。

```bash
go get github.com/xbcio/xbc/log
```

## 快速开始

```go
package main

import (
	"context"

	"github.com/xbcio/xbc/log"
)

func main() {
	cfg := log.DefaultConfig()
	cfg.Level = "debug"
	cfg.File.Enabled = true
	cfg.File.Path = "logs/app.jsonl" // extension determines the format: .jsonl -> json
	if err := log.Init(cfg); err != nil {
		panic(err)
	}
	defer log.Close()

	ctx := context.Background()
	log.TInfo(ctx, "服务启动", "port", 8080, "env", "prod")
}
```

## 退出前刷盘

```go
defer log.Close()   // flush + close file handles, use this on process exit
log.Sync()          // flush only, don't close; use when a long-lived process wants to flush immediately
```

`Close()` 内部先 `Sync()` 再关文件，两者都对「往终端/管道 fsync 返回 EINVAL」这个 zap 的著名毛刺做了吞掉处理 —— 不会让 `defer` 在每次正常退出时报一个假错。

## 两条等价路径

```go
log.TInfo(ctx, "下单", "order_id", 1001)     // sugar
log.Ctx(ctx).Info("下单", "order_id", 1001)  // facade
```

产出完全相同，caller 都指向你的调用行。链式派生时用门面：

```go
l := log.Ctx(ctx).With("module", "payment")
l.Info("发起支付", "amount", 99.5)
l.Warn("网关超时", "retry", 1)
```

`printf` 语义走 `f` 版本，但**能拆成 KV 的都别用** —— 拼进 msg 的字段检索不到：

```go
log.TInfof(ctx, "启动耗时 %.2fs", 1.35)   // fine: a one-off startup banner
log.TInfof(ctx, "订单 %d 金额 %.2f", id, amt)  // don't do this, order_id can't be searched for
log.TInfo(ctx, "下单", "order_id", id, "amount", amt)  // do it this way
```

## 链路追踪

```go
// Entry: decode the trace from the upstream header, start a new one if absent
ctx := log.Extract(r.Context(), propagation.HeaderCarrier(r.Header), "GET /orders/:id")

// Child span
dbCtx, done := log.Span(ctx, "db.query")
defer done()   // automatically logs one debug line with elapsed_ms
log.TInfo(dbCtx, "查询订单", "order_id", 1001)

// Outbound: pass the trace along to the downstream service
log.Inject(ctx, propagation.HeaderCarrier(req.Header))
```

`trace_id` 与 `request_id` 是同一个 128 bit 值的两种编码：前者是 W3C 的 32 位 hex，后者是 ULID 的 26 位 Crockford Base32。ULID 前 6 字节是毫秒时间戳，所以 `request_id` 天然按时间有序、肉眼可比大小。

跟 OTel SDK 混用时，`Trace` 内嵌了 `trace.SpanContext`，直接传即可：

```go
tr := log.TraceFrom(ctx)
otelCtx := trace.ContextWithSpanContext(ctx, tr.SpanContext)
```

## 配置

```yaml
log:
  level: info           # debug | info | warn | error
  caller: true          # whether to record the call site
  stacktrace: error     # from which level a stacktrace is attached

  console:
    enabled: true
    format: console     # console | json
    color: auto         # auto | always | never

  file:
    enabled: false
    path: logs/app.log  # extension determines the format: .log -> console, .jsonl/.json/.ndjson -> json
    format: ""          # empty = infer from extension; set to override the inference
    rotate: daily       # daily | size
    max_size: 100       # MB, threshold for the size strategy, also applies under daily
    max_age: 30         # days
    max_backups: 30     # count
    compress: true
    error_path: ""      # non-empty opens an extra sink that only receives error and above, format inferred from its own extension

  sampling:
    initial: 100        # log every one of the first N per second
    thereafter: 100     # then log 1 out of every N; set to 0 to disable sampling
  mask_fields: []       # additional fields to mask
```

格式是 sink 级而不是全局的 —— "终端 console + 文件 json" 是最常见的组合，全局单一 format 表达不了。

`color: auto` 的判定顺序：`NO_COLOR` 未设置 → `TERM` 不是 `dumb` → 输出是 TTY。

`rotate: daily` 是本包在 lumberjack 之上补的（它本身只按大小滚）。跨天滚出的归档文件名带的是**触发时刻**的时间戳而内容是**前一天**的 —— 这是 lumberjack 的既定命名规则。

## 部署前提：日志目录权限

框架只在**创建**日志目录时用 `0o750`；对于**已经存在**的目录，框架**不会**主动去收紧权限——这是为了不踩到运维刻意设置的权限（比如某些部署环境要求日志目录对特定用户组可写）。

所以：如果日志目录是由运维预先创建好的，必须自行保证该目录权限不宽于 `0750`，否则框架内置的目录权限保护形同虚设。

## 构建环境约束：Go 版本与字段名匹配

脱敏功能里字段名匹配用到的结构体 tag 解析规则，是在 Go 1.27 引入的 `encoding/json/v2`（jsonv2）行为下测出来的；而本仓库 `go.mod` 声明的 `go` 指令是 `1.25.0`，实际构建走的是 `encoding/json` v1 的语义。

也就是说：如果你用 Go 1.27 及以上版本、且该版本默认切换到 jsonv2 语义去构建本包，tag 解析在个别边界场景下的行为可能与当前 v1 语义下测出的结果有细微差异。日常在 `go 1.25.0` 下构建不受影响；升级构建所用的 Go 版本前，建议重新跑一遍 `log/mask_test.go` 确认边界行为未变化。

## 脱敏

敏感字段在 `zapcore.Core` 层被拦截替换成 `***`，调用方绕不过去，`log.Zap()` 逃生舱口也一样。

内置黑名单**始终生效、不可通过配置移除**：`password` / `token` / `access_token` / `refresh_token` / `secret` / `private_key` / `ak` / `sk` / `db_url` / `dsn` / `id_card` / `bank_card` / `phone` / `authorization` / `cookie` 等及其常见变体。字段名匹配前会归一化（转小写、去掉 `_` `-` `.`），所以 `accessToken`、`ACCESS_TOKEN`、`access-token` 命中同一条规则。

匹配是精确的不是前缀的 —— `phone` 命中，`phone_masked` 和 `token_count` 不命中。

`mask_fields` 只能追加：

```yaml
log:
  mask_fields: [salary, home_address]
```

## 换后端

```go
log.SetLogger(myLogger)   // just implement the log.Logger interface
```

**换后端等于换掉内置脱敏** —— 那是实现在本包里的，第三方实现不会自动带上。`SetLogger` 会往 stderr 打一条警示。

可选能力接口按需实现：

- `ZapProvider` —— 让 `log.Zap(ctx)` 能返回底层 `*zap.Logger`
- `CallerSkipper` —— 让 `TInfo` 等语法糖的 caller 指向调用点而不是本包内部

两个都不实现也能工作，只是失去对应能力。

## 从 gfa 迁移

gfa 的 `TInfo(ctx, args...)` 是 **Sprintln 语义**，本包是 **KV 语义**。签名兼容但含义不同，编译不会报错：

```go
log.TInfo(ctx, "支付成功", orderID, amount)
```

- `orderID` 是 `int` → 产出两个 `!BADKEY` 字段，日志里一眼看得出来
- `orderID` 是 `string` → **静默变成 `<订单号>=<金额>` 这样一个字段**，key 是订单号本身

第二种是真陷阱，运行时检测不出来（它是完全合法的 KV 用法）。迁移时逐个改写，别批量替换：

```go
log.TInfo(ctx, "支付成功", "order_id", orderID, "amount", amount)  // KV
log.TInfof(ctx, "支付成功 %s %.2f", orderID, amount)               // or go through printf explicitly
```

`grep -rn 'T\(Info\|Warn\|Error\|Debug\)(' ` 过一遍，确认每个调用的第三个参数往后都是 `"key", value` 交替。

## console 输出格式

```
10:23:45.123 INFO  01926f7e         order/service.go:42  校验通过  amount=99 order_id=1001
10:23:45.201 ERROR 01926f7e        payment/client.go:33  支付失败  err="connection refused"
```

| 段 | 宽度 | 说明 |
|---|---|---|
| 时间 | 12 | `15:04:05.000`，不打日期（日期在文件名里） |
| level | 5 | 左对齐，按级着色 |
| trace | 8 | `trace_id` 前 8 位，无链路时留空 |
| caller | 24 | 右对齐，超长从左侧截断加 `…` |
| msg | 变长 | 后跟两个空格 |
| KV | 变长 | `key=value`，含空格的值加引号 |

字段按 key 字母序输出：既保证输出确定性，也让同名字段每行落在相似位置。

console 下 `span_id` 与 `request_id` 不进 KV 区（`trace_id` 已占固定列），json sink 全留 —— 一个给人扫读，一个给机器检索。
