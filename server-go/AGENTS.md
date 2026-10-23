# server-go — SHOTWISE Go 后端

`server-go/` 是 Python `server/`（FastAPI）的 Go 重写，长期目标为功能对齐后替换。技术栈：Gin（HTTP 路由）+ GORM（SQLite 开发 / PostgreSQL 生产）+ cloudwego/eino（LLM 编排框架）。启动顺序与 Python 侧 lifespan 对齐：`assert_no_provider_secrets → 沙箱探测 → 认证就绪 → 数据库迁移 → 默认用户 → 事件服务 → 生成 worker → 监听`，关停顺序相反（见 `cmd/shotwised/main.go` 头注释）。

## 开发命令

```bash
go build ./...                     # 编译
go test ./...                      # 全量测试（-run 单个 / -count=1 禁缓存）
go run ./cmd/shotwised             # 启动服务
go run ./cmd/migrate               # 执行 migrations/ 下的数据库迁移
go vet ./...                       # 静态检查
```

## 顶层目录

```
server-go/
├── cmd/                 可执行入口
│   ├── shotwised/      服务主入口（main.go 装配全部依赖；assembly_smoke_test.go 装配冒烟测试）
│   └── migrate/        独立迁移工具入口
├── internal/           全部业务实现（Go 惯例：不对外导出）
├── migrations/         GORM AutoMigrate 之外的手写 SQL 迁移
├── go.mod / go.sum     模块名 github.com/shotwise/shotwise
└── projects/           本地开发态项目数据（含 .shotwise.db，勿提交）
```

## internal/ 分层

依赖方向自上而下单向流动：`transport → service → (domain, repository, queue) → (infra, providers)`。

```
internal/
├── app/            依赖装配（wire 各 service / queue / handler，Gin 路由注册）
├── transport/http/ HTTP 层（Gin handler，仅做参数解析 + 调 service + 响应）
├── service/        业务编排层（事务边界、跨 domain 组合）
├── domain/         纯领域模型与规则（无 IO 依赖，可单测）
├── repository/     GORM 数据访问
├── queue/          生成任务队列与 worker（image / video 独立并发通道）
├── agent/          智能体运行时（eino 集成，详见下文框架图）
├── textgen/        LLM 文本生成（eino ChatModel 封装）
├── providers/      生成模型供应商 backend（视频/图片 API 客户端）+ provkit 通用工具包
├── capcut/         剪映（JianYing）draft_content 生成器
├── infra/          基础设施：db / ffmpeg / httpc / fs / img
├── auth/           JWT 认证
├── config/         配置加载与 .env 语义
└── pricing/        计费/定价
```

## 各包职责详解

### cmd/shotwised（主入口）

`main.go` 是唯一装配点，按序完成：.env 加载（`config.LoadDotenv`，python-dotenv 语义：已存在的环境变量优先）→ `config.AssertNoProviderSecrets`（红线：provider 密钥禁止进入进程环境，必须在任何子进程启动前执行）→ `db.Open` + `db.Migrate`（GORM + 手写迁移混合）→ 默认用户/系统用户种子 → 官方 Creation Skill 目录物化（失败不阻断）→ 认证凭证 → 各 service 构造 → 生成 worker 启动 → HTTP 监听。`assembly_smoke_test.go` 验证整套装配可编译可拉起。

### internal/transport/http（HTTP 层）

按资源域拆分子包，Gin handler 只做参数解析、调 service、格式化响应，不含业务逻辑：

