/**
 * API 调用封装 (TypeScript)
 *
 * Typed API layer for all backend endpoints.
 * Import: import { API } from '@/api';
 */

import type {
  ProjectData,
  ProjectSummary,
  ImportConflictPolicy,
  ImportPreflightResponse,
  ImportProjectResponse,
  ExportDiagnostics,
  ImportFailureDiagnostics,
  EpisodeScript,
  TaskItem,
  TaskStats,
  SessionMeta,
  EntriesResponse,
  TimelineEntry,
  FailureObservation,
  SkillInfo,
  ProjectOverview,
  ProjectChangeBatchPayload,
  ProjectEventSnapshotPayload,
  ProjectDeletedPayload,
  GetSystemConfigResponse,
  GetSystemVersionResponse,
  ModelCandidatesResponse,
  OnboardingStatus,
  SystemConfigPatch,
  ApiKeyInfo,
  CreateApiKeyResponse,
  ProviderInfo,
  ProviderConfigDetail,
  ProviderTestResult,
  ProviderCredential,
  UsageStatsResponse,
  CustomProviderInfo,
  CustomProviderModelInfo,
  CustomProviderCreateRequest,
  CustomProviderFullUpdateRequest,
  CustomProviderModelInput,
  DiscoveredModel,
  EndpointDescriptor,
  CustomProviderCredentials,
  AnthropicDiscoverRequest,
  AnthropicDiscoverResponse,
  CostEstimateResponse,
  ReferenceVideoUnit,
  ReferenceResource,
  TransitionType,
  AdShot,
  AdReferenceUnit,
  ReferenceDurationPrecheck,
  ScriptPreview,
  ScriptReviewState,
  DramaNormalizedScript,
  NarrationStep1Draft,
  ReferenceStep1Draft,
  VideoCapabilities,
  WorkflowEdgeInput,
  WorkflowEvent,
  WorkflowExport,
  WorkflowDefinitionDetail,
  WorkflowDefinitionSummary,
  WorkflowNodeInput,
  WorkflowNodeLogEntry,
  WorkflowReviewItem,
  WorkflowRevisionSummary,
  WorkflowTemplateCatalogItem,
  WorkflowTemplateUpgrade,
  WorkflowRunDetail,
  WorkflowRunSummary,
  CharacterRelationEdge,
  CharacterRelationPosition,
  CharacterRelationsData,
  CharacterVariant,
  CreateMemoryRequest,
  MemoryCandidate,
  MemoryEntry,
  MemoryExport,
  MemoryListResponse,
  UpdateMemoryRequest,
  AssemblyPlanListResponse,
  AssemblyPlanResponse,
  CreateAssemblyPlanRequest,
  CreateAssemblyPlanRevisionRequest,
  ConfirmAssemblyPlanPreviewRequest,
  ConfirmAssemblyPlanPreviewResponse,
  TransitionAssemblyPlanRequest,
  CheckAssemblyPlanStaleRequest,
  AssemblyRenderJob,
  CreateAssemblyFinalRenderRequest,
  CreateAssemblyPreviewRenderRequest,
  AssemblyFinalReview,
  AssemblyFinalReviewFramePosition,
  ConfirmAssemblyFinalReviewRequest,
  ConfirmAssemblyFinalReviewResponse,
} from "@/types";
import type { GenerationRoute } from "@/utils/generation-mode";
import type { GridCapability, GridGeneration } from "@/types/grid";
import type {
  PromptRegistryResponse,
  PromptTemplateDetailResponse,
} from "@/types/prompt-registry";
import type { Asset, AssetType, AssetCreatePayload, AssetUpdatePayload } from "@/types/asset";
import type {
  CreateDurableBatchRequest,
  DurableBatchResponse,
} from "@/types/batch";
import type {
  AgentCredential,
  CreateAgentCredentialRequest,
  PresetProvidersResponse,
  TestConnectionRequest,
  TestConnectionResponse,
  UpdateAgentCredentialRequest,
} from "@/types/agent-credential";
import { getToken, clearToken } from "@/utils/auth";
import { isDemoProject } from "@/onboarding/demo-project";
import i18n from "./i18n";
import { openAuthenticatedSse, type SseConnection } from "@/utils/sse";

// ==================== Helper types ====================

/** 项目内四类资产（与后端 ASSET_SPECS 的 asset_type 对齐）。 */
export type ProjectAssetType = "character" | "scene" | "prop" | "product";

export interface CreativeBoardViewport {
  x?: number;
  y?: number;
  zoom?: number;
}

export interface CreativeBoardItemRecord {
  id: string;
  item_type: string;
  resource_type: string;
  resource_id: string;
  position: { x: number; y: number };
  size: { width: number; height: number };
  group_id?: string | null;
  display_settings?: Record<string, unknown>;
}

export interface CreativeBoardEdgeRecord {
  id: string;
  source_item_id: string;
  target_item_id: string;
  relation: string;
}

export interface CreativeBoardRecord {
  id: string;
  project_id: string;
  name: string;
  viewport: CreativeBoardViewport;
  items: CreativeBoardItemRecord[];
  edges: CreativeBoardEdgeRecord[];
  revision: number;
}

export interface CreativeBoardMutationResponse {
  revision: number;
  board?: CreativeBoardRecord;
  item?: CreativeBoardItemRecord;
  edge?: CreativeBoardEdgeRecord;
  [key: string]: unknown;
}

export function isCreativeBoardRevisionConflict(error: unknown): boolean {
  if (!error || typeof error !== "object") return false;
  const value = error as { detail?: unknown };
  // 只认 revision conflict 本身：409 还承载其它域错误（如 creative_context 的
  // creative_board_not_found），裸按状态码判定会把它们误弹成「同名文件已存在」。
  if (value.detail && typeof value.detail === "object") {
    const code = (value.detail as Record<string, unknown>).code;
    if (code === "creative_board_revision_conflict") return true;
  }
  const detail = typeof value.detail === "string" ? value.detail : JSON.stringify(value.detail ?? "");
  const message = error instanceof Error ? error.message : "";
  return detail.includes("creative_board_revision_conflict") || detail.includes("revision_conflict") || message.includes("creative_board_revision_conflict") || message.includes("revision conflict");
}

export function getCreativeBoardConflictRevision(error: unknown): number | undefined {
  if (!error || typeof error !== "object") return undefined;
  const value = error as { current_revision?: unknown; currentRevision?: unknown; revision?: unknown; detail?: unknown; response?: unknown; data?: unknown };
  for (const candidate of [value.current_revision, value.currentRevision, value.revision]) {
    if (typeof candidate === "number") return candidate;
  }
  for (const nested of [value.detail, value.response, value.data]) {
    const revision = getCreativeBoardConflictRevision(nested);
    if (revision !== undefined) return revision;
  }
  return undefined;
}

/** asset_type → REST 路径段（与后端 spec.subdir 对齐）。 */
const ASSET_TYPE_PATH: Record<ProjectAssetType, string> = {
  character: "characters",
  scene: "scenes",
  prop: "props",
  product: "products",
};

/** 资产级联重命名的影响报告（dry_run 预览与执行同一结构）。 */
export interface AssetRenameResult {
  success: boolean;
  dry_run: boolean;
  old_name: string;
  new_name: string;
  episodes: number;
  references: number;
  files: number;
}

/** Login response from POST /auth/token (mirrors backend TokenResponse). */
export interface LoginResponse {
  access_token: string;
  token_type: string;
  /** 登录用户的角色（admin/user）；旧后端可能不返回，默认按 admin 处理。 */
  role?: string;
}

/** Structured domain-error detail (e.g. `{"code": "preview_requires_confirmed_plan", "status": "draft"}`). */
export interface StructuredErrorDetail {
  code?: string;
  status?: string;
  message?: string;
}

/** Standard error response body from backend (mirrors FastAPI HTTPException detail). */
export interface ErrorResponse {
  detail: string | { msg?: string }[] | AgentFailureDetail | StructuredErrorDetail;
}

/** Structured detail returned when the local Agent process cannot start. */
export interface AgentFailureDetail {
  code: "agent_startup_failed";
  message: string;
  failure: FailureObservation;
}

/** Keeps the redacted failure observation attached while remaining a normal Error. */
export class AgentFailureError extends Error {
  readonly code = "agent_startup_failed" as const;

  constructor(
    message: string,
    public readonly failure: FailureObservation,
  ) {
    super(message);
    this.name = "AgentFailureError";
  }
}

/**
 * Error thrown when uploading a source file conflicts with an existing file
 * (HTTP 409). Carries the existing filename and a server-suggested alternative
 * so callers can prompt the user to retry with `on_conflict=rename|replace`.
 */
export class ConflictError extends Error {
  constructor(
    public readonly existing: string,
    public readonly suggestedName: string,
    message: string
  ) {
    super(message);
    this.name = "ConflictError";
  }
}

/** Error payload from the import project endpoint (extends ErrorResponse with import-specific fields). */
interface ImportErrorPayload {
  detail?: string | { msg?: string }[];
  errors?: string[];
  warnings?: string[];
  conflict_project_name?: string;
  diagnostics?: unknown;
}

/** Version metadata returned by the versions API. */
export interface VersionInfo {
  version: number;
  filename: string;
  created_at: string;
  file_size: number;
  is_current: boolean;
  file_url?: string;
  prompt?: string;
  restored_from?: number;
  /** 版本来源标记；"manual_upload" 表示用户手动上传 */
  source?: string;
}

/** 镜头/单元媒体上传的统一响应。 */
export interface ShotUploadResult {
  success: boolean;
  path: string;
  version: number;
  asset_fingerprints: Record<string, number>;
}

export interface ProjectEventStreamOptions {
  projectName: string;
  onSnapshot?: (payload: ProjectEventSnapshotPayload, event: MessageEvent) => void;
  onChanges?: (payload: ProjectChangeBatchPayload, event: MessageEvent) => void;
  /** 项目目录被删除后收到一次，随后流正常结束（浏览器会紧接着触发一次 onError）。 */
  onProjectDeleted?: (payload: ProjectDeletedPayload, event: MessageEvent) => void;
  onError?: (event: Event) => void;
}

/** Filters for {@link API.listTasks} and {@link API.listProjectTasks}. */
export interface TaskListFilters {
  projectName?: string;
  status?: string;
  taskType?: string;
  source?: string;
  page?: number;
  pageSize?: number;
}

/** Filters for {@link API.getUsageStats} and {@link API.getUsageCalls}. */
export interface UsageStatsFilters {
  projectName?: string;
  startDate?: string;
  endDate?: string;
}

export interface UsageCallsFilters {
  projectName?: string;
  callType?: string;
  status?: string;
  startDate?: string;
  endDate?: string;
  page?: number;
  pageSize?: number;
}

/** Generic success response used by many endpoints. */
export interface SuccessResponse {
  success: boolean;
  message?: string;
}

export type EpisodeDurationStrategy = "equal" | "proportional" | "manual";

export interface EpisodeDurationPlanInput {
  target_seconds: number;
  strategy: EpisodeDurationStrategy;
  manual_allocations?: Record<string, number>;
}

export interface EpisodeDurationItem {
  resource_id: string;
  duration_seconds: number | null;
  locked: boolean;
  generated: boolean;
}

export interface EpisodeDurationState {
  revision: string;
  plan: EpisodeDurationPlanInput | null;
  items: EpisodeDurationItem[];
}

export interface EpisodeDurationChange {
  resource_id: string;
  from_seconds: number | null;
  to_seconds: number;
  clamp_reason: string | null;
}

export interface EpisodeDurationPreview {
  revision: string;
  plan: EpisodeDurationPlanInput;
  target_seconds: number;
  changes: EpisodeDurationChange[];
}

/** 说书模式片段 PATCH 入参（drama 模式片段走 {@link API.updateScene}）。 */
export interface SegmentUpdatePayload {
  script_file: string;
  duration_seconds?: number;
  segment_break?: boolean;
  image_prompt?: unknown;
  video_prompt?: unknown;
  transition_to_next?: string;
  note?: string;
  characters_in_segment?: string[];
  scenes?: string[];
  props?: string[];
}

/** Payload for {@link API.createProject}. */
export interface CreateProjectPayload {
  title: string;
  name?: string;
  content_mode?: "narration" | "drama" | "ad";
  /** 源文件性质：novel（默认）/ screenplay。仅 drama 暴露，创建即定、不可变。 */
  source_kind?: "novel" | "screenplay";
  aspect_ratio?: "9:16" | "16:9";
  /** 生成路线，创建时必填二选一、无默认值（后端缺失即 422）。 */
  generation_mode: GenerationRoute;
  /** 分镜板（宫格）装配开关，可随创建写入；仅分镜路线有意义。 */
  grid_storyboard?: boolean;
  /** 口播语速估算（阅读单位 / 秒）；留空即按项目语言的默认速度估算。 */
  speech_rate_units_per_second?: number | null;
  default_duration?: number | null;
  /** 仅 ad：目标总时长（秒），UI 四档 15/30/60/90。 */
  target_duration?: number;
  /** 仅 ad：创作诉求短文本（可空）。 */
  brief?: string | null;
  style_template_id?: string | null;
  video_backend?: string | null;
  image_backend?: string | null;
  /** 项目默认图片模型。创建向导只暴露默认层（docs/adr/0054），能力桶留给项目设置页。 */
  default_image_backend?: string | null;
  text_backend_simple?: string | null;
  text_backend_complex?: string | null;
  default_text_backend?: string | null;
  model_settings?: Record<string, { resolution?: string | null }>;
}

export type CreativeDraftOperation = "generate" | "continue" | "expand" | "rewrite" | "polish" | "outline" | "split";

export interface CreativeDraftResult {
  operation: CreativeDraftOperation;
  content: string;
  provider: string;
  model: string;
}

function normalizeDiagnosticsBucket(value: unknown): { code: string; message: string; location?: string }[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value
    .filter(
      (item): item is { code: string; message: string; location?: string } =>
        Boolean(item)
        && typeof item === "object"
        && typeof (item as { code?: unknown }).code === "string"
        && typeof (item as { message?: unknown }).message === "string"
    )
    .map((item) => ({
      code: item.code,
      message: item.message,
      ...(typeof item.location === "string" ? { location: item.location } : {}),
    }));
}

function normalizeImportFailureDiagnostics(value: unknown): ImportFailureDiagnostics {
  const payload = (value && typeof value === "object") ? value as Record<string, unknown> : {};
  return {
    blocking: normalizeDiagnosticsBucket(payload.blocking),
    auto_fixable: normalizeDiagnosticsBucket(payload.auto_fixable),
    warnings: normalizeDiagnosticsBucket(payload.warnings),
  };
}

function normalizeExportDiagnostics(value: unknown): ExportDiagnostics {
  const payload = (value && typeof value === "object") ? value as Record<string, unknown> : {};
  return {
    blocking: normalizeDiagnosticsBucket(payload.blocking),
    auto_fixed: normalizeDiagnosticsBucket(payload.auto_fixed),
    warnings: normalizeDiagnosticsBucket(payload.warnings),
  };
}

// ==================== API class ====================

const API_BASE = "/api/v1";

class ApiHttpError extends Error {
  readonly status: number;
  readonly detail?: unknown;

  constructor(status: number, message: string, detail?: unknown) {
    super(message);
    this.name = "ApiHttpError";
    this.status = status;
    this.detail = detail;
  }
}

/**
 * 检查 fetch 响应状态，抛出包含后端错误信息的 Error。
 * 用于不经过 API.request() 的自定义 fetch 调用。
 */
async function throwIfNotOk(response: Response, fallbackMsg: string): Promise<void> {
  if (!response.ok) {
    handleUnauthorized(response);
    const error = await response
      .json()
      .catch(() => ({ detail: response.statusText })) as ErrorResponse;
    throw new ApiHttpError(response.status, errorDetailMessage(error.detail, fallbackMsg), error.detail);
  }
}

