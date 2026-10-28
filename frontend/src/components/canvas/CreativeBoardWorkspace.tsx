import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent as ReactDragEvent, type PointerEvent as ReactPointerEvent, type WheelEvent as ReactWheelEvent } from "react";
import { createPortal } from "react-dom";
/* eslint-disable jsx-a11y/click-events-have-key-events, jsx-a11y/no-noninteractive-element-interactions -- board nodes are pointer-driven canvas objects. */
/* eslint-disable react-hooks/exhaustive-deps -- keyboard commands intentionally capture current board actions. */
import { Check, ChevronDown, CircleHelp, Copy, Download, FileText, FolderOpen, Globe2, Grid3X3, History, Image as ImageIcon, Keyboard, Landmark, Layers3, Link2, Loader2, LocateFixed, Maximize2, MoreHorizontal, MousePointer2, Move, Package, PanelLeft, Pencil, Plus, Search, Send, Sparkles, Trash2, Upload, UserRound, Video } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useLocation, useSearch } from "wouter";
import { API, getCreativeBoardConflictRevision, isCreativeBoardRevisionConflict } from "@/api";
import { enqueueCanvasImageAdvanced, enqueueCanvasImageSplit, enqueueImageEdit, type CanvasAdvancedOperation } from "@/actions/generation";
import { useProjectsStore } from "@/stores/projects-store";
import { useTasksStore } from "@/stores/tasks-store";
import { loadCanvasAssets, type CanvasAsset } from "./canvas-assets";
import { CanvasSaveStatus } from "./CanvasSaveStatus";
import { ConflictModal, type ConflictResolution } from "./ConflictModal";
import { handleCanvasSaveShortcut, useCanvasPersistence } from "./canvasPersistence";
import { CreativeBoardActions } from "./CreativeBoardActions";
import { BoardSelectionOverlay, type BoardToolbarAction, type BoardToolbarSelection, type ResizeCorner } from "./BoardSelectionOverlay";
import { BoardImageToolPanel, type ReferenceAsset } from "./BoardImageToolPanel";
import { CanvasImageEditorOverlay, type CanvasEditorOperation, type CanvasEditorSubmission } from "./CanvasImageEditorOverlay";

type BoardItem = { id: string; item_type: string; resource_type: string; resource_id: string; position: { x: number; y: number }; size: { width: number; height: number }; group_id?: string | null; display_settings?: Record<string, unknown> };
type BoardEdge = { id: string; source_item_id: string; target_item_id: string; relation: string };
type Board = { id: string; project_id: string; name: string; viewport: { x?: number; y?: number; zoom?: number }; items: BoardItem[]; edges: BoardEdge[]; revision: number };
type BoardHistory = { past: Board[]; future: Board[] };
type ResizeState = {
  id: string;
  corner: ResizeCorner;
  startX: number;
  startY: number;
  position: { x: number; y: number };
  size: { width: number; height: number };
};
type BoardDragState = {
  pointerId: number;
  startX: number;
  startY: number;
  active: boolean;
  items: Array<{ id: string; x: number; y: number }>;
};
type BoardOption = { id: string; name: string };
const BOARD_DRAG_THRESHOLD = 4;
function BoardSwitcher({ name, boardId, boardOptions, open, creating, editingBoardId, editingBoardName, boardActionsId, labels, onToggle, onCreate, onSwitch, onEditNameChange, onSaveName, onCancelEdit, onToggleActions, onOpenNewWindow, onRename, onDuplicate, onDelete }: {
  name: string;
  boardId?: string;
  boardOptions: BoardOption[];
  open: boolean;
  creating: boolean;
  editingBoardId: string | null;
  editingBoardName: string;
  boardActionsId: string | null;
  labels: { title: string; add: string; defaultName: string; nameInput: string; more: string; openNewWindow: string; rename: string; duplicate: string; delete: string };
  onToggle: () => void;
  onCreate: () => void;
  onSwitch: (boardId: string) => void;
  onEditNameChange: (name: string) => void;
  onSaveName: (option: BoardOption) => void | Promise<void>;
  onCancelEdit: () => void;
  onToggleActions: (boardId: string) => void;
  onOpenNewWindow: (boardId: string) => void;
  onRename: (option: BoardOption) => void;
  onDuplicate: (option: BoardOption) => void | Promise<void>;
  onDelete: (option: BoardOption) => void | Promise<void>;
}) {
  const boardSwitcherRef = useRef<HTMLDivElement>(null);
  const [actionsPosition, setActionsPosition] = useState<{ id: string; left: number; top: number } | null>(null);

  useEffect(() => {
    if (!open) return;
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (boardSwitcherRef.current?.contains(target)) return;
      if (target instanceof Element && target.closest("[data-board-switcher-popup]")) return;
      onToggle();
    };
    document.addEventListener("pointerdown", closeOnOutsidePointerDown);
    return () => document.removeEventListener("pointerdown", closeOnOutsidePointerDown);
  }, [open, onToggle]);

  const toggleActions = (event: React.MouseEvent<HTMLButtonElement>, optionId: string) => {
    event.stopPropagation();
    if (boardActionsId === optionId) {
      setActionsPosition(null);
      onToggleActions(optionId);
      return;
    }
    const rect = event.currentTarget.getBoundingClientRect();
    const menuWidth = 160;
    const menuHeight = 148;
    setActionsPosition({
      id: optionId,
      left: Math.min(rect.right + 6, window.innerWidth - menuWidth - 8),
      top: Math.min(rect.top, window.innerHeight - menuHeight - 8),
    });
    onToggleActions(optionId);
  };

  useEffect(() => {
    if (!boardActionsId) return;
    const closeOnViewportChange = () => setActionsPosition(null);
    window.addEventListener("resize", closeOnViewportChange);
    window.addEventListener("scroll", closeOnViewportChange, true);
    return () => {
      window.removeEventListener("resize", closeOnViewportChange);
      window.removeEventListener("scroll", closeOnViewportChange, true);
    };
  }, [boardActionsId]);

  const renderActionsMenu = (option: BoardOption) => {
    if (boardActionsId !== option.id || actionsPosition?.id !== option.id || typeof document === "undefined") return null;
    return createPortal(
      <div role="menu" data-board-switcher-popup className="fixed z-[1000] w-40 rounded-lg border border-[#dce3ec] bg-white p-1 shadow-[0_10px_24px_rgba(50,63,82,0.16)]" style={{ left: actionsPosition.left, top: actionsPosition.top }}>
        <button type="button" onClick={() => onOpenNewWindow(option.id)} className="block w-full rounded-md px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f5f7fa]">{labels.openNewWindow}</button>
        <button type="button" onClick={() => onRename(option)} className="block w-full rounded-md px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f5f7fa]">{labels.rename}</button>
        <button type="button" onClick={() => void onDuplicate(option)} className="block w-full rounded-md px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f5f7fa]">{labels.duplicate}</button>
        <button type="button" onClick={() => void onDelete(option)} className="block w-full rounded-md px-2 py-1.5 text-left text-[11px] text-[#c45a5a] hover:bg-[#fff5f5]">{labels.delete}</button>
      </div>,
      document.body,
    );
  };

  return <div ref={boardSwitcherRef} className="relative flex min-w-0 items-center">
    <button type="button" onClick={onToggle} className="focus-ring flex min-w-0 items-center gap-0.5 rounded-md px-0.5 py-1 text-left transition-colors hover:bg-[#f4f6fa]" aria-label={labels.title} aria-expanded={open} aria-haspopup="menu" title={labels.title}>
      <span className="max-w-36 truncate text-[12px] font-medium text-[#4b5563]">{name || labels.defaultName}</span>
      <ChevronDown className="h-3.5 w-3.5 shrink-0 text-[#8a96a7]" aria-hidden />
    </button>
    {open ? <div role="menu" data-board-switcher-popup aria-label={labels.title} className="absolute left-0 top-[calc(100%+6px)] z-50 w-60 rounded-xl border border-[#dce3ec] bg-white p-1.5 shadow-[0_12px_30px_rgba(50,63,82,0.18)]">
      <div className="flex items-center justify-between border-b border-[#edf0f4] px-2.5 py-2">
        <span className="text-[11px] font-semibold text-[#475569]">{labels.title}</span>
        <div className="group relative">
          <button type="button" onClick={onCreate} disabled={creating} className="focus-ring rounded-md p-1 text-[#64748b] hover:bg-[#f1efff] hover:text-[#6254d9] disabled:cursor-wait disabled:opacity-60" aria-label={labels.add} title={labels.add}>
            {creating ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> : <Plus className="h-4 w-4" aria-hidden />}
          </button>
          <span className="pointer-events-none absolute right-0 top-[calc(100%+6px)] z-50 whitespace-nowrap rounded bg-[#30343b] px-2 py-1 text-[10px] text-white opacity-0 shadow-sm transition-opacity group-hover:opacity-100">{labels.add}</span>
        </div>
      </div>
      <div className="max-h-64 overflow-y-auto py-1">
        {boardOptions.map((option) => {
          const selected = option.id === boardId;
          const editing = editingBoardId === option.id;
          return <div key={option.id} role="menuitem" tabIndex={editing ? -1 : 0} onClick={() => { if (!editing) onSwitch(option.id); }} onKeyDown={(event) => { if (!editing && (event.key === "Enter" || event.key === " ")) { event.preventDefault(); onSwitch(option.id); } }} className={"group relative flex cursor-pointer items-center rounded-lg px-2.5 py-2 transition-colors " + (selected ? "bg-[#f1efff] text-[#5145b6]" : "text-[#475569] hover:bg-[#f8fafc]")}>
            <div className="min-w-0 flex-1">
              {editing ? <input onClick={(event) => event.stopPropagation()} value={editingBoardName} onChange={(event) => onEditNameChange(event.target.value)} onBlur={() => void onSaveName(option)} onKeyDown={(event) => { event.stopPropagation(); if (event.key === "Enter") { event.preventDefault(); event.currentTarget.blur(); } if (event.key === "Escape") onCancelEdit(); }} className="w-full rounded border border-[#cfd6e2] bg-white px-1.5 py-0.5 text-[11px] text-[#475569] outline-none focus:border-[#8e84e8]" aria-label={labels.nameInput} /> : <span className="block w-full truncate text-left text-[11px]">{option.name}</span>}
            </div>
            {editing ? null : <div className={"ml-1 flex shrink-0 items-center gap-0.5 " + (selected ? "" : "opacity-0 transition-opacity group-hover:opacity-100")}>
              <span className="w-4 text-center">{selected ? <Check className="mx-auto h-3.5 w-3.5" aria-hidden /> : null}</span>
              {!selected ? <div className="relative">
                <button type="button" onClick={(event) => toggleActions(event, option.id)} className="focus-ring rounded p-0.5 text-[#64748b] hover:bg-[#e9edf4]" aria-label={labels.more} aria-expanded={boardActionsId === option.id} title={labels.more}><MoreHorizontal className="h-3.5 w-3.5" aria-hidden /></button>
                {renderActionsMenu(option)}
              </div> : null}
            </div>}
          </div>;
        })}
      </div>
    </div> : null}
  </div>;
}

export type CreativeBoardSnapshot = { boardId: string; name: string; viewport: { x: number; y: number; zoom: number }; items: BoardItem[]; edges: BoardEdge[]; revision: number };
type Media = { id: string; original_name: string; kind: string; url?: string; content_url?: string; preview_url?: string; thumbnail_url?: string; mime_type?: string };
type AssetSource = "project" | "global" | "media";
type AssetCategory = "personal" | "agent" | "global";
type AssetKind = "character" | "scene" | "prop" | "product" | "media" | "video";
type UnifiedAsset = {
  id: string;
  name: string;
  kind: AssetKind;
  source: AssetSource;
  resourceType: string;
  /** Full design/reference image used inside the canvas. */
  imagePath?: string;
  /** Compact avatar/thumbnail used by sidebar lists. */
  sidebarImagePath?: string;
  projectName?: string;
  previewVersion?: number | null;
  media?: Media;
};
type BoardItemType = "document" | "character" | "scene" | "prop" | "product" | "media" | "video" | "episode" | "shot" | "skill_action" | "review" | "final";

const ASSET_TYPES: Array<AssetKind | "all"> = ["all", "character", "scene", "prop", "product", "media", "video"];
const ASSET_CATEGORIES: AssetCategory[] = ["personal", "agent", "global"];

const LEFT_PANEL_DEFAULT_WIDTH = 282;
const LEFT_PANEL_MIN_WIDTH = 220;
const LEFT_PANEL_MAX_WIDTH = 460;
const LEFT_PANEL_WIDTH_STORAGE_KEY = "shotwise:creativeBoardLeftPanelWidth";

function loadLeftPanelWidth(): number {
  const saved = Number(window.localStorage.getItem(LEFT_PANEL_WIDTH_STORAGE_KEY));
  return Number.isFinite(saved) && saved >= LEFT_PANEL_MIN_WIDTH && saved <= LEFT_PANEL_MAX_WIDTH
    ? Math.round(saved)
    : LEFT_PANEL_DEFAULT_WIDTH;
}
type ElementFilter = "all" | "text" | "image" | "video" | "video_edit" | "director" | "frame_extract" | "audio" | "script";

const IMAGE_ITEM_TYPES = new Set(["character", "scene", "prop", "product", "media"]);

function isImageBoardItem(item: BoardItem) {
  return IMAGE_ITEM_TYPES.has(item.item_type);
}

const ELEMENT_FILTERS: Array<{ value: ElementFilter; labelKey: string; label: string }> = [
  { value: "all", labelKey: "creative_board_filter_all", label: "全部" },
  { value: "text", labelKey: "creative_board_filter_text", label: "文本" },
  { value: "image", labelKey: "creative_board_filter_image", label: "图片" },
  { value: "video", labelKey: "creative_board_filter_video", label: "视频" },
  { value: "video_edit", labelKey: "creative_board_filter_video_edit", label: "视频编辑" },
  { value: "director", labelKey: "creative_board_filter_director", label: "导演台" },
  { value: "frame_extract", labelKey: "creative_board_filter_frame_extract", label: "逐帧拉片" },
  { value: "audio", labelKey: "creative_board_filter_audio", label: "音频" },
  { value: "script", labelKey: "creative_board_filter_script", label: "脚本" },
];

function matchesElementFilter(item: BoardItem, filter: ElementFilter) {
  switch (filter) {
    case "text": return item.item_type === "document";
    case "image": return ["character", "scene", "prop", "product", "media"].includes(item.item_type);
    case "video": return item.item_type === "video" || item.item_type === "final";
    case "video_edit": return item.item_type === "video_edit";
    case "director": return item.item_type === "skill_action";
    case "frame_extract": return item.item_type === "review";
    case "audio": return item.item_type === "audio";
    case "script": return ["document", "episode", "shot"].includes(item.item_type);
    default: return true;
  }
}

function parseBoard(value: unknown): Board {
  if (!value || typeof value !== "object") return { ...EMPTY_SNAPSHOT, project_id: "", revision: 1 } as unknown as Board;
  const valueRecord = value as Record<string, unknown>;
  const nested = valueRecord.board;
  const record = nested && typeof nested === "object" ? nested as Record<string, unknown> : valueRecord;
  return {
    ...record,
    viewport: record.viewport && typeof record.viewport === "object" ? record.viewport : {},
    items: Array.isArray(record.items) ? record.items as BoardItem[] : [],
    edges: Array.isArray(record.edges) ? record.edges as BoardEdge[] : [],
    revision: typeof record.revision === "number" ? record.revision : 1,
  } as Board;
}

function cloneBoard(board: Board): Board {
  return JSON.parse(JSON.stringify(board)) as Board;
}

function isCreativeBoardNotFoundError(error: unknown): boolean {
  const status = (error as { status?: unknown } | null)?.status;
  if (status === 404) return true;
  return error instanceof Error && error.message.includes("creative_board_not_found");
}

// 持久化 key 不含 revision：保存成功只是版本号前进，不构成一次编辑
function boardPersistenceKey(snapshot: Pick<CreativeBoardSnapshot, "boardId" | "name" | "viewport" | "items" | "edges">): string {
  const { boardId, name, viewport, items, edges } = snapshot;
  return JSON.stringify({ boardId, name, viewport, items, edges });
}

function isEditableTarget(target: EventTarget | null) {
  return target instanceof HTMLElement && Boolean(target.closest("input, textarea, select, [contenteditable='true']"));
}

