// 剧集剧本的锁内读-改-写与绑定解析（对应 server/routers/reference_videos.py 的集解析段）。
// 原属 reference 域文件，因方法 receiver 是 ProjectStore 且被 script / reference 两域消费，归属根包。
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// ErrEpisodeScriptRebound 表示解析候选与锁内复核之间 episode→script 绑定被并发改动。
//
// 对应 Python EpisodeScriptReboundError：调用方转 409 ref_script_rebound 提示前端重试，
// 不外泄内部绑定细节。
var ErrEpisodeScriptRebound = errors.New("episode script binding changed")

// EpisodeNotFoundError 表示目标集缺失或其未绑定剧本（404 ref_episode_not_found）。
type EpisodeNotFoundError struct{ Episode int }

func (e *EpisodeNotFoundError) Error() string { return fmt.Sprintf("episode %d not found", e.Episode) }

// NotReferenceVideoModeError 表示该集剧本不在参考生视频模式（409 ref_not_reference_video_mode）。
type NotReferenceVideoModeError struct{}

func (e *NotReferenceVideoModeError) Error() string { return "episode script is not in reference-video mode" }

// EpisodeScriptMissingError 表示 project.json 绑定的剧本文件缺失（404 ref_script_missing）。
type EpisodeScriptMissingError struct{}

func (e *EpisodeScriptMissingError) Error() string { return "episode script file missing" }

// EpisodeScriptResolver 从 project.json 解析目标集的 script_file（在项目锁内被调用）。
type EpisodeScriptResolver func(project *domain.ProjectFile) (string, error)

// EpisodeScriptFile 从 project.json 找目标集绑定的剧本文件名；缺失时返回 EpisodeNotFoundError。
func EpisodeScriptFile(project *domain.ProjectFile, episode int) (string, error) {
	for _, meta := range project.Episodes {
		if meta.Episode == episode && meta.ScriptFile != "" {
			return meta.ScriptFile, nil
		}
	}
	return "", &EpisodeNotFoundError{Episode: episode}
}


// LoadEpisodeScript 只读加载指定集的剧本（对应 _load_episode_script）。
//
// 返回 (project, script, script_file)；任一前置校验失败时返回对应错误，script 可能为 nil。
func (s *ProjectStore) LoadEpisodeScript(project string, episode int) (*domain.ProjectFile, map[string]any, string, error) {
	dir, err := s.Dir(project)
	if err != nil {
		return nil, nil, "", err
	}
	pf, err := s.Load(dir)
	if err != nil {
		return nil, nil, "", err
	}
	scriptFile, err := EpisodeScriptFile(pf, episode)
	if err != nil {
		return pf, nil, "", err
	}
	if !pf.IsReferenceVideo() {
		return pf, nil, scriptFile, &NotReferenceVideoModeError{}
	}
	script, err := s.ReadScriptJSON(project, scriptFile)
	if err != nil {
		return pf, nil, scriptFile, &EpisodeScriptMissingError{}
	}
	return pf, script, scriptFile, nil
}

// UpdateEpisodeScript 在「脚本锁 → 项目锁」顺序下解析 episode→script_file、复核绑定并做读-改-写。
//
// 对应 Python ProjectManager.locked_episode_script：候选解析在锁外，锁内复核绑定未变（被并发改绑
// 时返回 ErrEpisodeScriptRebound）；mutate 抛错则跳过写盘。写盘不触发 syncEpisodeFromScript（会
// 二次取项目锁自死锁），改在已持项目锁内联同步 project.json 集元数据。
func (s *ProjectStore) UpdateEpisodeScript(
	project string, resolver EpisodeScriptResolver, mutate func(script map[string]any) error,
) error {
	return s.updateEpisodeScript(project, resolver, mutate, true)
}

