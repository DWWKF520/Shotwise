# 智能体运行时：Claude Agent SDK → Eino

## 1. 现状架构

```
router/assistant.py
  → AssistantService（幂等键、skill 列表、SSE 编排）
  → SessionManager（容量/巡检/驱逐、ManagedSession 内存态）
  → SessionActor（每会话一个 asyncio task，串行化 SDK 调用）
  → ClaudeSDKClient（起 claude.exe 子进程，--output-format stream-json）
      ├── 内置工具 Skill/Task/Bash/BashOutput/KillBash/Read/Write/Edit/Grep/Glob/AskUserQuestion
      ├── PreToolUse/PostToolUse hooks + canUseTool 权限链
      ├── in-process MCP "shotwise"（37 工具，handler 跑主进程）
      ├── remote MCP（HTTP JSON-RPC）
      ├── SandboxSettings（bwrap/seatbelt）
      └── SessionStore 镜像（DB）
```

`SessionActor` 串行化的唯一原因：SDK 内部跑在 anyio 上，跨 task 持锁调用会死锁（ADR 0028）。
**Eino 是纯 Go 库，Agent 就是 goroutine —— 这一层约束消失。**

## 2. 能力清单与 Eino 替代评估

| # | 能力 | Eino 现成度 | 方案 |
|---|---|---|---|
| 1 | 子进程/进程模型 | — | 不需要了；goroutine + channel |
| 2 | Skills（SKILL.md） | ★★★★☆ | `adk/middlewares/skill`；**需自定义 Backend** 支持 `SKILL.<mode>.md` 变体与 `user-invocable` |
| 3 | Subagent | ★★★★☆ | `adk.SetSubAgents` / `NewAgentTool` / deep / supervisor；**需自研 `.claude/agents/*.md` loader** |
| 4 | 内置工具集 | ★★★☆☆ | `adk/filesystem` backend 覆盖 Read/Write/Edit/Glob/Grep；**Bash 无内置** |
| 5 | 权限与 hooks | ★☆☆☆☆ | **自研 GuardedTool 装饰器**（★核心） |
| 6 | in-process MCP | ★☆☆☆☆ | 不需要了；37 工具直接实现 `tool.InvokableTool` |
| 7 | remote MCP | ★★★★☆ | `eino-ext/components/tool/mcp`（stdio/sse/streamable-http） |
| 8 | transcript / checkpoint | ★★★☆☆ | `compose.CheckPointStore` 有接口无实现 → SQL 版自研 |
| 9 | branching / resume | ★★☆☆☆ | 有 checkpoint/resume，**无前缀分叉语义** → 基于事件日志复制自研 |
| 10 | AskUserQuestion / HITL | ★★★★★ | `compose.Interrupt` + `Runner.ResumeWithParams`（且持久化，优于现状） |
| 11 | structured output | ★★★★☆ | `WithJSONSchema` / `utils.InferTool[T]` |
| 12 | 流式 delta | ★★★★★ | 原生；自动 concat/copy/box |
| 13 | 中断 | ★★★★★ | v0.9 Agent Cancel（安全点、递归、超时升级、取消期 checkpoint） |
| 14 | 多模态 | ★★★☆☆ | `schema.Message.MultiContent` |
| 15 | 沙箱 | ☆☆☆☆☆ | **完全没有** → bwrap 包 Bash 工具 / Windows 白名单 |
| 16 | usage / cost | ★★☆☆☆ | token usage 有，**cost 聚合自研** |
| 17 | 后台任务通知 | ☆☆☆☆☆ | 无等价；走 agent event 流重新设计 |

## 3. 映射表

| Shotwise（Python） | Eino / Go |
|---|---|
| `SessionActor` + `_drive_query` 交织 | goroutine + `adk.TurnLoop`，`Push` 抢占代替 interrupt |
| `ManagedSession` | 自建（内存注册表 + per-session mutex） |
| `_process_inbox` 异步后处理 | goroutine + channel |
| `SseChannel` 溢出策略 | 自建 `EvictNonCriticalAndSignal` |
| `client_factory` 鸭子类型缝 | 保留同名 Go interface：`Query/ReceiveResponse/Interrupt/Close` |
| `ClaudeSDKClient` ReAct 循环 | `adk.NewChatModelAgent` + ToolsNode |
| AskUserQuestion | `compose.Interrupt` / `adk.Interrupt` + `GetResumeContext` |
| `pending_questions` 快照 | `event.Action.Interrupted.InterruptContexts[].ID` |
| Pre/PostToolUse hooks | **GuardedTool 装饰器**（在 `InvokableRun` 前后调 policy） |
| `canUseTool` deny | GuardedTool 返回 error message 给模型 |
| `PermissionResultAllow(updated_input=)` | GuardedTool 内改写 args 后传给真实 Tool |
| 37 个 MCP 工具 | `tool.InvokableTool` 直连（`utils.InferTool[T]` 自动生成 JSON Schema） |
| `DraftAccumulator` | 自建，但更简单（Eino chunk 自带 index 语义） |
| `sdk_id` 懒生成 + key swap | 保留自研（60s 超时） |
| 并发上限 / 巡检 / 驱逐 | 保留自研 |

