import { useState, useRef, useCallback, useEffect, useId } from "react";
import { voidCall, voidPromise } from "@/utils/async";
import { Bot, Send, Square, Plus, ChevronDown, Trash2, MessageSquare, PanelRightClose, Paperclip, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { ImageLightbox } from "@/components/ui/ImageLightbox";
import { useAssistantStore } from "@/stores/assistant-store";
import { useProjectsStore } from "@/stores/projects-store";
import { useAppStore } from "@/stores/app-store";
import { getAssistantSkin } from "@/utils/assistant-skins";
import { useAssistantSession } from "@/hooks/useAssistantSession";
import type { AttachedImage } from "@/hooks/useAssistantSession";
import { GlassPopover } from "@/components/ui/GlassPopover";
import { GlassModal } from "@/components/ui/GlassModal";
import { PresetIcon } from "@/components/agent/PresetIcon";
import { ContextBanner } from "./ContextBanner";
import { PendingQuestionWizard } from "./PendingQuestionWizard";
import { SlashCommandMenu } from "./SlashCommandMenu";
import type { SlashCommandMenuHandle } from "./SlashCommandMenu";
import { TodoListPanel } from "./TodoListPanel";
import { MessageRow } from "./chat/MessageRow";
import { AgentFailureCard } from "./chat/AgentFailureCard";
import { canEditUserTurn, composeAllTurns } from "./chat/utils";
import { MessageRail, messageAnchorId } from "./MessageRail";
import { uid } from "@/utils/id";
import { formatShortDateTime } from "@/utils/date-format";
import { ChatImageValidationError, prepareChatImages } from "@/utils/chat-image";

const MAX_IMAGES = 5;

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const MAX_TEXTAREA_HEIGHT_VH = 50;

// ---------------------------------------------------------------------------
// SessionSelector — 会话下拉选择器
// ---------------------------------------------------------------------------

function SessionSelector({
  onSwitch,
  onDelete,
}: {
  onSwitch: (sessionId: string) => void;
  onDelete: (sessionId: string) => void;
}) {
  const { t } = useTranslation("dashboard");
  const { sessions, currentSessionId } = useAssistantStore();
  const [open, setOpen] = useState(false);
  const titleId = useId();

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        title={t("switch_session")}
        aria-label={t("switch_session")}
        className="rounded p-1 transition-colors focus-ring"
        style={{ color: "var(--color-text-3)" }}
        onMouseEnter={(e) => {
          e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
          e.currentTarget.style.color = "var(--color-text)";
        }}
        onMouseLeave={(e) => {
          e.currentTarget.style.background = "transparent";
          e.currentTarget.style.color = "var(--color-text-3)";
        }}
      >
        <MessageSquare aria-hidden className="h-4 w-4" />
      </button>

      {/* 会话切换：居中弹窗列出全部会话 */}
      <GlassModal
        open={open}
        onClose={() => setOpen(false)}
        labelledBy={titleId}
        widthClassName="w-full max-w-sm"
        panelClassName="max-h-[72vh]"
      >
        <div className="px-5 py-4">
          <div
            id={titleId}
            className="font-mono text-[10px] font-bold uppercase tracking-[0.18em] text-accent-2"
          >
            {t("session_history")}
          </div>
          <div className="mt-3 max-h-[52vh] space-y-1 overflow-y-auto">
            {sessions.length === 0 ? (
              <div
                className="px-2.5 py-2 text-[12px] leading-relaxed"
                style={{ color: "var(--color-text-3)" }}
              >
                {t("session_history_empty")}
              </div>
            ) : (
              sessions.map((session) => {
                const isActive = session.id === currentSessionId;
                const title = session.title || formatTime(session.created_at, t);
                return (
                  <div
                    key={session.id}
                    className="group flex items-center gap-2 rounded-[8px] px-2.5 py-2 text-[12.5px] transition-colors"
                    style={
                      isActive
                        ? {
                            background: "var(--color-accent-dim)",
                            color: "var(--color-accent-2)",
                          }
                        : { color: "var(--color-text-2)" }
                    }
                    onMouseEnter={(e) => {
                      if (!isActive)
                        e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
                    }}
                    onMouseLeave={(e) => {
                      if (!isActive) e.currentTarget.style.background = "transparent";
                    }}
                  >
                    <button
                      type="button"
                      onClick={() => {
                        onSwitch(session.id);
                        setOpen(false);
                      }}
                      className="flex flex-1 items-center gap-2 truncate text-left"
                    >
                      <StatusDot status={session.status} />
                      <span className="truncate">{title}</span>
                    </button>
                    <button
                      type="button"
                      onClick={(e) => {
                        e.stopPropagation();
                        if (confirm(t("confirm_delete_session"))) onDelete(session.id);
                      }}
                      className="focus-ring shrink-0 rounded p-0.5 opacity-0 transition-all group-hover:opacity-100 focus-visible:opacity-100"
                      style={{ color: "var(--color-text-4)" }}
                      onMouseEnter={(e) => {
                        e.currentTarget.style.color = "var(--color-danger)";
                      }}
                      onMouseLeave={(e) => {
                        e.currentTarget.style.color = "var(--color-text-4)";
                      }}
                      title={t("delete_session")}
                      aria-label={t("delete_session")}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </button>
                  </div>
                );
              })
            )}
          </div>
        </div>
      </GlassModal>
    </>
  );
}

