# API 迁移映射表（FastAPI → gin）

## 0. 装配差异（最容易出错的地方）

| 项 | FastAPI | gin 注意点 |
|---|---|---|
| 全局前缀 | 每个 `include_router` 上显式写 `/api/v1` | 用 `r.Group("/api/v1")` 统一，注意 `characters.py:68-69` 有两条路径注册同一 handler |
| 中间件顺序 | `add_middleware` 是**头插**，注册顺序与执行顺序相反 | `Use()` 是尾插（先注册先执行）。最终顺序须为 `SPA no-cache → 请求日志 → CORS → 异常处理 → 路由` |
| 异常处理 | `register_error_handlers` 必须在 CORS **之前**调用（500 兜底绕过 CORS，handler 内手工补头） | gin 的 `Recovery()` 放最外层，自定义错误渲染在业务层统一出口 |
| 404/405 | Starlette 默认 `{"detail":"Not Found"}` / `"Method Not Allowed"` | gin 需 `NoRoute`/`NoMethod` 自行输出同形状 |
| 422 | FastAPI `RequestValidationError` 未注册 handler，输出 `{"detail":[{"loc":[],"msg":"","type":""}]}` | 需自行构造等价结构 |
| 路由冲突 | `/projects/{name}/script-scenes/{id}` 与 `/projects/{name}/scenes/{name}` 争抢（注释见 `projects.py:1050-1052`） | gin httprouter 对同前缀下「静态段 vs 参数段」有严格规则，**必须逐条验证 `/projects/:name/...` 下约 30 条路由** |
| 通配符 | `/files/{project}/{path:path}`、`/prompts/registry/{template_path:path}` | gin 的 `*param`（注意 `*param` 不能单独出现在中间段） |
| 非标准分段 | `/drafts/{episode}/step{step_num}`（`files.py:759`） | gin 无「字面量+参数」混合段，需改成 `/drafts/:episode/:step`（前端同步改）或手写兜底路由 |
| 同步 handler | `projects.py:484` 导出剪映草稿是同步 `def`（FastAPI 跑线程池） | Go 里直接同步执行，注意长耗时阻塞与超时 |
| SSE 鉴权 | `CurrentUserFlexible` 支持 `?token=` query | SSE 路由需单独的鉴权中间件 |

## 1. 生命周期（lifespan）等价物

启动顺序（`app.py:350-506`，**顺序不可换**）：
`assert_no_provider_secrets_in_environ` → `check_sandbox_available` → `detect_docker` → 日志挂载 →
`ensure_auth_password` → `init_db`（迁移）→ 默认用户/系统用户 → 官方 skill 同步 →
项目编码迁移 → 项目迁移 + 备份清理 → transcript 迁移 → JSON 配置迁 DB → profile 同步 →
HTTP client → AssistantService + SessionManager patrol → **GenerationWorker（`set_worker_cancel_callback` 必须在 `start()` 之前）** →
ProjectEventService → PublishingWorker → RenderWorker → WorkflowExecutor。

关停顺序：WorkflowExecutor → RenderWorker → PublishingWorker → ProjectEventService → GenerationWorker → HTTP client → DB。

Go 侧用 `sync.WaitGroup` + `context.Context` 实现，`app.state` 上的 6 个单例（`generation_worker` / `project_event_service` / `publishing_worker` / `render_worker` / `in_docker` / `sandbox_enabled`）挂在 `app.Runtime`。

## 2. 端点清单

认证列：**A** = Bearer JWT/API Key；**P** = 公开；**S** = 自带鉴权（`?token=` 或短时效下载 token）；**Adm** = 管理员。
路径均已含 router 自身 prefix，注册时统一加 `/api/v1`。

### 2.1 认证与用户

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/auth/status` | P | 返回 `AUTH_ENABLED` |
| POST | `/auth/token` | P | OAuth2 表单登录 → JWT（7 天） |
| POST | `/auth/logout` | A | 仅记日志（JWT 无状态） |
| GET | `/auth/verify` | A | 校验 token |
| GET | `/auth/github/config` | P | GitHub OAuth 是否配置 |
| GET | `/auth/github/authorize` | P | 生成 state（TTL 300s）后 302 |
| GET | `/auth/github/callback` | P | 换 token → 取用户 → 签 JWT → 302 `/app/projects#token=` |
| POST | `/api-keys` | A | 创建（`shotwise-<32hex>`，默认 30 天，明文仅此一次返回） |
| GET | `/api-keys` | A | 列表 |
| DELETE | `/api-keys/{key_id}` | A | 吊销（先清缓存再删库） |

