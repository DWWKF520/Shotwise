// 宫格记录模型（对应 lib/grid/models.py）。记录落 {project}/grids/{grid_id}.json，
// 字段名必须与 Python 侧 to_dict 逐字一致——两个服务在同一项目目录上共写共读这份文件。
package grid

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// GridStatus 是联合图生命周期状态。
//
// completed 仅表示联合图就绪，是否已切分落格由 SplitAt 表达。
const (
	GridStatusPending    = "pending"
	GridStatusGenerating = "generating"
	GridStatusCompleted  = "completed"
	GridStatusFailed     = "failed"
)

// 帧类型：格 0 是首个场景开场，中间格是相邻场景过渡，其余为占位。
const (
	GridFrameFirst       = "first"
	GridFrameTransition  = "transition"
	GridFramePlaceholder = "placeholder"
)

// gridIDPattern 与 GridGeneration.newID 的生成格式一致，供路径白名单校验使用。
var gridIDPattern = regexp.MustCompile(`^grid_[0-9a-f]{12}$`)

// ValidGridID 判定宫格 ID 是否合法（来自 URL 路径参数的 id 先卡此白名单再过越界校验）。
func ValidGridID(id string) bool { return gridIDPattern.MatchString(id) }

// GridReferenceImage 是生成宫格时用到的一张参考图元数据。
type GridReferenceImage struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	RefType string `json:"ref_type"`
}

// GridFrameCell 是宫格帧链中的单格。
type GridFrameCell struct {
	Index       int     `json:"index"`
	Row         int     `json:"row"`
	Col         int     `json:"col"`
	FrameType   string  `json:"frame_type"`
	PrevSceneID *string `json:"prev_scene_id"`
	NextSceneID *string `json:"next_scene_id"`
	ImagePath   *string `json:"image_path"`
}

// GridGeneration 是一次宫格联合图生成作业。
type GridGeneration struct {
	ID            string          `json:"id"`
	Episode       int             `json:"episode"`
	ScriptFile    string          `json:"script_file"`
	SceneIDs      []string        `json:"scene_ids"`
	GridImagePath *string         `json:"grid_image_path"`
	Rows          int             `json:"rows"`
	Cols          int             `json:"cols"`
	CellCount     int             `json:"cell_count"`
	FrameChain    []GridFrameCell `json:"frame_chain"`
	Status        string          `json:"status"`
	Prompt        *string         `json:"prompt"`
	Provider      string          `json:"provider"`
	Model         string          `json:"model"`
	GridSize      string          `json:"grid_size"`
	CreatedAt     string          `json:"created_at"`
	ErrorMessage  *string         `json:"error_message"`
	// ReferenceImages 为空时序列化为 null（与 Python 侧 [] / None 的写法一致）。
	ReferenceImages []GridReferenceImage `json:"reference_images"`
	// SplitAt 是最近一次按当前联合图切分落格的时间；联合图内容变更时清空。
	SplitAt *string `json:"split_at"`
	// VideoAspectRatio 是产出当前联合图时的单格目标比例（随联合图冻结，避免项目改比例后
	// 重切历史联合图被按新比例中心裁切）。存量记录为 nil，切分时回退到项目当前设置。
	VideoAspectRatio *string `json:"video_aspect_ratio"`
}

// BuildFrameChain 按分镜 ID 顺序填满 rows×cols 宫格帧链。
//
// 格 0：first，next_scene_id 指向第一个分镜；格 1..n-1：transition，带前后分镜；
// 其余格：placeholder。
func BuildFrameChain(sceneIDs []string, rows, cols int) []GridFrameCell {
	total := rows * cols
	chain := make([]GridFrameCell, 0, total)
	for idx := 0; idx < total; idx++ {
		cell := GridFrameCell{Index: idx, Row: idx / cols, Col: idx % cols}
		switch {
		case idx == 0:
			cell.FrameType = GridFrameFirst
			if len(sceneIDs) > 0 {
				next := sceneIDs[0]
				cell.NextSceneID = &next
			}
		case idx < len(sceneIDs):
			cell.FrameType = GridFrameTransition
			prev := sceneIDs[idx-1]
			next := sceneIDs[idx]
			cell.PrevSceneID = &prev
			cell.NextSceneID = &next
		default:
			cell.FrameType = GridFramePlaceholder
		}
		chain = append(chain, cell)
	}
	return chain
}

// NewGridID 生成 grid_ + 12 位十六进制字符的标识（对应 Python uuid4().hex[:12]）。
func NewGridID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		// 熵源不可用时退化为时间戳派生的确定性 id：仍满足格式白名单。
		return fmt.Sprintf("grid_%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return "grid_" + hex.EncodeToString(buf)
}

// NewGridGeneration 新建一条 pending 状态的宫格记录。
func NewGridGeneration(
	episode int, scriptFile string, sceneIDs []string, rows, cols int,
	gridSize, provider, model, videoAspectRatio string, prompt *string,
) *GridGeneration {
	createdAt := GridTimestampISO()
	return &GridGeneration{
		ID:               NewGridID(),
		Episode:          episode,
		ScriptFile:       scriptFile,
		SceneIDs:         sceneIDs,
		Rows:             rows,
		Cols:             cols,
		CellCount:        rows * cols,
		FrameChain:       BuildFrameChain(sceneIDs, rows, cols),
		Status:           GridStatusPending,
		Prompt:           prompt,
		Provider:         provider,
		Model:            model,
		GridSize:         gridSize,
		CreatedAt:        createdAt,
		VideoAspectRatio: &videoAspectRatio,
	}
}

// MarkCompositeReplaced 复位记录：联合图被用户动作换成新内容（手动上传 / 版本还原）后调用。
//
// SplitAt 无条件清空（旧落格结果不再对应当前联合图）；Status / ErrorMessage 仅在生成不在途
// 时复位——pending/generating 期间保留在途态，否则记录会谎报空闲、在途闸门失效。
func (g *GridGeneration) MarkCompositeReplaced() {
	imagePath := fmt.Sprintf("grids/%s.png", g.ID)
	g.GridImagePath = &imagePath
	g.SplitAt = nil
	if g.Status != GridStatusPending && g.Status != GridStatusGenerating {
		g.Status = GridStatusCompleted
		g.ErrorMessage = nil
	}
}

// GridTimestampISO 复刻 Python datetime.now(UTC).isoformat() 的文本形态（微秒 + +00:00 时区）。
//
// 宫格记录的 created_at / split_at 都用它：文本序即时间序，且与 Python 侧写入的记录可比。
func GridTimestampISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// GridGenerationFromJSON 解析宫格记录，兼容两个存量形态：
//   - status="splitting"：生成与切分曾是同一任务，该中间态下联合图已落盘，等价于
//     「联合图就绪、未落格」→ completed（且 split_at 保持未切分）。
//   - 缺 split_at 字段且 status="completed"：旧流程下 completed 只在切格落盘之后写入，
//     等价于「已切分」，用 created_at 充当落格时间。显式为 null 的新记录保持未切分。
func GridGenerationFromJSON(raw []byte) (*GridGeneration, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	grid := &GridGeneration{}
	if err := json.Unmarshal(raw, grid); err != nil {
		return nil, err
	}
	var rawStatus string
	_ = json.Unmarshal(probe["status"], &rawStatus)
	if rawStatus == "splitting" {
		grid.Status = GridStatusCompleted
	}
	if _, present := probe["split_at"]; !present && rawStatus == "completed" {
		createdAt := grid.CreatedAt
		grid.SplitAt = &createdAt
	}
	return grid, nil
}
