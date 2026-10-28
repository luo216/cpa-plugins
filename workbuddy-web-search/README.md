# workbuddy-web-search

cpa 插件：把 Claude Code 内置的 `web_search`（server tool）请求路由到
2api 网关（如 [workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)）
的 Anthropic Messages 兼容搜索端点，解决 chat-completions 协议上游
不执行 server tool 导致 WebSearch 失效的问题。

## 工作方式

1. **路由判定**（ModelRouter 能力）：入站为 Claude/Anthropic 协议、
   `tools` 里仅有 `web_search_20250305` / `web_search_20260209` 类型工具
   （即 Claude Code 的搜索子请求）时接管；混合普通工具的请求不动，照常走模型对话。
2. **执行**（Executor 能力）：从 user 消息里剥出
   `Perform a web search for the query: <query>` 的搜索词，POST 到配置的
   网关端点（Messages 形态请求），拿到单条 Messages JSON 响应后转成
   `message_start` / `content_block_*` / `message_stop` 的 SSE 事件流吐回客户端。
   发往网关的请求带 Claude Code UA，便于网关按客户端指纹做兼容处理。

## 编译

依赖本地 cpa 源码（`go.mod` 里的 `replace` 指向它，注意核对路径与版本）：

```bash
git clone --depth 1 --branch v8.0.13 --filter=blob:none --sparse \
  https://github.com/router-for-me/CLIProxyAPI.git
cd CLIProxyAPI && git sparse-checkout set sdk/pluginabi sdk/pluginapi

cd workbuddy-web-search   # 本目录
# go.mod 的 replace 改成你机器上的 CLIProxyAPI 路径
GOPROXY=https://goproxy.cn,direct go build -buildmode=c-shared -o workbuddy-web-search.so .
```

## 安装（cpa 配置）

```yaml
plugins:
  enabled: true
  dir: /path/to/plugin/dir      # .so 所在目录
  configs:
    workbuddy-web-search:
      enabled: true
      priority: 20
      endpoint: http://127.0.0.1:7863/v1/search/messages  # 2api 网关搜索端点
      api_key: your_gateway_key
      timeout_seconds: 30
```

改配置或换 .so 后需 `systemctl --user restart cpa`（或重启 cpa 进程）：
cpa 的热重载只监听 config.yaml 内容变更，不监听 .so 文件变更。

换其他 2api 后端：只要有 Messages 兼容搜索端点，改 `endpoint` / `api_key`
即可，无需重新编译。

## 配置项

| 字段 | 说明 |
|---|---|
| `enabled` | 关闭后所有请求回落默认路由 |
| `endpoint` | 网关搜索端点 URL（Messages 协议壳） |
| `api_key` | 网关 Bearer token |
| `timeout_seconds` | 单次搜索超时，默认 30 |

## 已知坑

- cpa 宿主校验插件元数据要求 `Name` / `Version` / `Author` /
  `GitHubRepository` 全部非空，缺 `GitHubRepository` 会报
  "invalid metadata or no capabilities"（且仍打印 "plugin loaded"，别被骗）。
- 插件按 ABI 编译，升级 cpa 大版本后需对新版源码重编译。
