# Hark2API · 本地管理界面

这是一个本地运行的 Hark 管理与 OpenAI 兼容网关示例。
管理员密钥在服务器以 SHA-256 校验值配置，前端不保存密钥；登录后使用 HttpOnly 会话 Cookie。
客户端 `/v1/*` 仍使用各自 API Key，管理员会话不替代客户端 Key。
当前使用 HTTP，浏览器与服务器之间的管理密钥和 Cookie 不受传输加密保护。
部署、服务状态、回滚及实际验证见 `research/deployment/VERIFICATION.txt`。

运行数据默认保存在程序目录下的 `data/`，该目录已被 `.gitignore` 排除。

## 打开界面

双击 `start-hark2api.cmd`，将启动服务并打开：

<http://127.0.0.1:8789>

首次运行需要 Go；启动脚本会在缺少 `hark2api.exe` 时自动编译。编译产物不会提交到 Git。双击 `stop-hark2api.cmd` 可停止本目录的服务。

也可以在终端中运行：

```powershell
cd path\to\hark2api
.\hark2api.exe -addr 127.0.0.1:8789
```

管理界面默认只监听本机。端口 8789 已被其他程序占用时，启动脚本会报错并保留那个程序。

## 已实现的功能

| 页面 | 实际行为 |
|---|---|
| 概览 | 本地服务状态、账号和 Key 数量、API 地址复制、接入步骤、最近活动 |
| 账号管理 | 手动粘贴 Cookie 导入、检测会话与移除 |
| API Keys | 随机生成、创建时显示完整值、复制、撤销；本地仅保存 SHA-256 哈希 |
| 请求测试 | 编辑文本 JSON、在本地校验字段、显示结果、复制 cURL 示例 |
| 运行日志 | 本次运行的真实操作记录、搜索、JSON 导出；最多 200 条 |
| 设置 | 工作空间名称、显示时区和预留请求超时；保存到本地配置 |

页面适配桌面和手机浏览器，静态资源内嵌于程序，不依赖外部字体或 CDN。

## 连接状态

已实现并实测 Cookie + Project 聊天适配器、跨轮上下文复用和会话结束归档。

- `GET /v1/models`：需要已创建的 API Key，返回模型别名 `hark`。
- `POST /v1/chat/completions`：验证 API Key 和文本输入，使用检测有效的 Cookie 建立独立会话、发送消息并轮询回复，返回 OpenAI JSON 或缓冲 SSE。
- 请求测试页支持本地校验和真实 Hark 回复；默认生成会话 ID，重置请求会生成新的会话 ID。
- 账号检测仅调用 `GET https://hark.com/api/auth/get-session`，区分有效会话、空会话、401、403、429及未知响应。
- 有效登录会话和聊天链路就绪是两个状态。

账号导入现在由用户手动提交 Cookie：

1. 在 Hark 官网完成登录，取得发送给 `hark.com` 的 Cookie 请求头值。
2. 在「账号管理」点击「导入账号」，输入账号名称，并将 Cookie 值粘贴到表单中；应为单行 `name=value; name=value` 格式。
3. 导入后点击「检测会话」。有效、失效、403 访问受阻与限流会分别显示；导入成功只表示本地接收了 Cookie，不代表 Hark 会话已经验证。

管理页面不再要求安装扩展，也不再生成登录链接。旧的桥接后端暂保留兼容，但前端不调用。完整 Cookie 不进入普通账号列表、日志或本地配置。

Cookie、账号名称和最近检测状态加密保存在 `data/accounts.enc`，AES-256-GCM 密钥保存在 `data/accounts.key`。服务重启会自动恢复账号；旧版仅在内存中的 Cookie 首次更新后需要重新导入一次。备份或迁移时同时保留这两个文件，文件内容损坏或缺少密钥时启动会报告错误，不自动覆盖。密钥和密文都在本机 data 目录，因此能读取整个目录的人仍可解密账号。

导入与移除仅在保存成功后返回成功，移除会同步更新加密文件。检测状态记录只是最近一次结果，不保证 Cookie 永远有效。客户端 Key 哈希和工作空间配置仍保存在 `data/config.json`。日志不记录 Cookie 或完整 Key，并在重启后重新开始。

## 本地验证

### 上游代理

