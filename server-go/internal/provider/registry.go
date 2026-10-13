package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// 内置供应商注册表：backend registry 键清单、(provider, media) → 构造规格、逐模型元数据与查询 API。
//
// 真相源划分（与 Python 侧一致，此处不得复制第二份声明）：
//   - 视频输入模式（t2v / i2v / r2v）与参考图上限等执行期能力归各 backend 的 VideoCapabilities
//     （capabilities.go），本表只带编排与展示口径的元数据（media_type、default、supported_durations、
//     resolutions 等）；补一份视频输入模式或参考图上限即引入第三份手写来源；
//   - 图片能力位（text_to_image / image_to_image）与音频 text_to_speech 的真相源是本表的
//     capabilities token，供能力桶判定与 UI 能力线读取；
//   - 定价声明不在本层，由计费侧持有（本表不镜像 pricing 字段）。
//
// fail-loud：未登记的 provider / model / 规格一律不回落不猜测；时长支持与否不在本层判定——
// SupportedDurations 为空表示未声明（不表示不支持），越界时长的 fail-loud 由消费点（编排守卫）
// 完成，不做「取最近档」等隐式回落。

// ModelCapability 是能力 token 的封闭词汇表取值：仅收录有消费方的 token，词汇与各媒体 backend
// 能力枚举同名同义。新增 token 先有消费方再入表——无人读取的声明是伪装成数据的注释。
type ModelCapability string

const (
	ModelCapTextGeneration   ModelCapability = "text_generation"
	ModelCapStructuredOutput ModelCapability = "structured_output" // 消费点：文本 backend 结构化输出探测
	ModelCapVision           ModelCapability = "vision"            // 消费点：文本解析的 vision 闸
	ModelCapTextToImage      ModelCapability = "text_to_image"     // 消费点：图片能力桶判定
	ModelCapImageToImage     ModelCapability = "image_to_image"    // 消费点：同上
	ModelCapGenerateAudio    ModelCapability = "generate_audio"    // 消费点：音轨开关判定（语义见 ModelAudioSwitchControllable）
	ModelCapTextToSpeech     ModelCapability = "text_to_speech"
)

// modelCapabilityMedia 是各 token 允许挂载的媒体类型：token 与媒体 backend 能力枚举同名同义，
// 挂错媒体即注册表作者笔误，装载期直接拦截。
var modelCapabilityMedia = map[ModelCapability]MediaType{
	ModelCapTextGeneration:   MediaText,
	ModelCapStructuredOutput: MediaText,
	ModelCapVision:           MediaText,
	ModelCapTextToImage:      MediaImage,
	ModelCapImageToImage:     MediaImage,
	ModelCapGenerateAudio:    MediaVideo,
	ModelCapTextToSpeech:     MediaAudio,
}

// ModelInfo 单模型元数据。
type ModelInfo struct {
	DisplayName string
	MediaType   MediaType
	// Capabilities 能力 token（词汇见 ModelCapability）。图片模型的 text_to_image / image_to_image
	// 是能力桶判定的真相源；视频模型的输入模式与参考图上限一概不在此声明——它们的真相源是各
	// backend 的 VideoCapabilities 与请求期 gate，与请求构造同源。
	Capabilities []ModelCapability
	// AudioAlwaysOn：成片恒有声且请求参数里没有可下发的音轨开关。与 generate_audio token 互斥——
	// token 表达「开关可控」，本位表达「不可控且恒开」，两者同时声明自相矛盾。按型号声明而非按
	// 供应商：同一供应商名下可以部分型号恒有声、部分型号可开关或无声。
	AudioAlwaysOn bool
	Default       bool
	// SupportedDurations 视频模型的声明时长（秒）；空 = 未声明。本层只暴露声明本身，
	// 非法时长的 fail-loud 由消费点完成。
	SupportedDurations []int
	// DurationResolutionConstraints 声明「指定分辨率下只允许这些时长」（如 1080p 仅 8 秒）；
	// 空 = 无分辨率维度约束。
	DurationResolutionConstraints map[string][]int
	// ReferenceImageDurations 使用参考图（参考生视频）时允许的时长；空 = 参考图路径不额外约束。
	ReferenceImageDurations []int
	Resolutions             []string
	// APIModelName 发给供应商 API 的模型名；空时回退 registry 键名。两栖模型（同一 API 模型名同时
	// 有图像 / 视频两个条目）用此字段让两条目共用一个 API 模型名，而 registry 键名各自唯一。
	APIModelName string
	// Hidden 从 UI 下拉剔除但保留条目（供「入队后、finish 前被下线」的边角仍能算价）。
	Hidden bool
}