const SHORTCUT_GROUPS = [
  {
    titleKey: "creative_board_shortcut_creation",
    titleDefault: "Creation",
    rows: [
{ labelKey: "creative_board_shortcut_group", labelDefault: "Group", keys: ["Ctrl", "Alt", "+", "G"] },
      { labelKey: "creative_board_shortcut_merge_group", labelDefault: "Merge groups", keys: ["Ctrl", "Alt", "+", "G"] },
      { labelKey: "creative_board_shortcut_ungroup", labelDefault: "Ungroup", keys: ["Ctrl", "Alt", "Shift", "+", "G"] },
      { labelKey: "creative_board_connect", labelDefault: "Connect", keys: ["Ctrl", "L"] },
      { labelKey: "creative_board_shortcut_duplicate_node_edges", labelDefault: "Duplicate node and edges", keys: ["Ctrl", "D"] },
      { labelKey: "creative_board_shortcut_generate", labelDefault: "Generate", keys: ["Ctrl", "Enter"] },
      { labelKey: "creative_board_shortcut_new_node", labelDefault: "New node", keys: ["Tab"] },
      { labelKey: "creative_board_shortcut_duplicate_node", labelDefault: "Duplicate node", keys: ["Alt", "+", "drag"] },
      { labelKey: "creative_board_shortcut_create_copy", labelDefault: "Create copy", keys: ["Ctrl", "Alt", "+", "drag"] },
    ],
  },
  {
    titleKey: "creative_board_shortcut_zoom",
    titleDefault: "Zoom",
    rows: [
      { labelKey: "creative_board_zoom_in", labelDefault: "Zoom in", keys: ["Ctrl", "+"] },
      { labelKey: "creative_board_zoom_out", labelDefault: "Zoom out", keys: ["Ctrl", "−"] },
      { labelKey: "creative_board_fit", labelDefault: "Fit canvas", keys: ["Ctrl", "0"] },
      { labelKey: "creative_board_shortcut_touchpad", labelDefault: "Trackpad", keys: ["⌘", "pinch"] },
      { labelKey: "creative_board_shortcut_mouse", labelDefault: "Mouse", keys: ["Ctrl", "wheel"] },
    ],
  },
  {
    titleKey: "creative_board_shortcut_pan",
    titleDefault: "Move canvas",
    rows: [
      { labelKey: "creative_board_shortcut_keyboard", labelDefault: "Keyboard", keys: ["Space", "＋", "🖱"] },
      { labelKey: "creative_board_shortcut_touchpad", labelDefault: "Trackpad", keys: ["two-finger drag"] },
      { labelKey: "creative_board_shortcut_mouse", labelDefault: "Mouse", keys: ["middle button"] },
      { labelKey: "creative_board_move_canvas", labelDefault: "Move", keys: ["V"] },
      { labelKey: "creative_board_shortcut_pan_tool", labelDefault: "Hand tool", keys: ["H"] },
      { labelKey: "creative_board_shortcut_arrange", labelDefault: "Arrange canvas", keys: ["Alt", "Shift", "F"] },
    ],
  },
  {
    titleKey: "creative_board_shortcut_other",
    titleDefault: "Other",
    rows: [
      { labelKey: "creative_board_undo", labelDefault: "Undo", keys: ["Ctrl", "Z"] },
      { labelKey: "creative_board_redo", labelDefault: "Redo", keys: ["Ctrl", "Shift", "Z"] },
      { labelKey: "creative_board_delete_selected", labelDefault: "Delete", keys: ["⌫"] },
    ],
  },
] as const;

function canvasAssetToUnified(asset: CanvasAsset, fingerprints: Record<string, number>): UnifiedAsset {
  const kind: AssetKind = asset.kind === "asset" ? "media" : asset.kind === "media" && asset.mimeType?.startsWith("video/") ? "video" : asset.kind;
  const isMedia = kind === "media" || kind === "video";
  return {
    id: asset.reference.id,
    name: asset.name,
    kind,
    source: asset.source === "global" ? "global" : isMedia ? "media" : "project",
    resourceType: isMedia ? "media_asset" : kind,
    imagePath: (asset.canvasPreviewUrl ?? asset.previewUrl) ?? undefined,
    sidebarImagePath: (asset.sidebarPreviewUrl ?? asset.previewUrl) ?? undefined,
    projectName: asset.projectName,
    previewVersion: asset.canvasPreviewUrl ? (fingerprints[asset.canvasPreviewUrl] ?? null) : null,
    media: isMedia ? {
      id: asset.reference.id,
      original_name: asset.name,
      kind: kind === "video" ? "video" : "image",
      preview_url: (asset.canvasPreviewUrl ?? asset.previewUrl) ?? undefined,
      mime_type: asset.mimeType,
    } : undefined,
  };
}

function assetKey(resourceType: string, id: string) { return resourceType + ":" + id; }

function asCanvasAssetLoaderResult(value: unknown): object | null | undefined {
  return typeof value === "object" && value !== null ? value : undefined;
}

async function loadProjectAssetKind(projectName: string, kind: Exclude<AssetKind, "media" | "video">): Promise<object | null | undefined> {
  const api = API as unknown as Record<string, unknown>;
  const singular = kind.charAt(0).toUpperCase() + kind.slice(1);
  const plural = kind === "prop" ? "Props" : singular + "s";
  const methodNames = [
    "listProject" + plural,
    "list" + plural,
    "getProject" + plural,
    "get" + plural,
  ];
  for (const methodName of methodNames) {
    const method = api[methodName];
    if (typeof method === "function") return asCanvasAssetLoaderResult(await (method as (project: string) => Promise<unknown>).call(API, projectName));
  }
  const getProject = api["getProject"];
  if (typeof getProject === "function") {
    const response = await (getProject as (project: string) => Promise<unknown>).call(API, projectName);
    if (response && typeof response === "object") {
      const root = response as Record<string, unknown>;
      const project = root.project && typeof root.project === "object" ? root.project as Record<string, unknown> : root;
      const values = project[kind + "s"] ?? project[kind] ?? project[kind + "_assets"];
      if (values !== undefined) return asCanvasAssetLoaderResult(values);
    }
  }
  return [];
}

function projectAssetBucket(project: unknown, kind: string): object {
  if (!project || typeof project !== "object") return [];
  const record = project as Record<string, unknown>;
  const values = record[kind + "s"] ?? record[kind] ?? record[kind + "_assets"];
  return typeof values === "object" && values !== null ? values : [];
}

async function loadGlobalAssets(): Promise<object> {
  const loader = (API as unknown as Record<string, unknown>).listAssets;
  if (typeof loader !== "function") return [];
  return (await (loader as (params: Record<string, unknown>) => Promise<unknown>).call(API, {})) as object;
}

function iconFor(type: string) {
  if (type === "video" || type === "final") return Video;
  if (type === "skill_action") return Sparkles;
  if (type === "document" || type === "episode" || type === "shot") return FileText;
  if (type === "character" || type === "scene" || type === "prop" || type === "product") return Layers3;
  return ImageIcon;
}

function colorFor(type: string) {
  if (type === "video" || type === "final") return "#806cff";
  if (type === "skill_action") return "#df9c42";
  if (type === "document" || type === "episode" || type === "shot") return "#5b96c8";
  return "#4f9f86";
}

function zoomClamp(value: number) { return Math.min(2.4, Math.max(0.35, Number(value.toFixed(2)))); }

function getWorldBounds(items: BoardItem[]) {
  const padding = 1000;
  const minX = Math.min(-padding, ...items.map((item) => item.position.x - padding));
  const minY = Math.min(-padding, ...items.map((item) => item.position.y - padding));
  const maxX = Math.max(padding, ...items.map((item) => item.position.x + item.size.width + padding));
  const maxY = Math.max(padding, ...items.map((item) => item.position.y + item.size.height + padding));
  return { minX, minY, width: maxX - minX, height: maxY - minY };
}

function nodeTitle(item: BoardItem, names: Map<string, string>) {
  const customName = item.display_settings?.name;
  return typeof customName === "string" && customName.trim()
    ? customName.trim()
    : names.get(assetKey(item.resource_type, item.resource_id)) || names.get(item.resource_id) || item.resource_id;
}

function mediaSource(media?: Media) { return media?.preview_url || media?.thumbnail_url || media?.content_url || media?.url; }

function assetSourceUrl(asset?: UnifiedAsset) {
  const mediaUrl = mediaSource(asset?.media);
  if (mediaUrl) return mediaUrl;
  return assetImageSourceUrl(asset, asset?.imagePath);
}

function sidebarAssetSourceUrl(asset?: UnifiedAsset) {
  const mediaUrl = mediaSource(asset?.media);
  if (mediaUrl) return mediaUrl;
  return assetImageSourceUrl(asset, asset?.sidebarImagePath);
}

function assetImageSourceUrl(asset: UnifiedAsset | undefined, imagePath: string | undefined) {
  if (!imagePath) return undefined;
  if (/^(data:|https?:|blob:|\/api\/)/i.test(imagePath)) return imagePath;
  if (asset?.source === "global") return API.getGlobalAssetUrl(imagePath) ?? imagePath;
  if (asset?.source === "project" && asset.projectName) {
    return API.getFileUrl(asset.projectName, imagePath, asset.previewVersion);
  }
  return imagePath;
}

function assetIcon(type: AssetKind) {
  if (type === "character") return UserRound;
  if (type === "scene") return Landmark;
  if (type === "prop" || type === "product") return Package;
  return type === "video" ? Video : ImageIcon;
}

const IMAGE_EDIT_TOOL_ACTIONS = new Set<BoardToolbarAction>([
  "edit",
  "portrait",
  "portrait-emotion",
  "lighting",
  "adjust",
  "symmetry",
]);

/** 高清下拉的六个操作：走独立画布编辑器（区域框选 + 每操作配置面板），即图标工具。 */
const CANVAS_EDITOR_OPERATIONS = new Set<BoardToolbarAction>(["hd", "outpaint", "redraw", "erase", "cutout", "crop"]);

const CANVAS_EDITOR_OPERATION_MAP: Record<CanvasEditorOperation, CanvasAdvancedOperation> = {
  hd: "canvas_image_hd",
  outpaint: "canvas_image_outpaint",
  redraw: "canvas_image_redraw",
  erase: "canvas_image_erase",
  cutout: "canvas_image_cutout",
  crop: "canvas_image_crop",
};

function imageEditInstruction(action: BoardToolbarAction, preset: string | undefined, instruction: string) {
  const normalized = instruction.trim();
  if (action === "edit") return normalized;
  const operation = preset ? `${action}:${preset}` : action;
  return `[${operation}] ${normalized}`;
}

function previewFor(item: BoardItem, asset: UnifiedAsset | undefined) {
  const source = assetSourceUrl(asset);
  const isVideo = item.item_type === "video" || asset?.kind === "video" || asset?.media?.mime_type?.startsWith("video/");
  if (source && isVideo) return <video src={source} muted playsInline preload="metadata" className="pointer-events-none h-full w-full object-cover" aria-label={asset?.name || item.resource_id} />;
  if (source) return <img src={source} alt={asset?.name || item.resource_id} draggable={false} onDragStart={(event) => event.preventDefault()} className="pointer-events-none h-full w-full object-cover" />;
  if (!asset) return <div className="flex h-full flex-col items-center justify-center gap-2 bg-[#fff8f8] px-3 text-center text-[#b35a5a]"><CircleHelp className="h-7 w-7" aria-hidden /><span className="text-[10px]">Unavailable reference</span><span className="max-w-full truncate text-[9px] text-[#c98585]">{item.resource_id}</span></div>;
  const Icon = assetIcon(asset.kind);
  return <div className="flex h-full flex-col items-center justify-center gap-2 bg-[linear-gradient(145deg,#f8f7ff,#eef3f8)] px-3 text-center text-[#667085]"><span className="flex h-10 w-10 items-center justify-center rounded-xl bg-white text-[#6254d9] shadow-sm"><Icon className="h-5 w-5" aria-hidden /></span><span className="max-w-full truncate text-[11px] font-semibold text-[#475569]">{asset.name}</span><span className="text-[9px] uppercase tracking-wide text-[#94a3b8]">{asset.kind}</span></div>;
}

function edgePath(source: BoardItem, target: BoardItem) {
  const leftToRight = source.position.x <= target.position.x;
  const startX = leftToRight ? source.position.x + source.size.width : source.position.x;
  const endX = leftToRight ? target.position.x : target.position.x + target.size.width;
  const startY = source.position.y + source.size.height / 2;
  const endY = target.position.y + target.size.height / 2;
  const bend = Math.max(80, Math.abs(endX - startX) * 0.42) * (leftToRight ? 1 : -1);
  return "M " + startX + " " + startY + " C " + (startX + bend) + " " + startY + ", " + (endX - bend) + " " + endY + ", " + endX + " " + endY;
}

function skillQuery(item: BoardItem | undefined, episode: number | undefined) {
  const params = new URLSearchParams();
  if (item?.resource_id) params.set("resource_id", item.resource_id);
  if (item?.resource_type) params.set("resource_type", item.resource_type);
  if (episode) params.set("episode", String(episode));
  return params.toString();
}

let clientIdSequence = 0;
function clientId(prefix: string) {
  clientIdSequence += 1;
  return "client-" + prefix + "-" + Date.now().toString(36) + "-" + clientIdSequence.toString(36);
}

const EMPTY_SNAPSHOT: CreativeBoardSnapshot = {
  boardId: "",
  name: "",
  viewport: { x: 28, y: 28, zoom: 1 },
  items: [],
  edges: [],
  revision: 1,
};

function boardItemPayload(item: BoardItem) {
  return {
    item_type: item.item_type,
    resource_type: item.resource_type,
    resource_id: item.resource_id,
    position: item.position,
    size: item.size,
    ...(item.group_id === undefined ? {} : { group_id: item.group_id }),
    display_settings: item.display_settings ?? {},
  };
}

function sameBoardItem(left: BoardItem, right: BoardItem) {
  return JSON.stringify(boardItemPayload(left)) === JSON.stringify(boardItemPayload(right));
}

function sameViewport(left: CreativeBoardSnapshot["viewport"], right: CreativeBoardSnapshot["viewport"]) {
  return left.x === right.x && left.y === right.y && left.zoom === right.zoom;
}

type PendingCanvasSplit = {
  sourceItemId: string;
  rows: number;
  cols: number;
  includeSplitLines: boolean;
};

type PendingCanvasAdvanced = {
  sourceItemId: string;
  operation: CanvasAdvancedOperation;
};

type CanvasSplitCell = {
  row: number;
  col: number;
  index: number;
  width?: number;
  height?: number;
  media_asset_id?: string;
  media_asset?: { id?: string };
};

type CanvasOutput = {
  index?: number;
  label?: string;
  width?: number;
  height?: number;
  media_asset_id?: string;
  media_asset?: { id?: string };
};

function readCanvasSplitCells(result: Record<string, unknown> | null): CanvasSplitCell[] {
  if (!result || !Array.isArray(result.cells)) return [];
  return result.cells.filter((cell): cell is CanvasSplitCell => {
    if (!cell || typeof cell !== "object") return false;
    const record = cell as Record<string, unknown>;
    return Number.isInteger(record.row) && Number.isInteger(record.col) && Number.isInteger(record.index);
  });
}

function readCanvasOutputs(result: Record<string, unknown> | null): CanvasOutput[] {
  if (!result || !Array.isArray(result.outputs)) return [];
  return result.outputs.filter((output): output is CanvasOutput => {
    if (!output || typeof output !== "object") return false;
    const record = output as Record<string, unknown>;
    return typeof record.media_asset_id === "string" || Boolean(record.media_asset && typeof record.media_asset === "object");
  });
}

function canvasOutputCardSize(source: BoardItem, output: CanvasOutput) {
  const sourceRatio = source.size.width / Math.max(1, source.size.height);
  const outputRatio = output.width && output.height && output.width > 0 && output.height > 0
    ? output.width / output.height
    : sourceRatio;
  const width = Math.max(160, Math.round(source.size.width * 0.9), Math.round(120 * outputRatio));
  return { width, height: Math.max(120, Math.round(width / outputRatio)) };
}

