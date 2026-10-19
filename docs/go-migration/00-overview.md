# SHOTWISE 后端 Go 化迁移总纲

> 目标：把 Python（FastAPI + SQLAlchemy + Claude Agent SDK）后端全量重写为 Go（gin + GORM + Eino）。
> 前端保持不动，API 契约逐条对齐；迁移期间 Python 与 Go 可并存，按路由组灰度切换。

## 1. 现状规模

| 项 | 量 |
|---|---|
| Python 代码 | `server/` 5.8 万行（152 文件）+ `lib/` 6.7 万行（278 文件） |
| HTTP 端点 | 344 个（45 个 router 文件 + 资产工厂 4 类型 × 4 端点）——**已迁 281 / 未迁 63**（2026-10-19 实测，`server-go/scripts/audit_endpoints.py`，见 §4.1） |
| DB 表 | 53 张业务表（Alembic 78 个迁移，3 个 merge 点） |
| 供应商 backend | 视频 11 个类 / 图片 9 个类 / 音频 2 个类 + 自定义供应商声明式端点 |
| 智能体工具 | 进程内 MCP 37 个（32 对外 + 5 内部） |
| Skill / Subagent | 11 个 Skill（含 1 个 content_mode 变体）/ 6 个 Subagent |

## 2. 三个已定决策

1. **范围**：全量重写为 Go 服务（不是绞杀者渐进）。
2. **智能体层**：完全用 Eino 替换 Claude Agent SDK（不再起 `claude.exe` 子进程）。
3. **HTTP 层**：gin 替换 FastAPI。

## 3. 目录布局

Go 服务放在 `server-go/`（与 `server/` 并存直到切换完成），模块路径 `github.com/shotwise/shotwise`。

```
server-go/
├── cmd/
│   ├── shotwised/main.go        # 服务入口：装配 → 监听 → 优雅关停
│   └── migrate/main.go          # 迁移 CLI（up / down / status）
├── internal/
│   ├── app/                     # gin 装配：中间件顺序、路由注册、生命周期
│   ├── config/                  # 配置：env > DB；provider 目录与四级解析
│   ├── i18n/                    # zh/en/vi 三语，key + params 间接层
│   ├── apperr/                  # 统一错误码 → {"detail": ...}
│   ├── domain/                  # 领域模型：project.json、剧集、资产、宫格
│   ├── infra/
│   │   ├── db/                  # 连接池、方言适配、迁移执行器
│   │   ├── fs/                  # path safety、原子 JSON、文件锁
│   │   ├── ffmpeg/              # ffmpeg/ffprobe 执行器（exec.CommandContext）
│   │   ├── img/                 # 图像处理（替代 PIL）
│   │   └── httpc/               # 出网 HTTP（错误分类：未送达 vs 已送达）
│   ├── repository/              # GORM 仓储
│   ├── service/                 # 业务服务（对齐 server/services/）
│   ├── transport/http/          # gin handler，按 router 分文件
│   ├── queue/                   # 任务队列 + worker（容量表/占用台账/熔断）
│   ├── providers/               # image / video / audio backend + registry
│   ├── pricing/                 # 10 种定价策略
│   └── agent/                   # Eino 智能体运行时
│       ├── runtime/             # 会话管理、TurnLoop、并发上限、驱逐
│       ├── tools/               # 37 个工具（tool.InvokableTool）
│       ├── policy/              # AgentAccessPolicy（读写围栏、Bash 白名单）
│       ├── skills/              # profile manifest 同步 + SKILL 变体 Backend
│       ├── sandbox/             # bwrap 包装 / Windows 白名单降级
│       ├── eventlog/            # append-only transcript + seq 游标
│       └── checkpoint/          # CheckPointStore SQL 实现 + 前缀分叉
├── migrations/                  # 0001_init.sql + 后续
└── web/                         # 前端 dist 挂载（开发期代理到 vite）
```

分层契约对应 Python 侧的 `lint-imports`：
`domain` ← `repository` ← `service` ← `transport`；`providers` 不得反向依赖 `service`。