// Has 报告是否声明了某能力 token。
func (m ModelInfo) Has(capability ModelCapability) bool {
	for _, c := range m.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// ModelEntry 一个模型条目：registry 键 + 元数据。ProviderMeta.Models 按登记顺序保存切片
// （Python dict 插入序），默认模型查找与 UI 排序依赖该顺序。
type ModelEntry struct {
	ID   string
	Info ModelInfo
}

// ProviderMeta 单供应商元数据（凭证键、模型表、默认域名与控制台地址）。
type ProviderMeta struct {
	DisplayName string
	Description string
	// RequiredKeys：必填凭证键；OptionalKeys：可选配置键；SecretKeys：写库时按密文存储的键。
	RequiredKeys   []string
	OptionalKeys   []string
	SecretKeys     []string
	Models         []ModelEntry
	DefaultBaseURL string
	// ConsoleURL 供应商控制台地址：用于「未配置」状态下引导用户申请 API Key；空则不渲染入口。
	ConsoleURL string
	// CredentialGroups 凭证「二选一」分组：非空时凭证表单按「满足任一组」校验（组内字段全填）。
	// 目前仅可灵需要（api_key 单键 / access_key+secret_key 双键二选一）；空表示默认「required ∩
	// secret 全填」。声明的每个 key 须是 required ∩ secret 的子集，装载期校验。
	CredentialGroups [][]string
	// DefaultConcurrency 按 lane（image / video / audio）声明的出厂默认并发上限；某条 lane
	// 未列入则走全局默认。声明给不支持的 lane 无害——容量投影会按 media_types 把不支持的 lane
	// 置 0。
	DefaultConcurrency map[string]int
}

// Model 按 registry 键取模型元数据。
func (m ProviderMeta) Model(modelID string) (ModelInfo, bool) {
	for _, entry := range m.Models {
		if entry.ID == modelID {
			return entry.Info, true
		}
	}
	return ModelInfo{}, false
}

// MediaTypes 返回本 provider 名下出现过的媒体类型（去重、排序）。
func (m ProviderMeta) MediaTypes() []MediaType {
	seen := make(map[MediaType]struct{}, 4)
	for _, entry := range m.Models {
		seen[entry.Info.MediaType] = struct{}{}
	}
	out := make([]MediaType, 0, len(seen))
	for media := range seen {
		out = append(out, media)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Capabilities 返回本 provider 名下声明过的全部能力 token（去重、排序）。
func (m ProviderMeta) Capabilities() []ModelCapability {
	seen := make(map[ModelCapability]struct{})
	for _, entry := range m.Models {
		for _, capability := range entry.Info.Capabilities {
			seen[capability] = struct{}{}
		}
	}
	out := make([]ModelCapability, 0, len(seen))
	for capability := range seen {
		out = append(out, capability)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// FullyCoveredCredentialGroups 返回本次提交「完整覆盖」的凭证组（组内所有 key 在 values 中均非空）。
//
// 驱动凭证创建/更新端点的切组判定：未声明 CredentialGroups 的 provider（绝大多数）该列表恒为空，
// 调用方据此保持「不做切组处理」的原语义不变。返回的组切片与注册表共享存储，调用方只读。
func (m ProviderMeta) FullyCoveredCredentialGroups(values map[string]string) [][]string {
	var covered [][]string
	for _, group := range m.CredentialGroups {
		complete := true
		for _, key := range group {
			if values[key] == "" {
				complete = false
				break
			}
		}
		if complete {
			covered = append(covered, group)
		}
	}
	return covered
}

// ProviderShape 内置 provider 的 backend 构造形态：决定后续波次的装载闭包走哪条通路，
// 不在闭包内按名字 if（形态是声明的，不是推导的）。
type ProviderShape string

const (
	// ShapeSimple 简单族：api_key + model + base_url（外加 ExtraKeys 声明的直传键）。
	ShapeSimple ProviderShape = "simple"
	// ShapeGemini gemini 特例族：gemini-aistudio / gemini-vertex 同映射 "gemini" backend，
	// 差异只在 backend_type，由各 spec 行声明。
	ShapeGemini ProviderShape = "gemini"
	// ShapeKling 可灵特例族：api_key（Bearer）与 access_key+secret_key（JWT）二选一，
	// 图像侧额外注入 api_model_name 解耦两栖别名键。
	ShapeKling ProviderShape = "kling"
	// ShapeOpenAIImage openai 图片特例：多消费一个单请求超时配置键（键名与构造参数名不同）。
	ShapeOpenAIImage ProviderShape = "openai_image"
	// ShapeTextSimple 简单文本族：registry_backend 即 provider_id 自身。
	ShapeTextSimple ProviderShape = "text_simple"
	// ShapeTextGemini gemini 文本族：aistudio / vertex 按 provider_id 分派，registry_backend 同为 "gemini"。
	ShapeTextGemini ProviderShape = "text_gemini"
	// ShapeTextOpenAICompat OpenAI 兼容文本族：openai 直传 base_url；dashscope / minimax
	// 经 helper 派生 base_url 并透传 provider_name 计费归因，registry_backend 同为 "openai"。
	ShapeTextOpenAICompat ProviderShape = "text_openai_compat"
)

// ProviderSpec 单条内置 (provider, media) 的 backend 构造规格。
type ProviderSpec struct {
	// ProviderID registry / config provider id，如 "ark"。
	ProviderID string
	MediaType  MediaType
	// RegistryBackend 映射到哪个 media backend registry 名。与 ProviderID 不必相同：
	// gemini-aistudio / gemini-vertex 同映射 "gemini"；dashscope / minimax 文本经 "openai" 别名。
	RegistryBackend string
	Shape           ProviderShape
	// ExtraKeys 除 api_key / model / base_url 外额外消费的凭证键，键名即构造参数名
	// （如 dashscope 视频的 wan3_base_url）。
	ExtraKeys []string
}

// BackendEntry 一条内置 backend 的记录（registry 键身份与实现类名；实现随后续波次落地）。
type BackendEntry struct {
	// Key registry 键，与 provider_id 不必相同（gemini-aistudio / gemini-vertex 同映射到 "gemini"）。
	Key string
	// ClassName 实现类名，随 Python 侧对称命名，供诊断与 UI 引用。
	ClassName string
	// Registered 是否在运行时 registry 注册。未注册者仅作自定义供应商 delegate 使用，
	// 不可经 create 查表构造。
	Registered bool
}

// JobIDPersister 把 submit 拿到的 provider_job_id（+提交端点）立即落库，供重启续跑定位远端 job。
//
// 由装配层注入：backend 层不依赖 queue / repository（分层契约 providers 不得反向依赖 service）。
// 返回错误视为整笔提交失败（fail-fast，ADR 0007）——未持久化的 submit 无法续跑，宁可原地失败。
type JobIDPersister func(ctx context.Context, taskID, jobID string, endpoint *string) error

// APICallIDPersister 把记账行的 id 写回 task payload，供 resume 精准翻 pending 记账（幂等）。
type APICallIDPersister func(ctx context.Context, taskID string, callID int64) error

// BackendOptions 是构造内置 backend 的通用输入信封：简单族消费 Model / APIKey / BaseURL，
// 专属族额外读 Extra 里声明的键（键名即构造参数名）。字段留空表示未配置，由后端自行兜底
// （环境变量或 fail-loud）——与 Python 侧「仅非空才写入 kwargs」同义。
//
// PersistJobID / PersistAPICallID 是 submit 后落库的注入点：nil 表示非 worker 路径
// （grid / 直生 / 测试），backend 据此跳过持久化，与 Python 侧 ProviderJobIdPersistenceMixin
// 「task_id 为 None 则跳过」同义。
type BackendOptions struct {
	Model   string
	APIKey  string
	BaseURL string
	Extra   map[string]string

	PersistJobID     JobIDPersister
	PersistAPICallID APICallIDPersister
}

// VideoBackendRegistration 一条视频 backend 注册记录：构造闭包 + 可选静态查询。
//
// 静态查询对应 Python 侧 backend 类上的同名静态方法：不构造实例、无需 api_key，供展示层与
// 计费估算读能力 / 计价参数，使 backend 保持唯一真相源。
type VideoBackendRegistration struct {
	// Create 构造实例。
	Create func(opts BackendOptions) (VideoBackend, error)
	// CapabilitiesForModel 纯查表声明某 model 的视频能力；nil 表示该 backend 不提供。
	CapabilitiesForModel func(modelID string) (VideoCapabilities, error)
	// EffectiveGenerateAudioForModel 默认执行档下的有效 generate_audio 计价参数；
	// nil 表示未声明专属规则，沿用请求值（true）。
	EffectiveGenerateAudioForModel func(modelID string) (bool, error)
}

// ImageBackendRegistration 一条图片 backend 注册记录。
type ImageBackendRegistration struct {
	Create func(opts BackendOptions) (ImageBackend, error)
}

// AudioBackendRegistration 一条音频 backend 注册记录。
type AudioBackendRegistration struct {
	Create func(opts BackendOptions) (AudioBackend, error)
}

// 运行时 backend registry：注册在装载期（init / 启动装配）完成，之后只读；加锁只为防启动期
// 多 goroutine 装配的数据竞争，不承诺热插拔。
var (
	backendRegistryMu sync.RWMutex

	videoBackendFactories = map[string]VideoBackendRegistration{}
	videoBackendOrder     []string
	imageBackendFactories = map[string]ImageBackendRegistration{}
	imageBackendOrder     []string
	audioBackendFactories = map[string]AudioBackendRegistration{}
	audioBackendOrder     []string
)

// RegisterVideoBackend 注册一个视频 backend（同名重复注册覆盖旧记录，登记位置不变）。
func RegisterVideoBackend(key string, registration VideoBackendRegistration) {
	backendRegistryMu.Lock()
	defer backendRegistryMu.Unlock()
	if _, exists := videoBackendFactories[key]; !exists {
		videoBackendOrder = append(videoBackendOrder, key)
	}
	videoBackendFactories[key] = registration
}

// RegisterImageBackend 注册一个图片 backend（同名重复注册覆盖旧记录，登记位置不变）。
func RegisterImageBackend(key string, registration ImageBackendRegistration) {
	backendRegistryMu.Lock()
	defer backendRegistryMu.Unlock()
	if _, exists := imageBackendFactories[key]; !exists {
		imageBackendOrder = append(imageBackendOrder, key)
	}
	imageBackendFactories[key] = registration
}

// RegisterAudioBackend 注册一个音频 backend（同名重复注册覆盖旧记录，登记位置不变）。
func RegisterAudioBackend(key string, registration AudioBackendRegistration) {
	backendRegistryMu.Lock()
	defer backendRegistryMu.Unlock()
	if _, exists := audioBackendFactories[key]; !exists {
		audioBackendOrder = append(audioBackendOrder, key)
	}
	audioBackendFactories[key] = registration
}

// RegisteredVideoBackendKeys 返回已注册的视频 backend 键（注册顺序）。
func RegisteredVideoBackendKeys() []string {
	backendRegistryMu.RLock()
	defer backendRegistryMu.RUnlock()
	return append([]string(nil), videoBackendOrder...)
}

// RegisteredImageBackendKeys 返回已注册的图片 backend 键（注册顺序）。
func RegisteredImageBackendKeys() []string {
	backendRegistryMu.RLock()
	defer backendRegistryMu.RUnlock()
	return append([]string(nil), imageBackendOrder...)
}

// RegisteredAudioBackendKeys 返回已注册的音频 backend 键（注册顺序）。
func RegisteredAudioBackendKeys() []string {
	backendRegistryMu.RLock()
	defer backendRegistryMu.RUnlock()
	return append([]string(nil), audioBackendOrder...)
}

// CreateVideoBackend 按注册键构造视频 backend；未注册的键报错，不回落默认实现。
func CreateVideoBackend(key string, opts BackendOptions) (VideoBackend, error) {
	backendRegistryMu.RLock()
	registration, ok := videoBackendFactories[key]
	backendRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown video backend: %s", key)
	}
	return registration.Create(opts)
}

// CreateImageBackend 按注册键构造图片 backend；未注册的键报错。
func CreateImageBackend(key string, opts BackendOptions) (ImageBackend, error) {
	backendRegistryMu.RLock()
	registration, ok := imageBackendFactories[key]
	backendRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown image backend: %s", key)
	}
	return registration.Create(opts)
}

// CreateAudioBackend 按注册键构造音频 backend；未注册的键报错。
func CreateAudioBackend(key string, opts BackendOptions) (AudioBackend, error) {
	backendRegistryMu.RLock()
	registration, ok := audioBackendFactories[key]
	backendRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown audio backend: %s", key)
	}
	return registration.Create(opts)
}

// VideoCapabilitiesForModel 读某 backend 对某 model 声明的视频能力——纯查表，不构造实例、无需 api_key。
//
// backend 键是 registry 名（内置侧由 ProviderSpec.RegistryBackend 给出，与 provider_id 不必
// 相同：gemini-aistudio / gemini-vertex 同映射 "gemini"）。展示层解析内置模型的布尔能力位走这里，
// 使 backend 保持唯一真相源，不在注册表复制第二份能力声明。
func VideoCapabilitiesForModel(backendKey, modelID string) (VideoCapabilities, error) {
	backendRegistryMu.RLock()
	registration, ok := videoBackendFactories[backendKey]
	backendRegistryMu.RUnlock()
	if !ok {
		return VideoCapabilities{}, fmt.Errorf("unknown video backend: %s", backendKey)
	}
	if registration.CapabilitiesForModel == nil {
		return VideoCapabilities{}, fmt.Errorf(
			"video backend %q does not declare video_capabilities_for_model", backendKey,
		)
	}
	return registration.CapabilitiesForModel(modelID)
}

// EffectiveGenerateAudioForModel 读 backend 对默认执行档声明的有效 generate_audio 计价参数。
//
// 未声明专属规则的 backend 沿用请求值（true）；需要按 model / 默认档收窄的 backend 提供静态
// 查询，避免估算侧复制执行实现。
func EffectiveGenerateAudioForModel(backendKey, modelID string) (bool, error) {
	backendRegistryMu.RLock()
	registration, ok := videoBackendFactories[backendKey]
	backendRegistryMu.RUnlock()
	if !ok {
		return false, fmt.Errorf("unknown video backend: %s", backendKey)
	}
	if registration.EffectiveGenerateAudioForModel == nil {
		return true, nil
	}
	return registration.EffectiveGenerateAudioForModel(modelID)
}

// VideoDelegate 是自定义供应商包装层的协议级视频执行体（声明式 delegate 或内置 backend）：
// 包装层只做转发、覆写 name / model 与记住 endpoint，故其形状与 VideoBackend 一致。
type VideoDelegate = VideoBackend

// ImageDelegate 是自定义供应商包装层的协议级图片执行体。
type ImageDelegate = ImageBackend

// AudioDelegate 是自定义供应商包装层的协议级音频执行体。
type AudioDelegate = AudioBackend

// CustomVideoBackend 是自定义供应商的视频包装形态：名称/模型覆写为 provider 侧值，并记住构造
// 所用的 endpoint（endpoint 决定协议，续跑须比对，见 ResumeEndpointChangedError）。
type CustomVideoBackend interface {
	VideoBackend
	// Endpoint 返回构造本 backend 所用的 endpoint 键；绕过工厂直接构造时为空串
	// （此时续跑不比对，与未持久化 endpoint 的存量任务一致）。
	Endpoint() string
	// Delegate 返回被包装的协议级执行体（用于读取其静态声明等）。
	Delegate() VideoDelegate
}

// CustomImageBackend 是自定义供应商的图片包装形态：名称/模型覆写为 provider 侧值。
type CustomImageBackend interface {
	ImageBackend
	Delegate() ImageDelegate
}

// CustomAudioBackend 是自定义供应商的音频包装形态：名称/模型覆写为 provider 侧值。
type CustomAudioBackend interface {
	AudioBackend
	Delegate() AudioDelegate
}

// 自定义供应商包装层的其余形状（interfaces only，实现随后续波次）：
//   - Declarative{Text,Image,Video,Audio}Delegate 按 endpoint 数据声明渲染 submit / poll，
//     形状与各 Delegate 一致（视频另含 Generate 的同步下载路径）；endpoint 声明数据、
//     出站校验与 Custom{Text,Image,Video,Audio}Backend 的工厂装配随后落地；
//   - 文本侧包装（CustomTextBackend / DeclarativeTextDelegate）随文本 backend 契约一并延后。

// alwaysAudibleWithoutTokenProviders：generate_audio token 语义是「音轨开关可控」，不是
// 「有无音轨」——这几家的视频恒有声但请求参数无法关闭该开关，故注册表不为其声明该 token
// （声明了会误导调用方以为开关生效）。ModelHasAudioTrack 据此在 token 判定之外单列例外，
// 供 voice_consistency 派生与前端能力线渲染共用，防止两处各自维护一份漂移的判断。
var alwaysAudibleWithoutTokenProviders = map[string]struct{}{
	"gemini-aistudio": {},
	"grok":            {},
	"dashscope":       {},
	"openai":          {},
}

// ModelHasAudioTrack 报告该视频 model 生成的成片是否带音轨（不等于「音轨开关可控」，
// 见 alwaysAudibleWithoutTokenProviders 的语义注）。非视频模型恒 false。
func ModelHasAudioTrack(providerID string, info ModelInfo) bool {
	if info.MediaType != MediaVideo {
		return false
	}
	if info.AudioAlwaysOn || info.Has(ModelCapGenerateAudio) {
		return true
	}
	_, alwaysAudible := alwaysAudibleWithoutTokenProviders[providerID]
	return alwaysAudible
}

// ModelAudioSwitchControllable 报告请求参数能否控制该视频 model 的音轨开关（即 generate_audio
// token 的字面语义）。与 ModelHasAudioTrack 共同构成音轨的两位描述：可控 → 用户的开关有效；
// 不可控时再看有无音轨，区分「恒有声」与「恒无声」。设置界面的开关禁用态与入队前的无声请求校验
// 都读这两位，不各自解读 token。
func ModelAudioSwitchControllable(info ModelInfo) bool {
	if info.MediaType != MediaVideo {
		return false
	}
	return info.Has(ModelCapGenerateAudio)
}

// ModelAudioAlwaysOn 报告成片恒有声且开关不可控——请求里没有可下发的音轨开关，关闭音频的意图
// 必然落空。
func ModelAudioAlwaysOn(providerID string, info ModelInfo) bool {
	return (info.AudioAlwaysOn || ModelHasAudioTrack(providerID, info)) && !ModelAudioSwitchControllable(info)
}

// 内置 backend registry 键清单：Key 与 ProviderMeta / config provider id 不必相同——
// gemini-aistudio / gemini-vertex 两个 provider_id 同映射到 "gemini"；ark 与 ark-agent-plan
// 两个键同用 Ark backend 实现。
var (
	videoBackendInventory = []BackendEntry{
		{Key: "gemini", ClassName: "GeminiVideoBackend", Registered: true},
		{Key: "ark", ClassName: "ArkVideoBackend", Registered: true},
		{Key: "ark-agent-plan", ClassName: "ArkVideoBackend", Registered: true},
		{Key: "grok", ClassName: "GrokVideoBackend", Registered: true},
		{Key: "openai", ClassName: "OpenAIVideoBackend", Registered: true},
		{Key: "newapi", ClassName: "NewAPIVideoBackend", Registered: true},
		{Key: "vidu", ClassName: "ViduVideoBackend", Registered: true},
		{Key: "dashscope", ClassName: "DashScopeVideoBackend", Registered: true},
		{Key: "minimax", ClassName: "MiniMaxVideoBackend", Registered: true},
		{Key: "kling", ClassName: "KlingVideoBackend", Registered: true},
		{Key: "agnes", ClassName: "AgnesVideoBackend", Registered: true},
		// v2 通用视频端点（/v2/video/generations「流派 C」）：不注册，仅作自定义供应商
		// 声明式端点的 delegate 使用。
		{Key: "v2_video_generations", ClassName: "V2VideoGenerationsBackend", Registered: false},
	}
	imageBackendInventory = []BackendEntry{
		{Key: "gemini", ClassName: "GeminiImageBackend", Registered: true},
		{Key: "ark", ClassName: "ArkImageBackend", Registered: true},
		{Key: "ark-agent-plan", ClassName: "ArkImageBackend", Registered: true},
		{Key: "grok", ClassName: "GrokImageBackend", Registered: true},
		{Key: "openai", ClassName: "OpenAIImageBackend", Registered: true},
		{Key: "vidu", ClassName: "ViduImageBackend", Registered: true},
		{Key: "dashscope", ClassName: "DashScopeImageBackend", Registered: true},
		{Key: "minimax", ClassName: "MiniMaxImageBackend", Registered: true},
		{Key: "kling", ClassName: "KlingImageBackend", Registered: true},
		{Key: "agnes", ClassName: "AgnesImageBackend", Registered: true},
	}
	audioBackendInventory = []BackendEntry{
		{Key: "dashscope", ClassName: "DashScopeAudioBackend", Registered: true},
		// openai 兼容语音合成：不注册，仅作自定义供应商端点的 delegate 使用（同步
		// /v1/audio/speech）。
		{Key: "openai", ClassName: "OpenAIAudioBackend", Registered: false},
	}
)

// VideoBackendInventory 返回视频 backend 键清单（含未注册者，Registered 标注是否可经 create 构造）。
func VideoBackendInventory() []BackendEntry {
	return append([]BackendEntry(nil), videoBackendInventory...)
}

// ImageBackendInventory 返回图片 backend 键清单。
func ImageBackendInventory() []BackendEntry {
	return append([]BackendEntry(nil), imageBackendInventory...)
}

// AudioBackendInventory 返回音频 backend 键清单。
func AudioBackendInventory() []BackendEntry {
	return append([]BackendEntry(nil), audioBackendInventory...)
}

// VideoBackendEntry 按 registry 键取视频 backend 记录。
func VideoBackendEntry(key string) (BackendEntry, bool) {
	return backendEntryFor(videoBackendInventory, key)
}

// ImageBackendEntry 按 registry 键取图片 backend 记录。
func ImageBackendEntry(key string) (BackendEntry, bool) {
	return backendEntryFor(imageBackendInventory, key)
}

// AudioBackendEntry 按 registry 键取音频 backend 记录。
func AudioBackendEntry(key string) (BackendEntry, bool) {
	return backendEntryFor(audioBackendInventory, key)
}

func backendEntryFor(inventory []BackendEntry, key string) (BackendEntry, bool) {
	for _, entry := range inventory {
		if entry.Key == key {
			return entry, true
		}
	}
	return BackendEntry{}, false
}

// validMediaTypes 是构造规格表允许的 media_type（text 非媒体通道，仅数据层面登记）。
var validMediaTypes = map[MediaType]struct{}{
	MediaImage: {},
	MediaVideo: {},
	MediaAudio: {},
	MediaText:  {},
}

// simpleSpec 登记一条简单族规格：registry_backend 即 provider_id 自身（媒体侧无别名映射）。
func simpleSpec(providerID string, media MediaType, extraKeys ...string) ProviderSpec {
	return ProviderSpec{
		ProviderID:      providerID,
		MediaType:       media,
		RegistryBackend: providerID,
		Shape:           ShapeSimple,
		ExtraKeys:       extraKeys,
	}
}

// geminiSpec 登记一条 gemini 特例规格：两个 provider_id 各按 backend_type 分一行，
// registry_backend 同为 "gemini"。
func geminiSpec(providerID string, media MediaType) ProviderSpec {
	return ProviderSpec{
		ProviderID:      providerID,
		MediaType:       media,
		RegistryBackend: "gemini",
		Shape:           ShapeGemini,
	}
}

// klingSpec 登记一条可灵规格。
func klingSpec(media MediaType) ProviderSpec {
	return ProviderSpec{
		ProviderID:      "kling",
		MediaType:       media,
		RegistryBackend: "kling",
		Shape:           ShapeKling,
	}
}

// textSimpleSpec 登记一条简单文本规格。
func textSimpleSpec(providerID string) ProviderSpec {
	return ProviderSpec{
		ProviderID:      providerID,
		MediaType:       MediaText,
		RegistryBackend: providerID,
		Shape:           ShapeTextSimple,
	}
}

// textGeminiSpec 登记一条 gemini 文本规格。
func textGeminiSpec(providerID string) ProviderSpec {
	return ProviderSpec{
		ProviderID:      providerID,
		MediaType:       MediaText,
		RegistryBackend: "gemini",
		Shape:           ShapeTextGemini,
	}
}

// textOpenAICompatSpec 登记一条 OpenAI 兼容文本规格（openai 直传；dashscope / minimax 别名映射）。
func textOpenAICompatSpec(providerID string) ProviderSpec {
	return ProviderSpec{
		ProviderID:      providerID,
		MediaType:       MediaText,
		RegistryBackend: "openai",
		Shape:           ShapeTextOpenAICompat,
	}
}

// builtinProviderSpecs 内置 (provider, media) → 构造规格，逐对显式登记、fail-loud（未登记的
// provider × media 查表报错，不「缺席即默认」）。只登记确有 backend 的对。
var builtinProviderSpecs = []ProviderSpec{
	// ── 简单族：image / video ───────────────────────────────────────
	simpleSpec("ark", MediaImage),
	simpleSpec("ark", MediaVideo),
	simpleSpec("ark-agent-plan", MediaImage),
	simpleSpec("ark-agent-plan", MediaVideo),
	simpleSpec("grok", MediaImage),
	simpleSpec("grok", MediaVideo),
	simpleSpec("openai", MediaVideo),
	simpleSpec("vidu", MediaImage),
	simpleSpec("vidu", MediaVideo),
	simpleSpec("dashscope", MediaImage),
	simpleSpec("minimax", MediaImage),
	simpleSpec("minimax", MediaVideo),
	simpleSpec("agnes", MediaImage),
	simpleSpec("agnes", MediaVideo),
	// dashscope 视频比简单族多一个 wan3_base_url：万相 3.0 走独立 maas 域名（含地域与
	// workspace，推不出），未配置时 backend 自行回落通用 base_url。
	simpleSpec("dashscope", MediaVideo, "wan3_base_url"),
	// ── 音频 ────────────────────────────────────────────────────────
	simpleSpec("dashscope", MediaAudio),
	// ── gemini 特例族 ───────────────────────────────────────────────
	geminiSpec("gemini-aistudio", MediaImage),
	geminiSpec("gemini-aistudio", MediaVideo),
	geminiSpec("gemini-vertex", MediaImage),
	geminiSpec("gemini-vertex", MediaVideo),
	// ── kling 特例族 ────────────────────────────────────────────────
	klingSpec(MediaImage),
	klingSpec(MediaVideo),
	// ── openai 图片特例：多消费 image_request_timeout_seconds 配置键 ──
	{
		ProviderID:      "openai",
		MediaType:       MediaImage,
		RegistryBackend: "openai",
		Shape:           ShapeOpenAIImage,
	},
	// ── 文本族 ──────────────────────────────────────────────────────
	textSimpleSpec("ark"),
	textSimpleSpec("ark-agent-plan"),
	textSimpleSpec("grok"),
	textSimpleSpec("agnes"),
	textGeminiSpec("gemini-aistudio"),
	textGeminiSpec("gemini-vertex"),
	textOpenAICompatSpec("openai"),
	textOpenAICompatSpec("dashscope"),
	textOpenAICompatSpec("minimax"),
}

// builtinProviderSpecByKey 按 (provider, media) 索引构造规格；装载期建立。
var builtinProviderSpecByKey map[providerMediaKey]ProviderSpec

type providerMediaKey struct {
	ProviderID string
	MediaType  MediaType
}

// ProviderSpecFor 按 (provider, media) 取内置构造规格；未登记的 pair 返回 ok=false，
// 不回落任何默认形态。
func ProviderSpecFor(providerID string, media MediaType) (ProviderSpec, bool) {
	spec, ok := builtinProviderSpecByKey[providerMediaKey{ProviderID: providerID, MediaType: media}]
	return spec, ok
}

// ProviderSpecs 返回全部内置构造规格（登记顺序）。
func ProviderSpecs() []ProviderSpec {
	return append([]ProviderSpec(nil), builtinProviderSpecs...)
}

// builtinProviderOrder 是内置 provider 的注册表声明顺序（Python dict 插入序）。
var builtinProviderOrder = []string{
	"gemini-aistudio",
	"gemini-vertex",
	"ark",
	"ark-agent-plan",
	"grok",
	"openai",
	"vidu",
	"dashscope",
	"minimax",
	"kling",
	"agnes",
}

// builtinProviders 内置 provider 元数据表。
var builtinProviders = map[string]ProviderMeta{
	"gemini-aistudio": {
		DisplayName:  "AI Studio",
		ConsoleURL:   "https://aistudio.google.com/apikey",
		Description:  "Google AI Studio 提供 Gemini 系列模型，支持图片和视频生成，适合快速原型和个人项目。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{
			"base_url", "image_rpm", "video_rpm", "request_gap", "image_max_workers", "video_max_workers",
		},
		SecretKeys: []string{"api_key"},
		Models: []ModelEntry{
			{ID: "gemini-3.1-pro-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3.1 Pro",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "gemini-3-flash-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3 Flash",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
				Default:      true,
			}},
			{ID: "gemini-3.1-flash-lite-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3.1 Flash Lite",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			{ID: "gemini-3-pro-image-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3 Pro Image",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Resolutions:  []string{"1K", "2K", "4K"},
			}},
			{ID: "gemini-3.1-flash-image-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3.1 Flash Image",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"1K", "2K", "4K"},
			}},
			// Veo 的分辨率↔时长、参考图↔时长约束按 Gemini API 文档参数表逐型号声明：
			// durationSeconds 在 reference images 与 1080p/4k 下必须为 8。Lite 未见于该参数表，
			// 其 4k 不支持取自 AI Studio 定价页明文；两条时长约束沿用同代 Veo 3.1 的行为。
			{ID: "veo-3.1-generate-preview", Info: ModelInfo{
				DisplayName:                   "Veo 3.1",
				MediaType:                     MediaVideo,
				AudioAlwaysOn:                 true,
				SupportedDurations:            []int{4, 6, 8},
				DurationResolutionConstraints: map[string][]int{"1080p": {8}, "4k": {8}},
				ReferenceImageDurations:       []int{8},
				Resolutions:                   []string{"720p", "1080p", "4k"},
			}},
			{ID: "veo-3.1-fast-generate-preview", Info: ModelInfo{
				DisplayName:                   "Veo 3.1 Fast",
				MediaType:                     MediaVideo,
				AudioAlwaysOn:                 true,
				SupportedDurations:            []int{4, 6, 8},
				DurationResolutionConstraints: map[string][]int{"1080p": {8}, "4k": {8}},
				ReferenceImageDurations:       []int{8},
				Resolutions:                   []string{"720p", "1080p", "4k"},
			}},
			{ID: "veo-3.1-lite-generate-preview", Info: ModelInfo{
				DisplayName:                   "Veo 3.1 Lite",
				MediaType:                     MediaVideo,
				AudioAlwaysOn:                 true,
				Default:                       true,
				SupportedDurations:            []int{4, 6, 8},
				DurationResolutionConstraints: map[string][]int{"1080p": {8}},
				ReferenceImageDurations:       []int{8},
				Resolutions:                   []string{"720p", "1080p"},
			}},
		},
	},
	"gemini-vertex": {
		DisplayName:  "Vertex AI",
		ConsoleURL:   "https://console.cloud.google.com/vertex-ai",
		Description:  "Google Cloud Vertex AI 企业级平台，支持 Gemini 和 Imagen 模型，提供更高配额和音频生成能力。",
		RequiredKeys: []string{"credentials_path"},
		OptionalKeys: []string{
			"gcs_bucket", "image_rpm", "video_rpm", "request_gap", "image_max_workers", "video_max_workers",
		},
		SecretKeys: []string{},
		Models: []ModelEntry{
			{ID: "gemini-3.1-pro-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3.1 Pro",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "gemini-3-flash-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3 Flash",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
				Default:      true,
			}},
			{ID: "gemini-3.1-flash-lite-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3.1 Flash Lite",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			{ID: "gemini-3-pro-image-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3 Pro Image",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Resolutions:  []string{"1K", "2K", "4K"},
			}},
			{ID: "gemini-3.1-flash-image-preview", Info: ModelInfo{
				DisplayName:  "Gemini 3.1 Flash Image",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"1K", "2K", "4K"},
			}},
			// 分辨率取自 Vertex 各型号文档页：001 型号里只有 standard 列出 4K，fast 仍是
			// 720p/1080p（与 AI Studio 的 preview 型号不同，故两侧声明不对称）。参考图↔时长：
			// standard 页明文「reference image to video only supports 8 seconds」；fast 页未提，
			// 按 Gemini API 文档中同代 Fast 的同一约束声明。分辨率↔时长约束 Vertex 页整体未提，
			// 同样按同代声明，与 backend 的执行期拒绝保持一致。
			{ID: "veo-3.1-generate-001", Info: ModelInfo{
				DisplayName:                   "Veo 3.1",
				MediaType:                     MediaVideo,
				Capabilities:                  []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations:            []int{4, 6, 8},
				DurationResolutionConstraints: map[string][]int{"1080p": {8}, "4k": {8}},
				ReferenceImageDurations:       []int{8},
				Resolutions:                   []string{"720p", "1080p", "4k"},
			}},
			{ID: "veo-3.1-fast-generate-001", Info: ModelInfo{
				DisplayName:                   "Veo 3.1 Fast",
				MediaType:                     MediaVideo,
				Capabilities:                  []ModelCapability{ModelCapGenerateAudio},
				Default:                       true,
				SupportedDurations:            []int{4, 6, 8},
				DurationResolutionConstraints: map[string][]int{"1080p": {8}},
				ReferenceImageDurations:       []int{8},
				Resolutions:                   []string{"720p", "1080p"},
			}},
		},
	},
	"ark": {
		DisplayName:  "火山方舟",
		ConsoleURL:   "https://console.volcengine.com/ark",
		Description:  "字节跳动火山方舟 AI 平台，支持 Seedance 视频生成和 Seedream 图片生成，具备音频生成和种子控制能力。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{"base_url", "video_max_workers", "image_max_workers"},
		SecretKeys:   []string{"api_key"},
		Models: []ModelEntry{
			{ID: "doubao-seed-2-0-pro-260215", Info: ModelInfo{
				DisplayName:  "豆包 Seed 2.0 Pro",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapVision},
			}},
			{ID: "doubao-seed-2-0-lite-260215", Info: ModelInfo{
				DisplayName:  "豆包 Seed 2.0 Lite",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapVision},
				Default:      true,
			}},
			{ID: "doubao-seed-2-0-mini-260215", Info: ModelInfo{
				DisplayName:  "豆包 Seed 2.0 Mini",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapVision},
			}},
			{ID: "doubao-seed-1-8-251228", Info: ModelInfo{
				DisplayName:  "豆包 Seed 1.8",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "doubao-seedream-5-0-lite-260128", Info: ModelInfo{
				DisplayName:  "Seedream 5.0 Lite",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
			}},
			{ID: "doubao-seedream-5-0-260128", Info: ModelInfo{
				DisplayName:  "Seedream 5.0",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
			}},
			{ID: "doubao-seedream-4-5-251128", Info: ModelInfo{
				DisplayName:  "Seedream 4.5",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
			}},
			{ID: "doubao-seedream-4-0-250828", Info: ModelInfo{
				DisplayName:  "Seedream 4.0",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
			}},
			{ID: "doubao-seedance-1-5-pro-251215", Info: ModelInfo{
				DisplayName:        "Seedance 1.5 Pro",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(4, 12),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			{ID: "doubao-seedance-2-0-260128", Info: ModelInfo{
				DisplayName:        "Seedance 2.0",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(4, 15),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			{ID: "doubao-seedance-2-0-fast-260128", Info: ModelInfo{
				DisplayName:        "Seedance 2.0 Fast",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(4, 15),
				Resolutions:        []string{"480p", "720p"},
			}},
			{ID: "doubao-seedance-2-0-mini-260615", Info: ModelInfo{
				DisplayName:        "Seedance 2.0 Mini",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				Default:            true,
				SupportedDurations: durationRange(4, 15),
				Resolutions:        []string{"480p", "720p"},
			}},
			// Seedance 2.5：官方声明 480p/720p 两档、原生 30 秒直出，时长全展开为 4–30 秒离散值；
			// 官方另有 -1（模型自选时长）不登记——它会让请求时长与剧本时长指引脱钩。
			{ID: "doubao-seedance-2-5-260628", Info: ModelInfo{
				DisplayName:        "Seedance 2.5",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(4, 30),
				Resolutions:        []string{"480p", "720p"},
			}},
		},
		DefaultBaseURL: "https://ark.cn-beijing.volces.com/api/v3",
	},
	"ark-agent-plan": {
		DisplayName:  "火山方舟 Agent Plan",
		ConsoleURL:   "https://console.volcengine.com/ark",
		Description:  "火山方舟 Agent Plan 套餐，聚合豆包及多家主流大模型，覆盖文本、图片与视频生成。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{"video_max_workers", "image_max_workers"},
		SecretKeys:   []string{"api_key"},
		Models: []ModelEntry{
			{ID: "doubao-seed-2.0-mini", Info: ModelInfo{
				DisplayName:  "豆包 Seed 2.0 Mini",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapVision},
			}},
			{ID: "doubao-seed-2.0-lite", Info: ModelInfo{
				DisplayName:  "豆包 Seed 2.0 Lite",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapVision},
				Default:      true,
			}},
			{ID: "doubao-seed-2.0-pro", Info: ModelInfo{
				DisplayName:  "豆包 Seed 2.0 Pro",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapVision},
			}},
			{ID: "doubao-seed-2.0-code", Info: ModelInfo{
				DisplayName:  "豆包 Seed 2.0 Code",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration},
			}},
			{ID: "deepseek-v4-flash", Info: ModelInfo{
				DisplayName:  "DeepSeek V4 Flash",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration},
			}},
			{ID: "deepseek-v4-pro", Info: ModelInfo{
				DisplayName:  "DeepSeek V4 Pro",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration},
			}},
			{ID: "glm-5.1", Info: ModelInfo{
				DisplayName:  "GLM 5.1",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration},
			}},
			{ID: "kimi-k2.6", Info: ModelInfo{
				DisplayName:  "Kimi K2.6",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration},
			}},
			{ID: "minimax-m2.7", Info: ModelInfo{
				DisplayName:  "MiniMax M2.7",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration},
			}},
			{ID: "doubao-seedream-5.0-lite", Info: ModelInfo{
				DisplayName:  "Seedream 5.0 Lite",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
			}},
			{ID: "doubao-seedance-1.5-pro", Info: ModelInfo{
				DisplayName:        "Seedance 1.5 Pro",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(4, 12),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			{ID: "doubao-seedance-2.0", Info: ModelInfo{
				DisplayName:        "Seedance 2.0",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(4, 15),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			{ID: "doubao-seedance-2.0-fast", Info: ModelInfo{
				DisplayName:        "Seedance 2.0 Fast",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(4, 15),
				Resolutions:        []string{"480p", "720p"},
			}},
			{ID: "doubao-seedance-2.0-mini", Info: ModelInfo{
				DisplayName:        "Seedance 2.0 Mini",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				Default:            true,
				SupportedDurations: durationRange(4, 15),
				Resolutions:        []string{"480p", "720p"},
			}},
		},
		DefaultBaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3",
	},
	"grok": {
		DisplayName:  "Grok",
		ConsoleURL:   "https://console.x.ai",
		Description:  "xAI Grok 模型，支持视频和图片生成。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{"video_max_workers", "image_max_workers"},
		SecretKeys:   []string{"api_key"},
		Models: []ModelEntry{
			{ID: "grok-4.20-0309-reasoning", Info: ModelInfo{
				DisplayName:  "Grok 4.20 Reasoning",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "grok-4.20-0309-non-reasoning", Info: ModelInfo{
				DisplayName:  "Grok 4.20 Non-Reasoning",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "grok-4-1-fast-reasoning", Info: ModelInfo{
				DisplayName:  "Grok 4.1 Fast Reasoning",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
				Default:      true,
			}},
			{ID: "grok-4-1-fast-non-reasoning", Info: ModelInfo{
				DisplayName:  "Grok 4.1 Fast (Non-Reasoning)",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "grok-imagine-image-pro", Info: ModelInfo{
				DisplayName:  "Grok Imagine Image Pro",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Resolutions:  []string{"1K", "2K"},
			}},
			{ID: "grok-imagine-image", Info: ModelInfo{
				DisplayName:  "Grok Imagine Image",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"1K", "2K"},
			}},
			{ID: "grok-imagine-video", Info: ModelInfo{
				DisplayName:        "Grok Imagine Video",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				Default:            true,
				SupportedDurations: durationRange(1, 15),
				Resolutions:        []string{"480p", "720p"},
			}},
		},
	},
	"openai": {
		DisplayName:  "OpenAI",
		ConsoleURL:   "https://platform.openai.com/api-keys",
		Description:  "OpenAI 官方平台，支持 GPT-5.5 / GPT-5.4 文本、GPT Image 2 图片和 Sora 视频生成。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{"base_url", "image_max_workers", "video_max_workers", "image_request_timeout_seconds"},
		SecretKeys:   []string{"api_key"},
		Models: []ModelEntry{
			{ID: "gpt-5.5", Info: ModelInfo{
				DisplayName:  "GPT-5.5",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "gpt-5.4", Info: ModelInfo{
				DisplayName:  "GPT-5.4",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "gpt-5.4-mini", Info: ModelInfo{
				DisplayName:  "GPT-5.4 Mini",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
				Default:      true,
			}},
			{ID: "gpt-5.4-nano", Info: ModelInfo{
				DisplayName:  "GPT-5.4 Nano",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
			}},
			{ID: "gpt-image-2", Info: ModelInfo{
				DisplayName:  "GPT Image 2",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"512px", "1K", "2K"},
			}},
			// Sora 2 原生含对话音轨，但请求参数里没有音轨开关，故不声明 generate_audio token，
			// 有音轨由 AudioAlwaysOn 表达。
			{ID: "sora-2", Info: ModelInfo{
				DisplayName:        "Sora 2",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				Default:            true,
				SupportedDurations: []int{4, 8, 12},
				Resolutions:        []string{"720p"},
			}},
			{ID: "sora-2-pro", Info: ModelInfo{
				DisplayName:        "Sora 2 Pro",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: []int{4, 8, 12},
				Resolutions:        []string{"720p", "1080p"},
			}},
		},
	},
	"vidu": {
		DisplayName:  "Vidu",
		ConsoleURL:   "https://platform.vidu.cn",
		Description:  "生数科技 Vidu 视频生成平台，支持文生视频、图生视频、首尾帧、参考生视频与参考生图，仅图片与视频能力。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{"base_url", "image_max_workers", "video_max_workers"},
		SecretKeys:   []string{"api_key"},
		Models: []ModelEntry{
			{ID: "viduq2", Info: ModelInfo{
				DisplayName:  "Vidu Q2 Image",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"1080p", "2K", "4K"},
			}},
			{ID: "viduq1", Info: ModelInfo{
				DisplayName:  "Vidu Q1 Image",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapImageToImage},
				Resolutions:  []string{"1080p"},
			}},
			{ID: "viduq3-turbo", Info: ModelInfo{
				DisplayName:        "Vidu Q3 Turbo",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				Default:            true,
				SupportedDurations: durationRange(1, 16),
				// 参考生视频端点的时长下限是 3 秒（文/图生视频仍为 1 起），不收窄会让 r2v 项目的
				// 时长下拉出现 1s / 2s 幽灵档位——选中后被 backend 静默取到 3 秒并按 3 秒计费。
				ReferenceImageDurations: durationRange(3, 16),
				Resolutions:             []string{"540p", "720p", "1080p"},
			}},
			{ID: "viduq3-pro", Info: ModelInfo{
				DisplayName:        "Vidu Q3 Pro",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(1, 16),
				Resolutions:        []string{"540p", "720p", "1080p"},
			}},
			{ID: "viduq3", Info: ModelInfo{
				DisplayName:             "Vidu Q3 (Reference)",
				MediaType:               MediaVideo,
				Capabilities:            []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations:      durationRange(3, 16),
				ReferenceImageDurations: durationRange(3, 16),
				Resolutions:             []string{"540p", "720p", "1080p"},
			}},
			{ID: "vidu2.0", Info: ModelInfo{
				DisplayName:        "Vidu 2.0",
				MediaType:          MediaVideo,
				SupportedDurations: []int{4, 8},
				// 图生/首尾帧端点 8 秒档只出 720p，360p 与 1080p 均仅 4 秒档可选。
				DurationResolutionConstraints: map[string][]int{"360p": {4}, "1080p": {4}},
				// 参考生视频端点时长仅认 4 秒；不收窄会让 r2v 项目的时长下拉出现 8 秒幽灵档位
				// ——选中后被 backend 静默取到 4 秒。
				ReferenceImageDurations: []int{4},
				Resolutions:             []string{"360p", "720p", "1080p"},
			}},
		},
	},
	"dashscope": {
		DisplayName:  "阿里百炼",
		ConsoleURL:   "https://bailian.console.aliyun.com/",
		Description:  "阿里云百炼（Model Studio）全模态平台，支持 Qwen 文本、Qwen-Image / 万相图像与 HappyHorse / 万相视频（含参考生视频）。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{"base_url", "image_max_workers", "video_max_workers", "audio_max_workers"},
		SecretKeys:   []string{"api_key"},
		Models: []ModelEntry{
			{ID: "qwen-plus", Info: ModelInfo{
				DisplayName:  "Qwen Plus",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
				Default:      true,
			}},
			{ID: "qwen3.6-plus", Info: ModelInfo{
				DisplayName:  "Qwen3.6 Plus",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			{ID: "qwen3-max", Info: ModelInfo{
				DisplayName:  "Qwen3 Max",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			{ID: "qwen3.7-max", Info: ModelInfo{
				DisplayName:  "Qwen3.7 Max",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			{ID: "qwen3.6-flash", Info: ModelInfo{
				DisplayName:  "Qwen3.6 Flash",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			{ID: "qwen-long", Info: ModelInfo{
				DisplayName:  "Qwen Long",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			// qwen-image-2.0 融合系列：T2I + I2I 同模型，size 用像素值 宽*高。
			{ID: "qwen-image-2.0", Info: ModelInfo{
				DisplayName:  "Qwen Image 2.0",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"2048*2048", "2688*1536", "1536*2688", "2368*1728", "1728*2368"},
			}},
			{ID: "qwen-image-2.0-pro", Info: ModelInfo{
				DisplayName:  "Qwen Image 2.0 Pro",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Resolutions:  []string{"2048*2048", "2688*1536", "1536*2688", "2368*1728", "1728*2368"},
			}},
			// 编辑专用系列：仅图生图（角色一致性增强）。
			{ID: "qwen-image-edit-plus", Info: ModelInfo{
				DisplayName:  "Qwen Image Edit Plus",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapImageToImage},
				Resolutions:  []string{"2048*2048", "2048*1152", "1152*2048", "2048*1536", "1536*2048"},
			}},
			{ID: "qwen-image-edit-max", Info: ModelInfo{
				DisplayName:  "Qwen Image Edit Max",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapImageToImage},
				Resolutions:  []string{"2048*2048", "2048*1152", "1152*2048", "2048*1536", "1536*2048"},
			}},
			// 万相 2.7 图像系列：size 用档位 1K/2K(/4K)。
			{ID: "wan2.7-image", Info: ModelInfo{
				DisplayName:  "万相 2.7 图像",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Resolutions:  []string{"1K", "2K"},
			}},
			{ID: "wan2.7-image-pro", Info: ModelInfo{
				DisplayName:  "万相 2.7 图像 Pro",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Resolutions:  []string{"1K", "2K", "4K"},
			}},
			// HappyHorse 1.1 系列与 1.0 系列、万相 2.7 视频系列：音频恒开（请求无音轨开关），
			// 故 AudioAlwaysOn 而非 generate_audio token。
			{ID: "happyhorse-1.1-i2v", Info: ModelInfo{
				DisplayName:        "HappyHorse 1.1 图生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				Default:            true,
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			{ID: "happyhorse-1.1-t2v", Info: ModelInfo{
				DisplayName:        "HappyHorse 1.1 文生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			{ID: "happyhorse-1.1-r2v", Info: ModelInfo{
				DisplayName:        "HappyHorse 1.1 参考生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			{ID: "happyhorse-1.0-i2v", Info: ModelInfo{
				DisplayName:        "HappyHorse 1.0 图生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"720p", "1080p"},
			}},
			{ID: "happyhorse-1.0-t2v", Info: ModelInfo{
				DisplayName:        "HappyHorse 1.0 文生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"720p", "1080p"},
			}},
			{ID: "happyhorse-1.0-r2v", Info: ModelInfo{
				DisplayName:        "HappyHorse 1.0 参考生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"720p", "1080p"},
			}},
			{ID: "wan2.7-i2v", Info: ModelInfo{
				DisplayName:        "万相 2.7 图生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: durationRange(2, 15),
				Resolutions:        []string{"720p", "1080p"},
			}},
			{ID: "wan2.7-t2v", Info: ModelInfo{
				DisplayName:        "万相 2.7 文生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: durationRange(2, 15),
				Resolutions:        []string{"720p", "1080p"},
			}},
			{ID: "wan2.7-r2v", Info: ModelInfo{
				DisplayName:        "万相 2.7 参考生视频",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				SupportedDurations: durationRange(2, 15),
				Resolutions:        []string{"720p", "1080p"},
			}},
			// 万相 3.0：单模型覆盖文生/图生/参考生三条路径，单次最长 30 秒。与 2.7 及
			// HappyHorse 不同，音轨由请求参数控制，故声明 generate_audio token 而非 AudioAlwaysOn。
			{ID: "wan3.0-video", Info: ModelInfo{
				DisplayName:        "万相 3.0 视频",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(2, 30),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			// qwen3-tts-flash：同步 HTTP 语音合成，按字符计费。
			{ID: "qwen3-tts-flash", Info: ModelInfo{
				DisplayName:  "Qwen3 TTS Flash",
				MediaType:    MediaAudio,
				Capabilities: []ModelCapability{ModelCapTextToSpeech},
				Default:      true,
			}},
		},
		DefaultBaseURL: "https://dashscope.aliyuncs.com",
	},
	"minimax": {
		DisplayName:  "MiniMax",
		ConsoleURL:   "https://platform.minimaxi.com",
		Description:  "MiniMax（海螺）多模态平台，提供文本、图片、视频生成。默认连接国内站，海外可将 base_url 切换到国际站。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{"base_url", "image_max_workers", "video_max_workers"},
		SecretKeys:   []string{"api_key"},
		Models: []ModelEntry{
			{ID: "MiniMax-M3", Info: ModelInfo{
				DisplayName:  "MiniMax M3",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput, ModelCapVision},
				Default:      true,
			}},
			{ID: "MiniMax-M2.7", Info: ModelInfo{
				DisplayName:  "MiniMax M2.7",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			// image-01：单步同步取 URL，T2I + I2I（subject_reference 单脸参考）。
			{ID: "image-01", Info: ModelInfo{
				DisplayName:  "MiniMax Image 01",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"1K", "2K"},
			}},
			// H3：多模态 v2 端点（content[] 数组），768P/2K × 4–15 秒任意整数，两档分辨率时长
			// 范围一致故无 DurationResolutionConstraints。原生立体声、请求无音轨开关，故恒有声。
			{ID: "MiniMax-H3", Info: ModelInfo{
				DisplayName:        "MiniMax H3",
				MediaType:          MediaVideo,
				AudioAlwaysOn:      true,
				Default:            true,
				SupportedDurations: durationRange(4, 15),
				Resolutions:        []string{"768p", "2k"},
			}},
			// 1080P 仅 6s（10s 仅 768P）；细粒度越界由 backend 抛 VideoCapabilityError，
			// DurationResolutionConstraints 同步给前端做下拉门控。
			{ID: "MiniMax-Hailuo-2.3", Info: ModelInfo{
				DisplayName:                   "MiniMax Hailuo 2.3",
				MediaType:                     MediaVideo,
				SupportedDurations:            []int{6, 10},
				DurationResolutionConstraints: map[string][]int{"1080p": {6}},
				Resolutions:                   []string{"768p", "1080p"},
			}},
			{ID: "MiniMax-Hailuo-2.3-Fast", Info: ModelInfo{
				DisplayName:                   "MiniMax Hailuo 2.3 Fast",
				MediaType:                     MediaVideo,
				SupportedDurations:            []int{6, 10},
				DurationResolutionConstraints: map[string][]int{"1080p": {6}},
				Resolutions:                   []string{"768p", "1080p"},
			}},
			// S2V-01：单张人脸驱动整段视频角色一致性（subject_reference 单脸 R2V）。固定输出
			// 720P/6s，请求不接受 resolution/duration（backend 走专门的 subject_reference 路径，
			// 忽略这两项）；SupportedDurations=[6] 仅供编排层时长守卫与档价口径。
			{ID: "S2V-01", Info: ModelInfo{
				DisplayName:        "MiniMax S2V-01",
				MediaType:          MediaVideo,
				SupportedDurations: []int{6},
				Resolutions:        []string{"768p"},
			}},
		},
		DefaultBaseURL: "https://api.minimaxi.com/v1",
	},
	"kling": {
		DisplayName: "可灵 Kling",
		ConsoleURL:  "https://www.klingai.com/dev/api-key",
		Description: "快手可灵 Kling 视频与图像生成平台。API Key（Bearer）适用于全部模型；" +
			"Access Key + Secret Key（JWT）仅适用于 3.0 及更早模型，二者二选一，同时填写时 API Key 优先。",
		// 首个需要两个 secret 字符串的内置 provider（JWT HS256 鉴权），凭证按 registry key 名
		// 存入 provider_credential 的定型列。
		RequiredKeys: []string{"api_key", "access_key", "secret_key"},
		OptionalKeys: []string{"base_url", "image_max_workers", "video_max_workers"},
		SecretKeys:   []string{"api_key", "access_key", "secret_key"},
		// api_key 单键 / access_key+secret_key 双键二选一（官方 API Key 鉴权全模型可用，
		// AK/SK JWT 仅 3.0 及更早模型）；同时填写时按 api_key 优先分派。
		CredentialGroups: [][]string{{"api_key"}, {"access_key", "secret_key"}},
		// JWT 直连：视频默认 kling-v2-5-turbo（性价比走量）+ v3/v3-omni（旗舰 4K + 人声 +
		// 多图主体）、v2-6（人声，官方限 1080P）、video-o1（多图主体 R2V）；图像 kling-image-o1
		// （默认）+ v3-omni（两栖）。
		Models: []ModelEntry{
			{ID: "kling-v2-5-turbo", Info: ModelInfo{
				DisplayName:        "可灵 2.5 Turbo",
				MediaType:          MediaVideo,
				Default:            true,
				SupportedDurations: []int{5, 10},
				Resolutions:        []string{"720p", "1080p"},
			}},
			{ID: "kling-image-o1", Info: ModelInfo{
				DisplayName:  "可灵图像 O1",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"1K", "2K"},
			}},
			// 两栖模型：API 模型名 kling-v3-omni 同时承载图像/视频；图像条目用别名键避开与视频
			// 条目撞 registry 键，APIModelName 回指真实 API 名。
			{ID: "kling-v3-omni-image", Info: ModelInfo{
				DisplayName:  "可灵 V3-Omni（图像）",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Resolutions:  []string{"1K", "2K", "4K"},
				APIModelName: "kling-v3-omni",
			}},
			{ID: "kling-v3", Info: ModelInfo{
				DisplayName:        "可灵 v3",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"720p", "1080p", "4k"},
			}},
			{ID: "kling-v3-turbo", Info: ModelInfo{
				DisplayName:        "可灵 v3 Turbo",
				MediaType:          MediaVideo,
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"720p", "1080p"},
				APIModelName:       "kling-3.0-turbo",
			}},
			{ID: "kling-v3-omni", Info: ModelInfo{
				DisplayName:        "可灵 v3 Omni",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: durationRange(3, 15),
				Resolutions:        []string{"720p", "1080p", "4k"},
			}},
			{ID: "kling-v2-6", Info: ModelInfo{
				DisplayName:        "可灵 v2.6",
				MediaType:          MediaVideo,
				Capabilities:       []ModelCapability{ModelCapGenerateAudio},
				SupportedDurations: []int{5, 10},
				Resolutions:        []string{"720p", "1080p"},
			}},
			{ID: "kling-video-o1", Info: ModelInfo{
				DisplayName:        "可灵 Video O1",
				MediaType:          MediaVideo,
				SupportedDurations: []int{5, 10},
				Resolutions:        []string{"720p", "1080p"},
			}},
		},
		// 国内调用域名官方已由 api.klingai.com 迁移至 api-beijing.klingai.com（旧域名仍可用，
		// 未强制下线）；仅影响未显式配置 base_url 的新用户。
		DefaultBaseURL: "https://api-beijing.klingai.com/v1",
	},
	"agnes": {
		DisplayName:  "Agnes",
		ConsoleURL:   "https://agnes-ai.com",
		Description:  "Agnes 多模态平台（OpenAI 风格），使用 Bearer API Key 鉴权；当前支持图像 / 文本 / 视频生成。",
		RequiredKeys: []string{"api_key"},
		OptionalKeys: []string{"base_url", "image_max_workers", "video_max_workers"},
		SecretKeys:   []string{"api_key"},
		Models: []ModelEntry{
			// Agnes 文本模型走 OpenAI 兼容 /v1/chat/completions；旧型号保留用于兼容存量配置。
			{ID: "agnes-2.0-flash", Info: ModelInfo{
				DisplayName:  "Agnes 2.0 Flash",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
			}},
			{ID: "agnes-2.5-flash", Info: ModelInfo{
				DisplayName:  "Agnes 2.5 Flash",
				MediaType:    MediaText,
				Capabilities: []ModelCapability{ModelCapTextGeneration, ModelCapStructuredOutput},
				Default:      true,
			}},
			// agnes-image-2.1-flash：OpenAI 兼容 /images/generations 单步同步，T2I + I2I。
			// Resolutions 为保守 UI 档位（未逐档实测）；实际尺寸由 backend 按比例计算、与此无耦合。
			{ID: "agnes-image-2.1-flash", Info: ModelInfo{
				DisplayName:  "Agnes Image 2.1 Flash",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Resolutions:  []string{"1K", "2K"},
			}},
			{ID: "agnes-image-2.5-flash", Info: ModelInfo{
				DisplayName:  "Agnes Image 2.5 Flash",
				MediaType:    MediaImage,
				Capabilities: []ModelCapability{ModelCapTextToImage, ModelCapImageToImage},
				Default:      true,
				Resolutions:  []string{"1K", "2K"},
			}},
			// agnes-video-v2.0：apihub 异步 /v1/videos，图生 / 首尾帧 / 多图主体参考；fps 固定 24。
			{ID: "agnes-video-v2.0", Info: ModelInfo{
				DisplayName:        "Agnes Video 2.0",
				MediaType:          MediaVideo,
				SupportedDurations: durationRange(1, 18),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			// Agnes 2.5 视频模型共用异步 /v1/videos 端点与同一能力约束；后端按传入 model 下发，
			// 不需要为型号复制一套请求构造逻辑。
			{ID: "agnes-video-2.5", Info: ModelInfo{
				DisplayName:        "Agnes Video 2.5",
				MediaType:          MediaVideo,
				Default:            true,
				SupportedDurations: durationRange(1, 18),
				Resolutions:        []string{"480p", "720p", "1080p"},
			}},
			// Flash 共用 2.5 的异步请求字段，但输出上限为 720p；不要把 1080p 暴露给 UI。
			{ID: "agnes-video-2.5-flash", Info: ModelInfo{
				DisplayName:        "Agnes Video 2.5 Flash",
				MediaType:          MediaVideo,
				SupportedDurations: durationRange(4, 12),
				Resolutions:        []string{"720p"},
			}},
		},
		DefaultBaseURL: "https://apihub.agnes-ai.com/v1",
		// Agnes 视频上游对并发敏感，出厂串行（默认 1）避免主动制造 503 Service busy；
		// 用户可经 video_max_workers 覆盖。其余 lane 未声明，走全局默认。
		DefaultConcurrency: map[string]int{"video": 1},
	},
}

// ProviderIDs 返回内置 provider 的 registry 键（注册表声明顺序）。
func ProviderIDs() []string {
	return append([]string(nil), builtinProviderOrder...)
}

// ProviderMetaFor 按 provider 键取元数据；未登记的 provider 返回 ok=false，
// 不回落任何默认供应商。
func ProviderMetaFor(providerID string) (ProviderMeta, bool) {
	meta, ok := builtinProviders[providerID]
	return meta, ok
}

// ModelInfoFor 返回该 (provider, model) 的模型元数据；provider 或 model 未登记时 ok=false。
//
// 供 backend 读取本模型的能力 / 约束声明，把注册表作为约束的单一真相源。未登记的路径
// （自定义供应商包装、中转站、下线型号）返回 ok=false，由调用方自行兜底。
func ModelInfoFor(providerID, modelID string) (ModelInfo, bool) {
	meta, ok := builtinProviders[providerID]
	if !ok {
		return ModelInfo{}, false
	}
	return meta.Model(modelID)
}

// DefaultModelForProvider 返回该 provider 在注册表中指定媒体类型的默认模型键；
// provider 未登记或该媒体无默认时 ok=false。
func DefaultModelForProvider(providerID string, media MediaType) (string, bool) {
	meta, ok := builtinProviders[providerID]
	if !ok {
		return "", false
	}
	for _, entry := range meta.Models {
		if entry.Info.MediaType == media && entry.Info.Default {
			return entry.ID, true
		}
	}
	return "", false
}

// durationRange 返回 [first, last] 闭区间内的整数时长（对应 Python 的 list(range(first, last+1))）。
func durationRange(first, last int) []int {
	if last < first {
		return nil
	}
	out := make([]int, 0, last-first+1)
	for v := first; v <= last; v++ {
		out = append(out, v)
	}
	return out
}

// init 在装载期建立规格索引并校验静态数据不变式，misconfig fail-fast——与 Python 侧注册表
// import 期抛出等价，把作者笔误挡在启动前而不是首次调用时。
func init() {
	builtinProviderSpecByKey = make(map[providerMediaKey]ProviderSpec, len(builtinProviderSpecs))
	for _, spec := range builtinProviderSpecs {
		key := providerMediaKey{ProviderID: spec.ProviderID, MediaType: spec.MediaType}
		if _, exists := builtinProviderSpecByKey[key]; exists {
			panic(fmt.Sprintf("provider registry misconfig: duplicate spec for provider=%q media=%q", spec.ProviderID, spec.MediaType))
		}
		if _, ok := validMediaTypes[spec.MediaType]; !ok {
			panic(fmt.Sprintf("provider registry misconfig: spec %q declares unknown media_type %q", spec.ProviderID, spec.MediaType))
		}
		builtinProviderSpecByKey[key] = spec
	}
	validateBuiltinProviders()
}

// validateBuiltinProviders 校验内置 provider 表的静态不变式。
func validateBuiltinProviders() {
	for _, providerID := range builtinProviderOrder {
		meta, ok := builtinProviders[providerID]
		if !ok {
			panic(fmt.Sprintf("provider registry misconfig: provider %q missing from table", providerID))
		}
		validateProviderMeta(providerID, meta)
	}
	if len(builtinProviders) != len(builtinProviderOrder) {
		panic("provider registry misconfig: provider table has entries missing from declaration order")
	}
}

func validateProviderMeta(providerID string, meta ProviderMeta) {
	// default_concurrency 是注册表静态声明：拼错的 lane key 会被静默忽略、该 lane 漂回全局
	// 默认（限并发失效），非正整数上限则在投影后把支持的 lane 误判为不支持。
	for lane, limit := range meta.DefaultConcurrency {
		if lane != "image" && lane != "video" && lane != "audio" {
			panic(fmt.Sprintf("provider registry misconfig: %s default_concurrency has unknown lane %q", providerID, lane))
		}
		if limit < 1 {
			panic(fmt.Sprintf("provider registry misconfig: %s default_concurrency[%q] must be >= 1, got %d", providerID, lane, limit))
		}
	}
	if len(meta.CredentialGroups) > 0 {
		allowed := intersectKeySets(stringKeySet(meta.RequiredKeys), stringKeySet(meta.SecretKeys))
		covered := map[string]struct{}{}
		for _, group := range meta.CredentialGroups {
			if len(group) == 0 {
				panic(fmt.Sprintf("provider registry misconfig: %s credential_groups contains an empty group", providerID))
			}
			for _, key := range group {
				if _, ok := allowed[key]; !ok {
					panic(fmt.Sprintf("provider registry misconfig: %s credential_groups key %q is not in required ∩ secret", providerID, key))
				}
				covered[key] = struct{}{}
			}
		}
		// 未被任何分组覆盖的 key 既不受「全部必填」约束，也不受任何分组约束——声明了分组却漏
		// 登记某个 key，前端会误判该 key 为可选。
		if len(covered) != len(allowed) {
			panic(fmt.Sprintf("provider registry misconfig: %s credential_groups does not cover all required ∩ secret keys", providerID))
		}
	}

	defaultByMedia := map[MediaType]string{}
	for _, entry := range meta.Models {
		info := entry.Info
		if _, ok := validMediaTypes[info.MediaType]; !ok {
			panic(fmt.Sprintf("provider registry misconfig: %s/%s declares unknown media_type %q", providerID, entry.ID, info.MediaType))
		}
		for _, capability := range info.Capabilities {
			expectedMedia, ok := modelCapabilityMedia[capability]
			if !ok {
				panic(fmt.Sprintf("provider registry misconfig: %s/%s declares unknown capability token %q", providerID, entry.ID, capability))
			}
			// token 与媒体 backend 能力枚举同名同义，挂错媒体即作者笔误。
			if expectedMedia != info.MediaType {
				panic(fmt.Sprintf(
					"provider registry misconfig: %s/%s token %q does not belong to media_type %q",
					providerID, entry.ID, capability, info.MediaType,
				))
			}
		}
		// 恒有声仅视频模型可置 True，且与「开关可控」（generate_audio token）互斥。
		if info.AudioAlwaysOn {
			if info.MediaType != MediaVideo {
				panic(fmt.Sprintf("provider registry misconfig: %s/%s audio_always_on requires media_type video", providerID, entry.ID))
			}
			if info.Has(ModelCapGenerateAudio) {
				panic(fmt.Sprintf("provider registry misconfig: %s/%s declares both audio_always_on and generate_audio", providerID, entry.ID))
			}
		}
		if info.Default {
			if previous, exists := defaultByMedia[info.MediaType]; exists {
				panic(fmt.Sprintf(
					"provider registry misconfig: %s has multiple defaults for media_type %q (%s and %s)",
					providerID, info.MediaType, previous, entry.ID,
				))
			}
			defaultByMedia[info.MediaType] = entry.ID
		}
	}
}

func stringKeySet(keys []string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

func intersectKeySets(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a))
	for key := range a {
		if _, ok := b[key]; ok {
			out[key] = struct{}{}
		}
	}
	return out
}