export function CreativeBoardWorkspace({ projectName }: { projectName: string }) {
  const { t } = useTranslation("dashboard");
  const [location, navigate] = useLocation();
  const routeSearch = useSearch();
  const projectData = useProjectsStore((state) =>
    state.currentProjectName === projectName ? state.currentProjectData : null,
  );
  const assetFingerprints = useProjectsStore((state) => state.assetFingerprints);
  const canvasRef = useRef<HTMLDivElement>(null);
  const viewportRef = useRef({ x: 28, y: 28, zoom: 1 });
  const boardRevisionsRef = useRef(new Map<string, number>());
  const currentBoardIdRef = useRef<string | null>(null);
  const dragRef = useRef<BoardDragState | null>(null);
  const resizeRef = useRef<ResizeState | null>(null);
  const panRef = useRef<{ startX: number; startY: number; originX: number; originY: number } | null>(null);
  const dragHistoryRecordedRef = useRef(false);
  const suppressNextNodeClickRef = useRef(false);
  const pendingNodePointerSelectionRef = useRef<string | null>(null);
  const [draggingItemIds, setDraggingItemIds] = useState<string[]>([]);
  const boardHistoryRef = useRef<BoardHistory>({ past: [], future: [] });
  const spacePanRef = useRef(false);
  const toolBeforeSpaceRef = useRef<"select" | "pan">("select");
  const mountedRef = useRef(true);
  const boardLoadGenerationRef = useRef(0);
  const pendingBoardEditRef = useRef<string | null>(null);
  const serverItemIdsRef = useRef(new Map<string, string>());
  const serverEdgeIdsRef = useRef(new Map<string, string>());
  const pendingCanvasSplitsRef = useRef(new Map<string, PendingCanvasSplit>());
  const pendingCanvasAdvancedRef = useRef(new Map<string, PendingCanvasAdvanced>());
  const consumedCanvasSplitsRef = useRef(new Set<string>());
  const consumedCanvasAdvancedRef = useRef(new Set<string>());
  const tasks = useTasksStore((state) => state.tasks);
  const [board, setBoard] = useState<Board | null>(null);
  const [boardOptions, setBoardOptions] = useState<BoardOption[]>([]);
  const [catalog, setCatalog] = useState<UnifiedAsset[]>([]);
  const [name, setName] = useState("");
  const [viewport, setViewportState] = useState({ x: 28, y: 28, zoom: 1 });
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [leftTab, setLeftTab] = useState<"canvas" | "assets">("canvas");
  const [_itemType, setItemType] = useState<BoardItemType>("media");
  const [_resourceId, setResourceId] = useState("");
  const [search, setSearch] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const [elementActionsId, setElementActionsId] = useState<string | null>(null);
  const [editingElementId, setEditingElementId] = useState<string | null>(null);
  const [editingElementName, setEditingElementName] = useState("");
  const elementNameInputRef = useRef<HTMLInputElement>(null);
  const [elementFilter, setElementFilter] = useState<ElementFilter>("all");
  const [elementFilterOpen, setElementFilterOpen] = useState(false);
  useEffect(() => {
    if (!editingElementId) return;
    elementNameInputRef.current?.focus();
    elementNameInputRef.current?.select();
  }, [editingElementId]);

  useEffect(() => {
    if (!elementActionsId) return;
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (target instanceof Element && target.closest(`[data-creative-board-element-actions="${elementActionsId}"]`)) return;
      setElementActionsId(null);
    };
    document.addEventListener("pointerdown", closeOnOutsidePointerDown);
    return () => document.removeEventListener("pointerdown", closeOnOutsidePointerDown);
  }, [elementActionsId]);

  const activeElementFilter = ELEMENT_FILTERS.find((filter) => filter.value === elementFilter) ?? ELEMENT_FILTERS[0];
  const [assetCategory, setAssetCategory] = useState<AssetCategory>("personal");
  const [assetTypeFilter, setAssetTypeFilter] = useState<AssetKind | "all">("all");
  const [leftOpen, setLeftOpen] = useState(true);
  const [leftPanelWidth, setLeftPanelWidth] = useState(loadLeftPanelWidth);
  const [leftPanelDragging, setLeftPanelDragging] = useState(false);
  const leftPanelDragRef = useRef<{ startX: number; startWidth: number } | null>(null);
  const leftPanelBodyStyleRef = useRef<{ cursor: string; userSelect: string } | null>(null);

  const restoreLeftPanelBodyStyle = useCallback(() => {
    const saved = leftPanelBodyStyleRef.current;
    if (saved) {
      document.body.style.cursor = saved.cursor;
      document.body.style.userSelect = saved.userSelect;
      leftPanelBodyStyleRef.current = null;
    }
  }, []);

  const handleLeftPanelResizeStart = useCallback(
    (event: React.MouseEvent<HTMLDivElement>) => {
      // 仅响应主键，避免右键/中键意外进入拖拽态
      if (event.button !== 0) return;
      event.preventDefault();
      leftPanelDragRef.current = { startX: event.clientX, startWidth: leftPanelWidth };
      leftPanelBodyStyleRef.current = {
        cursor: document.body.style.cursor,
        userSelect: document.body.style.userSelect,
      };
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";
      setLeftPanelDragging(true);
    },
    [leftPanelWidth],
  );

  useEffect(() => {
    if (!leftPanelDragging) return;

    const finishLeftPanelResize = () => {
      leftPanelDragRef.current = null;
      restoreLeftPanelBodyStyle();
      setLeftPanelDragging(false);
    };

    const onMouseMove = (event: MouseEvent) => {
      const drag = leftPanelDragRef.current;
      if (!drag) return;
      // 主键已在中途松开（如焦点切走时）→ 主动收尾
      if ((event.buttons & 1) === 0) {
        finishLeftPanelResize();
        return;
      }
      // 手柄在面板右缘，鼠标向右 (clientX 增大) → 宽度增大
      const next = drag.startWidth + (event.clientX - drag.startX);
      if (next < LEFT_PANEL_MIN_WIDTH) {
        // 缩到阈值以下 → 折叠；宽度不清除，重开回到上次宽度
        finishLeftPanelResize();
        setLeftOpen(false);
        return;
      }
      setLeftPanelWidth(Math.min(next, LEFT_PANEL_MAX_WIDTH));
    };

    window.addEventListener("mousemove", onMouseMove);
    window.addEventListener("mouseup", finishLeftPanelResize);
    // 鼠标在窗外松开时 mouseup 可能不触发，blur 兜底防止卡死
    window.addEventListener("blur", finishLeftPanelResize);
    return () => {
      window.removeEventListener("mousemove", onMouseMove);
      window.removeEventListener("mouseup", finishLeftPanelResize);
      window.removeEventListener("blur", finishLeftPanelResize);
      // 组件意外卸载时兜底清理 body 样式
      restoreLeftPanelBodyStyle();
    };
  }, [leftPanelDragging, restoreLeftPanelBodyStyle]);

  // 非拖拽期的面板宽度变化（含初次加载的回写）持久化；折叠态不覆盖，重开回到上次宽度
  useEffect(() => {
    if (leftPanelDragging || !leftOpen) return;
    window.localStorage.setItem(LEFT_PANEL_WIDTH_STORAGE_KEY, String(leftPanelWidth));
  }, [leftPanelDragging, leftOpen, leftPanelWidth]);
  const [gridVisible, setGridVisible] = useState(true);
  const [addMenuOpen, setAddMenuOpen] = useState(false);
  const [toolMenuOpen, setToolMenuOpen] = useState(false);
  const [minimapOpen, setMinimapOpen] = useState(false);
  const [zoomMenuOpen, setZoomMenuOpen] = useState(false);
  const [libraryOpen, setLibraryOpen] = useState(false);
  const [roleLibraryOpen, setRoleLibraryOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [shortcutsOpen, setShortcutsOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [boardMenuOpen, setBoardMenuOpen] = useState(false);
  const [creatingBoard, setCreatingBoard] = useState(false);
  const [editingBoardId, setEditingBoardId] = useState<string | null>(null);
  const [editingBoardName, setEditingBoardName] = useState("");
  const [boardActionsId, setBoardActionsId] = useState<string | null>(null);
  const [connectMode, setConnectMode] = useState(false);
  const [activeTool, setActiveTool] = useState<"select" | "pan">("select");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [revision, setRevision] = useState(1);
  const [conflict, setConflict] = useState(false);
  const [conflictOpen, setConflictOpen] = useState(false);
  const [conflictRevision, setConflictRevision] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [toolAction, setToolAction] = useState<BoardToolbarAction | null>(null);
  const [toolPreset, setToolPreset] = useState<string | undefined>(undefined);
  const [toolInstruction, setToolInstruction] = useState("");
  const [toolReferenceMediaAssetId, setToolReferenceMediaAssetId] = useState("");
  const [toolGridRows, setToolGridRows] = useState(3);
  const [toolGridCols, setToolGridCols] = useState(3);
  const [toolIncludeSplitLines, setToolIncludeSplitLines] = useState(true);
  const [toolAdjustmentOpen, setToolAdjustmentOpen] = useState(true);
  const [toolBusy, setToolBusy] = useState(false);
  const [toolEditorOperation, setToolEditorOperation] = useState<CanvasEditorOperation | null>(null);
  const [toolEditorBusy, setToolEditorBusy] = useState(false);
  const [canvasSplitGeneration, setCanvasSplitGeneration] = useState(0);
  const currentEpisode = useMemo(() => {
    if (typeof window === "undefined") return undefined;
    const value = Number(new URLSearchParams(window.location.search).get("episode"));
    return Number.isInteger(value) && value > 0 ? value : undefined;
  }, []);
  const currentBoardId = useMemo(() => new URLSearchParams(routeSearch).get("board") ?? undefined, [routeSearch]);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const fetchCatalog = useCallback(async (): Promise<{ assets: UnifiedAsset[]; error: string | null }> => {
    // projectData 是工作台进入时的快照；角色设计图可能由 Agent 在另一个面板
    // 中生成，不能只依赖这份快照。每次刷新目录都重新读取项目，确保拿到最新
    // 的 character_sheet / scene_sheet / prop_sheet。
    let latestProjectData = projectData;
    let latestFingerprints = assetFingerprints;
    if (projectName) {
      try {
        const latest = await API.getProject(projectName);
        latestProjectData = latest.project;
        latestFingerprints = latest.asset_fingerprints ?? assetFingerprints;
      } catch {
        // 已有项目快照仍可用于渲染；目录失败时由 loadCanvasAssets 返回可用数据。
      }
    }
    const loadProjectBucket = async (
      name: string,
      kind: Exclude<AssetKind, "media" | "video">,
    ): Promise<object | null | undefined> => {
      if (latestProjectData) return projectAssetBucket(latestProjectData, kind);
      return loadProjectAssetKind(name, kind);
    };
    const result = await loadCanvasAssets({
      characters: (name) => loadProjectBucket(name, "character"),
      scenes: (name) => loadProjectBucket(name, "scene"),
      props: (name) => loadProjectBucket(name, "prop"),
      products: (name) => loadProjectBucket(name, "product"),
      media: (name) => API.listMediaAssets(name),
      globalAssets: () => loadGlobalAssets(),
    }, projectName);
    return {
      assets: result.assets.map((asset) => canvasAssetToUnified(asset, latestFingerprints)),
      error: result.errors.length > 0 ? result.errors.map((item) => item.message).join("; ") : null,
    };
  }, [assetFingerprints, projectData, projectName]);

  const loadCatalog = useCallback(async (): Promise<UnifiedAsset[]> => {
    const { assets, error: catalogError } = await fetchCatalog();
    if (!mountedRef.current) return assets;
    setCatalog(assets);
    if (catalogError) setError(catalogError);
    return assets;
  }, [fetchCatalog, setError]);

  const load = useCallback(async (): Promise<CreativeBoardSnapshot | undefined> => {
    const loadGeneration = boardLoadGenerationRef.current + 1;
    boardLoadGenerationRef.current = loadGeneration;
    const isCurrentLoad = () => mountedRef.current && boardLoadGenerationRef.current === loadGeneration;
    setLoading(true);
    setError(null);
    setConflict(false);
    try {
      const boards = await API.listCreativeBoards(projectName);
      if (!isCurrentLoad()) return undefined;
      const listedBoardOptions = boards.items
        .filter((item) => typeof item.id === "string")
        .map((item) => ({ id: String(item.id), name: item.name || t("creative_board_default_name") }));
      const existing = boards.items.find((item) => typeof item.id === "string");
      let requestedBoard: Board | undefined;
      if (currentBoardId) {
        try {
          requestedBoard = parseBoard(await API.getCreativeBoard(currentBoardId));
        } catch (reason) {
          // URL 的 board 参数可能指向已删除的画布：404 时降级取列表首个/自动建板，而不是卡死整个工作台
          if (!isCreativeBoardNotFoundError(reason)) throw reason;
        }
      }
      const nextBoard = requestedBoard
        ?? (existing
          ? parseBoard(await API.getCreativeBoard(String(existing.id)))
          : parseBoard(await API.createCreativeBoard(projectName, { name: t("creative_board_default_name"), viewport: { x: 28, y: 28, zoom: 1 } })));
      if (!isCurrentLoad()) return undefined;
      serverItemIdsRef.current.clear();
      serverEdgeIdsRef.current.clear();
      for (const item of nextBoard.items) serverItemIdsRef.current.set(item.id, item.id);
      for (const edge of nextBoard.edges) serverEdgeIdsRef.current.set(edge.id, edge.id);
      boardHistoryRef.current = { past: [], future: [] };
      const nextBoardOptions = listedBoardOptions.some((item) => item.id === nextBoard.id)
        ? listedBoardOptions
        : [...listedBoardOptions, { id: nextBoard.id, name: nextBoard.name }];
      setBoardOptions(nextBoardOptions);
      setBoard(nextBoard);
      setName(nextBoard.name);
      setSelectedIds([]);
      const shouldEditNewBoard = pendingBoardEditRef.current === nextBoard.id;
      if (shouldEditNewBoard) pendingBoardEditRef.current = null;
      setBoardMenuOpen(shouldEditNewBoard);
      setEditingBoardId(shouldEditNewBoard ? nextBoard.id : null);
      setEditingBoardName(shouldEditNewBoard ? nextBoard.name : "");
      setBoardActionsId(null);
      const nextViewport = { x: nextBoard.viewport.x ?? 28, y: nextBoard.viewport.y ?? 28, zoom: zoomClamp(nextBoard.viewport.zoom ?? 1) };
      boardRevisionsRef.current.set(nextBoard.id, nextBoard.revision);
      currentBoardIdRef.current = nextBoard.id;
      setRevision(nextBoard.revision);
      viewportRef.current = nextViewport;
      setViewportState(nextViewport);
      return { boardId: nextBoard.id, name: nextBoard.name, viewport: nextViewport, items: nextBoard.items, edges: nextBoard.edges, revision: nextBoard.revision };
    } catch (reason) {
      if (isCurrentLoad()) setError(reason instanceof Error ? reason.message : t("creative_board_load_error"));
    } finally {
      if (isCurrentLoad()) setLoading(false);
    }
    return undefined;
  }, [currentBoardId, projectName, setError, t]);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- initialize the board from the project when the workspace mounts
    void load();
  }, [load]);

  useEffect(() => {
    // AI 生成角色设计图后，项目数据/文件指纹会更新；只刷新素材目录，
    // 不重新加载画布，避免清空用户当前选择和视口。
    // eslint-disable-next-line react-hooks/set-state-in-effect -- synchronize the catalog with project asset changes
    void loadCatalog();
  }, [loadCatalog]);

  const names = useMemo(() => {
    const result = new Map<string, string>();
    for (const asset of catalog) {
      result.set(assetKey(asset.resourceType, asset.id), asset.name);
      result.set(asset.id, asset.name);
    }
    return result;
  }, [catalog]);
  const assetsByReference = useMemo(() => {
    const result = new Map<string, UnifiedAsset>();
    for (const asset of catalog) {
      result.set(assetKey(asset.resourceType, asset.id), asset);
      if (!result.has(asset.id) || asset.source === "project") result.set(asset.id, asset);
    }
    return result;
  }, [catalog]);
  const selectedItems = useMemo(() => (board?.items ?? []).filter((item) => selectedIds.includes(item.id)), [board?.items, selectedIds]);
  const selectedItem = selectedItems.length === 1 ? selectedItems[0] : undefined;
  const selectedAsset = selectedItem ? assetsByReference.get(assetKey(selectedItem.resource_type, selectedItem.resource_id)) || assetsByReference.get(selectedItem.resource_id) : undefined;
  const referenceAssets = useMemo<ReferenceAsset[]>(() => catalog
    .filter((asset) => asset.source === "media" && asset.kind === "media")
    .map((asset) => ({ id: asset.id, name: asset.name, previewUrl: assetSourceUrl(asset), mimeType: asset.media?.mime_type })), [catalog]);
  const filteredItems = useMemo(() => { const value = search.trim().toLowerCase(); return (board?.items ?? []).filter((item) => matchesElementFilter(item, elementFilter) && (!value || nodeTitle(item, names).toLowerCase().includes(value) || item.item_type.includes(value) || item.resource_id.toLowerCase().includes(value))); }, [board?.items, elementFilter, names, search]);
  const filteredAssets = useMemo(() => {
    const value = search.trim().toLowerCase();
    return catalog.filter((asset) => {
      const matchesCategory = assetCategory === "personal"
        ? asset.source === "media"
        : assetCategory === "agent"
          ? asset.source === "project" && ["character", "scene", "prop"].includes(asset.kind)
          : asset.source === "global";
      const matchesType = assetTypeFilter === "all" || asset.kind === assetTypeFilter;
      return matchesCategory && matchesType && (!value || asset.name.toLowerCase().includes(value) || asset.kind.includes(value) || asset.source.includes(value) || asset.id.toLowerCase().includes(value));
    });
  }, [assetCategory, assetTypeFilter, catalog, search]);
  const activeAssetCategoryLabel = assetCategory === "personal"
    ? t("creative_board_asset_category_personal", { defaultValue: "Personal" })
    : assetCategory === "agent"
      ? t("creative_board_asset_category_agent", { defaultValue: "Agent" })
      : t("creative_board_asset_category_global", { defaultValue: "Global" });
  const gridSize = 28 * viewport.zoom;
  const worldBounds = useMemo(() => getWorldBounds(board?.items ?? []), [board?.items]);

  const setViewport = (next: { x: number; y: number; zoom: number }, _immediate = false) => {
    const normalized = { x: Math.round(next.x), y: Math.round(next.y), zoom: zoomClamp(next.zoom) };
    viewportRef.current = normalized;
    setViewportState(normalized);
  };

  const rememberBoard = () => {
    if (!board) return;
    boardHistoryRef.current.past.push(cloneBoard(board));
    boardHistoryRef.current.future = [];
  };

  useEffect(() => {
    if (!board || tasks.length === 0) return;
    const completed = tasks.filter((task) => task.project_name === projectName && task.status === "succeeded");
    for (const task of completed) {
      const pendingSplit = task.task_type === "canvas_image_split" ? pendingCanvasSplitsRef.current.get(task.task_id) : undefined;
      const pendingAdvanced = task.task_type !== "canvas_image_split" ? pendingCanvasAdvancedRef.current.get(task.task_id) : undefined;
      if ((!pendingSplit || consumedCanvasSplitsRef.current.has(task.task_id)) && (!pendingAdvanced || consumedCanvasAdvancedRef.current.has(task.task_id))) continue;
      const sourceItemId = pendingSplit?.sourceItemId ?? pendingAdvanced?.sourceItemId;
      const source = board.items.find((item) => item.id === sourceItemId);
      if (!source) continue;
      const cells = pendingSplit ? readCanvasSplitCells(task.result) : [];
      const outputs = pendingAdvanced ? readCanvasOutputs(task.result) : [];
      if (cells.length === 0 && outputs.length === 0) continue;
      const operation = pendingSplit ? "canvas_image_split" : pendingAdvanced?.operation ?? task.task_type;
      const groupId = (cells.length + outputs.length) > 1 ? clientId("canvas-output-group") : undefined;
      const createdItems: BoardItem[] = pendingSplit
        ? cells.sort((left, right) => left.index - right.index).flatMap((cell): BoardItem[] => {
            const mediaAssetId = cell.media_asset_id || cell.media_asset?.id;
            if (!mediaAssetId || !pendingSplit) return [];
            const cellWidth = Math.max(120, Math.round(source.size.width / pendingSplit.cols));
            const cellHeight = Math.max(120, Math.round(source.size.height / pendingSplit.rows));
            return [{
              id: clientId("grid-cell"), item_type: "media", resource_type: "media_asset", resource_id: mediaAssetId,
              position: { x: Math.round(source.position.x + source.size.width + 48 + cell.col * (cellWidth + 16)), y: Math.round(source.position.y + cell.row * (cellHeight + 16)) },
              size: { width: cellWidth, height: cellHeight }, group_id: groupId,
              display_settings: { canvas_operation: operation, source_item_id: source.id, grid_rows: pendingSplit.rows, grid_cols: pendingSplit.cols, include_split_lines: pendingSplit.includeSplitLines, cell: { row: cell.row, col: cell.col, index: cell.index } },
            }];
          })
        : outputs.flatMap((output): BoardItem[] => {
            const mediaAssetId = output.media_asset_id || output.media_asset?.id;
            if (!mediaAssetId) return [];
            const index = output.index ?? 0;
            const { width, height } = canvasOutputCardSize(source, output);
            return [{
              id: clientId("canvas-output"), item_type: "media", resource_type: "media_asset", resource_id: mediaAssetId,
              position: { x: Math.round(source.position.x + source.size.width + 48 + (index % 3) * (width + 16)), y: Math.round(source.position.y + Math.floor(index / 3) * (height + 16)) },
              size: { width, height }, group_id: groupId,
              display_settings: { canvas_operation: operation, source_item_id: source.id, output_index: index, output_label: output.label ?? operation },
            }];
          });
      if (createdItems.length === 0) continue;
      if (pendingSplit) {
        consumedCanvasSplitsRef.current.add(task.task_id);
        pendingCanvasSplitsRef.current.delete(task.task_id);
      } else {
        consumedCanvasAdvancedRef.current.add(task.task_id);
        pendingCanvasAdvancedRef.current.delete(task.task_id);
      }
      rememberBoard();
      setBoard((current) => current ? { ...current, items: current.items.concat(createdItems) } : current);
      setSelectedIds(createdItems.map((item) => item.id));
      void loadCatalog();
    }
  }, [board, canvasSplitGeneration, loadCatalog, projectName, tasks]);

  const undoBoard = () => {
    if (!board) return;
    const previous = boardHistoryRef.current.past.pop();
    if (!previous) return;
    boardHistoryRef.current.future.push(cloneBoard(board));
    setBoard(previous);
    setName(previous.name);
    setSelectedIds((ids) => ids.filter((id) => previous.items.some((item) => item.id === id)));
    setConnectMode(false);
  };

  const redoBoard = () => {
    if (!board) return;
    const next = boardHistoryRef.current.future.pop();
    if (!next) return;
    boardHistoryRef.current.past.push(cloneBoard(board));
    setBoard(next);
    setName(next.name);
    setSelectedIds((ids) => ids.filter((id) => next.items.some((item) => item.id === id)));
    setConnectMode(false);
  };

  const duplicateItems = (includeEdges: boolean, sourceItems = selectedItems) => {
    if (!board || sourceItems.length === 0) return [] as BoardItem[];
    rememberBoard();
    const sourceIds = new Set(sourceItems.map((item) => item.id));
    const idMap = new Map<string, string>();
    const clones = sourceItems.map((item) => {
      const id = clientId("item");
      idMap.set(item.id, id);
      return { ...item, id, position: { x: item.position.x + 36, y: item.position.y + 36 } };
    });
    const clonedEdges = includeEdges
      ? board.edges
          .filter((edge) => sourceIds.has(edge.source_item_id) && sourceIds.has(edge.target_item_id))
          .map((edge) => ({ ...edge, id: clientId("edge"), source_item_id: idMap.get(edge.source_item_id) ?? edge.source_item_id, target_item_id: idMap.get(edge.target_item_id) ?? edge.target_item_id }))
      : [];
    setBoard((current) => current ? { ...current, items: current.items.concat(clones), edges: current.edges.concat(clonedEdges) } : current);
    setSelectedIds(clones.map((item) => item.id));
    return clones;
  };

  const groupSelected = () => {
    if (!board || selectedItems.length < 2) return;
    rememberBoard();
    const groupId = clientId("group");
    const ids = new Set(selectedItems.map((item) => item.id));
    setBoard((current) => current ? { ...current, items: current.items.map((item) => ids.has(item.id) ? { ...item, group_id: groupId } : item) } : current);
  };

  const ungroupSelected = () => {
    if (!board || selectedItems.length === 0) return;
    const ids = new Set(selectedItems.map((item) => item.id));
    if (!selectedItems.some((item) => item.group_id)) return;
    rememberBoard();
    setBoard((current) => current ? { ...current, items: current.items.map((item) => ids.has(item.id) ? { ...item, group_id: null } : item) } : current);
  };

  const addBoardItem = (asset: UnifiedAsset, position?: { x: number; y: number }) => {
    if (!board) return;
    rememberBoard();
    const itemType = asset.kind === "video" ? "video" : asset.kind === "media" ? "media" : asset.kind;
    const nextPosition = position ?? { x: 180 + (board.items.length % 4) * 380, y: 150 + Math.floor(board.items.length / 4) * 270 };
    const nextItem: BoardItem = { id: clientId("item"), item_type: itemType, resource_type: asset.resourceType, resource_id: asset.id, position: { x: Math.round(nextPosition.x), y: Math.round(nextPosition.y) }, size: { width: 270, height: 246 } };
    setBoard((current) => current ? { ...current, items: current.items.concat(nextItem) } : current);
    setResourceId("");
  };

  const importAndAddAsset = async (asset: UnifiedAsset, position?: { x: number; y: number }) => {
    if (!board) return;
    setSaving(true);
    try {
      let projectAsset = asset;
      if (asset.source === "global") {
        await API.applyAssetsToProject({ asset_ids: [asset.id], target_project: projectName, conflict_policy: "skip" });
        const nextCatalog = await loadCatalog();
        if (!mountedRef.current) return;
        projectAsset = nextCatalog.find((candidate) => candidate.source === "project" && candidate.kind === asset.kind && (candidate.id === asset.id || candidate.name === asset.name)) || nextCatalog.find((candidate) => candidate.source === "project" && candidate.name === asset.name) || asset;
        if (projectAsset.source === "global") throw new Error(t("creative_board_asset_import_error", { defaultValue: "The asset was imported, but its project reference could not be resolved." }));
      }
      addBoardItem(projectAsset, position);
    } catch (reason) {
      if (mountedRef.current) setError(reason instanceof Error ? reason.message : t("creative_board_save_error", { defaultValue: "保存失败，请重试。" }));
    } finally {
      if (mountedRef.current) setSaving(false);
    }
  };


  const newBoardName = useMemo(() => {
    const baseName = t("creative_board_new_name");
    const existingNames = new Set(boardOptions.map((item) => item.name));
    if (!existingNames.has(baseName)) return baseName;
    let suffix = 1;
    while (existingNames.has(`${baseName} ${suffix}`)) suffix += 1;
    return `${baseName} ${suffix}`;
  }, [boardOptions, t]);

  const navigateToBoard = useCallback((boardId: string) => {
    const params = new URLSearchParams(routeSearch);
    params.set("project", projectName);
    params.set("board", boardId);
    const pathname = location.split("?", 1)[0] || location;
    navigate(`${pathname}?${params.toString()}`);
  }, [location, navigate, projectName, routeSearch]);

  const clearBoardParam = useCallback(() => {
    const params = new URLSearchParams(routeSearch);
    params.set("project", projectName);
    params.delete("board");
    const pathname = location.split("?", 1)[0] || location;
    navigate(`${pathname}?${params.toString()}`);
  }, [location, navigate, projectName, routeSearch]);

  const createNewBoard = useCallback(async () => {
    if (creatingBoard) return;
    setCreatingBoard(true);
    setError(null);
    try {
      const created = parseBoard(await API.createCreativeBoard(projectName, {
        name: newBoardName,
        viewport: { x: 28, y: 28, zoom: 1 },
      }));
      setBoardOptions((options) => [...options.filter((item) => item.id !== created.id), { id: created.id, name: created.name }]);
      pendingBoardEditRef.current = created.id;
      navigateToBoard(created.id);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : t("creative_board_create_error"));
    } finally {
      if (mountedRef.current) setCreatingBoard(false);
    }
  }, [creatingBoard, navigateToBoard, newBoardName, projectName, setError, t]);

  const switchBoard = useCallback((boardId: string) => {
    setBoardMenuOpen(false);
    setBoardActionsId(null);
    if (boardId !== board?.id) navigateToBoard(boardId);
  }, [board?.id, navigateToBoard]);

  const closeBoardMenu = useCallback(() => {
    setBoardMenuOpen(false);
    setBoardActionsId(null);
    setEditingBoardId(null);
  }, []);

  const beginBoardRename = useCallback((option: BoardOption) => {
    setBoardActionsId(null);
    setEditingBoardId(option.id);
    setEditingBoardName(option.name);
  }, []);

  // 把变更响应里的 revision 写回按板跟踪表；只同步当前展示的板到 state，
  // 避免旧板在途保存的响应覆盖当前板的版本号
  const applyBoardRevision = useCallback((boardId: string, response: { revision?: unknown; board?: unknown }) => {
    const nested = response.board && typeof response.board === "object" ? (response.board as { revision?: unknown }).revision : undefined;
    const nextRevision = typeof response.revision === "number" ? response.revision : typeof nested === "number" ? nested : undefined;
    if (nextRevision === undefined) return;
    boardRevisionsRef.current.set(boardId, nextRevision);
    if (mountedRef.current && currentBoardIdRef.current === boardId) setRevision(nextRevision);
  }, []);

  const saveBoardOptionName = useCallback(async (option: BoardOption) => {
    const nextName = editingBoardName.trim() || t("creative_board_default_name");
    setEditingBoardId(null);
    if (nextName === option.name) return;
    try {
      // 重命名带 CAS 并回写返回的 revision：不带版本号会让服务端版本 +1 而前端持有旧值，
      // 下一次自动保存必然 409，弹「同名文件已存在」冲突框
      const response = await API.updateCreativeBoard(option.id, {
        name: nextName,
        expected_revision: boardRevisionsRef.current.get(option.id),
      });
      applyBoardRevision(option.id, response);
      setBoardOptions((options) => options.map((item) => item.id === option.id ? { ...item, name: nextName } : item));
      if (option.id === board?.id) {
        setName(nextName);
        setBoard((current) => current ? { ...current, name: nextName } : current);
      }
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : t("creative_board_rename_error"));
    }
  }, [applyBoardRevision, board, editingBoardName, setError, t]);

  const openBoardInNewWindow = useCallback((boardId: string) => {
    const params = new URLSearchParams(routeSearch);
    params.set("project", projectName);
    params.set("board", boardId);
    const pathname = location.split("?", 1)[0] || location;
    window.open(window.location.origin + pathname + "?" + params.toString(), "_blank", "noopener,noreferrer");
    setBoardActionsId(null);
  }, [location, projectName, routeSearch]);

  const duplicateBoard = useCallback(async (option: BoardOption) => {
    setBoardActionsId(null);
    try {
      const copied = parseBoard(await API.duplicateCreativeBoard(option.id, { name: option.name + " " + t("creative_board_copy_suffix") }));
      if (!copied.id) throw new Error(t("creative_board_copy_error"));
      setBoardOptions((options) => [...options.filter((item) => item.id !== copied.id), { id: copied.id, name: copied.name }]);
      navigateToBoard(copied.id);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : t("creative_board_copy_error"));
    }
  }, [navigateToBoard, setError, t]);

  const deleteBoard = useCallback(async (option: BoardOption) => {
    setBoardActionsId(null);
    if (!window.confirm(t("creative_board_delete_confirm", { name: option.name }))) return;
    try {
      await API.deleteCreativeBoard(option.id);
      const remaining = boardOptions.filter((item) => item.id !== option.id);
      setBoardOptions(remaining);
      boardRevisionsRef.current.delete(option.id);
      // 删除当前板后必须切走：留着会让 URL 与本地 board 态仍指向已删的板，后续自动保存全部 404
      if (option.id === board?.id) {
        if (remaining[0]) navigateToBoard(remaining[0].id);
        else clearBoardParam();
      }
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : t("creative_board_delete_error"));
    }
  }, [board, boardOptions, clearBoardParam, navigateToBoard, setError, t]);

  const removeItem = (item: BoardItem, shouldRemember = true) => {
    if (!board) return;
    if (shouldRemember) rememberBoard();
    setBoard((current) => current ? { ...current, items: current.items.filter((candidate) => candidate.id !== item.id), edges: current.edges.filter((edge) => edge.source_item_id !== item.id && edge.target_item_id !== item.id) } : current);
    setSelectedIds((ids) => ids.filter((id) => id !== item.id));
  };
  const removeSelected = () => {
    if (!board || selectedItems.length === 0) return;
    rememberBoard();
    const ids = new Set(selectedItems.map((item) => item.id));
    setBoard((current) => current ? { ...current, items: current.items.filter((item) => !ids.has(item.id)), edges: current.edges.filter((edge) => !ids.has(edge.source_item_id) && !ids.has(edge.target_item_id)) } : current);
    setSelectedIds([]);
  };
  const relate = (targetId?: string) => {
    if (!board) return;
    const ids = targetId ? [selectedIds[0], targetId] : selectedIds;
    if (ids.length !== 2 || !ids[0] || !ids[1]) return;
    rememberBoard();
    const edge: BoardEdge = { id: clientId("edge"), source_item_id: ids[0], target_item_id: ids[1], relation: "reference" };
    setBoard((current) => current && current.edges.some((candidate) => candidate.source_item_id === edge.source_item_id && candidate.target_item_id === edge.target_item_id && candidate.relation === edge.relation) ? current : current ? { ...current, edges: current.edges.concat(edge) } : current);
    setConnectMode(false);
  };
  const removeEdge = (edgeId: string) => { rememberBoard(); setBoard((current) => current ? { ...current, edges: current.edges.filter((edge) => edge.id !== edgeId) } : current); };
  const openSkills = (item?: BoardItem) => { const query = skillQuery(item, currentEpisode); navigate("/skills" + (query ? "?" + query : "")); };
  const selectItem = (item: BoardItem, extend: boolean) => {
    if (connectMode && selectedIds.length === 1 && selectedIds[0] !== item.id) { relate(item.id); return; }
    setSelectedIds((ids) => extend ? ids.includes(item.id) ? ids.filter((id) => id !== item.id) : ids.concat(item.id) : ids.length === 1 && ids[0] === item.id ? [] : [item.id]);
  };

  const startResize = (event: ReactPointerEvent<HTMLButtonElement>, item: BoardItem, corner: ResizeCorner) => {
    event.stopPropagation();
    event.currentTarget.setPointerCapture(event.pointerId);
    resizeRef.current = {
      id: item.id,
      corner,
      startX: event.clientX,
      startY: event.clientY,
      position: { ...item.position },
      size: { ...item.size },
    };
    setSelectedIds([item.id]);
  };

  const onCanvasDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0 && event.button !== 1) return;
    const onNode = (event.target as HTMLElement).closest("[data-board-node]");
    const shouldPan = event.button === 1 || activeTool === "pan" || spacePanRef.current;
    if (!shouldPan) {
      if (!onNode) {
        if (toolAction) {
          setToolAdjustmentOpen(false);
        } else {
          setSelectedIds([]);
        }
      }
      return;
    }
    event.currentTarget.setPointerCapture(event.pointerId);
    panRef.current = { startX: event.clientX, startY: event.clientY, originX: viewportRef.current.x, originY: viewportRef.current.y };
  };
  const onPointerMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    const resize = resizeRef.current;
    if (resize) {
      if (!dragHistoryRecordedRef.current) {
        rememberBoard();
        dragHistoryRecordedRef.current = true;
      }
      const zoom = viewportRef.current.zoom;
      const dx = (event.clientX - resize.startX) / zoom;
      const dy = (event.clientY - resize.startY) / zoom;
      const minWidth = 120;
      const minHeight = 120;
      let width = resize.size.width;
      let height = resize.size.height;
      let x = resize.position.x;
      let y = resize.position.y;
      if (resize.corner.includes("e")) width = Math.max(minWidth, resize.size.width + dx);
      if (resize.corner.includes("s")) height = Math.max(minHeight, resize.size.height + dy);
      if (resize.corner.includes("w")) {
        width = Math.max(minWidth, resize.size.width - dx);
        x = resize.position.x + resize.size.width - width;
      }
      if (resize.corner.includes("n")) {
        height = Math.max(minHeight, resize.size.height - dy);
        y = resize.position.y + resize.size.height - height;
      }
      setBoard((current) => current ? { ...current, items: current.items.map((item) => item.id === resize.id ? { ...item, position: { x: Math.round(x), y: Math.round(y) }, size: { width: Math.round(width), height: Math.round(height) } } : item) } : current);
      return;
    }
    const drag = dragRef.current;
    if (drag && event.pointerId === drag.pointerId) {
      const distance = Math.hypot(event.clientX - drag.startX, event.clientY - drag.startY);
      if (!drag.active) {
        if (distance < BOARD_DRAG_THRESHOLD) return;
        drag.active = true;
        suppressNextNodeClickRef.current = true;
        setDraggingItemIds(drag.items.map((item) => item.id));
      }
      if (!dragHistoryRecordedRef.current) {
        rememberBoard();
        dragHistoryRecordedRef.current = true;
      }
      const zoom = viewportRef.current.zoom;
      const dx = (event.clientX - drag.startX) / zoom;
      const dy = (event.clientY - drag.startY) / zoom;
      setBoard((current) => current ? {
        ...current,
        items: current.items.map((item) => {
          const start = drag.items.find((candidate) => candidate.id === item.id);
          if (!start) return item;
          return {
            ...item,
            position: {
              x: Math.round(start.x + dx),
              y: Math.round(start.y + dy),
            },
          };
        }),
      } : current);
      return;
    }
    const pan = panRef.current;
    if (pan) setViewport({ x: pan.originX + event.clientX - pan.startX, y: pan.originY + event.clientY - pan.startY, zoom: viewportRef.current.zoom });
  };
  const onPointerUp = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (resizeRef.current) {
      resizeRef.current = null;
      dragHistoryRecordedRef.current = false;
    }
    const drag = dragRef.current;
    if (drag && event.pointerId === drag.pointerId) {
      dragRef.current = null;
      dragHistoryRecordedRef.current = false;
      setDraggingItemIds([]);
      // The persistence controller observes the updated board snapshot and saves the final position after debounce.
    }
    if (panRef.current) panRef.current = null;
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) event.currentTarget.releasePointerCapture?.(event.pointerId);
  };
  const onCanvasDrop = (event: ReactDragEvent<HTMLDivElement>) => {
    event.preventDefault();
    const raw = event.dataTransfer.getData("text/creative-board-asset");
    if (!raw || !canvasRef.current) return;
    try {
      const payload = JSON.parse(raw) as { id?: string; source?: AssetSource };
      const asset = catalog.find((candidate) => candidate.id === payload.id && (!payload.source || candidate.source === payload.source));
      if (!asset) return;
      const rect = canvasRef.current.getBoundingClientRect();
      const position = { x: (event.clientX - rect.left - viewportRef.current.x) / viewportRef.current.zoom - 135, y: (event.clientY - rect.top - viewportRef.current.y) / viewportRef.current.zoom - 100 };
      void importAndAddAsset(asset, position);
    } catch {
      setError(t("creative_board_invalid_asset_drop", { defaultValue: "This asset could not be added to the canvas." }));
    }
  };

  const onWheel = (event: ReactWheelEvent<HTMLDivElement>) => {
    event.preventDefault();
    const rect = canvasRef.current?.getBoundingClientRect();
    if (!rect) return;
    const currentViewport = viewportRef.current;
    if (!event.ctrlKey && !event.metaKey) {
      setViewport({ x: currentViewport.x - event.deltaX, y: currentViewport.y - event.deltaY, zoom: currentViewport.zoom });
      return;
    }
    const nextZoom = zoomClamp(currentViewport.zoom * (event.deltaY > 0 ? 0.9 : 1.1));
    const worldX = (event.clientX - rect.left - currentViewport.x) / currentViewport.zoom;
    const worldY = (event.clientY - rect.top - currentViewport.y) / currentViewport.zoom;
    setViewport({ x: event.clientX - rect.left - worldX * nextZoom, y: event.clientY - rect.top - worldY * nextZoom, zoom: nextZoom });
  };
  const fitView = () => {
    if (!board?.items.length) { setViewport({ x: 28, y: 28, zoom: 1 }); return; }
    const minX = Math.min(...board.items.map((item) => item.position.x));
    const minY = Math.min(...board.items.map((item) => item.position.y));
    const maxX = Math.max(...board.items.map((item) => item.position.x + item.size.width));
    const maxY = Math.max(...board.items.map((item) => item.position.y + item.size.height));
    const width = canvasRef.current?.clientWidth ?? 800;
    const height = canvasRef.current?.clientHeight ?? 600;
    const nextZoom = zoomClamp(Math.min((width - 110) / Math.max(1, maxX - minX), (height - 110) / Math.max(1, maxY - minY), 1.2));
    setViewport({ x: (width - (maxX - minX) * nextZoom) / 2 - minX * nextZoom, y: (height - (maxY - minY) * nextZoom) / 2 - minY * nextZoom, zoom: nextZoom });
  };

  const beginElementRename = useCallback((item: BoardItem) => {
    setElementActionsId(null);
    setEditingElementId(item.id);
    setEditingElementName(nodeTitle(item, names));
  }, [names]);

  const locateItem = useCallback((item: BoardItem) => {
    const rect = canvasRef.current?.getBoundingClientRect();
    const zoom = viewportRef.current.zoom;
    setSelectedIds([item.id]);
    setActiveTool("select");
    setConnectMode(false);
    if (rect) {
      setViewport({
        x: rect.width / 2 - (item.position.x + item.size.width / 2) * zoom,
        y: rect.height / 2 - (item.position.y + item.size.height / 2) * zoom,
        zoom,
      });
    }
  }, []);

  const downloadItem = useCallback((item: BoardItem) => {
    setElementActionsId(null);
    const asset = assetsByReference.get(assetKey(item.resource_type, item.resource_id)) || assetsByReference.get(item.resource_id);
    const source = asset?.media?.url || asset?.media?.content_url || assetImageSourceUrl(asset, asset?.imagePath);
    if (!source) {
      setError(t("creative_board_download_error", { defaultValue: "无法获取该素材的下载地址。" }));
      return;
    }
    const link = document.createElement("a");
    link.href = source;
    link.download = asset?.name || item.resource_id;
    link.target = "_blank";
    link.rel = "noreferrer";
    document.body.appendChild(link);
    link.click();
    link.remove();
  }, [assetsByReference, setError, t]);

  const closeToolPanel = useCallback(() => {
    setToolAction(null);
    setToolPreset(undefined);
    setToolInstruction("");
    setToolReferenceMediaAssetId("");
    setToolGridRows(3);
    setToolGridCols(3);
    setToolIncludeSplitLines(true);
    setToolAdjustmentOpen(true);
    setToolBusy(false);
    setToolEditorOperation(null);
    setToolEditorBusy(false);
  }, []);

  const expandSelectedItem = useCallback((item: BoardItem) => {
    rememberBoard();
    setBoard((current) => current ? {
      ...current,
      items: current.items.map((candidate) => candidate.id === item.id
        ? { ...candidate, position: { x: candidate.position.x - 40, y: candidate.position.y - 30 }, size: { width: Math.round(candidate.size.width * 1.25), height: Math.round(candidate.size.height * 1.25) } }
        : candidate),
    } : current);
  }, [board]);

  const openToolPanel = useCallback((selection: BoardToolbarSelection, item: BoardItem) => {
    const { action, preset } = selection;
    if (action === "download") {
      downloadItem(item);
      return;
    }
    if (action === "expand") {
      expandSelectedItem(item);
      return;
    }
    if (CANVAS_EDITOR_OPERATIONS.has(action)) {
      setSelectedIds([item.id]);
      setToolEditorOperation(action as CanvasEditorOperation);
      return;
    }
    setSelectedIds([item.id]);
    setToolPreset(preset);
    setToolInstruction("");
    setToolReferenceMediaAssetId("");
    if (action === "grid" || action === "split") {
      const presetMatch = preset?.match(/^(\\d+)x(\\d+)$/);
      setToolGridRows(presetMatch ? Math.min(8, Math.max(2, Number(presetMatch[1]))) : 3);
      setToolGridCols(presetMatch ? Math.min(8, Math.max(2, Number(presetMatch[2]))) : 3);
      setToolIncludeSplitLines(true);
    } else {
      setToolGridRows(3);
      setToolGridCols(3);
      setToolIncludeSplitLines(true);
    }
    setToolAdjustmentOpen(true);
    setToolAction(action);
  }, [downloadItem, expandSelectedItem]);

  const uploadToolReference = useCallback(async (file: File) => {
    if (!file.type.startsWith("image/")) {
      setError(t("creative_board_tool_reference_image", { defaultValue: "Reference image" }));
      return;
    }
    setToolBusy(true);
    try {
      const uploaded = await API.uploadMediaAsset(projectName, file);
      const record = uploaded && typeof uploaded === "object" ? uploaded : {};
      const uploadedId = typeof record.id === "string" ? record.id : typeof record.media_asset_id === "string" ? record.media_asset_id : "";
      await loadCatalog();
      if (uploadedId) setToolReferenceMediaAssetId(uploadedId);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : t("creative_board_save_error", { defaultValue: "Upload failed. Please try again." }));
    } finally {
      if (mountedRef.current) setToolBusy(false);
    }
  }, [loadCatalog, projectName, setError, t]);

  const submitToolAction = async () => {
    if (!toolAction || !selectedItem) return;
    const isGridAction = toolAction === "grid" || toolAction === "split";
    if (!isGridAction && !toolInstruction.trim()) return;
    setToolBusy(true);
    try {
      if (isGridAction) {
        const isMediaSource = selectedItem.resource_type === "media_asset";
        const projectResourceType = selectedItem.resource_type === "character"
          || selectedItem.resource_type === "scene"
          || selectedItem.resource_type === "prop"
          || selectedItem.resource_type === "product"
          || selectedItem.resource_type === "storyboard"
          ? selectedItem.resource_type
          : undefined;
        if (!isMediaSource && !projectResourceType) {
          setError(t("creative_board_tool_media_edit_unsupported"));
          return;
        }
        const enqueueResult = await enqueueCanvasImageSplit(projectName, isMediaSource
          ? {
              sourceKind: "media",
              mediaAssetId: selectedItem.resource_id,
              rows: toolGridRows,
              cols: toolGridCols,
              includeSplitLines: toolIncludeSplitLines,
            }
          : {
              sourceKind: "project",
              resourceType: projectResourceType,
              resourceId: selectedItem.resource_id,
              rows: toolGridRows,
              cols: toolGridCols,
              includeSplitLines: toolIncludeSplitLines,
            });
        for (const taskId of enqueueResult?.taskIds ?? []) {
          pendingCanvasSplitsRef.current.set(taskId, {
            sourceItemId: selectedItem.id,
            rows: toolGridRows,
            cols: toolGridCols,
            includeSplitLines: toolIncludeSplitLines,
          });
        }
        setCanvasSplitGeneration((value) => value + 1);
        setError(t("creative_board_tool_split_submitted"));
      } else {
        const isMediaSource = selectedItem.resource_type === "media_asset";
        const projectResourceType = selectedItem.resource_type === "character"
          || selectedItem.resource_type === "scene"
          || selectedItem.resource_type === "prop"
          || selectedItem.resource_type === "product"
          || selectedItem.resource_type === "storyboard"
          ? selectedItem.resource_type
          : undefined;
        if (!isMediaSource && !projectResourceType) {
          setError(t("creative_board_tool_media_edit_unsupported"));
          return;
        }
        const advancedOperation: CanvasAdvancedOperation | undefined = toolAction === "panorama"
          ? "canvas_image_panorama"
          : toolAction === "angles"
            ? "canvas_image_angles"
            : toolAction === "layers"
              ? "canvas_image_layers"
              : toolAction === "hd"
                ? "canvas_image_hd"
                : undefined;
        if (advancedOperation) {
          const enqueueResult = await enqueueCanvasImageAdvanced(projectName, isMediaSource
            ? { operation: advancedOperation, sourceKind: "media", mediaAssetId: selectedItem.resource_id, instruction: toolInstruction.trim() || undefined }
            : { operation: advancedOperation, sourceKind: "project", resourceType: projectResourceType, resourceId: selectedItem.resource_id, instruction: toolInstruction.trim() || undefined });
          for (const taskId of enqueueResult.taskIds) pendingCanvasAdvancedRef.current.set(taskId, { sourceItemId: selectedItem.id, operation: advancedOperation });
          setCanvasSplitGeneration((value) => value + 1);
          setError(t("creative_board_tool_advanced_submitted"));
        } else {
          if (!IMAGE_EDIT_TOOL_ACTIONS.has(toolAction) || !projectResourceType) {
            setError(t("creative_board_tool_media_edit_unsupported"));
            return;
          }
          await enqueueImageEdit(projectName, {
            resourceType: projectResourceType,
            resourceId: selectedItem.resource_id,
            instruction: imageEditInstruction(toolAction, toolPreset, toolInstruction),
            referenceMediaAssetId: toolReferenceMediaAssetId || null,
          });
          setError(t("creative_board_tool_edit_submitted"));
        }
      }
      closeToolPanel();
      await loadCatalog();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : t("creative_board_save_error", { defaultValue: "Save failed. Please try again." }));
    } finally {
      if (mountedRef.current) setToolBusy(false);
    }
  };

  const submitCanvasEditor = async (submission: CanvasEditorSubmission) => {
    if (!selectedItem || !toolEditorOperation) return;
    setToolEditorBusy(true);
    try {
      const operation = CANVAS_EDITOR_OPERATION_MAP[toolEditorOperation];
      const isMediaSource = selectedItem.resource_type === "media_asset";
      const projectResourceType = selectedItem.resource_type === "character"
        || selectedItem.resource_type === "scene"
        || selectedItem.resource_type === "prop"
        || selectedItem.resource_type === "product"
        || selectedItem.resource_type === "storyboard"
        ? selectedItem.resource_type
        : undefined;
      if (!isMediaSource && !projectResourceType) {
        setError(t("creative_board_tool_media_edit_unsupported"));
        return;
      }
      const base = {
        operation,
        instruction: submission.instruction || undefined,
        count: submission.count,
        region: submission.region,
        aspectRatio: submission.aspectRatio,
        multiplier: submission.multiplier,
      };
      const enqueueResult = await enqueueCanvasImageAdvanced(
        projectName,
        isMediaSource
          ? { ...base, sourceKind: "media", mediaAssetId: selectedItem.resource_id }
          : { ...base, sourceKind: "project", resourceType: projectResourceType, resourceId: selectedItem.resource_id },
      );
      for (const taskId of enqueueResult.taskIds) {
        pendingCanvasAdvancedRef.current.set(taskId, { sourceItemId: selectedItem.id, operation });
      }
      setCanvasSplitGeneration((value) => value + 1);
      setError(t("creative_board_tool_advanced_submitted"));
      closeToolPanel();
      await loadCatalog();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : t("creative_board_save_error", { defaultValue: "Save failed. Please try again." }));
    } finally {
      if (mountedRef.current) setToolEditorBusy(false);
    }
  };

  const cancelElementRename = useCallback(() => {
    setEditingElementId(null);
    setEditingElementName("");
  }, []);

  const saveElementRename = useCallback((item: BoardItem, rawName: string) => {
    const nextName = rawName.trim();
    cancelElementRename();
    if (!nextName || nextName === nodeTitle(item, names)) return;
    rememberBoard();
    setBoard((current) => current ? {
      ...current,
      items: current.items.map((candidate) => candidate.id === item.id
        ? { ...candidate, display_settings: { ...candidate.display_settings, name: nextName } }
        : candidate),
    } : current);
  }, [cancelElementRename, names]);

  const persistSnapshot = useCallback(async (snapshot: CreativeBoardSnapshot, previousSnapshot: CreativeBoardSnapshot | undefined): Promise<CreativeBoardSnapshot> => {
    const previous = previousSnapshot?.boardId === snapshot.boardId ? previousSnapshot : EMPTY_SNAPSHOT;
    // revision 按板维度跟踪：旧板在途保存的响应只写回旧板条目，不污染当前板的版本号
    let revision = boardRevisionsRef.current.get(snapshot.boardId) ?? snapshot.revision;
    const applyRevision = (response: { revision?: unknown; board?: unknown }) => {
      const nestedRevision = response.board && typeof response.board === "object" ? (response.board as Record<string, unknown>).revision : undefined;
      if (typeof response.revision === "number") revision = response.revision;
      else if (typeof nestedRevision === "number") revision = nestedRevision;
      boardRevisionsRef.current.set(snapshot.boardId, revision);
      if (mountedRef.current && currentBoardIdRef.current === snapshot.boardId) setRevision(revision);
    };
    const saveRequest = async <T extends { revision?: unknown; board?: unknown }>(request: () => Promise<T>) => {
      const response = await request();
      applyRevision(response);
      return response;
    };
    const currentItemIds = new Set(snapshot.items.map((item) => item.id));
    const previousItemById = new Map(previous.items.map((item) => [item.id, item]));
    const currentEdgeIds = new Set(snapshot.edges.map((edge) => edge.id));
    const previousEdgeIds = new Set(previous.edges.map((edge) => edge.id));
    const apiItemId = (id: string) => serverItemIdsRef.current.get(id) ?? id;
    const apiEdgeId = (id: string) => serverEdgeIdsRef.current.get(id) ?? id;
    try {
      if (!previousSnapshot || snapshot.name !== previous.name || !sameViewport(snapshot.viewport, previous.viewport)) {
        await saveRequest(() => API.updateCreativeBoard(snapshot.boardId, { name: snapshot.name, viewport: snapshot.viewport, expected_revision: revision }));
      }
      for (const edge of previous.edges) {
        if (!currentEdgeIds.has(edge.id)) {
          await saveRequest(() => API.deleteCreativeBoardEdge(snapshot.boardId, apiEdgeId(edge.id), revision));
        }
      }
      for (const item of snapshot.items) {
        const previousItem = previousItemById.get(item.id);
        if (!previousItem) {
          if (!serverItemIdsRef.current.has(item.id)) {
            const created = await saveRequest(() => API.addCreativeBoardItem(snapshot.boardId, { ...boardItemPayload(item), expected_revision: revision }));
            const record = created.item ?? created;
            if (typeof record.id === "string") serverItemIdsRef.current.set(item.id, record.id);
          }
        } else if (!sameBoardItem(item, previousItem)) {
          await saveRequest(() => API.updateCreativeBoardItem(snapshot.boardId, apiItemId(item.id), { ...boardItemPayload(item), expected_revision: revision }));
        }
      }
      for (const item of previous.items) {
        if (!currentItemIds.has(item.id)) {
          await saveRequest(() => API.deleteCreativeBoardItem(snapshot.boardId, apiItemId(item.id), revision));
        }
      }
      for (const edge of snapshot.edges) {
        if (!previousEdgeIds.has(edge.id) && !serverEdgeIdsRef.current.has(edge.id)) {
          const created = await saveRequest(() => API.addCreativeBoardEdge(snapshot.boardId, { source_item_id: apiItemId(edge.source_item_id), target_item_id: apiItemId(edge.target_item_id), relation: edge.relation, expected_revision: revision }));
          const record = created.edge ?? created;
          if (typeof record.id === "string") serverEdgeIdsRef.current.set(edge.id, record.id);
        }
      }
      if (mountedRef.current) { setConflict(false); setError(null); }
      return { ...snapshot, revision };
    } catch (reason) {
      if (mountedRef.current) {
        const revisionConflict = isCreativeBoardRevisionConflict(reason);
        const latestRevision = getCreativeBoardConflictRevision(reason);
        setConflict(revisionConflict);
        setConflictRevision(latestRevision ?? null);
        if (revisionConflict) setConflictOpen(true);
        setError(revisionConflict ? t("canvas.saveStatus.failed") : reason instanceof Error ? reason.message : t("creative_board_save_error", { defaultValue: "保存失败，请重试。" }));
      }
      throw reason;
    }
  }, [setError, t]);

  const persistenceSnapshot = useMemo<CreativeBoardSnapshot>(() => board ? { boardId: board.id, name, viewport: { x: viewport.x, y: viewport.y, zoom: viewport.zoom }, items: board.items, edges: board.edges, revision } : EMPTY_SNAPSHOT, [board, name, revision, viewport]);
  const persistence = useCanvasPersistence({ snapshot: persistenceSnapshot, snapshotKey: board ? boardPersistenceKey(persistenceSnapshot) : "", hydrated: Boolean(board) && !loading, enabled: Boolean(board), debounceMs: 1_000, save: persistSnapshot });
  const { status: persistenceStatus, saveNow, retry, reset } = persistence;

  const reloadBoard = useCallback(async () => {
    const loaded = await load();
    if (loaded && mountedRef.current) reset(loaded, boardPersistenceKey(loaded));
  }, [load, reset]);

  const resolveConflict = useCallback((decision: ConflictResolution) => {
    setConflictOpen(false);
    if (decision === "replace") {
      void reloadBoard();
      return;
    }
    if (decision === "rename") {
      if (conflictRevision !== null && board) boardRevisionsRef.current.set(board.id, conflictRevision);
      setConflict(false);
      setError(null);
      void retry();
    }
  }, [board, conflictRevision, reloadBoard, retry, setError]);

  const handleSaveRetry = useCallback(() => {
    if (conflict) {
      setConflictOpen(true);
      return Promise.resolve(false);
    }
    return retry();
  }, [conflict, retry]);

  const ensureSaved = useCallback(async () => {
    if (!board || conflict) return false;
    return saveNow();
  }, [board, conflict, saveNow]);

  const handleCopied = useCallback((copiedBoardId: string) => {
    navigateToBoard(copiedBoardId);
  }, [navigateToBoard]);

  useEffect(() => {
    if (!boardMenuOpen) return;
    const onKeyDown = (event: KeyboardEvent) => {
if (event.key === "Escape") closeBoardMenu();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [boardMenuOpen, closeBoardMenu]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      handleCanvasSaveShortcut(event, saveNow);
      if (event.key === "Escape") {
        setAddMenuOpen(false);
        setToolMenuOpen(false);
        setLibraryOpen(false);
        setRoleLibraryOpen(false);
        setHistoryOpen(false);
        setShortcutsOpen(false);
        setHelpOpen(false);
        setZoomMenuOpen(false);
        setConnectMode(false);
        return;
      }
      if (isEditableTarget(event.target)) return;
      const key = event.key.toLowerCase();
      const command = event.ctrlKey || event.metaKey;
      if (event.key === " ") {
        if (!spacePanRef.current) {
          toolBeforeSpaceRef.current = activeTool;
          spacePanRef.current = true;
          setActiveTool("pan");
        }
        event.preventDefault();
        return;
      }
      if (event.key === "Tab" && !event.shiftKey) {
        event.preventDefault();
        setAddMenuOpen(true);
        return;
      }
      if (!command && !event.altKey && !event.shiftKey && key === "v") {
        setActiveTool("select");
        return;
      }
      if (!command && !event.altKey && !event.shiftKey && key === "h") {
        setActiveTool("pan");
        return;
      }
      if (event.altKey && event.shiftKey && key === "f") {
        event.preventDefault();
        fitView();
        return;
      }
      if (command && event.shiftKey && key === "z") {
        event.preventDefault();
        redoBoard();
        return;
      }
      if (command && !event.shiftKey && key === "z") {
        event.preventDefault();
        undoBoard();
        return;
      }
      if (command && key === "d") {
        event.preventDefault();
        duplicateItems(true);
        return;
      }
      if (command && key === "l") {
        event.preventDefault();
        if (selectedIds.length === 2) relate();
        else setConnectMode(true);
        return;
      }
      if (command && event.altKey && key === "g") {
        event.preventDefault();
        if (event.shiftKey) ungroupSelected();
        else groupSelected();
        return;
      }
      if (command && event.key === "Enter") {
        event.preventDefault();
        if (selectedItem) openSkills(selectedItem);
        return;
      }
      if (command && (event.key === "=" || event.key === "+")) {
        event.preventDefault();
        const current = viewportRef.current;
        setViewport({ x: current.x, y: current.y, zoom: current.zoom + 0.1 });
        return;
      }
      if (command && (event.key === "-" || event.key === "_")) {
        event.preventDefault();
        const current = viewportRef.current;
        setViewport({ x: current.x, y: current.y, zoom: current.zoom - 0.1 });
        return;
      }
      if (command && event.key === "0") {
        event.preventDefault();
        fitView();
        return;
      }
      if (event.key === "Delete" || event.key === "Backspace") {
        event.preventDefault();
        void removeSelected();
      }
    };
    const onKeyUp = (event: KeyboardEvent) => {
      if (event.key !== " ") return;
      if (!spacePanRef.current) return;
      spacePanRef.current = false;
      setActiveTool(toolBeforeSpaceRef.current);
      event.preventDefault();
    };
    window.addEventListener("keydown", onKeyDown);
    window.addEventListener("keyup", onKeyUp);
    return () => {
      window.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("keyup", onKeyUp);
    };
  }, [activeTool, duplicateItems, fitView, groupSelected, openSkills, redoBoard, relate, removeSelected, saveNow, selectedIds.length, selectedItem, setViewport, undoBoard, ungroupSelected]);

  return <div className="flex h-full min-h-0 flex-col overflow-hidden bg-[#f6f8fc] text-[var(--color-text)]">
    {conflictOpen ? <ConflictModal existing={name} suggestedName={name} onResolve={resolveConflict} /> : null}
    <header className="shrink-0 border-b border-[#e4e9f0] bg-white"><div className="flex h-11 items-center gap-1 px-3"><Grid3X3 className="h-5 w-5 text-[#202124]" aria-hidden /><BoardSwitcher name={name} boardId={board?.id} boardOptions={boardOptions} open={boardMenuOpen} creating={creatingBoard} editingBoardId={editingBoardId} editingBoardName={editingBoardName} boardActionsId={boardActionsId} labels={{ title: t("creative_board_switch"), add: t("creative_board_add"), defaultName: t("creative_board_default_name"), nameInput: t("creative_board_name_input"), more: t("creative_board_more"), openNewWindow: t("creative_board_open_new_window"), rename: t("creative_board_rename"), duplicate: t("creative_board_duplicate"), delete: t("creative_board_delete") }} onToggle={() => setBoardMenuOpen((open) => !open)} onCreate={() => void createNewBoard()} onSwitch={switchBoard} onEditNameChange={setEditingBoardName} onSaveName={saveBoardOptionName} onCancelEdit={() => setEditingBoardId(null)} onToggleActions={(id) => setBoardActionsId((currentId) => currentId === id ? null : id)} onOpenNewWindow={openBoardInNewWindow} onRename={beginBoardRename} onDuplicate={duplicateBoard} onDelete={deleteBoard} /></div></header>
    <div className="relative flex min-h-0 flex-1">
      {leftOpen ? (
        <aside
          className="relative flex shrink-0 flex-col border-r border-[#e2e8f0] bg-white"
          style={{ width: leftPanelWidth }}
        >
          <div className="flex h-12 items-center gap-2 border-b border-[#edf0f4] px-2.5">
            <div className="flex flex-1 items-center gap-0.5">
              <button type="button" onClick={() => setLeftTab("canvas")} className={"rounded-md px-2.5 py-1.5 text-[11px] font-medium transition-colors " + (leftTab === "canvas" ? "bg-[#eeeeef] text-[#3f4650]" : "text-[#7d8590] hover:bg-[#f7f7f8]")}>
                {t("creative_board_canvas_label", { defaultValue: "Canvas" })}
              </button>
              <button type="button" onClick={() => setLeftTab("assets")} className={"rounded-md px-2.5 py-1.5 text-[11px] font-medium transition-colors " + (leftTab === "assets" ? "bg-[#eeeeef] text-[#3f4650]" : "text-[#7d8590] hover:bg-[#f7f7f8]")}>
                {t("creative_board_assets_label", { defaultValue: "Assets" })}
              </button>
            </div>
            <button type="button" onClick={() => setLeftOpen(false)} className="focus-ring rounded p-1 text-[#56606d]" aria-label={t("creative_board_collapse_left", { defaultValue: "Collapse left panel" })}>
<Keyboard className="h-4 w-4" aria-hidden />
            </button>
          </div>
          {searchOpen ? <div className="border-b border-[#edf0f4] px-2.5 py-2"><label className="flex h-8 items-center gap-1.5 rounded-md border border-[#dfe5ed] bg-white px-2 text-[10px] text-[#8a96a7]"><Search className="h-3 w-3" aria-hidden /><input value={search} onChange={(event) => setSearch(event.target.value)} className="min-w-0 flex-1 bg-transparent outline-none" placeholder={t(leftTab === "assets" ? "creative_board_search_assets" : "creative_board_search", { defaultValue: leftTab === "assets" ? "Search assets" : "Search canvas elements" })} /></label></div> : null}
          {leftTab === "canvas" ? (
            <>
              <div className="relative flex h-12 items-center justify-between border-b border-[#f0f1f4] px-2.5">
                <div className="flex items-center gap-1.5"><span className="text-[11px] font-medium text-[#8b9199]">{t("creative_board_elements", { defaultValue: "Canvas elements" })}</span></div>
                <div className="relative flex items-center gap-1 text-[11px] text-[#4d535c]">
                  <button type="button" onClick={() => setElementFilterOpen((value) => !value)} className="focus-ring inline-flex items-center gap-1 rounded-md px-1.5 py-1 hover:bg-[#f5f7fa]" aria-haspopup="menu" aria-expanded={elementFilterOpen} aria-label={t("creative_board_filter_label", { defaultValue: "Filter canvas elements" })}>
                    <span>{t(activeElementFilter.labelKey, { defaultValue: activeElementFilter.label })}</span><ChevronDown className="h-3 w-3 text-[#8a96a7]" aria-hidden />
                  </button>
                  <button type="button" onClick={() => setSearchOpen((value) => !value)} className="focus-ring rounded p-0.5 text-[#3f4650]" aria-label={t("creative_board_search", { defaultValue: "Search canvas elements" })}><Search className="h-4 w-4" aria-hidden /></button>
                  {elementFilterOpen ? (
                    <div className="absolute right-7 top-[calc(100%+6px)] z-40 w-48 rounded-xl border border-[#dfe5ed] bg-white p-1.5 shadow-[0_10px_28px_rgba(50,63,82,0.14)]" role="menu">
                      {ELEMENT_FILTERS.map((filter) => (
                        <button key={filter.value} type="button" role="menuitem" onClick={() => { setElementFilter(filter.value); setElementFilterOpen(false); }} className={"flex w-full items-center justify-between rounded-lg px-2.5 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc] " + (elementFilter === filter.value ? "bg-[#f1f2f4]" : "")}>
                          <span>{t(filter.labelKey, { defaultValue: filter.label })}</span>{elementFilter === filter.value ? <span aria-hidden>✓</span> : null}
                        </button>
                      ))}
                    </div>
                  ) : null}
                </div>
              </div>
              <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
                {filteredItems.map((item) => {
                  const Icon = iconFor(item.item_type);
                  const selected = selectedIds.includes(item.id);
                  const asset = assetsByReference.get(assetKey(item.resource_type, item.resource_id)) || assetsByReference.get(item.resource_id);
                  const source = sidebarAssetSourceUrl(asset);
                  return (
                    <div
                      key={item.id}
                      role="button"
                      tabIndex={0}
                      onClick={() => selectItem(item, false)}
                      onKeyDown={(event) => {
                        if (event.key === "Enter" || event.key === " ") {
                          event.preventDefault();
                          selectItem(item, false);
                        }
                      }}
                      className={"group flex min-h-10 w-full cursor-pointer items-center gap-2 rounded-lg border px-1.5 py-1 text-left transition-colors focus-within:ring-2 focus-within:ring-[#c8c1ff] " + (selected ? "border-transparent bg-[#f3f3f5]" : "border-transparent hover:bg-[#f8f8fa]")}
                    >
                      <span className="flex h-8 w-8 shrink-0 items-center justify-center overflow-hidden rounded-md border border-[#e4e8ef] bg-[#f5f6f8]" style={{ color: colorFor(item.item_type) }}>
                        {source ? <img src={source} alt="" draggable={false} onDragStart={(event) => event.preventDefault()} className="h-full w-full object-cover" /> : <Icon className="h-3.5 w-3.5" aria-hidden />}
                      </span>
                      {editingElementId === item.id ? (
                        <input
                          ref={elementNameInputRef}
                          value={editingElementName}
                          onChange={(event) => setEditingElementName(event.target.value)}
                          onClick={(event) => event.stopPropagation()}
                          onBlur={() => saveElementRename(item, editingElementName)}
                          onKeyDown={(event) => {
                            if (event.key === "Enter") {
                              event.preventDefault();
                              event.stopPropagation();
                              saveElementRename(item, editingElementName);
                            } else if (event.key === "Escape") {
                              event.preventDefault();
                              event.stopPropagation();
                              cancelElementRename();
                            }
                          }}
                          className="min-w-0 flex-1 rounded border border-[#c8c1ff] bg-white px-1.5 py-0.5 text-[11px] font-medium text-[#4b5563] outline-none"
                          aria-label={t("creative_board_item_name_input")}
                        />
                      ) : (
                        <span className="min-w-0 flex-1 truncate text-[11px] font-medium text-[#4b5563]">{nodeTitle(item, names)}</span>
                      )}
                      <div className="flex shrink-0 items-center gap-0.5 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100">
                        <span className="group/locate relative">
                          <button
                            type="button"
                            onClick={(event) => {
                              event.stopPropagation();
                              locateItem(item);
                            }}
                            className="focus-ring rounded p-1 text-[#a5afbd] transition-colors hover:bg-[#ecebff] hover:text-[#6254d9]"
                            aria-label={t("creative_board_locate_node")}
                            title={t("creative_board_locate_node")}
                          >
                            <LocateFixed className="h-3.5 w-3.5" aria-hidden />
                          </button>
                          <span role="tooltip" className="pointer-events-none absolute bottom-[calc(100%+7px)] right-0 z-50 whitespace-nowrap rounded-md bg-[#30343b] px-2 py-1 text-[10px] font-normal text-white opacity-0 shadow-[0_4px_12px_rgba(20,24,32,0.18)] transition-opacity group-hover/locate:opacity-100">
                            {t("creative_board_locate_node")}
                          </span>
                        </span>
                        <div className="relative" data-creative-board-element-actions={item.id}>
                          <button
                            type="button"
                            onClick={(event) => {
                              event.stopPropagation();
                              setElementActionsId((currentId) => currentId === item.id ? null : item.id);
                            }}
                            className={"focus-ring rounded p-1 text-[#a5afbd] transition-colors hover:bg-[#ecebff] hover:text-[#6254d9] " + (elementActionsId === item.id ? "bg-[#ecebff] text-[#6254d9]" : "")}
                            aria-label={t("creative_board_more")}
                            aria-haspopup="menu"
                            aria-expanded={elementActionsId === item.id}
                            title={t("creative_board_more")}
                          >
                            <MoreHorizontal className="h-3.5 w-3.5" aria-hidden />
                          </button>
                          {elementActionsId === item.id ? (
                            <div role="menu" className="absolute right-0 top-[calc(100%+4px)] z-50 w-36 rounded-lg border border-[#dce3ec] bg-white p-1 shadow-[0_10px_24px_rgba(50,63,82,0.16)]">
                              <button type="button" role="menuitem" onClick={(event) => { event.stopPropagation(); beginElementRename(item); }} className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f5f7fa]">
                                <Pencil className="h-3.5 w-3.5 text-[#64748b]" aria-hidden />
                                {t("creative_board_rename_item")}
                              </button>
                              <button type="button" role="menuitem" onClick={(event) => { event.stopPropagation(); setElementActionsId(null); duplicateItems(false, [item]); }} className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f5f7fa]">
                                <Copy className="h-3.5 w-3.5 text-[#64748b]" aria-hidden />
                                {t("creative_board_duplicate_item")}
                              </button>
                              <button type="button" role="menuitem" onClick={(event) => { event.stopPropagation(); void downloadItem(item); }} className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f5f7fa]">
                                <Download className="h-3.5 w-3.5 text-[#64748b]" aria-hidden />
                                {t("creative_board_download_item")}
                              </button>
                            </div>
                          ) : null}
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>
            </>
          ) : (
            <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
              <div className="flex h-12 items-center justify-between border-b border-[#f0f1f4] px-2.5">
                <span className="text-[10px] font-semibold text-[#475569]">{t("creative_board_assets", { defaultValue: "Unified assets" })}</span>
                <button type="button" onClick={() => setSearchOpen((value) => !value)} className="focus-ring rounded p-0.5 text-[#3f4650]" aria-label={t("creative_board_search_assets", { defaultValue: "Search assets" })}><Search className="h-4 w-4" aria-hidden /></button>
              </div>
              <div className="space-y-2 px-1 pb-2">
                <div className="grid grid-cols-3 gap-1 rounded-lg bg-[#f4f6f9] p-1" role="tablist" aria-label={t("creative_board_asset_category", { defaultValue: "Asset category" })}>
                  {ASSET_CATEGORIES.map((category) => (
                    <button
                      key={category}
                      type="button"
                      role="tab"
                      aria-selected={assetCategory === category}
                      onClick={() => {
                        setAssetCategory(category);
                        setAssetTypeFilter("all");
                      }}
                      className={"rounded-md px-1.5 py-1.5 text-[10px] font-medium transition-colors " + (assetCategory === category ? "bg-white text-[#5145b6] shadow-sm" : "text-[#7d8795] hover:text-[#475569]")}
                    >
                      {category === "global" ? <Globe2 className="mr-1 inline h-3 w-3" aria-hidden /> : null}{t("creative_board_asset_category_" + category, { defaultValue: category === "personal" ? "Personal" : category === "agent" ? "Agent" : "Global" })}
                    </button>
                  ))}
                </div>
                <select value={assetTypeFilter} onChange={(event) => setAssetTypeFilter(event.target.value as AssetKind | "all")} aria-label={t("creative_board_asset_type_filter", { defaultValue: "Asset type" })} className="w-full rounded-md border border-[#dfe5ed] bg-white px-2 py-1.5 text-[10px] text-[#64748b]">
                  <option value="all">{t("creative_board_asset_type_all", { defaultValue: "All types" })}</option>
                  {ASSET_TYPES.filter((type) => type !== "all" && (assetCategory !== "agent" || ["character", "scene", "prop"].includes(type))).map((type) => (
                    <option key={type} value={type}>{t("creative_board_type_" + type, { defaultValue: type })}</option>
                  ))}
                </select>
              </div>
              <div className="min-h-0 overflow-y-auto px-1 pb-2">
                {filteredAssets.map((asset) => {
                  const Icon = assetIcon(asset.kind);
                  const source = sidebarAssetSourceUrl(asset);
                  return (
                    <button type="button" key={asset.source + ":" + asset.resourceType + ":" + asset.id} draggable onDragStart={(event) => event.dataTransfer.setData("text/creative-board-asset", JSON.stringify({ id: asset.id, source: asset.source }))} onClick={() => void importAndAddAsset(asset)} disabled={saving} className="group mb-1.5 flex w-full items-center gap-2 rounded-lg border border-[#edf0f4] p-1.5 text-left hover:border-[#c8c1ff] hover:bg-[#faf9ff] disabled:opacity-60">
                      <span className="h-9 w-10 shrink-0 overflow-hidden rounded bg-[#f0f2f6]">
                        {source ? <img src={source} alt="" draggable={false} onDragStart={(event) => event.preventDefault()} className="h-full w-full object-cover" /> : <span className="flex h-full items-center justify-center" style={{ color: colorFor(asset.kind) }}><Icon className="h-4 w-4" aria-hidden /></span>}
                      </span>
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-[10px] font-medium text-[#334155]">{asset.name}</span>
                        <span className="mt-0.5 block truncate text-[9px] text-[#94a3b8]">{activeAssetCategoryLabel} · {t("creative_board_type_" + asset.kind, { defaultValue: asset.kind })}</span>
                      </span>
                      {asset.source === "global" ? <Upload className="h-3 w-3 shrink-0 text-[#6254d9]" aria-hidden /> : <Plus className="h-3 w-3 shrink-0 text-[#8a96a7] opacity-0 group-hover:opacity-100" aria-hidden />}
                    </button>
                  );
                })}
                {filteredAssets.length === 0 ? (
                  <div className="rounded-lg border border-dashed border-[#ccd6e2] p-4 text-center text-[10px] text-[#8a96a7]">{t("creative_board_no_assets", { defaultValue: "No matching assets" })}</div>
                ) : null}
              </div>
            </div>
          )}
          {/* ---- 右缘拖拽调宽手柄：缩到阈值以下折叠，双击手柄折叠 ---- */}
          <div
            role="separator"
            aria-orientation="vertical"
            aria-valuemin={LEFT_PANEL_MIN_WIDTH}
            aria-valuemax={LEFT_PANEL_MAX_WIDTH}
            aria-valuenow={leftPanelWidth}
            aria-label={t("creative_board_resize_left", { defaultValue: "Resize left panel" })}
            title={t("creative_board_resize_left", { defaultValue: "Resize left panel" })}
            onMouseDown={handleLeftPanelResizeStart}
            onDoubleClick={() => setLeftOpen(false)}
            className="group absolute inset-y-0 right-0 z-10 w-1.5 cursor-col-resize select-none"
          >
            <div
              className={
                "absolute inset-y-0 right-0 w-px transition-colors duration-150 " +
                (leftPanelDragging ? "bg-[#6254d9]" : "bg-transparent group-hover:bg-[#6254d9]")
              }
            />
          </div>
        </aside>
      ) : (
        <button type="button" onClick={() => setLeftOpen(true)} className="focus-ring absolute left-1 top-1 z-30 grid h-5 w-5 place-items-center rounded-md border border-[#dfe5ed] bg-white p-0.5 shadow-sm" aria-label={t("creative_board_open_left", { defaultValue: "Open left panel" })}>
          <PanelLeft className="h-3 w-3" aria-hidden />
        </button>
      )}
      <section className="relative flex min-w-0 flex-1 flex-col bg-[#f5f8fc]">
{error ? <div className="z-20 flex items-center justify-between border-b border-[#e9c2c2] bg-[#fff8f8] px-4 py-2 text-[10px] text-[#a34a4a]"><span>{error}</span><button type="button" onClick={() => void load()} className="underline">{t("creative_board_retry")}</button></div> : null}<div ref={canvasRef} onPointerDown={onCanvasDown} onPointerMove={onPointerMove} onPointerUp={onPointerUp} onPointerCancel={onPointerUp} onWheel={onWheel} onDoubleClick={(event) => { if ((event.target as HTMLElement).closest("[data-board-node], button, input, [role=menu]")) return; setAddMenuOpen(true); }} onDragOver={(event) => event.preventDefault()} onDrop={onCanvasDrop} data-testid="creative-board-canvas" className={"relative min-h-0 flex-1 overflow-hidden " + (activeTool === "pan" ? "cursor-grab" : "cursor-default")} style={gridVisible ? { backgroundImage: "linear-gradient(#dce4ee 1px, transparent 1px), linear-gradient(90deg, #dce4ee 1px, transparent 1px)", backgroundSize: gridSize + "px " + gridSize + "px", backgroundPosition: viewport.x % gridSize + "px " + viewport.y % gridSize + "px" } : undefined}>{loading ? <div className="flex h-full items-center justify-center text-[11px] text-[#8a96a7]"><Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />{t("creative_board_loading")}</div> : board ? <div className="absolute" style={{ left: worldBounds.minX, top: worldBounds.minY, width: worldBounds.width, height: worldBounds.height, overflow: "visible", transform: "translate(" + (viewport.x - worldBounds.minX) + "px, " + (viewport.y - worldBounds.minY) + "px) scale(" + viewport.zoom + ")", transformOrigin: "0 0" }}><svg className="pointer-events-none absolute left-0 top-0 overflow-visible" width={worldBounds.width} height={worldBounds.height} aria-label={t("creative_board_connections", { defaultValue: "Board connections" })}><defs><marker id="creative-board-arrow" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto"><path d="M0,0 L8,4 L0,8 z" fill="#a8b3c3" /></marker></defs>{board.edges.map((edge) => { const source = board.items.find((item) => item.id === edge.source_item_id); const target = board.items.find((item) => item.id === edge.target_item_id); return source && target ? <path key={edge.id} d={edgePath(source, target)} fill="none" stroke="#a8b3c3" strokeWidth="2.5" markerEnd="url(#creative-board-arrow)" className="pointer-events-auto cursor-pointer hover:stroke-[#6254d9]" onClick={() => void removeEdge(edge.id)} /> : null; })}</svg>{board.items.map((item) => { const Icon = iconFor(item.item_type); const selected = selectedIds.includes(item.id); const dragging = draggingItemIds.includes(item.id); const itemAsset = assetsByReference.get(assetKey(item.resource_type, item.resource_id)) || assetsByReference.get(item.resource_id); return <article key={item.id} data-testid={`creative-board-item-${item.id}`} data-board-node className={"group absolute flex select-none flex-col overflow-hidden rounded-xl border bg-white " + (dragging ? "z-30 cursor-grabbing shadow-[0_14px_34px_rgba(53,68,91,0.2)] " : "shadow-[0_8px_24px_rgba(53,68,91,0.12)] ") + (selected ? "border-[#6254d9] ring-2 ring-[#6254d9]/20" : "border-[#d9e0ea] hover:shadow-[0_12px_30px_rgba(53,68,91,0.16)]")} style={{ left: item.position.x, top: item.position.y, width: item.size.width, height: item.size.height, minHeight: item.size.height }} onPointerDown={(event) => { if (event.button !== 0 || activeTool === "pan") return; event.preventDefault(); event.stopPropagation(); if (connectMode) return; pendingNodePointerSelectionRef.current = item.id; if (event.shiftKey || !selectedIds.includes(item.id)) selectItem(item, event.shiftKey); const draggedItem = event.altKey ? duplicateItems(event.ctrlKey || event.metaKey, [item])[0] ?? item : item; const dragItems = event.altKey ? [draggedItem] : selectedIds.includes(item.id) && selectedItems.length > 0 ? selectedItems : [draggedItem]; dragRef.current = { pointerId: event.pointerId, startX: event.clientX, startY: event.clientY, active: false, items: dragItems.map((candidate) => ({ id: candidate.id, x: candidate.position.x, y: candidate.position.y })) }; canvasRef.current?.setPointerCapture?.(event.pointerId); }} onClick={(event) => { if (activeTool === "pan") return; event.stopPropagation(); if (suppressNextNodeClickRef.current) { suppressNextNodeClickRef.current = false; pendingNodePointerSelectionRef.current = null; return; } if (pendingNodePointerSelectionRef.current === item.id) { pendingNodePointerSelectionRef.current = null; return; } if (event.shiftKey || !selectedIds.includes(item.id)) selectItem(item, event.shiftKey); }} onDoubleClick={() => item.resource_type === "media_asset" ? navigate("/media?asset=" + encodeURIComponent(item.resource_id)) : navigate("/timeline")}><div className="flex items-center gap-2 border-b border-[#edf0f4] px-3 py-2"><span className="flex h-5 w-5 items-center justify-center rounded" style={{ background: colorFor(item.item_type) + "20", color: colorFor(item.item_type) }}><Icon className="h-3 w-3" aria-hidden /></span><span className="min-w-0 flex-1 truncate text-[10px] font-semibold text-[#334155]">{nodeTitle(item, names)}</span><button type="button" onPointerDown={(event) => event.stopPropagation()} onClick={(event) => { event.stopPropagation(); void removeItem(item); }} aria-label={t("creative_board_delete_item")} className="rounded p-0.5 text-[#a5afbd] opacity-0 group-hover:opacity-100"><Trash2 className="h-3 w-3" aria-hidden /></button></div><div className="min-h-0 flex-1 overflow-hidden border-b border-[#edf0f4]">{previewFor(item, itemAsset)}</div><div className="flex items-center justify-between px-3 py-2"><span className="truncate text-[9px] text-[#94a3b8]">{t("creative_board_type_" + item.item_type)}</span><span className="flex items-center gap-1 text-[9px] text-[#94a3b8]"><Link2 className="h-3 w-3" aria-hidden />{board.edges.filter((edge) => edge.source_item_id === item.id || edge.target_item_id === item.id).length}</span></div><span className="absolute -left-1.5 top-1/2 h-3 w-3 -translate-y-1/2 rounded-full border-2 border-white bg-[#a8b3c3] shadow-sm" /><span className="absolute -right-1.5 top-1/2 h-3 w-3 -translate-y-1/2 rounded-full border-2 border-white bg-[#a8b3c3] shadow-sm" /></article>; })}{selectedItem && draggingItemIds.length === 0 ? <BoardSelectionOverlay name={nodeTitle(selectedItem, names)} position={selectedItem.position} size={selectedItem.size} zoom={viewport.zoom} multiSelected={selectedItems.length > 1} onResizeStart={(event, corner) => startResize(event, selectedItem, corner)} onDuplicate={() => { duplicateItems(false, [selectedItem]); }} onDelete={() => removeItem(selectedItem)} onOpenSkills={() => openSkills(selectedItem)} showTools={isImageBoardItem(selectedItem)} labels={{ openTools: t("creative_board_selection_tools", { defaultValue: "Creative tools" }), duplicate: t("creative_board_duplicate_item", { defaultValue: "Duplicate element" }), delete: t("creative_board_delete_item"), resize: (corner) => t("creative_board_resize", { defaultValue: "Resize {{corner}}", corner }), toolbar: { portrait: t("creative_board_toolbar_portrait"), portraitEmotion: t("creative_board_toolbar_portrait_emotion"), panorama: t("creative_board_toolbar_panorama"), angles: t("creative_board_toolbar_angles"), lighting: t("creative_board_toolbar_lighting"), grid: t("creative_board_toolbar_grid"), gridPending: t("creative_board_tool_grid_pending"), hd: t("creative_board_toolbar_hd"), hd2k: t("creative_board_toolbar_hd2k"), hd4k: t("creative_board_toolbar_hd4k"), outpaint: t("creative_board_toolbar_outpaint"), redraw: t("creative_board_toolbar_redraw"), erase: t("creative_board_toolbar_erase"), cutout: t("creative_board_toolbar_cutout"), crop: t("creative_board_toolbar_crop"), edit: t("creative_board_toolbar_edit"), layers: t("creative_board_toolbar_layers"), split: t("creative_board_toolbar_split"), split2x2: t("creative_board_toolbar_split2x2"), split3x3: t("creative_board_toolbar_split3x3"), split4x4: t("creative_board_toolbar_split4x4"), adjust: t("creative_board_toolbar_adjust"), symmetry: t("creative_board_toolbar_symmetry"), download: t("creative_board_download_item"), expand: t("creative_board_toolbar_expand") } }} onToolbarAction={(selection) => openToolPanel(selection, selectedItem)} /> : null}{toolAction && selectedItem ? <BoardImageToolPanel action={toolAction} preset={toolPreset} adjustmentOpen={toolAdjustmentOpen} onPreviewClick={() => setToolAdjustmentOpen(true)} title={toolAction === "portrait-emotion" ? t("creative_board_toolbar_portrait_emotion") : toolAction === "grid" ? t("creative_board_toolbar_grid") : t("creative_board_toolbar_" + toolAction)} imageUrl={assetSourceUrl(selectedAsset)} instruction={toolInstruction} referenceMediaAssetId={toolReferenceMediaAssetId} referenceAssets={referenceAssets} gridRows={toolGridRows} gridCols={toolGridCols} includeSplitLines={toolIncludeSplitLines} busy={toolBusy} onClose={closeToolPanel} onInstructionChange={setToolInstruction} onReferenceChange={setToolReferenceMediaAssetId} onGridRowsChange={setToolGridRows} onGridColsChange={setToolGridCols} onIncludeSplitLinesChange={setToolIncludeSplitLines} onUploadReference={uploadToolReference} onSubmit={submitToolAction} labels={{ description: toolAction === "edit" ? t("creative_board_tool_description_edit") : t("creative_board_tool_description_adjust"), close: t("creative_board_tool_close"), previewUnavailable: t("creative_board_tool_preview_unavailable"), instructionPlaceholder: t("creative_board_tool_instruction_placeholder"), instructionLabel: t("creative_board_tool_instruction_label"), referenceImage: t("creative_board_tool_reference_image"), upload: t("creative_board_tool_upload"), noReference: t("creative_board_tool_no_reference"), cancel: t("creative_board_tool_cancel"), submitEdit: t("creative_board_tool_submit_edit"), applyAdjustment: t("creative_board_tool_apply_adjustment"), gridRows: t("creative_board_tool_grid_rows"), gridCols: t("creative_board_tool_grid_cols"), includeSplitLines: t("creative_board_tool_split_lines") }} /> : null}{toolEditorOperation && selectedItem ? <CanvasImageEditorOverlay operation={toolEditorOperation} title={t("creative_board_toolbar_" + toolEditorOperation)} imageUrl={assetSourceUrl(selectedAsset)} busy={toolEditorBusy} onClose={closeToolPanel} onSubmit={submitCanvasEditor} labels={{ close: t("creative_board_tool_close"), run: t("creative_board_tool_run"), running: t("creative_board_tool_running"), instructionPlaceholder: t("creative_board_tool_instruction_placeholder"), instructionLabel: t("creative_board_tool_instruction_label"), regionHint: t("creative_board_editor_region_hint"), ratio: t("creative_board_editor_ratio"), ratioOriginal: t("creative_board_editor_ratio_original"), ratio116: t("creative_board_editor_ratio_1x1"), ratio34: t("creative_board_editor_ratio_3x4"), ratio169: t("creative_board_editor_ratio_16x9"), resolution: t("creative_board_editor_resolution"), resolution2k: t("creative_board_editor_resolution_2k"), resolution4k: t("creative_board_editor_resolution_4k"), count: t("creative_board_editor_count"), multiplier: t("creative_board_editor_multiplier"), multiplier2: t("creative_board_editor_multiplier_2"), multiplier4: t("creative_board_editor_multiplier_4"), multiplier6: t("creative_board_editor_multiplier_6") }} /> : null}</div> : null}
        <div className="pointer-events-none absolute inset-x-0 bottom-4 z-30 flex items-end px-4 sm:px-6" onPointerDown={(event) => event.stopPropagation()}>
          <div className="pointer-events-auto flex shrink-0 items-center gap-1 rounded-xl border border-[#dce3ec] bg-white/95 p-1.5 shadow-[0_8px_24px_rgba(50,63,82,0.16)] backdrop-blur-sm">
            <button type="button" onClick={fitView} className="focus-ring rounded-lg p-1.5 text-[#64748b] hover:bg-[#f5f7fa]" aria-label={t("creative_board_fit", { defaultValue: "Arrange canvas" })} title={t("creative_board_fit", { defaultValue: "Arrange canvas" })}><Maximize2 className="h-3.5 w-3.5" aria-hidden /></button>
            <div className="relative">
              <button type="button" onClick={() => setMinimapOpen((open) => !open)} className={"focus-ring rounded-lg p-1.5 " + (minimapOpen ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_minimap", { defaultValue: "Canvas minimap" })} aria-expanded={minimapOpen} title={t("creative_board_minimap", { defaultValue: "Canvas minimap" })}><PanelLeft className="h-3.5 w-3.5" aria-hidden /></button>
              {minimapOpen ? <div className="absolute bottom-[calc(100%+8px)] left-1/2 z-40 -translate-x-1/2 rounded-xl border border-[#dce3ec] bg-white p-2 shadow-[0_12px_30px_rgba(50,63,82,0.18)]"><div className="relative h-24 w-40 overflow-hidden rounded-lg border border-[#e3e8ef] bg-[#f8fafc]" aria-label={t("creative_board_minimap", { defaultValue: "Canvas minimap" })}>{(board?.items ?? []).map((item) => <span key={item.id} className="absolute h-2.5 w-4 rounded-sm bg-[#b9b1f4]" style={{ left: Math.min(94, Math.max(2, (item.position.x - worldBounds.minX) / worldBounds.width * 100)) + "%", top: Math.min(92, Math.max(2, (item.position.y - worldBounds.minY) / worldBounds.height * 100)) + "%" }} />)}</div></div> : null}
            </div>
            <button type="button" onClick={() => setConnectMode((value) => !value)} className={"focus-ring rounded-lg p-1.5 " + (connectMode ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_connections", { defaultValue: "Show node connections" })} title={t("creative_board_connections", { defaultValue: "Show node connections" })}><Link2 className="h-3.5 w-3.5" aria-hidden /></button>
            <button type="button" onClick={() => setGridVisible((value) => !value)} className={"focus-ring rounded-lg p-1.5 " + (gridVisible ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_grid", { defaultValue: "Snap to grid" })} title={t("creative_board_grid", { defaultValue: "Snap to grid" })}><Grid3X3 className="h-3.5 w-3.5" aria-hidden /></button>
            <div className="relative">
              <button type="button" onClick={() => setZoomMenuOpen((open) => !open)} className="focus-ring rounded-lg px-1.5 py-1.5 text-[10px] tabular-nums text-[#64748b] hover:bg-[#f5f7fa]" aria-label={t("creative_board_zoom_menu", { defaultValue: "Zoom" })} aria-expanded={zoomMenuOpen} title={t("creative_board_zoom_menu", { defaultValue: "Zoom" })}>{Math.round(viewport.zoom * 100)}%</button>
              {zoomMenuOpen ? <div className="absolute bottom-[calc(100%+8px)] left-1/2 z-40 w-48 -translate-x-1/2 rounded-xl border border-[#dce3ec] bg-white p-2 shadow-[0_12px_30px_rgba(50,63,82,0.18)]"><div className="mb-1 flex items-center rounded-lg bg-[#f5f7fa] px-2 py-1"><input type="number" min="35" max="240" value={Math.round(viewport.zoom * 100)} onChange={(event) => setViewport({ ...viewportRef.current, zoom: Number(event.target.value) / 100 }, true)} className="w-full bg-transparent text-[12px] text-[#334155] outline-none" aria-label={t("creative_board_zoom_menu", { defaultValue: "Zoom" })} /><span className="text-[11px] text-[#94a3b8]">%</span></div><button type="button" onClick={() => setViewport({ ...viewportRef.current, zoom: viewportRef.current.zoom + 0.1 }, true)} className="block w-full rounded-lg px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]">{t("creative_board_zoom_in", { defaultValue: "Zoom in" })}</button><button type="button" onClick={() => setViewport({ ...viewportRef.current, zoom: viewportRef.current.zoom - 0.1 }, true)} className="block w-full rounded-lg px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]">{t("creative_board_zoom_out", { defaultValue: "Zoom out" })}</button><button type="button" onClick={fitView} className="block w-full rounded-lg px-2 py-1.5 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]">{t("creative_board_fit", { defaultValue: "Fit canvas" })}</button></div> : null}
            </div>
          </div>
          <div className="pointer-events-auto absolute bottom-0 left-1/2 flex w-max max-w-[calc(100%-2rem)] -translate-x-1/2 items-center gap-1 overflow-x-auto rounded-xl border border-[#dce3ec] bg-white/95 p-1.5 shadow-[0_8px_24px_rgba(50,63,82,0.16)] backdrop-blur-sm">
            <div className="relative">
              <button type="button" onClick={() => setAddMenuOpen((open) => !open)} className={"focus-ring grid h-8 w-8 place-items-center rounded-lg " + (addMenuOpen ? "bg-[#6254d9] text-white" : "bg-[#1f2937] text-white")} aria-label={t("creative_board_add_card")} aria-expanded={addMenuOpen} title={t("creative_board_add_card")}><Plus className="h-4 w-4" aria-hidden /></button>
            {addMenuOpen ? (
              <div className="absolute bottom-[calc(100%+8px)] left-0 z-40 max-h-[min(520px,calc(100vh-120px))] w-64 overflow-y-auto rounded-xl border border-[#dce3ec] bg-white p-2.5 shadow-[0_12px_30px_rgba(50,63,82,0.18)]">
                <div className="mb-2 px-2 text-[11px] font-semibold text-[#334155]">{t("creative_board_add_node_title", { defaultValue: "Add node" })}</div>
                <div className="space-y-0.5">
                  <button type="button" onClick={() => { setItemType("document"); setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><FileText className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_text", { defaultValue: "Text" })}</button>
                  <button type="button" onClick={() => { setItemType("media"); setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><ImageIcon className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_image", { defaultValue: "Image" })}</button>
                  <button type="button" onClick={() => { setItemType("video"); setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Video className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_video", { defaultValue: "Video" })}</button>
                  <button type="button" onClick={() => { setItemType("video"); setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Sparkles className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_video_edit", { defaultValue: "Video editing" })}<span className="ml-auto rounded-full bg-[#f1f3f5] px-1.5 py-0.5 text-[9px] text-[#7b8492]">{t("creative_board_beta_badge", { defaultValue: "Beta" })}</span></button>
                  <button type="button" onClick={() => { setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Landmark className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_director", { defaultValue: "Director's desk" })}<span className="ml-auto rounded-full bg-[#e9fbff] px-1.5 py-0.5 text-[9px] font-semibold text-[#0f9fb8]">{t("creative_board_new_badge", { defaultValue: "NEW" })}</span></button>
                  <button type="button" onClick={() => { setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Search className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_frame_extract", { defaultValue: "Frame extraction" })}<span className="ml-auto rounded-full bg-[#fff0c7] px-1.5 py-0.5 text-[9px] font-semibold text-[#a87800]">SD 2.5</span></button>
                  <button type="button" onClick={() => { setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Send className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_audio", { defaultValue: "Audio" })}</button>
                  <button type="button" onClick={() => { setItemType("document"); setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><FileText className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_script", { defaultValue: "Script" })}<ChevronDown className="ml-auto h-3 w-3" aria-hidden /></button>
                  <button type="button" onClick={() => { setAddMenuOpen(false); setLibraryOpen(true); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Package className="h-3.5 w-3.5" aria-hidden />{t("creative_board_add_asset_library", { defaultValue: "Asset library" })}<ChevronDown className="ml-auto h-3 w-3" aria-hidden /></button>
                </div>
                <div className="my-2 border-t border-[#edf0f4]" />
                <div className="px-2 pb-1 text-[10px] font-medium text-[#94a3b8]">{t("creative_board_add_resource_title", { defaultValue: "Add resource" })}</div>
                <button type="button" onClick={() => { setAddMenuOpen(false); setLeftOpen(true); setLeftTab("assets"); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Upload className="h-3.5 w-3.5" aria-hidden />{t("creative_board_upload", { defaultValue: "Upload" })}</button>
                <button type="button" onClick={() => setAddMenuOpen(false)} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><History className="h-3.5 w-3.5" aria-hidden />{t("creative_board_from_history", { defaultValue: "Choose from generation history" })}</button>
              </div>
            ) : null}
            </div>
            <div className="relative">
              <button type="button" onClick={() => setToolMenuOpen((open) => !open)} className={"focus-ring rounded-lg p-1.5 " + (toolMenuOpen || activeTool === "pan" ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t(activeTool === "select" ? "creative_board_select_tool" : "creative_board_move_canvas", { defaultValue: activeTool === "select" ? "Select tool" : "Move canvas" })} aria-expanded={toolMenuOpen} title={t(activeTool === "select" ? "creative_board_select_tool" : "creative_board_move_canvas", { defaultValue: activeTool === "select" ? "Select tool" : "Move canvas" })}>{activeTool === "pan" ? <Move className="h-3.5 w-3.5" aria-hidden /> : <MousePointer2 className="h-3.5 w-3.5" aria-hidden />}</button>
              {toolMenuOpen ? <div className="absolute bottom-[calc(100%+8px)] left-1/2 z-40 w-40 -translate-x-1/2 rounded-xl border border-[#dce3ec] bg-white p-1.5 shadow-[0_12px_30px_rgba(50,63,82,0.18)]"><button type="button" onClick={() => { setActiveTool("select"); setToolMenuOpen(false); }} className="flex w-full items-center justify-between rounded-lg px-2.5 py-2 text-left text-[11px] text-[#475569]"><span className="inline-flex items-center gap-2"><MousePointer2 className="h-3.5 w-3.5" aria-hidden />{t("creative_board_select_tool", { defaultValue: "Select" })}</span><kbd>V</kbd></button><button type="button" onClick={() => { setActiveTool("pan"); setToolMenuOpen(false); }} className="flex w-full items-center justify-between rounded-lg px-2.5 py-2 text-left text-[11px] text-[#475569]"><span className="inline-flex items-center gap-2"><Move className="h-3.5 w-3.5" aria-hidden />{t("creative_board_move_canvas", { defaultValue: "Move canvas" })}</span><kbd>H</kbd></button></div> : null}
            </div>
            <button type="button" onClick={() => setConnectMode((value) => !value)} className={"focus-ring rounded-lg p-1.5 " + (connectMode ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_connections", { defaultValue: "Show node connections" })} title={t("creative_board_connections", { defaultValue: "Show node connections" })}><Link2 className="h-3.5 w-3.5" aria-hidden /></button>
            <div className="relative">
              <button type="button" onClick={() => { setLibraryOpen((open) => !open); setRoleLibraryOpen(false); }} className={"focus-ring rounded-lg p-1.5 " + (libraryOpen ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_asset_library", { defaultValue: "Asset library" })} aria-expanded={libraryOpen} title={t("creative_board_asset_library", { defaultValue: "Asset library" })}><FolderOpen className="h-3.5 w-3.5" aria-hidden /></button>
              {libraryOpen ? <div className="absolute bottom-[calc(100%+8px)] left-1/2 z-40 w-52 -translate-x-1/2 rounded-xl border border-[#dce3ec] bg-white p-2.5 shadow-[0_12px_30px_rgba(50,63,82,0.18)]"><div className="mb-2 px-2 text-[11px] font-semibold text-[#334155]">{t("creative_board_asset_library", { defaultValue: "Asset library" })}</div><button type="button" onClick={() => { setLeftOpen(true); setLeftTab("assets"); setLibraryOpen(false); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Package className="h-3.5 w-3.5" aria-hidden />{t("creative_board_style_library", { defaultValue: "Style library" })}<span className="ml-auto rounded-full bg-[#f1f3f5] px-1.5 py-0.5 text-[9px] text-[#7b8492]">NEW</span></button><button type="button" onClick={() => { setLeftOpen(true); setLeftTab("assets"); setLibraryOpen(false); }} className="mt-0.5 flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><Sparkles className="h-3.5 w-3.5" aria-hidden />{t("creative_board_effect_library", { defaultValue: "Effect library" })}<span className="ml-auto rounded-full bg-[#f1f3f5] px-1.5 py-0.5 text-[9px] text-[#7b8492]">NEW</span></button></div> : null}
            </div>
            <div className="relative">
              <button type="button" onClick={() => { setRoleLibraryOpen((open) => !open); setLibraryOpen(false); }} className={"focus-ring rounded-lg p-1.5 " + (roleLibraryOpen ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_role_library", { defaultValue: "Character library" })} aria-expanded={roleLibraryOpen} title={t("creative_board_role_library", { defaultValue: "Character library" })}><UserRound className="h-3.5 w-3.5" aria-hidden /></button>
              {roleLibraryOpen ? <div className="absolute bottom-[calc(100%+8px)] left-1/2 z-40 w-48 -translate-x-1/2 rounded-xl border border-[#dce3ec] bg-white p-2.5 shadow-[0_12px_30px_rgba(50,63,82,0.18)]"><div className="mb-2 px-2 text-[11px] font-semibold text-[#334155]">{t("creative_board_role_library", { defaultValue: "Character library" })}</div><button type="button" onClick={() => { setLeftOpen(true); setLeftTab("assets"); setAssetCategory("agent"); setAssetTypeFilter("character"); setRoleLibraryOpen(false); }} className="flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left text-[11px] text-[#475569] hover:bg-[#f8fafc]"><UserRound className="h-3.5 w-3.5" aria-hidden />{t("creative_board_role_assets", { defaultValue: "Characters" })}</button></div> : null}
            </div>
            <div className="relative">
              <button type="button" onClick={() => setHistoryOpen((open) => !open)} className={"focus-ring rounded-lg p-1.5 " + (historyOpen ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_history", { defaultValue: "History" })} aria-expanded={historyOpen} title={t("creative_board_history", { defaultValue: "History" })}><History className="h-3.5 w-3.5" aria-hidden /></button>
              {historyOpen ? <div className="absolute bottom-[calc(100%+8px)] right-0 z-40 w-56 rounded-xl border border-[#dce3ec] bg-white p-3 text-[11px] text-[#64748b] shadow-[0_12px_30px_rgba(50,63,82,0.18)]"><div className="font-semibold text-[#334155]">{t("creative_board_history", { defaultValue: "History" })}</div><div className="mt-1 leading-relaxed">{t("creative_board_history_hint", { defaultValue: "Save a version to restore or compare the canvas later." })}</div></div> : null}
            </div>
            <div className="relative">
              <button type="button" onClick={() => setShortcutsOpen(true)} className={"focus-ring rounded-lg p-1.5 " + (shortcutsOpen ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_shortcuts", { defaultValue: "Keyboard shortcuts" })} aria-expanded={shortcutsOpen} title={t("creative_board_shortcuts", { defaultValue: "Keyboard shortcuts" })}><Keyboard className="h-3.5 w-3.5" aria-hidden /></button>
            </div>
            <div className="relative">
              <button type="button" onClick={() => setHelpOpen((open) => !open)} className={"focus-ring rounded-lg p-1.5 " + (helpOpen ? "bg-[#f0edff] text-[#6254d9]" : "text-[#64748b] hover:bg-[#f5f7fa]")} aria-label={t("creative_board_help", { defaultValue: "Help" })} aria-expanded={helpOpen} title={t("creative_board_help", { defaultValue: "Help" })}><CircleHelp className="h-3.5 w-3.5" aria-hidden /></button>
              {helpOpen ? <div className="absolute bottom-[calc(100%+8px)] right-0 z-40 w-56 rounded-xl border border-[#dce3ec] bg-white p-3 text-[11px] text-[#64748b] shadow-[0_12px_30px_rgba(50,63,82,0.18)]"><div className="font-semibold text-[#334155]">{t("creative_board_help", { defaultValue: "Help" })}</div><div className="mt-1 leading-relaxed">{t("creative_board_help_hint", { defaultValue: "Drag nodes to arrange the canvas. Select two nodes to connect them." })}</div></div> : null}
            </div>
            <span className="mx-1 h-5 w-px bg-[#e2e8f0]" />
            <CanvasSaveStatus status={persistenceStatus} onSave={saveNow} onRetry={handleSaveRetry} conflict={conflict} />
            {board ? <CreativeBoardActions boardId={board.id} projectName={projectName} snapshot={persistenceSnapshot} saveStatus={persistenceStatus} ensureSaved={ensureSaved} onRestored={reloadBoard} onCopied={handleCopied} onError={setError} /> : null}
          </div>
        </div>
        </div></section>
      {shortcutsOpen ? <div className="fixed inset-0 z-[80] flex items-center justify-center bg-[#172033]/10 p-4" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) setShortcutsOpen(false); }}><div className="relative w-full max-w-4xl rounded-xl border border-[#dce3ec] bg-white px-6 py-5 shadow-[0_20px_60px_rgba(33,45,66,0.2)]" role="dialog" aria-modal="true" aria-label={t("creative_board_shortcuts", { defaultValue: "Keyboard shortcuts" })}><button type="button" onClick={() => setShortcutsOpen(false)} className="focus-ring absolute right-3 top-3 rounded-md px-2 py-1 text-lg leading-none text-[#64748b] hover:bg-[#f5f7fa]" aria-label={t("creative_board_action_cancel", { defaultValue: "Close" })}>×</button><div className="grid gap-6 md:grid-cols-4">{SHORTCUT_GROUPS.map((group) => <section key={group.titleKey} className="min-w-0"><h2 className="mb-4 text-[12px] font-semibold text-[#12a8c0]">{t(group.titleKey, { defaultValue: group.titleDefault })}</h2><div className="space-y-3">{group.rows.map((row) => <div key={row.labelKey} className="flex items-center justify-between gap-3 text-[10px] text-[#5f6b7a]"><span className="min-w-0 truncate">{t(row.labelKey, { defaultValue: row.labelDefault })}</span><span className="flex shrink-0 items-center gap-1">{row.keys.map((key, index) => <span key={key + index} className={key === "+" ? "px-0.5 text-[#8a96a7]" : "rounded border border-[#dfe5ed] bg-[#fafbfe] px-1.5 py-0.5 font-mono text-[9px] text-[#64748b]"}>{key}</span>)}</span></div>)}</div></section>)}</div></div></div> : null}
    </div>
  </div>;
}
