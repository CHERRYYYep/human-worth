# Moderation：初始任务送审与最小审核闭环

日期：2026-10-16。**Human Design**：本轮 4A～4D 指令、PRD S2/S4 与过审即展示约束。下述数据结构、RPC、租约、超时及兼容策略均为 **Agent Self-Claimed 实现方案**，不是额外已批准产品规则。

## 1. 基线与范围

Asset PR #27 已于 2026-10-16 合入上游 dev（`b2a7399`）；从该提交建立 `backend/feat-moderation-review`。旧 Content `ApplyAssetReview` 仅是受信决定应用端，不是案件系统。当前切片只处理任务及其 human/agent 初始作品整体送审；不实现独立追加投稿、举报、审计平台、自动审核/病毒扫描、Voting、Discovery、Challenge 完整联调、前端或公网部署。

Content 独占草稿、不可变送审快照、投稿状态和展示状态；Moderation 独占案件、管理员决定、投递状态与 Content 执行回执；Asset 只拥有文件、归属和可用状态，**不增加审核状态**。各服务独立 schema、owner/runtime；无跨 schema 查询/FK，无数据库事务内 RPC。

## 2. 送审与修改规则

```mermaid
stateDiagram-v2
    draft --> pending_review: 完整双方＋expectedRevision＋资产复核
    pending_review --> rejected: Content 应用驳回
    rejected --> draft: expectedRevision 整体替换，revision+1
    draft --> pending_review: 新版本，新的 review ID/快照
    pending_review --> published: Content 原子批准任务及初始作品
    published --> taken_down: 既有受信收紧协议
```

- 草稿允许不齐双方；送审必须至少一件 human 和一件 agent，沿用作品来源/显式 cloudUse 约束；cloudUse=false 不阻止展示，不能要求云端授权来送审。
- 送审不递增内容 revision；以 `(task_id, revision)` 唯一。重复送审找回该版本的快照和当前审核结果，包括已驳回后工作副本已变动的旧版本重试，不新建案件。
- pending_review 不允许编辑：不另留可编辑工作副本；快照绝不可修改。驳回后 PUT 产生新 revision 并回 draft，再显式 POST 送审；旧快照和案件保留。published/taken_down 不通过此入口编辑或重提。
- 送审前在事务外读取版本并复核所有 file 资产归属/ready；事务内重新锁定作者行并核对 revision/状态，写快照、pending 状态及 outbox。验证期间发生编辑必须 409，而不能冻结未复核的新内容。
- 已 ready 资产的内容不可变。没有资产删除 API；未来引入资产删除时需另外协调引用，不能把当前复核解释为永久权限。

## 3. 权威记录与可靠投递

Content 新增迁移 `004_task_reviews.sql`（不改 001～003）：task_drafts 状态扩展；`review_requests` 保存完整快照、预分配稳定 entry ID、当前审核结果；`review_outbox` 保存待建案消息、尝试次数/租约；`review_decision_receipts` 保存决定请求摘要和稳定执行回执。快照/作者/版本不能由 runtime 更新。

Moderation 独立迁移：`reviews` 以 review ID 及 `(task_id, submission_revision)` 唯一建案，保存不可变 Content 快照、决定、管理员/时间、Content 回执；`decision_outbox` 保存待应用意图及租约。owner 迁移校验和保护，runtime 仅必要列权限，不 DDL、不跨库。

双方用单条 `UPDATE ... FROM (SELECT ... FOR UPDATE SKIP LOCKED)` 领取，持久化随机 lease token 和到期时间；提交领取后再 RPC。只持当前 token 的 worker 可以确认投递，超时或崩溃由到期租约恢复。RPC 关闭自动重试；后台用同一消息/决定 ID 至少一次重试。响应丢失由接收方唯一约束/回执找回，不重建案件或重复应用。

建案请求只接受 mTLS content，载荷来自已持久化快照。管理员决定先锁案件，保存唯一决定及 outbox，不在事务里调用 Content。相同 decision/reason 重试返回同一案件，不重写原 reviewer；不同决定/理由 409。Content 核对 review ID、task ID、submission revision、当前 pending 状态，并在同事务应用状态/展示和回执。旧决定精确重放只返回旧回执，不重新打开资源。不可恢复的版本冲突标记 blocked；不能谎称已生效。

批准理由可选，提供时原样保存于 Moderation 并随 outbox 传给 Content，参与决定幂等比较及执行回执摘要；缺省/空理由仍兼容。驳回理由仍必填且不能全为空白。两种理由均要求有效 UTF-8、无 NUL、最多 4000 **UTF-8 字节**（不是 4000 个字符），不截断。Content 草稿的 `rejectionReason` 仍只用于驳回，不以批准理由冒充驳回状态。新增 `002_approval_reason.sql` 替换理由 CHECK，保留 001 及校验和；先迁移再滚动服务，旧案件/理由不改写。002 后就绪才放行。以上为 **Agent Self-Claimed** 兼容实现。