## 4. 迁移阶段

| 阶段 | 内容 | 完成判据 | 状态 |
|---|---|---|---|
| **P0 骨架** | gin 装配、配置、日志、i18n、错误信封、健康检查、DB 连接与迁移执行器 | `go build ./...` 通过，`/health` 返回 200 | 完成 |
| **P1 数据层** | 53 表 GORM 模型 + 0001_init + repository；project.json / 剧集 JSON 读写与校验 | 能对既有 `projects/.shotwise.db` 读全表 | 完成（0001_init 基线 + 53 表模型 + 执行器；剧集 JSON 校验待随 P2 补） |
| **P2 领域与资产** | projects / characters / scenes / props / products / files / versions | 项目 CRUD + 资产 CRUD 冒烟通过 | 进行中（项目 CRUD 5 + 四类资产工厂 16 端点含级联改名已通；分集账本（episodes 账本字段 + planning_cursor + source_fingerprints）与源文解析已补；**versions 2 已迁**（版本列表 + 还原，含 asset sheet / 剧本 generated_assets / 宫格记录 / 视频缩略图的还原副作用）。**未迁（50 个）**：files 12、projects 其余 17（import/export/scripts/episodes/overview 等）、全局 assets 库 8、characters avatar/variants 6、character_relations 3、auth GitHub OAuth 3、cost_estimation 1） |
| **P3 供应商与计费** | registry、resolver 四级解析、backend 接口、pricing、api_calls | 一次真实图片生成成功并正确记账 | 进行中（**service 层完成**：内置 10 家 + 自定义声明式包装层 backend 全量注册；ImageGenerator 记账括号闭环、自定义供应商图片路线已通；文本后端已补最小实现 `internal/textgen`（gemini-aistudio 单家 + 分层解析，其余 provider 直接报错不回退）。**HTTP 管理面已迁 providers 12**（收敛版：目录读注册表、凭证 CRUD + 互斥激活、启用开关、OpenAI 兼容端点连接测试）；**未迁 custom_providers 15**（自定义供应商声明式端点，本轮范围外）） |
| **P4 生成流水线** | 队列去重、双通道 worker、容量表、熔断、租约、resume、ffmpeg | 批量生成 + 取消 + 断点续传冒烟 | 完成（HTTP 面 44/44 全迁；**service 层完成**：queue 全套单测绿；定价目录 `pricing.BuiltinCatalog` 按内置注册表移植（11 家 / 107 模型 / 10 种 kind，费率与 Python 逐项一致）；`TaskDispatcher` 覆盖 storyboard / design / tts / image_edit / grid / video / reference_video 全部 task_type；`cmd/shotwised` 装配 queue + worker 并**注入全部钩子**：终态→项目变更事件（含 asset_fingerprints，`NewTerminalEmitter`）、中断记账收口（`NewLedgerSettler`，启动收口 + 取消收口）、失败编码（`EncodeTaskFailure` / `EncodeTaskError`）、瞬态判定（`IsTransientTaskError`，熔断只计上游瞬态失败）、执行身份投影（`NewProviderExtractor`，三级回退且失败兜底不阻断认领）、执行前复检（`NewWorkerPreflight`）、resume 执行器（`NewResumeExecutor`，按提交时锁定身份轮询下载、绝不重新 submit）。端到端冒烟 `cmd/shotwised/assembly_smoke_test.go` 覆盖批量生成 + 终态事件广播 + 取消 + 记账收口 + 断点续传五条链路。**HTTP 面已全迁（44/44）**：generate 14（含 canvas-images 2）、tasks 8、grids 7、episode_duration 5、batches 4、end_frames 3、queue_observability 2、shot_uploads 1。**已知留白**：`MediaIndexer`（media_assets 登记）未接入——`WithMediaIndexer` 有定义但 `cmd/` 从未调用，属 media_* 长尾；变更事件文案的 zh/en/vi 三语化未做（按 Python 保持中文字面量）；grid 任务的 preflight 复检未纳入；**worker 侧 `canvas_image_*` 十个 task_type 尚未实现**（`TaskDispatcher` 显式报错 `unsupported task_type`，即画布端点可提交但落执行态即失败，需补 `tasks_canvas.go`）） |
| **P5 智能体** | Eino ChatModelAgent + GuardedTool + 37 工具 + SSE entry 流 | 一条完整会话跑通（含 Skill 调用与 AskUserQuestion） | 进行中（会话面已闭环：TurnLoop 驱动、guard middleware + AgentAccessPolicy（含 EnhancedInvokableToolCall 包裹）、ask_user HITL（checkpoint 落 SQL + 按 question_id 回答续跑）、事件日志落 `agent_session_event_log` 且 SSE `id: seq` + `Last-Event-ID` 续传、`entries` / `entries/stream` / `interrupt` / `questions/{id}/answer` 端点已接；37 个工具已全部实现：32 个对外工具（list_project_text_files / read_project_text / list_pending_assets / generate_assets / generate_storyboards / edit_images / generate_grid / generate_narration_audio / plan_episodes / reset_episode_planning / patch_project / get_video_capabilities / analyze_character_relations / inspect_director_review / patch_episode_script / insert_segment / remove_segment / split_segment / patch_episode_meta / rename_asset / split_narration_segments / normalize_drama_script / generate_episode_script / confirm_script_review / split_reference_video_units / open_reference_step1_for_edit / validate_and_promote_reference_draft / generate_video_episode / generate_video_scene / generate_video_all / generate_video_selected / propose_memory_candidate）与 5 个内部 timeline 工具（get_episode_media_manifest / create_timeline_plan / get_timeline_plan / update_timeline_plan / validate_timeline_plan）；剧本生成的 ad / reference_video / drama / narration 四条 step2 路径均已迁移。剧本写盘统一入口（锁 + 结构不更坏 + metadata 重算 + 分集镜像）、结构校验器、step1→step2 审核 gate（确认指纹落 `episodes[i].step1_review` + 读时派生状态）与参考路径正式 step1 单一写盘出口（per-path 锁 + OCC 基线比对 + 清 step2 草稿）已就位；skills / subagents / filesystem+沙箱 已装配；存量 step1 时长收编迁移（`step1_migration.go`）、参考音频「多段总时长」闸门、Web 保存 / 确认入口（ScriptReviewService + 4 个 step1-review gate 端点 + pending-authoring 端点，测试对齐 Python 侧三组用例）均已补齐。**已知未迁移项**：双模型通道未动——`NewChatModel` 仅 openai 单通道，`03-agent-runtime-eino.md` §15 的 claude↔openai 分派、`ModelRetryConfig` / `ModelFailoverConfig`、switch_agent 同构切换均未做；assistant 端点残缺——14 个中仅 6 个已实现且**挂错前缀**（现注册在 `/agent/sessions*`，Python 与前端 `frontend/src/api.ts` 的契约是 `/projects/{n}/assistant/sessions*`），send / rewrite / 删除 / 详情 / messages(410) / snapshot(410) / switch-agent / skills 8 个未接；agent_config 8 与 agent_chat 1 亦未迁（**P5 小计 23 个**）） |
| **P6 长尾 router** | workflows(41) / memories(15) / creative_boards(16) / publishing / media_* | 端点覆盖对齐清单 | 已完成（151/151 端点全覆盖：memories 15、media_assembly 8、creative_boards 16、publishing 11、media_assets 9、jianying_assembly 1、creative_context 2、media_rendering 14、reference_videos 10（10 端点全量：list / derive-units / add / patch / delete / reorder / duration-precheck / script-preview / generate(202) / upload-video；ad 派生索引与 narration/drama 自包含 unit 双口径、资产 preflight 硬/软分层、视频能力解析闸、上传落盘后绑定复核均带单测）、**workflows 41**、**creation_plans 13**。workflows：定义/版本/运行/事件日志 + 模板市场与审核 + 预算预留结算 + 智能体 patch，图哈希与 Python `canonical_json` 逐字节对齐（整数值浮点按 Python repr 补 `.0`，否则同一张图两套服务算出不同 graph_hash）；模板契约的安全校验（绝对路径 / URL / 父目录穿越 / 外链 `$ref`）同步迁入。creation_plans：官方 Skill 目录同步（启动期物化，失败不阻断）、计划编译/预览/启动/取消/作废/重编译/重启、兼容性事件与指标；启动路径需在同一事务内「建运行 → 跃迁 → 派发」，否则派发读不到未提交的运行行。官方 Skill 铸造的 revision 归属 `system`，启动官方计划时跳过 revision / 定义的归属与工作区校验（计划行本身仍按 user_id 收敛）。**draft_promotions 9 + prompt_registry 2 = 11**，至此 **151/151 全覆盖**。draft_promotions：Agent/Upload/Online-AI 三源草稿的隔离评审流（草稿只存评审记录，正式剧本仍是唯一内容真相源），域内含三方合并（已知集合按 scene_id/segment_id/shot_id/unit_id 稳定 ID 逐项合并、其余数组整体冲突、JSON Pointer 转义与冲突路径口径与 Python 逐字段对拍）、结构校验注入（`ValidateScriptStructure` 的错误逐条转 `script_structure_invalid`）、晋升是剧本锁内的 compare-and-write（revision = mtime_ns，与 Python `st_mtime_ns` 同口径；读必须先内容后 stat，否则存量迁移的写回会让基线 mtime 过期、晋升恒被判 stale；锁内必须走无锁读，否则撞存量迁移会二次取同一把锁自死锁）。草稿 store 落 `<数据目录>/.draft_promotions.json`，直存 `json.RawMessage`——过一遍 `map[string]any` 会把 int64 的 mtime 修订号（约 1.8e18）经 float64 抹掉精度（该量级 ULP 256ns），compare-and-write 必然误判。prompt_registry：`agent_runtime_profile` 的只读投影（system/agent/skill/reference 四组、frontmatter/标题/小节/mode 注释推导、CRLF 通用换行归一、码点截断），与 Python 实算的 23 条目录逐条对拍。错误信封统一为 `{"detail": "<翻译文案>"}`（ApiError 口径），三语补齐 `script_not_found` / `request_invalid` / `prompt_template_not_found`） |
| **P7 切换** | 前端指向 Go 服务，Python 服务下线 | 前端全流程回归通过 | 未开始（**前置条件未满足**：63 个端点缺失，前端核心流程——生成触发 / 任务面板 / 供应商配置 / 宫格 / 文件上传 / 费用统计——依赖这些端点，`frontend/src/api.ts` 实际在调；另需先修 assistant 6 端点的前缀错位） |

