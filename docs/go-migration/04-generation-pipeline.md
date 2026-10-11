# 生成流水线迁移

## 1. 模块地图

| 层 | Python | Go |
|---|---|---|
| 入队 API | `lib/generation_queue_client.py` | `internal/queue/client.go` |
| 队列门面 | `lib/generation_queue.py` | `internal/queue/queue.go` |
| 持久化 | `lib/db/repositories/task_repo.py` | `internal/repository/task.go` |
| 调度 | `lib/generation_worker.py` | `internal/queue/worker.go` |
| 健康 | `lib/provider_health.go` | `internal/queue/health.go` |
| 配置解析 | `lib/config/resolver.py` | `internal/config/resolver.go` |
| 执行门面 | `lib/media_generator.py` | `internal/service/generator.go` |
| 派发 | `server/services/generation_tasks.py`（19 种 task_type） | `internal/service/tasks.go` |
| 记账 | `lib/ledger.py` → `usage_repo.py` → `cost_calculator.py` → `pricing/` | `internal/pricing/` |

## 2. 任务队列

### 2.1 入队 → 派生执行身份

```
enqueue_task(project, task_type, media_type, resource_id, payload, ...)
  ├─ provider_id == nil → 解析（video 先定桶 i2v/r2v，再 resolver 解析）
  ├─ 视频：payload["video_provider_{i2v|r2v}"] = "provider/model"   ← 锁定，restart 沿用
  └─ repo.enqueue → {task_id, status, deduped, existing_task_id}
```

结构校验单点 `TaskSpec.from_request()`：
- `video`：prompt 是 string 或 action 对象，action 非空，dialogue 必须是 list
- `storyboard`：结构化 scene 对象，`scene` 非空
- `tts`：prompt 可空，非空须为非空串
- 保留键 `prompt`/`script_file`（+ tts 的 `text`）禁止从 `extra_payload` 注入

批量入队：`prepare_all()`（事务外归一化）→ `admit_all()`（单次原子）；**不**去重回退，冲突即整批回滚。

### 2.2 去重（在 DB，不在应用层）

```sql
CREATE UNIQUE INDEX idx_tasks_dedupe_active ON tasks (
  project_name, task_type, resource_id,
  COALESCE(script_file,''), COALESCE(resource_type,'')
) WHERE status IN ('queued','running','cancelling');
```
冲突 → 按 `queued_at DESC` 回查 1 条 → `{deduped:true, existing_task_id}`。
只有活跃态占用去重键；落到终态后同键可再次入队。
`resource_type` 仅 `image_edit` 写入（该 task_type 跨四类资产，资源命名空间不互斥）。

### 2.3 优先级

**不存在优先级**。`ORDER BY queued_at ASC LIMIT 1`，纯 FIFO。
真正的调度权重在两条正交轴上：per-(provider, media) 并发上限 + 熔断折减/池满黑名单。

### 2.4 依赖

字段 `dependency_task_id` / `dependency_group` / `dependency_index`（后两者只是元信息）。
claim 守卫：`dependency_task_id IS NULL OR dependency.status='succeeded'`。
- 前驱 failed → 递归把 `queued` 下游标 failed（`cascade_blocked_dependency`，预算裁剪防 JSON 指数增长）。
- 前驱 cancelled → 递归级联取消。
- 规划器 `build_storyboard_dependency_plan()`：按 `segment_break` 切组，组内串行链、组间并行。

### 2.5 状态机

| status | 类型 | 进入 | 出口 |
|---|---|---|---|
| `queued` | 活跃 | enqueue / 回队 / requeue_running | claim→running；cancel→cancelled；前驱 failed→failed |
| `running` | 活跃 | claim 的 `UPDATE ... WHERE status='queued'` | succeeded/failed；cancel→cancelling |
| `cancelling` | 活跃(中间) | `_mark_cancelling WHERE status='running'` | worker finally / drain / `finalize_cancelling` → cancelled |
| `succeeded` / `failed` / `cancelled` | 终态 | | |

取消语义（ADR 0006「秒级响应」）：两级 = DB 状态 + 进程内取消。
1. `queued` → 直接终态 + 递归级联；`running` → 落 `cancelling`，**不立即级联下游**；终态 → skipped。
2. 返回的 `cancelling` 列表**同步**调 worker 取消回调（不用 fire-and-forget，否则延迟到下次调度）。
3. worker 侧取消后落终态，**DB 写入用不带 cancel 的 context**（等价于 Python 的 `asyncio.shield`）。
4. `cancel_all_active()`：worker 离线时直接把 `cancelling` 批量落 `cancelled`。