function StatusDot({ status }: { status: string }) {
  const colorMap: Record<string, string> = {
    idle: "var(--color-text-4)",
    running: "var(--color-warn)",
    completed: "var(--color-good)",
    error: "var(--color-danger)",
    interrupted: "var(--color-text-3)",
  };
  return (
    <span
      className="h-1.5 w-1.5 shrink-0 rounded-full"
      style={{ background: colorMap[status] ?? "var(--color-text-4)" }}
    />
  );
}

function formatTime(isoStr: string | undefined, t: TFunction): string {
  return formatShortDateTime(isoStr) ?? t("new_session");
}

// ---------------------------------------------------------------------------
// AgentCopilot — 主面板
// ---------------------------------------------------------------------------

export function AgentCopilot() {
  const { t } = useTranslation(["dashboard", "common"]);
  const {
    turns, draftTurn, messagesLoading,
    sending, sessionStatus, pendingQuestion, answeringQuestion, error, startupFailure, startupFailureOrigin, editingTurnUuid,
  } = useAssistantStore();

  const { currentProjectName } = useProjectsStore();
  const toggleAssistantPanel = useAppStore((s) => s.toggleAssistantPanel);
  const assistantSkin = useAppStore((s) => s.assistantSkin);
  const skin = getAssistantSkin(assistantSkin);
  const SkinIcon = skin.icon;
  const sdkType = useAssistantStore((s) => s.sdkType);
  const agentModels = useAssistantStore((s) => s.agentModels);
  const agentModel = useAssistantStore((s) => s.agentModel);
  const agentCredentials = useAssistantStore((s) => s.agentCredentials);
  const activeCredentialId = useAssistantStore((s) => s.activeCredentialId);
  const setEditingTurnUuid = useAssistantStore((s) => s.setEditingTurnUuid);
  const { sendMessage, rewriteMessage, answerQuestion, interrupt, createNewSession, switchSession, deleteSession, switchAgent, switchAgentProvider, switchAgentModel, loadAgentModels } =
    useAssistantSession(currentProjectName);

  // 供应商（Claude / OpenAI）切换浮层
  const [agentMenuOpen, setAgentMenuOpen] = useState(false);
  const agentMenuRef = useRef<HTMLDivElement>(null);
  // 供应商（credential）切换浮层
  const [providerMenuOpen, setProviderMenuOpen] = useState(false);
  const providerMenuRef = useRef<HTMLDivElement>(null);
  // 模型切换浮层
  const [modelMenuOpen, setModelMenuOpen] = useState(false);
  const modelMenuRef = useRef<HTMLDivElement>(null);

  // 挂载与 Agent 切换后加载模型选项
  useEffect(() => {
    if (!currentProjectName) return;
    void loadAgentModels(sdkType);
  }, [currentProjectName, sdkType, loadAgentModels]);

  const scrollRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const isComposingRef = useRef(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const imageGenRef = useRef(0);
  const slashMenuRef = useRef<SlashCommandMenuHandle>(null);
  const [localInput, setLocalInput] = useState("");
  const [showSlashMenu, setShowSlashMenu] = useState(false);
  const [attachedImages, setAttachedImages] = useState<AttachedImage[]>([]);
  const [attachError, setAttachError] = useState<string | null>(null);
  const [isDragOver, setIsDragOver] = useState(false);
  const [lightboxSrc, setLightboxSrc] = useState<string | null>(null);
  const allTurns = composeAllTurns(turns, draftTurn);
  const isRunning = sessionStatus === "running";
  const inputDisabled = Boolean(pendingQuestion) || answeringQuestion || isRunning || sending;
  const attachDisabled = inputDisabled || attachedImages.length >= MAX_IMAGES;

  const handleSwitchAgent = useCallback(
    async (next: "claude" | "openai") => {
      if (next === sdkType) {
        setAgentMenuOpen(false);
        return;
      }
      if (isRunning || sending) {
        useAppStore.getState().pushToast(t("agent_switch_busy"), "warning");
        setAgentMenuOpen(false);
        return;
      }
      const confirmed = window.confirm(t("agent_switch_confirm"));
      if (!confirmed) {
        setAgentMenuOpen(false);
        return;
      }
      setAgentMenuOpen(false);
      await switchAgent(next);
    },
    [sdkType, isRunning, sending, switchAgent, t],
  );

  const handleSwitchProvider = useCallback(
    async (credentialId: number) => {
      if (isRunning || sending) {
        useAppStore.getState().pushToast(t("agent_switch_busy"), "warning");
        setProviderMenuOpen(false);
        return;
      }
      setProviderMenuOpen(false);
      await switchAgentProvider(credentialId);
    },
    [isRunning, sending, switchAgentProvider, t],
  );

  const activeCredential =
    agentCredentials.find((c) => c.id === activeCredentialId) ??
    agentCredentials.find((c) => c.is_active) ??
    null;

  const handleModelChange = useCallback(
    (model: string) => {
      voidCall(switchAgentModel(model));
    },
    [switchAgentModel],
  );
  const inputPlaceholder = pendingQuestion
    ? t("answer_above_hint")
    : isRunning
      ? t("generating_stop_hint")
      : t("input_placeholder");

  const addImages = useCallback((files: File[]) => {
    setAttachError(null);
    const gen = imageGenRef.current;
    void prepareChatImages(files, attachedImages.map((image) => image.dataUrl))
      .then((prepared) => {
        if (imageGenRef.current !== gen || prepared.length === 0) return;
        setAttachedImages((prev) => {
          const available = Math.max(0, MAX_IMAGES - prev.length);
          return [
            ...prev,
            ...prepared.slice(0, available).map((image) => ({
              id: uid(),
              dataUrl: image.dataUrl,
              mimeType: image.mimeType,
            })),
          ];
        });
      })
      .catch((error: unknown) => {
        if (imageGenRef.current !== gen) return;
        const key = error instanceof ChatImageValidationError ? error.key : "assistant_image_invalid";
        setAttachError(t(key));
      });
  }, [attachedImages, t]);


  const handlePaste = useCallback((e: React.ClipboardEvent) => {
    const items = Array.from(e.clipboardData.items);
    const imageItems = items.filter((item) => item.type.startsWith("image/"));
    if (imageItems.length === 0) return;
    e.preventDefault();
    const files = imageItems.map((item) => item.getAsFile()).filter(Boolean) as File[];
    addImages(files);
  }, [addImages]);

  const handleDragOver = useCallback((e: React.DragEvent) => {
    const hasFiles = Array.from(e.dataTransfer.items).some((i) => i.kind === "file");
    if (!hasFiles) return;
    e.preventDefault();
    setIsDragOver(true);
  }, []);

  const handleDragLeave = useCallback(() => {
    setIsDragOver(false);
  }, []);

  const handleDrop = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    setIsDragOver(false);
    const files = Array.from(e.dataTransfer.files).filter((f) => f.type.startsWith("image/"));
    if (files.length > 0) addImages(files);
  }, [addImages]);

  const handleFileSelect = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(e.target.files ?? []);
    if (files.length > 0) addImages(files);
    e.target.value = "";
  }, [addImages]);

  const removeImage = useCallback((id: string) => {
    setAttachedImages((prev) => prev.filter((img) => img.id !== id));
    setAttachError(null);
  }, []);

  const handleSend = useCallback(() => {
    if (inputDisabled || (!localInput.trim() && attachedImages.length === 0)) return;
    imageGenRef.current += 1; // invalidate pending FileReader callbacks
    setShowSlashMenu(false);
    // 发送期间输入锁定（sending 置位）；受理成功才清空，失败保留内容供重试
    voidCall(
      sendMessage(localInput.trim(), attachedImages.length > 0 ? attachedImages : undefined).then(
        (accepted) => {
          if (!accepted) return;
          setLocalInput("");
          setAttachedImages([]);
          setAttachError(null);
          // Reset textarea height
          if (textareaRef.current) {
            textareaRef.current.style.height = "auto";
          }
        },
      ),
    );
  }, [inputDisabled, localInput, attachedImages, sendMessage]);

  // 改写成功后由会话切换重建时间线（编辑态随 resetTimeline 清空）；失败保留编辑态，
  // 用户可以改完再试，错误经消息区上方的错误条呈现
  const handleSubmitEdit = useCallback((turnUuid: string, text: string) => {
    voidCall(rewriteMessage(turnUuid, text));
  }, [rewriteMessage]);

  const handleKeyDown = useCallback((e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // Delegate to slash menu when open
    if (showSlashMenu && slashMenuRef.current) {
      const consumed = slashMenuRef.current.handleKeyDown(e.key);
      if (consumed) {
        e.preventDefault();
        if (e.key === "Escape") setShowSlashMenu(false);
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey) {
      const nativeEvent = e.nativeEvent;
      if (nativeEvent.isComposing || nativeEvent.keyCode === 229 || isComposingRef.current) {
        return;
      }
      e.preventDefault();
      handleSend();
    }
  }, [handleSend, showSlashMenu]);

  // Track the slash "/" position so we know where the command token starts
  const slashPosRef = useRef(-1);

  const handleInputChange = useCallback((e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const val = e.target.value;
    const cursor = e.target.selectionStart ?? val.length;
    setLocalInput(val);

    // Check text left of cursor: trigger menu only when "/" starts the message
    // (允许前导空白，发送时会 trim)。与后端 /skill-name 解析规则一致——只有消息开头的
    // 首 token 会被当作命令，正文中间的 "/" 是普通文本，不弹菜单以免选中后不生效。
    const textBeforeCursor = val.slice(0, cursor);
    const lastSlash = textBeforeCursor.lastIndexOf("/");
    if (lastSlash >= 0) {
      const atCommandStart = textBeforeCursor.slice(0, lastSlash).trim() === "";
      const afterSlash = textBeforeCursor.slice(lastSlash + 1);
      const noSpaceAfterSlash = !afterSlash.includes(" ");
      if (atCommandStart && noSpaceAfterSlash) {
        setShowSlashMenu(true);
        slashPosRef.current = lastSlash;
      } else {
        setShowSlashMenu(false);
        slashPosRef.current = -1;
      }
    } else {
      setShowSlashMenu(false);
      slashPosRef.current = -1;
    }

    // Auto-resize: grow upward until 50vh, then scroll
    const el = e.target;
    el.style.height = "auto";
    const maxH = window.innerHeight * (MAX_TEXTAREA_HEIGHT_VH / 100);
    el.style.height = `${Math.min(el.scrollHeight, maxH)}px`;
    el.style.overflowY = el.scrollHeight > maxH ? "auto" : "hidden";
  }, []);

  // Derive slash filter from input (text after "/" up to cursor)
  // eslint-disable-next-line react-hooks/refs -- slashPosRef 同时被 render 和 handleSlashSelect 使用，转 state 会引入 stale-closure 问题；此处仅用于过滤展示，不影响 UI 一致性
  const slashFilter = showSlashMenu && slashPosRef.current >= 0
    // eslint-disable-next-line react-hooks/refs -- 同上
    ? localInput.slice(slashPosRef.current + 1).split(/\s/)[0]
    : "";

  const handleSlashSelect = useCallback((cmd: string) => {
    // Replace the "/filter" token with the selected command, keep surrounding text
    const pos = slashPosRef.current;
    if (pos >= 0) {
      const before = localInput.slice(0, pos);
      // Find end of the slash token (next whitespace or end of string)
      const afterSlash = localInput.slice(pos);
      const tokenEnd = afterSlash.search(/\s/);
      const after = tokenEnd >= 0 ? localInput.slice(pos + tokenEnd) : "";
      setLocalInput(before + cmd + " " + after.trimStart());
    } else {
      setLocalInput(localInput + cmd + " ");
    }
    setShowSlashMenu(false);
    slashPosRef.current = -1;
    textareaRef.current?.focus();
  }, [localInput]);

  // 消费外部投递的一次性预填文本（如分集空态 CTA 经 store.input 投递）：
  // 写入本地输入框后清空 store 字段，避免残留触发重复预填。
  // 覆盖而非追加——预填来自用户的明确点击意图。
  useEffect(() => {
    return useAssistantStore.subscribe((state, prev) => {
      if (!state.input || state.input === prev.input) return;
      setLocalInput(state.input);
      // 延后到微任务清空，避免在 zustand 订阅通知期间嵌套 dispatch
      void Promise.resolve().then(() => {
        useAssistantStore.getState().setInput("");
      });
      // 面板可能同帧刚被打开（inert 尚未移除），等一帧再聚焦
      requestAnimationFrame(() => textareaRef.current?.focus());
    });
  }, []);

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [allTurns.length]);

  return (
    <div
      className="relative isolate flex h-full flex-col"
      style={{ background: "var(--color-shell-copilot-a)" }}
    >
      {/* Header */}
      <div
        className="flex h-12 items-center gap-2 px-3"
        style={{ borderBottom: "1px solid var(--color-hairline)" }}
      >
        <div className="flex min-w-0 flex-1 items-center gap-2">
          <button
            type="button"
            onClick={toggleAssistantPanel}
            className="shrink-0 rounded p-1 transition-colors focus-ring"
            style={{ color: "var(--color-text-3)" }}
            onMouseEnter={(e) => {
              e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
              e.currentTarget.style.color = "var(--color-text)";
            }}
            onMouseLeave={(e) => {
              e.currentTarget.style.background = "transparent";
              e.currentTarget.style.color = "var(--color-text-3)";
            }}
            title={t("collapse_panel")}
            aria-label={t("collapse_panel")}
          >
            <PanelRightClose aria-hidden className="h-4 w-4" />
          </button>
          <div
            className="grid h-6 w-6 shrink-0 place-items-center rounded-md"
            style={{
              background: `linear-gradient(135deg, ${skin.from}, ${skin.to})`,
              color: "oklch(0.12 0 0)",
            }}
          >
            <SkinIcon className="h-3.5 w-3.5" />
          </div>
          {isRunning || sending ? (
            <span
              className="flex shrink-0 items-center gap-1.5 whitespace-nowrap text-[12px]"
              style={{ color: "var(--color-accent-2)" }}
              title={t("shotwise_agent")}
            >
              <span
                className="h-1.5 w-1.5 animate-pulse rounded-full"
                style={{ background: "var(--color-accent)" }}
              />
              {t("thinking")}
            </span>
          ) : (
            <div className="relative min-w-0" ref={agentMenuRef}>
              <button
                type="button"
                onClick={() => setAgentMenuOpen((v) => !v)}
                aria-haspopup="menu"
                aria-expanded={agentMenuOpen}
                title={t("switch_agent")}
                className="flex min-w-0 items-center gap-1 rounded px-1 py-0.5 transition-colors focus-ring"
                style={{ color: "var(--color-text)" }}
                onMouseEnter={(e) => {
                  e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
                }}
                onMouseLeave={(e) => {
                  e.currentTarget.style.background = "transparent";
                }}
              >
                <span className="display-serif truncate text-[13px] font-semibold leading-[1.1]">
                  {t(sdkType === "openai" ? "agent_sdk_openai" : "agent_sdk_claude")}
                </span>
                <ChevronDown
                  className={`h-3 w-3 shrink-0 text-text-3 transition-transform ${
                    agentMenuOpen ? "rotate-180" : ""
                  }`}
                />
              </button>
              <GlassPopover
                open={agentMenuOpen}
                onClose={() => setAgentMenuOpen(false)}
                anchorRef={agentMenuRef}
                sideOffset={4}
                width="w-56"
                layer="assistantPopover"
                showHairline={false}
              >
                <div role="menu" className="py-1">
                  {(
                    [
                      { id: "claude", label: t("agent_sdk_claude") },
                      { id: "openai", label: t("agent_sdk_openai") },
                    ] as const
                  ).map((opt) => (
                    <button
                      key={opt.id}
                      type="button"
                      role="menuitem"
                      onClick={() => void handleSwitchAgent(opt.id)}
                      className={`flex w-full items-center justify-between px-3 py-2 text-left text-[12.5px] transition-colors ${
                        sdkType === opt.id ? "font-medium" : ""
                      }`}
                      style={
                        sdkType === opt.id
                          ? { color: "var(--color-accent-2)", background: "var(--color-accent-dim)" }
                          : { color: "var(--color-text-2)" }
                      }
                      onMouseEnter={(e) => {
                        if (sdkType !== opt.id)
                          e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
                      }}
                      onMouseLeave={(e) => {
                        if (sdkType !== opt.id) e.currentTarget.style.background = "transparent";
                      }}
                    >
                      {opt.label}
                      {sdkType === opt.id && <span className="text-[10px] opacity-70">✓</span>}
                    </button>
                  ))}
                </div>
              </GlassPopover>
            </div>
          )}
          {/* 供应商切换图标按钮：平时只显示当前供应商图标，点击展开列表显示配置的供应商名称 */}
          <div className="relative shrink-0" ref={providerMenuRef}>
            <button
              type="button"
              onClick={() => setProviderMenuOpen((v) => !v)}
              aria-haspopup="menu"
              aria-expanded={providerMenuOpen}
              title={t("switch_provider")}
              className="grid h-6 w-6 shrink-0 place-items-center rounded-md border transition-colors focus-ring"
              style={{ borderColor: "var(--color-hairline)" }}
              onMouseEnter={(e) => {
                e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
              }}
              onMouseLeave={(e) => {
                e.currentTarget.style.background = "transparent";
              }}
            >
              {activeCredential ? (
                <PresetIcon iconKey={activeCredential.icon_key ?? null} size={14} />
              ) : (
                <SkinIcon className="h-3.5 w-3.5" />
              )}
            </button>
            <GlassPopover
              open={providerMenuOpen}
              onClose={() => setProviderMenuOpen(false)}
              anchorRef={providerMenuRef}
              sideOffset={4}
              width="w-64"
              layer="assistantPopover"
              showHairline={false}
            >
              <div role="menu" className="py-1">
                {agentCredentials.length === 0 ? (
                  <div
                    className="px-3 py-2 text-[11.5px] leading-relaxed"
                    style={{ color: "var(--color-text-3)" }}
                  >
                    {t("no_agent_credentials")}
                  </div>
                ) : (
                  agentCredentials.map((cred) => {
                    const isActive = cred.id === activeCredential?.id || cred.is_active;
                    return (
                      <button
                        key={cred.id}
                        type="button"
                        role="menuitem"
                        onClick={() => void handleSwitchProvider(cred.id)}
                        className={`flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-[12.5px] transition-colors ${
                          isActive ? "font-medium" : ""
                        }`}
                        style={
                          isActive
                            ? { color: "var(--color-accent-2)", background: "var(--color-accent-dim)" }
                            : { color: "var(--color-text-2)" }
                        }
                        onMouseEnter={(e) => {
                          if (!isActive)
                            e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
                        }}
                        onMouseLeave={(e) => {
                          if (!isActive) e.currentTarget.style.background = "transparent";
                        }}
                      >
                        <span className="flex min-w-0 items-center gap-2">
                          <PresetIcon iconKey={cred.icon_key ?? null} size={16} />
                          <span className="truncate">{cred.display_name}</span>
                        </span>
                        {isActive && <span className="text-[10px] opacity-70">✓</span>}
                      </button>
                    );
                  })
                )}
              </div>
            </GlassPopover>
          </div>
          {/* 模型切换：无框线按钮 + 下拉菜单，交互与 Agent 切换一致，点击切换对应供应商配置的模型 */}
          <div className="relative min-w-0 shrink-0" ref={modelMenuRef}>
            <button
              type="button"
              onClick={() => setModelMenuOpen((v) => !v)}
              aria-haspopup="menu"
              aria-expanded={modelMenuOpen}
              aria-label={t("agent_model_label")}
              title={t("agent_model_label")}
              className="flex min-w-0 max-w-44 items-center gap-1 rounded px-1 py-0.5 transition-colors focus-ring"
              style={{ color: "var(--color-text)" }}
              onMouseEnter={(e) => {
                e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
              }}
              onMouseLeave={(e) => {
                e.currentTarget.style.background = "transparent";
              }}
            >
              <span
                className="truncate font-mono text-[12px] leading-[1.1]"
                style={{ color: agentModel ? undefined : "var(--color-text-3)" }}
              >
                {agentModel || t("agent_model_label")}
              </span>
              <ChevronDown
                className={`h-3 w-3 shrink-0 text-text-3 transition-transform ${
                  modelMenuOpen ? "rotate-180" : ""
                }`}
              />
            </button>
            <GlassPopover
              open={modelMenuOpen}
              onClose={() => setModelMenuOpen(false)}
              anchorRef={modelMenuRef}
              sideOffset={4}
              width="w-56"
              layer="assistantPopover"
              showHairline={false}
            >
              <div role="menu" className="py-1">
                {agentModels.length === 0 ? (
                  <div
                    className="px-3 py-2 text-[11.5px] leading-relaxed"
                    style={{ color: "var(--color-text-3)" }}
                  >
                    {t("no_agent_models")}
                  </div>
                ) : (
                  agentModels.map((option) => {
                    const isActive = option.request_model === agentModel;
                    return (
                      <button
                        key={option.request_model}
                        type="button"
                        role="menuitem"
                        onClick={() => {
                          setModelMenuOpen(false);
                          handleModelChange(option.request_model);
                        }}
                        className={`flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-[12.5px] transition-colors ${
                          isActive ? "font-medium" : ""
                        }`}
                        style={
                          isActive
                            ? { color: "var(--color-accent-2)", background: "var(--color-accent-dim)" }
                            : { color: "var(--color-text-2)" }
                        }
                        onMouseEnter={(e) => {
                          if (!isActive)
                            e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
                        }}
                        onMouseLeave={(e) => {
                          if (!isActive) e.currentTarget.style.background = "transparent";
                        }}
                      >
                        <span className="truncate font-mono">{option.menu_name}</span>
                        {isActive && <span className="text-[10px] opacity-70">✓</span>}
                      </button>
                    );
                  })
                )}
              </div>
            </GlassPopover>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          <SessionSelector onSwitch={voidPromise(switchSession)} onDelete={voidPromise(deleteSession)} />
          <button
            type="button"
            onClick={createNewSession}
            className="rounded p-1 transition-colors focus-ring"
            style={{ color: "var(--color-text-3)" }}
            onMouseEnter={(e) => {
              e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
              e.currentTarget.style.color = "var(--color-text)";
            }}
            onMouseLeave={(e) => {
              e.currentTarget.style.background = "transparent";
              e.currentTarget.style.color = "var(--color-text-3)";
            }}
            title={t("new_session")}
            aria-label={t("new_session")}
          >
            <Plus aria-hidden className="h-4 w-4" />
          </button>
        </div>
      </div>

      {/* Context banner */}
      <ContextBanner />

      {/* Messages */}
      <div className="relative min-h-0 min-w-0 flex-1">
        <div ref={scrollRef} className="h-full min-h-0 min-w-0 space-y-3 overflow-y-auto overflow-x-hidden px-3 py-3 pr-10">
        {allTurns.length === 0 && !messagesLoading && !startupFailure && (
          <div className="flex h-full flex-col items-center justify-center text-center">
            <div
              className="mb-3 grid h-12 w-12 place-items-center rounded-2xl"
              style={{
                background:
                  "linear-gradient(135deg, var(--color-accent-dim), var(--color-shell-copilot-btn))",
                border: "1px solid var(--color-accent-soft)",
                boxShadow: "0 0 24px -8px var(--color-accent-glow)",
              }}
            >
              <Bot
                className="h-5 w-5"
                style={{ color: "var(--color-accent-2)" }}
              />
            </div>
            <p
              className="display-serif text-[14px] font-semibold"
              style={{ color: "var(--color-text)" }}
            >
              {t("start_chat_hint")}
            </p>
            <p
              className="mt-1 text-[11.5px]"
              style={{ color: "var(--color-text-3)" }}
            >
              {t("quick_skill_hint")}
            </p>
          </div>
        )}
        {allTurns.map((turn, i) => (
          <MessageRow
            key={turn.uuid || `turn-${i}`}
            anchorId={messageAnchorId(turn, i)}
            turn={turn}
            streaming={turn === draftTurn}
            editable={canEditUserTurn(turn, {
              sessionStatus,
              hasPendingQuestion: Boolean(pendingQuestion),
              isSending: sending,
            })}
            editing={Boolean(turn.uuid) && turn.uuid === editingTurnUuid}
            submitting={sending}
            onStartEdit={setEditingTurnUuid}
            onCancelEdit={() => setEditingTurnUuid(null)}
            onSubmitEdit={handleSubmitEdit}
          />
        ))}
        {startupFailure && (
          // 改写失败时原始输入留在仍开着的编辑器里，重试由它的「重新发送」发起：
          // 卡片这里给重试只会重放主输入框的无关内容（为空时更是毫无反应）
          <AgentFailureCard
            failure={startupFailure}
            onRetry={startupFailureOrigin === "rewrite" ? undefined : handleSend}
          />
        )}
        </div>
        <MessageRail turns={allTurns} scrollContainerRef={scrollRef} />
      </div>

      {pendingQuestion && (
        <PendingQuestionWizard
          pendingQuestion={pendingQuestion}
          answeringQuestion={answeringQuestion}
          error={error}
          onSubmitAnswers={voidPromise(answerQuestion)}
        />
      )}

      <TodoListPanel turns={turns} draftTurn={draftTurn} />

      {!pendingQuestion && (error || attachError) && (
        <div
          role="alert"
          aria-live="assertive"
          className="px-3 py-2 text-[11.5px]"
          style={{
            borderTop: "1px solid oklch(0.70 0.18 25 / 0.3)",
            background: "oklch(0.70 0.18 25 / 0.12)",
            color: "oklch(0.85 0.10 25)",
          }}
        >
          {error || attachError}
        </div>
      )}

      {/* Input area */}
      <div
        className="px-3 pb-3"
        style={{ borderTop: "1px solid var(--color-hairline-soft)" }}
      >
        {/* Thumbnail strip */}
        {attachedImages.length > 0 && (
          <div className="mb-2 flex flex-wrap gap-2">
            {attachedImages.map((img) => (
              <div key={img.id} className="relative">
                <button
                  type="button"
                  className="h-16 w-16 cursor-pointer border-0 bg-transparent p-0"
                  onClick={() => setLightboxSrc(img.dataUrl)}
                  aria-label={t("enlarge_image")}
                >
                  <img
                    src={img.dataUrl}
                    alt={t("assistant_input")}
                    className="h-16 w-16 rounded-md object-cover"
                    style={{ border: "1px solid var(--color-hairline)" }}
                  />
                </button>
                <button
                  type="button"
                  onClick={() => removeImage(img.id)}
                  className="absolute -right-1 -top-1 flex h-4 w-4 items-center justify-center rounded-full transition-colors focus-ring"
                  style={{
                    background: "var(--color-shell-copilot-field)",
                    color: "var(--color-text-2)",
                    border: "1px solid var(--color-hairline)",
                  }}
                  onMouseEnter={(e) => {
                    e.currentTarget.style.background = "var(--color-danger)";
                    e.currentTarget.style.color = "oklch(0.14 0 0)";
                    e.currentTarget.style.borderColor = "var(--color-danger)";
                  }}
                  onMouseLeave={(e) => {
                    e.currentTarget.style.background = "var(--color-shell-copilot-field)";
                    e.currentTarget.style.color = "var(--color-text-2)";
                    e.currentTarget.style.borderColor = "var(--color-hairline)";
                  }}
                  aria-label={t("remove_image")}
                >
                  <X className="h-2.5 w-2.5" />
                </button>
              </div>
            ))}
          </div>
        )}

        <div
          className="relative flex items-end gap-2 rounded-lg px-3 py-2 transition-colors"
          style={{
            border: `1px solid ${isDragOver ? "var(--color-accent)" : "var(--color-hairline)"}`,
            background: isDragOver
              ? "var(--color-accent-dim)"
              : "var(--color-shell-copilot-btn)",
            backdropFilter: "blur(8px)",
            WebkitBackdropFilter: "blur(8px)",
            boxShadow: isDragOver
              ? "0 0 0 3px var(--color-accent-soft), inset 0 1px 0 oklch(1 0 0 / 0.04)"
              : "inset 0 1px 0 oklch(1 0 0 / 0.04)",
          }}
          onDragOver={handleDragOver}
          onDragLeave={handleDragLeave}
          onDrop={handleDrop}
        >
          {showSlashMenu && (
            <SlashCommandMenu
              ref={slashMenuRef}
              filter={slashFilter}
              onSelect={handleSlashSelect}
            />
          )}
          <textarea
            ref={textareaRef}
            role="combobox"
            value={localInput}
            onChange={handleInputChange}
            onKeyDown={handleKeyDown}
            onCompositionStart={() => {
              isComposingRef.current = true;
            }}
            onCompositionEnd={() => {
              isComposingRef.current = false;
            }}
            onPaste={handlePaste}
            placeholder={inputPlaceholder}
            rows={1}
            aria-label={t("assistant_input")}
            aria-expanded={showSlashMenu}
            aria-controls={showSlashMenu ? "slash-command-menu" : undefined}
            aria-activedescendant={
              // eslint-disable-next-line react-hooks/refs -- aria-activedescendant 需实时读取 slashMenuRef 的派生值，改用回调 prop 需修改 SlashCommandMenu 接口，超出范围
              slashMenuRef.current?.activeDescendantId
            }
            className="flex-1 resize-none overflow-hidden bg-transparent text-[13px] outline-none"
            style={{
              maxHeight: `${MAX_TEXTAREA_HEIGHT_VH}vh`,
              color: "var(--color-text)",
            }}
            disabled={inputDisabled}
          />

          {/* Attachment button */}
          <button
            type="button"
            onClick={() => fileInputRef.current?.click()}
            disabled={attachDisabled}
            className="shrink-0 rounded p-1.5 transition-colors focus-ring disabled:opacity-30"
            style={{ color: "var(--color-text-3)" }}
            onMouseEnter={(e) => {
              if (!attachDisabled) {
                e.currentTarget.style.background = "var(--color-shell-copilot-hover)";
                e.currentTarget.style.color = "var(--color-text)";
              }
            }}
            onMouseLeave={(e) => {
              e.currentTarget.style.background = "transparent";
              e.currentTarget.style.color = "var(--color-text-3)";
            }}
            title={attachedImages.length >= MAX_IMAGES ? t("max_images_hint", { count: MAX_IMAGES }) : t("attach_image")}
            aria-label={t("attach_image")}
          >
            <Paperclip className="h-4 w-4" />
          </button>

          {isRunning ? (
            <button
              onClick={voidPromise(interrupt)}
              className="shrink-0 rounded p-1.5 transition-colors focus-ring"
              style={{ color: "var(--color-danger)" }}
              onMouseEnter={(e) => {
                e.currentTarget.style.background = "oklch(0.70 0.18 25 / 0.15)";
              }}
              onMouseLeave={(e) => {
                e.currentTarget.style.background = "transparent";
              }}
              title={t("stop_session")}
              aria-label={t("stop_session")}
            >
              <Square className="h-4 w-4" />
            </button>
          ) : (
            <button
              onClick={handleSend}
              disabled={(!localInput.trim() && attachedImages.length === 0) || inputDisabled}
              className="shrink-0 rounded-md p-1.5 transition-opacity focus-ring disabled:cursor-not-allowed disabled:opacity-30"
              style={{
                color: "oklch(0.14 0 0)",
                background:
                  "linear-gradient(180deg, var(--color-accent-2), var(--color-accent))",
                boxShadow:
                  "inset 0 1px 0 oklch(1 0 0 / 0.3), 0 4px 14px -4px var(--color-accent-glow)",
              }}
              title={t("send_message")}
              aria-label={t("send_message")}
            >
              <Send className="h-4 w-4" />
            </button>
          )}
        </div>

        {/* Hidden file input */}
        <input
          ref={fileInputRef}
          type="file"
          multiple
          accept="image/*"
          aria-label={t("upload_attachment_aria")}
          className="hidden"
          onChange={handleFileSelect}
        />
      </div>

      {lightboxSrc && (
        <ImageLightbox
          src={lightboxSrc}
          alt={t("assistant_input")}
          onClose={() => setLightboxSrc(null)}
        />
      )}
    </div>
  );
}
