# AI 助手 — 前后端接口契约 (v2)

## 架构总则（先读这一段）

1. **每个会话一个 goroutine**：后端 `Manager` 持有 `convID → Session` 映射，每个 `Session` 是一条独立 goroutine，**独占**该会话的全部可变状态（当前生成、累积的 CoT/正文、消息缓存）。命令通过 mailbox（channel）串行投递，所以会话内部**没有锁、没有竞态**。
2. **前后端之间只有一条 SSE 长连接**：`GET /api/stream`，全局唯一，事件里带 `conv_id` / `message_id`，前端按需过滤。不再有「每次生成开一条流」。
3. **前端不持有真相**：前端**不做乐观更新**。用户消息也是等后端通过 `message` 事件下发后才出现在界面上。任何时刻前端显示的内容都等于后端记录。
4. **上行是普通 POST**：命令走普通 JSON 请求，返回 ack（不含内容）。内容一律从那条 SSE 流下来。
5. **会话忙时新消息直接丢弃**：某会话正在生成回复时，再往该会话发消息，这条消息**不落库、不排队**，直接丢弃并返回 409，前端弹提示。不同会话互不影响，可以并行生成。
6. **断线即重同步**：客户端重连后重新拉一次 `GET /api/conversations/{id}/state`（后端会返回**内存里的实时内容**，而不是 250ms 前的落库副本），配合 `seq` 高水位丢弃过期事件，保证不重不漏。

所有接口在同源 `/api` 下，时间戳为 Unix 毫秒。错误响应统一：

```json
{ "error": { "code": "bad_request", "message": "人类可读的说明" } }
```

错误码与 HTTP 状态：

| code | HTTP | 触发 |
|---|---|---|
| `bad_request` | 400 | 参数 / 请求体非法（含挂载了不属于自己的附件 / 文件 id、试图删除自己） |
| `no_model` | 400 | 这家供应商下没有可用模型 |
| `no_provider` | 400 | 还没配置任何供应商（或那家没填 API 地址） |
| `unauthorized` | 401 | 缺少 / 错误的访问令牌 |
| `forbidden` | 403 | Host / Origin 守卫拒绝（疑似 DNS rebinding 或跨源请求，见 §0）；或非管理员访问用户管理接口 |
| `not_found` | 404 | 会话 / 消息 / 供应商 / 用户不存在，或存在但不属于当前用户 |
| `busy` | 409 | 该会话正在生成（新消息被丢弃；streaming 中的消息不可编辑/删除） |
| `compacting` | 500 | 会话正在压缩上下文时又发消息，或重复触发压缩 |
| `archived` | 500 | 压缩冻结区（`id <= compacted_before`）的消息被编辑/还原/还原 CoT/删除/重新生成 |
| `upstream_error` | 502 | 代理上游请求失败（如拉模型列表） |
| `internal` | 500 | 其它内部错误 |

---

## 0. 访问令牌与请求守卫（多用户，强制鉴权）

**没有「不鉴权模式」**：服务是多用户（多租户）的，每个访问令牌对应 `users` 表里的一个用户，
除 `GET /api/health` 外的所有 `/api/*` 都必须带有效令牌，否则一律 `401`。
静态资源（`/`、`/assets/**` 等前端产物）必须放行——页面是 JS 渲染的，把入口也拦掉，
「输入令牌」那个界面自己就加载不出来。

### 用户与令牌（users 表）

- **一令牌一用户**：令牌解析 = 把明文令牌 SHA-256（hex）后查 `users.token_hash` 列。
- **库里只存哈希，永远不存明文**：库泄露了也拿不到可用令牌。明文令牌只在创建 / 重置用户的
  响应里出现一次（见 §1）。
- 令牌长度上限 256 字符，超长直接视为无效（防撞库）。
- 通过鉴权的请求会**节流刷新**该用户的 `last_seen_at`（每用户每分钟最多写一次库）。

### 首次启动引导（`-token`）

| 场景 | 行为 |
|---|---|
| users 表为空（首次启动） | 创建管理员用户「admin」，其令牌 = `-token` 的值；留空或 `auto` 都会生成随机令牌。**只有绑到非回环地址且本次确实创建了管理员时**，启动日志才打印明文令牌和 `/?token=…` 引导链接；纯 `127.0.0.1` 首次启动应显式指定 `-token`，否则无法从日志得知自动生成的令牌。同时把单租户时代的存量数据全部划归这个管理员：`user_id = 0` 的 conversations / providers / files / attachments 行，以及旧 `settings` 表的键值（迁到 `user_settings`） |
| users 表非空，且 `-token` 非空、没有任何用户持有它 | **再建一个 admin**（应急通道：管理员令牌丢了，可以用 `-token <新令牌值>` 重启——只要这个值不属于任何现有用户，就会为它创建一个管理员，恢复管理入口） |
| users 表非空，`-token` 为空或已被某用户持有 | 什么都不做；鉴权完全走 users 表，与启动参数无关 |

### 令牌放在哪里（按优先级依次尝试，取第一个非空值）

| 位置 | 用途 |
|---|---|
| `Cookie: ai_token=<token>` | **主路径**。首次引导后自动带上，`fetch` / SSE / `<img>` 全都覆盖 |
| `Authorization: Bearer <token>` | 给 curl、脚本，以及禁用 cookie 的环境 |
| `X-Auth-Token: <token>` | 同上（第二条 header 通路） |
| `?token=<token>` | 只用于**首次引导**，以及给加不了 header 的请求兜底（如 `<img src>`） |

**首次引导**：手机打开 `/ ?token=xxx`

```
GET /?token=xxx  ->  302 Location: /
                     Set-Cookie: ai_token=xxx; Path=/; Max-Age=31536000; HttpOnly; SameSite=Strict
```

令牌正确 → 下发一年期 cookie 并 302 抹掉地址栏里的 `token`（其它查询参数保留）；
令牌不对 → 不重定向、照常渲染页面（前端据此弹「输入访问令牌」，比白屏友好）。