## 4. 必须自研的模块

### 4.1 GuardedTool（★核心，替代 hooks + canUseTool）

Eino 没有 5 步权限链（Hooks → Deny → Permission mode → Allow → canUseTool）。
在 `compose.ToolsNodeConfig` 里包一层装饰器，覆盖全部三类 hook：

| 现有 hook | GuardedTool 内的实现 |
|---|---|
| `_file_access_hook`（读写围栏） | 调 `policy.CheckRead/CheckWrite` |
| `_bash_env_scrub_hook`（Bash env 剥离） | 改写 args：包成 `env -u <密钥变量> sh -c '<cmd>'`（仅 POSIX） |
| `_json_validation_hook`（Write/Edit JSON 预校验） | `json.Valid()` 检查；Post 阶段失败回写备份 |
| Windows Bash 白名单 | `policy.IsBashCommandWhitelisted` 四步判定 |

四步判定：①拒 shell metachar `[;&|<>``$\r\n]` ②拒 `..` 路径段（原串 + 剥引号 + 反斜杠两种归一化各查一遍）③token 边界匹配 ④python 入口必须是 `.claude/skills/<skill>/scripts/<script>.py`。
白名单前缀：`python .claude/skills/`、`ffmpeg`、`ffprobe`。`BashOutput`/`KillBash` 直接放行。

### 4.2 AgentAccessPolicy（757 行 Python → Go）

单一真相源 `PROTECTED_WRITE_RULES`，每行同时声明 **hook 谓词**与 **sandbox 投影**：

| 规则 | hook 谓词 | sandbox_subpaths | 理由 |
|---|---|---|---|
| `project_json` | 拒 `scripts/` 整子树 + 根 `project.json` | `("scripts","project.json")` | 写入口收归工具 |
| `reference_step1` | 只拒 `drafts/episode_N/step1_reference_units.json` 正式文件 | `("drafts",)` | 三条路径持同一把 per-path 锁，Write/Edit 取不到锁会丢更新 |

两层**刻意不对称**：sandbox 按 `drafts/` 整目录 deny（清单装配期一次性构造），hook 只拒正式文件 —— 同目录的 `.invalid.json` 隔离草稿是留给内置 Edit 的编辑工位。

其余：代码扩展名禁写 `{.py,.js,.ts,.tsx,.sh,.yaml,.yml,.toml}`；
敏感读路径 deny（`.env`、`.env.*`、`vertex_keys/`、`.system_config.json*`、`.shotwise.db*`、profile settings.json、日志目录）；
读放行例外（`~/.claude/projects/.../tool-results`、`/tmp/claude-*/tasks`、仓库根参考资料）；
**raw + resolved 双 base** 枚举（防 macOS `/var↔/private/var`、Linux symlink 项目根绕过）。

### 4.3 沙箱

- Linux/macOS：给 Bash 工具套 `bwrap`（`--unshare-user --unshare-net --unshare-pid --ro-bind / /`）+ 网络白名单（anthropic.com + dev 域名）。
- Windows：无 bwrap → 回退命令前缀白名单。
- **面比现在更小**：in-process 工具跑在主进程（不受沙箱约束），真正需要沙箱的只剩 Bash + 内置文件工具，文件工具走 `GuardedTool` 路径校验即可。

### 4.4 事件日志（transcript）

保留 append-only + `seq` 的设计（DB 表已存在）。Eino 的 `schema.Message` 没有 `uuid`/`parentUuid`/`sidechain`/`toolUseResult` 这些 jsonl 语义，
**因此不再伪造 SDK transcript**，而是：事件日志 = 权威 transcript，分叉 = 复制 entries 到新 session_id + 用新 session_id 起 Agent 并把 prefix entries 作为历史消息注入。这实际上比现状更简单。

entry 定型契约保留：`type`(user/assistant/tool_result/system) × `subtype`(interrupt/task_notification/question_answer/agent_turn_failure/skill_invocation)、
`content[]`、`uuid`、`timestamp`、`parent_tool_use_id`（三形态归一：`parent_tool_use_id`/`parentToolUseID`/`parentToolUseId`）、`message_id`、`tool_use_id`。