### 4.1 端点覆盖盘点（脚本实测，替代人工点数）

点数靠人眼必然失准，改用 `server-go/scripts/audit_endpoints.py` 逐条对拍：Python 侧解析 `include_router` 前缀 + `APIRouter(prefix=...)` + 路由装饰器，Go 侧解析 `x.Group(...)` 链与常量/拼接路由（含 `internal/app/app.go` 的直接注册），路径参数统一归一为 `:param` 后取差集。

```
python server-go/scripts/audit_endpoints.py          # 汇总 + 缺失清单
python server-go/scripts/audit_endpoints.py --diff   # 只打缺失
```

当前实测：**Python 321 条路由装饰器 / Go 已注册 273 / 缺失 63**（2026-10-19）。321 与 §1 的 344 之差来自 16 个资产工厂端点（由 Go 侧工厂循环注册，脚本不展开）与 `api_route` 多方法去重。

未迁端点按 router 分布（端点数）：projects 17、custom_providers 15、assistant 14、files 12、assets（全局库）8、characters avatar/variants 6、system_config 4、character_relations 3、usage 3、agent_chat / cost_estimation / creative 各 1（合计 73）。其中 assistant 14 个里有 6 个已实现但**前缀错位**（现挂 `/agent/sessions*`，Python 与前端 `api.ts` 的契约是 `/projects/{n}/assistant/sessions*`）。