/**
 * 把后端错误 detail 统一抽成可读文案。
 *
 * 域错误（成片计划闸门、渲染冲突等）返回的是结构化对象，例如
 * `{"code": "preview_requires_confirmed_plan", "status": "draft"}`；
 * 只处理字符串 detail 会把这类原因吞成兜底文案。
 */
function errorDetailMessage(detail: unknown, fallback: string): string {
  if (typeof detail === "string" && detail) return localizeWorkflowError(detail);
  if (Array.isArray(detail)) {
    const joined = detail
      .map((item) => (typeof item === "string" ? item : (item as { msg?: string })?.msg))
      .filter(Boolean)
      .join("; ");
    return joined || fallback;
  }
  if (detail && typeof detail === "object") {
    const record = detail as Record<string, unknown>;
    for (const candidate of [record.message, record.code, record.detail]) {
      if (typeof candidate === "string" && candidate) return localizeWorkflowError(candidate);
    }
  }
  return fallback;
}

/**
 * 域错误码 → 翻译 key。后端在 409/422 的 detail 里回的是机器可读 code（预览闸门、渲染冲突等），
 * 直接透出会把内部英文串展示给用户；这里统一映射到 zh/en/vi 文案。未登记的 code 原样返回，
 * 方便排查未覆盖的分支。
 */
const WORKFLOW_ERROR_KEYS: Record<string, string> = {
  workflowtemplatenotpublished: "flow_template_not_published",
  previewrequiresconfirmedplan: "assembly_preview_requires_confirmed_plan",
  previewjobalreadyrunning: "assembly_preview_job_already_running",
  previewrevisionrequired: "assembly_preview_revision_required",
  currentrevisionmissing: "assembly_preview_revision_required",
  previewconfirmationrequired: "assembly_preview_confirmation_required_for_render",
  renderconfirmationrequired: "assembly_render_confirmation_required_for_render",
  finalrenderpendingrequired: "assembly_render_pending_required",
  finaljobalreadyrunning: "assembly_final_job_already_running",
  revisionconflict: "assembly_revision_conflict",
  sourcefingerprintconflict: "assembly_source_changed",
  finalrenderstatusconflict: "assembly_revision_conflict",
  retryrequiresfailedjob: "assembly_revision_conflict",
  retrylimitreached: "assembly_revision_conflict",
  previewretrynotallowed: "assembly_preview_revision_required",
  finalretryrequiresfinaljob: "assembly_revision_conflict",
  artifactpathinvalid: "assembly_source_changed",
  sourcefingerprintmissing: "assembly_source_changed",
};

function localizeWorkflowError(message: string): string {
  const normalized = message.toLowerCase().replace(/[^a-z0-9]/g, "");
  const key = WORKFLOW_ERROR_KEYS[normalized];
  return key ? i18n.t(key, { ns: "dashboard" }) : message;
}

export interface CreativeBoardVersionRecord {
  id: string;
  board_id: string;
  version_number: number;
  version_name: string;
  created_at: string;
  items_count?: number;
  edges_count?: number;
}

export interface CreativeBoardSnapshotPayload {
  board_id: string;
  name: string;
  viewport: { x: number; y: number; zoom: number };
  items: Array<Record<string, unknown>>;
  edges: Array<Record<string, unknown>>;
  revision: number;
}

export interface CreativeBoardVersionResponse {
  id: string;
  board_id: string;
  version_number: number;
  version_name: string;
  created_at: string;
  snapshot?: CreativeBoardSnapshotPayload;
}

export interface CreativeBoardRestoreResponse {
  board: Record<string, unknown>;
  revision: number;
}

export interface CreativeBoardDuplicateResponse {
  id: string;
  board?: Record<string, unknown>;
}

function handleUnauthorized(response: Response): void {
  if (response.status !== 401) return;

  clearToken();
  // 携带当前所在的站内地址，登录成功后回跳；仅对 /app/ 下的页面附加 from，
  // 避免把登录页自身等非应用路径写进回跳参数。
  const current = `${globalThis.location.pathname}${globalThis.location.search}${globalThis.location.hash}`;
  globalThis.location.href = current.startsWith("/app/")
    ? `/login?from=${encodeURIComponent(current)}`
    : "/login";
  throw new Error("认证已过期，请重新登录");
}

function isAgentFailureDetail(value: unknown): value is AgentFailureDetail {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const detail = value as Record<string, unknown>;
  return (
    detail.code === "agent_startup_failed"
    && typeof detail.message === "string"
    && Boolean(detail.failure)
    && typeof detail.failure === "object"
    && !Array.isArray(detail.failure)
  );
}

/** 为 fetch options 注入 Authorization header */
let apiReadOnly = false;

/**
 * 进入 / 离开只读态（引导演示工作台）。只读期间任何非 GET / HEAD 请求会在发出前被拒绝。
 *
 * 演示工作台是用真组件渲染假数据，写操作的入口都已经不渲染；这道闸门是结构性兜底 ——
 * 漏掉一个入口时会得到一个明确的异常，而不是一条真写进用户项目的请求。
 */
export function setApiReadOnly(readOnly: boolean): void {
  apiReadOnly = readOnly;
}

export class ReadOnlyModeError extends Error {
  constructor(method: string) {
    super(`Blocked ${method} request: the workspace is in read-only demo mode`);
    this.name = "ReadOnlyModeError";
  }
}

// 静态归档导入端点，不是「项目名恰好叫 import」——项目名允许字母数字中划线，
// `import` 本身是合法项目名（ProjectManager.normalize_project_name 不排除它），
// 按精确路径匹配而非按名称黑名单，避免把 `/projects/import/...`（真实项目名为
// import 的写请求）一并误判成不带项目归属
const RESERVED_PROJECT_ENDPOINTS = new Set(["/projects/import"]);

// 不带项目归属、但本身不写入任何项目数据的系统级端点：闸门默认拦截所有无项目归属的
// 写请求（全局资产库、供应商凭证等），这里是唯一的窄豁免。引导 tour 退出时会在仍处于
// 演示路由（apiReadOnly 尚未复位）期间写这一条「已看过」标记，它只影响当前用户的引导
// 状态，不属于闸门要防的「误写演示态/其他项目数据」范畴。
const READ_ONLY_GATE_EXEMPT_ENDPOINTS = new Set(["/onboarding/seen"]);

/** 从形如 `/projects/{name}` 或 `/projects/{name}/...` 的 endpoint 中取出项目名；非项目路径或静态保留端点返回 null */
function extractProjectName(endpoint: string): string | null {
  if (RESERVED_PROJECT_ENDPOINTS.has(endpoint)) return null;
  const match = /^\/projects\/([^/?]+)/.exec(endpoint);
  if (!match) return null;
  return decodeURIComponent(match[1]);
}

/**
 * 只读闸门是否应拦截这次请求。指向某个具体真实项目的写请求放行 ——
 * 演示态可能在该请求发出前才切入，但它拦不住已经从真实项目发起的操作；
 * 指向演示项目本身或不带项目归属的请求（全局资产库等）仍按闸门原意拦截，
 * 窄豁免名单中的系统端点除外。
 */
function isReadOnlyGateBlocking(endpoint: string): boolean {
  if (!apiReadOnly) return false;
  if (READ_ONLY_GATE_EXEMPT_ENDPOINTS.has(endpoint)) return false;
  const projectName = extractProjectName(endpoint);
  return projectName === null || isDemoProject(projectName);
}

function withAuth(endpoint: string, options: RequestInit = {}): RequestInit {
  const method = (options.method ?? "GET").toUpperCase();
  if (method !== "GET" && method !== "HEAD" && isReadOnlyGateBlocking(endpoint)) {
    throw new ReadOnlyModeError(method);
  }
  const token = getToken();
  const headers = new Headers(options.headers);
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  // Add Accept-Language header based on current i18n language
  headers.set("Accept-Language", i18n.language || "zh");
  return { ...options, headers };
}


class API {
  /**
   * 通用请求方法
   */
  static async request<T = unknown>(
    endpoint: string,
    options: RequestInit = {}
  ): Promise<T> {
    const url = `${API_BASE}${endpoint}`;
    const defaultOptions: RequestInit = {
      headers: {
        "Content-Type": "application/json",
      },
    };

    const response = await fetch(url, withAuth(endpoint, { ...defaultOptions, ...options }));

    if (!response.ok) {
      handleUnauthorized(response);
      const error = await response
        .json()
        .catch(() => ({ detail: response.statusText })) as ErrorResponse;
      if (isAgentFailureDetail(error.detail)) {
        throw new AgentFailureError(error.detail.message, error.detail.failure);
      }
      throw new ApiHttpError(response.status, errorDetailMessage(error.detail, "请求失败"), error.detail);
    }

    if (response.status === 204) {
      return undefined as T;
    }
    return response.json() as Promise<T>;
  }

  // ==================== 系统配置 ====================

  static async getSystemConfig(): Promise<GetSystemConfigResponse> {
    return this.request("/system/config");
  }

  /**
   * 能力桶下拉的候选数据源（docs/adr/0054）：默认层全量 + 每个桶按能力过滤后的模型列表。
   * 与 getSystemConfig 的 options 同口径（同样剔除 hidden 模型），过滤只加在桶层。
   */
  static async getModelCandidates(
    options: { signal?: AbortSignal } = {}
  ): Promise<ModelCandidatesResponse> {
    return this.request("/system/config/model-candidates", { signal: options.signal });
  }

  static async getSystemVersion(): Promise<GetSystemVersionResponse> {
    return this.request("/system/version");
  }

  // ==================== 提示词模板注册表 ====================

  /** 全部提示词模板的元数据（不含正文），顺序即设置页展示顺序。 */
  static async getPromptRegistry(): Promise<PromptRegistryResponse> {
    return this.request("/prompts/registry");
  }

  /** 单条提示词模板的元数据与正文；path 是相对 profile 根目录的 POSIX 路径。 */
  static async getPromptTemplate(path: string): Promise<PromptTemplateDetailResponse> {
    const encoded = path.split("/").map(encodeURIComponent).join("/");
    return this.request(`/prompts/registry/${encoded}`);
  }

  // ==================== 首次使用引导 ====================

  static async getOnboardingStatus(
    options: { signal?: AbortSignal } = {}
  ): Promise<OnboardingStatus> {
    return this.request("/onboarding/status", { signal: options.signal });
  }

  static async markOnboardingSeen(): Promise<OnboardingStatus> {
    return this.request("/onboarding/seen", { method: "POST" });
  }

