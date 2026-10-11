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
