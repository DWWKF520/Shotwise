# 数据层迁移

## 1. ORM 选型

Python：SQLAlchemy 2.0 **async** ORM（aiosqlite / asyncpg）。
Go：**GORM** + `glebarez/sqlite`（纯 Go，无 cgo，Windows 友好）与 `gorm.io/driver/postgres`（pgx）。

Go 侧不需要 async：goroutine + 连接池即可。`expire_on_commit=False` 的语义在 Go 里天然成立（无 identity map）。

必须复刻的连接层差异：

| 项 | SQLite | PostgreSQL |
|---|---|---|
| 连接参数 | `busy_timeout=30000`、`journal_mode=WAL`、`foreign_keys=ON` | `pool_size=10, max_overflow=20, pool_recycle=3600` |
| 预检 | `pool_pre_ping=true` | 同 |
| 部分索引 WHERE 字面量 | `is_active = 1` | `is_active` |
| 布尔默认值 | `"1"` | `true()` |

## 2. 表清单（53 张业务表）

| 域 | 表 |
|---|---|
| 任务队列 | `tasks`(37 列)、`worker_lease`、`generation_batches`、`external_executions` |
| 用量计费 | `api_calls`(36 列) |
| 供应商配置 | `provider_config`、`system_setting`、`provider_credential`、`agent_anthropic_credentials`、`custom_provider`、`custom_provider_model` |
| 用户鉴权 | `users`、`api_keys`、`publishing_accounts` |
| Agent 会话 | `agent_sessions`、`agent_session_event_log`、`agent_session_user_message_links`、`agent_session_entries`、`agent_session_summaries`、`memory_entries`、`memory_candidates` |
| 资产媒体 | `assets`、`media_assets`、`media_bindings`、`media_derivations` |
| 装配渲染发布 | `assembly_plans`、`assembly_plan_revisions`、`render_jobs`、`render_artifacts`、`render_review_snapshots`、`publish_jobs` |
| 创意板/创作计划 | `creative_boards`(+items/edges/versions)、`creation_plans`、`creation_compatibility_events`、`creation_skill_definitions`(+versions/compatibilities) |
| Workflow | `workflow_definitions`、`workflow_revisions`、`workflow_nodes`、`workflow_edges`、`workflow_runs`、`workflow_node_runs`、`workflow_node_run_items`、`workflow_templates`、`workflow_marketplace_reviews`、`workflow_usage_stats`、`workflow_approvals`、`budget_reservations`、`project_event_log` |

### 2.1 已知的 ORM ↔ DB 漂移（Go 侧按实际 DDL 建）

- `tasks.submitted_base_url`、`tasks.execution_checkpoint_json` **只在迁移里，ORM 未声明**。
- 一批索引（`ix_tasks_fencing_token/lease_owner/lease_until/progress/progress_source/phase_code/request_idempotency_key/input_fingerprint`）同样只在迁移里。
- `CreativeBoardVersion` 注册了 metadata 但未在 `models/__init__.py` 导出。

**结论：迁移链本身已不是 schema 的权威描述。** Go 侧以「实测 DDL」为准。

## 3. 迁移策略

**一次性按最终 schema 建 `0001_init`，不要分批复刻 78 个 Alembic 迁移。**

理由：
1. 78 步里大量是历史事故修补（`3c8b0ae43345` 修 CAST 截断、`b942e8c5d545` 时间戳类型统一、`3649100774fa` 删表、image backend 桶收拢等），对新库无意义。
2. 3 个 merge 迁移（`8b8a38ba7c17` / `wf4_merge_heads` / `20260830_merge_all_heads`）的 `upgrade()` 是空操作。
3. 已存在 ORM/DB 漂移，说明权威描述只有实测 DDL。

执行方式：
- 从 `projects/.shotwise.db` 导出与方言无关的规范 DDL 作为 `migrations/0001_init.sql` 基线。
- 用 Go 自研的极简迁移执行器（记录 `schema_migrations(version)`，按文件名顺序执行 `.sql`，每步一个事务）。
- 迁移文件按方言分叉：`0001_init.sql`（通用）+ `0001_init.sqlite.sql` / `0001_init.pg.sql` 覆盖差异化部分。

### 3.1 两处必须显式分叉的地方

1. **部分唯一索引**：`tasks`（活跃去重）、`render_jobs`（活动任务）、`provider_credential`（one active）、`agent_anthropic_credentials`（per user+sdk）、`agent_session_event_log`（client_key）、`agent_session_entries`（uuid）。
2. **表达式索引**：`idx_tasks_dedupe_active` 用 `COALESCE(script_file,'')`。SQLite 重建表时会丢，需要等价保护逻辑。