`api_keys` 额外守卫：API Key 自身调用 API Key 管理端点 → 403。

### 2.2 项目与资产

| 方法 | 路径 | 认证 |
|---|---|---|
| GET/POST | `/projects` | A |
| GET/PATCH/DELETE | `/projects/{name}` | A |
| POST | `/projects/import`、`/projects/import/preflight` | A |
| POST | `/projects/{name}/export/token` | A（签发 300s 下载 token） |
| GET | `/projects/{name}/export`、`/export/jianying-draft` | **S** |
| GET | `/projects/{name}/video-capabilities` | A |
| GET/PATCH | `/projects/{name}/scripts/{script_file}` | A |
| PATCH | `/projects/{name}/script-scenes/{scene_id}`、`/script-shots/{shot_id}` | A |
| POST | `/projects/{name}/script-shots/reorder` | A |
| DELETE | `/projects/{name}/episodes/{episode}` | A |
| PATCH | `/projects/{name}/episodes/{episode}`、`/episodes/{episode}/display-number`、`/segments/{segment_id}` | A |
| POST | `/projects/{name}/source`、`/generate-overview` | A |
| PATCH | `/projects/{name}/overview` | A |
| GET/POST/PATCH/DELETE | `/assets`、`/assets/{asset_id}` | A |
| POST | `/assets/{asset_id}/image`、`/assets/from-project`、`/assets/apply-to-project` | A |
| GET/PUT | `/projects/{name}/character-relations`、POST `/analyze` | A |
| GET | `/projects/{name}/cost-estimate` | A |
| GET | `/projects/{name}/versions/{resource_type}/{resource_id}`、POST `.../restore/{version}` | A |

**资产工厂**（`_asset_router_factory.build_asset_router`）生成 4 类 × 4 端点，
`{subdir}` ∈ `characters`/`scenes`/`props`/`products`：
`POST /projects/{name}/{subdir}`、`PATCH .../{entry}`、`POST .../{entry}/rename`（支持 `dry_run`）、`DELETE .../{entry}`。

`characters.py` 额外 5 个：`POST /{char}/avatar`、`GET|POST /{char}/variants`、`PATCH|DELETE /{char}/variants/{id}`。

### 2.3 生成与队列

| 端点组 | 数量 | 路径前缀 |
|---|---|---|
| `generate.py` | 14 | `/projects/{name}/generate/{storyboard,video,tts,character,scene,prop,product}/{id}`、`/canvas-images/{split,advanced}`、`/edit/image`、`/audio-backend/voices`、`/characters/{n}/voice-sample[ /confirm]` |
| `batches.py` | 4 | `/projects/{name}/batches[/{id}[/cancel,/retry-failed]]` |
| `tasks.py` | 8 | `/tasks`、`/tasks/stats`、`/tasks/{id}`、`/tasks/{id}/cancel[-preview]`、`/projects/{name}/tasks[ /cancel-all[-preview]]` |
| `queue_observability.py` | 2 | `/queue/observability`、`/tasks/{id}/queue-position` |
| `grids.py` | 7 | `/projects/{name}/grids[/{id}[/regenerate,/split,/upload]]`、`/generate/grid/{episode}`、`/grid-capability` |
| `end_frames.py` | 3 | `/projects/{name}/shots/{id}/end-frame[ /upload,/select]` |
| `shot_uploads.py` | 1 | `/projects/{name}/shots/{id}/upload/{kind}`（`kind ∈ storyboard,video`） |
| `episode_duration.py` | 5 | `/projects/{name}/episodes/{ep}/duration-plan[...]` |
| `usage.py` | 3 | `/usage/stats`、`/usage/calls`、`/usage/projects`（**Adm**） |

### 2.4 助手与 Agent

