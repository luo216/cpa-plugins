# cpa-plugins

CLIProxyAPI（[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)，简称 cpa）的自用插件集合。

## 插件

| 插件 | 功能 |
|---|---|
| [workbuddy-web-search](workbuddy-web-search/) | 把 Claude Code 的 `web_search` 请求路由到 2api 网关（workbuddy2api 等）的 Anthropic Messages 兼容搜索端点 |

## 背景

cpa 的 openai-compatibility（chat-completions 协议）上游不执行 Anthropic 的
`web_search` server tool——翻译层把它当普通 function 转发，模型回一个空
`tool_use` 就结束，Claude Code 的 WebSearch 因此失效（原生 antigravity /
codex / xai 路径不受影响）。

本仓库的插件在 cpa 侧补上这层：识别 Claude Code 的搜索子请求，转调支持
Anthropic Messages 搜索协议壳的网关，把结果按 Claude SSE 事件流吐回。