## 4. HTTP / RPC 契约

| HTTP | RPC / 调用方 | 身份、幂等、超时 |
| --- | --- | --- |
| POST `/api/me/tasks/{taskId}/review-requests` | Content.RequestTaskReview / gateway | 网站＋Origin/CSRF，`{expectedRevision}`，task+revision 自然幂等；5 秒 |
| 内部建案 | Moderation.CreateReviewCase / content | mTLS content；review ID＋task/revision，同 ID 异快照409；5秒 |
| GET `/api/admin/reviews?status=pending&limit=20&cursor=...` | Moderation.ListReviews / gateway | 管理员网站断言，limit1～100；只返回初始任务案件；5秒 |
| GET `/api/admin/reviews/{reviewId}` | Moderation.GetReview / gateway | 管理员网站断言；2秒 |
| POST `/api/admin/reviews/{reviewId}/decision` | Moderation.DecideReview / gateway | 管理员网站＋CSRF，approve可选理由、reject必填非空理由；2秒持久化；待投递202，确认应用后200 |
| 内部决定应用 | Content.ApplyReviewDecision / moderation | mTLS moderation；不可变 decision ID/哈希，确切送审版本；5秒 |
| 内部快照读取 | Content.GetReviewSnapshot / moderation | mTLS moderation，只读已送审版本；2秒 |
| GET `/api/admin/reviews/{reviewId}/assets/{assetId}/content` | Asset.DownloadReviewAsset → Content.CheckReviewAssetAccess | gateway mTLS＋Asset方法专用管理员网站断言；Content只查确切 pending 快照引用，不回调Asset；流15分钟，权限子预算2秒 |

现有 Review `status` 保持 pending/approved/rejected，只有 Content 回执确认后才从 pending 变终态；增加 `decision`、`deliveryState` 和 `applicationRevision` 区分未决定/待投递/已应用/冲突。不把内部 outbox 状态冒充审核完成。GET 读取无写副作用。列表固定 created_at DESC,id DESC，游标带版本和管理员/有效status scope，不是授权凭据；重复/未知参数、损坏/过长游标400，空cursor第一页，末页null。不查totalCount，完整快照按Proto字节预算分页。

未登录401，非管理员/MCP/CSRF403；本人资源越权与不存在统一404；非法输入400；缺双方422；版本/状态/幂等冲突409；依赖不可用503。服务与方法白名单默认拒绝，Identity每次复核当前网站会话/角色。请求不能指定author/owner/role/reviewer。

待审文件必须同时满足当前管理员会话、确切review/asset绑定、仍pending和Asset ready。打开对象后再次查权限，再发送首帧；不发永久URL，不走所有者绕过，不缓存授权。审核后从待审入口拒绝；已批准文件走既有公开入口。已发出的字节无法收回。

## 5. ApplyAssetReview 兼容过渡

保留所有既有Proto字段/编号、旧决定回执和已审核快照；不修改003迁移。正式送审过的任务不允许通过旧RPC首次批准或重新批准，防止绕过案件/旧决定恢复展示；已有精确回执仍可找回，收紧（下架/撤销）仍按原revision和来源版本执行。旧3C任务可继续旧协议维护；旧RPC首次批准未送审草稿现在返回409 formal_review_required，原3C首次批准测试改为明确的旧已部署快照/回执升级夹具，正式新批准由独立的真实Moderation测试覆盖。但不能通过新送审入口把已展示任务当成新草稿复活。新批准只由 ApplyReviewDecision 复制**已送审快照**，而不是临时读取当前草稿。旧客户端/旧服务滚动期间不双发决定；先迁移和授权，再滚动Content，再启Moderation投递。

## 6. 失败与恢复矩阵

| 故障 | 持久事实 / 恢复 |
| --- | --- |
| Asset复核失败 | 保留原草稿，无快照/outbox |
| Content提交后HTTP丢失 | 原版本重试找回review ID |
| Moderation停机 | Gateway保持就绪，登录/草稿/上传继续可用，审核列表/详情/决定503；Content送审保持pending，outbox租约后重试 |
| 建案成功响应丢失 | 相同快照重试找回同一案件 |
| 两管理员并发相反决定 | 案件行锁；恰一个决定，另一个409 |
| 决定持久化后Content停机 | deliveryState=pending；不称已批准；恢复重投 |
| Content应用成功回执丢失 | 同decision ID找回同一回执，无重复应用 |
| 领取者崩溃 | 租约到期由其他副本接管；旧token不能确认新领取 |
| 驳回后新版本送审，旧决定迟到 | 精确回执只查回，异请求/错版本409，不影响新版本 |
| 批准后下架，批准重放 | 原回执，不恢复展示；新下载404 |
| Identity/Content权限检查不可用 | 文件/审核接口拒绝，不用缓存放行 |