为什么是 cookie 而不是把令牌留在 URL 里：URL 被保存成 PWA 入口后，启动时走的可能是
manifest 的 `start_url` 或浏览器自己存的地址，query 很容易丢，表现成每次打开都要重新输令牌。
cookie 按 origin 存储，加桌面、重启手机都还在。

cookie **刻意不带 `Secure`**：局域网是明文 http，带 `Secure` 的 cookie 浏览器根本不会存，手机这条路
会直接断。这是「内网明文 + 长期 cookie」的自觉取舍 —— 令牌只应该在内网用、不要与别的服务复用。
`SameSite=Strict` 顺带把 CSRF 也堵了（跨站请求一律不带 cookie），`HttpOnly` 让 JS 读不到。

未通过鉴权时返回 `401` + `{"error":{"code":"unauthorized","message":"需要访问令牌"}}`。

### Host / Origin 守卫

`guard` 中间件同时挡 **DNS rebinding** 和跨源请求，规则与绑定地址**无关**：

| `Host` | 结果 |
|---|---|
| IP 字面量 / `localhost`（手机用局域网 IP 走的就是这条） | 放行 |
| 在 `-allow-host` 白名单里（支持 `my-pc`、`my-pc:28888`、`*.local`、`*.ts.net`，端口忽略） | 放行 |
| 其它（必然是域名） | `403` + `forbidden` |

拦截点选在 `Host` 而不是 `Origin`：**同源 GET 浏览器不发 `Origin`**，只查 `Origin` 挡不住最主要
的读数据路径；而 `Host` 一定存在，正常访问它只会是 IP 字面量或 localhost，rebinding 却必然带一个
攻击者域名过来。`Origin` 与 `Host` 不同源时同样拒绝（本机回环任意端口放行，是给 Vite 开发服务器
的代理转发留的口子）。

> 历史坑：早先 `-addr 0.0.0.0` 会整体跳过 Host 校验（理由写的是「用户主动对外开放」）——
> 而绑 `0.0.0.0` 正是用手机的唯一方式。实测 `Host: evil.com:28888` 配
> `Origin: http://evil.com:28888` 能拿到 `200`，rebinding 完全可打。已在
> `TestGuardStillBlocksRebindingWhenExposed` 里锁住。

---

## 1. 当前用户与用户管理

```
GET    /api/me               -> 200 {"id":1,"name":"admin","is_admin":true}   # 任何登录用户
GET    /api/users            -> 200 [User,…]      # 仅管理员；非管理员 403 forbidden「需要管理员权限」
POST   /api/users            <- {"name":"小明"}    # 仅管理员；name 为空白 400
                              -> 201 {"user": User, "token": "64 位十六进制"}
POST   /api/users/{id}/token -> 200 {"token":"…"}  # 仅管理员；重置令牌，旧令牌**立即失效**；
                              -> 404 not_found（用户不存在）
DELETE /api/users/{id}       -> 204               # 仅管理员
                              -> 400 bad_request（试图删除当前登录的自己 / 最后一个管理员）
                              -> 404 not_found（用户不存在）
```

```json
User = { "id": 1, "name": "admin", "is_admin": true,
         "created_at": 1750000000000, "last_seen_at": 1750000000000 }
```

- `GET /api/me` 是前端判断「显不显示用户管理入口」的唯一依据（看 `is_admin`）。
- **明文令牌只在创建 / 重置的响应里出现一次**，之后任何接口都拿不回来（库里只有哈希），
  只能再重置。前端必须当场提供复制，并优先复制完整登录链接 `http://<host>/?token=<token>`
  （比裸令牌好用，直接发给别人即可引导换 cookie）。
- `POST /api/users` 建出的永远是普通用户（`is_admin=false`）；管理员只能由首次启动引导
  / 应急通道创建。
- **删除用户的清理**（一个事务内）：该用户的 conversations（messages / message_attachments /
  message_files 靠外键级联）、files 记录、user_settings、providers（models 靠外键级联）、users 行。
- **刻意保留**：attachments 元数据行与 misc 目录里的文件本体——附件按内容哈希去重，
  别的用户可能正引用同一条记录 / 同一份磁盘文件，不能跟着删。
- 删除成功后，向**被删用户**的所有 SSE 连接广播一条 `notice`（`level:"error"`，
  text「这个账号已被管理员删除」）；该用户的下一次 SSE 重连 / 请求会拿到 401，
  前端据此弹回令牌输入框。

### 多租户边界（一般规则）

- 会话 / 消息 / 供应商 / 文件 / 设置全部按属主（`user_id`）过滤；访问别人的对象一律表现得
  像不存在（`404 not_found`），不泄露「有这条但不是你的」。
- **SSE 按用户隔离**：连接在订阅时绑定 userID，带属主的事件只推给同用户的连接，
  跨用户投递在总线层被硬拒绝；`userID=0` 的全局事件只用于后端重启这类系统级信号。
- 供应商 API key 完全隔离：每个用户只能看到 / 使用 / 测试自己的供应商。
- 附件可见性（`GET /api/attachments/{id}/thumb|raw`）：满足任一即可见——
  (1) 本人是它的首个上传者；(2) 它挂在本人会话的消息上（哈希去重下别人先传了同一张图、
  本人复用同一行记录的情形）。其它情况 404。
- 注意这是「按属主过滤 + 内容寻址共享」的隔离，**不是绝对物理隔离**：同一张图（同 sha256）
  在磁盘上只有一份，多用户共享；删除用户也不清理磁盘文件。

---

## 2. 唯一的 SSE 流

```
GET /api/stream     -> 200 text/event-stream
```

响应头：`Content-Type: text/event-stream; charset=utf-8`、`Cache-Control: no-cache, no-transform`、
`Connection: keep-alive`、`X-Accel-Buffering: no`（阻止反代缓冲整个响应）；每 15 秒发一次注释心跳 `: ping`。