### 3.2 时间戳列类型（最大的隐形地雷）

SQLite 上这些列实际是 **TEXT/VARCHAR**（Alembic 的时间戳统一迁移在 SQLite 上跳过改类型）：
`tasks.queued_at/started_at/finished_at/updated_at`、`api_calls.started_at/finished_at/created_at`、`worker_lease.updated_at`、`agent_sessions.created_at/updated_at`、`media_assets.created_at`、`media_bindings/derivations.created_at`。

**Go 侧必须写自定义 Scanner/Valuer**：读时接受 `TEXT`(ISO8601) / `INTEGER`(epoch) / `REAL`(julian) / `TIMESTAMPTZ`，写时统一 ISO8601 UTC 字符串。
新库建议直接统一，但灰度期要能读既有库。

## 4. 配置系统

### 4.1 来源优先级

```
1. DB —— 唯一真相源（system_setting / provider_config / provider_credential /
                      agent_anthropic_credentials / custom_provider*）
2. env / .env —— 仅基础设施项：DATABASE_URL、SHOTWISE_DATA_DIR、AI_ANIME_PROJECTS、
                  AUTH_USERNAME/PASSWORD/TOKEN_SECRET、PUBLISHING_ENCRYPTION_KEY
3. 代码常量 —— provider 目录 PROVIDER_REGISTRY、各类默认值
```

**env 不许持有 provider 密钥**：启动首行 fail-fast（`assert_no_provider_secrets_in_environ`），因为 fork 出去的沙箱子进程会继承父 env。

数据目录优先级：`SHOTWISE_DATA_DIR` > `AI_ANIME_PROJECTS` > `<PROJECT_ROOT>/projects`。

### 4.2 后端/模型四级解析（ADR 0054）

```
payload（入队时锁定） > 项目桶 > 项目默认 > 全局桶 > 全局默认 > 自动推断
```

| 媒体 | 项目桶键 | 项目默认键 | 全局桶键 | 全局默认键 |
|---|---|---|---|---|
| image | `image_provider_t2i` / `_i2i` | `default_image_backend` | `default_image_backend_t2i` / `_i2i` | `default_image_backend` |
| video | `video_provider_i2v` / `_r2v` | `video_backend` | `default_video_backend_i2v` / `_r2v` | `default_video_backend` |
| audio | — | `audio_backend` | — | `default_audio_backend` |
| text | `text_backend_simple` / `_complex` | `default_text_backend` | 同左 | `default_text_backend` |

桶是可选覆盖，空值回退默认层。解析出的 provider 被禁用 → **fail loud，不静默换供应商**。

### 4.3 密钥存储

| 用途 | 是否加密 |
|---|---|
| `provider_config`（`is_secret` 标记）、`provider_credential.api_key`、`agent_anthropic_credentials.api_key`、`custom_provider.api_key` | **明文**（读侧 `mask_secret()` 脱敏） |
| `publishing_accounts.encrypted_access_token/_refresh_token` | **AES-256-GCM 真加密**，密钥来自 `PUBLISHING_ENCRYPTION_KEY`（urlsafe-b64，解码须正好 32 字节）；令牌格式 `v1.{b64(nonce)}.{b64(ciphertext)}`，AAD=`"v1"` |

脱敏规则：长度 ≤8 全 `••••`，否则 `前4…后4`。

## 5. project.json schema（schema_version = 5）

```
project.json
├── schema_version=5, title, content_mode(narration|drama|ad),
│   source_kind(novel|screenplay), generation_mode(storyboard|reference_video),
│   grid_storyboard: bool, aspect_ratio="9:16", style(非空必填),
│   style_template_id?, style_image?, default_duration?, target_duration?(仅ad),
│   brief?(仅ad), source_language?, speech_rate_units_per_second?,
│   source_fingerprints?, deleted_episode_script_files?, planning_cursor?,
│   narration_voice?, narration_speed?, video_generate_audio?
├── 后端覆盖：video_backend, video_provider_i2v/_r2v, image_provider_t2i/_i2i,
│             default_image_backend, audio_backend, text_backend_simple/_complex,
│             default_text_backend, model_settings
├── overview{synopsis,genre,theme,world_setting,language,generated_at}
├── character_relations[], metadata{created_at,updated_at}
├── episodes[{episode,title,script_file,display_episode?,ledger_status?,
│             source_range?,hook?,outline?}]
├── characters{name:{description,character_sheet,voice_style,
│                    reference_image?,reference_audio?,character_avatar?,
│                    voice_notice_dismissed_at?,voice_updated_at?}}
├── scenes{name:{description,scene_sheet}}
├── props{name:{description,prop_sheet}}
└── products{name:{description,product_sheet,brand,reference_images[],selling_points[]}}
```

