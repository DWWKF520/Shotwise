import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useAssistantSession } from "@/hooks/useAssistantSession";
import { useAppStore } from "@/stores/app-store";
import { useAssistantStore } from "@/stores/assistant-store";
import { useProjectsStore } from "@/stores/projects-store";
import { AgentCopilot } from "./AgentCopilot";
import { SlashCommandMenu } from "./SlashCommandMenu";

vi.mock("@/hooks/useAssistantSession", () => ({
  useAssistantSession: vi.fn(),
}));

vi.mock("./ContextBanner", () => ({
  ContextBanner: () => <div data-testid="context-banner" />,
}));

vi.mock("./SlashCommandMenu", () => ({
  SlashCommandMenu: vi.fn(() => null),
}));

vi.mock("./chat/ChatMessage", () => ({
  ChatMessage: ({ message }: { message: { type: string } }) => (
    <div data-testid="chat-message">{message.type}</div>
  ),
}));

const mockedUseAssistantSession = vi.mocked(useAssistantSession);

function makePendingQuestion() {
  return {
    question_id: "q-1",
    questions: [
      {
        header: "输出",
        question: "输出格式是什么？",
        multiSelect: false,
        options: [
          { label: "摘要", description: "简洁输出" },
          { label: "详细", description: "完整说明" },
        ],
      },
    ],
  };
}