**读表注意**：P3 的"完成"仅指 service 层（registry / pricing / queue / worker / 钩子），它的 HTTP 管理面（providers 12 + custom_providers 15）未迁，整组不可用，因此 P3 实际仍是"进行中"。P4 已完成（HTTP 面 44/44）。缺失端点不是边角——供应商配置、文件上传、项目导出等是前端流程的直接依赖。

### 4.2 本轮已落地（2026-10-19 第二批：26 + 11 端点）

| 组 | 端点数 | 落点 | 关键语义 |
|---|---|---|---|
| tasks | 8 | `transport/http/tasks.go` | 列表/详情按请求 locale 渲染 `error_message` 与 `result.warnings`（绝不改队列自有 dict）；`task_not_found` 在 cancel 是 400、在 detail 是 404 |
| queue_observability | 2 | `transport/http/queue_observability.go` | 快照四源（tasks / api_calls / worker lane / worker_lease）；alerts 只给机器码 + 数值 |
| versions | 2 | `transport/http/versions.go` | `is_current` / `file_url` 读时派生；还原的四处副作用（asset sheet、剧本 generated_assets、宫格记录、视频缩略图） |
| generate | 14 | `transport/http/generate.go` | 分镜/视频/tts/资产/图片编辑/音色样本 + **canvas-images 2**；`message_key` 在传输层渲染，服务层不持 locale |
| batches | 4 | `service/generation_batch.go` + `transport/http/batches.go` | 准入原子（一个事务、冲突整批回滚）；状态从任务状态派生，不落库；`extra="forbid"` 严格入参 |
| episode_duration | 5 | `service/episode_duration.go` + `transport/http/episode_duration.go` | 保存只写 metadata；预览纯读；应用才写分镜时长；写路径全部锁内比对修订号，过期 409 带 `actual_revision` |

