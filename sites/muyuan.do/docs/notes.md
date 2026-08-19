# Muyuan 上游与 Sub2API 部署路线

## 目标

- 在已授权的宝塔服务器上部署 Sub2API。
- 将 `https://muyuan.do` 配置为 OpenAI/Codex 兼容上游。
- 使上游看到与本机 Codex 客户端一致的请求身份，并验证模型列表和实际 GPT 请求。

## 已知事实

- 目标仓库为 `Wei-Shaw/sub2api`，当前审阅基线为版本 `0.1.177`（提交 `baeac1f`）。
- 宝塔入口从本机浏览历史定位为 `http://<SERVER_IP>:<BT_PANEL_PORT>`；直接访问当前返回安全入口校验页，入口路径可能已变更。
- 用户提供了宝塔面板 API 密钥和 muyuan 上游 API 密钥；两者仅用于部署配置，不写入本笔记。
- 用户现象：muyuan 的 GPT 模型会验证 Codex 客户端 UA；成功标准是 Sub2API 可拉取模型并完成 GPT 请求。
- 仓库提供 Docker Compose 部署，包含 Sub2API、PostgreSQL、Redis，且支持 OpenAI Responses/WebSocket 入口。

## 方案对比

### A：流量优先，对照验证（推荐）

- 做法：先用上游的模型列表和最小 Responses 请求建立基线，记录响应状态、允许的请求头和错误；再在 Sub2API 的 OpenAI 账号/渠道配置中设置上游地址、密钥和必要的客户端头，逐项回归。
- 优点：能区分 UA、路径、协议和其他指纹要求，改动最小，验证证据完整。
- 风险：上游可能返回动态挑战或按 IP 限制，需要按失败响应继续定位。

### B：源码优先，直接扩展 Sub2API 转发头

- 做法：审阅 OpenAI/Codex 网关的请求头构造点，增加固定 UA 或可配置头后重新构建镜像。
- 优点：对特殊上游的行为可完全控制。
- 风险：若实际上还需要 Beta 头、路径或 WebSocket 协商，单独改 UA 不足；自定义镜像增加维护成本。

### C：在宝塔前置反向代理改写请求头

- 做法：Sub2API 保持标准上游配置，在 Nginx/Caddy 层向 muyuan 改写请求头。
- 优点：应用无需改代码，回滚简单。
- 风险：宝塔代理对 WebSocket/HTTP2/流式 Responses 的转发细节可能导致请求异常，且不能处理应用层请求体差异。

## 推荐路线

采用 A，先以现有代码和配置能力完成部署与对照请求；只有当证据表明上游要求 Sub2API 默认不会转发的头或协议细节时，再选择 B 或 C 的最小补丁。

## 待验证假设

- muyuan 是否只检查 `User-Agent`，还是同时检查 `OpenAI-Beta`、`Accept`、`Content-Type`、Responses 路径、WebSocket 握手或模型名。
- 当前宝塔安全入口的实际路径，以及面板 API 是否允许通过该 API 密钥执行 Docker/文件操作。
- Sub2API 当前版本的 OpenAI 账号字段是否支持自定义请求头或需要源码补丁。
- 上游密钥对应的模型列表和可用模型名。

## 下一步

- 完成仓库部署文档与转发代码审阅。
- 通过不带敏感值的 HTTP 探测和现有浏览器会话确定宝塔/API 入口。
- 建立上游基线，再按证据进入实施计划。

## 2026-08-16 上游请求矩阵（脱敏）

| 请求 | 身份 | 结果 |
| --- | --- | --- |
| `GET /v1/models` | 普通 `Mozilla/5.0` | `200`，返回非空模型列表 |
| `POST /v1/responses`（非流式） | 普通 `Mozilla/5.0` | 超时 |
| `POST /v1/responses`（非流式） | `codex-tui/0.146.0 (Ubuntu 22.4.0; x86_64) xterm-256color` | `200`，最小提示词返回 `OK` |
| `POST /v1/responses`（流式） | 同上 | `200`，`text/event-stream`，事件流完整并返回 `OK` |

成功请求同时带有 `originator: codex_cli_rs`、`version: 0.148.0-alpha.9`、
`OpenAI-Beta: responses=experimental` 和非空 `X-Codex-Window-ID`；密钥值已省略。
已确认模型包含 `glm-5.2`、`codex-auto-review`、`gpt-5.5`、`gpt-5.6-terra`、
`gpt-5.6-sol`。本机 Windows Codex 安装包内部版本为 `0.148.0-alpha.9`，
但直接启动 WindowsApps 二进制受系统权限限制，故部署使用已验证且不低于上游门槛的
`codex-tui/0.146.0` 身份。