| 事件 | data |
|---|---|
| `hello` | `{"seq":100,"client_id":"c-1","server_time":1750000000000}` |
| `delta` | `{"seq":101,"conv_id":1,"message_id":12,"channel":"reasoning","text":"…"}` |
| `message` | `{"seq":102,"conv_id":1,"action":"created","message_id":12,"message":{…Message…}}` |
| `conv` | `{"seq":103,"conv_id":1,"action":"updated","conv":{…Conversation…}}` |
| `notice` | `{"seq":104,"conv_id":1,"level":"warn","text":"…"}` |

- `hello.client_id` 是这条连接的订阅序号（`c-1`、`c-2`… 顺序递增的整数），诊断用，前端不需要处理。
- `channel`：`reasoning`（CoT）/ `content`（正文）/ `tool` / `toolargs`：
  - `tool`：**一次工具调用开始**，`text` 是一句状态行（「正在搜索：x」「正在执行 Python…」
    「正在生成图片：…」「正在回看图片 #3…」「正在读取文件 #5…」）。低频事件，前端直接展示成状态行。
  - `toolargs`：`tool_calls` 的 `arguments` **流式增量**（模型正在写的查询词 / 代码 / 参数），
    需按序**拼接**累积，结束后对应那次调用开始执行。一轮可含多个并行调用；单条回复的所有工具
    合计**最多 8 轮**，达到上限后后端不再声明工具，模型必须基于已有结果直接写正文。
- `action`：`created` / `updated` / `deleted` / `truncated`。
  - `created` / `updated` 一定带完整 `message`，前端直接按 id 覆盖。
  - `deleted` / `truncated` 表示消息结构变了（删除单条 / 重新生成时的截断），**不带 message**，前端应重新拉 `state`。
- `conv.action`：`updated` / `deleted`。
- **`Conversation.generating` 在所有地方都是权威的**：会话列表接口按「有没有 streaming 消息」算出来，
  `conv` 事件里由会话 goroutine 按自身真实状态填好。每次生成开始和结束都会各广播一次 `conv` 事件，
  所以左侧列表的「正在生成」标记**直接信这个字段即可**，不需要前端自己去推断，也不依赖用户是否正在看这个会话。
- `notice`：服务端主动提示，前端弹 toast 即可。`conv_id` 可能缺省（全局通知不带）。目前四种用途：
  **生成失败**（`level:"error"` 且带 `conv_id`，用户可能已切到别的会话，不提示的话他要切回来才发现这一轮失败了）；
  **上下文压缩的结果**（成功 / 失败 / 无需压缩都有，见 §3.4，`level` 为 `info` 或 `error`，带 `conv_id`）；
  **供应商配置变更**（`level:"info"`、`text:"provider-changed"` 且无 `conv_id`，只推给操作者本人，
  该用户的所有标签页应重新拉设置）；
  **账号被管理员删除**（`level:"error"`，推给被删用户，见 §1）。
- `delta` 是**唯一**的增量事件；它是后端内存状态的投影，前端只做追加。

### `seq` 高水位规则（保证不重不漏）

`seq` 是后端全局单调递增的发布序号。`GET .../state` 返回 `seq` 字段表示「这份快照已经包含到哪个序号」。

前端维护一个全局 `lastSeq`：

- 收到任何事件，若 `event.seq <= lastSeq` → **丢弃**（快照里已经包含了）。
- **只在应用快照时**把 `lastSeq = state.seq`；应用单个事件时不要推进它。

前端必须先建立 SSE 连接、再拉快照；顺序反了会在两步之间丢事件。

**但只做到上面这一步仍然会丢字**，因为 `fetch(state)` 是异步的，事件在它飞行期间还在到达：

```
事件 101~105 到达 → 直接应用
state 请求返回 seq=100 → 用快照整体替换本地状态
→ 101~105 被覆盖，且不会再推第二次 → 永久丢字
```

所以拉快照必须走「**闸门 + 排队 + 回放**」协议（JS 单线程，不需要锁）：

1. 开始拉快照前打开闸门 `resyncing = true`，并清空待处理队列（旧队列代表的变更已被即将到达的快照包含）。
2. 闸门打开期间到达的事件**不应用**，只带着 `seq` 入队。
3. 快照到达后整体替换本地状态，并设 `lastSeq = state.seq`。
4. 关闸，回放队列里 `seq > lastSeq` 的事件，其余丢弃。

**任何**会整体替换状态的操作（切换会话、断线重连后的重同步、`deleted`/`truncated` 触发的重新拉取、会话列表重同步）都必须走这个闸门。

---

## 3. 会话

```json
Conversation = {
  "id": 1, "title": "新对话",
  "provider_id": 2, "provider_name": "DeepSeek",
  "model_id": "deepseek-chat", "effort": "medium",
  "system_prompt": "", "web_search": false,
  "summary": "", "compacted_before": 0, "compacting": false,
  "pinned": false, "archived": false, "generating": false,
  "message_count": 4, "created_at": 1750000000000, "updated_at": 1750000000000,
  "last_read_at": 1750000000000, "unread": false
}
```

**会话绑定到具体供应商**：`provider_id` + `model_id` 一起决定这条对话发给谁。
两个网关都有 `gpt-4o` 时靠 `provider_id` 区分。`provider_name` 是只读的冗余字段，方便界面直接显示。

**`web_search` 是联网搜索开关**：开启后生成时模型可自主调用 `web_search` 工具（function calling）。
能不能开取决于部署是否配了 SearXNG——看 `GET /api/settings` 的 `web_search_available`，
不可用时应禁用这个开关。

**`summary` / `compacted_before` 是上下文压缩的产物**（见 §3.4）：`compacted_before` 是摘要覆盖到的
消息 id（含），更早的消息已折进摘要、**冻结只读**（编辑/还原/还原 CoT/删除/重新生成一律拒绝）。
`compacting` / `generating` 都不是数据库列：`compacting` 表示该会话此刻正在压缩（期间阻塞发送），
`generating` 表示正在生成；列表接口和 `conv` 事件里这两个字段都是后端填好的权威值。

