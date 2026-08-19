# Muyuan Codex 上游接入实施计划

## 目标

- 在 `<SERVER_IP>` 的宝塔服务器上以 Docker Compose 部署 Sub2API。
- 将 `https://muyuan.do` 作为 OpenAI API-key 上游接入。
- 让上游收到可接受的 Codex 客户端身份，并完成模型列表与实际 Responses 请求验收。

## 已知事实

- 审阅基线为 Sub2API `0.1.177`（`baeac1f`）。
- 服务器具备 Docker 28.0.1 和 Docker Compose 2.32.1，端口 8080 可用。
- 宝塔安全入口已关闭，现有 `kuwo-app`、`rustdesk-server`、`qinglong` 容器不可受影响。
- OpenAI API-key 账号原生支持 `base_url`、`api_key`、`user_agent` 和自定义请求头。
- `GATEWAY_FORCE_CODEX_CLI=true` 可统一 OpenAI Responses 出站身份。
- 上游密钥和宝塔 API 密钥只进入运行时请求或服务器私密配置，不写入本文件。

## 待验证假设

- 本机 Codex CLI 的版本号可从安装包元数据或二进制中可靠取得。
- Muyuan 接受 Codex `User-Agent`，并可能同时要求 `originator`、`version`、`OpenAI-Beta` 与窗口 ID。
- Muyuan 的模型列表位于 `/v1/models`，GPT 推理入口兼容 `/v1/responses`。
- Sub2API 当前发行镜像与审阅代码的账号字段和网关行为一致。

## 计划产物

- 上游请求矩阵及响应证据，敏感值脱敏记录在 `docs/notes.md`。
- 服务器 `/www/sub2api/docker-compose.yml` 与权限受限的 `.env`。
- 一个 Muyuan OpenAI API-key 账号、可路由分组和 Sub2API 客户端 API 密钥。
- 模型列表、非流式 Responses、流式 Responses、健康检查与重启持久性结果。

### 任务 1：锁定 Codex 身份和上游基线

**目标：** 确定上游实际接受的最小请求身份。

**输入材料：** 本机 Codex 安装包、Muyuan API 地址和授权密钥。

**输出产物：** 可复用的 UA 与头部组合、可用模型名。

**步骤：**

- [ ] 从安装包版本信息和二进制字符串交叉确认 CLI 版本。
- [ ] 分别用普通客户端头、Codex UA、完整 Codex 身份请求 `/v1/models`。
- [ ] 对可用 GPT 模型发送最小 `/v1/responses` 请求并记录状态与响应结构。
- [ ] 仅在完整身份仍失败时，根据错误响应转入证据链排错。

**验证标准：** 至少一组请求成功返回非空模型列表，且至少一次 Responses 返回有效输出。

**失败回退：** 若响应不一致，进入 `web-reverse-systematic-debugging`，逐项固定请求头、协议和请求体。

### 任务 2：部署 Sub2API

**目标：** 在不影响现有容器的前提下启动独立三容器栈。

**输入材料：** 官方 Compose、服务器 Docker 环境、随机生成的部署凭据。

**输出产物：** `/www/sub2api` 下的 Compose 与私密环境文件、三个健康容器。

**步骤：**

- [ ] 生成 PostgreSQL、Redis、JWT、TOTP 和管理员随机凭据。
- [ ] 经宝塔 API 写入部署文件，并将 `.env` 权限设为 600。
- [ ] 执行 `docker compose pull` 和 `docker compose up -d`。
- [ ] 放行 8080/tcp，并从服务器内外检查 `/health`。

**验证标准：** `sub2api`、`sub2api-postgres`、`sub2api-redis` 均为 healthy，外部 `/health` 返回 200。

**失败回退：** 查看目标栈日志；只调整 `/www/sub2api` 配置，不停止或重建其他容器。

### 任务 3：建立上游路由

**目标：** 通过管理 API 创建最小可用路由。

**输入材料：** 管理员凭据、任务 1 的有效身份组合、Muyuan 授权密钥。

**输出产物：** OpenAI API-key 账号、分组、客户端 API 密钥。

**步骤：**

- [ ] 登录 `/api/v1/auth/login` 并确认管理员身份。
- [ ] 创建 OpenAI 分组并配置允许的 GPT 模型。
- [ ] 创建 API-key 账号，设置 `base_url`、UA 和必要的覆盖头。
- [ ] 创建绑定该分组的 Sub2API 客户端 API 密钥。

**验证标准：** 管理 API 可读回账号和分组；账号连接测试成功；客户端密钥可鉴权。

**失败回退：** 对照当前版本 DTO 和接口测试修正字段；不直接修改数据库。

### 任务 4：端到端验收与持久性检查

**目标：** 证明请求确实经过 Sub2API 到达 Muyuan 并可持续运行。

**输入材料：** Sub2API 地址和客户端 API 密钥。

**输出产物：** 完整验收记录和交付凭据位置。

**步骤：**

- [ ] 经 Sub2API 请求 `/v1/models`，确认包含任务 1 的 GPT 模型。
- [ ] 经 Sub2API 请求 `/v1/responses` 的非流式与流式模式。
- [ ] 检查请求日志、账号命中和用量记录。
- [ ] 重启目标 Compose 栈，再次验证健康检查、登录和 Responses 请求。

**验证标准：** 模型列表非空、两类 Responses 均产出有效文本、重启后配置和数据保持。

**失败回退：** 根据失败所在层分别检查客户端鉴权、分组路由、账号头部和上游响应，不改动已验证的其他层。