- `httpkit/` — 公共工具：请求解析（`request.go`）、错误格式（`errors.go`）、SSE 流（`stream.go`）、用户注入（`user.go`）、feature flag（`feature.go`）、测试桩（`httpkittest/`）
- `system/` — 健康检查、认证登录、系统配置、onboarding
- `project/` — 项目 CRUD、归档（`project_archive.go`）、源码导出
- `asset/` — 角色/场景/道具资产、角色变体与关系、记忆（`memories.go`）
- `script/` — 剧本审阅、草稿晋升（`draft_promotions.go`）
- `generation/` — 生成任务、批次、宫格、参考视频、版本查询、队列可观测性
- `media/` — 媒体资产、渲染、剪辑草稿组装（`jianying_assembly.go`）、发布、分镜上传
- `creative/` — 创意板、创作计划、工作流、prompt 注册表
- `agentrt/` — 智能体运行时端点（`agent.go` 会话管理、`agent_chat.go` SSE 聊天）
- `agentconfig/` — 智能体配置
- `providers/` — 供应商管理（含自定义供应商 CRUD）
- `assistant/`、`files/` — 助手会话与文件上传

### internal/service（业务编排层）

根包：`project_store.go`（项目 JSON 单一真相源）、`events.go` + `EventHub`（项目事件订阅总线，worker 终态经它广播给前端）、`ledger.go`（计费结算）、`gridqueue.go`/`grid_manager.go`（宫格队列）、`versions.go`、`memory.go`、`episode_script_store.go` 等。

子包按域划分：

- `generation/` — 生成核心：`TaskDispatcher`（按 task_type 路由）、`image_generator.go`/`generator.go`（image/audio/video 三个生成器）、`tasks_*.go`（按任务类型的执行路径：design/canvas/video/voice 等）、`resume.go`（续跑，不经常规视频流水线）、`generation_batch.go`、`queue_observability.go`
- `script/` — 剧本生成：多步流水线（`script_step2.go`、`narration_step1.go`、`pending_authoring.go`、`episode_planning`）、prompt 构造器（按 content_mode 分 ad/reference 变体）、`episode_ledger.go`（剧集计费）
- `media/` — 媒体服务：资产索引、渲染、剪映导出（`jianying_export.go`/`jianying_draft.go` 模板 `draft_meta_info_template.json`）、镜头上传
- `creative/` — 创作工作流：`creation_skill_catalog.go`（官方 Skill 物化）、`workflows.go`、`creation_plans.go`、`creative_board.go`
- `reference/` — 参考视频路线：预检、单位划分、隔离（`quarantine.go`）、step1 迁移、上传
- `admin/` — 管理域：系统配置、供应商管理、智能体凭证、自定义供应商
- `asset/` — 资产服务：upsert、end frame
- `publishing/` — 发布（含发布加解密 `publishing_crypto.go`）

### internal/domain（领域层）

纯模型与规则，无 IO，全部可单测（每个文件基本配 `_test.go`）：

- `project/` — 项目/资产模型、画幅尺寸、资产重命名
- `creation/` — 创作计划、Creation Skill、工作流、prompt 注册表、设计 prompt
- `script/` — 剧本模型、骨架、编辑器、分集时长规划
- `media/` — 媒体组装、字幕、参考单元、分镜序列、审阅
- `grid/` — 宫格模型、布局、prompt
- `character/` — 角色变体与关系
- `task/` — 任务失败分类、prompt 预览
- `draft/` — 草稿晋升

### internal/agent（智能体运行时 — 核心框架）

用 cloudwego/eino 重建，取代 Python 侧 Claude Agent SDK 子进程模型。`internal/agent` 是本项目智能体框架的心脏，见下文框架图。