创建即定不可变：`source_kind`、`generation_mode`。ad 项目拒绝 `default_duration` 与 `grid_storyboard`，`episodes` 恒为 `[{episode:1}]`。

资产名须过 `validate_asset_name()`：禁 `/ \ : * ? " < > | ]`、控制字符、`..`、尾随点、Windows 保留设备名；统一 NFC 归一。

## 6. 剧集 JSON schema

骨架种类由 `resolve_declared_kind()` 决定：

| kind | id 字段 | 角色名单 | 触发条件 |
|---|---|---|---|
| `segments` | `segment_id` | `characters_in_segment` | narration + storyboard |
| `scenes` | `scene_id` | `characters_in_scene` | drama + storyboard |
| `shots` | `shot_id` | `characters_in_shot` | ad（恒定） |
| `video_units` | `unit_id` | 无 | narration/drama + reference_video |

条目字段：`<id_field>`（须匹配 `^E\d+S\d+(_\d+)?$`）、`duration_seconds>0`、
`<chars_field>[]`、`scenes[]/props[]/products_in_shot[]`、`novel_text`（segments 必填）、
`utterances[{kind:dialogue|voiceover, speaker?, text}]`、`source_text?`、
`voiceover_text`（ad 必填）、`section?`、`image_prompt`（非空必填）、`video_prompt`（非空必填）、
`end_frame_image?`（必须在 `end_frames/` 内）、
`generated_assets{storyboard_image, storyboard_last_image, video_clip, video_thumbnail,
                  video_uri, narration_audio, grid_id?, grid_cell_index?, status, video_generated_at?}`。

`metadata.episode_duration_plan{target_seconds, strategy, manual_allocations}`。

## 7. 单一真相源判定

| 数据 | 真相源 |
|---|---|
| 角色/场景/道具/产品目录 | **`project.json`**（`assets` 表是跨项目全局库副本） |
| 分集清单与账本坐标 | **`project.json.episodes[]`** |
| 镜头/分镜数组 | **`scripts/episode_N.json`**（DB 无对应表） |
| 媒体文件位置 | **`generated_assets.*` 相对路径**（`media_assets` 表只是只读索引，受 feature flag 控制） |
| 供应商配置 / API Key | **DB** |
| 供应商/模型目录 | **代码常量**（Go 侧 `internal/config/registry.go`） |
| 生成路线 | **`project.json.generation_mode`**（剧本不携带） |
| 版本历史 | **`versions/versions.json` + `versions/{type}/{id}_v{n}_{ts}{ext}`** |

## 8. 文件系统布局

```
projects/
├── .shotwise.db(+ -wal/-shm)
├── _global_assets/{character,scene,prop}/
└── <project_name>/          # 名字须匹配 ^[A-Za-z0-9-]+$
    ├── project.json
    ├── source/ scripts/ drafts/ characters/ scenes/ props/ products/
    ├── storyboards/ videos/ audio/ end_frames/ reference_videos/
    ├── thumbnails/ output/ grids/ versions/
    └── style_reference.{png,jpg,jpeg,webp}
```

写入约定：所有 JSON 走**原子写**（同目录临时文件 + `os.replace`），`indent=2`、UTF-8 无转义。
读：严格版与容错版各一个（`load_json` / `load_json_or_none`）。

锁：`project.json` 一把锁、剧本文件一把锁（`EpisodeScriptReboundError` 并发改绑重试）、`versions.json` 全局 `sync.Map` + mutex。

## 9. 路径安全

唯一校验入口 `safe_join(base, *parts)`：
- 用 `realpath` + **前缀字符串比较**（不用 `filepath.Rel`，因为这是 CodeQL 能识别的 sanitizer 收敛模式）。
- base 末尾预补分隔符，避免根/盘符根场景误判。
- 传绝对路径时等价于「校验该绝对路径是否在 base 内」。
- `candidate == base` 单独判支，避免 `allow_base=false` 被绕过。
- 非法字符（NUL 等）统一转成 `PathTraversalError`，不退化成 500。

Go 侧注意 Windows：`\\?\` 扩展长度前缀、大小写不敏感、NFD/NFC 归一 → 比较前统一 `normcase + casefold + NFC`。
