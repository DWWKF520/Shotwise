package reference

import "github.com/shotwise/shotwise/internal/service"

// 草稿晋升仓储（对应 server/draft_promotion_repository.py 的 FileDraftPromotionRepository）。
//
// 草稿记录落在 <数据目录>/.draft_promotions.json（Python: app_data_dir()/.draft_promotions.json），
// 正式剧本仍是唯一内容真相源，读写全部经 service.ProjectStore。晋升是剧本锁内的
// compare-and-write：先按 revision + fingerprint 比对，再走统一写盘入口
// （结构不更坏守卫 + 集号一致性 + metadata 重算 + project.json 同步）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// ErrDraftProjectNotFound 项目不存在（Python: get_project_path 的 FileNotFoundError
// → router 转 404 script_not_found）。
var ErrDraftProjectNotFound = errors.New("project not found")

// ErrDraftScriptNameInvalid 剧本文件名非法（Python: safe_join 的 service.ValueError
// → router 转 400 request_invalid）。
var ErrDraftScriptNameInvalid = errors.New("invalid script file name")

// FileDraftPromotionRepository 文件适配器：草稿存 JSON，晋升走剧本锁。
type FileDraftPromotionRepository struct {
	store *service.ProjectStore
	path  string
	locks *fs.KeyedLocks
}

// NewFileDraftPromotionRepository 构造仓储；草稿 store 位于数据目录根部。
func NewFileDraftPromotionRepository(store *service.ProjectStore) *FileDraftPromotionRepository {
	return &FileDraftPromotionRepository{
		store: store,
		path:  filepath.Join(store.Root(), ".draft_promotions.json"),
		locks: fs.NewKeyedLocks(),
	}
}

// StorePath 返回草稿 store 文件路径（测试与运维排查用）。
func (r *FileDraftPromotionRepository) StorePath() string { return r.path }

// ReadOfficial 读取正式剧本快照；剧本文件不存在时返回空内容 + revision 0
// （对应 Python read_official 的 exists 分支）。
func (r *FileDraftPromotionRepository) ReadOfficial(target domain.DraftTarget) (domain.OfficialScript, error) {
	dir, err := r.projectDir(target.ProjectName)
	if err != nil {
		return domain.OfficialScript{}, err
	}
	filename, err := draftScriptFilename(target.ScriptFile)
	if err != nil {
		return domain.OfficialScript{}, err
	}
	return draftReadOfficial(r.store, dir, target.ProjectName, filename, false)
}

// LoadDraft 读取草稿；不存在或 actor 不匹配时返回 (nil, nil)。
func (r *FileDraftPromotionRepository) LoadDraft(draftID, actorID string) (*domain.Draft, error) {
	store, err := r.loadStore()
	if err != nil {
		return nil, err
	}
	raw, ok := store[draftID]
	if !ok {
		return nil, nil
	}
	draft, err := draftFromJSON(raw)
	if err != nil {
		return nil, nil
	}
	if actorID != "" && draft.ActorID != actorID {
		return nil, nil
	}
	return draft, nil
}

// ListDrafts 列出某剧本的全部草稿（不排序，排序由领域服务负责）。
func (r *FileDraftPromotionRepository) ListDrafts(target domain.DraftTarget, actorID string) ([]*domain.Draft, error) {
	store, err := r.loadStore()
	if err != nil {
		return nil, err
	}
	var out []*domain.Draft
	for _, raw := range store {
		draft, err := draftFromJSON(raw)
		if err != nil {
			continue
		}
		if draft.Target != target {
			continue
		}
		if actorID != "" && draft.ActorID != actorID {
			continue
		}
		out = append(out, draft)
	}
	return out, nil
}