**`unread` 是算出来的**，不是存出来的：只要存在「比 `last_read_at` 更新的、已经答完的」助手消息
（`finished_at > 0`）就算未读。所以**正在生成时不会亮，答完那一刻才亮** —— 前端据此在侧边栏挂小红点。

`updated_at` 决定侧边栏的排序（`pinned` 优先，其余按它倒序）。它在两个时机被刷新：
**消息创建时**（你按下发送）和**回答落地时**（生成结束）。后者才是「答完了、把它顶上来」的那一下。

```
GET    /api/conversations                 -> [Conversation, …]
POST   /api/conversations                 <- {"title?","provider_id?","model_id?","effort?"} -> 201 Conversation
                                             # provider_id / model_id 省略时用设置里的默认值
PATCH  /api/conversations/{id}            <- {"title?","provider_id?","model_id?","effort?","pinned?","archived?","system_prompt?","web_search?"}
                                             # 只改 provider_id 不改 model_id 时，后端会把 model_id 清空，
                                             # 由前端随后把 model_id 设成新供应商下的某个模型
                                          -> {"conv": Conversation}
DELETE /api/conversations/{id}            -> 204
GET    /api/conversations/{id}/state      -> State
POST   /api/conversations/{id}/read       -> {"conv": Conversation}   # 标记「我看到了」，小红点熄灭
                                             # 刻意**不动 updated_at**：看过一条消息不该把它顶到最前面，
                                             # 那会让「按最近活跃排序」失去意义
POST   /api/conversations/{id}/fork       <- {"before_message_id?":12}  -> 201 {"conv": Conversation}  # §3.3
GET    /api/conversations/{id}/messages?before=&limit=   -> [Message, …]  # 向上翻页，§3.2
POST   /api/conversations/{id}/compact    -> 202 {"accepted": true}     # 手动压缩，§3.4
```

```json
State = {
  "conv": Conversation,
  "seq": 110,
  "generating": {
    "message_id": 12, "provider_id": 2, "model_id": "deepseek-reasoner", "effort": "high",
    "started_at": 1750000000000, "reasoning_len": 1024, "content_len": 0
  },
  "messages": [Message, …],
  "has_more": false
}
```

`generating` 为 `null` 表示该会话空闲。`generating` 非空时，`messages` 里对应那条消息的 `reasoning` / `content` 已经是**内存中的实时值**（不是落库的滞后副本）。`reasoning_len` / `content_len` 是**字节数**，只作诊断用；前端要显示字数请直接数文本字符。

**`messages` 首屏只含最近 10 条**（长会话不再全量下发），`has_more=true` 表示还有更早的，
用 `GET /api/conversations/{id}/messages?before=<首屏最旧一条的 id>` 往前翻（§3.2）。

### 3.1 会话标题是两步出来的

1. **秒出的兜底标题**：首轮生成结束时，直接拿第一条用户消息截断到 24 字（换行压成空格、超长补 `…`）。
   零成本、零延迟，左侧列表立刻有个能看的名字。
2. **异步的 AI 标题**：紧接着在后台发一次**独立**的上游请求让模型起名，回来覆盖掉兜底标题。

关于第 2 步：

- 那次请求**不进聊天记录**：不写 `messages` 表、不出现在 `state.messages` 里、界面上也看不到。
  它只是一次普通的上游调用。
- 它显式带 `reasoning_effort: "none"` 来关掉思考 —— 起标题不需要 CoT。实测 DeepSeek
  `deepseek-flash`：不关思考时会把较小的 `max_tokens` 额度耗在 reasoning 上，**正文可能返回空**
  （`finish_reason=length`）；关掉思考后能稳定拿到干净标题。
- **只尝试一次**：标题已经不是默认值时不再触发；请求回来时如果标题又变了（用户手动改过名），
  直接放弃覆盖。超时/失败都只是打条日志，界面上保留兜底标题，不会出现空白名。
- 用户随时可以自己改标题：`PATCH /api/conversations/{id}` 传 `title`（前端在顶栏和侧栏都做了入口）。

### 3.2 消息分页（向上翻页）

`state.messages` 只是最近 10 条。要更早的消息：

```
GET /api/conversations/{id}/messages?before=<message_id>&limit=<n>
-> 200 [Message, …]     # before 之前（id 更小，不含它本身）的最多 limit 条，正序；
                        # 附件 / 文件一并填好
-> 400 bad_request      # before 缺失或不是正整数
-> 404 not_found        # 会话不存在或不属于当前用户
```

- `before` **必填**，传「当前已加载的最旧一条消息的 id」，返回的是它**之前**的消息。
- `limit` 默认 20，合法范围 1~100；非法值（非数字、≤0、>100）一律静默按 20 处理。
- 返回不足 `limit` 条 = 没有更早的了，前端据此隐藏「加载更早」入口。
- 纯读库的只读接口，不经过会话 goroutine；翻页结果直接**前插**进本地消息列表即可
  （注意保持正序），不需要走 §2 的闸门重放协议。

### 3.3 分叉（fork）

```
POST /api/conversations/{id}/fork
<- {} 或 {"before_message_id": 12}    # body 可整体省略
-> 201 {"conv": Conversation}
-> 404 not_found（会话不存在或不属于当前用户；锚点早于最早一条消息时也是）
```

把锚点消息（含）之前的历史**全量复制**成一个新会话：

- **锚点规则**：`before_message_id` 省略或为 0 → 锚到**最后一条**消息（即复制整个会话）。
  否则锚点 = 该会话里 `id <= before_message_id` 的最后一条；`before_message_id` 比所有消息都大
  时等同整会话复制，比最早一条还小则锚不到 → 404。
- **复制内容**：锚点及之前的全部消息（正文 / CoT / `content_original` / `reasoning_original` /
  编辑标记 / `provider_id` / `model_id` / `effort` / `error` / usage / `search_log` / `tool_rounds`），
  消息的**附件关联与文档关联都复制**（`INSERT OR IGNORE`，seq 重排）；图片与文档本体不复制——
  按内容哈希去重，新消息共享同一份。摘要与冻结边界（`summary` / `compacted_before`）原样带走。