有意思的巧合：Eino 的 skill prompt 也是 `"Base directory for this skill: %s"`，与 Claude 一致 —— `event_log` 的 skill 注入识别前缀可原样复用。

### 4.5 profile_manifest 同步（约 500 行 Go）

- 变体解析：`CLAUDE.narration.md` → `(logical="CLAUDE.md", mode="narration")`；通用 + 变体互斥，变体必须 3 个 mode 齐全。
- 决策表：`{P存/缺} × {D存/缺} × {M无/active/tombstone}` exhaustive，未列状态报错。
- **mode 切换 = reset**：manifest 的 `content_mode` 与请求不一致 → 整棵 `.claude` + `CLAUDE.md` 全删重建。
- 文件锁用 `O_NOFOLLOW` 语义防 symlink 攻击（Go 侧 `os.OpenFile` 后检查 `Lstat` 非符号链接）。

### 4.6 system prompt 组装顺序

`_PERSONA_PROMPT`（身份 + 行为准则）→ `PROJECT_DOCUMENT_WORKFLOW`（强制文稿工作流）→
语言规范（zh/en/vi，覆盖回答/视频内容/文档/Prompt）→ 项目上下文（标识 + cwd 绝对路径 + "Bash 必须单行"）→
长期记忆（≤20 条 / 8000 字符，显式标注"不可信"）→ 跨 Agent 切换的历史提示。

项目 `CLAUDE.md` 由 Eino 侧从 cwd 加载（不再靠 SDK 的 `setting_sources`）。

## 5. 可直接平移（与 SDK 无关的部分）

| 模块 | 行数 | 说明 |
|---|---|---|
| `event_log.py` 定型器 | 935 | 语义保留，只换输入源 |
| `entry_pipeline.py` | 380 | draft 累积可简化 |
| `service.py` 幂等键编排 | ~400 | client_key LRU + DB 兜底 |
| `usage_extraction.py` / `result_status.py` | 188 | 纯函数 |
| `failure_observation.py` | 155 | 纯函数 |
| `keyed_locks.py` | 36 | `sync.Map` + mutex |
| `mcp_broker.py` | 361 | 授权/校验/审计 |
| `turn_schema.py` | 117 | content block 归一化 |
| `agent_access_policy.py` | 757 | 纯逻辑，注释明说不 import SDK 类型 |

## 6. 37 个工具清单

**对外 31 个**：`list_project_text_files`、`read_project_text`、`list_pending_assets`、`generate_assets`、
`generate_storyboards`、`generate_grid`、`edit_images`、`generate_video_episode`、`generate_video_scene`、
`generate_video_all`、`generate_video_selected`、`generate_narration_audio`、`get_video_capabilities`、
`generate_episode_script`、`confirm_script_review`、`normalize_drama_script`、`split_reference_video_units`、
`open_reference_step1_for_edit`、`validate_and_promote_reference_draft`、`split_narration_segments`、
`plan_episodes`、`reset_episode_planning`、`patch_episode_script`、`insert_segment`、`remove_segment`、
`split_segment`、`patch_episode_meta`、`patch_project`、`rename_asset`、`analyze_character_relations`、
`propose_memory_candidate`、`inspect_director_review`。

**内部 5 个**：`get_episode_media_manifest`、`create_timeline_plan`、`get_timeline_plan`、`update_timeline_plan`、`validate_timeline_plan`。

工具错误约定：`{"content":[{"type":"text",...}],"is_error":true}`，reason 截断 2000 字符，instructions 上限 4000 字符。
`project_name` 闭包绑定（防 prompt injection 改项目）。

## 7. 风险清单

| 风险 | 等级 | 说明 |
|---|---|---|
| **模型能力退化** | 高 | Claude Code preset 自带大量内建行为（TodoWrite、Edit 的 old_string 容错、上下文压缩），Eino 的 ChatModelAgent 是通用 ReAct，需靠 system prompt 重建 |
| **prompt 重写** | 高 | 3 个 `CLAUDE.*.md` + 11 个 SKILL.md 全部按 Claude Code 语义编写，含大量 `mcp__shotwise__*` 引用，需系统性改写 |
| **沙箱缺失** | 高 | ADR 0025 是硬失败设计，Eino 下需重新论证安全模型 |
| **存量 transcript 不兼容** | 中 | 存量会话 DB 镜像在新运行时下无法直接消费，需一次性迁移 |
| **双通道（claude ↔ openai）切换** | 中 | `switch_agent` / `resolve_sdk_session_id` / `claude_resume_id` 的跨 SDK 续接设计整体失效，需重新设计 |
| **Eino checkpoint 版本兼容** | 中 | v0.3.26 有过序列化断裂；`schema.RegisterName` 的 key 一旦确定不可改 |
| **测试资产重写** | 高 | `tests/agent_runtime/` 大量 case 基于 `ClaudeSDKClient` patch，需整体重写 |
| **Windows 支持** | 中 | 当前已是降级路径，Eino 下需重建 |

