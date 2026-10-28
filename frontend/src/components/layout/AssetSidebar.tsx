import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "wouter";
import { useTranslation } from "react-i18next";
import {
  ChevronLeft,
  ChevronRight,
  Clapperboard,
  LayoutDashboard,
  BookOpen,
  Users,
  Landmark,
  Package,
  Plus,
  Search,
  ShoppingBag,
  Workflow,
} from "lucide-react";
import { useProjectsStore } from "@/stores/projects-store";
import { useCostStore } from "@/stores/cost-store";
import { useAppStore } from "@/stores/app-store";
import { API } from "@/api";
import { ConfirmDialog } from "@/components/ui/ConfirmDialog";
import { GlassModal } from "@/components/ui/GlassModal";
import { PrimaryButton } from "@/components/ui/PrimaryButton";
import { SecondaryButton } from "@/components/ui/SecondaryButton";
import { errMsg } from "@/utils/async";
import { useDemoWorkbench } from "@/onboarding/use-demo-workbench";
import { isDemoProject } from "@/onboarding/demo-project";
import { EpisodeCard } from "./EpisodeCard";

interface AssetSidebarProps {
  className?: string;
}

const SIDEBAR_DEFAULT_WIDTH = 256;
const SIDEBAR_MIN_WIDTH = 200;
const SIDEBAR_MAX_WIDTH = 420;
const SIDEBAR_COLLAPSED_WIDTH = 64;
const SIDEBAR_WIDTH_STORAGE_KEY = "shotwise:sidebarWidth";

function loadSidebarWidth(): number {
  const saved = Number(window.localStorage.getItem(SIDEBAR_WIDTH_STORAGE_KEY));
  return Number.isFinite(saved) && saved >= SIDEBAR_MIN_WIDTH && saved <= SIDEBAR_MAX_WIDTH
    ? Math.round(saved)
    : SIDEBAR_DEFAULT_WIDTH;
}

interface NavItem {
  key: string;
  path: string;
  label: string;
  icon: React.ComponentType<{ className?: string; style?: React.CSSProperties }>;
  meta?: number;
}

/**
 * 工作台侧栏 v3：
 * - 工作区导航（5 个胶囊按钮：项目概览 / 源文件 / 角色集 / 场景库 / 道具库）
 * - 分集列表（搜索 + 卡片列表，每张卡片含缩略+状态+进度+费用）
 * - 折叠态（64px）：仅图标 + Ex 字符
 * - 右缘可拖拽调宽（200–420px，持久化）；拖到低于 200px 阈值自动折叠，
 *   折叠态向外拖越过阈值展开，双击手柄切换折叠
 */