// ListAll 列出某项目的全部草稿，按 id 字典序。
func (r *FileDraftPromotionRepository) ListAll(projectName, actorID string) ([]*domain.Draft, error) {
	store, err := r.loadStore()
	if err != nil {
		return nil, err
	}
	var out []*domain.Draft
	for _, raw := range store {
		draft, err := draftFromJSON(raw)
		if err != nil {
			continue
		}
		if draft.Target.ProjectName != projectName {
			continue
		}
		if actorID != "" && draft.ActorID != actorID {
			continue
		}
		out = append(out, draft)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SaveDraft upsert 一条草稿记录。
//
// 直存 json.RawMessage：store 一旦经过 map[string]any 中转，int64 的
// expected_revision / base_revision（mtime_ns，约 1.8e18）会被 float64 抹掉精度
// ——float64 只有 53 位尾数，该量级下 ULP 是 256ns，读回来必然和写入值差几十上百，
// 晋升的 compare-and-write 于是恒判「官方已变」。Python 的 json.load 把整数解析成
// 任意精度 int，没有这个问题；Go 侧用原始字节等价复刻。
func (r *FileDraftPromotionRepository) SaveDraft(draft *domain.Draft) error {
	encoded, err := json.Marshal(draft)
	if err != nil {
		return err
	}
	var writeErr error
	r.locks.WithLock("draft_promotions_store", func() {
		store, loadErr := r.loadStore()
		if loadErr != nil {
			writeErr = loadErr
			return
		}
		store[draft.ID] = json.RawMessage(encoded)
		writeErr = fs.WriteJSONAtomic(r.path, store)
	})
	return writeErr
}

// PromoteAtomically 在剧本锁内 compare-and-write 正式剧本。
func (r *FileDraftPromotionRepository) PromoteAtomically(
	target domain.DraftTarget, content map[string]any, expectedRevision int64, expectedFingerprint string,
) (domain.OfficialScript, error) {
	filename, err := draftScriptFilename(target.ScriptFile)
	if err != nil {
		return domain.OfficialScript{}, err
	}
	err = r.store.WithScriptLock(target.ProjectName, filename, func(dir, locked string) error {
		current, readErr := draftReadOfficial(r.store, dir, target.ProjectName, locked, true)
		if readErr != nil {
			return readErr
		}
		if current.Revision != expectedRevision || current.Fingerprint != expectedFingerprint {
			return &domain.OfficialRevisionConflict{Current: current}
		}
		// 与 Python _write_script_unlocked(validate=True, before=改前剧本) 同序：
		// 结构不更坏守卫 → 集号一致性 → metadata 重算 → 原子写 → 集元数据同步。
		return r.store.WriteScript(target.ProjectName, dir, locked, service.CloneScript(content), current.Content, true)
	})
	if err != nil {
		return domain.OfficialScript{}, err
	}
	// 锁外重读一次拿新 revision / fingerprint（Python 同样 return self.read_official(target)）。
	return r.ReadOfficial(target)
}

// loadStore 读取草稿 store；文件不存在时按空表处理。
func (r *FileDraftPromotionRepository) loadStore() (map[string]json.RawMessage, error) {
	store := map[string]json.RawMessage{}
	ok, err := fs.ReadJSONOrNil(r.path, &store)
	if err != nil {
		return nil, err
	}
	if !ok || store == nil {
		return map[string]json.RawMessage{}, nil
	}
	return store, nil
}

func (r *FileDraftPromotionRepository) projectDir(projectName string) (string, error) {
	if projectName == "" {
		return "", ErrDraftProjectNotFound
	}
	dir, err := r.store.Dir(projectName)
	if err != nil {
		// 项目名穿越 / 非法字符与「项目不存在」在 Python 侧分别走 service.ValueError 与
		// FileNotFoundError；SafeJoin 把两者都收敛成 PathTraversalError，这里按存在性区分。
		if r.store.Exists(projectName) {
			return "", ErrDraftScriptNameInvalid
		}
		return "", ErrDraftProjectNotFound
	}
	if !r.store.Exists(projectName) {
		return "", ErrDraftProjectNotFound
	}
	return dir, nil
}

// draftScriptFilename 归一剧本文件名；非单段（含 ../）一律拒绝。
func draftScriptFilename(name string) (string, error) {
	filename := service.NormalizeScriptFilename(name)
	if err := service.ValidateScriptBase(filename); err != nil {
		return "", ErrDraftScriptNameInvalid
	}
	return filename, nil
}

// draftReadOfficial 读取正式剧本快照：revision 取 mtime_ns（Python st_mtime_ns 口径）。
//
// insideLock 表示调用方已持该剧本的锁：此时必须走无锁读（readScriptMigrated），
// 否则 service.ReadScriptJSON 撞上存量迁移会二次取同一把锁造成同进程自死锁——对应 Python
// promote_atomically 里刻意调用 _read_script_unlocked 的原因。
//
// revision 必须在读**之后**才 stat：读路径可能顺手把存量迁移写回盘（mtime 随之变化），
// 先 stat 会把迁移前的旧 mtime 当基线，下一次读取必然对不上，晋升被误判成 stale。
// Python read_official 同样是 load_script 之后才 stat。
func draftReadOfficial(store *service.ProjectStore, dir, project, filename string, insideLock bool) (domain.OfficialScript, error) {
	path := filepath.Join(dir, "scripts", filename)
	var (
		content map[string]any
		err     error
	)
	if insideLock {
		content, _, err = service.ReadScriptMigrated(path)
	} else {
		content, err = store.ReadScriptJSON(project, filename)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			// 剧本文件不存在：空内容 + revision 0（Python read_official 的 exists 分支）。
			fingerprint, fpErr := domain.CanonicalFingerprint(map[string]any{})
			if fpErr != nil {
				return domain.OfficialScript{}, fpErr
			}
			return domain.OfficialScript{Content: map[string]any{}, Revision: 0, Fingerprint: fingerprint}, nil
		}
		return domain.OfficialScript{}, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return domain.OfficialScript{}, statErr
	}
	fingerprint, err := domain.CanonicalFingerprint(content)
	if err != nil {
		return domain.OfficialScript{}, err
	}
	return domain.OfficialScript{
		Content:     content,
		Revision:    info.ModTime().UnixNano(),
		Fingerprint: fingerprint,
	}, nil
}

// draftFromJSON 还原草稿记录；字段缺失 / 类型不符按坏条目处理（对应 Python 的
// _draft_from_dict 抛错 → list 侧跳过）。
func draftFromJSON(raw json.RawMessage) (*domain.Draft, error) {
	var draft domain.Draft
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&draft); err != nil {
		return nil, err
	}
	if draft.ID == "" || draft.Target.ProjectName == "" || draft.Target.ScriptFile == "" {
		return nil, errors.New("stored draft is invalid")
	}
	if draft.Status == "" {
		draft.Status = domain.DraftStatusActive
	}
	return &draft, nil
}

// ScriptStructureDraftValidator 注入给领域服务的结构校验：对应 Python router 的
// _validate_script_draft —— 先确认是 JSON 对象，再跑剧本结构校验，错误逐条转成
// script_structure_invalid 问题（Pydantic 的报错文本已是成品文案）。
func ScriptStructureDraftValidator() domain.DraftValidator {
	return func(content map[string]any) []domain.DraftValidationIssue {
		if content == nil {
			return []domain.DraftValidationIssue{{
				Code:    "draft_not_object",
				Message: "Draft content must be a JSON object.",
			}}
		}
		messages := domain.ValidateScriptStructure(content)
		if len(messages) == 0 {
			return nil
		}
		issues := make([]domain.DraftValidationIssue, 0, len(messages))
		for _, message := range messages {
			issues = append(issues, domain.DraftValidationIssue{Code: "script_structure_invalid", Message: message})
		}
		return issues
	}
}

// 编译期断言：仓储满足领域接缝。
var _ domain.DraftPromotionRepository = (*FileDraftPromotionRepository)(nil)
