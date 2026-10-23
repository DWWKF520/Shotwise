// step1→step2 审核 gate 的核心逻辑（对应 lib/script_review.py）：适用性判定、step1 路径、
// 内容指纹、审核状态派生与确认记录写回。
//
// 真值只存「确认指纹」于 project.json episodes[i].step1_review；pending / confirmed 由读时
// 比对 live step1 内容指纹派生。故重跑 normalize、agent 改写 step1、web 手改 step1 都会让指纹
// 漂移、自动重新待审。
//
// 适用范围（拥有结构化 step1 中间态的三条两段式路径）：drama / narration 的图生 / 宫格路径；
// reference_video 路径（跨 narration / drama）。ad 无 step1，不纳入 gate。
package script

import (
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/service/reference"
	"os"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/domain/project"
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
func Step1Kind(pf *project.ProjectFile) string {
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
func IsReviewApplicable(pf *project.ProjectFile) bool { return Step1Kind(pf) != "" }

// Step1Path 返回该集结构化 step1 中间态文件路径；不适用 gate 时 ok=false。
func Step1Path(projectDir string, pf *project.ProjectFile, episode int) (string, bool) {
	kind := Step1Kind(pf)
	if kind == "" {
		return "", false
	}
	filename := step1Filenames[kind]
	if kind == step1KindReferenceVideo {
		filename = service.ReferenceVideoStep1Filename
	}
	return filepath.Join(service.EpisodeDraftsDir(projectDir, episode), filename), true
}

// Step2Generated 判定该集 step2 产物（生成的剧本 JSON）是否已存在——存量 grandfather 判据。
func Step2Generated(projectDir string, pf *project.ProjectFile, episode int) bool {
	scriptFile := ""
	if entry := service.FindEpisodeEntry(pf, episode); entry != nil {
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
func ReviewStatus(projectDir string, pf *project.ProjectFile, episode int) string {
	path, ok := Step1Path(projectDir, pf, episode)
	if !ok {
		return ReviewStatusNotApplicable
	}
	// 隔离草稿在场先于指纹判定：违约产物尚未处置，该集还没有一份「可放行」的 step1。
	if Step1Quarantined(projectDir, pf, episode) {
		return ReviewStatusPendingReview
	}
	live, err := service.ContentFingerprint(path)
	if err != nil {
		return ReviewStatusPendingReview
	}
	if live == nil {
		return ReviewStatusNoStep1
	}
	if review := service.StoredReview(pf, episode); review != nil && review.Fingerprint != "" {
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
func GateBlocksStep2(projectDir string, pf *project.ProjectFile, episode int) bool {
	return ReviewStatus(projectDir, pf, episode) == ReviewStatusPendingReview
}

// Step1QuarantinePath 返回该集 step1 隔离草稿路径（仅 reference_video 变体有隔离草稿）。
func Step1QuarantinePath(projectDir string, pf *project.ProjectFile, episode int) (string, bool) {
	if Step1Kind(pf) != step1KindReferenceVideo {
		return "", false
	}
	return reference.QuarantinePath(projectDir, episode, reference.QuarantineKindStep1), true
}

// Step1Quarantined 判定该集 step1 是否有违约产物被隔离——gate 与 step2 的独立阻塞判据。
func Step1Quarantined(projectDir string, pf *project.ProjectFile, episode int) bool {
	if Step1Kind(pf) != step1KindReferenceVideo {
		return false
	}
	return reference.QuarantineExists(projectDir, episode, reference.QuarantineKindStep1)
}
