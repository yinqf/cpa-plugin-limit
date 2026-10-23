# cpa-plugin-limit

CLIProxyAPI 上游账户限流插件。默认 **同一账户滚动 60 秒最多放行 5 次，并发 1**。
不同下游 API Key、会话和模型，只要选中同一账户，都共享计数。

沿用 `cpa-plugin-xty` 的 C ABI、JSON RPC、可视化配置和监控接口架构。
不接管 Scheduler，不改写请求体、请求头或会话绑定，也不修改 CPA 源码。

## 配置

在 CPA 界面打开 **插件管理 → cpa-plugin-limit → 配置**，即可编辑全部限流参数，
不必手动修改 YAML。每分钟请求数、最大并发为数字输入框；限制范围为 JSON 数组框；
账户合并、单账户阈值为 JSON 对象框；账户显示方式为下拉选择。字段均提供默认值和中文填写说明，清空后恢复默认。
点击保存后由 CPA 现有的插件配置机制应用，插件保留已有计数。
监控页面的 **配置填写示例** 提供可直接复制的 JSON，三个示例可以配合使用。
表单说明使用纯文本指引，避免宿主 HTML 转义使示例中的双引号显示成实体编码。

下面的 YAML 仅供查看完整配置结构，日常操作使用界面即可。

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-plugin-limit:
      enabled: true
      requests_per_minute: 5
      max_concurrency: 1
      auth_indexes: []
      account_groups: {}
      account_limits: {}
      account_display: email

routing:
  session-affinity: true
  session-affinity-ttl: "1h"
```

- `requests_per_minute`：每账户滚动 60 秒放行次数；默认 5，0 关闭限频。
- `max_concurrency`：每账户同时占用的请求名额；默认 1，0 关闭并发限制。
- `auth_indexes`：要限制的 `selected_auth_index` 或 `selected_auth_id` 数组。
  **留空限制所有供应商的上游凭据**。只想限制 Codex 时，填写 Codex 凭据索引。
- `account_groups`：同一真实账户有多份凭据时，将这些索引映射到同一分组名。
- `account_limits`：按分组名或未分组凭据索引覆盖阈值，未填字段继承全局配置。
- `account_display`：`email` 默认显示完整邮箱；`masked_email` 显示脱敏邮箱；
  `fingerprint` 仅显示账户指纹。邮箱模式不附带指纹，长邮箱自动换行显示完整内容；
  无邮箱时显示“未提供邮箱”，查询失败时显示“邮箱读取失败”，不影响账户身份或限流计数。

数值须为 0～10000 的整数。配置字段会出现在 CPA 的插件配置编辑器中。
对象编辑框填写 JSON，例如：

```json
{
  "codex-shared": {"requests_per_minute": 5, "max_concurrency": 2}
}
```

同账户重复导入的配置示例，放在插件配置下：

```yaml
auth_indexes: [codex-index-a, codex-index-b]
account_groups:
  codex-index-a: codex-shared
  codex-index-b: codex-shared
account_limits:
  codex-shared:
    requests_per_minute: 5
    max_concurrency: 2