- `runtime.go` — Runtime：eino 编排循环的宿主，管理智能体生命周期
- `session.go` — 会话管理与串行化（对应 Python SessionActor 的每会话专属 task 语义）
- `chat_model_test.go` 对应的 ChatModel 适配 — 把 providers 侧 LLM 包成 eino 统一模型接口
- `provider.go` — 模型供应商解析（DB 凭证 → eino ChatModel）
- `tool.go` — 工具注册接口（envelope：名称/描述/JSON schema/执行器）
- `events.go` + `eventlog_adapter.go` — eino 事件流 → 系统事件
- `assistant.go` — 助手会话入口
- `checkpoint/` — SQL 检查点存储（`agent_checkpoints` 表，供中断恢复）
- `eventlog/` — 事件日志 SQL 存储（`agent_session_event_log` 表，权威 transcript，SSE 按 seq 游标续传依赖它；`sql_store_ext.go` 为扩展查询）
- `policy/` — 策略与守卫：`guard.go`（工具调用安全门）、`policy.go`（内容模式/生成模式路由策略）
- `skills/` — Skill 目录与 profile（`catalog.go`、`backend.go`、i18n）；官方 Creation Skill 的加载与匹配
- `subagents/` — 子智能体派生
- `workspace/` — 智能体文件工作区（路径边界约束）
- `tools/` — 全部智能体工具，按域分子包：
  - `builtin/` 基础工具；`memory/` 记忆；`project/` 项目读写
  - `script/` 剧本生成与分集规划；`storyboard/` 分镜与宫格
  - `assets/` 资产与关系；`imageedit/` 图片指令式编辑
  - `video/` 视频生成与参考；`timeline/` 时间线；`review/` 审阅
  - 根文件 `gentask.go`/`assettask.go`（任务生成工具）、`genjson.go`（结构化 JSON 输出）、`textgate.go`（文本闸门）、`mediaprobe.go`（媒体探测）、`videocaps.go`、`projectdoc.go`、`envelope.go`、`deps.go`（工具依赖注入容器）

### internal/providers（生成模型供应商）

按供应商一目录（`ark/`、`gemini/`、`kling/`、`minimax/`、`vidu/`、`dashscope/`、`grok/`、`openai/`、`agnes/`、`newapi/`），每个内分 `*_image.go` / `*_video.go` / `*_audio.go`。`provkit/` 是通用工具包（common/image_common/video_common/fs），新增供应商优先复用。`register.go` 全局注册；`custom_*.go` 支持用户自定义供应商（`custom-` 前缀，能力声明读 DB）；`v2_video_generations.go` 为视频生成 v2 协议。

### internal/infra（基础设施）

- `db/` — GORM 打开/迁移（SQLite/PostgreSQL 双方言）、时间类型
- `ffmpeg/` — ffmpeg 封装：probe/concat/render/capture/frames/subtitles/packaging
- `httpc/` — 出站 HTTP 客户端（统一超时/重试）
- `fs/` — JSON 原子读写、safejoin（路径逃逸防护）
- `img/` — 图片处理：normalize/composite/avatar

### internal/capcut（剪映导出）

生成剪映 draft_content JSON：`draft/`（草稿文件夹）、`script/`（模板 `draft_content_template.json`）、`segment/`（video/audio/text/effect 各类轨道片段）、`track/`（轨道）、`animation/`（动画）、`template/`。独立 LICENSE，代码含剪映协议常量。

### 其余小包

- `app/` — `app.go`（HTTP server 组装）、`middleware.go`（认证/日志中间件）、`spa.go`（前端静态托管 fallback）
- `auth/` — JWT 签发校验、凭证管理
- `config/` — `envfile.go`（.env 加载）、`AssertNoProviderSecrets`、`EnsureAuthPassword`
- `pricing/` — 内置定价目录与计算器（`pricing.BuiltinCatalog`，ledger 结算依赖）
- `textgen/` — LLM 文本生成门面（供 service/script 等非 agent 路径使用）
- `repository/` — GORM 仓储（如 `NewUserRepository`）
- `queue/` — 生成队列基础设施

## Agent 框架图