describe("AgentCopilot", () => {
  // Mocks whose callers wrap them with voidPromise must return a Promise
  // so the .catch(...) chain in voidPromise resolves instead of crashing.
  const sendMessage = vi.fn().mockResolvedValue(undefined);
  const rewriteMessage = vi.fn().mockResolvedValue(true);
  const answerQuestion = vi.fn().mockResolvedValue(undefined);
  const interrupt = vi.fn().mockResolvedValue(undefined);
  const createNewSession = vi.fn();
  const switchSession = vi.fn().mockResolvedValue(undefined);
  const deleteSession = vi.fn().mockResolvedValue(undefined);
  const switchAgent = vi.fn().mockResolvedValue(undefined);
  const switchAgentProvider = vi.fn().mockResolvedValue(undefined);
  const switchAgentModel = vi.fn().mockResolvedValue(undefined);
  const loadAgentModels = vi.fn().mockResolvedValue(undefined);

  beforeEach(() => {
    useAssistantStore.setState(useAssistantStore.getInitialState(), true);
    useProjectsStore.setState(useProjectsStore.getInitialState(), true);
    useAppStore.setState(useAppStore.getInitialState(), true);
    vi.clearAllMocks();

    useProjectsStore.getState().setCurrentProject("demo", null);
    mockedUseAssistantSession.mockReturnValue({
      sendMessage,
      rewriteMessage,
      answerQuestion,
      interrupt,
      createNewSession,
      switchSession,
      deleteSession,
      switchAgent,
      switchAgentProvider,
      switchAgentModel,
      loadAgentModels,
    });
  });

  it("renders the pending-question wizard and disables normal sending", () => {
    useAssistantStore.setState({
      pendingQuestion: makePendingQuestion(),
      skills: [{ name: "plan", description: "Plan", scope: "project", path: "/tmp/plan" }],
    });

    render(<AgentCopilot />);

    expect(screen.getByText("需要你的选择")).toBeInTheDocument();
    expect(screen.getByLabelText("助手输入")).toBeDisabled();
    expect(screen.getByLabelText("发送消息")).toBeDisabled();
    expect(screen.getByPlaceholderText("请先回答上方问题")).toBeInTheDocument();
  });

  it("submits wizard answers through answerQuestion", () => {
    useAssistantStore.setState({
      pendingQuestion: makePendingQuestion(),
    });

    render(<AgentCopilot />);

    fireEvent.click(screen.getByLabelText("摘要"));
    fireEvent.click(screen.getByRole("button", { name: /完成并提交/ }));

    expect(answerQuestion).toHaveBeenCalledWith("q-1", {
      "输出格式是什么？": "摘要",
    });
  });

  it("keeps assistant root isolated and opens a centered dialog for session history", () => {
    useAssistantStore.setState({
      sessions: [
        {
          id: "session-1",
          project_name: "demo",
          title: "当前会话",
          status: "idle",
          created_at: "2026-02-01T00:00:00Z",
          updated_at: "2026-02-01T00:00:00Z",
        },
      ],
      currentSessionId: "session-1",
    });

    const { container } = render(<AgentCopilot />);

    expect(container.firstElementChild).toHaveClass("isolate");

    fireEvent.click(screen.getByTitle("切换会话"));
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getByText("会话历史")).toBeInTheDocument();
    expect(screen.getByText("当前会话")).toBeInTheDocument();
  });

  it("always shows the session entry and renders an empty-state hint inside the dialog", () => {
    render(<AgentCopilot />);
    // 草稿态：入口常显，点开弹窗显示空态提示
    expect(screen.getByTitle("切换会话")).toBeInTheDocument();
    expect(screen.getByTitle("新会话")).toBeInTheDocument();

    fireEvent.click(screen.getByTitle("切换会话"));
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getByText("暂无历史会话，发送消息后即可创建")).toBeInTheDocument();
  });

  it("switches between Claude Agent and OpenAI Agent from the header title", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    render(<AgentCopilot />);

    // 标题显示短名（无 SDK 后缀），点击标题弹出 Claude / OpenAI 切换菜单
    expect(screen.getByTitle("切换智能体")).toBeInTheDocument();
    expect(screen.getByText("Claude Agent")).toBeInTheDocument();
    fireEvent.click(screen.getByTitle("切换智能体"));
    expect(screen.getByRole("menuitem", { name: /OpenAI Agent/ })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("menuitem", { name: /OpenAI Agent/ }));
    await waitFor(() => {
      expect(switchAgent).toHaveBeenCalledWith("openai");
    });
  });

  it("lists configured providers from the icon button and switches the active one", async () => {
    useAssistantStore.setState({
      agentCredentials: [
        {
          id: 1,
          sdk_type: "claude",
          protocol: "anthropic_messages",
          preset_id: "anthropic",
          display_name: "Anthropic 主账号",
          icon_key: "Anthropic",
          base_url: "",
          api_key_masked: "sk-***",
          model: null,
          haiku_model: null,
          sonnet_model: null,
          opus_model: null,
          subagent_model: null,
          model_map: null,
          is_active: true,
          created_at: null,
        },
        {
          id: 2,
          sdk_type: "claude",
          protocol: "anthropic_messages",
          preset_id: "claude",
          display_name: "备用端点",
          icon_key: "Claude",
          base_url: "",
          api_key_masked: "sk-***",
          model: null,
          haiku_model: null,
          sonnet_model: null,
          opus_model: null,
          subagent_model: null,
          model_map: null,
          is_active: false,
          created_at: null,
        },
      ],
      activeCredentialId: 1,
    });
    render(<AgentCopilot />);

    // 图标按钮（title=切换供应商）平时只显示图标；点击后列出配置的供应商名称
    fireEvent.click(screen.getByTitle("切换供应商"));
    expect(screen.getByRole("menuitem", { name: /Anthropic 主账号/ })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /备用端点/ })).toBeInTheDocument();

    // 切换到另一个供应商
    fireEvent.click(screen.getByRole("menuitem", { name: /备用端点/ }));
    await waitFor(() => {
      expect(switchAgentProvider).toHaveBeenCalledWith(2);
    });
  });

  it("does not send when Enter is used to confirm an IME composition", () => {
    render(<AgentCopilot />);

    const textarea = screen.getByLabelText("助手输入");
    fireEvent.change(textarea, { target: { value: "你好" } });

    fireEvent.compositionStart(textarea);
    fireEvent.keyDown(textarea, {
      key: "Enter",
      code: "Enter",
      keyCode: 229,
      which: 229,
      isComposing: true,
    });

    expect(sendMessage).not.toHaveBeenCalled();

    fireEvent.compositionEnd(textarea);
    fireEvent.keyDown(textarea, {
      key: "Enter",
      code: "Enter",
      keyCode: 13,
      which: 13,
    });

    expect(sendMessage).toHaveBeenCalledWith("你好", undefined);
  });

  it("renders model_map entries in the model menu and switches by request_model", async () => {
    useAssistantStore.setState({
      agentModels: [
        { menu_name: "V4 Flash", request_model: "deepseek-v4-flash" },
        { menu_name: "V4 Pro", request_model: "deepseek-v4-pro" },
      ],
      agentModel: "deepseek-v4-flash",
    });
    render(<AgentCopilot />);

    // 菜单展示 menu_name，当前模型（agentModel = request_model）高亮
    fireEvent.click(screen.getByTitle("Agent 模型"));
    expect(screen.getByRole("menuitem", { name: /V4 Flash/ })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /V4 Pro/ })).toBeInTheDocument();

    // 选中后写回实际请求模型（request_model），而非展示名
    fireEvent.click(screen.getByRole("menuitem", { name: /V4 Pro/ }));
    await waitFor(() => {
      expect(switchAgentModel).toHaveBeenCalledWith("deepseek-v4-pro");
    });
  });

  it("consumes a one-shot prefill dispatched via the assistant store's input field", async () => {
    render(<AgentCopilot />);

    act(() => {
      useAssistantStore.getState().setInput("为第 1 集生成剧本");
    });

    expect(screen.getByLabelText("助手输入")).toHaveValue("为第 1 集生成剧本");

    await waitFor(() => {
      expect(useAssistantStore.getState().input).toBe("");
    });
  });

  it("only opens the slash menu when / starts the message", () => {
    render(<AgentCopilot />);
    const textarea = screen.getByLabelText("助手输入");
    // 与后端 /skill-name 解析同规则：只有消息开头的首 token 会被当作命令。
    // 菜单是条件渲染（showSlashMenu 为 false 时组件不被调用），因此用调用次数判断开关。
    const menuMock = SlashCommandMenu as unknown as ReturnType<typeof vi.fn>;
    const callCount = () => menuMock.mock.calls.length;
    const lastFilter = () =>
      (menuMock.mock.calls.at(-1)?.[0] as { filter: string }).filter;

    fireEvent.change(textarea, { target: { value: "/gen" } });
    expect(callCount()).toBeGreaterThan(0);
    expect(lastFilter()).toBe("gen");

    // 正文中间的 "/" 是普通文本：菜单不打开
    const opened = callCount();
    fireEvent.change(textarea, { target: { value: "看 /gen" } });
    expect(callCount()).toBe(opened);

    // 前导空白允许：发送时会 trim，后端仍按命令处理
    fireEvent.change(textarea, { target: { value: "  /gen" } });
    expect(callCount()).toBeGreaterThan(opened);
    expect(lastFilter()).toBe("gen");
  });

});