- 复制时仍处于 `streaming` 的消息标为 `aborted`（复制过去不可能继续）。
- 新会话标题 = `{原标题}-消息{锚点消息id}-fork`；供应商 / 模型 / 思考强度 / 系统提示词 /
  联网搜索开关原样复制。
- **冻结消息可作锚点**：已压缩归档（`id <= compacted_before`）的消息照常参与复制（只读拷贝），
  复制后在新会话里依然处于冻结区、**不可编辑**。
- **归属校验**：只能 fork 自己的会话（fork 会全量复制历史含 CoT，别人的会话一律 404）。
- 成功后向本人广播一次 `conv` 事件（`updated`），其它标签页立刻能看到新会话；
  复制出的新会话从此独立，后续编辑互不影响。

### 3.4 上下文压缩（compact）

```
POST /api/conversations/{id}/compact   -> 202 {"accepted": true}
-> 409 busy（正在生成，等结束再压）
-> 500 compacting（已经在压缩中，见错误码一节）
```

- **手动触发**（会话窗口右上角的「压缩当前上下文」按钮）；没有自动触发——
  `settings.compact_threshold` 只用于前端进度条提示「达到 X% 建议手动压缩」。
- 立刻返回 202，压缩在后台跑：进度看 `conv` 事件的 `compacting` 字段（true 起 / false 止），
  **每个结局都会有一条 `notice`**——成功（「上下文已压缩…」）、失败（超时 / 上游错 / 空摘要等）、
  「消息还太少 / 没有新内容 / 新增有效内容太少」（无事发生，`compacting` 也会照常解除）。
- 语义：保留最近 10 条不压，把更早的历史（连同上一次的摘要滚动合并，输入上限 6 万字符）
  交给**本会话的模型**压成约 1/4 长度的摘要（下限 300 字、上限 3000 字，超时 8 分钟），
  写入 `conv.summary` / `conv.compacted_before`。之后发上游的历史 = 系统提示词
  （会话级「宪法」，永远在最前、不参与压缩）+ 全局默认提示词 + 日期提示 + 摘要 + 近期消息。
- **冻结**：`id <= compacted_before` 的消息只读——编辑 / 还原 / 还原 CoT / 删除 / 重新生成
  一律被 `archived` 拒绝（后端硬保证，前端隐藏入口只是体验层）。
- 压缩期间该会话**阻塞新消息**（发消息得到 `compacting`），其它会话不受影响。

---

## 4. 消息
```json
Message = {
  "id": 12, "conv_id": 1, "role": "assistant", "status": "done",
  "content": "…正文…", "reasoning": "…完整 CoT…",
  "edited": true, "content_original": "模型原文", "reasoning_original": "",
  "provider_id": 2, "model_id": "deepseek-reasoner", "effort": "high", "error": "",
  "search_log": "", "seg_usages": [],
  "usage": {"prompt_tokens":120,"completion_tokens":800,"reasoning_tokens":512,"total_tokens":920,
            "cache_hit_tokens":64,"cache_miss_tokens":56},
  "attachments": [Attachment,…], "files": [File,…],
  "created_at": 1750000000000, "finished_at": 1750000009000
}
```

`provider_id` 是这条消息**实际发给了哪家**。多供应商下两个网关可能有同名模型（都叫 `gpt-4o`），
只记 `model_id` 是有歧义的。

`usage` 里的两个缓存字段是**前缀缓存**的命中/未命中：
DeepSeek 报的是 `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`，
OpenAI 系只报 `prompt_tokens_details.cached_tokens`，后端会把两种归一成这两个字段
（只给了命中数时，未命中按 `prompt_tokens - 命中` 推出来）。
上游完全没报缓存数据时两个都是 0，前端据此不显示这一项。

`reasoning_original` 非空 = CoT 被编辑过（存的是模型原文），可一键还原（§4.4）。

`search_log` 是本轮工具调用的 JSON 摘要（展示用），空串 = 本轮没调工具。数组元素的 `type`：
`"search"`（含 `query` 与 `results:[{title,url}]`，失败时还有 `error`）、`"python"`
（`code` / `stdout` / `stderr` / `exit_code`）、`"image"`（`prompt`，生成的图本体在 `attachments` 里）、
`"file"`（`name` / `id`，以及可选的 `pages` / `images`，read_file 的读取记录）。工具交互的完整记录（tool_rounds）只面向模型
回放，**不**随 API 下发。

`seg_usages` 是工具轮里**各段上游请求各自的 usage**（`usage` 是它们的**累加值**；
`seg_usages` 的最后一个元素是写正文那一段）。没有工具轮时该字段缺省。

`attachments` / `files` 不是数据库列，是查询时顺带填上的（§5 / §5.1）；带 `omitempty`，没有对应内容时字段省略。

`role`：`user` / `assistant` / `system`　　`status`：`streaming` / `done` / `error` / `aborted`

### 4.1 发送消息

```
POST /api/conversations/{id}/messages
<- { "content": "你好", "provider_id": 2, "model_id": "deepseek-reasoner", "effort": "high",
     "attachment_ids": [3], "file_ids": [5], "client_msg_id": "…" }
-> 200 { "accepted": true, "user_message_id": 11, "assistant_message_id": 12 }
-> 409 { "error": { "code": "busy", "message": "该会话正在生成回复，这条消息已丢弃" } }
-> 400 bad_request（content 为空且没带图时——只带 file_ids 不带文字和图同样被拒；
                    附件 / 文件 id 不属于当前用户时也是，提示「附件 / 文件不存在或无权使用」）
-> 500 compacting（该会话正在压缩，见错误码一节）
```

**这个接口不返回内容**。返回 200 只代表命令被接受，用户消息和助手消息都会通过 SSE 的 `message` 事件下发。

`409 busy` 时这条消息**没有落库**，前端应保留输入框里的文字并提示「上一条还在生成中，这条消息已丢弃」。