## 7. 部署和验收边界

`cmd/moderation` 独立进程（非platform），MODERATION_DATABASE_URL_FILE owner/runtime、IDENTITY_TARGET、CONTENT_TARGET、mTLS moderation证书；gateway与Content配置MODERATION_TARGET。Content outbox与Moderation决定worker各随两个副本运行。readiness不将互相业务投递形成依赖环：Content不依赖Moderation健康；Moderation就绪依赖本地DB及Identity连接，Content不可用时保留待办。kind注册Moderation依赖Identity/Content，gateway再依赖Moderation；NetworkPolicy显式开放Content↔Moderation、gateway→Moderation、Asset→Content、Moderation→Identity。不开通Moderation直读Asset数据库。

**Agent Self-Claimed 故障隔离修正（2026-10-17）**：上述 catalog 依赖是部署排序，不等于请求期全局 readiness 依赖。Gateway 不因 Moderation 不健康退出 Service endpoints，只由相应审核 RPC 返回 503；Content 送审只依赖本地事务及必要身份/资产复核，不等待 Moderation。没有放宽 Identity、Content、Asset 的既有就绪策略，也不缓存审核授权。验收通过一次性私有 NodePort → 正常 Service → ready EndpointSlices，停机后等待探针窗口、核对两个 Gateway ready endpoint，并用新 TCP 连接验证登录发起/会话/草稿/实际上传完成/送审和审核503；不能用 port-forward 代替该故障隔离验收。Content 停机的回执恢复子测试仍使用诊断 port-forward，不声明 Content 故障时全局 Gateway 可用。

隔离 validator 为 Identity 注入一次性合成 OAuth client 配置，仅检查停机前后的登录流程落库/302 授权跳转，不跟随跳转、不调用 Google 换码，不读取实际 OAuth 凭据，也不把它称为真人 Google 登录成功。初次新增验收因原 fixture 未配置 OAuth 在该断言返回503；保留失败记录，补齐上述前置条件后重跑，不降低302判据。

先跑专用PostgreSQL＋正式mTLS＋真实Identity/HTTP集成，再运行一次性namespace内**真实Moderation Pod**的kind验收；测试证书直调不能替代。验收覆盖本轮4D矩阵、租约过期、丢响应、停机恢复、角色/CSRF、待审文件隔离、运行账号跨schema/DDL拒绝。

4A～4D 已完成本地实现与验证；本地验证不等于已合入、远端 CI、生产部署或公网发布。公开任务/作品详情不在本切片；最多声明“审核及过审文件链路完成”，不能声明“公开内容浏览完成”。

## 8. 技术参考（Agent Self-Claimed）

