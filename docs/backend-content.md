# Content 首阶段：作者私有任务草稿

日期：2026-09-20。行为来源为 [PRD S2](prd.md#trusted-scenarios)、H1 及本轮用户列出的八项验收目标。具体数据结构、幂等键、上限与事务方案为 **Agent Self-Claimed**。

本阶段实现“创建 → 本人分页找回/读取 → 整体替换草稿”。不送审、不公开、不追加公开作品、不改变投票资格、不触发云端挑战。**作者私有草稿是审核前阶段，不是给过审作品新增公开/私有开关。** 未实现服务不以空壳替代。

## 1. 数据与状态

| 数据 | 归属与规则 |
| --- | --- |
| `content.task_drafts.id` | 服务端生成 `tsk_` + 128 位随机十六进制 ID；不是权限凭据 |
| `author_id` | 创建时取自 Identity `VerifyActor` 的稳定账号；创建后不可变；没有跨 schema 外键 |
| `content` | JSONB 任务聚合：title、summary、description、可选 category、按顺序的 entries；由具体 Proto 类型与服务端校验限制，不是任意 JSON 存储 API |
| 初始 `entries` | `content.entries` 中的作品草稿值对象，与任务同作者、同版本、同事务；不独立分配作品 ID；整体替换可增加、删除、重排，不保留数组索引作为稳定作品标识 |
| 作品来源与授权 | `source` 是署名/来源说明，不能决定账号；human 禁止 agentConfiguration，agent 要求 model；cloudUse 必须显式 true/false，与展示状态无关 |
| `revision` | 创建为 1；每次成功替换加一，含内容相同的有效版本替换；只按同一任务串行检查 |
| `state` | 唯一可写状态为 `draft`，数据库 CHECK 也阻止公开/送审；初始作品处于同一个草稿阶段 |
| `create_key` / `create_hash` | 同作者创建幂等键与规范化初始内容 SHA-256；唯一约束 `(author_id, create_key)`；后续替换不修改 |
| 时间 | 数据库记录创建、最后替换时间；目前 HTTP TaskSubmission 不扩展时间字段 |

草稿允许 entries 为空或仅一方。已填写的一件作品仍须符合现有 EntryInput 的完整字段约束；“任务未凑齐双方”不等于允许无来源、无授权或空交付物。当前最多 32 件、每件最多 32 个交付物，规范化 content 不超过 12000 字节；HTTP 总请求体不超过 16384 字节。限制是首阶段可调整的技术选择。

目前只支持 HTTP(S) 外链，不抓取、不执行、不自动访问 URL；拒绝带用户凭据的 URL。`file` 明确返回 409 `asset_service_not_ready`，不忽略资产归属校验，也不访问未实现的 Asset。Agent parameters 只作为惰性草稿元数据存储，不代表模型配置已获运行许可；未来云端使用须验证支持的模型、参数、来源及用途授权。

后续审核阶段再迁移状态约束、引入作品稳定 ID 与投稿快照，使审核结果绑定确切版本。未来 rejected 可改回 draft，pending_review/published/taken_down 不允许此编辑操作；本阶段不存在这些状态，也没有审核历史可覆盖。

## 2. HTTP / Proto 对齐

[OpenAPI](../openapi.yaml) 是 HTTP 字段依据；[Content Proto](../backend/proto/humanworth/content/v1/content.proto) 是真实进程契约，生成代码位于 `backend/gen/humanworth/content/v1/`。Identity 既有 RPC 不改字段与包名。

| HTTP | RPC | 输入与响应 |
| --- | --- | --- |
| `POST /api/tasks` | `CreateTaskDraft` | TaskDraftInput，必需 `Idempotency-Key`；201 TaskSubmission |
| `GET /api/me/submissions` | `ListMySubmissions` | 可选 kind/state/category、cursor、limit；200 SubmissionPage，首阶段 items 仅含完整 TaskSubmission |
| `GET /api/me/tasks/{taskId}` | `GetMyTaskSubmission` | 路径 ID；200 TaskSubmission |
| `PUT /api/me/tasks/{taskId}` | `ReplaceTaskDraft` | `{expectedRevision, content}`；200 TaskSubmission |

HTTP 单项及列表元素都保持 `kind: task`、`authorId`、数值 revision、`state: draft`、完整 content、`reviewId: null`、`rejectionReason: null`；不以摘要替换已有 TaskSubmission 契约。Proto 内部不重复传常量 kind/空审核字段，由 gateway 构造；不直接用 Proto JSON 代替 HTTP 响应（int64 等表示不同）。作品结构、可选值与参数对象均有对应 Proto 字段。

私有详情和本人列表只允许网站会话，不放入 MCP 白名单。没有登录 401；无权账号/不存在 ID 统一 404（管理员也不例外）；无效输入 400；Origin/CSRF/MCP 用途拒绝 403；重复键异内容或陈旧版本 409；依赖不可用 503；现有中间件容量限制 429。错误不返回数据库文本、断言或凭据。

公网列表、公开详情、送审、审核结果和独立作品投稿列表仍未实现；`GET /api/me/submissions` 首阶段只查询本人 task/draft 聚合，不把草稿读成公开任务，也不伪造 EntrySubmission。

### 请求示例

在已有本地 HTTPS gateway 和登录会话下，创建正文：

```json
{"title":"我的比较任务","summary":"暂存说明","description":"任务正文","entries":[]}
```

带 Cookie、正确 Origin、`X-CSRF-Token` 和 `Idempotency-Key: draft-create-example-0001` POST 到 `/api/tasks`。记录响应 id 与 revision，再 GET `/api/me/tasks/<id>`。替换正文：

```json
{"expectedRevision":1,"content":{"title":"修改后的任务","summary":"","description":"新的正文","entries":[]}}
```

PUT 到同一本人路径；成功后 revision 为 2。不要在 JSON 中填写 authorId、role、actorAssertion、state 或 publish。写入成功仅保存，不改变展示资格。

列表第一页：

```http
GET /api/me/submissions?limit=20
```

下一页把响应中的非空 `nextCursor` 原样放回；分类精确筛选示例为 `GET /api/me/submissions?kind=task&state=draft&category=%E7%BB%98%E7%94%BB&limit=20`。点击列表项后继续使用 `GET /api/me/tasks/{taskId}`，修改仍使用原 `PUT`。省略 cursor 或显式 `cursor=` 都是第一页；category 显式空值非法。

## 3. 身份链路与通信成本

```text
浏览器 Cookie +（写操作 Origin/CSRF）
  → gateway：固定路由决定 content audience 与完整 RPC 方法
  → Identity.ResolvePrincipal：检查会话/账号/用途/CSRF，签发短期断言
  → Content RPC（gateway 服务证书）：验证调用服务为 gateway
  → Identity.VerifyActor（content 服务证书）：方法/audience/签名/当前凭据复核
  → Content：只用 Principal.account_id 查/写自己的行
  → Content PostgreSQL 事务 → 返回本人快照
```

每个请求有三次内部 RPC（两次 Identity、一次 Content）。列表在一次 VerifyActor 后执行一次批量数据库查询，不对每条草稿调用 Identity、Asset 或详情 RPC，也没有跨库查询。暂不缓存鉴权结果、不为了减少通信去掉撤销复核。Content 不接收原始网站 Cookie/MCP token，不持有 Identity 签名密钥，不信任 HTTP 请求提供的作者。

会话撤销后发起的新认证不能放行；即便 gateway 在撤销前拿到了尚未过期的断言，Content 后续 VerifyActor 也拒绝。**已在撤销前完成 VerifyActor 的在途操作仍可能提交**；这里不承诺跨 Identity/Content 事务的全局瞬时撤销，这是避免跨服务数据库事务的明确边界。

mTLS 客户端同时验证证书 DNS 和 SPIFFE URI 的预期服务名称，不能只改连接地址而跳过服务身份。gateway 的 CONTENT_TARGET 未配置时草稿操作 503，不影响旧 Identity-only 启动；配置后就绪检测包含 Content。

<a id="database"></a>
## 4. 独立数据库权限与迁移

源码入口：`cmd/content`、`internal/content/server.go`、`internal/content/migrate.go`、`internal/content/migrations/001_content.sql` 与新增 `002_task_draft_list.sql`。一个 PostgreSQL 数据库可承载多个 schema，但权限分离，不跨 schema 查询，不做 Identity join。

1. 数据库管理员在选定的应用数据库执行 [bootstrap.sql](../ops/content/bootstrap.sql)。运行前通过 `CONTENT_OWNER_PASSWORD`、`CONTENT_RUNTIME_PASSWORD` 环境变量提供不同随机密码；脚本用 psql `\getenv`，不要把密码写进仓库或命令行。
2. 给 `content_owner` 单独的 DSN 文件（权限 600），用 `CONTENT_DATABASE_URL_FILE=<owner DSN 文件> go run ./cmd/content migrate` 迁移。
3. 用 owner/管理员执行 [grants.sql](../ops/content/grants.sql)。运行账号只有 Content schema USAGE、迁移版本/草稿 SELECT、草稿 INSERT，以及 content/revision/updated_at 三列 UPDATE；不能改 author/state/key，不能 DELETE 或 DDL，也不授予 Identity 权限。
4. 运行 `cmd/content` 改用 `content_runtime` DSN 文件，不与迁移进程共用权限。Identity 运行账号也不能读 Content。

初始化脚本明确是一次性管理员动作；角色已存在时失败，不隐式覆盖密码或扩大既有权限。应用数据库及 public schema 不应向 PUBLIC 授予 CREATE（PG18 新库默认如此，旧库须管理员核对）。迁移使用独立 advisory transaction lock、文件摘要与版本记录，可重复执行、拒绝修改已应用文件；不修改已应用的 001。002 只增加 `(author_id, created_at DESC, id DESC)` 且限定 `state = 'draft'` 的 partial index。category 仍在数据库 WHERE 中过滤，但不把完整分类值放入 B-tree：001 曾允许仅受草稿整体 12000 字节约束的长分类，表达式索引会让这类旧数据因 PostgreSQL 索引项上限而阻塞升级。升级测试同时保存重复中文和确定性摘要串组成的难压缩长分类，并先确认旧完整分类索引以 SQLSTATE 54000 失败，再执行当前迁移。服务运行时不执行迁移。

服务配置和构建命令见 [Go README](../backend/README.md)。已有 Dockerfile 的 SERVICE 参数可构建 content，不必复制镜像脚本。**未修改生产/kind 部署和 NetworkPolicy，不在本阶段自动部署。**

## 5. 分页、并发、超时与重试

### 本人列表

- 固定排序 `created_at DESC, id DESC`；边界条件为 `(created_at, id) < (cursor.created_at, cursor.id)`，查询 `limit + 1`，最多返回 limit 条。相同创建时间由 ID 确定顺序；编辑只更新 `updated_at`，不改变 `created_at`。
- category 在 SQL 的 WHERE 中精确筛选，不 trim、不改变大小写、不做 Unicode 归一化；缺省不筛选。为兼容 001 已允许的数据，category 不增加独立字符数限制，继续受有效 UTF-8、禁止 NUL 和草稿整体 12000 字节上限约束；筛选参数最多 12000 个 UTF-8 字节，因此覆盖所有合法既有分类。不新增枚举、分类表或服务。既有显式空 category 草稿仍可在未筛选列表中找回，但 HTTP 的 `category=` 不作为筛选值。为容纳合法 UTF-8 值的 percent-encoding，只有该列表路由的 Request-URI 上限为 40 KiB，gateway HTTP header 上限为 48 KiB；其他路由仍保留 8 KiB Request-URI 上限。
- kind/state 缺省归一为当前唯一支持的 task/draft；明确传入其他值或空值返回 400 `unsupported_submission_filter`。category 非法返回 `invalid_category`，损坏/版本错误/跨 scope/超过 2048 字符的游标返回 `invalid_cursor`；未知参数、重复参数和非法 limit 返回 `invalid_request`。非法游标不退化为第一页。
- 游标为版本化 Base64URL JSON，保存精确 RFC3339Nano 排序时间、ID 和账号/有效筛选/排序的 scope 摘要。它没有签名，不能作为授权凭据；Content 始终用 VerifyActor 的 author_id 形成 SQL 条件，并复核 scope，因此跨账号或换筛选复用会被拒绝。若未来需要隐藏或抵抗主动改写边界，再引入多副本共享的独立游标签名密钥。
- 不查询 totalCount，也不跨 HTTP 请求保持数据库快照。静态数据可连续翻页且无重复遗漏；翻页期间改变 category 会按新筛选成员关系影响后续页，客户端改变 category/kind/state 时必须从第一页开始。
- `limit` 是条数上限，不保证每个非末页都填满。Content 逐行读取并把当前 JSONB 解码为完整 Proto 后立即用 `proto.Size` 检查 2 MiB 单页预算；达到条数或字节预算即停止并关闭结果集，不再先解码整批 `limit + 1`。字节边界最多额外解码第一条放不下的记录，`nextCursor` 指向最后一条实际返回记录，后续记录由下一页继续读取，不截断单条对象或字段。Content 发送与 gateway 接收上限一致为 4 MiB，给应用预算留出一倍传输余量；单条草稿仍受 12000 字节 JSON 上限约束。

### 创建

- 键由客户端生成，16～128 个 ASCII 字母、数字、下划线或连字符；同一逻辑创建必须复用键。
- 内容经过 Proto 字段校验、默认数组及 JSON 对象键规范化后计算摘要。ActorAssertion、会话和 HTTP request ID 不参与指纹。
- 事务 INSERT ... ON CONFLICT DO NOTHING，再按作者+键读取；唯一约束处理跨进程竞争，不依赖内存互斥。
- 相同键+内容：201 返回同一任务的**当前**快照；即使已编辑也不覆盖。键相同但初始内容不同：409，不新建。
- 没有单独 TTL 清理，键随草稿持久存在；之后增加删除功能时必须再定义幂等记录保留策略。

### 替换

- 先 VerifyActor，再打开本地事务，以 `id + author_id` 查询并 `FOR UPDATE` 锁住任务；检查状态和 expectedRevision，原子替换整个聚合，revision+1。
- 同版本并发编辑恰有一个成功，其余 409；作品与任务不会部分写入。没有数据库锁内的外部 RPC。
- 重复 PUT 的旧 revision 返回 409，不悄悄当作新编辑，也不伪称可以精确找回历史编辑响应。超时后 GET 当前快照核对；并发编辑已发生时需客户端明确合并，不能盲目更新 expectedRevision 后覆盖。
- gateway 和 Content 的 gRPC 客户端关闭自动应用重试；`ListMySubmissions` 在 gateway HTTP 操作层和 Content gRPC 中间件均使用方法级 5 秒 deadline，其他普通操作仍为 2 秒，Google 回调为 15 秒。数据库仍保留 5 秒 statement timeout、2 秒 lock timeout 等更窄的资源保护子预算；5 秒不是取消这些数据库限制。超时不等于事务一定未提交。

## 6. 验收与证据范围

运行 `backend` 下的 `go test -race -tags=integration ./... -count=1 -timeout=120s`，必须提供专用 `IDENTITY_TEST_DATABASE_URL[_FILE]`。测试建临时数据库、Identity 与 Content 各自受限角色，结束清理；不连接产品库。Content 测试复用现有 OIDC HTTP 供应方，不调用真实 Google。

| 目标 | 检查入口与判据 |
| --- | --- |
| 本人创建/分页找回/读/改 | HTTPS gateway → mTLS Content OS 进程 → 真 Identity.VerifyActor → PostgreSQL；验证作者、内容、版本与状态 |
| 未登录/CSRF/伪造作者 | HTTP 拒绝；数据库行数不增加 |
| B 不能访问 A | GET/PUT 均 404；列表只返回各自作者行，管理员也不能借“我的列表”读取他人；同键不同账号不串数据 |
| 撤销立即影响后续认证 | 真实 logout 后 HTTP 401；撤销前已签发的读/写断言也不能通过新的 VerifyActor |
| 持久化 | 停止实际 Content 进程，启动新的进程，重新 HTTP 读取相同内容、revision 与列表 |
| 分页/筛选 | 空页、limit 前后边界、多页静态数据、同 created_at、末页 null、category 精确/未命中/变更、非法参数和跨账号/跨筛选游标均走真实 HTTP→gRPC→PostgreSQL；数字 Struct 触发字节分页，预算边界后的坏存储行不污染当前页 |
| 超时预算 | 临时 PostgreSQL RLS 测试函数把真实列表 SQL 稳定延迟 2.5 秒；列表经 gateway 与 Content 后仍成功，普通 RPC 的 2 秒中间件预算由单元测试保持 |
| 并发/重试 | 两个进程同键创建只有一行；竞争版本只有一个200；旧版本重试409；创建重试不回滚编辑；真实锁等待超时不部分写入，释放锁后可继续编辑 |
| 不自动公开 | state 数据库约束；无公开读取路由；公开 ID 访问不泄露草稿 |
| 信任边界 | 错误服务、错误 CA、跨方法断言、直接 RPC 非法输入拒绝；Identity 依赖中断时不放行 |
| 数据隔离 | Content owner/runtime 不能读 Identity；Identity runtime 不能读 Content；runtime 不能 DDL/改作者/改状态/删除 |

本次实际执行结果与未完成的环境门槛在任务交付报告记录；只有运行测试后才能标 PASS。HTTP/OIDC 测试供应方不能证明真实 Google 控制台配置；本地进程重启不是 Kubernetes 滚动发布、主备切换或公网验收。

## 7. 下一阶段

先对接 Asset 的上传/归属/授权检查，再设计提交审核的版本快照、稳定作品 ID、Content 与 Moderation 决定应用协议。随后才能打通公开可见性；投票与统计仍归 Voting。不要复用草稿读取方法实现公开查询，也不要在 Content 内直接读取身份数据库来省 RPC。

### 本轮实际验收记录（2026-09-20）

本机 Go 1.27.1、Buf 1.72.0、PostgreSQL 18.0。Docker socket 无权访问；复用 `/tmp` 中既有的 PostgreSQL 18 本地构建，每次 `initdb` 新建隔离数据目录，本次优化复验仅监听 `127.0.0.1:25434`，测试结束自动停止并删除数据目录与权限 600 的 DSN 文件。它不是产品数据库。以下结果不代表远端 GitHub CI 已运行或服务已上线：

| 实际命令 / 动作 | 结果 |
| --- | --- |
| `cd backend && /tmp/hw-submissions-buf lint` | 退出 0 |
| `cd backend && /tmp/hw-submissions-buf generate`；生成前复制 `gen/`，生成后 `diff -ru` | 退出 0；生成代码与 Proto 一致 |
| `cd backend && test -z "$(gofmt -l cmd internal)" && go vet ./... && go build ./cmd/... && node --check internal/gateway/web/account.js` | 退出 0；四个程序入口可构建 |
| `cd backend && go test -race ./... -count=1` | 退出 0；Content、Gateway、Identity 及 platform 单元/回归测试通过；受控 2.5 秒 handler 延迟确认仅列表使用 5 秒 Content 中间件预算，普通 RPC 仍为 2 秒 |
| `cd backend && IDENTITY_TEST_DATABASE_URL_FILE=<临时文件> go test -race -tags=integration ./... -count=1 -timeout=120s` | 退出 0；Identity 包 23.383 秒，覆盖真实 HTTPS→gateway→mTLS Content 进程→VerifyActor→受限 PostgreSQL、列表 SQL 受控延迟 2.5 秒、3000 数字嵌套 Struct 的 100 条逐行按字节分页、预算边界后坏存储行隔离、长分类写入/幂等重试/精确筛选、重启与依赖故障拒绝 |
| 集成夹具从 001 schema 与 6300 UTF-8 字节重复中文、6400 字节确定性摘要串分类升级，再重复执行 `content.Migrate` 并检查 schema 版本、排序索引、owner/runtime 权限与跨 schema 拒绝 | 退出 0；旧完整 category B-tree 探针以 SQLSTATE 54000 失败；001 未修改，002 可重复执行且不建该索引，两种旧值均保持不变 |
| 根目录 `npm run ci` | 退出 0；可信场景/链接、Swagger 构建、5 个 Node HTTP 测试、11 个 Python 测试通过 |
| `git diff --check` | 退出 0 |

必要说明：第一次一次性 PostgreSQL 初始化沿用了操作系统用户名，测试 DSN 指定的 `postgres` 角色不存在，因此测试立即失败；改为 `initdb --username=postgres` 后按 120 秒门槛重跑通过。响应预算修复后的首次集成重跑发现 8 KiB Request-URI 无法承载兼容长分类，调整为列表路由 40 KiB、HTTP header 48 KiB；随后两次重跑发现数字 Struct 大页在 `-race` 下超过原 2 秒 deadline，先将累计大小计算从 O(n²) 改为逐项 O(n)，再为列表设置 5 秒 deadline。此次端到端超时复验最初使用表锁，但被既有 2 秒 `lock_timeout` 先行终止，不能证明 RPC context；改用只在测试期启用、每条 SQL 仅睡眠一次的 RLS 函数后通过。逐行解码夹具第一次写入的数据不满足既有 `entries` 数据库约束，改为“满足 schema 但无法解码为 Proto”的对象后通过。Buf generate 首次在沙箱内不能访问固定版本插件，按正式权限机制联网重跑通过。最终 Go 检查链首次使用默认 `~/.cache/go-build` 时遇到只读缓存，改用隔离的 `GOCACHE=/tmp/hw-go-cache` 后同一命令通过。上述失败实例、进程和临时凭据均已清理，不计为 PASS。没有运行远端 GitHub CI、kind 故障实验、Google 真人授权、公网部署或数据库主备切换。