## 8. 建议的实施分层

```
第 0 层（与 Eino 无关，先做）
  ├─ Go 版 AgentRuntime interface：Query / ReceiveResponse / Interrupt / Close
  └─ SessionManager 语义平移：容量 / 巡检 / 驱逐 / sdk_id 懒生成 / key swap

第 1 层（Eino 直接替代）
  ├─ adk.NewChatModelAgent + ToolsNode      → 替代 ReAct 循环
  ├─ compose.Interrupt + CheckPointStore    → 替代 AskUserQuestion / interrupt
  ├─ adk/middlewares/skill（自定义 Backend）→ 替代 Skills
  ├─ adk/filesystem backend + Bash 工具     → 替代内置文件工具
  ├─ eino-ext tool/mcp                      → 替代 remote MCP
  └─ TurnLoop                               → 替代 SessionActor 多轮驱动

第 2 层（必须自研）
  ├─ GuardedTool 装饰器（★核心）
  ├─ AgentAccessPolicy Go 版（含 PROTECTED_WRITE_RULES 双投影）
  ├─ 沙箱执行器（bwrap 包装 / Windows 白名单）
  ├─ 37 个工具实现 tool.InvokableTool
  ├─ CheckPointStore SQL 实现 + 事件日志
  ├─ 前缀分叉（基于事件日志复制）
  ├─ profile_manifest 同步（变体 + 决策表）
  └─ SSE 层（entry/draft/question/delta/status + seq 游标）
```

---

以下 §9–§16 是上述分层的落地设计：每层用哪个 Eino API、接口长什么样、
语义对照是否成立。目标 Eino 版本 ≥ v0.9（Cancel / Retry / Failover / Enhanced middleware）。

## 9. 装配总设计

### 9.1 路线决策：经典 Message 路径

ADK 按 `MessageType` 泛型化（`*schema.Message` 经典路径 / `*schema.AgenticMessage` agentic 路径）。
**选经典路径**，理由：

1. 双通道硬需求：eino-ext 的 agentic 系列无 claude 实现（agenticopenai/gemini/deepseek/ark/qwen），
   经典路径 `model/claude` + `model/openai` 都是成熟实现；
2. 现有骨架（`server-go/internal/agent/`）已按经典路径起步，事件面（`schema.Message` 流式 chunk）语义一致；
3. 多模态需求用 `schema.Message.MultiContent` 覆盖（分镜图等图片往返），不依赖 agentic block。

### 9.2 装配总图

```
transport(SSE) ◄──── eventlog.Adapter ── AsyncIterator[AgentEvent] ──┐
                     （AgentEvent → entry → seq → SseChannel）        │
                                                                     ▼
        ┌──────────────────── adk.Runner ────────────────────┐
        │  CheckPointStore(SQL) · WithCheckPointID(sessionID) │
        │  EnableStreaming: true                              │
        └────────────────────▲────────────────────────────────┘
                             │ 每会话一个
        ┌──────────────────── adk.TurnLoop[InboxItem] ────────────┐
        │  Push(inbox) 排队 · Stop(WithGraceful) 收口             │
        │  GenInput → PrepareAgent(每轮装配) → OnAgentEvents      │
        └────────────────────▲────────────────────────────────────┘
                             │
        ┌───────────── adk.ChatModelAgent ───────────────────────┐
        │ Model: claude | openai（Retry + Failover，§15）         │
        │ Instruction: §4.6 顺序组装 + SessionValues 注入         │
        │ Handlers: patchtoolcalls → agentsmd → summarization    │
        │           → reduction → guard(★§12) → filesystem       │
        │           → skill(§11.3) → plantask                    │
        │ ToolsConfig: 37 工具(InvokableTool) + 6 Subagent       │
        │             (AgentTool) + ask_user(§13) + file/shell   │
        └─────────────────────────────────────────────────────────┘
```

### 9.3 Runtime 门面

第 0 层 interface 不变（`Query / ReceiveResponse / Interrupt / Close`），
实现委托给 per-session TurnLoop：