- `provider_id` / `model_id` / `effort` 省略时用会话自身配置；显式传了 `provider_id` / `model_id`
  且与会话当前值不同，成功发送后会**写回会话**（并广播 `conv` 事件）——发送框里的选择从此变成
  这个会话的固定配置。`effort` 只作用于本轮，不落会话。
- `attachment_ids` / `file_ids` 是上传接口（§5 / §5.1）返回的 id，可并列都传；
  后端会校验归属（必须是本人上传的、或已挂在本人会话上的），别人的 id 一律 400。
- `client_msg_id`：请求体会解析这个字段，但当前**不使用**（保留字段，没有任何效果——
  尤其不要指望它去重）。
- 助手占位消息在发起生成**之前**就已落库、生成结束才改状态，所以发送后立刻刷新页面也能看到
  「正在生成」的那条消息。

### 4.1.1 生成途中改模型 / 供应商：只对下一轮生效

允许在生成过程中改 `PATCH /api/conversations/{id}`，但**正在跑的那一轮不会被改道**：
后端在发起生成时就把「用哪个 client、哪家供应商、哪个模型」钉进了本次生成的状态，
之后改会话配置只影响下一轮。

这一轮产出的消息，`messages.provider_id` / `model_id` 记的是**实际用的那个**（不是会话最新的配置）。
前端要据此提示用户，例如「本轮仍在用 X，切换从下一轮生效」——否则用户会以为切换没生效。

### 4.2 停止生成

```
POST /api/conversations/{id}/stop   -> 200 { "stopped": true }
```

### 4.3 重新生成（截断）

```
POST /api/messages/{id}/regenerate  -> 200 { "accepted": true }
-> 409 busy（该会话正在生成）
```

- 目标是 **assistant** 消息：删除该会话中 `id >= 目标 id` 的所有消息，然后用前面的历史重新生成。
- 目标是 **user** 消息：删除该会话中 `id > 目标 id` 的所有消息，然后基于这条用户消息重新生成。
- 目标是冻结区消息（`id <= compacted_before`）→ `500 archived`。

截断会通过 `message` 事件的 `truncated` 动作通知前端重新拉 `state`。

### 4.4 编辑 / 还原 / 删除

```
PATCH  /api/messages/{id}          <- {"content?":"…","reasoning?":"…"}  -> {"message": Message}
POST   /api/messages/{id}/restore                                 -> {"message": Message}
POST   /api/messages/{id}/restore-reasoning                       -> {"message": Message}
DELETE /api/messages/{id}                                         -> 204
```

- 编辑 assistant 和 user 消息都支持；`content` 与 `reasoning` 可以在**同一次 PATCH** 里一起给
  （至少给一个，都不给返回 400）。
- `content_original` 只在第一次编辑正文时记录，之后反复编辑不会覆盖它，「还原原文」始终可用。
- **编辑 CoT**：`reasoning_original` 同样只在第一次编辑 CoT 时记录模型原文，
  `POST /restore-reasoning` 把它还原回来（与正文的编辑/还原完全对称）。
  编辑后的 CoT 会随历史回传给上游——这是「改 CoT 能引导模型后续思路」的闭环。
- 编辑后的正文 / CoT 就是后续发给 LLM 的历史内容（这是编辑的意义所在）。
- 正在 `streaming` 的消息不可编辑/删除 → `409 busy`。
- **冻结只读**：`id <= conv.compacted_before` 的消息（已被压缩归档）不可编辑 / 还原 /
  还原 CoT / 删除，以它为目标的重新生成也会被拒 → `error.code = "archived"`
  （HTTP 状态见错误码一节）。前端应隐藏这些消息的对应入口，但后端的拒绝是硬保证。

---

## 5. 附件（图片）

上传的图片存在**独立于数据库的 misc 目录**里（`-misc` 参数，默认 `./misc`），
所以 Docker 部署时可以把 `data`（SQLite）和 `misc`（图片）分别挂到不同的卷上。

```
POST /api/uploads                  multipart/form-data，字段名 file（可以带多个），单文件 ≤20MB
-> 200 {"attachments":[Attachment,…]}
-> 400 {"error":{"code":"bad_request","message":"…"}}     # 非图片 / 超过 20MB
-> 500 internal（这个部署没启用图片，即 misc 目录未配置）

GET  /api/attachments/{id}/thumb   -> 缩略图（JPEG，长边 320，通常 10~30 KB）
GET  /api/attachments/{id}/raw     -> 原图
                                    # 两者都带 ETag + immutable 长缓存，支持 304
                                    # 可见性按属主过滤，见 §1「多租户边界」
```

接受的格式只有 **png / jpg / gif / webp**（Go 的解码器集合）。HEIC / HEIF / AVIF 会被拒，
提示「这个文件不是能识别的图片」—— 注意**这不是让前端放弃**：HEIC 由前端转码后再上传
（见 §7 第 13 条），后端只会收到转好的 JPEG。

```json
Attachment = { "id": 3, "kind": "image", "mime": "image/png", "name": "shot.png",
               "size": 5765120, "width": 1600, "height": 1200,
               "thumb_url": "/api/attachments/3/thumb", "raw_url": "/api/attachments/3/raw",
               "created_at": 1750000000000 }
```

发消息时把上传拿到的 id 带上：

```
POST /api/conversations/{id}/messages
<- { "content": "看看这张图", "attachment_ids": [3, 4] }
```

- 允许 `content` 为空、只发图（`attachment_ids` 非空即可，见 §4.1）。
- **同一张图（内容 sha256 相同）只存一份**：重复上传会返回同一个 attachment id
  （首传者记为属主，后继上传者复用同一行记录）。
- 上传时保存原图并生成缩略图（长边 320）；原图较大时再生成**发送版**（长边 1024 的 JPEG）。
  原图本身够小（长边 ≤1024 且 ≤900KB）时不生成发送版——截图这类图转 JPEG 会把文字糊掉。