### 4.6 本轮第六批：projects 剧本/分集组（10 端点）

| 组 | 端点数 | 落点 | 关键语义 |
|---|---|---|---|
| projects 扩展 | 10 | `service/project_extra.go` + `transport/http/projects_extra.go` | 剧本编辑（scenes/shots/segments）整段 RMW 在单一剧本锁内、未命中锁内报错跳过写回；ad 镜头 PATCH 白名单（shot_id/generated_assets 不可改写）；shots 重排按全排列校验（length/duplicate/mismatch 三类 400）；分集 title 以剧本顶层为唯一真相源（脚本锁→项目锁 + 内联镜像同步）；删集取消在途任务 + 剩余集 display_episode 重排 + 剧本入 deleted_episode_script_files；video-capabilities 按 generation_mode 定桶、与执行链同一解析入口 |

**未迁（7）**：import ×2（zip 全项目搬运）、export ×3（打包下载 + 短时效 token + 剪映草稿包）、generate-overview（LLM）、source（与 SourceLoader 同批）。

### 4.5 本轮第五批：文件管理面（10 端点）

| 组 | 端点数 | 落点 | 关键语义 |
|---|---|---|---|
| files（轻量组） | 10 | `service/project_files.go` + `transport/http/files.go` | 静态文件服务**跳过 symlink + nosniff**（用户上传字节禁内容嗅探成脚本）；越界一律 403（不让 404/403 差异成为存在性探针）；source 删除级联 raw 备份；drama step1 写入前校验 JSON 形状；参考视频 step1 走 ScriptReviewService 单一写盘出口；参考音频删除「先删文件后清字段」且只认 `characters/refs_audio` 内的路径（字段可被 PATCH 成任意项目内字符串，须防被诱导删除 project.json） |

**未注册**：`POST /upload/{type}`（SourceLoader：docx/epub/pdf → UTF-8 归一 + 章节切分）与 `POST /style-image`（Gemini 风格分析）——上传管线重头，单独一批。

### 4.4 本轮第四批：供应商面（20 端点，按「只要 openai / claude / seedance / seedream」收敛）