```
                            HTTP (Gin)
                                │
                    internal/transport/http/agentrt/
                    (agent.go 会话管理 · agent_chat.go SSE)
                                │
                                ▼
                ┌───────────────────────────────┐
                │   internal/agent                │
                │                                 │
                │   assistant.go ──► session.go  │  会话入口；每会话串行化
                │        (对应 Python SessionActor)│  所有模型/工具调用
                │                        │        │
                │                        ▼        │
                │   ┌────────── Runtime (runtime.go) ──────────┐
                │   │            eino 编排循环宿主              │
                │   │                                        │
                │   │  ┌─────────────┐      ┌─────────────┐  │
                │   │  │  ChatModel   │      │   Tools      │  │
                │   │  │ (provider.go │      │ (tool.go     │  │
                │   │  │  DB凭证→模型) │◄────►│  envelope +  │  │
                │   │  └──────┬──────┘      │  注册接口)    │  │
                │   │         │             └──────┬──────┘  │
                │   │         ▼                    │         │
                │   │  ┌──────────────┐    ┌───────▼────────┐ │
                │   │  │ policy/guard │    │  tools/ 子包     │ │
                │   │  │ 工具调用安全门 │───►│ builtin memory  │ │
                │   │  └──────────────┘    │ project script  │ │
                │   │                      │ storyboard …   │ │
                │   │  ┌──────────────┐    └───────┬────────┘ │
                │   │  │ skills/      │            │          │
                │   │  │ 目录+profile │    ┌───────▼────────┐ │
                │   │  └──────────────┘    │ workspace/     │ │
                │   │  ┌──────────────┐    │ 文件工作区边界  │ │
                │   │  │ subagents/   │    └───────┬────────┘ │
                │   │  │ 子智能体派生  │            │          │
                │   │  └──────────────┘            │          │
                │   └─────────────────────────────┼──────────┘
                │                    │            │
                │         ┌──────────┴─────────┐ │
                │         ▼                      ▼
                │  checkpoint/               eventlog/
                │  (agent_checkpoints 表,    (agent_session_event_log 表,
                │   中断恢复)                  权威 transcript,
                │                              SSE 按 seq 续传)
                └───────────────────────────────┘
                                │
              ┌─────────────────┼──────────────────┐
              ▼                 ▼                  ▼
      providers/          service/            infra/
      (eino-ext/openai    (generation/ 队列,    (ffmpeg 媒体处理,
       + 各供应商 backend)  asset, script,       db, fs, httpc)
                           media …)
```

一次智能体调用的数据流：HTTP 进入 `agentrt` → `assistant.go` 建立会话 → `Runtime` 驱动 eino 循环 → ChatModel 产出工具调用 → `policy/guard` 审查 → `tools/` 执行（落 `workspace/` 边界内）→ 结果回喂模型 → 每步事件写 `eventlog`（SSE 流式推给前端）+ 关键状态写 `checkpoint`（进程重启后可恢复）。生成类工具最终走 `service/generation` 入队，由 worker 异步执行，终态经 `service.EventHub` 广播。

## 开发注意事项

- **红线**：provider 密钥禁止出现在进程环境变量（`config.AssertNoProviderSecrets` 在 main 启动即断言）——fork 出去的子进程会继承 env
- **启动顺序**：worker 的取消回调必须先于 worker 启动注册；EventHub 必须先于 queue 构造（worker 终态经它广播）
- **依赖方向**：`transport → service → domain`，domain 不 import 任何 IO 包；跨 service 组合放 `service` 根包
- **测试**：各包普遍使用 `fakes_test.go` + `testhelpers_test.go` 模式（fake 注入而非 mock 框架）；`real_test.go`（ffmpeg）需要真实二进制，CI 可能跳过
- **迁移**：GORM AutoMigrate 与 `migrations/` 手写 SQL 并存（`db.Migrate` 混合执行，含 stamp 语义）；SQLite 与 PostgreSQL 双方言（`0001_init.sqlite.sql` / `0001_init.pg.sql`）
- **沙箱**：Agent 工具执行假设沙箱默认开启，路径越界与白名单外网络会被拒（Linux bwrap / macOS sandbox-exec；Windows 降级为命令前缀白名单）
- **与 Python 侧关系**：功能对齐目标，行为语义（启动顺序、content_mode/generation_mode、resume 路径）以 Python 侧与根目录 AGENTS.md 为准

