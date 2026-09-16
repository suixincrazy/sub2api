# Codex 历史推理与记录 ID 过滤转发层

用于 Windows + Node.js 24+。无额外依赖，不需要 npm install。

转发路径：Codex → 本机过滤层（默认 18181）→ 网关 → 上游。
这里的「网关」可以是 sub2api、任意 http(s) 中转，或本机的 CC Switch。过滤层本身不依赖 CC Switch。

## 上游选择的三种方式

三者互斥，按下面的优先级生效：

- 默认（不加任何上游参数）：接管 `config.toml` 当前指向的地址。过滤层把该地址记进备份，然后把配置改指向自己，转发时再用回原地址。
- `--upstream URL`：转发到指定的 http(s) 网关，可以是远端地址。
- `--cc-switch` / `--cc-db PATH`：可选集成，从 CC Switch 数据库读取 Codex 代理端口。只有显式给出这两个参数时才会读该数据库，没有硬编码端口回退。

`--upstream` 与 CC Switch 两组参数不能同时给出。

## 使用

在 PowerShell 中运行：

~~~powershell
cd tools\ccswitch-codex-filter
node .\filter.cjs start
node .\filter.cjs status
~~~

默认方式下，`config.toml` 里已经指向的网关地址就是上游，不需要先开任何本机程序。
若要走 CC Switch，先在 CC Switch 中打开「本地代理」和 Codex 接管，再用 `--cc-switch` 启动；脚本也可以先启动待命。

看到 status 的 state 为 filtering、upstreamReachable 为 true 后，重新启动 Codex 终端，或完全退出并重开使用该配置的 Codex 应用。
已经运行的客户端可能缓存旧地址，修改文件不会给当前连接自动加上过滤。
恢复旧会话时，应检查 filteredRequests、lastFilteredAt 是否随新请求变化，
并检查 removedReasoningItems、removedItemIds 的增量；总数可能包含连通性测试，不能单凭总数判断当前窗口已接入。

更新脚本后，需要先 stop 再 start，重新加载代码。当前版本 status 应显示 filterVersion: 3。

关闭过滤层：

~~~powershell
node .\filter.cjs stop
~~~

停止会把配置里的地址恢复成脚本写入前的原值，再关闭监听；随后同样需要重启 Codex 客户端。
恢复只按备份改写地址，不需要上游可达，所以网关已经下线时 stop 依然能用。
重复 start/stop 是安全的。start 为隐藏后台进程，关掉启动它的终端仍会运行；
serve 在前台运行，可用 Ctrl+C 停止并恢复。未安装开机启动任务。

也可以使用 PowerShell 包装脚本：

~~~powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\ccswitch-filter.ps1 Start
powershell -NoProfile -ExecutionPolicy Bypass -File .\ccswitch-filter.ps1 Start -Upstream http://127.0.0.1:15721
powershell -NoProfile -ExecutionPolicy Bypass -File .\ccswitch-filter.ps1 Start -CcSwitch
powershell -NoProfile -ExecutionPolicy Bypass -File .\ccswitch-filter.ps1 Status
powershell -NoProfile -ExecutionPolicy Bypass -File .\ccswitch-filter.ps1 Stop
~~~

## 实际过滤内容

- 删除每次 POST /responses 或 /v1/responses 的 input 中整个 type: reasoning 项。
- 从 include 中删除 reasoning.encrypted_content，并设置 store: false。
- 清理 input 中普通消息、function_call / function_call_output、custom_tool_call / custom_tool_call_output 的顶层 id。
  这些历史 ID 可能属于另一上游资源，曾引发 “The requested item was created under a different ... resource”。
  仅移除可重新提交完整内容的记录 ID，保留工具配对所需的 call_id、消息 phase、工具参数与结果、嵌套文件/资源引用。
- 保留当前请求的 reasoning.effort、普通消息、图片、工具调用、工具结果和 call_id。
- 支持 JSON、gzip、deflate、br、zstd 请求；重新计算 Content-Length。
- 流式响应直接转发；不修改响应中的推理。即使上游仍返回密文，下次请求也会过滤它。
- 不重试模型请求。网关自己的故障转移收到的已经是过滤后的请求。
- 不修改任何数据库、API Key、Codex 历史文件。日志只记录时间、状态、数量和错误码，不记录请求正文或鉴权头。

这会丢弃模型前轮的内部推理状态，可能影响复杂任务的连续表现；保留本轮推理强度并不能恢复这部分状态。
它针对历史推理密文导致的解密失败，不保证解决所有上游报错。

## 长会话与接管边界

- 已有 compaction、item_reference、previous_response_id、conversation 引用或其他加密上下文时，会明确拒绝转发，避免静默丢失上下文。
- /responses/compact 以及请求中启用的自动 compaction 会被拒绝。Codex 长会话自动压缩可能因此停止；应提前请模型给出任务摘要，再新建会话接续。
- 仅支持 Responses HTTP/SSE。WebSocket 握手会返回 426；若手动启用了 WebSocket，应关闭该传输配置后使用。
- 仅编辑顶层 model_provider 指向的 [model_providers.NAME] 下的常规单行 base_url。自定义 --profile、命令行 --config 覆盖、应用内其他配置来源可能绕过它；以状态统计是否增长为准。
- 指定了 `--upstream` 或 CC Switch 而配置里的地址是别的目标时，过滤层进入 waiting-for-upstream，不会强行覆盖该地址。
  把配置改回指定的上游、等待 filtering 后重启 Codex；已缓存直连地址的窗口不会自动改道。
  默认方式没有这个状态：配置里是什么地址就接管什么地址。