| 组 | 端点数 | 落点 | 收敛方式 |
|---|---|---|---|
| providers | 12 | `service/provider_admin.go` + `transport/http/providers.go` | 不按厂商写分支：目录直接读 `provider.ProviderIDs()` / `ProviderMetaFor`，状态派生「有活跃凭证即 ready」；连接测试只对 OpenAI 兼容端点（openai / ark）实现，其余回 `success=false` + 明确原因，不假装通过 |
| agent_config | 8 | `service/agent_credential.go` + `transport/http/agent_config.go` | 预设目录收敛为 anthropic（`sdk_type=claude`）与 openai 两家 + `__custom__` 哨兵；凭证按 `(user, sdk_type)` 互斥激活，切换「先清后置」；试连按协议分派（OpenAI 走 `GET /models`，Anthropic 发一条 `max_tokens=1` 的最小 messages） |

关键判定：
- **供应商范围**：文本 openai / claude，视频 ark 的 seedance + openai video，图片 openai image + ark 的 seedream。管理面本身不感知这层收敛（目录来自注册表），所以后续加厂商无需改这层代码。
- **密钥只进不出**：`GET /providers/:id/config` 与凭证列表一律回 `••••` + 尾 4 位；`PATCH config` 拒绝密钥键（密钥必须走凭证端点，否则「是否有活跃凭证」的判定会失真）。
- **启用开关落 `system_setting`**（`provider_enabled:<id>`），解析链与 UI 读同一处。

审计脚本修复（重要）：URL 前缀与字符串常量改为**按文件作用域**解析。此前 `admin` / `base` 这类短变量被全局覆盖——`creation_plans.go` 的 `admin := group.Group("", ...)` 把 `agent_config.go` 的 `/agent` 前缀冲掉，导致整组端点被误判「未迁」。修正后 agent_config 8 个端点正确计入。

### 4.3 本轮第三批：P4 收尾 + 系统面（19 端点）

| 组 | 端点数 | 落点 | 关键语义 |
|---|---|---|---|
| grids | 7 | `service/grid_task.go` + `transport/http/grids.go` | 动作端点（重生成/切分/上传）都过 `ensure_grid_writable` 闸门；preflight 早于任何「删旧记录/入队计费」；重生成沿用记录上冻结的 rows/cols/prompt/比例 |
| end_frames | 3 | `service/end_frame.go` + `transport/http/end_frames.go` | 两条设置通道同一落点（快照到 `end_frames/scene_{id}.png`）；文件副作用与字段写回同在一把剧本锁内；落盘失败按「操作代次」条件回滚 |
| shot_uploads | 1 | `service/shot_upload.go` + `transport/http/shot_uploads.go` | 旧文件补登版本 → 写 canonical → 登记新版本 → `generated_assets` + status 回写；视频走流式落盘并抽缩略图 |
| onboarding | 2 | `transport/http/onboarding.go` | 引导标记刻意不进 `/system/config` 白名单（不是用户可配置项） |
| api_keys | 3 | `transport/http/onboarding.go` | 管理操作必须 JWT 身份（key 不能自我续期/吊销）；完整 key 只在创建响应出现一次 |
| features | 3 | `transport/http/features.go` | 诊断面只给「开关名/生效值/来源/是否合法」，**绝不回传原始 env 值**；非法布尔值 fail closed |
| system | 1 | `transport/http/system.go` | 诊断包只收 `shotwise.log*` 且**跳过 symlink**（防软链外泄）；诊断文本只写环境事实，不含密钥 |

新增基础设施：
- `internal/infra/img`（`Sniff` / `NormalizeToPNG` / `ReadAllBounded` / `FitLongEdge`）——上传面的格式归一与像素预算。**已知留白：EXIF 方向矫正未实现**（手机直出照片的横竖可能反，生成器产出的图不受影响）。
- `config.ConfigService.SetSetting`——system_setting 的通用写入口（先 UPDATE 再按需 INSERT，该表 key 上无唯一索引）。
- `domain.VideoAspectRatioOf` / `GridStoryboardEnabled` / `NewStoryboardGeneratedAssets` / `UpdateStoryboardItemStatus`——宫格与资产回写的域级口径。