`cancelled_by` ∈ `user` / `cascade` / `episode_deleted`，用 `coalesce` 写入（首次优先）。

**终态事件在事务提交后统一 emit**（不是 repo 内 emit）。

### 2.6 error_message 编码

`[code] {json params}`，上限 2000 字符。机器码落库，文案读侧按 Accept-Language 渲染。
code 表：`lib/task_failure.py`（能力类 34 项 + 通用 20 项）。

## 3. Worker 并发模型

**不是一把 Semaphore，而是「容量表 + 占用台账 + 健康账」三件套。**

### 3.1 三通道 lane
`media_type` → `video` / `audio` / `image`（`reference_video`→video，`tts`→audio）。

### 3.2 CapacityTable（上限）
`provider_id → {media_type → 上限}`。来源三层回退：
env（`IMAGE_MAX_WORKERS=5`/`VIDEO_MAX_WORKERS=3`/`AUDIO_MAX_WORKERS=10`）→ DB（ConfigService + registry + 自定义供应商列）→ lane 默认值。
三态：已知+lane 在表→登记值；已知+lane 不在表→**0（不支持）**；provider 未知→懒默认。

### 3.3 SlotTable（占用台账）
`map[(provider, media)] → map[taskID]*Occupant{future, pending}`，空 bucket 自动删除（黑名单决策支点）。

### 3.4 主循环
```
lease = acquire_or_renew(ttl=10s)      // 心跳即每圈续约，无独立线程
drain_finished()
lease 丢失 > 3×ttl ⇒ 允许重扫 orphan
if 持 lease 且首次: 处理孤儿任务
claimed = claim_tasks()
sleep(0.05 if claimed else 1.0)
```
常量：`TASK_WORKER_LEASE_TTL_SEC=10`、`HEARTBEAT=3`、`POLL_INTERVAL=1`（构造时做下界 clamp）。

### 3.5 认领
```
for media in (image, video, audio):
  pool_full = 每轮重算
  task = claim_next(media, pool_full_providers)
  容量 <= 0   → mark_failed(provider_unsupported_media)      // 不支持 = fail-fast
  熔断后容量 <= 0 → 回队等半开探针; break                    // 熔断 ≠ 失败
  无空位      → 回队; break                                  // 池满 ≠ 失败
  注册占用 → 起 goroutine 执行
```

**池满过滤用黑名单而非白名单**：`AND (provider_id IS NULL OR provider_id NOT IN :providers)`。
白名单会把「已知但已删除/禁用的 provider」任务永久过滤掉造成静默堆积；黑名单只排除已知池满，`NULL` 老数据照常 claim 交 worker 二次校验。

### 3.6 熔断
三态 CLOSED / OPEN / HALF_OPEN；`failure_threshold=3`、`open_seconds=60`、`max_open_seconds=600`。
容量：OPEN→0，HALF_OPEN→1，CLOSED→`max(1, base - consecutive_failures)`。
**只有 transient 失败计账**，能力/参数类错误不计。

### 3.7 视频 resume 才用 Semaphore
`x/sync/semaphore.Weighted`；父协程先起 goroutine 再**同步**以 PENDING 登记（避免异步调度未触发时 `has_room` 误判），acquire 成功后 promote → INFLIGHT。

### 3.8 超时 / 重试
- **无 worker 级超时**，超时在 backend 轮询层：各家 `max_wait` 不同（agnes/dashscope/kling/minimax `max(900, dur*60)`；ark default 档 `max(600, 60*dur)`、非 default 3600；openai `max(600, dur*30)`；gemini 600）。
- 重试：默认 3 次 / backoff (2,4,8) + jitter；下载 5 次 / (5,10,20,40)。
- **非幂等 POST 的歧义态不重试**：提交时的连接类/读超时要区分「确定未送达」（可重试）与「可能已送达」（`AmbiguousSubmitError`，终态失败）。
- 三套重试谓词：submit（Connect 类 + 5xx/408/425/429）、poll（404 视为未就绪，重试）、download（404 fail-fast）。