挂载前缀 `/api/v1/projects/{project_name}/assistant`：

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| POST | `/sessions/send` | A | 发消息（无 id 则新建） |
| POST | `/sessions/{id}/rewrite` | A | 改写 → 分叉新会话重跑 |
| GET | `/sessions`、`/sessions/{id}` | A | 列表 / 详情 |
| DELETE | `/sessions/{id}` | A | 删除 |
| GET | `/sessions/{id}/messages`、`/snapshot`、`/stream` | A | **已下线 → 恒 410** |
| GET | `/sessions/{id}/entries` | A | 冷读（`?after=<seq>`） |
| GET | `/sessions/{id}/entries/stream` | **S** | **SSE entry 流** |
| POST | `/sessions/{id}/interrupt`、`/switch-agent` | A | |
| POST | `/sessions/{id}/questions/{qid}/answer` | A | HITL |
| GET | `/skills` | A | 项目可用 Skill |

其它：`POST /agent/chat`（同步封装，120s 超时）、`/agent/preset-providers`、`/agent/credentials*`（7 个）、`/agent/test-connection`。

### 2.5 文件与剧本

`files.py`（13 个）：`GET /files/{project}/{*path}`（**P**）、`GET /global-assets/{type}/{filename}`（**P**）、
`POST /projects/{n}/upload/{upload_type}`（8 种类型，规则表 `UPLOAD_SPECS`）、
`DELETE /projects/{n}/characters/{name}/reference-audio`、`GET /projects/{n}/files`、
`GET|PUT|DELETE /projects/{n}/source/{filename}`、`GET|PUT|DELETE /projects/{n}/drafts/{ep}/step{n}`、
`POST /projects/{n}/style-image`。

`script_review.py`（4）、`draft_promotions.py`（9）、`creative.py`（1）、`prompt_registry.py`（2）。

### 2.6 供应商与配置

- `providers.py`（12）：`/providers[/{id}[/config,/model-types,/credentials[/{cid}[/activate]],/test]]`、`/providers/gemini-vertex/credentials/upload`
- `custom_providers.py`（16）：`/custom-providers[...]`、`/endpoints`、`/discover*`、`/test`
- `system_config.py`（4）：`/system/config[ /model-candidates]`、`/system/version`；`system.py`（1，`Adm`）：`/system/logs/download`
- `features.py`（3）、`onboarding.py`（2）

### 2.7 媒体装配 / 渲染 / 发布

- `media_assembly.py`（8）：`/projects/{n}/assembly-plans`、`/assembly-plans/{id}[/revisions,/status,/preview-confirm,/render-confirm,/stale-check]`
- `media_rendering.py`（14）：`/assembly-plans/{id}/{preview-renders,final-renders,subtitles,final-review}`、`/render-jobs/{id}[/retry,/final-retry,/cancel,/artifact]`、`/render-artifacts/{id}[/review-confirm,/review-frames/{pos}]`
- `media_assets.py`（9，feature gate `media_library`）
- `publishing.py`（11）：`/publishing/{platforms,accounts}`、`/render-artifacts/{id}/publish`、`/publish-jobs[/{id}[/retry,/cancel,/poll,/retract]]`
- `jianying_assembly.py`（1）

### 2.8 记忆 / 创意板 / 创作计划 / Workflow

- `memories.py`（15）：`/memories/user`、`/projects/{n}/memories`、`/memories/{id}`、`/memory-candidates[/{cid}/accept,/reject]`、`/memories/export|import`
- `creative_boards.py`（16，gate `creative_board`）
- `creative_context.py`（2，gate `context_agent`）
- `creation_plans.py`（13，gate `creation_plan` + `official_creation_skills`）
- `workflows.py`（**41，最重**）：`/workflows[/{id}[/revisions,/export,/template-upgrade]]`、`/workflow-revisions/{id}[/validate,/publish,/runs]`、`/workflow-runs/{id}[/start,/pause,/resume,/cancel,/retry,/budget/*,/patch/*,/nodes/{key}/log]`、`/workflow-templates[...]`（含 `/admin/...` 审核队列）
- `reference_videos.py`（10）

## 3. SSE（3 条真实通道）