```go
type Runtime interface {
    Query(ctx context.Context, sessionID, project, message string) (<-chan Event, error)
    ReceiveResponse(ctx context.Context, sessionID string) (<-chan Event, error)
    Interrupt(ctx context.Context, sessionID string, opts ...InterruptOption) error
    Answer(ctx context.Context, sessionID string, answer AnswerPayload) error // HITL 续跑
    Branch(ctx context.Context, from SessionRef) (string, error)              // 前缀分叉
    Close(ctx context.Context) error
}
```

## 10. 会话驱动：TurnLoop 替代 SessionActor

### 10.1 语义对照

重读 `_drive_query` 确认：Python 侧新 query 在轮进行中是**暂存排队**（`pending_query`），
不是抢占；interrupt 是独立命令。因此 TurnLoop 的映射比 §3 预估的更直接：

| Python（SessionActor） | Eino（TurnLoop） |
|---|---|
| `_cmd_queue` + `enqueue` | `loop.Push(item)`（非阻塞） |
| query 排队 / drain before new query | 轮进行中 Push 自然排队，本轮事件流关闭后 `GenInput` 消费 |
| `interrupt` 命令（无新输入） | per-turn `adk.WithCancel()` 的 cancelFn，安全点 `CancelAfterToolCalls`；本轮以 `CancelError` 收尾，checkpoint 保留 |
| interrupt + 立即新 query（演进预留） | `Push(item, adk.WithPreempt(AfterToolCalls))`，v1 不启用 |
| `_drain_pending_commands`（actor 退出兜底） | `loop.Wait()` 的 `TurnLoopExitState.UnhandledItems` |
| task notification 注入会话 | worker 完成回调里 `loop.Push(InboxItem{Kind: task_notification})` |
| 巡检 / 空闲驱逐 | `UntilIdleFor(d)` 自动退出 + SessionManager sweep 落盘 |
| disconnect | `loop.Stop(adk.WithGraceful())` |

`InboxItem` 是自定义类型（`Kind: user_message | task_notification`），
`GenInput` 把一批 item 合成一轮输入：user_message 为主文本，
task_notification 折叠为系统提示消息附在同轮（与 `_process_inbox` 等价）。

### 10.2 骨架

```go
loop := adk.NewTurnLoop(adk.TurnLoopConfig[InboxItem, *schema.Message]{
    GenInput: func(ctx context.Context, l *adk.TurnLoop[InboxItem, *schema.Message], items []InboxItem) (*adk.GenInputResult[InboxItem, *schema.Message], error) {
        msgs := buildTurnMessages(items) // user 文本 + notification 折叠
        return &adk.GenInputResult[InboxItem, *schema.Message]{RunCtx: ctx, Input: &adk.AgentInput{Messages: msgs}, Consumed: items}, nil
    },
    PrepareAgent: func(ctx context.Context, l *adk.TurnLoop[InboxItem, *schema.Message], consumed []InboxItem) (adk.Agent, error) {
        return s.buildAgent(ctx) // 每轮重装配：CLAUDE.md / 长期记忆 / SessionValues 热更新，构造成本可忽略
    },
    OnAgentEvents: s.consumeEvents, // §14 事件适配：AgentEvent → event_log → SSE
})
loop.Run(ctx)
```

SessionManager（容量 / 驱逐 / key 语义）按 §3 保留自研，
`Session` 内不再需要 `sync.Mutex` 串行化 SDK——TurnLoop 本身就是单 goroutine 消费点。

## 11. ChatModelAgent 装配与 middleware 链

### 11.1 配置逐项

| 字段 | 取值 | 说明 |
|---|---|---|
| `Name` / `Description` | `"shotwise-director"` | 多 agent 编排时供路由 |
| `Instruction` | §4.6 组装产物 | f-string 占位符 + `WithSessionValues` 注入 `{project}` `{project_root}` `{content_mode}`；`project_name` 闭包绑定（§6）由 SessionValues 等价实现，工具侧 `adk.GetSessionValue(ctx, "project")` 读取，prompt 改不动它 |
| `Model` | §15 双通道 | `model.BaseChatModel` |
| `ToolsConfig.Tools` | 37 工具 + 6 Subagent + ask_user | `tool.BaseTool` |
| `ToolsConfig.EmitInternalEvents` | `true` | Subagent（AgentTool）事件透传到父流，SSE 才能看到子代理动态；`parent_tool_use_id` 从事件的 agent 链取 |
| `MaxIterations` | 显式设 40 | 默认 20 对 Skill 长流程（generate-video 批量生成）偏紧 |
| `Handlers` | 见 11.2 | 顺序敏感 |
| `ModelRetryConfig` / `ModelFailoverConfig` | 见 §15 | |

