package provider

// 视频能力桶（i2v / r2v）归属判定——把既有能力声明翻译成桶，不新增第二份声明。
//
// 判定来源逐桶固定：内置视频取 backend 的 VideoCapabilities——i2v 取 FirstFrame、r2v 取
// MaxReferenceImages；自定义供应商取 endpoint 系统判定 ⊕ 模型级覆盖的合成结果，同样取这两个
// 维度。取 backend 而非注册表元数据是因为 backend 的能力声明与请求构造同源（例如 vidu 按端点
// 白名单算能力，白名单外的 model 提交首帧会在构造请求时报错），注册表不声明这两维。桶承诺的是
// 「选中的组合执行得了」，故以执行期同源的那一份为准。
//
// 判定不出（backend 未声明能力）时返回空集而非全集：候选列表宁缺勿滥，配进去的组合执行期一样
// 必败，不如不出现在下拉里。

// VideoBucket 是视频能力桶：i2v（图生视频 / 宫格，由首帧驱动；另承接参考路线无参考图退化
// 镜头的降级执行）、r2v（参考生视频的有参考图镜头）。t2v 不设桶。
type VideoBucket string

const (
	// BucketI2V 图生视频桶。
	BucketI2V VideoBucket = "i2v"
	// BucketR2V 参考生视频桶。
	BucketR2V VideoBucket = "r2v"
)

// VideoBucketOrder 是桶的固定遍历顺序。
var VideoBucketOrder = []VideoBucket{BucketI2V, BucketR2V}

// DefaultVideoBucket 是表外输入（无项目上下文与脏数据）落的桶。
const DefaultVideoBucket = BucketI2V

// VideoCapabilitySatisfied 判断一组视频能力声明是否满足某个桶——桶归属判定的唯一口径。
//
// 解析闸与桶候选下拉共用本函数，不各写一份布尔式：下拉挡掉的组合解析层必然也挡，反之亦然。
func VideoCapabilitySatisfied(bucket VideoBucket, firstFrame bool, maxReferenceImages int) bool {
	if bucket == BucketI2V {
		return firstFrame
	}
	return maxReferenceImages > 0
}

// SatisfiedVideoBuckets 返回一组能力声明满足的全部桶（候选下拉口径），顺序固定。
func SatisfiedVideoBuckets(caps VideoCapabilities) []VideoBucket {
	buckets := make([]VideoBucket, 0, len(VideoBucketOrder))
	for _, bucket := range VideoBucketOrder {
		if VideoCapabilitySatisfied(bucket, caps.FirstFrame, caps.MaxReferenceImages) {
			buckets = append(buckets, bucket)
		}
	}
	return buckets
}

// VideoBucketByTaskType 是视频任务类型 → 能力桶的映射：图生视频 / 宫格生视频（task_type
// video）→ i2v；参考生视频（reference_video）按镜头是否携带参考图分流，本表登记其代表桶 r2v，
// 仅供剧本 / unit 读不到时回退。表外任务类型无视频桶，调用方按「不定桶」处理。
var VideoBucketByTaskType = map[string]VideoBucket{
	"video":           BucketI2V,
	"reference_video": BucketR2V,
}

// VideoBucketByGenerationMode 是生成模式 → 能力桶的映射。与 VideoBucketByTaskType 描述同一套
// 映射的两个入口：执行路径按已成形任务的 task_type 定桶，读侧（能力查询 / 时长约束收窄等）在
// 任务成形前只有项目的 GenerationMode，按它定同一个桶，两侧因此回答同一个「当前配置真正会执行的
// 模型」。参考路线内无参考图退化镜头的镜头级降级（→ i2v）不经本表，见 ReferenceVideoBucket。
var VideoBucketByGenerationMode = map[string]VideoBucket{
	"storyboard":      BucketI2V,
	"reference_video": BucketR2V,
}

// VideoBucketForTaskType 按任务类型定桶；表外任务类型返回 ok=false（调用方按「不定桶」处理）。
func VideoBucketForTaskType(taskType string) (VideoBucket, bool) {
	bucket, ok := VideoBucketByTaskType[taskType]
	return bucket, ok
}

// VideoBucketForGenerationMode 把项目的 GenerationMode 归到哪个能力桶——读侧定桶的唯一入口。
//
// project.json 是明文文件，GenerationMode 可能被写成脏值（含空串），一并落默认桶；无项目上下文
// （如 provider 目录查询）同样没有路线可依。
func VideoBucketForGenerationMode(generationMode string) VideoBucket {
	if bucket, ok := VideoBucketByGenerationMode[generationMode]; ok {
		return bucket
	}
	return DefaultVideoBucket
}

// ReferenceVideoBucket 是参考生视频镜头的能力桶：有参考图 → r2v；无参考图的退化镜头 → i2v
// （不送入拒空参考的 r2v 桶模型——部分 r2v 模型对空 reference_images 抛
// video_reference_images_required）。
//
// 这里只影响预检指向与限流路由的精度，执行层独立判定。
func ReferenceVideoBucket(withReferences bool) VideoBucket {
	if withReferences {
		return BucketR2V
	}
	return BucketI2V
}

// ResolveVideoBucket 是执行路径定桶的单一入口：任务类型优先（video → i2v；reference_video →
// 连带按镜头是否携带参考图分流：有参考图 → r2v，无参考图的退化镜头 → i2v）；表外任务类型与空
// taskType 退回按 GenerationMode 定桶（storyboard → i2v，reference_video → r2v，未知 → 默认桶）。
func ResolveVideoBucket(taskType, generationMode string, withReferences bool) VideoBucket {
	if taskType == "video" {
		return BucketI2V
	}
	if taskType == "reference_video" {
		return ReferenceVideoBucket(withReferences)
	}
	return VideoBucketForGenerationMode(generationMode)
}