### 3.9 租约与孤儿
- `worker_lease` 表；`acquire_or_renew`（fast path UPDATE `owner==自己 OR lease_until<=now`，slow path INSERT 抢锁）。
- 启动孤儿扫描：self-active 防 self-preemption；`cancelling`→cancelled；image/audio running→`[restart_lost]`（无 job_id，重跑=双重扣费）；video + `NON_RESUMABLE_VIDEO_PROVIDERS`（Grok/Vidu）→`[resume_unsupported]`；有 job_id 的 video 按 provider 分桶交后台受容量约束分批。
- 启动收口 pending 记账：`settle_interrupted_calls`（时间下界 = worker 构造时刻）。

## 4. Backend 抽象

### 4.1 接口

```go
type VideoBackend interface {
    Name() string
    Model() string
    VideoCapabilities() VideoCapabilities
    Generate(ctx, req VideoGenerationRequest) (VideoGenerationResult, error)
    ResumeVideo(ctx, jobID string, req VideoGenerationRequest) (VideoGenerationResult, error) // 未实现返回 ErrResumeUnsupported
}

type ImageBackend interface {
    Name() string; Model() string
    Capabilities() set[ImageCapability]  // TEXT_TO_IMAGE | IMAGE_TO_IMAGE
    Generate(ctx, req ImageGenerationRequest) (ImageGenerationResult, error)
}

type AudioBackend interface {
    Name() string; Model() string
    Capabilities() set[AudioCapability]  // TEXT_TO_SPEECH
    ListVoices() []VoiceOption           // 同步
    Synthesize(ctx, req AudioSynthesisRequest) (AudioSynthesisResult, error)
}
```

`VideoCapabilities` 的 10 个字段语义（first_frame / last_frame / max_reference_images / reference_audio_mode / max_reference_audio_count / max_reference_audio_total_seconds / reference_audio_per_image / max_prompt_chars / first_frame_ratio_adaptive_only）是后端能力契约的唯一真相源，**原样迁移，注释一并搬**。

### 4.2 共享基础设施（做成包级函数）

- `persistProviderJobID()` — submit 后**立即**落库 `provider_job_id`（+endpoint），DB 瞬态错误重试 3 次 (1,2,4)，用尽则抛（fail-fast，防"幽灵任务"）
- `persistAPICallID()`
- `pollWithRetry()` — 通用轮询：`pollFn` / `isDone` / `isFailed` / `interval` / `maxWait` / `retryIf` / `onProgress`
- `submitPost()` / `downloadVideo()`

**能力闸门先于记账括号**：`gate_video_request()` 在 `media_generator` 调用，失败不扣费、不留 failed ApiCall 行。

### 4.3 实现清单

**视频 11 个类 / 11 个 key**：gemini（`gemini-aistudio`/`gemini-vertex` 同映射）、ark + ark-agent-plan、grok（**不可 resume**）、openai、newapi、vidu（**不可 resume**）、dashscope、minimax、kling、agnes、`v2_video_generations`（未注册，作自定义供应商 delegate）。

**图片 9 个类 / 10 个 key**：gemini、ark(+agent-plan)、grok、openai、vidu、dashscope、minimax、kling、agnes。
**音频 2 个类**：dashscope（registry 唯一注册）、openai（未注册，作 delegate）。
另有自定义供应商声明式包装层（Declarative{Text,Image,Video,Audio}Delegate + Custom{Text,Image,Audio,Video}Backend）。

### 4.4 异步长任务模式

```
submit → 立即持久化 job_id → poll → download
resume → 只 poll + download，绝不重新 submit（ADR 0007）
```
各家 job_id 字段名与终态识别不同（`task_id` / `id` / `video.id`；失败态枚举不只认 `failed`，Agnes 认 `failed/error/cancelled/canceled`）。
计费时长：Agnes 不读 `usage.duration_seconds`（那是处理耗时）；half-up 取整、clamp 到 86400。

**endpoint 列承载两种语义**：自定义供应商存 endpoint 标识（协议维度），内置供应商存实际请求域名（连接维度）。resume 时比对，变了抛 `ResumeEndpointChangedError`。

## 5. ffmpeg

**关键结论：Python 侧没有用 ffmpeg-python**（声明了但零 import），全是 `create_subprocess_exec(argv)` 无 shell → Go 直接 `exec.CommandContext` 1:1 对应。

