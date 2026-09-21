# Codex 历史过滤

Sub2API 原生实现 `ccswitch-codex-filter` v2 的请求过滤，并在原生过滤协议 v3 中补全网页搜索、代理消息及工具搜索历史。客户端直接连接 Sub2API 的 Responses API，服务端处理历史密文和记录 ID，无需运行 CC Switch、Node.js 或本地过滤进程。

## 启用

管理员进入「系统设置 → 网关 → Codex 历史过滤」开启。开关立即保存，默认关闭；重启后保留开关，计数从零开始。客户端应使用 Sub2API 地址和对应分组的 API Key，通过 HTTP/SSE 发送完整消息、工具调用及工具结果历史。

- 查询：`GET /api/v1/admin/settings/codex-history-filter`。
- 切换：`PUT /api/v1/admin/settings/codex-history-filter`，JSON 为 `{"enabled":true}` 或 `{"enabled":false}`。
- 两个接口均受现有管理员认证保护。关闭后恢复网关原有处理流程。
- 设置缓存在进程内最多 30 秒；通过本进程管理接口修改立即生效。设置读取失败时请求返回 503，不会默默跳过过滤。

## 请求语义

覆盖 `POST /responses`、`POST /v1/responses`、`POST /backend-api/codex/responses` 及尾斜杠形式。

| 内容 | 处理 |
| --- | --- |
| `input` 中 `type: reasoning` 的整项 | 删除 |
| `include` 中 `reasoning.encrypted_content` | 删除 |
| `store` | 强制 `false` |
| 普通消息、函数/自定义工具调用及结果的顶层 `id` | 删除 |
| `web_search_call`、`agent_message`、`tool_search_call`、`tool_search_output` 的顶层 `id` | 删除，保留搜索动作、来源、代理身份与工具配对 |
| `agent_message.content` 中被标记为 `encrypted_content` 的明文 | 恢复为 `input_text`，保留完整文本 |
| 代理消息中的真实加密任务/回复正文 | 原样保留，不当作历史 reasoning 丢弃 |
| 无 `type`、含字符串 `role` 及 `content` 的简写消息顶层 `id` | 删除 |
| 本轮 `reasoning.effort`、`call_id`、`phase`、工具内容、图片和嵌套资源 ID | 保留 |
| 其他资源型记录的 ID | 保留 |

原生 `web_search_call` 的历史 ID 同样绑定创建它的资源；只检查 `ws_` 前缀不足以跨 Azure 资源重放，必须移除该顶层 ID。搜索项本身及其查询、来源和后续回答引用均保留。

代理消息可能同时包含明文信封和加密任务/回复正文。对不符合加密令牌特征的 `encrypted_content` 文本块修正类型；`gAAAA` 开头或其他长编码令牌保留。该处理限定在代理消息的内容块，不递归改写函数结果、工具参数或用户 JSON。不能解密的真实代理正文仍需要原上游支持，不会声称恢复其内容。

仅删除可携带完整内容重放的记录 ID，不递归删除工具参数或资源引用。使用原始 JSON 值保留工具内容和大整数。API Key 鉴权及分组模型白名单仍先执行；白名单检查原始 JSON 的所有模型字段，避免过滤重写掩盖重复模型名。

支持 identity、gzip、deflate、br、zstd，原始及解压后的请求体均受 64 MiB 上限约束，原有网关配置可施加更小限制。解压与过滤后更新长度及编码头。账号特有的请求转换结束后再次执行同一策略，防止转换器重新添加加密推理选项。过滤层不新增模型重试、不处理响应正文、不缓冲 SSE，不改变网关原有的响应与取消处理。

## 明确拒绝的上下文

- truthy `previous_response_id` 或 `conversation`：400 / `response_reference_not_portable`。
- `input` 内 compaction、item_reference 或非 reasoning 项的 truthy `encrypted_content`：400 / `encrypted_context_not_portable`。
- `context_management` 中自动 compaction：400 / `encrypted_compaction_disabled`。
- Responses compact 端点：409 / `encrypted_compaction_disabled`。
- Responses WebSocket：426 / `websocket_filtering_unsupported`。
- 携带 Origin 或 Sec-Fetch-Site 的浏览器 Responses 请求：403 / `browser_request_rejected`。

这些拒绝避免静默丢失只能由原上游恢复的上下文。长会话应在自动压缩前生成文本摘要，再新建会话接续。过滤会丢弃历史内部推理状态；本轮推理强度会保留，但不能恢复已删除的状态。其他服务器资源型工具记录仍可能受上游归属限制。

## 状态与验收

管理卡片展示已过滤请求数、删除推理项数、删除记录 ID 数、修复代理文本片段数（`normalized_agent_text_parts`）、拒绝数、最近处理时间，以及网关最终 HTTP 状态和传输错误。HTTP 计数以网关完成请求的状态为准，不把内部成功恢复的每次重试单独计为失败。统计限当前服务进程；过滤日志仅写计数及错误码，不记录正文和鉴权。

核心测试的 `testdata/reference-v2.json` 是从原 `proxy.cjs` v2 生成的 46 组独立对照样本。v3 对上述 46 组样本保持兼容，另有真实故障结构的搜索/代理消息回归。原源码 SHA256 为 `3167507ac3c24c36bf5d85153ab534995d47a6edf1c34122281c92205066f425`。附加测试覆盖大整数与不透明工具数据、压缩与体积边界、全部路由别名、鉴权/模型白名单、SSE 首包与取消，以及 API Key/OAuth/SetupToken 的普通和透传转发与既有 429 重试。

```sh
cd backend
go test -tags=unit ./internal/pkg/codexhistory ./internal/pkg/httputil ./internal/server/middleware ./internal/server/routes ./internal/service ./internal/handler/admin
```

实际连接时应确认客户端地址直接指向 Sub2API，并比较一次真实请求前后的计数增量。一次模型成功回复不能单独证明过滤已启用。

## v3 故障验证

2026-09-21 的真实对照中，同一个历史 `web_search_call` 保留原 ID 时返回 400 / `The requested item was created under a different Azure OpenAI resource`，只移除该 ID 后返回 200。伪造的短 ID 没有复现同一错误，因此线上验收必须使用真实旧记录标识。

自定义 provider 的代理明文保留 `encrypted_content` 类型时会返回 400 / `Encrypted function output content could not be decrypted or decoded`。修正为文本片段后正常回放。该兼容处理不删除真实代理密文，不关闭本轮推理，不改写响应/SSE，也不新增重试。
