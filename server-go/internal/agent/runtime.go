package agent

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// Runtime 是智能体运行时的对外门面：模型工厂 + 会话管理 + 工具集 + ReAct 引擎。
// 对应 Python 侧的 AssistantService + SessionManager。
type Runtime struct {
	cfg      ModelConfig
	dataDir  string
	sessions *SessionManager
	engine   *Engine
}

// NewRuntime 构造运行时。guard 传 nil 表示暂不启用权限围栏（演示用）。
func NewRuntime(ctx context.Context, cfg ModelConfig, dataDir string, guard Guard) (*Runtime, error) {
	cm, err := NewChatModel(ctx, cfg)
	if err != nil {
		return nil, err
	}
	tools := DefaultToolSet(dataDir, guard)
	engine, err := NewEngine(cm, tools, SystemPrompt())
	if err != nil {
		return nil, err
	}
	return &Runtime{
		cfg:      cfg,
		dataDir:  dataDir,
		sessions: NewSessionManager(0),
		engine:   engine,
	}, nil
}

// Query 执行一次用户提问。返回会话标识与事件流（调用方按 SSE 消费）。
// 会话历史由 Runtime 内部持久化，无需调用方关心。
func (r *Runtime) Query(ctx context.Context, sessionID, project, message string) (string, <-chan Event, error) {
	if sessionID == "" {
		sessionID = NewSessionID()
	}
	sess := r.sessions.GetOrCreate(sessionID, project)
	history := sess.HistorySnapshot()
	origLen := len(history)

	msgs := make([]*schema.Message, 0, len(history)+2)
	msgs = append(msgs, &schema.Message{Role: schema.System, Content: r.engine.system})
	msgs = append(msgs, history...)
	msgs = append(msgs, &schema.Message{Role: schema.User, Content: message})
	sess.Append(&schema.Message{Role: schema.User, Content: message})

	evCh, finalCh, err := r.engine.Run(ctx, sessionID, msgs)
	if err != nil {
		return sessionID, nil, err
	}

	// 后台把本轮新消息（assistant / tool）持久化到会话历史。
	go func() {
		final := <-finalCh
		if len(final) > origLen+1 {
			sess.AppendTail(final[origLen+1:])
		}
	}()

	return sessionID, evCh, nil
}

// ListSessions 返回所有活跃会话的轻量信息。
func (r *Runtime) ListSessions() []SessionInfo {
	return r.sessions.List()
}

// SystemPrompt 是身份 + 项目文档工作流 + 语言规范的基线系统提示（文档 4.6 节顺序的精简版）。
func SystemPrompt() string {
	return `你是 Shotwise 的 AI 导演助手，负责辅助短视频/短剧的脚本、分镜与资产生成。

# 行为准则
1. 始终先理解用户的项目上下文，再给出可执行的建议或调用工具。
2. 需要读取项目真相源时，优先调用 read_project_json / list_projects，不要凭空杜撰项目结构。
3. 调用任何工具前，先在内心确认参数合理；工具失败会以其回包形式反馈，要据此修正重试。
4. 用与用户相同的语言回答（中文提问用中文，英文用英文）。

# 安全约束
- 你只能在用户指定的项目目录内工作；写操作（Write/Edit）一律经由受 GuardedTool 约束的工具，不要尝试直接修改文件。
- 不要泄露任何 API Key 或密钥。`
}