E2E 过程中踩到的两个坑（已修）：
- **`db.Save` 覆写 `created_at`**：行已存在时 Save 会把该列写成零值，而列是 NOT NULL——「只改一个标志」的写（批量取消/重试批次）直接 500。统一收敛到 `upsertBatchRow`（先 UPDATE 再按需 INSERT）。
- **`api_keys.user_id` 外键**：创建 key 前必须存在对应用户行，测试夹具漏建默认用户时表现为 500（FK violation）。

附带修复：
- **错误信封形态**：`tasks` / `versions` / `queue_observability` / `generate` 的 detail 从 `{"code": ...}` 对象改成翻译后的字符串（Python 侧这些走 `ApiError` → app 级 handler，对象形只用于 creation_plans / creative_boards / media_* / publishing / episode_duration 等少数域）。
- **i18n 补齐**：`bundle_generated.go` 由 `scripts/gen_i18n.py` 从 `lib/i18n/{zh,en,vi}` 生成（636 key × 3），手写条目优先，避免再生时覆盖措辞。
- **`UpdateEpisodeScriptNoValidate`**：只改 `duration_seconds` / `duration_locked` 的热路径不再重算 metadata（否则 `updated_at` 刷新会让剧本修订号漂移）。

## 5. 不可回退的语义（必须先钉死）

这些是 Python 侧踩过坑才定下来的不变式，Go 侧照抄，不要"优化"掉：

1. **任务去重在 DB 部分唯一索引，不在应用层查重**。
   `UNIQUE(project_name, task_type, resource_id, COALESCE(script_file,''), COALESCE(resource_type,'')) WHERE status IN ('queued','running','cancelling')`
2. **`mark_succeeded`/`mark_failed` 返回受影响行数，0 行 = 外部已把状态翻走**（0-rows-cancelled 协议）。不当错误处理。
3. **`cancelling` 是中间态**，必须有 worker finally 或 `finalize_cancelling` 收口，否则 worker 离线时任务永久悬停。
4. **池满过滤用黑名单而非白名单**，且 `provider_id IS NULL` 必须放行（否则已删除/禁用 provider 的任务静默堆积）。
5. **非幂等 POST 的歧义态不重试**（`AmbiguousSubmitError`），否则重复建任务 + 重复计费。
6. **resume 绝不重新 submit**（ADR 0007）；`resume_video` 未实现的供应商必须在 `NON_RESUMABLE_VIDEO_PROVIDERS` 登记。
7. **供应商解析失败要 fail loud**，不静默换供应商（ADR 0054 四级解析的最后一层 auto-resolve 除外）。
8. **project.json 是资产的唯一真相源**；`assets` 表是跨项目全局库副本；`media_assets` 表只是只读索引。
9. **env 禁止持有 provider 密钥**，启动即 fail-fast（fork 出去的子进程会继承）。
10. **SSE 的 `entry` 事件 `id` 字段即 `seq`**，断点续传靠 `Last-Event-ID`。
11. **i18n 只传 key + params**，错误信息不硬编码文案。
12. **路径安全统一走 `safe_join`**（realpath + 前缀比较），不自己拼。

## 6. 子进程与平台

- Python 侧全部 ffmpeg 调用已是 `create_subprocess_exec(argv)` 无 shell，Go 直接 `exec.CommandContext` 1:1 对应。
- Windows：不走 `--reload`；`exec.LookPath` 探测 ffmpeg/ffprobe，缺失降级而非失败。
- 沙箱：Linux/macOS 用 bwrap 包住 Bash 工具；Windows 回退到命令前缀白名单（`python .claude/skills/`、`ffmpeg`、`ffprobe`）。

## 7. 相关文档

- `01-api-map.md` — 343 个端点的迁移映射表
- `02-data-layer.md` — 53 表、迁移策略、project.json schema
- `03-agent-runtime-eino.md` — Eino 替代方案与自研缺口
- `04-generation-pipeline.md` — 队列、worker、backend、ffmpeg、计费