  static async downloadDiagnostics(): Promise<{ blob: Blob; filename: string }> {
    const response = await fetch(
      `${API_BASE}/system/logs/download`,
      withAuth("/system/logs/download", { method: "GET" }),
    );
    await throwIfNotOk(response, `HTTP ${response.status}`);
    const disposition = response.headers.get("Content-Disposition") ?? "";
    const match = disposition.match(/filename="?([^";]+)"?/);
    const filename = match?.[1] ?? "shotwise-diagnostics.zip";
    const blob = await response.blob();
    return { blob, filename };
  }

  static async updateSystemConfig(
    patch: SystemConfigPatch,
  ): Promise<GetSystemConfigResponse> {
    return this.request("/system/config", {
      method: "PATCH",
      body: JSON.stringify(patch),
    });
  }


  // ==================== 项目管理 ====================

  static async listProjects(): Promise<{ projects: ProjectSummary[] }> {
    return this.request("/projects");
  }

  static async createProject(
    payload: CreateProjectPayload,
  ): Promise<{ success: boolean; name: string; project: ProjectData }> {
    return this.request("/projects", {
      method: "POST",
      body: JSON.stringify(payload),
    });
  }

  static async getProject(
    name: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<{
    project: ProjectData;
    scripts: Record<string, EpisodeScript>;
    asset_fingerprints?: Record<string, number>;
  }> {
    return this.request(`/projects/${encodeURIComponent(name)}`, { signal: options.signal });
  }

  static async updateProject(
    name: string,
    updates: Partial<ProjectData> & { clear_style_image?: boolean }
  ): Promise<{ success: boolean; project: ProjectData }> {
    if ("content_mode" in updates) {
      throw new Error("项目创建后不支持修改 content_mode");
    }
    return this.request(`/projects/${encodeURIComponent(name)}`, {
      method: "PATCH",
      body: JSON.stringify(updates),
    });
  }

  static async deleteProject(name: string): Promise<SuccessResponse> {
    return this.request(`/projects/${encodeURIComponent(name)}`, {
      method: "DELETE",
    });
  }

  /**
   * 三级解析（项目 > 系统设置 > 系统默认）后的视频模型能力。
   *
   * `videoBackend`（"provider/model"）用于设置表单里尚未保存的候选模型：不传按已落盘配置
   * 解析，传了则按该候选模型 × 本项目生效 generation_mode 解析。
   *
   * `episode` 用于按集查看的界面：生成模式可被单集覆盖，传集号则能力按该集生效模式解析，
   * 与执行层同口径；不传只解析到项目级（设置页等无集号上下文的调用）。
   */
  static async getVideoCapabilities(
    name: string,
    options: { signal?: AbortSignal; videoBackend?: string; episode?: number } = {}
  ): Promise<VideoCapabilities> {
    const params = new URLSearchParams();
    if (options.videoBackend) params.set("video_backend", options.videoBackend);
    if (options.episode !== undefined) params.set("episode", String(options.episode));
    const qs = params.size > 0 ? `?${params.toString()}` : "";
    return this.request(`/projects/${encodeURIComponent(name)}/video-capabilities${qs}`, {
      signal: options.signal,
    });
  }

  static async requestExportToken(
    projectName: string,
    scope: "full" | "current" = "full"
  ): Promise<{ download_token: string; expires_in: number; diagnostics: ExportDiagnostics }> {
    const payload = await this.request<{
      download_token: string;
      expires_in: number;
      diagnostics?: unknown;
    }>(
      `/projects/${encodeURIComponent(projectName)}/export/token?scope=${encodeURIComponent(scope)}`,
      {
        method: "POST",
      }
    );
    return {
      download_token: payload.download_token,
      expires_in: payload.expires_in,
      diagnostics: normalizeExportDiagnostics(payload.diagnostics),
    };
  }

  static getExportDownloadUrl(
    projectName: string,
    downloadToken: string,
    scope: "full" | "current" = "full"
  ): string {
    return `${API_BASE}/projects/${encodeURIComponent(projectName)}/export?download_token=${encodeURIComponent(downloadToken)}&scope=${encodeURIComponent(scope)}`;
  }

  /** 构造剪映草稿下载 URL */
  static getJianyingDraftDownloadUrl(
    projectName: string,
    episode: number,
    draftPath: string,
    downloadToken: string,
    jianyingVersion: string = "6",
  ): string {
    return `${API_BASE}/projects/${encodeURIComponent(projectName)}/export/jianying-draft?episode=${encodeURIComponent(episode)}&draft_path=${encodeURIComponent(draftPath)}&download_token=${encodeURIComponent(downloadToken)}&jianying_version=${encodeURIComponent(jianyingVersion)}`;
  }

  static async importProject(
    file: File,
    conflictPolicy: ImportConflictPolicy = "prompt"
  ): Promise<ImportProjectResponse> {
    const formData = new FormData();
    formData.append("file", file);
    formData.append("conflict_policy", conflictPolicy);

    const response = await fetch(
      `${API_BASE}/projects/import`,
      withAuth("/projects/import", {
        method: "POST",
        body: formData,
      })
    );

    if (!response.ok) {
      handleUnauthorized(response);

      const payload = await response
        .json()
        .catch(() => ({ detail: response.statusText, errors: [], warnings: [] })) as ImportErrorPayload;
      const error = new Error(
        typeof payload.detail === "string" ? payload.detail : "导入失败"
      ) as Error & {
        status?: number;
        detail?: string;
        errors?: string[];
        warnings?: string[];
        conflict_project_name?: string;
        diagnostics?: ImportFailureDiagnostics;
      };
      error.status = response.status;
      error.detail = typeof payload.detail === "string" ? payload.detail : "导入失败";
      error.errors = Array.isArray(payload.errors) ? payload.errors : [];
      error.warnings = Array.isArray(payload.warnings) ? payload.warnings : [];
      if (typeof payload.conflict_project_name === "string") {
        error.conflict_project_name = payload.conflict_project_name;
      }
      error.diagnostics = normalizeImportFailureDiagnostics(payload.diagnostics);
      throw error;
    }

    const payload = await response.json() as ImportProjectResponse & { diagnostics?: { auto_fixed?: unknown[]; warnings?: unknown[] } };
    return {
      ...payload,
      diagnostics: {
        auto_fixed: normalizeDiagnosticsBucket(payload?.diagnostics?.auto_fixed),
        warnings: normalizeDiagnosticsBucket(payload?.diagnostics?.warnings),
      },
    };
  }

  // ==================== 角色管理 ====================

  static async addCharacter(
    projectName: string,
    name: string,
    description: string,
    voiceStyle: string = ""
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters`,
      {
        method: "POST",
        body: JSON.stringify({
          name,
          description,
          voice_style: voiceStyle,
        }),
      }
    );
  }

  static async updateCharacter(
    projectName: string,
    charName: string,
    updates: Record<string, unknown>
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(charName)}`,
      {
        method: "PATCH",
        body: JSON.stringify(updates),
      }
    );
  }

  static async deleteCharacter(
    projectName: string,
    charName: string
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(charName)}`,
      {
        method: "DELETE",
      }
    );
  }

  static async listCharacterVariants(projectName: string, charName: string): Promise<{ variants: CharacterVariant[] }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(charName)}/variants`,
    );
  }

  static async createCharacterVariant(
    projectName: string,
    charName: string,
    variant: Omit<CharacterVariant, "id" | "character_id"> & { slug: string },
  ): Promise<{ success: boolean; variant: CharacterVariant }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(charName)}/variants`,
      { method: "POST", body: JSON.stringify(variant) },
    );
  }

  static async updateCharacterVariant(
    projectName: string,
    charName: string,
    variantId: string,
    updates: Partial<Omit<CharacterVariant, "id" | "character_id">>,
  ): Promise<{ success: boolean; variant: CharacterVariant }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(charName)}/variants/${encodeURIComponent(variantId)}`,
      { method: "PATCH", body: JSON.stringify(updates) },
    );
  }

  static async deleteCharacterVariant(
    projectName: string,
    charName: string,
    variantId: string,
  ): Promise<{ success: boolean; variant: CharacterVariant }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(charName)}/variants/${encodeURIComponent(variantId)}`,
      { method: "DELETE" },
    );
  }

  static async getCharacterRelations(
    projectName: string,
    options: { signal?: AbortSignal } = {},
  ): Promise<CharacterRelationsData> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/character-relations`,
      { signal: options.signal },
    );
  }

  static async analyzeCharacterRelations(projectName: string): Promise<CharacterRelationsData> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/character-relations/analyze`,
      { method: "POST" },
    );
  }

  static async saveCharacterRelations(
    projectName: string,
    baseRevision: number,
    edges: CharacterRelationEdge[],
    nodePositions: Record<string, CharacterRelationPosition>,
  ): Promise<CharacterRelationsData> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/character-relations`,
      {
        method: "PUT",
        body: JSON.stringify({ base_revision: baseRevision, edges, node_positions: nodePositions }),
      },
    );
  }

  // ==================== 项目场景管理 ====================

  static async addProjectScene(
    projectName: string,
    name: string,
    description: string
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/scenes`,
      {
        method: "POST",
        body: JSON.stringify({ name, description }),
      }
    );
  }

  static async updateProjectScene(
    projectName: string,
    sceneName: string,
    updates: Record<string, unknown>
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/scenes/${encodeURIComponent(sceneName)}`,
      {
        method: "PATCH",
        body: JSON.stringify(updates),
      }
    );
  }

  static async deleteProjectScene(
    projectName: string,
    sceneName: string
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/scenes/${encodeURIComponent(sceneName)}`,
      {
        method: "DELETE",
      }
    );
  }

  // ==================== 项目道具管理 ====================

  static async addProjectProp(
    projectName: string,
    name: string,
    description: string
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/props`,
      {
        method: "POST",
        body: JSON.stringify({ name, description }),
      }
    );
  }

  static async updateProjectProp(
    projectName: string,
    propName: string,
    updates: Record<string, unknown>
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/props/${encodeURIComponent(propName)}`,
      {
        method: "PATCH",
        body: JSON.stringify(updates),
      }
    );
  }

  static async deleteProjectProp(
    projectName: string,
    propName: string
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/props/${encodeURIComponent(propName)}`,
      {
        method: "DELETE",
      }
    );
  }

  // ==================== 项目产品管理 ====================

  static async addProjectProduct(
    projectName: string,
    name: string,
    description: string,
    brand?: string
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/products`,
      {
        method: "POST",
        body: JSON.stringify(brand ? { name, description, brand } : { name, description }),
      }
    );
  }

  static async updateProjectProduct(
    projectName: string,
    productName: string,
    updates: Record<string, unknown>
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/products/${encodeURIComponent(productName)}`,
      {
        method: "PATCH",
        body: JSON.stringify(updates),
      }
    );
  }

  static async deleteProjectProduct(
    projectName: string,
    productName: string
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/products/${encodeURIComponent(productName)}`,
      {
        method: "DELETE",
      }
    );
  }

  // ==================== 项目资产重命名 ====================

  /**
   * 级联重命名项目内资产。`dryRun: true` 只返回影响预览（将更新的集数/引用处数/文件数），
   * 预览与执行共用后端同一套扫描逻辑，确认框数字与实际执行一致。
   */
  static async renameProjectAsset(
    projectName: string,
    assetType: ProjectAssetType,
    name: string,
    newName: string,
    options: { dryRun?: boolean; signal?: AbortSignal } = {}
  ): Promise<AssetRenameResult> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/${ASSET_TYPE_PATH[assetType]}/${encodeURIComponent(name)}/rename`,
      {
        method: "POST",
        body: JSON.stringify({ new_name: newName, dry_run: options.dryRun ?? false }),
        signal: options.signal,
      }
    );
  }

  // ==================== 场景管理 ====================

  static async getScript(
    projectName: string,
    scriptFile: string
  ): Promise<EpisodeScript> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/scripts/${encodeURIComponent(scriptFile)}`
    );
  }

  static async updateScene(
    projectName: string,
    sceneId: string,
    scriptFile: string,
    updates: Record<string, unknown>
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/script-scenes/${encodeURIComponent(sceneId)}`,
      {
        method: "PATCH",
        body: JSON.stringify({ script_file: scriptFile, updates }),
      }
    );
  }

  /** 更新分集顶层元数据（当前仅 title）。以剧本顶层 title 为唯一真相源，后端会镜像到 project.json。 */
  static async updateEpisode(
    projectName: string,
    episode: number,
    updates: { title: string }
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}`,
      {
        method: "PATCH",
        body: JSON.stringify(updates),
      }
    );
  }

  static async deleteEpisode(projectName: string, episode: number): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}`,
      { method: "DELETE" },
    );
  }

  static async updateEpisodeDisplayNumber(
    projectName: string,
    episode: number,
    displayEpisode: number,
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/display-number`,
      {
        method: "PATCH",
        body: JSON.stringify({ display_episode: displayEpisode }),
      },
    );
  }

  static async getEpisodeDurationPlan(
    projectName: string,
    episode: number,
    options: { signal?: AbortSignal } = {},
  ): Promise<EpisodeDurationState> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/duration-plan`,
      { signal: options.signal },
    );
  }

  static async saveEpisodeDurationPlan(
    projectName: string,
    episode: number,
    revision: string,
    input: EpisodeDurationPlanInput,
  ): Promise<{ revision: string; plan: EpisodeDurationPlanInput }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/duration-plan`,
      { method: "PUT", body: JSON.stringify({ ...input, expected_revision: revision }) },
    );
  }

  static async previewEpisodeDurationPlan(
    projectName: string,
    episode: number,
    input: EpisodeDurationPlanInput,
  ): Promise<EpisodeDurationPreview> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/duration-plan/preview`,
      { method: "POST", body: JSON.stringify(input) },
    );
  }

  static async applyEpisodeDurationPlan(
    projectName: string,
    episode: number,
    revision: string,
    input: EpisodeDurationPlanInput,
  ): Promise<{ revision: string; plan: EpisodeDurationPlanInput; applied: Record<string, number>; skipped: string[] }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/duration-plan/apply`,
      { method: "POST", body: JSON.stringify({ ...input, expected_revision: revision }) },
    );
  }

  static async setEpisodeDurationLock(
    projectName: string,
    episode: number,
    resourceId: string,
    revision: string,
    locked: boolean,
  ): Promise<{ revision: string; resource_id: string; locked: boolean }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/duration-plan/items/${encodeURIComponent(resourceId)}/lock`,
      { method: "PATCH", body: JSON.stringify({ expected_revision: revision, locked }) },
    );
  }

  // ==================== step1→step2 审核 gate ====================

  /** 读取该集 step1 结构化中间态 + 审核状态（供 web 渲染与编辑）。 */
  static async getScriptReview(
    projectName: string,
    episode: number,
    options: { signal?: AbortSignal } = {}
  ): Promise<ScriptReviewState> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/script-review`,
      { signal: options.signal }
    );
  }

  /** 保存手动 / agent 编辑后的结构化中间态，返回最新状态（重新待审）。
   *
   * `baseFingerprint` 传 GET 时拿到的内容指纹：编辑期间 step1 被另一写入方（如 agent 晋升）
   * 改过时服务端 409 冲突、不落盘，避免静默覆盖对方的修改；不传则不比对。 */
  static async saveScriptReviewContent(
    projectName: string,
    episode: number,
    content: DramaNormalizedScript | NarrationStep1Draft | ReferenceStep1Draft,
    baseFingerprint?: string | null
  ): Promise<ScriptReviewState> {
    const query = baseFingerprint
      ? `?base_fingerprint=${encodeURIComponent(baseFingerprint)}`
      : "";
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/script-review/content${query}`,
      {
        method: "PUT",
        body: JSON.stringify(content),
      }
    );
  }

  /** 用户显式确认 step1 内容，放行 step2 视觉生成。 */
  static async confirmScriptReview(
    projectName: string,
    episode: number
  ): Promise<ScriptReviewState> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/episodes/${episode}/script-review/confirm`,
      { method: "POST" }
    );
  }

  // ==================== 片段管理（说书模式） ====================

  /** `updates` 字段形状参见 {@link SegmentUpdatePayload}；保留 Record 以兼容 spread 调用。 */
  static async updateSegment(
    projectName: string,
    segmentId: string,
    updates: Record<string, unknown>
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/segments/${encodeURIComponent(segmentId)}`,
      {
        method: "PATCH",
        body: JSON.stringify(updates),
      }
    );
  }

  // ==================== 镜头管理（广告/短片模式） ====================

  /** 更新 ad 剧本中的单个镜头（口播文案 / section / 时长 / 引用列表等白名单字段）。 */
  static async updateShot(
    projectName: string,
    shotId: string,
    scriptFile: string,
    updates: Record<string, unknown>
  ): Promise<SuccessResponse & { shot?: AdShot }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/script-shots/${encodeURIComponent(shotId)}`,
      {
        method: "PATCH",
        body: JSON.stringify({ script_file: scriptFile, updates }),
      }
    );
  }

  /** 按给定全排列重排 ad 剧本的镜头顺序。 */
  static async reorderShots(
    projectName: string,
    scriptFile: string,
    shotIds: string[]
  ): Promise<SuccessResponse & { shots?: AdShot[] }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/script-shots/reorder`,
      {
        method: "POST",
        body: JSON.stringify({ script_file: scriptFile, shot_ids: shotIds }),
      }
    );
  }

  // ==================== 文件管理 ====================

  static async uploadFile(
    projectName: string,
    uploadType: string,
    file: File,
    name: string | null = null,
    options: { onConflict?: "fail" | "replace" | "rename" } = {}
  ): Promise<{
    success: boolean;
    path: string;
    url: string;
    filename?: string;
    normalized?: boolean;
    original_kept?: boolean;
    original_filename?: string;
    used_encoding?: string | null;
    chapter_count?: number;
  }> {
    const formData = new FormData();
    formData.append("file", file);

    const qsParts: string[] = [];
    if (name) qsParts.push(`name=${encodeURIComponent(name)}`);
    if (uploadType === "source" && options.onConflict) {
      qsParts.push(`on_conflict=${encodeURIComponent(options.onConflict)}`);
    }
    const qs = qsParts.join("&");
    const url = `/projects/${encodeURIComponent(projectName)}/upload/${uploadType}${qs ? "?" + qs : ""}`;

    const response = await fetch(`${API_BASE}${url}`, withAuth(url, {
      method: "POST",
      body: formData,
    }));

    if (response.status === 409) {
      let detail: { existing?: string; suggested_name?: string; message?: string } | null = null;
      try {
        const body = (await response.json()) as { detail?: { existing?: string; suggested_name?: string; message?: string } };
        detail = body?.detail ?? null;
      } catch {
        /* ignore */
      }
      // 后端 SourceLoader 的 ConflictError 必然携带 existing + suggested_name；
      // 若 detail 缺字段则视为协议异常，抛通用错误（带文件名标识）而非手搓 fallback —
      // 避免前端"猜"一个可能与后端命名规则不一致的 suggested_name 误导用户
      if (!detail?.existing || !detail?.suggested_name) {
        throw new Error(`上传 "${file.name}" 失败：服务端返回 409 但 detail 字段不完整`);
      }
      throw new ConflictError(
        detail.existing,
        detail.suggested_name,
        detail.message ?? "conflict",
      );
    }

    await throwIfNotOk(response, "上传失败");
    return (await response.json()) as {
      success: boolean;
      path: string;
      url: string;
      filename?: string;
      normalized?: boolean;
      original_kept?: boolean;
      original_filename?: string;
      used_encoding?: string | null;
      chapter_count?: number;
    };
  }

  /** 单文件 multipart 上传 POST，返回 JSON 响应体。 */
  private static async postFileUpload<T>(url: string, file: File): Promise<T> {
    const formData = new FormData();
    formData.append("file", file);
    const response = await fetch(`${API_BASE}${url}`, withAuth(url, { method: "POST", body: formData }));
    await throwIfNotOk(response, "上传失败");
    return (await response.json()) as T;
  }

  static async uploadCharacterAvatar(
    projectName: string,
    charName: string,
    file: File,
  ): Promise<{ success: boolean; path: string }> {
    const url =
      "/projects/" + encodeURIComponent(projectName) +
      "/characters/" + encodeURIComponent(charName) + "/avatar";
    try {
      return await this.postFileUpload(url, file);
    } catch (error) {
      // Older development workers exposed this endpoint without /projects.
      // Retry only the generic route miss; a real project/character error must
      // continue to surface from the canonical endpoint.
      if (
        !(error instanceof ApiHttpError) ||
        error.status !== 404 ||
        error.message !== "Not Found"
      ) {
        throw error;
      }
      const legacyUrl =
        "/" + encodeURIComponent(projectName) +
        "/characters/" + encodeURIComponent(charName) + "/avatar";
      return this.postFileUpload(legacyUrl, file);
    }
  }

  /** 上传分镜图或镜头视频，替换该镜头的 AI 生成资产（storyboard/grid 模式）。 */
  static async uploadShotMedia(
    projectName: string,
    scriptFile: string,
    shotId: string,
    kind: "storyboard" | "video",
    file: File
  ): Promise<ShotUploadResult> {
    const url =
      `/projects/${encodeURIComponent(projectName)}/shots/${encodeURIComponent(shotId)}` +
      `/upload/${kind}?script_file=${encodeURIComponent(scriptFile)}`;
    return API.postFileUpload<ShotUploadResult>(url, file);
  }

  // ==================== 镜头尾帧 ====================
  //
  // 三个端点同一落点：设置走上传或项目内选图两条通道，都归一为 PNG 快照写到
  // end_frames/scene_{id}.png（原地覆盖），清除删快照并置空字段。返回的
  // end_frame_image 是项目内相对路径；换图后路径不变，靠资产指纹 cache-bust。

  /** 上传任意图片作为该镜头的尾帧。 */
  static async uploadEndFrame(
    projectName: string,
    shotId: string,
    scriptFile: string,
    file: File
  ): Promise<{ success: boolean; end_frame_image: string }> {
    const url =
      `/projects/${encodeURIComponent(projectName)}/shots/${encodeURIComponent(shotId)}` +
      `/end-frame/upload?script_file=${encodeURIComponent(scriptFile)}`;
    return API.postFileUpload<{ success: boolean; end_frame_image: string }>(url, file);
  }

  /** 选项目内已有图片作为该镜头的尾帧（快照复制，不建立引用）。 */
  static async selectEndFrame(
    projectName: string,
    shotId: string,
    scriptFile: string,
    sourcePath: string
  ): Promise<{ success: boolean; end_frame_image: string }> {
    const url =
      `/projects/${encodeURIComponent(projectName)}/shots/${encodeURIComponent(shotId)}` +
      `/end-frame/select`;
    return this.request(url, {
      method: "POST",
      body: JSON.stringify({ script_file: scriptFile, source_path: sourcePath }),
    });
  }

  /** 清除该镜头的尾帧。 */
  static async clearEndFrame(
    projectName: string,
    shotId: string,
    scriptFile: string
  ): Promise<SuccessResponse> {
    const url =
      `/projects/${encodeURIComponent(projectName)}/shots/${encodeURIComponent(shotId)}` +
      `/end-frame?script_file=${encodeURIComponent(scriptFile)}`;
    return this.request(url, { method: "DELETE" });
  }

  /** 上传参考生视频单元的成片视频。 */
  static async uploadReferenceUnitVideo(
    projectName: string,
    episode: number,
    unitId: string,
    file: File
  ): Promise<ShotUploadResult> {
    const url =
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}` +
      `/units/${encodeURIComponent(unitId)}/upload-video`;
    return API.postFileUpload<ShotUploadResult>(url, file);
  }

  static async listFiles(
    projectName: string
  ): Promise<{
    files: {
      source?: { name: string; size: number; url: string; raw_filename?: string | null }[];
      characters?: { name: string; size: number; url: string }[];
      scenes?: { name: string; size: number; url: string }[];
      props?: { name: string; size: number; url: string }[];
      storyboards?: { name: string; size: number; url: string }[];
      videos?: { name: string; size: number; url: string }[];
      output?: { name: string; size: number; url: string }[];
    };
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/files`
    );
  }

  static getFileUrl(
    projectName: string,
    path: string,
    cacheBust?: number | string | null
  ): string {
    // 引导演示的占位图是现算的内联 SVG（data: URI），直接用，不要再包一层项目路径。
    // 只放行 data: —— 目前没有第二种自带协议的图源，多放行的协议只是没人用的入口。
    if (path.startsWith("data:")) {
      return path;
    }
    const base = `${API_BASE}/files/${encodeURIComponent(projectName)}/${path}`;
    if (cacheBust == null || cacheBust === "") {
      return base;
    }

    return `${base}?v=${encodeURIComponent(String(cacheBust))}`;
  }

  // ==================== Source 文件管理 ====================

  /**
   * 获取 source 文件内容
   */
  static async getSourceContent(
    projectName: string,
    filename: string
  ): Promise<string> {
    const url = `/projects/${encodeURIComponent(projectName)}/source/${encodeURIComponent(filename)}`;
    const response = await fetch(
      `${API_BASE}${url}`,
      withAuth(url)
    );
    await throwIfNotOk(response, "获取文件内容失败");
    return response.text();
  }

  /**
   * 保存 source 文件（新建或更新）
   */
  static async saveSourceFile(
    projectName: string,
    filename: string,
    content: string
  ): Promise<SuccessResponse> {
    const url = `/projects/${encodeURIComponent(projectName)}/source/${encodeURIComponent(filename)}`;
    const response = await fetch(
      `${API_BASE}${url}`,
      withAuth(url, {
        method: "PUT",
        headers: { "Content-Type": "text/plain" },
        body: content,
      })
    );
    await throwIfNotOk(response, "保存文件失败");
    return response.json() as Promise<SuccessResponse>;
  }

  /**
   * 删除 source 文件
   */
  static async deleteSourceFile(
    projectName: string,
    filename: string
  ): Promise<SuccessResponse> {
    const url = `/projects/${encodeURIComponent(projectName)}/source/${encodeURIComponent(filename)}`;
    const response = await fetch(
      `${API_BASE}${url}`,
      withAuth(url, {
        method: "DELETE",
      })
    );
    await throwIfNotOk(response, "删除文件失败");
    return response.json() as Promise<SuccessResponse>;
  }

  /**
   * 删除角色参考音频样本（清空字段并移除文件）
   */
  static async deleteCharacterReferenceAudio(
    projectName: string,
    characterName: string
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(characterName)}/reference-audio`,
      { method: "DELETE" }
    );
  }

  // ==================== 草稿文件管理 ====================

  /**
   * 获取草稿内容
   */
  static async getDraftContent(
    projectName: string,
    episode: number,
    stepNum: number
  ): Promise<string> {
    const url = `/projects/${encodeURIComponent(projectName)}/drafts/${episode}/step${stepNum}`;
    const response = await fetch(
      `${API_BASE}${url}`,
      withAuth(url)
    );
    await throwIfNotOk(response, "获取草稿内容失败");
    return response.text();
  }

  /**
   * 保存草稿内容
   */
  static async saveDraft(
    projectName: string,
    episode: number,
    stepNum: number,
    content: string
  ): Promise<SuccessResponse> {
    const url = `/projects/${encodeURIComponent(projectName)}/drafts/${episode}/step${stepNum}`;
    const response = await fetch(
      `${API_BASE}${url}`,
      withAuth(url, {
        method: "PUT",
        headers: { "Content-Type": "text/plain" },
        body: content,
      })
    );
    await throwIfNotOk(response, "保存草稿失败");
    return response.json() as Promise<SuccessResponse>;
  }

  /**
   * 删除草稿
   */
  static async deleteDraft(
    projectName: string,
    episode: number,
    stepNum: number
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/drafts/${episode}/step${stepNum}`,
      { method: "DELETE" }
    );
  }

  // ==================== 项目概述管理 ====================

  /**
   * 使用 AI 生成项目概述
   */
  static async generateOverview(
    projectName: string
  ): Promise<{ success: boolean; overview: ProjectOverview }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate-overview`,
      {
        method: "POST",
      }
    );
  }

  /**
   * 更新项目概述（手动编辑）
   */
  static async updateOverview(
    projectName: string,
    updates: Partial<ProjectOverview>
  ): Promise<SuccessResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/overview`,
      {
        method: "PATCH",
        body: JSON.stringify(updates),
      }
    );
  }

  // ==================== 生成 API ====================

  /**
   * 生成分镜图
   * @param projectName - 项目名称
   * @param segmentId - 片段/场景 ID
   * @param prompt - 图片生成 prompt（支持字符串或结构化对象）
   * @param scriptFile - 剧本文件名
   */
  static async generateStoryboard(
    projectName: string,
    segmentId: string,
    prompt: string | Record<string, unknown>,
    scriptFile: string
  ): Promise<{ success: boolean; task_id: string; deduped: boolean; message: string }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/storyboard/${encodeURIComponent(segmentId)}`,
      {
        method: "POST",
        body: JSON.stringify({ prompt, script_file: scriptFile }),
      }
    );
  }

  /**
   * 生成视频
   * @param projectName - 项目名称
   * @param segmentId - 片段/场景 ID
   * @param prompt - 视频生成 prompt（支持字符串或结构化对象）
   * @param scriptFile - 剧本文件名
   * @param durationSeconds - 时长（秒）
   */
  static async generateVideo(
    projectName: string,
    segmentId: string,
    prompt: string | Record<string, unknown>,
    scriptFile: string,
    durationSeconds: number = 4
  ): Promise<{ success: boolean; task_id: string; deduped: boolean; message: string }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/video/${encodeURIComponent(segmentId)}`,
      {
        method: "POST",
        body: JSON.stringify({
          prompt,
          script_file: scriptFile,
          duration_seconds: durationSeconds,
        }),
      }
    );
  }

  /**
   * 生成单段旁白配音（文本由后端从剧本 novel_text 读取）
   * @param projectName - 项目名称
   * @param segmentId - 片段 ID
   * @param scriptFile - 剧本文件名
   */
  static async generateNarrationAudio(
    projectName: string,
    segmentId: string,
    scriptFile: string
  ): Promise<{ success: boolean; task_id: string; deduped: boolean; message: string }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/tts/${encodeURIComponent(segmentId)}`,
      {
        method: "POST",
        body: JSON.stringify({ script_file: scriptFile }),
      }
    );
  }

  /**
   * 批量生成全集旁白配音（只入队缺少旁白且有原文的段）
   * @param projectName - 项目名称
   * @param scriptFile - 剧本文件名
   */
  static async generateEpisodeNarrationAudio(
    projectName: string,
    scriptFile: string
  ): Promise<{ success: boolean; task_ids: string[]; deduped: boolean; message: string }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/tts`,
      {
        method: "POST",
        body: JSON.stringify({ script_file: scriptFile }),
      }
    );
  }

  /**
   * 读取当前项目实际生效的 audio backend 音色枚举，供 TTS 试听弹窗选择音色。
   * configured=false 表示未配置任何 audio 供应商，前端据此禁用生成入口。
   * @param projectName - 项目名称
   */
  static async getAudioBackendVoices(
    projectName: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<{
    configured: boolean;
    provider_id: string | null;
    model: string | null;
    voices: { id: string; label: string }[];
  }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/audio-backend/voices`, {
      signal: options.signal,
    });
  }

  /**
   * 提交角色 TTS 试听样本生成任务（预览用，需再调用 confirmCharacterVoiceSample 才落资产）
   * @param projectName - 项目名称
   * @param charName - 角色名称
   * @param text - 待合成文本
   * @param voice - 音色 id
   */
  static async generateCharacterVoiceSample(
    projectName: string,
    charName: string,
    text: string,
    voice: string
  ): Promise<{ success: boolean; task_id: string; deduped: boolean; message: string }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(charName)}/voice-sample`,
      {
        method: "POST",
        body: JSON.stringify({ text, voice }),
      }
    );
  }

  /**
   * 把已生成、已试听的 TTS 样本提升为角色 reference_audio
   * @param projectName - 项目名称
   * @param charName - 角色名称
   * @param taskId - generateCharacterVoiceSample 返回的 task_id
   */
  static async confirmCharacterVoiceSample(
    projectName: string,
    charName: string,
    taskId: string
  ): Promise<{ success: boolean; path: string; url: string }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/characters/${encodeURIComponent(charName)}/voice-sample/confirm`,
      {
        method: "POST",
        body: JSON.stringify({ task_id: taskId }),
      }
    );
  }

  /**
   * 生成角色设计图
   * @param projectName - 项目名称
   * @param charName - 角色名称
   * @param prompt - 角色描述 prompt
   */
  static async generateCharacter(
    projectName: string,
    charName: string,
    prompt: string,
    regenerate = false
  ): Promise<{
    success: boolean;
    task_id: string | null;
    deduped: boolean;
    reused?: boolean;
    message: string;
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/character/${encodeURIComponent(charName)}`,
      {
        method: "POST",
        body: JSON.stringify({ prompt, ...(regenerate ? { regenerate: true } : {}) }),
      }
    );
  }

  /**
   * 生成场景设计图
   * @param projectName - 项目名称
   * @param sceneName - 场景名称
   * @param prompt - 场景描述 prompt
   */
  static async generateProjectScene(
    projectName: string,
    sceneName: string,
    prompt: string,
    regenerate = false
  ): Promise<{
    success: boolean;
    task_id: string | null;
    deduped: boolean;
    reused?: boolean;
    message: string;
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/scene/${encodeURIComponent(sceneName)}`,
      {
        method: "POST",
        body: JSON.stringify({ prompt, ...(regenerate ? { regenerate: true } : {}) }),
      }
    );
  }

  /**
   * 生成道具设计图
   * @param projectName - 项目名称
   * @param propName - 道具名称
   * @param prompt - 道具描述 prompt
   */
  static async generateProjectProp(
    projectName: string,
    propName: string,
    prompt: string,
    regenerate = false
  ): Promise<{
    success: boolean;
    task_id: string | null;
    deduped: boolean;
    reused?: boolean;
    message: string;
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/prop/${encodeURIComponent(propName)}`,
      {
        method: "POST",
        body: JSON.stringify({ prompt, ...(regenerate ? { regenerate: true } : {}) }),
      }
    );
  }

  /**
   * 生成产品标准参考图（product sheet）
   * @param projectName - 项目名称
   * @param productName - 产品名称
   * @param prompt - 产品描述 prompt
   */
  static async generateProjectProduct(
    projectName: string,
    productName: string,
    prompt: string,
    regenerate = false
  ): Promise<{
    success: boolean;
    task_id: string | null;
    deduped: boolean;
    reused?: boolean;
    message: string;
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/product/${encodeURIComponent(productName)}`,
      {
        method: "POST",
        body: JSON.stringify({ prompt, ...(regenerate ? { regenerate: true } : {}) }),
      }
    );
  }

  /**
   * 提交图片指令式编辑任务：以当前图为唯一底图、指令为唯一 prompt 走 i2i，
   * 新图覆盖 current、旧图进版本历史。分镜（resourceType="storyboard"）须带 scriptFile。
   */
  static async editImage(
    projectName: string,
    params: {
      resourceType: "character" | "scene" | "prop" | "product" | "storyboard";
      resourceId: string;
      instruction: string;
      scriptFile?: string | null;
      /** Optional project-scoped media library image used as an additional reference. */
      referenceMediaAssetId?: string | null;
    }
  ): Promise<{
    success: boolean;
    task_id: string;
    deduped: boolean;
    message: string;
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/edit/image`,
      {
        method: "POST",
        body: JSON.stringify({
          resource_type: params.resourceType,
          resource_id: params.resourceId,
          instruction: params.instruction,
          script_file: params.scriptFile ?? null,
          ...(params.referenceMediaAssetId ? { reference_media_asset_id: params.referenceMediaAssetId } : {}),
        }),
      }
    );
  }

  // ==================== 任务队列 API ====================

  static async splitCanvasImage(
    projectName: string,
    params: {
      sourceKind: "project" | "media";
      resourceType?: "character" | "scene" | "prop" | "product" | "storyboard";
      resourceId?: string;
      mediaAssetId?: string;
      scriptFile?: string | null;
      rows: number;
      cols: number;
      includeSplitLines: boolean;
    },
  ): Promise<{ success: boolean; task_id: string; deduped: boolean }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/canvas-images/split`, {
      method: "POST",
      body: JSON.stringify({
        source_kind: params.sourceKind,
        ...(params.resourceType ? { resource_type: params.resourceType } : {}),
        ...(params.resourceId ? { resource_id: params.resourceId } : {}),
        ...(params.mediaAssetId ? { media_asset_id: params.mediaAssetId } : {}),
        ...(params.scriptFile ? { script_file: params.scriptFile } : {}),
        rows: params.rows,
        cols: params.cols,
        include_split_lines: params.includeSplitLines,
      }),
    });
  }

  static async advancedCanvasImage(
    projectName: string,
    params: {
      operation:
        | "canvas_image_panorama"
        | "canvas_image_angles"
        | "canvas_image_layers"
        | "canvas_image_hd"
        | "canvas_image_outpaint"
        | "canvas_image_redraw"
        | "canvas_image_erase"
        | "canvas_image_cutout"
        | "canvas_image_crop";
      sourceKind: "project" | "media";
      resourceType?: "character" | "scene" | "prop" | "product" | "storyboard";
      resourceId?: string;
      mediaAssetId?: string;
      scriptFile?: string | null;
      instruction?: string;
      count?: number;
      region?: { x: number; y: number; width: number; height: number };
      aspectRatio?: string;
      multiplier?: number;
      quality?: string;
    },
  ): Promise<{ success: boolean; task_id: string; deduped: boolean }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/canvas-images/advanced`, {
      method: "POST",
      body: JSON.stringify({
        operation: params.operation,
        source_kind: params.sourceKind,
        ...(params.resourceType ? { resource_type: params.resourceType } : {}),
        ...(params.resourceId ? { resource_id: params.resourceId } : {}),
        ...(params.mediaAssetId ? { media_asset_id: params.mediaAssetId } : {}),
        ...(params.scriptFile ? { script_file: params.scriptFile } : {}),
        ...(params.instruction ? { instruction: params.instruction } : {}),
        ...(params.count ? { count: params.count } : {}),
        ...(params.region ? { region: params.region } : {}),
        ...(params.aspectRatio ? { aspect_ratio: params.aspectRatio } : {}),
        ...(params.multiplier ? { multiplier: params.multiplier } : {}),
        ...(params.quality ? { quality: params.quality } : {}),
      }),
    });
  }

  static async getTask(taskId: string): Promise<TaskItem> {
    return this.request(`/tasks/${encodeURIComponent(taskId)}`);
  }

  static async listTasks(
    filters: TaskListFilters = {}
  ): Promise<{ items: TaskItem[]; total: number; page: number; page_size: number }> {
    const params = new URLSearchParams();
    if (filters.projectName) params.append("project_name", filters.projectName);
    if (filters.status) params.append("status", filters.status);
    if (filters.taskType) params.append("task_type", filters.taskType);
    if (filters.source) params.append("source", filters.source);
    if (filters.page) params.append("page", String(filters.page));
    if (filters.pageSize) params.append("page_size", String(filters.pageSize));
    const query = params.toString();
    return this.request(`/tasks${query ? "?" + query : ""}`);
  }

  static async listProjectTasks(
    projectName: string,
    filters: Omit<TaskListFilters, "projectName"> = {}
  ): Promise<{ items: TaskItem[]; total: number; page: number; page_size: number }> {
    const params = new URLSearchParams();
    if (filters.status) params.append("status", filters.status);
    if (filters.taskType) params.append("task_type", filters.taskType);
    if (filters.source) params.append("source", filters.source);
    if (filters.page) params.append("page", String(filters.page));
    if (filters.pageSize) params.append("page_size", String(filters.pageSize));
    const query = params.toString();
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/tasks${query ? "?" + query : ""}`
    );
  }

  static async getTaskStats(
    projectName: string | null = null
  ): Promise<{ stats: TaskStats }> {
    const params = new URLSearchParams();
    if (projectName) params.append("project_name", projectName);
    const query = params.toString();
    return this.request(`/tasks/stats${query ? "?" + query : ""}`);
  }

  // ==================== 任务取消 API ====================

  static async cancelPreview(
    taskId: string
  ): Promise<{
    task: { task_id: string; task_type: string; resource_id: string; status: string };
    cascaded: { task_id: string; task_type: string; resource_id: string }[];
  }> {
    return this.request(`/tasks/${encodeURIComponent(taskId)}/cancel-preview`);
  }

  static async cancelTask(
    taskId: string
  ): Promise<{
    cancelled: TaskItem[];
    cancelling: string[];
    skipped_terminal: TaskItem[];
  }> {
    return this.request(`/tasks/${encodeURIComponent(taskId)}/cancel`, {
      method: "POST",
    });
  }

  static async createBatch(
    projectName: string,
    payload: CreateDurableBatchRequest,
  ): Promise<DurableBatchResponse> {
    return this.request<DurableBatchResponse>(
      "/projects/" + encodeURIComponent(projectName) + "/batches",
      {
        method: "POST",
        body: JSON.stringify(payload),
      },
    );
  }

  static async getBatch(
    projectName: string,
    batchId: string,
  ): Promise<DurableBatchResponse> {
    return this.request<DurableBatchResponse>(
      "/projects/" + encodeURIComponent(projectName) + "/batches/" + encodeURIComponent(batchId),
    );
  }

  static async cancelBatch(
    projectName: string,
    batchId: string,
  ): Promise<DurableBatchResponse> {
    return this.request<DurableBatchResponse>(
      "/projects/" + encodeURIComponent(projectName) + "/batches/" + encodeURIComponent(batchId) + "/cancel",
      {
        method: "POST",
      },
    );
  }

  static async retryFailedBatch(
    projectName: string,
    batchId: string,
  ): Promise<DurableBatchResponse> {
    return this.request<DurableBatchResponse>(
      "/projects/" + encodeURIComponent(projectName) + "/batches/" + encodeURIComponent(batchId) + "/retry-failed",
      {
        method: "POST",
      },
    );
  }

  static async cancelAllPreview(
    projectName: string
  ): Promise<{ active_count: number }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/tasks/cancel-all-preview`
    );
  }

  static async cancelAllActive(
    projectName: string
  ): Promise<{
    cancelled_count: number;
    cancelling_count: number;
    skipped_terminal_count: number;
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/tasks/cancel-all`,
      { method: "POST" }
    );
  }

  static openProjectEventStream(options: ProjectEventStreamOptions): SseConnection {
    const url = `${API_BASE}/projects/${encodeURIComponent(options.projectName)}/events/stream`;
    const source = openAuthenticatedSse({ url, headers: withAuth(url).headers });

    const parsePayload = (event: MessageEvent): unknown => {
      try {
        return JSON.parse((event.data as string) || "{}");
      } catch (err) {
        console.error("解析项目事件 SSE 数据失败:", err, event.data);
        return null;
      }
    };

    const createHandler = <T>(
      callback?: (payload: T, event: MessageEvent) => void
    ) => {
      return (event: Event) => {
        if (typeof callback !== "function") return;
        const payload = parsePayload(event as MessageEvent);
        if (payload) {
          callback(payload as T, event as MessageEvent);
        }
      };
    };

    source.addEventListener("snapshot", createHandler(options.onSnapshot));
    source.addEventListener("changes", createHandler(options.onChanges));
    source.addEventListener("project_deleted", createHandler(options.onProjectDeleted));

    source.onerror = (event: Event) => {
      if (typeof options.onError === "function") {
        options.onError(event);
      }
    };

    return source;
  }

  // ==================== 版本管理 API ====================

  /**
   * 获取资源版本列表
   * @param projectName - 项目名称
   * @param resourceType - 资源类型 (storyboards, videos, characters, scenes, props)
   * @param resourceId - 资源 ID
   */
  static async getVersions(
    projectName: string,
    resourceType: string,
    resourceId: string
  ): Promise<{
    resource_type: string;
    resource_id: string;
    current_version: number;
    versions: VersionInfo[];
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/versions/${encodeURIComponent(resourceType)}/${encodeURIComponent(resourceId)}`
    );
  }

  /**
   * 还原到指定版本
   * @param projectName - 项目名称
   * @param resourceType - 资源类型
   * @param resourceId - 资源 ID
   * @param version - 要还原的版本号
   */
  static async restoreVersion(
    projectName: string,
    resourceType: string,
    resourceId: string,
    version: number
  ): Promise<SuccessResponse & { file_path?: string; asset_fingerprints?: Record<string, number> }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/versions/${encodeURIComponent(resourceType)}/${encodeURIComponent(resourceId)}/restore/${version}`,
      {
        method: "POST",
      }
    );
  }

  // ==================== 风格参考图 API ====================

  /**
   * 上传风格参考图
   * @param projectName - 项目名称
   * @param file - 图片文件
   * @returns 包含 style_image, style_description, url 的结果
   */
  static async uploadStyleImage(
    projectName: string,
    file: File
  ): Promise<{
    success: boolean;
    style_image: string;
    style_description: string;
    url: string;
  }> {
    const formData = new FormData();
    formData.append("file", file);

    const url = `/projects/${encodeURIComponent(projectName)}/style-image`;
    const response = await fetch(
      `${API_BASE}${url}`,
      withAuth(url, {
        method: "POST",
        body: formData,
      })
    );

    await throwIfNotOk(response, "上传失败");

    return response.json() as Promise<{ success: boolean; style_image: string; style_description: string; url: string }>;
  }

  // ==================== 助手会话 API ====================

  /** Build the project-scoped assistant base path. */
  private static assistantBase(projectName: string): string {
    return `/projects/${encodeURIComponent(projectName)}/assistant`;
  }

  static async listAssistantSessions(
    projectName: string,
    status: string | null = null,
    options: { signal?: AbortSignal } = {}
  ): Promise<{ sessions: SessionMeta[] }> {
    const params = new URLSearchParams();
    if (status) params.append("status", status);
    const query = params.toString();
    return this.request(
      `${this.assistantBase(projectName)}/sessions${query ? "?" + query : ""}`,
      { signal: options.signal }
    );
  }

  static async getAssistantSession(
    projectName: string,
    sessionId: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<{ session: SessionMeta }> {
    return this.request(
      `${this.assistantBase(projectName)}/sessions/${encodeURIComponent(sessionId)}`,
      { signal: options.signal }
    );
  }

  /** 冷读会话事件日志（after 为 seq 游标，-1 表示从头）。 */
  static async listAssistantEntries(
    projectName: string,
    sessionId: string,
    after: number = -1,
    options: { signal?: AbortSignal } = {}
  ): Promise<EntriesResponse> {
    const query = after >= 0 ? `?after=${after}` : "";
    return this.request(
      `${this.assistantBase(projectName)}/sessions/${encodeURIComponent(sessionId)}/entries${query}`,
      { signal: options.signal }
    );
  }

  static async sendAssistantMessage(
    projectName: string,
    content: string,
    sessionId?: string | null,
    images?: Array<{ data: string; media_type: string }>,
    clientKey?: string,
    sdkType?: "claude" | "openai"
  ): Promise<{ session_id: string; status: string; entry: TimelineEntry | null }> {
    return this.request(`${this.assistantBase(projectName)}/sessions/send`, {
      method: "POST",
      body: JSON.stringify({
        content,
        session_id: sessionId || undefined,
        images: images || [],
        client_key: clientKey || undefined,
        sdk_type: sdkType || "claude",
      }),
    });
  }

  static async preflightProjectImport(file: File): Promise<ImportPreflightResponse> {
    const formData = new FormData();
    formData.append("file", file);
    const response = await fetch(
      `${API_BASE}/projects/import/preflight`,
      withAuth("/projects/import/preflight", { method: "POST", body: formData }),
    );
    if (!response.ok) {
      handleUnauthorized(response);
      const payload = (await response.json().catch(() => ({ detail: response.statusText }))) as ImportErrorPayload;
      const error = new Error(typeof payload.detail === "string" ? payload.detail : "导入预检失败") as Error & {
        status?: number;
        diagnostics?: ImportFailureDiagnostics;
      };
      error.status = response.status;
      error.diagnostics = normalizeImportFailureDiagnostics(payload.diagnostics);
      throw error;
    }
    return response.json() as Promise<ImportPreflightResponse>;
  }

  static async rewriteAssistantMessage(
    projectName: string,
    sessionId: string,
    anchorEntryUuid: string,
    content: string,
    clientKey?: string,
  ): Promise<{ status: string; session_id: string; origin_session_id: string | null; entry: TimelineEntry | null }> {
    return this.request(
      `${this.assistantBase(projectName)}/sessions/${encodeURIComponent(sessionId)}/rewrite`,
      {
        method: "POST",
        body: JSON.stringify({
          anchor_entry_uuid: anchorEntryUuid,
          content,
          client_key: clientKey || undefined,
        }),
      },
    );
  }

  static async switchAssistantAgent(
    projectName: string,
    sessionId: string,
    sdkType: "claude" | "openai"
  ): Promise<{ status: string; session_id: string; meta: SessionMeta }> {
    return this.request(
      `${this.assistantBase(projectName)}/sessions/${encodeURIComponent(sessionId)}/switch-agent`,
      {
        method: "POST",
        body: JSON.stringify({ sdk_type: sdkType }),
      }
    );
  }

  static async interruptAssistantSession(
    projectName: string,
    sessionId: string
  ): Promise<SuccessResponse> {
    return this.request(
      `${this.assistantBase(projectName)}/sessions/${encodeURIComponent(sessionId)}/interrupt`,
      {
        method: "POST",
      }
    );
  }

  static async answerAssistantQuestion(
    projectName: string,
    sessionId: string,
    questionId: string,
    answers: Record<string, string>
  ): Promise<SuccessResponse> {
    return this.request(
      `${this.assistantBase(projectName)}/sessions/${encodeURIComponent(sessionId)}/questions/${encodeURIComponent(questionId)}/answer`,
      {
        method: "POST",
        body: JSON.stringify({ answers }),
      }
    );
  }

  /** entry 流 SSE URL（after 为 seq 游标）。认证凭证通过 Authorization header 发送。 */
  static getAssistantEntriesStreamUrl(
    projectName: string,
    sessionId: string,
    after: number = -1
  ): string {
    const base = `${API_BASE}${this.assistantBase(projectName)}/sessions/${encodeURIComponent(sessionId)}/entries/stream`;
    const url = after >= 0 ? `${base}?after=${after}` : base;
    return url;
  }

  static openAssistantEntriesStream(
    projectName: string,
    sessionId: string,
    after: number = -1,
  ): SseConnection {
    const url = this.getAssistantEntriesStreamUrl(projectName, sessionId, after);
    return openAuthenticatedSse({ url, headers: withAuth(url).headers });
  }

  static async listAssistantSkills(
    projectName: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<{ skills: SkillInfo[] }> {
    return this.request(
      `${this.assistantBase(projectName)}/skills`,
      { signal: options.signal }
    );
  }

  static async deleteAssistantSession(
    projectName: string,
    sessionId: string
  ): Promise<SuccessResponse> {
    return this.request(
      `${this.assistantBase(projectName)}/sessions/${encodeURIComponent(sessionId)}`,
      {
        method: "DELETE",
      }
    );
  }

  // ==================== 费用统计 API ====================

  /**
   * 获取统计摘要
   * @param filters - 筛选条件
   */
  static async getUsageStats(
    filters: UsageStatsFilters = {},
    options: { signal?: AbortSignal } = {}
  ): Promise<Record<string, unknown>> {
    const params = new URLSearchParams();
    if (filters.projectName)
      params.append("project_name", filters.projectName);
    if (filters.startDate) params.append("start_date", filters.startDate);
    if (filters.endDate) params.append("end_date", filters.endDate);
    const query = params.toString();
    return this.request(`/usage/stats${query ? "?" + query : ""}`, {
      signal: options.signal,
    });
  }

  /**
   * 获取调用记录列表
   * @param filters - 筛选条件
   */
  static async getUsageCalls(
    filters: UsageCallsFilters = {},
    options: { signal?: AbortSignal } = {}
  ): Promise<Record<string, unknown>> {
    const params = new URLSearchParams();
    if (filters.projectName)
      params.append("project_name", filters.projectName);
    if (filters.callType) params.append("call_type", filters.callType);
    if (filters.status) params.append("status", filters.status);
    if (filters.startDate) params.append("start_date", filters.startDate);
    if (filters.endDate) params.append("end_date", filters.endDate);
    if (filters.page) params.append("page", String(filters.page));
    if (filters.pageSize) params.append("page_size", String(filters.pageSize));
    const query = params.toString();
    return this.request(`/usage/calls${query ? "?" + query : ""}`, { signal: options.signal });
  }

  /**
   * 获取有调用记录的项目列表
   */
  static async getUsageProjects(): Promise<{ projects: string[] }> {
    return this.request("/usage/projects");
  }

  // ==================== API Key 管理 API ====================

  /** 列出所有 API Key（不含完整 key）。 */
  static async listApiKeys(): Promise<ApiKeyInfo[]> {
    return this.request("/api-keys");
  }

  /** 创建新 API Key，返回含完整 key 的响应（仅此一次）。 */
  static async createApiKey(name: string, expiresDays?: number): Promise<CreateApiKeyResponse> {
    return this.request("/api-keys", {
      method: "POST",
      body: JSON.stringify({ name, expires_days: expiresDays ?? null }),
    });
  }

  /** 删除（吊销）指定 API Key。 */
  static async deleteApiKey(keyId: number): Promise<void> {
    return this.request(`/api-keys/${keyId}`, { method: "DELETE" });
  }

  // ==================== Agent 长期记忆 API ====================

  /** 获取当前用户已确认的全局记忆。 */
  static async listUserMemories(): Promise<MemoryListResponse<MemoryEntry>> {
    return this.request("/memories/user");
  }

  /** 创建一条用户级长期记忆。 */
  static async createUserMemory(body: CreateMemoryRequest): Promise<MemoryEntry> {
    return this.request("/memories/user", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  /** 更新一条属于当前用户的长期记忆。 */
  static async updateMemory(memoryId: string, body: UpdateMemoryRequest): Promise<MemoryEntry> {
    return this.request(`/memories/${encodeURIComponent(memoryId)}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    });
  }

  /** 删除一条属于当前用户的长期记忆。 */
  static async deleteMemory(memoryId: string): Promise<void> {
    return this.request(`/memories/${encodeURIComponent(memoryId)}`, { method: "DELETE" });
  }

  /** 清空当前用户的全局记忆。 */
  static async clearUserMemories(): Promise<{ deleted: number }> {
    return this.request("/memories/user", { method: "DELETE" });
  }

  /** 获取当前项目已确认的长期记忆。 */
  static async listProjectMemories(projectName: string): Promise<MemoryListResponse<MemoryEntry>> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/memories`);
  }

  /** 创建一条项目级长期记忆。 */
  static async createProjectMemory(projectName: string, body: CreateMemoryRequest): Promise<MemoryEntry> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/memories`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  /** 清空指定项目的长期记忆。 */
  static async clearProjectMemories(projectName: string): Promise<{ deleted: number }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/memories`, { method: "DELETE" });
  }

  /** 将项目记忆导入指定项目；服务端只接受项目记忆，不会写入用户级记忆。 */
  static async importProjectMemories(
    projectName: string,
    payload: unknown,
  ): Promise<{ imported: number; skipped: number }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/memories/import`, {
      method: "POST",
      body: JSON.stringify(payload),
    });
  }

  /** 导入用户主动选择的记忆包；按作用域追加并去重，不覆盖已有记忆。 */
  static async importMemories(
    payload: unknown,
    projectName?: string,
  ): Promise<{ imported: number; skipped: number }> {
    const query = projectName ? `?project_name=${encodeURIComponent(projectName)}` : "";
    return this.request(`/memories/import${query}`, {
      method: "POST",
      body: JSON.stringify(payload),
    });
  }

  /** 获取待用户确认的记忆候选。默认不传项目筛选，返回当前用户全部候选。 */
  static async listMemoryCandidates(): Promise<MemoryListResponse<MemoryCandidate>> {
    return this.request("/memory-candidates");
  }

  static async acceptMemoryCandidate(candidateId: string): Promise<{
    memory: MemoryEntry;
    candidate_id: string;
    status: string;
  }> {
    return this.request(`/memory-candidates/${encodeURIComponent(candidateId)}/accept`, { method: "POST" });
  }

  static async rejectMemoryCandidate(candidateId: string): Promise<{ candidate_id: string; status: string }> {
    return this.request(`/memory-candidates/${encodeURIComponent(candidateId)}/reject`, { method: "POST" });
  }

  /** 导出当前用户记忆；未传项目名时只包含用户级记忆。 */
  static async exportMemories(projectName?: string): Promise<MemoryExport> {
    const query = projectName ? `?project_name=${encodeURIComponent(projectName)}` : "";
    return this.request(`/memories/export${query}`);
  }
  // ==================== Provider 管理 API ====================

  /** 获取所有 provider 列表及状态。 */
  static async getProviders(): Promise<{ providers: ProviderInfo[] }> {
    return this.request("/providers");
  }

  /** 获取指定 provider 的配置详情（含字段列表）。 */
  static async getProviderConfig(id: string): Promise<ProviderConfigDetail> {
    return this.request(`/providers/${encodeURIComponent(id)}/config`);
  }

  /** 更新指定 provider 的配置字段。 */
  static async patchProviderConfig(
    id: string,
    patch: Record<string, string | null>
  ): Promise<void> {
    return this.request(`/providers/${encodeURIComponent(id)}/config`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    });
  }

  /** 开关预置供应商的启用状态；关闭后生成链路不再选择/调用该供应商。 */
  static async setProviderEnabled(id: string, enabled: boolean): Promise<void> {
    return this.request(`/providers/${encodeURIComponent(id)}/enabled`, {
      method: "PATCH",
      body: JSON.stringify({ enabled }),
    });
  }

  /** 更新连接测试发现的未登记模型媒体类型覆盖。 */
  static async patchProviderModelTypes(
    id: string,
    modelTypes: Record<string, string | null>,
  ): Promise<{ model_type_overrides: Record<string, string>; discovered_models: string[] }> {
    return this.request(`/providers/${encodeURIComponent(id)}/model-types`, {
      method: "PATCH",
      body: JSON.stringify({ model_types: modelTypes }),
    });
  }

  /** 测试指定 provider 的连接。 */
  static async testProviderConnection(id: string, credentialId?: number): Promise<ProviderTestResult> {
    const params = credentialId != null ? `?credential_id=${credentialId}` : "";
    return this.request(`/providers/${encodeURIComponent(id)}/test${params}`, {
      method: "POST",
    });
  }

  // ==================== Provider 凭证管理 API ====================

  static async listCredentials(providerId: string): Promise<{ credentials: ProviderCredential[] }> {
    return this.request(`/providers/${encodeURIComponent(providerId)}/credentials`);
  }

  static async createCredential(
    providerId: string,
    data: { name: string; api_key?: string; base_url?: string; access_key?: string; secret_key?: string },
  ): Promise<ProviderCredential> {
    return this.request(`/providers/${encodeURIComponent(providerId)}/credentials`, {
      method: "POST",
      body: JSON.stringify(data),
    });
  }

  static async updateCredential(
    providerId: string,
    credId: number,
    data: { name?: string; api_key?: string; base_url?: string; access_key?: string; secret_key?: string },
  ): Promise<void> {
    return this.request(
      `/providers/${encodeURIComponent(providerId)}/credentials/${credId}`,
      { method: "PATCH", body: JSON.stringify(data) },
    );
  }

  static async deleteCredential(providerId: string, credId: number): Promise<void> {
    return this.request(
      `/providers/${encodeURIComponent(providerId)}/credentials/${credId}`,
      { method: "DELETE" },
    );
  }

  static async activateCredential(providerId: string, credId: number): Promise<void> {
    return this.request(
      `/providers/${encodeURIComponent(providerId)}/credentials/${credId}/activate`,
      { method: "POST" },
    );
  }

  static async uploadVertexCredential(name: string, file: File): Promise<ProviderCredential> {
    const formData = new FormData();
    formData.append("file", file);
    const url = `/providers/gemini-vertex/credentials/upload?name=${encodeURIComponent(name)}`;
    const response = await fetch(
      `${API_BASE}${url}`,
      withAuth(url, { method: "POST", body: formData }),
    );
    await throwIfNotOk(response, "上传凭证失败");
    return response.json() as Promise<ProviderCredential>;
  }

  // ==================== Agent 配置 / 凭证 API ====================

  static async listAgentPresetProviders(sdkType: "claude" | "openai" = "claude"): Promise<PresetProvidersResponse> {
    return this.request(`/agent/preset-providers?sdk_type=${sdkType}`);
  }

  static async listAgentCredentials(
    sdkType?: "claude" | "openai",
  ): Promise<{ credentials: AgentCredential[] }> {
    const query = sdkType ? `?sdk_type=${sdkType}` : "";
    return this.request(`/agent/credentials${query}`);
  }

  static async createAgentCredential(
    data: CreateAgentCredentialRequest,
  ): Promise<AgentCredential> {
    return this.request("/agent/credentials", {
      method: "POST",
      body: JSON.stringify(data),
    });
  }

  static async updateAgentCredential(
    id: number,
    data: UpdateAgentCredentialRequest,
  ): Promise<AgentCredential> {
    return this.request(`/agent/credentials/${id}`, {
      method: "PATCH",
      body: JSON.stringify(data),
    });
  }

  static async deleteAgentCredential(id: number): Promise<void> {
    return this.request(`/agent/credentials/${id}`, { method: "DELETE" });
  }

  static async activateAgentCredential(id: number): Promise<{ active_id: number }> {
    return this.request(`/agent/credentials/${id}/activate`, { method: "POST" });
  }

  static async testAgentCredential(id: number): Promise<TestConnectionResponse> {
    return this.request(`/agent/credentials/${id}/test`, { method: "POST" });
  }

  static async testAgentConnectionDraft(
    data: TestConnectionRequest,
  ): Promise<TestConnectionResponse> {
    return this.request("/agent/test-connection", {
      method: "POST",
      body: JSON.stringify(data),
    });
  }

  // ==================== 自定义供应商 API ====================

  static async listCustomProviders(): Promise<{ providers: CustomProviderInfo[] }> {
    return this.request("/custom-providers");
  }

  static async listEndpointCatalog(): Promise<{ endpoints: EndpointDescriptor[] }> {
    return this.request("/custom-providers/endpoints");
  }

  static async createCustomProvider(data: CustomProviderCreateRequest): Promise<CustomProviderInfo> {
    return this.request("/custom-providers", { method: "POST", body: JSON.stringify(data) });
  }

  static async getCustomProvider(id: number): Promise<CustomProviderInfo> {
    return this.request(`/custom-providers/${id}`);
  }

  static async updateCustomProvider(id: number, data: Partial<Omit<CustomProviderCreateRequest, "discovery_format" | "models" | "image_max_workers" | "video_max_workers" | "audio_max_workers" | "image_request_timeout_seconds">>): Promise<void> {
    return this.request(`/custom-providers/${id}`, { method: "PATCH", body: JSON.stringify(data) });
  }

  static async fullUpdateCustomProvider(id: number, data: CustomProviderFullUpdateRequest): Promise<CustomProviderInfo> {
    return this.request(`/custom-providers/${id}`, { method: "PUT", body: JSON.stringify(data) });
  }

  static async deleteCustomProvider(id: number): Promise<void> {
    return this.request(`/custom-providers/${id}`, { method: "DELETE" });
  }

  static async replaceCustomProviderModels(id: number, models: CustomProviderModelInput[]): Promise<CustomProviderModelInfo[]> {
    return this.request(`/custom-providers/${id}/models`, { method: "PUT", body: JSON.stringify({ models }) });
  }

  static async discoverModels(
    data: { discovery_format: string; base_url: string; api_key: string },
    options: { signal?: AbortSignal } = {},
  ): Promise<{ models: DiscoveredModel[] }> {
    return this.request("/custom-providers/discover", {
      method: "POST",
      body: JSON.stringify(data),
      signal: options.signal,
    });
  }

  static async discoverModelsForProvider(
    id: number,
    options: { signal?: AbortSignal } = {},
  ): Promise<{ models: DiscoveredModel[] }> {
    return this.request(`/custom-providers/${id}/discover`, { method: "POST", signal: options.signal });
  }

  static async testCustomConnection(data: { discovery_format: string; base_url: string; api_key: string }): Promise<{ success: boolean; message: string }> {
    return this.request("/custom-providers/test", { method: "POST", body: JSON.stringify(data) });
  }

  static async testCustomConnectionById(id: number): Promise<{ success: boolean; message: string }> {
    return this.request(`/custom-providers/${id}/test`, { method: "POST" });
  }

  static async getCustomProviderCredentials(id: number): Promise<CustomProviderCredentials> {
    return this.request(`/custom-providers/${id}/credentials`);
  }

  static async discoverAnthropicModels(
    data: AnthropicDiscoverRequest,
    options: { signal?: AbortSignal } = {},
  ): Promise<AnthropicDiscoverResponse> {
    return this.request("/custom-providers/discover-anthropic", {
      method: "POST",
      body: JSON.stringify(data),
      signal: options.signal,
    });
  }

  static async discoverOpenAIModels(
    data: AnthropicDiscoverRequest,
    options: { signal?: AbortSignal } = {},
  ): Promise<AnthropicDiscoverResponse> {
    return this.request("/custom-providers/discover-openai", {
      method: "POST",
      body: JSON.stringify(data),
      signal: options.signal,
    });
  }

  // ==================== 用量统计（按 provider 分组）API ====================

  /**
   * 获取按 provider 分组的用量统计。
   * @param params - 可选筛选：provider、start、end（ISO 日期字符串）
   */
  static async getUsageStatsGrouped(
    params: { provider?: string; start?: string; end?: string } = {}
  ): Promise<UsageStatsResponse> {
    const searchParams = new URLSearchParams();
    searchParams.append("group_by", "provider");
    if (params.provider) searchParams.append("provider", params.provider);
    if (params.start) searchParams.append("start_date", params.start);
    if (params.end) searchParams.append("end_date", params.end);
    return this.request(`/usage/stats?${searchParams.toString()}`);
  }

  // ==================== 费用估算 API ====================

  /**
   * 获取项目费用估算。
   * @param projectName - 项目名称
   */
  static async getCostEstimate(
    projectName: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<CostEstimateResponse> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/cost-estimate`, {
      signal: options.signal,
    });
  }

  // ==================== Grid 图生视频 API ====================

  /**
   * 生成 Grid 图像（多场景网格）
   * @param projectName - 项目名称
   * @param episode - 剧集编号
   * @param scriptFile - 剧本文件名
   * @param sceneIds - 可选，指定场景 ID 列表
   */
  static async generateGrid(
    projectName: string,
    episode: number,
    scriptFile: string,
    sceneIds?: string[]
  ): Promise<{ success: boolean; grid_ids: string[]; task_ids: string[]; deduped: boolean; message: string }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/generate/grid/${episode}`,
      { method: "POST", body: JSON.stringify({ script_file: scriptFile, scene_ids: sceneIds }) }
    );
  }

  /**
   * 列出项目所有 Grid 记录
   * @param projectName - 项目名称
   */
  static async listGrids(
    projectName: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<GridGeneration[]> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/grids`, {
      signal: options.signal,
    });
  }

  /**
   * 获取项目的宫格档位能力（4×4/5×5 是否可用、单张格数上限）
   * @param projectName - 项目名称
   */
  static async getGridCapability(
    projectName: string,
    options: { signal?: AbortSignal } = {}
  ): Promise<GridCapability> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/grid-capability`, {
      signal: options.signal,
    });
  }

  /**
   * 获取单个 Grid 详情
   * @param projectName - 项目名称
   * @param gridId - Grid ID
   */
  static async getGrid(projectName: string, gridId: string): Promise<GridGeneration> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/grids/${encodeURIComponent(gridId)}`);
  }

  /**
   * 重新生成 Grid 图像
   * @param projectName - 项目名称
   * @param gridId - Grid ID
   */
  static async regenerateGrid(
    projectName: string,
    gridId: string
  ): Promise<{ success: boolean; task_id: string; deduped: boolean }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/grids/${encodeURIComponent(gridId)}/regenerate`,
      { method: "POST" }
    );
  }

  /**
   * 切分落格：按当前联合图覆写各分镜格（唯一覆写分镜格的操作，同步执行）
   * @param projectName - 项目名称
   * @param gridId - Grid ID
   */
  static async splitGrid(
    projectName: string,
    gridId: string
  ): Promise<{
    success: boolean;
    split_at: string | null;
    updated_scene_ids: string[];
    missing_scene_ids: string[];
    asset_fingerprints: Record<string, number>;
  }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/grids/${encodeURIComponent(gridId)}/split`,
      { method: "POST" }
    );
  }

  /**
   * 上传联合图替换当前宫格图（仅转 PNG 归一化，不缩放、不校验布局；不触发切分）
   * @param projectName - 项目名称
   * @param gridId - Grid ID
   * @param file - 图片文件
   */
  static async uploadGridImage(
    projectName: string,
    gridId: string,
    file: File
  ): Promise<{ success: boolean; path: string; version: number; asset_fingerprints: Record<string, number> }> {
    const url = `/projects/${encodeURIComponent(projectName)}/grids/${encodeURIComponent(gridId)}/upload`;
    return API.postFileUpload(url, file);
  }

  // ==================== Global Asset Library ====================

  static async listAssets(
    params: { type?: AssetType; q?: string; limit?: number; offset?: number } = {},
    options: RequestInit = {},
  ) {
    const usp = new URLSearchParams();
    if (params.type) usp.set("type", params.type);
    if (params.q) usp.set("q", params.q);
    if (params.limit) usp.set("limit", String(params.limit));
    if (params.offset) usp.set("offset", String(params.offset));
    return this.request<{ items: Asset[] }>(`/assets?${usp.toString()}`, options);
  }

  static async getAsset(id: string) {
    return this.request<{ asset: Asset }>(`/assets/${encodeURIComponent(id)}`);
  }

  static async createAsset(payload: AssetCreatePayload & { image?: File }) {
    const form = new FormData();
    form.append("type", payload.type);
    form.append("name", payload.name);
    form.append("description", payload.description ?? "");
    form.append("voice_style", payload.voice_style ?? "");
    if (payload.image) form.append("image", payload.image);
    const endpoint = "/assets";
    const url = `${API_BASE}${endpoint}`;
    const response = await fetch(url, withAuth(endpoint, { method: "POST", body: form }));
    if (!response.ok) {
      handleUnauthorized(response);
      const error = (await response.json().catch(() => ({ detail: response.statusText }))) as {
        detail?: string;
      };
      throw new Error(typeof error.detail === "string" ? error.detail : "请求失败");
    }
    return response.json() as Promise<{ asset: Asset }>;
  }

  static async updateAsset(id: string, patch: AssetUpdatePayload) {
    return this.request<{ asset: Asset }>(`/assets/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    });
  }

  static async replaceAssetImage(id: string, image: File) {
    const form = new FormData();
    form.append("image", image);
    const endpoint = `/assets/${encodeURIComponent(id)}/image`;
    const url = `${API_BASE}${endpoint}`;
    const response = await fetch(url, withAuth(endpoint, { method: "POST", body: form }));
    if (!response.ok) {
      handleUnauthorized(response);
      const error = (await response.json().catch(() => ({ detail: response.statusText }))) as {
        detail?: string;
      };
      throw new Error(typeof error.detail === "string" ? error.detail : "请求失败");
    }
    return response.json() as Promise<{ asset: Asset }>;
  }

  static async deleteAsset(id: string): Promise<void> {
    return this.request(`/assets/${encodeURIComponent(id)}`, { method: "DELETE" });
  }

  static async addAssetFromProject(payload: {
    project_name: string;
    resource_type: AssetType;
    resource_id: string;
    override_name?: string;
    overwrite?: boolean;
  }) {
    return this.request<{ asset: Asset }>(`/assets/from-project`, {
      method: "POST",
      body: JSON.stringify(payload),
    });
  }

  static async applyAssetsToProject(payload: {
    asset_ids: string[];
    target_project: string;
    conflict_policy: "skip" | "overwrite" | "rename";
  }) {
    return this.request<{
      succeeded: Array<{ id: string; name: string }>;
      skipped: Array<{ id: string; name: string }>;
      failed: Array<{ id: string; reason: string }>;
    }>(`/assets/apply-to-project`, {
      method: "POST",
      body: JSON.stringify(payload),
    });
  }

  static getGlobalAssetUrl(path: string | null, fp?: string | null): string | null {
    if (!path) return null;
    const parts = path.split("/");
    if (parts.length < 3 || parts[0] !== "_global_assets") return null;
    const type = parts[1];
    const filename = parts.slice(2).join("/");
    const qs = fp ? `?fp=${encodeURIComponent(fp)}` : "";
    return `${API_BASE}/global-assets/${type}/${filename}${qs}`;
  }

  // ==================== Reference-to-Video API ====================

  /** List reference-video units for an episode. */
  static async listReferenceVideoUnits(
    projectName: string,
    episode: number,
  ): Promise<{ units: ReferenceVideoUnit[] }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/units`,
    );
  }

  /** Create a new reference-video unit. */
  static async addReferenceVideoUnit(
    projectName: string,
    episode: number,
    payload: {
      prompt: string;
      references: ReferenceResource[];
      duration_seconds?: number;
      transition_to_next?: TransitionType;
      note?: string | null;
    },
  ): Promise<{ unit: ReferenceVideoUnit }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/units`,
      { method: "POST", body: JSON.stringify(payload) },
    );
  }

  /** Patch prompt/references/duration/transition/note on an existing unit. */
  static async patchReferenceVideoUnit(
    projectName: string,
    episode: number,
    unitId: string,
    patch: {
      prompt?: string;
      references?: ReferenceResource[];
      duration_seconds?: number;
      transition_to_next?: TransitionType;
      note?: string | null;
    },
  ): Promise<{ unit: ReferenceVideoUnit }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/units/${encodeURIComponent(unitId)}`,
      { method: "PATCH", body: JSON.stringify(patch) },
    );
  }

  /** Delete a unit. Returns void on 204. */
  static async deleteReferenceVideoUnit(
    projectName: string,
    episode: number,
    unitId: string,
  ): Promise<void> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/units/${encodeURIComponent(unitId)}`,
      { method: "DELETE" },
    );
  }

  /** Reorder units by providing the full ordered unit_id list. */
  static async reorderReferenceVideoUnits(
    projectName: string,
    episode: number,
    unitIds: string[],
  ): Promise<{ units: ReferenceVideoUnit[] }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/units/reorder`,
      { method: "POST", body: JSON.stringify({ unit_ids: unitIds }) },
    );
  }

  /**
   * 入队前的时长取档预检：申请秒数与剧本编排不一致时需先向用户确认。
   *
   * 取档按项目当前配置近似解析，实际档位以执行时的模型能力为准。
   */
  static async precheckReferenceVideoDuration(
    projectName: string,
    episode: number,
    unitId: string,
    options?: { signal?: AbortSignal },
  ): Promise<ReferenceDurationPrecheck> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/units/${encodeURIComponent(unitId)}/duration-precheck`,
      { signal: options?.signal },
    );
  }

  /**
   * 分镜文稿的读时派生预览：shots / references / utterances + 降级可见性提示。
   *
   * 只读、不落盘——文稿是唯一真相。提示文本由后端按请求语言渲染（含依赖项目当前
   * 视频模型能力的声音相关几条），前端不再二次翻译。
   */
  static async previewReferenceScript(
    projectName: string,
    episode: number,
    prompt: string,
    options?: { signal?: AbortSignal },
  ): Promise<ScriptPreview> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/script-preview`,
      { method: "POST", body: JSON.stringify({ prompt }), signal: options?.signal },
    );
  }

  /** Enqueue generation; returns 202 with task_id. */
  static async generateReferenceVideoUnit(
    projectName: string,
    episode: number,
    unitId: string,
  ): Promise<{ task_id: string; deduped: boolean }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/units/${encodeURIComponent(unitId)}/generate`,
      { method: "POST" },
    );
  }

  // ==================== Ad Reference-to-Video（派生分组） ====================

  /** ad 项目：列出已持久化的派生分组索引（未派生时为空数组）。 */
  static async listAdReferenceUnits(
    projectName: string,
    episode: number,
    options?: { signal?: AbortSignal },
  ): Promise<{ units: AdReferenceUnit[] }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/units`,
      { signal: options?.signal },
    );
  }

  /** ad 项目：从 shots（重新）派生分组索引并持久化；分组可复现，仅 ad 开放。 */
  static async deriveAdReferenceUnits(
    projectName: string,
    episode: number,
  ): Promise<{ units: AdReferenceUnit[] }> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/reference-videos/episodes/${episode}/derive-units`,
      { method: "POST" },
    );
  }

  // ==================== Shotwise Flow ====================

  static async listWorkflowRuns(
    projectName: string,
    options?: { signal?: AbortSignal },
  ): Promise<{ items: WorkflowRunSummary[] }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/workflow-runs`, {
      signal: options?.signal,
    });
  }

  static async getWorkflowRun(
    runId: string,
    options?: { signal?: AbortSignal },
  ): Promise<WorkflowRunDetail> {
    return this.request(`/workflow-runs/${encodeURIComponent(runId)}`, {
      signal: options?.signal,
    });
  }

  static async generateCreativeDraft(
    projectName: string,
    payload: { operation: CreativeDraftOperation; content?: string; instruction?: string },
  ): Promise<CreativeDraftResult> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/creative-draft/generate`, {
      method: "POST",
      body: JSON.stringify(payload),
    });
  }

  static async retryWorkflowRun(
    runId: string,
    nodeKey: string,
    start = true,
  ): Promise<{ id: string; status: string; version: number; source_run_id: string; retry_from: string }> {
    return this.request(`/workflow-runs/${encodeURIComponent(runId)}/retry`, {
      method: "POST",
      body: JSON.stringify({ node_key: nodeKey, start }),
    });
  }

  static async listWorkflowEvents(
    projectName: string,
    options?: { signal?: AbortSignal },
  ): Promise<{ items: WorkflowEvent[]; cursor: number }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/event-log?limit=200`, {
      signal: options?.signal,
    });
  }

  static async createWorkflowDefinition(projectName: string): Promise<{ id: string }> {
    return this.request("/workflows", {
      method: "POST",
      body: JSON.stringify({
        workspace_id: "default",
        project_id: projectName,
        name: `${projectName} production`,
      }),
    });
  }

  static async createWorkflowRevision(
    definitionId: string,
    body: Record<string, unknown>,
  ): Promise<{ id: string }> {
    return this.request(`/workflows/${encodeURIComponent(definitionId)}/revisions`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async publishWorkflowRevision(revisionId: string): Promise<{ id: string }> {
    return this.request(`/workflow-revisions/${encodeURIComponent(revisionId)}/publish`, {
      method: "POST",
    });
  }

  static async planWorkflowRun(
    revisionId: string,
    projectName: string,
    options?: { episode_id?: string; budget_limit?: number; input_snapshot?: Record<string, unknown> },
  ): Promise<{ id: string; version: number }> {
    return this.request(`/workflow-revisions/${encodeURIComponent(revisionId)}/runs`, {
      method: "POST",
      body: JSON.stringify({
        workspace_id: "default",
        project_id: projectName,
        mode: "hybrid",
        input_snapshot: { project_id: projectName, ...(options?.input_snapshot ?? {}) },
        episode_id: options?.episode_id,
        budget_limit: options?.budget_limit,
      }),
    });
  }

  static async transitionWorkflowRun(
    runId: string,
    action: "start" | "pause" | "resume" | "cancel",
    expectedVersion: number,
  ): Promise<{ id: string; status: string; version: number }> {
    return this.request(`/workflow-runs/${encodeURIComponent(runId)}/${action}`, {
      method: "POST",
      body: JSON.stringify({ expected_version: expectedVersion }),
    });
  }

  // ---- creation skills, plans, media and creative board ----
  static async listCreationSkills(projectName: string): Promise<{ items: Array<Record<string, unknown>> }> {
    return this.request("/creation-skills?project_id=" + encodeURIComponent(projectName));
  }

  static async listCreationSkillVersions(skillId: string): Promise<{ items: Array<Record<string, unknown>> }> {
    return this.request("/creation-skills/" + encodeURIComponent(skillId) + "/versions");
  }
  static async listCreationResources(projectName: string): Promise<{ items: Array<Record<string, unknown>> }> {
    return this.request("/creation-resources?project_id=" + encodeURIComponent(projectName));
  }
  static async previewCreationPlan(projectName: string, body: Record<string, unknown>): Promise<Record<string, unknown>> {
    return this.request("/creation-plans/preview", { method: "POST", body: JSON.stringify({ ...body, project_id: projectName }) });
  }
  static async startCreationPlan(planId: string): Promise<Record<string, unknown>> {
    return this.request("/creation-plans/" + encodeURIComponent(planId) + "/start", { method: "POST", body: JSON.stringify({ mode: "hybrid" }) });
  }
  static async cancelCreationPlan(planId: string): Promise<Record<string, unknown>> {
    return this.request("/creation-plans/" + encodeURIComponent(planId) + "/cancel", { method: "POST" });
  }
  static async invalidateCreationPlan(planId: string): Promise<Record<string, unknown>> {
    return this.request("/creation-plans/" + encodeURIComponent(planId) + "/invalidate", { method: "POST" });
  }
  static async restartCreationPlan(planId: string): Promise<Record<string, unknown>> {
    return this.request("/creation-plans/" + encodeURIComponent(planId) + "/restart", { method: "POST", body: JSON.stringify({ mode: "hybrid" }) });
  }
  static async recompileCreationPlan(planId: string): Promise<Record<string, unknown>> {
    return this.request("/creation-plans/" + encodeURIComponent(planId) + "/recompile", { method: "POST" });
  }
  static async listAssemblyPlans(
    projectName: string,
    options: { signal?: AbortSignal } = {},
  ): Promise<AssemblyPlanListResponse> {
    return this.request(
      `/projects/${encodeURIComponent(projectName)}/assembly-plans`,
      { signal: options.signal },
    );
  }

  static async getAssemblyPlan(
    planId: string,
    options: { signal?: AbortSignal } = {},
  ): Promise<AssemblyPlanResponse> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}`, { signal: options.signal });
  }

  static async createAssemblyPlan(
    projectName: string,
    body: CreateAssemblyPlanRequest,
  ): Promise<AssemblyPlanResponse> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/assembly-plans`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async createAssemblyPlanRevision(
    planId: string,
    body: CreateAssemblyPlanRevisionRequest,
  ): Promise<AssemblyPlanResponse> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}/revisions`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  /** Confirm the reviewed preview for one immutable revision. This is deliberately
   * separate from the generic status endpoint so the UI cannot forge preview readiness. */
  static async confirmAssemblyPlanPreview(
    planId: string,
    body: ConfirmAssemblyPlanPreviewRequest,
  ): Promise<ConfirmAssemblyPlanPreviewResponse> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}/preview-confirm`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  /** Confirm the reviewed preview before transitioning the plan into final rendering. */
  static async confirmAssemblyPlanRender(
    planId: string,
    body: ConfirmAssemblyPlanPreviewRequest,
  ): Promise<ConfirmAssemblyPlanPreviewResponse> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}/render-confirm`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async createAssemblyFinalRender(
    planId: string,
    body: CreateAssemblyFinalRenderRequest,
  ): Promise<AssemblyRenderJob> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}/final-renders`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async createAssemblyPreviewRender(
    planId: string,
    body: CreateAssemblyPreviewRenderRequest,
  ): Promise<AssemblyRenderJob> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}/preview-renders`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async getAssemblyFinalReview(planId: string): Promise<AssemblyFinalReview> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}/final-review`);
  }

  static async downloadAssemblyFinalReviewFrame(
    artifactId: string,
    position: AssemblyFinalReviewFramePosition,
  ): Promise<Blob> {
    const endpoint = `/render-artifacts/${encodeURIComponent(artifactId)}/review-frames/${position}`;
    const response = await fetch(`${API_BASE}${endpoint}`, withAuth(endpoint, { method: "GET" }));
    await throwIfNotOk(response, "加载最终审阅帧失败");
    return response.blob();
  }

  static async downloadAssemblySubtitles(
    planId: string,
    format: "srt" | "vtt",
  ): Promise<Blob> {
    const endpoint = `/assembly-plans/${encodeURIComponent(planId)}/subtitles?format=${format}`;
    const response = await fetch(`${API_BASE}${endpoint}`, withAuth(endpoint, { method: "GET" }));
    await throwIfNotOk(response, "下载字幕失败");
    return response.blob();
  }

  static async downloadAssemblyJianyingDraft(planId: string): Promise<Blob> {
    const endpoint = `/assembly-plans/${encodeURIComponent(planId)}/jianying-draft`;
    const response = await fetch(`${API_BASE}${endpoint}`, withAuth(endpoint, { method: "GET" }));
    await throwIfNotOk(response, "下载剪映草稿失败");
    return response.blob();
  }

  static async confirmAssemblyFinalReview(
    artifactId: string,
    body: ConfirmAssemblyFinalReviewRequest,
  ): Promise<ConfirmAssemblyFinalReviewResponse> {
    return this.request(`/render-artifacts/${encodeURIComponent(artifactId)}/review-confirm`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async getRenderJob(jobId: string): Promise<AssemblyRenderJob> {
    return this.request(`/render-jobs/${encodeURIComponent(jobId)}`);
  }

  static async cancelRenderJob(jobId: string): Promise<AssemblyRenderJob> {
    return this.request(`/render-jobs/${encodeURIComponent(jobId)}/cancel`, {
      method: "POST",
    });
  }

  static async retryRenderJob(jobId: string): Promise<AssemblyRenderJob> {
    return this.request(`/render-jobs/${encodeURIComponent(jobId)}/retry`, {
      method: "POST",
    });
  }

  static async retryFinalRenderJob(jobId: string): Promise<AssemblyRenderJob> {
    return this.request(`/render-jobs/${encodeURIComponent(jobId)}/final-retry`, {
      method: "POST",
    });
  }

  static getRenderJobArtifactUrl(jobId: string): string {
    return `${API_BASE}/render-jobs/${encodeURIComponent(jobId)}/artifact`;
  }

  static async downloadRenderJobArtifact(jobId: string): Promise<Blob> {
    const endpoint = `/render-jobs/${encodeURIComponent(jobId)}/artifact`;
    const response = await fetch(`${API_BASE}${endpoint}`, withAuth(endpoint, { method: "GET" }));
    await throwIfNotOk(response, "下载最终成片失败");
    return response.blob();
  }

  static async transitionAssemblyPlan(
    planId: string,
    body: TransitionAssemblyPlanRequest,
  ): Promise<AssemblyPlanResponse> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}/status`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async checkAssemblyPlanStale(
    planId: string,
    body: CheckAssemblyPlanStaleRequest,
  ): Promise<AssemblyPlanResponse> {
    return this.request(`/assembly-plans/${encodeURIComponent(planId)}/stale-check`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async listMediaAssets(
    projectName: string,
    options?: {
      kind?: "image" | "video" | "audio";
      origin?: "upload" | "generated" | "edited" | "extracted" | "imported";
      workflow_run_id?: string;
      binding_kind?: string;
      target_id?: string;
      purpose?: string;
      archived?: boolean;
    },
  ): Promise<{ items: Array<Record<string, unknown>> }> {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(options ?? {})) {
      if (value !== undefined && value !== "") params.set(key, String(value));
    }
    const query = params.toString();
    return this.request("/projects/" + encodeURIComponent(projectName) + "/media-assets" + (query ? "?" + query : ""));
  }
  static getMediaAssetContentUrl(projectName: string, mediaAssetId: string): string {
    return API_BASE + "/projects/" + encodeURIComponent(projectName) + "/media-assets/content/" + encodeURIComponent(mediaAssetId);
  }
  static async getMediaAsset(projectName: string, mediaAssetId: string): Promise<Record<string, unknown>> {
    return this.request("/projects/" + encodeURIComponent(projectName) + "/media-assets/" + encodeURIComponent(mediaAssetId));
  }
  static async uploadMediaAsset(projectName: string, file: File): Promise<Record<string, unknown>> {
    const body = new FormData();
    body.append("file", file);
    return this.request("/projects/" + encodeURIComponent(projectName) + "/media-assets/upload", { method: "POST", body });
  }
  static async bindMediaAsset(projectName: string, mediaAssetId: string, body: { binding_kind: string; target_id?: string | null; purpose: string }): Promise<Record<string, unknown>> {
    return this.request("/projects/" + encodeURIComponent(projectName) + "/media-assets/" + encodeURIComponent(mediaAssetId) + "/bindings", { method: "POST", body: JSON.stringify(body) });
  }
  static async archiveMediaAsset(projectName: string, mediaAssetId: string, archived: boolean): Promise<Record<string, unknown>> {
    return this.request("/projects/" + encodeURIComponent(projectName) + "/media-assets/" + encodeURIComponent(mediaAssetId) + "/archive", { method: "PATCH", body: JSON.stringify({ archived }) });
  }
  static async listCreativeBoards(projectName: string): Promise<{ items: CreativeBoardRecord[] }> {
    return this.request("/projects/" + encodeURIComponent(projectName) + "/creative-boards");
  }
  static async listCreativeBoardVersions(boardId: string): Promise<{ items: CreativeBoardVersionRecord[] }> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/versions");
  }
  static async getCreativeBoardVersion(boardId: string, versionId: string): Promise<CreativeBoardVersionResponse> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/versions/" + encodeURIComponent(versionId));
  }
  static async createCreativeBoardVersion(
    boardId: string,
    body: { version_name: string; expected_revision?: number },
  ): Promise<CreativeBoardVersionResponse> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/versions", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }
  static async restoreCreativeBoardVersion(
    boardId: string,
    versionId: string,
    expectedRevision: number,
  ): Promise<CreativeBoardRestoreResponse> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/versions/" + encodeURIComponent(versionId) + "/restore", {
      method: "POST",
      body: JSON.stringify({ expected_revision: expectedRevision }),
    });
  }
  static async duplicateCreativeBoard(
    boardId: string,
    body: { name: string },
  ): Promise<CreativeBoardDuplicateResponse> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/copy", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }
  static async createCreativeBoard(projectName: string, body: { name: string; viewport?: CreativeBoardViewport }): Promise<CreativeBoardRecord> {
    return this.request("/projects/" + encodeURIComponent(projectName) + "/creative-boards", { method: "POST", body: JSON.stringify(body) });
  }
  static async getCreativeBoard(boardId: string): Promise<CreativeBoardRecord> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId));
  }
  static async updateCreativeBoard(boardId: string, body: { name?: string; viewport?: CreativeBoardViewport; expected_revision?: number }): Promise<CreativeBoardMutationResponse> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId), { method: "PATCH", body: JSON.stringify(body) });
  }
  static async deleteCreativeBoard(boardId: string): Promise<Record<string, unknown>> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId), { method: "DELETE" });
  }
  static async addCreativeBoardItem(boardId: string, body: { item_type: string; resource_type: string; resource_id: string; position?: { x: number; y: number }; size?: { width: number; height: number }; group_id?: string | null; display_settings?: Record<string, unknown>; expected_revision?: number }): Promise<CreativeBoardMutationResponse> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/items", { method: "POST", body: JSON.stringify(body) });
  }
  static async updateCreativeBoardItem(boardId: string, itemId: string, body: { item_type: string; resource_type: string; resource_id: string; position?: { x: number; y: number }; size?: { width: number; height: number }; group_id?: string | null; display_settings?: Record<string, unknown>; expected_revision?: number }): Promise<CreativeBoardMutationResponse> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/items/" + encodeURIComponent(itemId), { method: "PATCH", body: JSON.stringify(body) });
  }
  static async deleteCreativeBoardItem(boardId: string, itemId: string, expectedRevision?: number): Promise<CreativeBoardMutationResponse> {
    const query = expectedRevision === undefined ? "" : "?expected_revision=" + encodeURIComponent(String(expectedRevision));
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/items/" + encodeURIComponent(itemId) + query, { method: "DELETE" });
  }
  static async addCreativeBoardEdge(boardId: string, body: { source_item_id: string; target_item_id: string; relation: string; expected_revision?: number }): Promise<CreativeBoardMutationResponse> {
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/edges", { method: "POST", body: JSON.stringify(body) });
  }
  static async deleteCreativeBoardEdge(boardId: string, edgeId: string, expectedRevision?: number): Promise<CreativeBoardMutationResponse> {
    const query = expectedRevision === undefined ? "" : "?expected_revision=" + encodeURIComponent(String(expectedRevision));
    return this.request("/creative-boards/" + encodeURIComponent(boardId) + "/edges/" + encodeURIComponent(edgeId) + query, { method: "DELETE" });
  }
  // ---- canvas workflow management ----

  static async listWorkflowDefinitions(
    projectName: string,
    options?: { signal?: AbortSignal },
  ): Promise<{ items: WorkflowDefinitionSummary[] }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/workflows`, {
      signal: options?.signal,
    });
  }

  static async getWorkflowDefinition(
    definitionId: string,
    options?: { signal?: AbortSignal },
  ): Promise<WorkflowDefinitionDetail> {
    return this.request(`/workflows/${encodeURIComponent(definitionId)}`, {
      signal: options?.signal,
    });
  }

  static async reserveWorkflowBudget(
    runId: string,
    amount: number,
  ): Promise<{ reservation_id: string; remaining_amount: number | null }> {
    return this.request(`/workflow-runs/${encodeURIComponent(runId)}/budget/reserve`, {
      method: "POST",
      body: JSON.stringify({ amount }),
    });
  }

  static async settleWorkflowBudget(
    runId: string,
    reservationId: string,
    amount: number,
  ): Promise<{ spent_amount: number; remaining_amount: number | null }> {
    return this.request(`/workflow-runs/${encodeURIComponent(runId)}/budget/settle`, {
      method: "POST",
      body: JSON.stringify({ reservation_id: reservationId, amount }),
    });
  }

  static async listWorkflowRevisions(
    definitionId: string,
  ): Promise<{ items: WorkflowRevisionSummary[] }> {
    return this.request(`/workflows/${encodeURIComponent(definitionId)}/revisions`);
  }

  static async revertWorkflowRevision(
    definitionId: string,
    revisionId: string,
  ): Promise<{ revision_id: string; reverted_from: string }> {
    return this.request(
      `/workflows/${encodeURIComponent(definitionId)}/revisions/${encodeURIComponent(revisionId)}/revert`,
      { method: "POST" },
    );
  }

  static async getWorkflowTemplateUpgrade(definitionId: string): Promise<WorkflowTemplateUpgrade> {
    return this.request(
      "/workflows/" + encodeURIComponent(definitionId) + "/template-upgrade",
    );
  }

  static async applyWorkflowTemplateUpgrade(
    definitionId: string,
  ): Promise<{ upgrade: WorkflowTemplateUpgrade; revision: { id: string; status: string } }> {
    return this.request(
      "/workflows/" + encodeURIComponent(definitionId) + "/template-upgrade",
      {
        method: "POST",
        body: JSON.stringify({ confirmed: true }),
      },
    );
  }
  static async listWorkflowTemplates(): Promise<{ items: WorkflowTemplateCatalogItem[] }> {
    return this.request("/workflow-templates");
  }

  static async createWorkflowTemplateDraft(body: {
    name: string;
    description?: string;
    template_type: "manga" | "short_drama";
    contract?: Record<string, unknown>;
    nodes: WorkflowNodeInput[];
    edges: WorkflowEdgeInput[];
    content_mode?: string;
    generation_mode?: string;
    cover_ref?: string | null;
  }): Promise<{ id: string; status: string }> {
    return this.request("/workflow-templates", { method: "POST", body: JSON.stringify(body) });
  }

  static async listWorkflowCreatorTemplates(): Promise<{ items: WorkflowTemplateCatalogItem[] }> {
    return this.request("/workflow-templates/mine");
  }

  static async updateWorkflowTemplateDraft(
    templateId: string,
    body: {
      name: string;
      description: string;
      template_type: "manga" | "short_drama";
      contract: Record<string, unknown>;
      nodes: Array<Record<string, unknown>>;
      edges: Array<Record<string, unknown>>;
      content_mode: string;
      generation_mode: string;
      cover_ref?: string;
    },
  ): Promise<WorkflowTemplateCatalogItem> {
    return this.request("/workflow-templates/" + encodeURIComponent(templateId), { method: "PUT", body: JSON.stringify(body) });
  }

  static async submitWorkflowTemplate(templateId: string): Promise<{ id: string; status: string }> {
    return this.request("/workflow-templates/" + encodeURIComponent(templateId) + "/submit", { method: "POST" });
  }

  static async withdrawWorkflowTemplate(templateId: string): Promise<{ id: string; status: string }> {
    return this.request("/workflow-templates/" + encodeURIComponent(templateId) + "/withdraw", { method: "POST" });
  }


  static async deriveWorkflowTemplate(
    templateId: string,
    body: { workspace_id: string; project_id: string; name: string },
  ): Promise<{ definition_id: string; revision_id: string; template_id: string }> {
    return this.request("/workflow-templates/" + encodeURIComponent(templateId) + "/derive", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async validateWorkflowPatch(
    runId: string,
    body: { patch: Record<string, unknown> },
  ): Promise<Record<string, unknown>> {
    return this.request("/workflow-runs/" + encodeURIComponent(runId) + "/patch/validate", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async applyWorkflowPatch(
    runId: string,
    body: { patch: Record<string, unknown>; confirmed: boolean; start?: boolean },
  ): Promise<Record<string, unknown>> {
    return this.request("/workflow-runs/" + encodeURIComponent(runId) + "/patch/apply", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }
  static async listWorkflowReviewQueue(options?: {
    template_type?: "manga" | "short_drama";
    risk_tag?: string;
  }): Promise<{ items: WorkflowReviewItem[] }> {
    const query = new URLSearchParams();
    if (options?.template_type) query.set("template_type", options.template_type);
    if (options?.risk_tag) query.set("risk_tag", options.risk_tag);
    const suffix = query.toString() ? `?${query.toString()}` : "";
    return this.request(`/admin/workflow-templates/reviews${suffix}`);
  }


  static async reviewWorkflowTemplate(
    templateId: string,
    body: {
      decision: "start" | "approve" | "reject" | "changes_requested";
      comment: string;
    },
  ): Promise<{ id: string; status: string }> {
    return this.request("/admin/workflow-templates/" + encodeURIComponent(templateId) + "/review", {
      method: "POST",
      body: JSON.stringify(body),
    });
  }
  static async rateWorkflowTemplate(templateId: string, rating: number): Promise<{
    template_id: string;
    rating: number;
    rating_count: number;
  }> {
    return this.request(`/workflow-templates/${encodeURIComponent(templateId)}/rating`, {
      method: "POST",
      body: JSON.stringify({ rating }),
    });
  }

  static async exportWorkflowDefinition(definitionId: string): Promise<WorkflowExport> {
    return this.request(`/workflows/${encodeURIComponent(definitionId)}/export`);
  }

  static async importWorkflow(
    projectName: string,
    body: {
      workspace_id: string;
      name: string;
      nodes: WorkflowNodeInput[];
      edges: WorkflowEdgeInput[];
      template_lock?: Record<string, unknown> | null;
      content_mode?: string;
      generation_mode?: string;
      input_schema?: Record<string, unknown>;
    },
  ): Promise<{ definition_id: string; revision_id: string }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/workflows/import`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  }

  static async migrateProjectWorkflow(projectName: string): Promise<{
    migrated: boolean;
    definition_id: string;
    revision_id?: string;
  }> {
    return this.request(`/projects/${encodeURIComponent(projectName)}/workflows/migrate`, {
      method: "POST",
      body: JSON.stringify({ workspace_id: "default" }),
    });
  }

  static async getWorkflowNodeLogs(
    runId: string,
    nodeKey: string,
  ): Promise<{ items: WorkflowNodeLogEntry[] }> {
    return this.request(
      `/workflow-runs/${encodeURIComponent(runId)}/nodes/${encodeURIComponent(nodeKey)}/log`,
    );
  }
}

export { API };
