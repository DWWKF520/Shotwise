# SHOTWISE 后端 Go 化迁移总纲

> 目标：把 Python（FastAPI + SQLAlchemy + Claude Agent SDK）后端全量重写为 Go（gin + GORM + Eino）。
> 前端保持不动，API 契约逐条对齐；迁移期间 Python 与 Go 可并存，按路由组灰度切换。

## 1. 现状规模

| 项 | 量 |
|---|---|
| Python 代码 | `server/` 5.8 万行（152 文件）+ `lib/` 6.7 万行（278 文件） |
| HTTP 端点 | 约 343 个（45 个 router 文件 + 资产工厂 4 类型 × 4 端点） |
| DB 表 | 53 张业务表（Alembic 78 个迁移，3 个 merge 点） |
| 供应商 backend | 视频 11 个类 / 图片 9 个类 / 音频 2 个类 + 自定义供应商声明式端点 |
| 智能体工具 | 进程内 MCP 37 个（31 对外 + 5 内部 + 1 复用） |
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
| **P2 领域与资产** | projects / characters / scenes / props / products / files / versions | 项目 CRUD + 资产 CRUD 冒烟通过 | 进行中（项目 CRUD + 四类资产 16 端点含级联改名已通；files / versions / 全局 assets 库 / 剧集 JSON 校验未动） |
| **P3 供应商与计费** | registry、resolver 四级解析、backend 接口、pricing、api_calls | 一次真实图片生成成功并正确记账 | 未开始 |
| **P4 生成流水线** | 队列去重、双通道 worker、容量表、熔断、租约、resume、ffmpeg | 批量生成 + 取消 + 断点续传冒烟 | 未开始 |
| **P5 智能体** | Eino ChatModelAgent + GuardedTool + 37 工具 + SSE entry 流 | 一条完整会话跑通（含 Skill 调用与 AskUserQuestion） | 起步（ReAct 循环与最小工具有骨架，policy/沙箱/事件日志未动） |
| **P6 长尾 router** | workflows(41) / memories(15) / creative_boards(16) / publishing / media_* | 端点覆盖对齐清单 | 未开始 |
| **P7 切换** | 前端指向 Go 服务，Python 服务下线 | 前端全流程回归通过 | 未开始 |

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