调用点：缩略图首帧、帧数探测、按 index 抽帧、尾帧提取、音频时长/容器探测、多段参考音频总时长、
拼片 filter_complex（scale+pad+setsar+fps+format+setpts / aresample+aformat+asetpts / anullsrc / xfade+acrossfade / concat）、
render_video（libx264 + aac + faststart）、低清预览、片头片尾包装、字幕烧录、音频合成（volume/afade/adelay/amix）、
黑帧检测、响度检测（ebur128）、静音段检测、审阅抽帧。

降级策略（统一口径）：
1. `exec.LookPath` 探测 + `sync.OnceValue` 缓存，**ffmpeg 与 ffprobe 独立检查**（精简容器可能只装一个）。
2. 缺失即跳过不阻断（缩略图/尾帧/音频时长返回 None）；**渲染路径相反，抛 `RenderToolError`**。
3. 分级超时（metadata 10s / count_frames 120s / 抽帧 60s / 缩略图 30s / ffprobe 10s），超时 kill + wait。
4. 长任务**连续排空 stderr**（goroutine 读 `StderrPipe`），否则管道满死锁；正则 `time=(\d+):(\d{2}):(\d{2}(\.\d+)?)` 算进度；`cancel_check` 每 0.5s 轮询 → kill。
5. **SSRF 防护**：`-protocol_whitelist file` 出现在每一处（防 HLS/RTMP 播放列表跟随发起网络请求）。
6. 产物后置校验（probe + `size>0` 双检）。
7. 临时文件中转 + 原子替换。

Windows 特有：
- ffmpeg 滤镜路径转义 `as_posix().replace("\\","\\\\").replace(":","\\:").replace("'","\\'")`（盘符冒号 + 反斜杠）。
- PIL 持有文件句柄导致覆写失败 → Go 侧用 `image.Decode` 后立即释放，不等 GC。
- 文件占用导致删除失败 → 只告警不失败。
- 端口 1241 被本机保留 → 忽略，回落 18080。

## 6. 计费

### 6.1 记账括号

三条落库通道：`Ledger.record()`（pending → finish）、`resume_success/resume_failed`（按 `api_call_id` 幂等，`WHERE status='pending'`）、`Ledger.backfill()`（事后补录）。
生成路径：image / image(canvas 独立输出) / audio / video / text。**resume 路径不开括号**（防双重扣费）。
取消收口：`resume_failed()`；启动收口：把 `started_before` 之前的 pending 翻 failed（`interrupted_by_restart`，零费用不猜价）。

Go 侧替代 asynccontextmanager：
```go
call, err := ledger.Start(...)
defer func() { if err != nil && !errors.Is(err, context.Canceled) { ledger.FinishFailed(...) } }()
...
ledger.FinishSuccess(call, ...)   // 「正常退出未声明 success → 报错」这条不变式必须保留
```

### 6.2 定价

链路：`_settle()` → `calculate_cost()` → `lookup_pricing()` → `calculate_pricing()`。

结算前置：
- `duration_ms = finished - started`（SQLite 回读为 naive，按 UTC 补齐）
- 计费时长覆盖：`0 < billed <= 86400` 才覆盖请求时长
- OpenAI 图片 token 聚合：`input = image_in + text_in`，`output = image_out + text_out`
- 显式 `cost_amount`（SDK 直报）优先于自动计算

**10 种定价形状**（`kind` 为判别标签）：
`per_token`、`per_image_flat`、`per_image_by_resolution`、`per_image_openai_token`、`per_second_matrix`、
`per_second_tiered`（Kling，4K 独立成档）、`per_video_bucket`（MiniMax 离散档，未命中取最近档）、
`per_token_video`（Ark 按 service_tier × generate_audio）、`per_character`（TTS 每万字符）、`vidu_delegate`。

回退链：`anthropic` → 内置表；`vidu` → delegate（provider 级，不经 model 回落）；
registry 命中且 media 匹配且有 pricing → 该声明；否则本 provider 默认 model → 其余一律 gemini 兜底。
按秒计费视频 `duration` 缺失/0 → 补 8。

## 7. 宫格 grid