- **发给模型的是原图**：≤10MiB 的原图直接编码成 data URL 发上游（信息最全，尺寸归一化交给
  上游自己做）；只有原图 **>10MiB** 才退回发送版（上游对单图大小普遍有限制，base64 还有 33% 膨胀）。

### 5.1 文件（文档）

上传的文档存在 `misc/files/`（按内容 sha256 去重），**没有**对应的下载/查看接口——
文件内容只在模型调用 `read_file` 工具时由 pybox 沙箱按需提取（pypdf / python-docx / openpyxl，
提取结果截断到约 30000 字符），前端展示 `name` / `size` 即可。PDF 还支持 `pages` 与
`images=true`：模型自主指定页码，pybox 用 PyMuPDF 将这些页渲染成 JPEG，通过工具多段 content
回传给视觉模型；每次最多 5 页，不自动渲染整份文档。

```
POST /api/files    multipart/form-data，字段名 file（可以带多个），单文件 ≤20MB
-> 200 {"files":[{"id":5,"name":"report.pdf","ext":".pdf","size":102400}]}
-> 400 bad_request（没有 file 字段 / 不支持的扩展名 / 超过 20MB）
-> 500 internal（这个部署没启用文件上传，即 misc 目录未配置）
```

支持的扩展名：txt / md / markdown / log / csv / json / jsonl / xml / yaml / yml / toml / ini /
sql / html / css / py / js / ts / go / java / c / h / cpp / rs / sh / pdf / docx / xlsx。

```json
File（挂在消息上时）= { "id": 5, "hash": "…sha256…", "name": "report.pdf", "ext": ".pdf",
                        "size": 102400, "created_at": 1750000000000 }
# /api/files 的响应只含 id/name/ext/size；消息上的 files 是全量字段
```

- 同内容重复上传复用同一记录（去重），只把文件名刷新成最新的。
- 发消息时把 id 放进 `file_ids`（与 `attachment_ids` 并列，见 §4.1），归属校验同附件。
- 文件内容不进历史：模型只会在历史里看到一行 `[文件#5「report.pdf」]` 标注，需要时自己调
  `read_file` 读——这是防上下文爆炸的设计。

### 前端硬约束（性能相关，必须遵守）

1. **消息里的图片一律只请求 `thumb_url`**，并加 `loading="lazy"`；`raw_url` **只在用户点开时才请求**。
   恢复聊天历史时绝不能去拉原图——这正是缩略图存在的理由。
2. 灯箱关闭后可以保留已加载的 `raw_url`（浏览器有 immutable 缓存），但不要在消息列表里预加载。
3. 上传要支持：粘贴（Ctrl+V）、拖拽到输入框、点按钮选文件。
4. 待发送的缩略图显示在输入框上方，每张可单独移除；发送成功或失败都要有明确反馈。
5. 只发图不打字时也应当能发送（`content` 传空串）。

---

## 6. 供应商与模型

一个部署可以配**任意多个**供应商（每家的 base_url / api_key 一套），**按用户隔离**：
每个用户只能看到 / 使用 / 测试自己的供应商，apikey 互不可见。

```
GET  /api/health                 -> {"ok":true,"version":"1.0.0","stream_clients":2}
                                 # stream_clients = 当前挂着的 SSE 连接数（诊断用）

# ---- 供应商 CRUD ----
GET    /api/providers            -> [Provider,…]        # 按 id 升序
POST   /api/providers            <- {"name?","base_url","api_key"}            -> 201 Provider
PATCH  /api/providers/{id}       <- {"name?","base_url?","api_key?"}         -> Provider
                                    # api_key 省略或空串 = 保留原 key 不变
DELETE /api/providers/{id}       -> 204
                                    # 级联删掉它的模型缓存；引用它的会话会回退到「默认供应商」
                                    # （删除后如果一条供应商都不剩，会话将无法发起生成，直到重新配一个）

# ---- 单个供应商的操作 ----
POST   /api/providers/{id}/models/fetch   -> {"total":42,"added":7,"models":[Model,…]}
POST   /api/providers/{id}/test           <- {"model_id?":"…"}
                                          -> {"ok":true,"latency_ms":412,"message":"连接正常，模型 … 可用","model_id":"…"}
                                          -> {"ok":false,"latency_ms":0,"model_id":"…","message":"上游错误详情"}
                                             # 上游失败也返回 HTTP 200，失败原因在 message 里——
                                             # 前端别把业务失败当网络异常重试

# ---- 模型与默认值 ----
GET  /api/models                 -> [Model,…]     # 当前用户的所有供应商的模型，每项带 provider_id
GET  /api/models?provider_id=2   -> [Model,…]     # 只看某一家

GET  /api/settings
-> { "providers": [Provider,…],                    # 与 GET /api/providers 相同，省一次请求
     "defaults": {"provider_id":2,"model_id":"deepseek-chat","effort":"medium","system_prompt":""},
     "web_search_available": true,                  # 部署配了 -searxng 才为 true；前端据此显示联网搜索开关
     "compact_threshold": 0.7 }                     # 上下文压缩阈值（0~1），只用于前端进度条提示；
                                                    # 压缩本身是手动触发（§3.4），0 = 不提示

PUT  /api/settings/defaults      <- {"provider_id?":2,"model_id?":"…","effort?":"…","system_prompt?":"…"}
                                 # 新建会话的默认值；字段省略=不改，空串/0=清空
                                 # 换默认供应商时，如果原来的默认模型不属于新供应商，
                                 # 后端会自动在该供应商下挑一个顶上（保证默认值始终自洽）
                                 -> {"providers":[…],"defaults":{…}}   # 与 GET /api/settings 同形状
```

- `defaults.system_prompt`（全局默认系统提示词）是**注入式**的：每轮发上游前现读设置、
  作为一条 system 消息注入（顺序在会话级 `system_prompt` 之后、日期提示之前）。
  **改它立即影响所有会话的下一条消息**——不是创建会话时复制一份。
- 供应商 / 模型 / 默认值全部按用户隔离；`models/fetch` 和 `test` 也只解析**本人**的供应商
  （别人的供应商 id → 404 not_found）。

