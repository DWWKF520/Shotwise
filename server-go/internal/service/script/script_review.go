// step1→step2 审核 gate 的核心逻辑（对应 lib/script_review.py）：适用性判定、step1 路径、
// 内容指纹、审核状态派生与确认记录写回。
//
// 真值只存「确认指纹」于 project.json episodes[i].step1_review；pending / confirmed 由读时
// 比对 live step1 内容指纹派生。故重跑 normalize、agent 改写 step1、web 手改 step1 都会让指纹
// 漂移、自动重新待审。
//
// 适用范围（拥有结构化 step1 中间态的三条两段式路径）：drama / narration 的图生 / 宫格路径；
// reference_video 路径（跨 narration / drama）。ad 无 step1，不纳入 gate。
package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
)

// 审核状态取值。
const (
	ReviewStatusNotApplicable = "not_applicable"
	ReviewStatusNoStep1       = "no_step1"
	ReviewStatusPendingReview = "pending_review"
	ReviewStatusConfirmed     = "confirmed"
)

// step1KindReferenceVideo 是参考路径的 step1 变体名（跨 content_mode，按生成路线优先判定）。
const step1KindReferenceVideo = "reference_video"

// Step1Kind 返回项目的 step1 变体；无结构化 step1 中间态（如 ad）时返回 ""。
func Step1Kind(pf *domain.ProjectFile) string {
	if pf == nil {
		return ""
	}
	if _, ok := step1Filenames[pf.ContentMode]; !ok {
		return ""
	}
	if pf.IsReferenceVideo() {
		return step1KindReferenceVideo
	}
	return pf.ContentMode
}

// IsReviewApplicable 判定 gate 是否适用于该项目。
func IsReviewApplicable(pf *domain.ProjectFile) bool { return Step1Kind(pf) != "" }

// Step1Path 返回该集结构化 step1 中间态文件路径；不适用 gate 时 ok=false。
func Step1Path(projectDir string, pf *domain.ProjectFile, episode int) (string, bool) {
	kind := Step1Kind(pf)
	if kind == "" {
		return "", false
	}
	filename := step1Filenames[kind]
	if kind == step1KindReferenceVideo {
		filename = ReferenceVideoStep1Filename
	}
	return filepath.Join(EpisodeDraftsDir(projectDir, episode), filename), true
}

// ContentFingerprintOfData 计算已解析 JSON 对象的指纹：规范化 dump（键序 / 空白重排不改指纹）的 sha256。
//
// 规范形状与 Python 侧 content_fingerprint_of_data 对齐：键排序、紧凑分隔符、不转义非 ASCII。
func ContentFingerprintOfData(data any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(data) // 末尾带换行，去掉后与 Python 的 dumps 一致。
	canonical := strings.TrimRight(buf.String(), "\n")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// ContentFingerprint 计算 step1 文件的内容指纹：合法 JSON 取规范化 dump 的 sha256，
// 非 JSON 退化为原始字节 sha256；文件不存在返回 (nil, nil)。
//
// 只把「文件不存在」降级为 nil（→ no_step1、gate 放行）；其它读错误向上抛，避免把文件系统
// 故障静默当成「step1 未产出」而误放行 step2。
func ContentFingerprint(path string) (*string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var parsed any
	if json.Unmarshal(raw, &parsed) == nil {
		value := ContentFingerprintOfData(parsed)
		return &value, nil
	}
	sum := sha256.Sum256(raw)
	value := hex.EncodeToString(sum[:])
	return &value, nil
}

// FindEpisodeEntry 返回该集条目；不存在返回 nil。
func FindEpisodeEntry(pf *domain.ProjectFile, episode int) *domain.EpisodeMeta {
	if pf == nil {
		return nil
	}
	for i := range pf.Episodes {
		if pf.Episodes[i].Episode == episode {
			return &pf.Episodes[i]
		}
	}
	return nil
}

// StoredReview 返回该集已存的确认记录；缺失返回 nil。
func StoredReview(pf *domain.ProjectFile, episode int) *domain.Step1Review {
	entry := FindEpisodeEntry(pf, episode)
	if entry == nil {
		return nil
	}
	return entry.Step1Review
}

// Step2Generated 判定该集 step2 产物（生成的剧本 JSON）是否已存在——存量 grandfather 判据。
func Step2Generated(projectDir string, pf *domain.ProjectFile, episode int) bool {
	scriptFile := ""
	if entry := FindEpisodeEntry(pf, episode); entry != nil {
		scriptFile = entry.ScriptFile
	}
	if scriptFile == "" {
		scriptFile = EpisodeScriptRelpath(episode)
	}
	if _, err := os.Stat(filepath.Join(projectDir, filepath.FromSlash(scriptFile))); err == nil {
		return true
	}
	return false
}

// ReviewStatus 派生该集审核状态。
func ReviewStatus(projectDir string, pf *domain.ProjectFile, episode int) string {
	path, ok := Step1Path(projectDir, pf, episode)
	if !ok {
		return ReviewStatusNotApplicable
	}
	// 隔离草稿在场先于指纹判定：违约产物尚未处置，该集还没有一份「可放行」的 step1。
	if Step1Quarantined(projectDir, pf, episode) {
		return ReviewStatusPendingReview
	}
	live, err := ContentFingerprint(path)
	if err != nil {
		return ReviewStatusPendingReview
	}
	if live == nil {
		return ReviewStatusNoStep1
	}
	if review := StoredReview(pf, episode); review != nil && review.Fingerprint != "" {
		if review.Fingerprint == *live {
			return ReviewStatusConfirmed
		}
		return ReviewStatusPendingReview
	}
	// 无确认指纹（存量 / 首次）：用 step2 产物是否已存在做 grandfather 判据。
	if Step2Generated(projectDir, pf, episode) {
		return ReviewStatusConfirmed
	}
	return ReviewStatusPendingReview
}

// GateBlocksStep2 判定 step2 是否应被 gate 阻塞——仅 pending_review 阻塞。
func GateBlocksStep2(projectDir string, pf *domain.ProjectFile, episode int) bool {
	return ReviewStatus(projectDir, pf, episode) == ReviewStatusPendingReview
}

// Step1QuarantinePath 返回该集 step1 隔离草稿路径（仅 reference_video 变体有隔离草稿）。
func Step1QuarantinePath(projectDir string, pf *domain.ProjectFile, episode int) (string, bool) {
	if Step1Kind(pf) != step1KindReferenceVideo {
		return "", false
	}
	return QuarantinePath(projectDir, episode, QuarantineKindStep1), true
}

// Step1Quarantined 判定该集 step1 是否有违约产物被隔离——gate 与 step2 的独立阻塞判据。
func Step1Quarantined(projectDir string, pf *domain.ProjectFile, episode int) bool {
	if Step1Kind(pf) != step1KindReferenceVideo {
		return false
	}
	return QuarantineExists(projectDir, episode, QuarantineKindStep1)
}

// ApplyConfirmation 就地把确认记录写入 project episodes[i].step1_review；集条目不存在返回 false。
func ApplyConfirmation(pf *domain.ProjectFile, episode int, fingerprint, confirmedAt string) bool {
	entry := FindEpisodeEntry(pf, episode)
	if entry == nil {
		return false
	}
	entry.Step1Review = &domain.Step1Review{Fingerprint: fingerprint, ConfirmedAt: confirmedAt}
	return true
}