- `GridGeneration.split_at` 把「联合图就绪」与「已落格」解耦；联合图内容变更时清空 `split_at`。
- `video_aspect_ratio` **冻结在记录上**（项目比例随时可改，隔很久再切会按新比例裁切）。
- 切分梯子 `(4,2×2) (9,3×3) (16,4×4) (25,5×5)` 全为 N×N 方形；大宫格仅 4K 分辨率放行，否则回落 2K，上限 9 格。
- 生成只产联合图，不触碰分镜格；切分时 `versions.ensure_current_tracked()` 先补登旧文件版本再写。
- **占位格检测用 numpy**（`std<15 && mean<160`）→ Go 手写像素遍历。

**PIL 用到的全部能力**：`Image.open`、`ImageOps.exif_transpose`、`convert`、`resize(LANCZOS)`、
`save(PNG/JPEG, quality, optimize, subsampling)`、`verify`、`load`（完整解码防截断）、EXIF Orientation(0x0112)、
`copy`（脱离文件句柄）、`ImageOps.fit(LANCZOS)`。
Go 方案：`golang.org/x/image/draw`（CatmullRom ≈ LANCZOS）、`image/jpeg`、`image/png`、`golang.org/x/image/webp`；EXIF 用第三方；`ImageOps.fit` 自己写约 20 行。

**参考图压缩梯子**（CPU 密集 → goroutine 跑，但要限并发避免打满核）：
- `FRAME`（首/尾帧）**永不缩尺寸**，仅超预算才 JPEG q92/4:4:4 重编码
- `ARRAY` 按档重编码；step==0 且已 JPEG + <1MB + 尺寸合规 → 原样透传
- 413 兜底：从 `landed+1` 续档，到地板抛 `ReferencePayloadFloorError`
- **临时文件名沿用源 stem**（gemini 按文件名推断参考图名称，随机名会毁掉 I2I 角色识别）
- 保序保数：透传项按原 spec 序位合并回完整列表

## 8. Python 特有依赖 → Go 对应

| Python | Go |
|---|---|
| `asyncio.Semaphore`（仅 video resume） | `golang.org/x/sync/semaphore.Weighted`。**主调度不要塞进 semaphore**，用 `sync.RWMutex` 保护的 `map[providerMediaKey]*lane` |
| `asyncio.Task.cancel()` | `context.WithCancel` + `map[taskID]CancelFunc` |
| `asyncio.shield()`（DB 写入不被取消打断） | DB 写入用**不带 cancel 的独立 context**（天然等价且更安全） |
| `asyncio.Event` | `chan struct{}` + `close()` 广播 |
| `asyncio.gather/wait` | `errgroup.Group` |
| `asyncio.to_thread`（CPU 卸载） | **不需要**（goroutine 直接跑）；但图像处理仍要 `errgroup.SetLimit` 限并发 |
| PIL / Pillow | `golang.org/x/image/draw` 等，见 §7 |
| numpy（仅占位格检测） | 手写循环 |
| ffmpeg-python | **未使用**，无需迁移 |
| httpx（错误分类） | `net/http`；用 `errors.As(&net.OpError{})` + `Op=="dial"` 区分「确定未送达」 |
| openai SDK | 无官方 Go SDK 覆盖 `videos`，**直接 REST**；404 ↔ `NotFoundError` |
| google-genai SDK | 只用 generate/poll/download 三件事，**REST 更可控**；Vertex 走 `x/oauth2/google` |
| SQLAlchemy async | GORM + 双驱动 |
| claim_next 原子性 | 建议改**单条原子语句**：PG `UPDATE ... WHERE task_id=(SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING *`；SQLite `BEGIN IMMEDIATE` 事务 |
| pydantic | `go-playground/validator` + struct tag |
| `functools.cache` | `sync.OnceValue` |
| `contextvars` | 显式参数传递（推荐）或 `context.WithValue` |
| dataclass + StrEnum | struct + `type Xxx string` 常量 |

## 9. 六个最易踩坑的不变式

1. 去重 = DB 部分唯一索引，**不是**应用层查重（改成 SELECT-then-INSERT 会引入竞态）。
2. `mark_succeeded`/`mark_failed` 返回受影响行数，**0 行 = 外部已翻状态**，不当错误处理。
3. `cancelling` 是中间态，必须有收口，否则 worker 离线时任务永久悬停。
4. 池满用**黑名单**，且 `provider_id IS NULL` 必须放行。
5. 非幂等 POST 的歧义态不重试，否则重复建任务 + 重复计费。
6. resume **绝不重新 submit**；不可 resume 的供应商必须显式登记。