```json
Provider = { "id":2, "name":"DeepSeek", "base_url":"https://api.deepseek.com",
             "normalized_base_url":"https://api.deepseek.com/v1",
             "api_key_set":true, "api_key_hint":"sk-12****abcd",
             "model_count":2, "created_at":1750000000000, "updated_at":1750000000000 }

Model = { "id":3, "provider_id":2, "model_id":"deepseek-reasoner", "display_name":"deepseek-reasoner",
          "reasoning":true, "efforts":["low","medium","high"], "context_len":65536,
          "fetched_at":1750000000000 }
```

`efforts` 是前端思考强度下拉的**唯一数据来源**；空数组表示不支持思考，前端禁用该下拉并提示「该模型不支持思考强度」。

**前端硬约束**：模型选择器必须按 `provider_id` 分组展示（同名模型在不同供应商下是两条不同的记录），
选中一项时**同时**写回 `provider_id` 和 `model_id`。

---

## 7. 前端硬约束

1. **不要用 `EventSource`**（无法带 header、无法精确控制重连）。用 `fetch()` + `response.body.getReader()` + `TextDecoder` 解 SSE 线格式（按空行切块，`event:` / `data:` 逐行解析，多行 `data` 按 `\n` 拼接，`:` 开头是心跳注释，解析失败只跳过该块）。
2. **全局只有一条流**。断线要按指数退避重连（1s→2s→4s，上限 15s），重连成功后重新拉当前会话的 `state`。
3. **必须按帧节流**：delta 先进缓冲区，用 `requestAnimationFrame` 每帧 flush 一次再写响应式状态。上游可达 100 事件/秒，逐个 delta 触发 markdown 重渲染会卡死。
4. **输入法**：`Enter` 发送、`Ctrl+Enter` 换行、`Shift+Enter` 换行；必须判断 `e.isComposing || e.keyCode === 229` 并直接放行（中文拼音选词时按回车会误发）。
5. **自动滚动**：只在用户本来就贴着底部时跟随，用户往上翻时不要抢滚动条。
6. **CoT**：正文上方的可折叠块，流式期间自动展开逐字刷新，用户手动折叠后不再自动展开；结束后保持可见（后端已持久化）。
7. **不做乐观更新**：发送后不要自己往列表里插消息，等 `message` 事件。
8. **窄屏侧栏必须是抽屉**：`≤860px` 时 `.sidebar` 是 `position: fixed` 浮层，不占布局宽度；
   `.app--collapsed` 在宽屏表示「收起」、在窄屏表示「抽屉关着」，语义相反但复用同一个状态。
   **窄屏初始值必须是关**（否则 390px 宽的屏上对话区只剩 130px），并记住用户的选择；
   选中会话后自动关。不要只靠 `@media` 隐藏——抽屉要有遮罩，点遮罩关闭。
9. **输入框字号不得小于 16px**：iOS Safari 聚焦更小的输入框会强制缩放整页且不会自动缩回。
10. **软键盘**：iOS 弹键盘只缩 `visualViewport`、不缩布局视口，`100dvh` 不会跟着变。
    必须监听 `visualViewport.resize/scroll`，把被遮挡高度写进 `--kb-h`，让 `.app` 的高度减去它；
    用 80px 门槛把「地址栏收起」那种几十像素的变化排除掉，并且只在输入框获得焦点时才算。
    Android 另外需要 viewport meta 里的 `interactive-widget=resizes-content`。
11. **侧栏操作入口是常驻图标行，不许藏**：每个会话项固定显示一行操作图标
    （重命名 / 系统提示词 / 置顶 / 分叉 / 删除，`Sidebar.vue` 的 `.conv__ops`），
    不用 `:hover` 展开、也不用「⋯」折叠菜单——触屏的 sticky hover 会把展开状态压乱，按钮也藏得太深。
    触屏 `(hover: none)` 下点击目标最小边 `≥32px`（现有触屏样式把 `.icon-btn` 放大到 32~34px），
    入口的可见性**不依赖 `:hover`**。
12. **令牌**：所有 `/api/*` 请求要能带上令牌（cookie 自动带，`fetch` 可补 `X-Auth-Token`）；
    收到 `401 unauthorized` 时弹「输入访问令牌」，而不是当成网络错误重试。静态资源不需要令牌。
    令牌校验通过后服务会下发 cookie，前端**不要**自己把令牌存 localStorage。
13. **HEIC / HEIF 必须在上传前于前端转成 JPEG**：后端（Go）没有 HEIF 解码器，只认
    png/jpg/gif/webp；而能解 HEIC 的就是产出它的那台设备（iOS 走系统 ImageIO）。
    用 `createImageBitmap`（带 `imageOrientation: 'from-image'` 摆正 EXIF 方向）→ canvas →
    `toBlob('image/jpeg')` 即可，**不要引 WASM 解码器**（heic2any 2.7 MB / libheif-js 9.0 MB）。
    长边要缩到 ≤3840：iOS Safari 的 canvas 总像素上限约 16.7M，48MP 照片画不进去。
    浏览器解不了时要给出可操作的提示（改相机格式 / 先转格式），不要透传后端那句
    「不是能识别的图片」。`<input type="file">` 的 `accept` 只列后端支持的四种类型，
    顺带给 iOS 一个「按需转码」的信号。
14. **用户管理入口依 `GET /api/me` 的 `is_admin` 显隐**：非管理员不显示入口（直接调
    `/api/users*` 也会被 `403 forbidden` 拒绝）。列表要展示 `name` / `is_admin` /
    `created_at` / `last_seen_at`，并提供创建、重置令牌、删除操作。
15. **明文令牌只显示一次**：创建 / 重置用户拿到的 `token` 关闭弹窗后不可找回（只能重置），
    UI 必须当场提供复制按钮，且优先复制完整登录链接 `http://<host>/?token=<token>`；
    重置他人的令牌后其旧 cookie 立即失效，前端要给出「该用户需要重新登录」的提示。