export function AssetSidebar({ className }: AssetSidebarProps) {
  const { t } = useTranslation(["common", "dashboard"]);
  const { currentProjectName, currentProjectData, refreshProject } = useProjectsStore();
  const debouncedFetchCost = useCostStore((s) => s.debouncedFetch);
  const [location, setLocation] = useLocation();
  const [collapsed, setCollapsed] = useState(false);
  const [width, setWidth] = useState(loadSidebarWidth);
  const [dragging, setDragging] = useState(false);
  const dragStateRef = useRef<{ startX: number; startWidth: number; wasCollapsed: boolean } | null>(null);
  const restoreBodyStyleRef = useRef<{ cursor: string; userSelect: string } | null>(null);
  const [search, setSearch] = useState("");
  const [episodeToDelete, setEpisodeToDelete] = useState<number | null>(null);
  const [episodeToRenumber, setEpisodeToRenumber] = useState<number | null>(null);
  const [numberDraft, setNumberDraft] = useState("");
  const [episodeActionLoading, setEpisodeActionLoading] = useState(false);

  const characterCount = Object.keys(currentProjectData?.characters ?? {}).length;
  const sceneCount = Object.keys(currentProjectData?.scenes ?? {}).length;
  const propCount = Object.keys(currentProjectData?.props ?? {}).length;
  const productCount = Object.keys(currentProjectData?.products ?? {}).length;
  const episodes = currentProjectData?.episodes ?? [];
  // 广告/短片项目恒单集：隐藏「集」语义（标题/计数/搜索/添加），直达唯一视频
  const isAd = currentProjectData?.content_mode === "ad";

  const sourceFilesVersion = useAppStore((s) => s.sourceFilesVersion);
  const [sourceCount, setSourceCount] = useState<number>(0);

  // 演示项目没有服务端侧数据，源文件计数跳过（导航与分集列表照常渲染）
  const demoMode = useDemoWorkbench();

  useEffect(() => {
    if (currentProjectName) debouncedFetchCost(currentProjectName);
  }, [currentProjectName, debouncedFetchCost]);

  useEffect(() => {
    // demoMode 演示→真实切换时先于 store 变为 false，currentProjectName 单独判一次
    // 兜住这一帧仍读到旧演示项目名的窗口，避免对不存在的演示项目发一次必然失败的请求。
    if (!currentProjectName || demoMode || isDemoProject(currentProjectName)) return;
    let cancelled = false;
    API.listFiles(currentProjectName)
      .then((res) => {
        if (!cancelled) setSourceCount(res.files?.source?.length ?? 0);
      })
      .catch(() => {
        // 失败时保留上一份成功值，避免把网络/权限错误伪装成 0
      });
    return () => {
      cancelled = true;
    };
  }, [currentProjectName, sourceFilesVersion, demoMode]);

  // Derive active episode from `/episodes/:id`
  const activeEp = useMemo(() => {
    const m = location.match(/^\/episodes\/(\d+)/);
    return m ? parseInt(m[1], 10) : null;
  }, [location]);

  const navItems: NavItem[] = [
    { key: "overview", path: "/", label: t("dashboard:workspace_nav_overview"), icon: LayoutDashboard },
    ...(demoMode
      ? []
      : [
          { key: "skills", path: "/skills", label: t("dashboard:workspace_nav_skills"), icon: Workflow },
          { key: "media", path: "/media", label: t("dashboard:workspace_nav_media"), icon: Package },
          { key: "assembly", path: "/assembly", label: t("dashboard:workspace_nav_assembly"), icon: Clapperboard },
          { key: "board", path: "/creative-board", label: t("dashboard:workspace_nav_board"), icon: LayoutDashboard },
        ]),
    // 演示项目没有可切片的源文件，且后端不存在该项目，隐藏入口而非渲染必然报错的空页
    ...(demoMode
      ? []
      : [
          {
            key: "source",
            path: "/source",
            label: t("dashboard:workspace_nav_source"),
            icon: BookOpen,
            meta: sourceCount,
          },
        ]),
    {
      key: "characters",
      path: "/characters",
      label: t("dashboard:workspace_nav_characters"),
      icon: Users,
      meta: characterCount,
    },
    {
      key: "scenes",
      path: "/scenes",
      label: t("dashboard:workspace_nav_scenes"),
      icon: Landmark,
      meta: sceneCount,
    },
    {
      key: "props",
      path: "/props",
      label: t("dashboard:workspace_nav_props"),
      icon: Package,
      meta: propCount,
    },
    ...(isAd
      ? [
          {
            key: "products",
            path: "/products",
            label: t("dashboard:workspace_nav_products"),
            icon: ShoppingBag,
            meta: productCount,
          },
        ]
      : []),
  ];

  const isNavActive = (item: NavItem): boolean => {
    if (item.path === "/") return location === "/";
    return location === item.path || location.startsWith(item.path + "/");
  };

  // ad 隐藏搜索框，残留的 search state 不参与过滤，避免唯一视频入口被吞
  const filteredEps = isAd
    ? episodes
    : episodes.filter(
        (ep) =>
          !search ||
          ep.title.includes(search) ||
          String(ep.display_episode ?? ep.episode).includes(search),
      );

  const episodeForDelete = episodeToDelete == null
    ? null
    : episodes.find((ep) => ep.episode === episodeToDelete) ?? null;
  const episodeForRenumber = episodeToRenumber == null
    ? null
    : episodes.find((ep) => ep.episode === episodeToRenumber) ?? null;

  const handleDeleteEpisode = async () => {
    if (!currentProjectName || episodeToDelete == null) return;
    setEpisodeActionLoading(true);
    try {
      await API.deleteEpisode(currentProjectName, episodeToDelete);
      const wasActive = activeEp === episodeToDelete;
      setEpisodeToDelete(null);
      await refreshProject(currentProjectName);
      if (wasActive) setLocation("/");
      useAppStore.getState().pushToast(t("dashboard:episode_deleted"), "success");
    } catch (error) {
      useAppStore.getState().pushToast(
        errMsg(error, t("dashboard:episode_delete_failed")),
        "error",
      );
    } finally {
      setEpisodeActionLoading(false);
    }
  };

  const handleRenumberEpisode = async () => {
    if (!currentProjectName || episodeToRenumber == null) return;
    const nextNumber = Number.parseInt(numberDraft, 10);
    if (!Number.isInteger(nextNumber) || nextNumber < 1 || nextNumber > episodes.length) {
      useAppStore.getState().pushToast(
        t("dashboard:episode_number_invalid", { max: episodes.length }),
        "error",
      );
      return;
    }
    setEpisodeActionLoading(true);
    try {
      await API.updateEpisodeDisplayNumber(currentProjectName, episodeToRenumber, nextNumber);
      setEpisodeToRenumber(null);
      await refreshProject(currentProjectName);
      useAppStore.getState().pushToast(t("dashboard:episode_number_updated"), "success");
    } catch (error) {
      useAppStore.getState().pushToast(
        errMsg(error, t("dashboard:episode_number_update_failed")),
        "error",
      );
    } finally {
      setEpisodeActionLoading(false);
    }
  };

  const restoreBodyStyle = useCallback(() => {
    const saved = restoreBodyStyleRef.current;
    if (saved) {
      document.body.style.cursor = saved.cursor;
      document.body.style.userSelect = saved.userSelect;
      restoreBodyStyleRef.current = null;
    }
  }, []);

  const handleResizeMouseDown = useCallback(
    (e: React.MouseEvent<HTMLDivElement>) => {
      // 仅响应主键，避免右键/中键意外进入拖拽态
      if (e.button !== 0) return;
      e.preventDefault();
      dragStateRef.current = { startX: e.clientX, startWidth: width, wasCollapsed: collapsed };
      restoreBodyStyleRef.current = {
        cursor: document.body.style.cursor,
        userSelect: document.body.style.userSelect,
      };
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";
      setDragging(true);
    },
    [width, collapsed],
  );

  const handleResizeDoubleClick = useCallback(() => {
    setCollapsed((c) => !c);
  }, []);

  useEffect(() => {
    if (!dragging) return;

    const finishResize = () => {
      dragStateRef.current = null;
      restoreBodyStyle();
      setDragging(false);
    };

    const onMouseMove = (e: MouseEvent) => {
      const drag = dragStateRef.current;
      if (!drag) return;
      // 主键已在中途松开（如焦点切走时）→ 主动收尾
      if ((e.buttons & 1) === 0) {
        finishResize();
        return;
      }
      // 手柄在侧栏右缘，鼠标向右 (clientX 增大) → 宽度增大
      const base = drag.wasCollapsed ? SIDEBAR_COLLAPSED_WIDTH : drag.startWidth;
      const next = base + (e.clientX - drag.startX);
      if (drag.wasCollapsed) {
        // 折叠态向外拖：越过阈值展开并重定基点，继续拖动无宽度跳变
        if (next >= SIDEBAR_MIN_WIDTH) {
          dragStateRef.current = { startX: e.clientX, startWidth: next, wasCollapsed: false };
          setCollapsed(false);
          setWidth(next);
        }
      } else if (next < SIDEBAR_MIN_WIDTH) {
        // 缩到阈值以下 → 折叠；width 不动，展开按钮回到上次宽度
        dragStateRef.current = {
          startX: e.clientX,
          startWidth: SIDEBAR_COLLAPSED_WIDTH,
          wasCollapsed: true,
        };
        setCollapsed(true);
      } else {
        setWidth(Math.min(next, SIDEBAR_MAX_WIDTH));
      }
    };

    window.addEventListener("mousemove", onMouseMove);
    window.addEventListener("mouseup", finishResize);
    // 鼠标在窗外松开时 mouseup 可能不触发，blur 兜底防止卡死
    window.addEventListener("blur", finishResize);
    return () => {
      window.removeEventListener("mousemove", onMouseMove);
      window.removeEventListener("mouseup", finishResize);
      window.removeEventListener("blur", finishResize);
      // 组件意外卸载时兜底清理 body 样式
      restoreBodyStyle();
    };
  }, [dragging, restoreBodyStyle]);

  // 非拖拽期的展开宽度变化（含初次加载的回写）持久化；折叠态不覆盖，展开时回到上次宽度
  useEffect(() => {
    if (dragging || collapsed) return;
    window.localStorage.setItem(SIDEBAR_WIDTH_STORAGE_KEY, String(width));
  }, [dragging, collapsed, width]);

  return (
    <>
    <aside
      className={`relative flex flex-col overflow-hidden ${className ?? ""}`}
      style={{
        width: collapsed ? SIDEBAR_COLLAPSED_WIDTH : width,
        flexShrink: 0,
        transition: dragging ? "none" : "width .18s ease",
        borderRight: "1px solid var(--color-hairline)",
        background:
          "linear-gradient(180deg, var(--color-shell-side-a), var(--color-shell-side-b))",
        boxShadow: "inset -1px 0 0 oklch(1 0 0 / 0.015)",
      }}
    >
      {/* ---- Workspace nav ---- */}
      <div className="px-2.5 pb-1.5 pt-2.5">
        {navItems.map((item) => {
          const Icon = item.icon;
          const active = isNavActive(item);
          return (
            <button
              key={item.key}
              type="button"
              onClick={() => setLocation(item.path)}
              title={collapsed ? item.label : ""}
              aria-label={collapsed ? item.label : undefined}
              className="relative mb-px flex w-full items-center gap-2.5 rounded-md px-2 py-1.5 transition-colors focus-ring hover:bg-[var(--color-shell-hover)]"
              style={{
                background: active
                  ? "linear-gradient(90deg, var(--color-accent-soft), var(--color-accent-dim) 70%, transparent)"
                  : "transparent",
                color: active ? "var(--color-text)" : "var(--color-text-2)",
              }}
            >
              {active && (
                <span
                  className="absolute -left-px top-[7px] bottom-[7px] w-0.5 rounded"
                  style={{
                    background: "var(--color-accent)",
                    boxShadow: "0 0 8px var(--color-accent-glow)",
                  }}
                />
              )}
              <span
                className="grid w-4 shrink-0 place-items-center"
                style={{ color: active ? "var(--color-accent-2)" : "var(--color-text-3)" }}
              >
                <Icon className="h-4 w-4" />
              </span>
              {!collapsed && (
                <>
                  <span
                    className="flex-1 text-left text-[13px]"
                    style={{
                      fontWeight: active ? 600 : 500,
                      letterSpacing: "-0.05px",
                    }}
                  >
                    {item.label}
                  </span>
                  {item.meta != null && (
                    <span
                      className="num rounded-[3px] px-1.5 py-px text-[10.5px]"
                      style={{
                        color: active ? "var(--color-text-3)" : "var(--color-text-4)",
                        background: active ? "var(--color-shell-badge)" : "transparent",
                      }}
                    >
                      {item.meta}
                    </span>
                  )}
                </>
              )}
            </button>
          );
        })}
      </div>

      <div
        className="mx-3.5 my-1 h-px"
        style={{ background: "var(--color-hairline-soft)" }}
      />

      {/* ---- Episodes ---- */}
      {!collapsed ? (
        <>
          <div className="flex items-center gap-2 px-3.5 pb-1.5 pt-2.5">
            <span
              className="text-[10.5px] font-bold uppercase"
              style={{ color: "var(--color-text-4)", letterSpacing: "0.8px" }}
            >
              {isAd
                ? t("dashboard:ad_video_section_title")
                : t("dashboard:episodes_section_title")}
            </span>
            {!isAd && (
              <>
                <span className="num text-[10px]" style={{ color: "var(--color-text-4)" }}>
                  {episodes.length}
                </span>
                <span className="flex-1" />
                <button
                  type="button"
                  disabled
                  aria-disabled="true"
                  className="grid h-5 w-5 place-items-center rounded focus-ring disabled:cursor-not-allowed disabled:opacity-50"
                  style={{
                    background: "var(--color-shell-btn-2)",
                    color: "var(--color-text-3)",
                  }}
                  title={t("dashboard:add_episode_unavailable")}
                  aria-label={t("dashboard:add_episode")}
                >
                  <Plus className="h-3 w-3" />
                </button>
              </>
            )}
          </div>

          {!isAd && (
            <div className="px-2.5 pb-2">
              <div
                className="flex items-center gap-1.5 rounded-md px-2 py-1.5"
                style={{
                  background: "var(--color-shell-field)",
                  border: "1px solid var(--color-hairline)",
                }}
              >
                <Search
                  className="h-3 w-3 shrink-0"
                  style={{ color: "var(--color-text-4)" }}
                />
                <input
                  type="search"
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                  placeholder={t("dashboard:episode_search_placeholder")}
                  aria-label={t("dashboard:episode_search_placeholder")}
                  className="min-w-0 flex-1 bg-transparent text-xs outline-none focus-ring"
                  style={{ color: "var(--color-text)" }}
                />
              </div>
            </div>
          )}

          <div className="flex-1 overflow-y-auto px-2 pb-2.5">
            {filteredEps.length === 0 ? (
              <div
                className="px-2 py-6 text-center text-[11px] italic"
                style={{ color: "var(--color-text-4)" }}
              >
                {episodes.length === 0
                  ? t("dashboard:no_episodes_yet")
                  : t("dashboard:no_episode_search_results")}
              </div>
            ) : (
              filteredEps.map((ep) => (
                <EpisodeCard
                  key={ep.episode}
                  ep={ep}
                  active={ep.episode === activeEp}
                  onClick={() => setLocation(`/episodes/${ep.episode}`)}
                  onDelete={!isAd ? () => setEpisodeToDelete(ep.episode) : undefined}
                  onEditNumber={!isAd ? () => {
                    setEpisodeToRenumber(ep.episode);
                    setNumberDraft(String(ep.display_episode ?? ep.episode));
                  } : undefined}
                  showEpisodeBadge={!isAd}
                  fallbackTitle={isAd ? currentProjectData?.title : undefined}
                />
              ))
            )}
          </div>
        </>
      ) : (
        <div className="flex-1 overflow-y-auto px-2.5 py-1.5">
          {filteredEps.map((ep) => {
            const epLabel = isAd
              ? t("dashboard:ad_video_section_title")
              : t("dashboard:episode_collapsed_button_label", {
                  episode: ep.episode,
                  title: ep.title,
                });
            return (
            <button
              key={ep.episode}
              type="button"
              onClick={() => setLocation(`/episodes/${ep.episode}`)}
              title={epLabel}
              aria-label={epLabel}
              className="num mb-[3px] flex h-9 w-full items-center justify-center rounded-md text-[11px] font-bold focus-ring"
              style={{
                background: ep.episode === activeEp ? "var(--color-accent-dim)" : "transparent",
                color:
                  ep.episode === activeEp
                    ? "var(--color-accent-2)"
                    : "var(--color-text-3)",
              }}
            >
              {isAd ? <Clapperboard className="h-4 w-4" aria-hidden /> : `E${ep.episode}`}
            </button>
            );
          })}
        </div>
      )}

      {/* ---- Collapse footer ---- */}
      <div
        className="flex items-center gap-2 px-2.5 py-2"
        style={{
          borderTop: "1px solid var(--color-hairline)",
          background: "var(--color-shell-footer)",
        }}
      >
        <button
          type="button"
          onClick={() => setCollapsed((c) => !c)}
          className="grid h-7 w-7 place-items-center rounded-md focus-ring"
          aria-expanded={!collapsed}
          style={{
            background: "var(--color-shell-btn)",
            color: "var(--color-text-3)",
          }}
          title={collapsed ? t("dashboard:sidebar_expand") : t("dashboard:sidebar_collapse")}
          aria-label={
            collapsed ? t("dashboard:sidebar_expand") : t("dashboard:sidebar_collapse")
          }
        >
          {collapsed ? (
            <ChevronRight className="h-3.5 w-3.5" />
          ) : (
            <ChevronLeft className="h-3.5 w-3.5" />
          )}
        </button>
      </div>
      {/* ---- 右缘拖拽调宽手柄：缩到阈值以下折叠，折叠态拖出展开，双击切换 ---- */}
      {/* role="separator" 是 WAI-ARIA Authoring Practices 的 window splitter 模式， */}
      {/* jsx-a11y 默认未把 separator 列入 interactive 角色，就地抑制该规则（同 AssistantResizeHandle）。 */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
      <div
        role="separator"
        aria-orientation="vertical"
        aria-valuemin={SIDEBAR_MIN_WIDTH}
        aria-valuemax={SIDEBAR_MAX_WIDTH}
        aria-valuenow={width}
        aria-label={t("dashboard:sidebar_resize")}
        title={t("dashboard:sidebar_resize")}
        onMouseDown={handleResizeMouseDown}
        onDoubleClick={handleResizeDoubleClick}
        className="group absolute inset-y-0 right-0 z-10 w-1.5 cursor-col-resize select-none"
      >
        <div
          className={`absolute inset-y-0 right-0 w-px transition-colors duration-150 ${
            dragging
              ? "bg-[var(--color-accent)]"
              : "bg-transparent group-hover:bg-[var(--color-accent)]"
          }`}
        />
      </div>
    </aside>

    <ConfirmDialog
      open={episodeForDelete !== null}
      title={t("dashboard:confirm_delete_episode_title")}
      description={t("dashboard:confirm_delete_episode_description", {
        episode: episodeForDelete?.display_episode ?? episodeForDelete?.episode ?? "",
      })}
      confirmLabel={t("dashboard:delete_episode")}
      loadingLabel={t("dashboard:deleting_episode")}
      cancelLabel={t("dashboard:cancel_action")}
      tone="danger"
      loading={episodeActionLoading}
      onConfirm={handleDeleteEpisode}
      onCancel={() => {
        if (!episodeActionLoading) setEpisodeToDelete(null);
      }}
    />

    <GlassModal
      open={episodeForRenumber !== null}
      onClose={episodeActionLoading ? () => {} : () => setEpisodeToRenumber(null)}
      ariaLabel={t("dashboard:edit_episode_number")}
      closeOnBackdrop={!episodeActionLoading}
      closeOnEscape={!episodeActionLoading}
    >
      <div className="px-6 pb-6 pt-5">
        <h2 className="display-serif text-[17px] font-semibold tracking-tight" style={{ color: "var(--color-text)" }}>
          {t("dashboard:edit_episode_number")}
        </h2>
        <p className="mt-1 text-[12.5px] leading-relaxed" style={{ color: "var(--color-text-3)" }}>
          {t("dashboard:edit_episode_number_description")}
        </p>
        <label className="mt-4 block text-[11px] font-semibold" style={{ color: "var(--color-text-3)" }}>
          {t("dashboard:episode_number_label")}
          <input
            type="number"
            min={1}
            max={episodes.length}
            value={numberDraft}
            onChange={(event) => setNumberDraft(event.target.value)}
            className="mt-1.5 w-full rounded-md border border-hairline bg-[var(--color-shell-field)] px-3 py-2 text-sm text-text outline-none focus:border-accent focus:ring-1 focus:ring-accent"
            disabled={episodeActionLoading}
          />
        </label>
        <div className="mt-5 flex justify-end gap-2">
          <SecondaryButton
            size="sm"
            onClick={() => setEpisodeToRenumber(null)}
            disabled={episodeActionLoading}
          >
            {t("dashboard:cancel_action")}
          </SecondaryButton>
          <PrimaryButton
            size="sm"
            onClick={() => void handleRenumberEpisode()}
            disabled={episodeActionLoading}
          >
            {episodeActionLoading ? t("dashboard:saving_episode_number") : t("dashboard:save_episode_number")}
          </PrimaryButton>
        </div>
      </div>
    </GlassModal>
    </>
  );
}