- 当前 ID 清理覆盖 Codex 常见的普通消息及函数/自定义工具记录。其他依赖服务器资源的工具记录保留其 ID，不保证跨上游可移植。
- 每秒读取一次配置。检测到地址被改回上游后会再次接入；若在这一秒内启动客户端，仍可能缓存绕过过滤层的地址，需在 filtering 后重启客户端。
- 停止时只恢复脚本写入的精确地址，不覆盖其他设置或后来手工切换到的其他地址。
- 强制结束进程/重启电脑后，先重新 start 恢复监听，或 stop 离线恢复地址。不要先删除 .runtime/routes.json。

## 配置与状态

默认 Codex 配置为 CODEX_HOME/config.toml（设置了 CODEX_HOME 时），否则为用户目录 .codex/config.toml。
只有在给出 `--cc-switch` 或 `--cc-db` 时，才会以只读方式读取 CC Switch 数据库（默认当前用户 .cc-switch/cc-switch.db）中 Codex 的监听端口。

可覆盖路径、端口和上游：

~~~powershell
node .\filter.cjs start --port 18181 --upstream https://gateway.example
node .\filter.cjs start --config "D:\custom-codex\config.toml" --runtime "D:\custom-filter-runtime"
~~~

自定义 runtime 时，status/stop 也要指定同一个 --runtime。变更端口或配置路径前先 stop。

状态：
- filtering：已将文件中的地址接入过滤层；客户端是否真正经过它，看 filteredRequests 是否增长。
- waiting-for-upstream：后台待命，配置中的地址还不是指定的上游。
- waiting-for-codex-config：找不到 config.toml。
- waiting-for-supported-provider：没有找到支持的 provider/base_url 配置。
- config-error：接入失败，lastError 给出原因。
- stopped：过滤层未运行。
- stale-state：留有状态文件但进程已不在。

stats.removedItemIds 是清理的历史记录 ID 数量；stats.lastFilteredAt 是最后处理 Responses 请求的时间。
stats.lastUpstreamStatus / upstreamHttpErrors 记录上游 HTTP 状态及 4xx/5xx 次数；HTTP 200 内的流式业务错误仍由客户端显示。
status 的 upstream 是当前实际转发目标，upstreamReachable 是对它的一次 TCP 探测。

运行文件位于 .runtime/：filter.log 是日志，routes.json 是仅用于地址恢复的备份，service.json 是进程控制信息（包含本地停止令牌，请勿公开）。
filter.log 超过 4 MiB 时在下次 start 轮换一次。

## 验证

~~~powershell
node --test .\filter.test.cjs
~~~

测试使用工作区临时配置和本机模拟服务，不调用收费模型，覆盖密文与历史 ID 过滤、工具配对、文件引用、压缩请求、SSE、取消、错误透传、地址恢复、默认接管方式、重复启停。

可选真实连通性检查（会向当前上游发送一条极短模型请求）：

~~~powershell
node .\live-check.cjs
~~~

它调用本机已安装的 Codex CLI，创建临时会话，在本地请求中注入故意无效的历史 reasoning 密文，
以及 5 个带有模拟历史 ID 的消息和工具记录，再经运行中的过滤层检查返回 FILTER_OK。
同时检查密文和 ID 的清理计数，避免仅凭一次成功回复漏掉过滤缺陷。测试使用 low 推理强度，不修改你的正常推理设置。
不会输出密钥或改动已有会话。此检查不主动切换账号，不能代替每个上游的兼容性验证。部分上游限制客户端类型，因此使用真实 Codex CLI；普通 HTTP 探针可能收到 401。

找不到 CLI 时可加 --codex "C:\path\to\codex.exe"。--baseline 直接连当前上游做对照，不注入密文、不经过过滤层。

2026-09-14 初版实测：8 项本地测试通过，仅验证了 reasoning 密文过滤，未覆盖历史记录 ID。
后续修复：真实 Codex CLI 经当时的上游返回 HTTP 200 / FILTER_OK；
清理了 1 项无效 reasoning 和 11 个记录 ID（含探针注入的 5 个 ID，以及客户端自带记录的 ID）。
旧版本在当时的上游未复现原来的资源归属错误；该次实测验证了修复后的清理行为和当时上游的兼容性，未验证所有上游切换组合。
当前版本（filterVersion 3）解除了对 CC Switch 的依赖：11 项本地测试全部通过，其中包含默认接管方式的用例；
未在真实远端网关上重跑 live-check。

接口背景参考：[OpenAI reasoning guide](https://developers.openai.com/api/docs/guides/reasoning)。
当前实现依据 Codex 0.154.0 请求实测；文档在线访问返回 403，未据此声称已核对最新接口文档。