```

修改阈值保留现有计数；修改账户分组前需停止新流量、等在途请求完成且最近
60 秒记录过期，否则拒绝应用新分组。插件重启或替换二进制会清空内存状态，
需要严格计数时应先排空请求。

## 行为

CPA 选好账户后，插件原子检查并发和滑动窗口。满额立即返回本地 429 与
`Retry-After`，不在动态库中阻塞排队，也不会主动切换账户或报告上游失败。
继续使用 CPA 原有的会话亲和；若计费插件接管受限凭据路由，需开启计费插件
自己的会话亲和配置。

每次宿主实际调用 after-auth 钩子都会消耗次数，完成、失败不退还次数。
同一次请求内，同一账户的重试复用并发名额；不同账户的故障转移会保守占用
所有尝试过的账户名额，直到整个请求结束。成功、失败、拒绝或取消均通过
`request.complete` 幂等释放，不因经过 60 秒就提前释放长流。

## 监控

菜单中的 **账户限流监控** 打开中文监控页：

```text
/v0/resource/plugins/cpa-plugin-limit/ui
/v0/resource/plugins/cpa-plugin-limit/status
```

页面沿用 CPA 的字体、卡片、按钮和表格样式。同源嵌入管理界面时，自动同步
CPA 的颜色变量及羊毛纸、纯白、暗色主题；兼容管理面板注入的主题变量。
独立打开或跨域嵌入时，可在外观选择框中切换主题，自动模式优先使用同源
CPA 已保存的主题，否则跟随系统。无需修改 CPA 或管理面板代码。

页面包含并发名额占用、最近 60 秒放行、累计放行、累计拒绝四项汇总；
账户明细展示占用进度、限频/并发状态、限频恢复时间以及拒绝次数。
同时显示当前生效的全局阈值、限制范围和分组/覆盖数量，方便核对界面保存是否生效。
默认每 5 秒自动刷新，可在页面切换为关闭、10 秒或 30 秒，也可立即手动刷新。
页面隐藏时暂停自动刷新；刷新失败保留上次数据并显示错误提示。

账户邮箱通过 CPA 的 `host.auth.list` 回调自动读取，仅使用明确的邮箱字段，
不从文件名、账户标签或 API Key 猜测。每次监控刷新最多使用 30 秒缓存；
不在请求准入路径查询邮箱。重复凭据及已观察到的分组成员去重显示邮箱，
显示邮箱相同的独立凭据仍各自计数。宿主不支持查询或查询失败时显示邮箱读取失败。

这些资源接口不经过 CPA 管理鉴权。默认返回完整邮箱、账户指纹、计数和阈值。
**完整邮箱会通过公开状态接口返回，请在网关限制资源路径访问。**
指纹仅保留在 API 中用于关联，邮箱模式的页面不显示指纹。
不返回原始凭据索引、分组名、API Key、会话、
请求 ID 或请求内容。没有新增已弃用的 `/v0/management` 接口。

并发名额等待 CPA 的异步完成通知释放，可能有短暂延迟。空闲账户窗口过期后
会删除，对应账户的统计随之清零；全局放行/拒绝计数保留到插件重启。
最多跟踪 4096 个账户、16384 个在途请求；达到容量或缺少必要标识时返回 503。

## 必须了解的覆盖边界

这是对 **宿主交给插件的请求准入** 做保护，不能代替宿主内部的每次发送控制：

- 部分 executor 内部重发，包括某些刷新认证后的重试，不再经过 after-auth 钩子，
  不会新增限频计数。
- Codex 双向 WebSocket 的后续 `response.create`、`response.append`、steer
  可以绕过请求钩子。插件可能在整个连接生命周期持有一个请求名额，不能保证
  逐轮次 5 RPM，也不是空闲 WebSocket 连接池的连接数限制器。
- 不打断已经执行的请求，不增加网络、流式生成或请求超时。
- 多实例之间不共享计数；直接使用上游账户的流量也不包含在内。
- 禁用、卸载或宿主熔断插件后无法继续保护。严格覆盖所有上游发送、双向轮次
  或多个 CPA 实例，需要宿主钩子或共享协调能力。

## 构建与验证

需要支持插件的 CPA，宿主 RPC schema 至少为 2 并支持请求终止与完成通知。
依赖版本与参考插件一致，当前 v8 宿主仍使用兼容的 native ABI 1 / schema 2。
Go 1.26+、C 编译器；macOS：

```sh
gofmt -w .
go vet ./...
go test -race ./...
mkdir -p build
go build -buildmode=c-shared -o build/cpa-plugin-limit.dylib .
python3 scripts/smoke_native.py build/cpa-plugin-limit.dylib
```

Linux 使用 `.so`，Windows 使用 `.dll`。将动态库复制到 CPA 插件目录后启用，
保留 `cpa-plugin-limit` 文件名。测试使用可控时钟和并发竞争，原生检查脚本
通过实际 C ABI 调用动态库，不请求真实账户、不产生费用。

已提供 CI、仅 Linux amd64 的发布工作流和插件商店 `registry.json`。
其他平台可手动构建。推送 `vX.Y.Z` 标签才会触发发布；本地构建不会
提交、推送、发布或部署。