本机程序默认通过 `http://127.0.0.1:7897` 访问 Hark。账号检测、只读诊断、
Project 创建、消息发送和历史轮询共用同一个代理客户端。管理界面仍是
`http://127.0.0.1:8789`，不改变浏览器或其他项目的代理设置。
代理不可用时返回连接错误，不自动切换直连；启动前需要代理程序监听7897。
`/admin/state` 的 `upstream_proxy` 显示当前代理地址。
自定义启动示例：`hark2api.exe -upstream-proxy http://127.0.0.1:7897`。
显式直连使用 `hark2api.exe -upstream-proxy ""`。

### Cookie + 独立 Project 聊天

下游 `/v1/chat/completions`、管理请求测试和格式校验共用原生 Project 格式适配层。
请求体上限为 1 MiB，管理设置等其他接口仍为 32KB。消息角色及内容保留，
不将 developer 改成 system，不添加工具调用模拟指令，不裁剪历史。
按管理员选择，每次 message 包裹当前 Project 上下文约束，说明见下文。
单条 user 的字符串原文保留在上下文约束包装内；纯文本块按换行连接。
多条消息、单条非 user 消息以原始 messages 数组的 JSON 文本放入 message，
包含额外消息字段及 tool_calls/tool_call_id。图片等内容块保留 JSON；
这不代表 Hark 的文字接口具备 OpenAI 多模态输入能力。

model=hark 是本地模型别名，stream 控制返回 JSON/SSE，session_id/session_end
控制 Project 生命周期。它们由网关处理；messages 映射为原生 message 字段。
tools、tool_choice、temperature、max_tokens、metadata、response_format 等其他
参数值作为原始 JSON 字段转发，接受与否由 Hark 决定，不在本地改写或编译 Schema。
路由字段 conversationId/idempotencyKey/responseMessageId 和 message 由网关管理，
请求体显式提供这些保留字段时返回 400，避免改变会话路由。
响应头为 X-Hark-Input-Adapter: native-project-v1 和
X-Hark-Parameter-Forwarding: original-json-values。
Hark 回复的文字内容直接封装为 OpenAI JSON 或缓冲 SSE，不将文字 JSON 解析成 tool_calls。
非法 JSON 返回 `invalid_json`；超出1 MiB返回413 `request_too_large`。

每个 API Key + 会话 ID 自动绑定 Hark Project，同会话复用，无需手动创建或选择。
已核对官网产品前端：普通 Chat 使用 `/api/conversations/main`；Project
使用 `POST /api/conversations`，传入 `title`、`timezone`、`autoTitle`，
得到 `conversationId`，官网对应 `/project/<conversationId>`。
发送使用 `POST /api/messages/send?cid=...`，历史使用
`GET /api/messages/history?conversationId=...`。`/api/projects` 并不存在。
真实 Project 已创建并返回测试文字 `HARK_PROJECT_OK`，证据保存在
`research/project-mode/live-project-result.json`。

发送成功必须确认 conversationId、messageId 与本次请求一致且
redirected=false，否则返回 `upstream_project_mismatch`，不回退到 Chat。
responseMessageId 是任务占位消息；文字答案通过 triggeredByMessageId
关联本次 idempotencyKey，来源为 send_bubble、presentation.kind=text。
只有本次主任务完成、所有已观察到的关联任务和文字停止流式输出，
才返回答案；识别 jobStatus 和显式 isStreaming 字段，不根据文字稳定猜测。
失败、取消、interrupted、cost_capped 和 error 返回错误。未知完成字段
返回 `upstream_protocol_unverified`。消息发送不自动重试。

不同 Key 或不同会话 ID 的 Project ID 不共享。单条 user 保留原文并包裹会话约束；
多条消息作为带 role/content 的 JSON 文本发送，保留客户端历史。
每次都保留本次请求提供的完整消息数组；同一 Key + 会话 ID 的 Project 仍复用。
客户端可只传最新 user 以使用 Project 历史；传完整历史时网关不会自行去重。
消息角色由文本表达。活跃 Project 在官网列表显示，结束或闲置后归档；
账号级长期记忆可能跨 Project 作用，这里保证的是独立 Project 路由。
401、403、429 分别报告会话过期、访问受阻与限流；无有效账号返回409。
超时返回504，已受理任务可能仍在上游运行。

