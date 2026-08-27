# 配置约定

XBC 默认按顺序查找 `application.yml` 和 `configs/application.yml`，也可以通过
`--config` 显式指定文件。可运行配置以
[`examples/quickstart/application.yml`](../examples/quickstart/application.yml) 为准。

顶级命名空间的所有权固定如下：

- `xbc`：核心运行时设置，例如整轮关闭预算和是否自动迁移。
- `log`：日志模块设置。
- `plugins.<key>`：插件配置；`key` 必须等于插件的 `Definition.Key`。
- `app`：业务自由配置，框架不解释其结构。

HTTP 地址不属于核心，因此没有全局 `server` 节；启用 web 模块时应写在
`plugins.web` 下。未知的插件配置节会导致启动失败，避免拼写错误被静默忽略。

```yaml
xbc:
  shutdown_timeout: 30s
  auto_migrate: false

log:
  level: info
  console:
    enabled: true
    format: console

plugins:
  web:
    addr: ":8080"
    base_path: "/api/v1"
    read_timeout: 10s
    write_timeout: 30s

app:
  feature_x: true
```

配置值不会展开 `${VAR}`。环境变量名称由完整绑定路径生成，点号转换为下划线，
例如 `XBC_XBC_SHUTDOWN_TIMEOUT`、`XBC_LOG_LEVEL` 和
`XBC_PLUGINS_WEB_ADDR`。