| 路径 | 认证 | 说明 |
|---|---|---|
| `/projects/{name}/events/stream` | S | 项目变更流 |
| `/projects/{name}/assistant/sessions/{id}/entries/stream` | S | 助手 entry 流 |
| `/projects/{name}/assistant/sessions/{id}/stream` | S | 已下线，恒 410 |

任务/渲染/发布进度**没有独立 SSE**，统一汇入项目变更流。

### 3.1 项目变更流事件

```
event: snapshot        首帧必发   {project_name, fingerprint, generated_at}
event: changes         增量       {project_name, batch_id, fingerprint, generated_at,
                                   source: worker|webui|filesystem,
                                   changes: [{entity_type, action, entity_id, label,
                                              focus: {pane, episode?, anchor_type, anchor_id}|null,
                                              important, script_file?, episode?}]}
event: project_deleted 终止       {project_name}     （流随后结束）
```
心跳 1s（`{"type":"_idle"}` 内部哨兵不下发）；溢出策略 `DropSubscriber`（队列满静默移除）。

### 3.2 助手 entry 流事件

| event | SSE id | data |
|---|---|---|
| `entry` | **`str(seq)`** | 完整 entry（`seq`/`type`/`content`/`parent_tool_use_id`…） |
| `draft` | — | `{session_id, **draft_state}`（含 `draft_rev`） |
| `question` | — | `{**question, session_id}` |
| `delta` | — | message 去掉 `type` 后的字段 |
| `status` | — | 终态 `idle/completed/error/interrupted` |

序列协议：
```
非 running: entry×N(after_seq 之后) → status(终态) → 结束
running:    entry×N → draft → question×N → [status 若已终态] → 直播 entry/delta/question/status
```
游标优先级：`Last-Event-ID` header > `?after=<seq>`。
**关键**：`result` 消息要暂存，等 `log_turn_complete`（末条条目已广播）才出终态，否则丢末条；心跳兜底 `drain_beats >= 2`。

## 4. 统一错误响应

所有错误统一 `{"detail": ...}`，`detail` 是**已翻译字符串**或 `{"code": ..., ...}` 对象。

| 异常 | 状态码 | detail |
|---|---|---|
| `ApiError` 家族（BadRequest/NotFound/Conflict/ServiceUnavailable） | 400/404/409/503 | `_t(key, **params)` |
| `TaskSpecValidationError` | 400 | `_t(code, **params)` |
| `ScriptEditError` | 400 | `{"detail": _t("script_data_corrupted", reason=...)}` |
| `FileNotFoundError` | 404 | `_t("resource_not_found")`（**不回传路径**） |
| 兜底 | 500 | `_t("internal_server_error")` + 手工补 CORS |

常见业务对象形状（router 直接抛）：

| 场景 | 码 | detail |
|---|---|---|
| feature 未开启 | 404 | `{"code":"feature_disabled","feature":"<name>"}` |
| 创意板版本冲突 | 409 | `{"code":"creative_board_revision_conflict","current_revision":n,"current_updated_at":ISO}` |
| 媒体资产仍被引用 | 409 | `{"code":"media_asset_still_referenced","references":[...]}` |
| 装配计划校验失败 | 422 | `{"code":"invalid_assembly_plan","errors":[...]}` |
| 分集时长版本冲突 | 409 | `{"code":"episode_duration_revision_conflict","message":..,"actual_revision":..}` |
| Agent 启动失败 | 502 | `{"code":"agent_startup_failed","message":..,"failure":{...}}` |

401 一律带 `WWW-Authenticate: Bearer`。

## 5. 鉴权实现要点

- JWT：HS256，`{sub, iat, exp}`，7 天；密钥 = `AUTH_TOKEN_SECRET` 或运行时随机。
- 下载 token：`{sub, project, purpose:"download", user_id?}`，**300s**，校验 `purpose` 与 `project` 匹配。
- API Key：`shotwise-<32hex>`，SHA-256 后查表；LRU 缓存 ≤512 条 / TTL 300s。
- 匿名模式：`AUTH_ENABLED=false/0/no/off` 全放行，返回固定 `admin`。
- 密码哈希 bcrypt；用户名比对用恒定时间。
- 用户角色查库失败 → 降级 `user`（fail-closed，不静默给 admin）。