启动时 `adk.SetLanguage(adk.LanguageChinese)`——否则内置 middleware 的注入文案
（skill 加载提示、plantask 工具描述）是英文，污染中文为主的对话。

### 11.2 middleware 取舍

按官方推荐顺序（patchtoolcalls 最先），★ 为 Shotwise 自研实现：

| 顺序 | middleware | 取舍 | 对应现状 |
|---|---|---|---|
| 1 | `patchtoolcalls` | 直接用 | 打断/恢复后的 dangling tool call 修复，替代 SDK 内部处理 |
| 2 | `agentsmd` | 直接用，注入项目 cwd 的 `CLAUDE.md` | 替代 SDK `setting_sources` 加载（§4.6） |
| 3 | `summarization` | 直接用，Trigger 150k tokens | 替代 Claude Code 上下文自动压缩；Model 用会话主模型 |
| 4 | `reduction` | 直接用，Backend 落项目 tmp 目录 | 超长工具结果（timeline plan / 剧本全文）截断+外置 |
| 5 | **guard（★§12）** | 自研 | 替代 Pre/PostToolUse hooks + canUseTool |
| 6 | `filesystem` | 用，但 **Backend + Shell 自研** | Read/Write/Edit/Glob/Grep + Bash；见 11.4 |
| 7 | `skill`（★Backend 自研，11.3） | 改造 | Skills 渐进披露 |
| 8 | `plantask` | 暂缓 | TodoWrite 替代品；模型能力退化风险（§7#1）的补位手段，P5 后期评估 |

### 11.3 skill Backend 自研（`adk/middlewares/skill`）

`skill.NewBackendFromFilesystem` 只认裸 `SKILL.md`，不满足两条现状（§2#2），
自研 `skill.Backend` 实现（接口方法名实现期对齐上游）：

- **变体解析**：`manga-workflow` 目录含 `SKILL.drama.md` / `SKILL.narration.md` / `SKILL.ad.md`；
  `List()` 按会话 `content_mode` 返回对应变体的 name/description，`Get()` 返回变体正文；
  变体必须三 mode 齐全（与 §4.5 manifest 决策表同一约束，装配期校验）。
- **Base directory 前缀**：渲染时追加 `"Base directory for this skill: <dir>"`——
  与 SDK 措辞一致，`event_log` 的 skill 注入识别前缀（§4.4）原样复用。
- **user-invocable**：Skills 包另暴露 `SkillCatalog`（router `/skills` 端点的数据源）；
  显式触发 = 前端把 `/skill-name` 翻译为引用技能的用户消息入 TurnLoop，不依赖模型自主发现。

### 11.4 filesystem Backend / Shell = 沙箱执行器落点

`filesystem.MiddlewareConfig{Backend, Shell}` 两个注入点：

- `Backend`：`eino-ext/adk/backend/local`（本地盘）。
- `Shell`：自研 `SandboxedShell` 实现——Linux/macOS 用 bwrap 包装
  （§4.3 参数），Windows 走 `_WINDOWS_BASH_PREFIX_WHITELIST` 四步判定（§4.1）。
  文件读写围栏不在这里做——统一走 guard middleware（§12），Shell 只管进程隔离。

### 11.5 Subagent loader（`.claude/agents/*.md`，6 个）

自研 loader：解析 frontmatter（name / description）+ 正文为独立
`adk.NewChatModelAgent`（各自受限工具集：generate-assets 子代理只挂生成类工具），
再 `adk.NewAgentTool` 包成工具注册进主 agent 的 `ToolsConfig.Tools`。

- **选 `NewAgentTool` 而非 `SetSubAgents`**：现状是 Task 工具式显式派发（prompt 指名调用），
  不是 LLM 自主 transfer 路由；AgentTool 语义（隔离上下文、返回结构化摘要）与
  `generate-assets.md` 的「接收任务清单、返回摘要」契约一致。
- 名字沿用 agent 文件的 name 字段（`generate-assets` 等），prompt 重写时统一。
- 子代理同样受 guard middleware 约束（工具调用走同一 ToolsNode 路径），隔离上下文不豁免围栏。

## 12. GuardedTool：统一工具包裹点（★核心）

### 12.1 位置：middleware 而非逐工具装饰