`stream:true` 是等完整答案后封装 SSE，响应标记
`X-Hark-Stream-Mode: buffered`，不是逐字增量。用量不作猜测。
管理界面的「请求测试」无需填写 API Key，点击「发送到 Hark」后显示
真实 Project 回复、HTTP 状态、耗时与原始 JSON/SSE 响应；「校验格式」
仍只做本地校验。页面使用受管理员会话保护的 `/admin/playground/chat`
由服务端调用已保存的 Hark 账号，不向浏览器下发客户端 Key。
外部客户端的 `/v1/chat/completions` 仍要求有效 API Key。
「停止等待」只停止客户端等待，上游任务可能已经受理，不自动重发。
最短调用（将 YOUR_API_KEY 换成你创建的 Key）：

```sh
curl http://127.0.0.1:8789/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"hark","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

账号卡片的「诊断聊天链路」只读检测登录与 Project 列表。
`project_protocol: verified` 表示已经核对并实测 Project 协议；
`chat_ready` 取自该账号最近一次成功的 API 聊天调用，并要求当前登录有效。
`upstream_sent: false` 表示诊断操作本身没有发送消息，不表示聊天失败。
检测 Cookie 不会自动发测试消息。成功聊天状态和 Cookie 一起加密持久化。

```powershell
go test ./...
go vet ./...
go build -o hark2api.exe .
go test -c -o research/account-bridge/app.test.exe ./internal/app
python research/verify_evidence.py verify REPORT.md design-reviewed
```

Go 测试覆盖 Cookie 加密持久化、不在普通响应中暴露、Project 参数和路由隔离、回复关联及完成状态。旧扩展的测试记录保存在 `research/account-bridge/VERIFICATION.txt`，仅作历史参考。

截图：`research/ui-desktop.png`、`research/ui-mobile.png`。验收记录：`UI-VERIFICATION.txt`。

`ROLLBACK-UI.sh` 只在指定的界面副本上恢复初始 HTML，用于验证回退；服务使用内嵌页面，修改源码后需重新编译程序。该脚本不修改原 WorkBuddy 项目。

探索报告见 `REPORT.md`。后续聊天适配应根据登录实测完成，具体接口证据及公开源码审阅记录位于 `research`。

## Project 会话复用与归档

同一 API Key + 会话 ID 复用同一个 Hark Project；不同 Key 或不同会话 ID 的路由和回复关联隔离。
会话固定绑定首次选用的账号，不自动切换账号。映射保存在 `data/project-sessions.json`，
服务重启后恢复；文件不存明文 Key、Cookie 或原始会话 ID，也不通过管理接口公开 Project ID。

请求头 `X-Hark-Session-Id` 优先于请求体 `session_id`。
会话 ID 请保持稳定、区分大小写，最长 256 字节，不含控制字符及首尾空格。
未提供会话 ID 时每次新建 Project，不复用 Project 历史，也会在闲置后归档。
同账号的 Hark 原生检索仍能跨 Project，新增 Project 本身不是权限边界。

```json
{
  "model": "hark",
  "session_id": "conversation-001",
  "messages": [{"role": "user", "content": "继续上一轮的问题"}],
  "stream": false
}
```

每次保留客户端提供的完整历史；客户端也可只传当前输入并依靠 Project 已有历史。
同一会话并发返回 `409 session_busy`；不同会话可以并行。
客户端断开只停止本地等待，上游可能仍在生成，不要直接重发。

最后一次聊天请求设置 `session_end: true`，或请求头 `X-Hark-Session-End: true`，
收到完整回复后归档。不带结束标记的会话默认闲置 30 分钟后归档，每分钟检查一次；
启动参数 `-project-idle 30m` 可配置（至少 1 分钟）。归档不是删除，但该会话下次请求
会创建新的 Project，不自动恢复旧上下文。归档失败保留映射并重试；显式结束的会话
在归档完成前返回 `409 session_ending`。

响应头 `X-Hark-Project-Reused`、`X-Hark-Project-Archived` 标识本次复用与归档结果。
请求测试页面也支持上述 JSON 扩展字段，使用独立于下游 API Key 的命名空间。

## 同账号的 Project 上下文约束

管理员选择继续使用现有 Hark 账号，采用行为约束而非独立账号隔离。
每次发往 Hark 的 message 都增加 `HARK2API PROJECT CONTEXT BOUNDARY`：
只使用当前 Project 的历史、文件和工作，以及客户端显式提供的内容；
禁止列举、搜索、读取、引用、导入或修改其他 Project/会话，以及读取或
更新账号全局记忆、画像和跨会话搜索结果。依赖其他 Project 的请求应请
客户端把内容提供到当前会话，不先查询。原客户端文字/历史保留在包装内，
其他 tools、temperature 等参数保留原值；不会模拟或注册工具。

约束覆盖 API Key、管理页测试、复用已有 Project 的每次发送，重启后仍
由内置代码添加。响应头 `X-Hark-Context-Isolation: instruction-only`，
管理状态 `provider.context_isolation=instruction-only`、`hard_isolation=false`。
这是发给模型的行为指令，不是上游权限控制，不保证对抗性输入下的隔离。
旧 Project 的已有外来信息不能靠此清除，当前 Project 内历史仍保留。

上游字段 `includeMemoryContext=false` 已实测没有阻止跨 Project 读取，
当前实现不添加这个未验证的开关、不删除全局记忆或改动个人账号配置。
基线/修改后真实对照及可执行回滚见 research/project-isolation/VERIFICATION.txt。

## 原始参数转发的协议边界

原封不动将 OpenAI 请求体 POST 到 /api/messages/send 返回 HTTP 400；该接口需要
Hark 的 message 和 Project 路由字段，所以两种请求协议需要转换。
补上原生 message，但同时保留 OpenAI messages 等运行参数的探测返回
HTTP 403：run override not enabled。网关保留这种拒绝为 upstream_run_override_denied，
不会转换成模拟工具成功，也不会丢字段重发。工具提示词模拟已移除。

按必要字段映射并携带原始 tools/tool_choice 的真实请求返回 HTTP 200，
但 Hark 的文字回复表示没有 echo_fixture 这个客户端工具；未返回结构化 tool_calls。
HTTP 200 只证明请求被接收并生成回复，不证明自定义 tools 已注册或 tool_choice 已生效。
Hark 的原生工具与客户端自定义工具是不同接口。直接转发不能为 Hark 注册后者；
原生 run override 支持也需要 Hark 上游提供。

普通原生文字请求、关联完成状态、Project 复用与归档仍实测成功。
这不表示 OpenAI tools/multimodal/response_format 已被 Hark 原生执行。
真实验证与回滚见 research/transparent-forward/VERIFICATION.txt。

## 已完成但未回答的结束提示

Project 回复中的 send_bubble 不只有 presentation.kind=text，还可能包含
summary 正文。网关按上游消息顺序合并当前请求关联且完成的 text/summary，
保留 Markdown 和原文，不裁剪字符。图片等未知种类仍需单独适配。
真实只读历史验证及回滚见 research/summary-reply/VERIFICATION.txt。

Hark 有时完成空文字的 web 任务后，追加没有 triggeredByMessageId 的
declined_notice（presentation.kind 为 declined）。网关在确认本次关联任务
完成、无待完成的关联任务后，仅接收同一用户轮次内、主任务之后、明确
isStreaming=false 的提示。旧轮次或下一轮的提示不会作为当前回复。

这类提示按原文转换成 HTTP 200 的 assistant.content，finish_reason=stop；
流式响应输出 [DONE]，响应头 X-Hark-Upstream-Outcome=declined。
网关记录警告，不把提示计作账号聊天验证成功，不误等到 upstream_timeout。
真实未完成任务仍按原有超时机制处理。只读历史验证见
research/dsh-timeout/VERIFICATION.txt。

后续维护提示词：

```text
后续可在本项目目录继续实现 Hark 上游；项目与其他工作区相互独立。
保留现有界面和管理 API。先读取 README.md、REPORT.md 和 research 证据。
保留已经实测的自动 Project 创建和 /v1/chat/completions 适配。
使用 nativeProjectPayload 转发原始参数，映射原生 message/路由和下游响应格式。
保留管理员选择的 projectScopedMessage 行为约束，每次发送均添加。
不添加工具模拟指令，不解析文字 JSON 为工具调用，不裁剪 messages。
Hark run override not enabled 原样记录为 HTTP 403，禁止丢字段重发。
Project 使用 /api/conversations 的 title/timezone/autoTitle 参数，
禁止回退到 /api/conversations/main。使用本次 idempotencyKey 关联
triggeredByMessageId，核对 jobStatus/isStreaming 和 send_bubble 文字。
provider 与账号的最近成功调用状态绑定，并保留 Cookie 加密持久化。
本地测试和真实 Hark 调用结果分开记录。
```