- [PostgreSQL 18 SELECT](https://www.postgresql.org/docs/18/sql-select.html) 官方文档的 SKIP LOCKED queue 用法；仅用于领取，不用于用户一致读取。
- [Transactional Outbox](https://microservices.io/patterns/data/transactional-outbox.html) / [Idempotent Consumer](https://microservices.io/patterns/communication-style/idempotent-consumer.html) 模式：业务状态和待发送记录同事务，接收方仍必须去重；不是恰好一次投递，不引入消息中间件。

## 9. 阶段 4A～4D 实际验收记录（2026-10-16）

以下是本地实际结果；新增数据模型、租约、超时、测试组织和 kind 验证方式均为 **Agent Self-Claimed** 实现方案：

| 验收层 | 结果 | 证据 |
| --- | --- | --- |
| Proto / 生成 | PASS | `buf lint`、`buf generate` 均退出 0；独立临时目录重新生成与 `backend/gen` 差异退出 0；`buf breaking --against upstream/dev` 退出 0。 |
| Go 单元与构建 | PASS | `gofmt -l cmd internal`、`go vet ./...`、`go build ./cmd/...` 均退出 0；普通测试 134 项通过，race 测试 134 项通过。 |
| 真实 PG/S3/mTLS/HTTP | PASS | 临时 PostgreSQL 18 与 SeaweedFS 4.13；最终全量集成退出 0，250 项通过、无 skip/fail；Moderation 专项 45 项通过。凭据和容器已清理。 |
| 真实 Moderation kind | PASS | `python3 -u ops/lab/validate_modules.py` 退出 0；一次性 namespace 已清理；真实 Moderation 两副本通过送审、停机恢复、管理员待审文件读取、驳回重提、并发决定、回执重放、过审文件读取。 |
| 网络与部署控制 | PASS | kind 网络探针、失败 rollout 回滚、迁移失败重试、重复完整部署均通过；共享命名空间资源快照未被验证过程改写。 |
| 根目录检查 | PASS | `npm run check`、`npm run build:docs`、`npm test`、Python unittest（27 项）、`npm run ci`、`git diff --check` 均退出 0。 |

### 证据边界与剩余事项

- 已验证的是本地一次性 kind lab、临时真实 PostgreSQL/S3、真实 mTLS 和真实 Moderation Pod；没有执行公网部署，也没有声称远端 GitHub CI 已通过。
- 以上为修复前的历史验收：2026-10-17 定向审查发现 Gateway 故障隔离和批准理由兼容缺口，原 port-forward 验收不能排除前者。是否可提交审查以本次修复后的新增验证记录为准。当前工作区仍未 commit/push/创建 PR；正式发布状态仍是 `unpublished`。
- 公开任务/作品详情、追加投稿、举报/审计平台、自动审核/病毒扫描、Voting、Discovery、Challenge 完整业务联调和生产对象存储 HA 不在本阶段。

## 10. 两项审查问题修复与重新验收（2026-10-17）

**Agent Self-Claimed 实现与测试方案**：移除 Gateway 对 Moderation 的全局 readiness 健康检查，保留审核 RPC fail closed；支持并持久化批准理由，Content 同步验证并纳入回执摘要；新增 Moderation 002 迁移（不改001）及带旧案件升级测试。新增 UTF-8/NUL/4000字节边界、批准理由回读/原样重试/异理由409测试。顺带修正既有 Asset 集成夹具浅拷贝 Proto mutex，使用 `proto.Clone`，无业务行为改动。

证据目录：`/tmp/hw-moderation-fix-20261017/`。以下命令均已退出：

| 命令 / 范围 | 退出码与结果 |
| --- | --- |
| `buf lint`；`buf generate`（正式buf.gen.yaml） | 各0；版本固定的go run插件，正常本地Go缓存；未用临时生成插件 |
| 独立临时输出 `buf generate --output …` 后 `diff -qr gen …/gen` | 0，重新生成完全一致；本次修复未改Proto字段/编号 |
| `buf breaking --against '../.git#ref=HEAD,subdir=backend'` | 0；兼容基线为当前HEAD，不冒称远端CI |
| `git diff --exit-code -- gen` | **1**：整个阶段4的生成代码仍未提交、相对HEAD有预期增量；这不是独立重新生成不一致 |
| `gofmt -l cmd internal`；`go vet ./...`；`go build ./cmd/...` | 各0；gofmt输出为空 |
| `go test -json ./... -count=1`；`go test -race -json ./... -count=1` | 各0；各135测试/子测试通过，无skip/fail |
| `go vet -tags=integration ./...`；`go vet -tags=lab ./internal/identity` | 修正上述既有夹具copylock后各0；保留初次失败记录 |
| 真实临时PG18/S3：`go test -race -json -tags=integration -p=1 ./... -count=1 -timeout=120s` | 0；251测试/子测试通过，无skip/fail；夹具修正后完整重跑同样通过 |
| 真实审核专项：`go test -race -json -tags=integration ./internal/identity -run '^TestModeration(HTTPOutbox\|IdentityAuthorization\|ApprovalMigration)$' -count=3 -timeout=120s` | 0；48测试/子测试通过，无skip/fail；覆盖旧理由约束复现→002升级、历史保留、重复迁移及新约束 |
| `npm run ci`（check/build:docs/Node test/Python unittest） | 0；5个Node、27个Python测试通过 |
| `python3 -m py_compile ops/lab/validate_modules.py`；OpenAPI YAML解析；`git diff --check` | 各0 |
| `sg docker -c 'umask 022; DOCKER_CONFIG="$HOME/.config/human-worth/lab/docker" python3 -u ops/lab/validate_modules.py'` | **0**；4项真实kind业务测试通过，网络允许/拒绝、失败rollout回退、失败迁移同SHA重试及重复完整部署通过 |

最终 kind namespace `human-worth-verify-c778d351` 已删除。Moderation 专项62.59秒：停掉两个真实 Moderation Pod，等待15秒探针窗口，确认两个 Gateway ready endpoints，再用每请求新TCP的 HTTPS NodePort/Service 链路检查会话/草稿/完整文件上传/登录发起/持久送审成功、审核列表503；恢复后自动投递，带中文批准理由完成审核、回读与幂等重试，过审文件读取成功。Content停机子测试仍是诊断port-forward，不扩大声明。首次kind因缺合成OAuth配置退出1，记录保留于 `kind-attempt1.log`；补齐隔离前置条件后完整重跑退出0，未掩盖失败。

本地必要验证为 **PASS，可提交复审**。没有使用临时GOCACHE（`/home/cherry/.cache/go-build`）；临时测试数据库/S3容器、凭据、namespace均已清理，未改产品数据。未commit/push/创建PR、未部署公网，未进行真人Google登录或新增公网验收，生产S3 HA/远端GitHub CI仍不在本轮证据内。