§4.1 原设计是 per-tool 装饰器。落地改为 **middleware 的 `WrapInvokableToolCall`**
（同时实现 `WrapEnhancedInvokableToolCall`，覆盖 Enhanced 工具），
一个包裹点覆盖全部工具：37 个业务工具、filesystem 注入的文件/Shell 工具、Subagent AgentTool。
逐工具装饰做不到后者，而文件工具恰恰是围栏重点。
实现期验证点：确认 middleware 链上 Wrap 对 `filesystem` 注入工具同样生效（单测钉死）。

### 12.2 接口

```go
type ToolGuard interface {
    // Pre 返回 (args, error)：error 非 nil → 拒绝，error message 回灌模型（deny）；
    // args 非 nil → 改写后传给真实工具（updated_input / bash env 剥离）。
    Pre(ctx context.Context, call ToolCall) (json.RawMessage, error)
    // Post 校验/审计：返回 error 时工具回包被替换为失败信封（JSON 预校验失败回写备份）。
    Post(ctx context.Context, call ToolCall, result string) error
}

func (g *GuardMiddleware) WrapInvokableToolCall(ctx context.Context, ep adk.InvokableToolCallEndpoint, tc *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
    return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
        call := ToolCall{Name: tc.Name, Args: json.RawMessage(args)}
        rewritten, err := g.policy.Pre(ctx, call)
        if err != nil {
            adk.SendEvent(ctx, denyEvent(tc.Name, err)) // 审计进事件流 → event_log system entry
            return denyEnvelope(err.Error()), nil
        }
        res, runErr := ep(ctx, string(rewritten), opts...)
        if perr := g.policy.Post(ctx, call, res); perr != nil {
            return denyEnvelope(perr.Error()), nil
        }
        return res, runErr
    }, nil
}
```

`AgentAccessPolicy`（§4.2）实现 `ToolGuard`；四步 Bash 白名单判定、
`PROTECTED_WRITE_RULES` 双投影、env 剥离改写全部收在 `policy` 包，纯函数无 Eino 依赖。

## 13. HITL：AskUserQuestion → Interrupt / Resume

### 13.1 工具侧

```go
askUser, _ := utils.InferOptionableTool("ask_user", "向用户提出澄清问题",
    func(ctx context.Context, input *AskUserInput, opts ...tool.Option) (string, error) {
        if isResume, _, answer := compose.GetResumeContext[AnswerPayload](ctx); isResume {
            return marshalAnswer(answer), nil // 恢复：把用户选择交回模型
        }
        return "", compose.Interrupt(ctx, input) // 挂起：问题结构（questions/options/multiSelect）原样透出
    })
```

### 13.2 服务侧流程

```
模型调 ask_user
  → compose.Interrupt → Runner 存 checkpoint(WithCheckPointID=sessionID) → CheckPointStore(SQL)
  → 事件流收到 Action.Interrupted → event_log entry(subtype=question) + SSE question 事件
  → 内存 pendingIntents[sessionID] = {interruptID = InterruptContexts[].ID}
前端 POST answer
  → Runtime.Answer → runner.ResumeWithParams(ctx, sessionID, &adk.ResumeParams{
        Targets: {interruptID: answerPayload}})
  → checkpoint 恢复，ask_user 的 GetResumeContext 命中，轮次继续 → 事件走同一适配器
```

`pending_questions` 快照来源不变：`event.Action.Interrupted.InterruptContexts[]`（ID + Info）。
服务重启后从 checkpoint 表反查未决 question 重建 pendingIntents。

### 13.3 CheckPointStore SQL 实现

`compose.CheckPointStore` 只有 `Set/Get([]byte)`：表
`agent_checkpoints(session_id TEXT PK, payload BLOB, updated_at)`，开发 SQLite / 生产同库。
序列化是 gob：自定义类型（`InboxItem`、`AskUserInput`、`AnswerPayload`、policy 裁决记录）
在 `init()` 里 `gob.RegisterName` 注册——key 一旦发布不可改（§7 兼容性风险），命名规范
`shotwise.agent.<Type>`，v1 一次定死。

### 13.4 前缀分叉

沿用 §4.4：分叉 = 复制 event_log entries 到新 session_id + `runner.Run(ctx, prefixMessages)`
注入历史。历史重放不恢复 checkpoint——分叉出的会话从中断点语义降级为普通历史
（与现状一致：分叉后不可在中断点上继续，只能接着对话）。

## 14. 事件面：AgentEvent → event_log → SSE

### 14.1 转换表

`OnAgentEvents` 消费 `AsyncIterator[AgentEvent]`，一条事件走三个出口：
event_log append（权威 transcript，带 seq）→ SseChannel → HTTP SSE（`id: seq`，Last-Event-ID 续传）。