// UpdateEpisodeScriptNoValidate 与 UpdateEpisodeScript 同源，但跳过结构校验与元数据重算。
//
// 对应 Python `locked_episode_script(..., validate=False)`：只动 duration_seconds /
// duration_locked 这类热路径字段时既不重算metadata（否则 updated_at 与统计被刷新，
// 剧本修订号随之漂移），也不做「不更坏」比对。
func (s *ProjectStore) UpdateEpisodeScriptNoValidate(
	project string, resolver EpisodeScriptResolver, mutate func(script map[string]any) error,
) error {
	return s.updateEpisodeScript(project, resolver, mutate, false)
}

func (s *ProjectStore) updateEpisodeScript(
	project string, resolver EpisodeScriptResolver, mutate func(script map[string]any) error, validate bool,
) error {
	dir, err := s.Dir(project)
	if err != nil {
		return err
	}
	candidateProject, err := s.Load(dir)
	if err != nil {
		return err
	}
	candidate, err := resolver(candidateProject)
	if err != nil {
		return err
	}
	norm := NormalizeScriptFilename(candidate)
	if err := ValidateScriptBase(norm); err != nil {
		return err
	}

	var runErr error
	s.locks.WithLock(project+"/scripts/"+norm, func() {
		s.locks.WithLock(project, func() {
			current, loadErr := s.Load(dir)
			if loadErr != nil {
				runErr = loadErr
				return
			}
			cur, resolveErr := resolver(current)
			if resolveErr != nil {
				runErr = resolveErr
				return
			}
			if NormalizeScriptFilename(cur) != norm {
				runErr = ErrEpisodeScriptRebound
				return
			}

			target, joinErr := fs.SafeJoin(filepath.Join(dir, "scripts"), false, norm)
			if joinErr != nil {
				runErr = joinErr
				return
			}
			if _, statErr := os.Stat(target); statErr != nil {
				runErr = &EpisodeScriptMissingError{}
				return
			}
			script, _, readErr := ReadScriptMigrated(target)
			if readErr != nil {
				runErr = readErr
				return
			}
			before := CloneScript(script)
			if mutateErr := mutate(script); mutateErr != nil {
				runErr = mutateErr
				return
			}
			if writeErr := s.writeScriptFile(dir, norm, script, before, validate); writeErr != nil {
				runErr = writeErr
				return
			}
			if _, isInt := scriptEpisodeNumber(script); isInt {
				if syncErr := applyEpisodeSync(current, script, norm); syncErr != nil {
					runErr = syncErr
					return
				}
			}
			runErr = s.touchAndWrite(dir, current)
		})
	})
	return runErr
}

// EpisodeSource 是解析后的剧集源文及其来源。
type EpisodeSource struct {
	Path         string
	RelativePath string
	Text         string
	IsDerived    bool
}

// ResolveEpisodeSourceHook 由 script 包注册（依赖倒置：源文解析依赖剧集账本，归属 script 域，
// 根包不反向依赖）。agent 工具与 reference 域经下面的 wrapper 调用。
var ResolveEpisodeSourceHook func(projectDir string, episode int, source string, pf *domain.ProjectFile) (EpisodeSource, error)

// ResolveEpisodeSource 按统一优先级解析一集源文；实现见 script 包的注册函数。
func ResolveEpisodeSource(projectDir string, episode int, source string, pf *domain.ProjectFile) (EpisodeSource, error) {
	if ResolveEpisodeSourceHook == nil {
		return EpisodeSource{}, errors.New("episode source resolver not registered")
	}
	return ResolveEpisodeSourceHook(projectDir, episode, source, pf)
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

// ApplyConfirmation 就地把确认记录写入 project episodes[i].step1_review；集条目不存在返回 false。
func ApplyConfirmation(pf *domain.ProjectFile, episode int, fingerprint, confirmedAt string) bool {
	entry := FindEpisodeEntry(pf, episode)
	if entry == nil {
		return false
	}
	entry.Step1Review = &domain.Step1Review{Fingerprint: fingerprint, ConfirmedAt: confirmedAt}
	return true
}