| Eino 事件 | event_log entry | SSE 事件 |
|---|---|---|
| `Output.MessageOutput` streaming chunk（Role=assistant） | 累积，轮末落 assistant entry | `delta`（chunk.Content 直透） |
| 轮末完整 assistant 消息（含 ToolCalls） | assistant entry + 每 ToolCall 一条 tool_use | `tool_use` |
| `Output.MessageOutput` Role=tool | tool_result entry | `tool_result`（IsError 由 is_error 信封） |
| guard deny（`adk.SendEvent` CustomizedOutput） | system entry | `tool_result`(is_error) |
| `Action.Interrupted` | entry(subtype=question) | `question` |
| `Err = *adk.CancelError` | entry(subtype=interrupt) | `status=cancelled` |
| `Err` 其他 | entry(subtype=agent_turn_failure) | `status=error` |
| `ResponseMeta.Usage` | usage 聚合（§2#16 cost 自研） | `usage` |

### 14.2 delta 与 draft

`DraftAccumulator` 大幅简化：Eino chunk 自带流式语义，适配器只做
「chunk → SSE 直透 + 轮末 assistant 整消息落库」两件事，
不再需要按 index 拼装工具调用分片（Runner 已合并）。

## 15. 双模型通道与容错

### 15.1 两个 eino-ext 实现

| 通道 | 实现 | 说明 |
|---|---|---|
| claude | `eino-ext/components/model/claude` | 原生 Anthropic API（支持 ByBedrock）；替换现有 openai 兼容网关方案 |
| openai | `eino-ext/components/model/openai` | 兼容各类 OpenAI 协议网关 |

构造由 `provider.go::NewChatModel` 扩展为 `NewChatModel(channel, cfg)` 分派。

### 15.2 Retry / Failover

```go
ModelRetryConfig: &adk.ModelRetryConfig{           // 通道内：瞬时失败 / 空响应
    MaxRetries: 3,
    ShouldRetry: func(ctx context.Context, rc *adk.RetryContext) *adk.RetryDecision {
        if rc.Err != nil { return &adk.RetryDecision{Retry: true, Backoff: 2 * time.Second} }
        if rc.OutputMessage == nil || rc.OutputMessage.Content == "" {
            return &adk.RetryDecision{Retry: true}
        }
        return &adk.RetryDecision{Retry: false}
    },
},
ModelFailoverConfig: &adk.ModelFailoverConfig[*schema.Message]{ // 通道间：claude → openai
    MaxRetries: 1,
    ShouldFailover: func(ctx context.Context, msg *schema.Message, err error) bool {
        return err != nil // retry 已穷尽时 LastErr 为 RetryExhaustedError
    },
    GetFailoverModel: func(ctx context.Context, fc *adk.FailoverContext[*schema.Message]) (model.BaseModel[*schema.Message], []*schema.Message, error) {
        return backupModel, fc.InputMessages, nil
    },
},
```

### 15.3 switch_agent 重设计

§7#5 的跨 SDK 续接设计整体作废，Eino 下收敛为**同构切换**：两通道都说 `schema.Message`，
历史就是消息数组。`switch_agent` = 用目标通道模型重建 ChatModelAgent（PrepareAgent 每轮装配天然支持）
+ event_log 历史注入，`claude_resume_id` / `resolve_sdk_session_id` 等跨 SDK 续接机制全部删除。
注意切通道会丢 SDK 侧缓存语义（prompt cache 断），成本上可接受。

## 16. 存量骨架收敛（`server-go/internal/agent/`）

| 现有文件 | 处置 |
|---|---|
| `engine.go`（自研 ReAct 循环） | **退役**——`adk.ChatModelAgent` 替代；mergeToolCall/finalizeToolCalls 等流式拼片逻辑由 Runner 承担 |
| `tool.go`（Guard/Tool/ToolSet 自定义接口） | 收敛到 `tool.InvokableTool` + guard middleware（§12）；`Kind` 枚举迁入 `policy` 包 |
| `session.go`（SessionManager） | 保留（容量/驱逐语义本就自研）；`Session.History` 降级为只读缓存，权威历史在 event_log |
| `provider.go`（NewChatModel） | 扩展双通道分派（§15.1） |
| `events.go`（Event/EncodeSSE） | 保留为 SSE 出口；新增 eventlog.Adapter 作为唯一生产者 |

实施顺序上，§8 第 1 层的顺序调整为：guard middleware（§12）先于 filesystem/skill 接入——
围栏是硬失败设计（ADR 0025），文件工具在无围栏状态下不得上线。
