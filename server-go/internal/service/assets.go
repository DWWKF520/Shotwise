// 项目级资产（角色/场景/道具/产品）的读写服务。四类资产共用一套实现，
// 差异全部由 domain.AssetSpec 驱动 —— 与 Python 侧 build_asset_router 同源。
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// AssetService 承载资产 CRUD 与级联改名。
type AssetService struct{ store *ProjectStore }

// NewAssetService 构造资产服务。
func NewAssetService(store *ProjectStore) *AssetService { return &AssetService{store: store} }

var (
	// ErrAssetNotFound 桶里没有这个资产（按 NFC 坐标系解析后仍未命中）。
	ErrAssetNotFound = errors.New("asset not found")
	// ErrUnknownAssetKind 请求的资产类型没在 spec 注册表里。
	ErrUnknownAssetKind = errors.New("unknown asset kind")
)

// AssetNameError 资产名未通过边界校验。保留原始输入供文案回填。
type AssetNameError struct{ Raw string }

func (e *AssetNameError) Error() string { return "invalid asset name: " + e.Raw }

// AssetExistsError 同名资产已存在。Name 是桶里真实落盘的 key。
type AssetExistsError struct{ Name string }

func (e *AssetExistsError) Error() string { return "asset already exists: " + e.Name }

// AssetFieldErrorKind 区分「类型不对」与「值不合法」：前者是不被翻译的英文原文
// （Python 侧同样不走 Translator），后者是真实用户会看到的业务错误，须走 i18n。
type AssetFieldErrorKind string

const (
	FieldMustBeString AssetFieldErrorKind = "string"
	FieldMustBeList   AssetFieldErrorKind = "list"
	// FieldVoiceNoticeStale：voice_notice_dismissed_at 必须原样回填角色当前的
	// voice_updated_at。放行任意字符串会让客户端写入远未来时间戳，永久压制存量过渡横幅。
	FieldVoiceNoticeStale AssetFieldErrorKind = "voice_notice_stale"
)

// AssetFieldError 是请求体字段级校验失败（HTTP 422）。
type AssetFieldError struct {
	Field string
	Kind  AssetFieldErrorKind
}

func (e *AssetFieldError) Error() string {
	return fmt.Sprintf("field %q invalid (%s)", e.Field, e.Kind)
}

// RenameFileCollisionError：改名要迁移的落盘文件目标已被占用。
// 规划早于任何写入，因此整体拒绝、一处都不落盘。
type RenameFileCollisionError struct{ Path string }

func (e *RenameFileCollisionError) Error() string { return "rename target occupied: " + e.Path }

// RenameReport 是级联改名的影响报告。dry-run 预览与实际执行共用同一次扫描，数字必然一致。
type RenameReport struct {
	Table      string
	OldName    string
	NewName    string
	Episodes   int
	References int
	Files      int
	DryRun     bool
}

// Create 新增资产条目。extras 是请求体里除 name/description 之外的键，
// 只有 spec 登记的字符串/列表字段会落盘（其余键静默丢弃，避免污染 project.json）。
func (a *AssetService) Create(project string, kind domain.AssetKind, rawName, description string, extras map[string]any) (*domain.AssetEntry, error) {
	spec, ok := domain.Spec(kind)
	if !ok {
		return nil, ErrUnknownAssetKind
	}
	name, err := domain.ValidateAssetName(rawName)
	if err != nil {
		return nil, &AssetNameError{Raw: rawName}
	}
	strValues := map[string]string{}
	for _, field := range spec.ExtraStringFields {
		value, present := extras[field]
		if !present || value == nil {
			continue
		}
		if field == "voice_notice_dismissed_at" {
			// 新建角色尚无 voice_updated_at，PATCH 侧的等值校验在此恒不成立；
			// 直接拒绝创建时携带该字段，防止绕过等值校验（把已有角色整体复制进请求体）。
			return nil, &AssetFieldError{Field: field, Kind: FieldVoiceNoticeStale}
		}
		s, ok := value.(string)
		if !ok {
			return nil, &AssetFieldError{Field: field, Kind: FieldMustBeString}
		}
		strValues[field] = s
	}
	listValues := map[string][]string{}
	for _, field := range spec.ExtraListFields {
		value, present := extras[field]
		if !present || value == nil {
			continue
		}
		items, ok := toStringList(value)
		if !ok {
			return nil, &AssetFieldError{Field: field, Kind: FieldMustBeList}
		}
		listValues[field] = items
	}

	var created *domain.AssetEntry
	err = a.store.Update(project, func(pf *domain.ProjectFile) error {
		bucket := bucketFor(pf, kind)
		if existing, ok := bucket.ResolveKey(name); ok {
			return &AssetExistsError{Name: existing}
		}
		entry := domain.NewAssetEntry()
		entry.SetString("description", description)
		entry.SetString(spec.SheetKey, "")
		for _, field := range spec.ExtraStringFields {
			entry.SetString(field, strValues[field])
		}
		// 创建即带 reference_audio 也要机械戳 voice_updated_at：与 PATCH 侧同一理由，
		// 否则该角色的存量片段横幅感知不到这次「设置了声音」。
		if entry.MustString("reference_audio") != "" {
			entry.SetString("voice_updated_at", nowISO())
		}
		for _, field := range spec.ExtraListFields {
			entry.SetList(field, listValues[field])
		}
		bucket[name] = entry
		created = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Patch 按 spec 白名单更新条目字段。payload 里值为 nil 的字段跳过。
func (a *AssetService) Patch(project string, kind domain.AssetKind, entryName string, payload map[string]any) (*domain.AssetEntry, error) {
	spec, ok := domain.Spec(kind)
	if !ok {
		return nil, ErrUnknownAssetKind
	}
	stringFields := spec.WritableStringFields()
	listFields := spec.WritableListFields()

	// 写入前先做类型校验：错误类型会污染 project.json，并在下游把路径字段拼成非法路径。
	stringValues := map[string]string{}
	for _, field := range stringFields {
		value, present := payload[field]
		if !present || value == nil {
			continue
		}
		s, ok := value.(string)
		if !ok {
			return nil, &AssetFieldError{Field: field, Kind: FieldMustBeString}
		}
		stringValues[field] = s
	}
	listValues := map[string][]string{}
	for _, field := range listFields {
		value, present := payload[field]
		if !present || value == nil {
			continue
		}
		items, ok := toStringList(value)
		if !ok {
			return nil, &AssetFieldError{Field: field, Kind: FieldMustBeList}
		}
		listValues[field] = items
	}

	var updated *domain.AssetEntry
	err := a.store.Update(project, func(pf *domain.ProjectFile) error {
		bucket := bucketFor(pf, kind)
		key, ok := bucket.ResolveKey(entryName)
		if !ok {
			return ErrAssetNotFound
		}
		entry := bucket[key]
		if entry == nil {
			entry = domain.NewAssetEntry()
			bucket[key] = entry
		}
		for _, field := range stringFields {
			value, present := stringValues[field]
			if !present {
				continue
			}
			if field == "voice_notice_dismissed_at" && value != entry.MustString("voice_updated_at") {
				return &AssetFieldError{Field: field, Kind: FieldVoiceNoticeStale}
			}
			if field == "reference_audio" && value != entry.MustString("reference_audio") {
				entry.SetString("voice_updated_at", nowISO())
			}
			entry.SetString(field, value)
		}
		for _, field := range listFields {
			if values, present := listValues[field]; present {
				entry.SetList(field, values)
			}
		}
		updated = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Delete 删除条目。角色还须一并移除关系边，避免画布留下无法渲染的孤儿节点。
func (a *AssetService) Delete(project string, kind domain.AssetKind, entryName string) error {
	return a.store.Update(project, func(pf *domain.ProjectFile) error {
		bucket := bucketFor(pf, kind)
		key, ok := bucket.ResolveKey(entryName)
		if !ok {
			return ErrAssetNotFound
		}
		if kind == domain.AssetCharacter {
			pf.CharacterRelations = domain.RemoveRelationReferences(pf.CharacterRelations, key)
		}
		delete(bucket, key)
		return nil
	})
}

// Rename 级联改名：剧本/草稿引用、条目路径字段、磁盘文件、角色关系图一起同步。
//
// dryRun 与执行共用同一次扫描，预览数字与实际改动必然一致。
// 落盘顺序是「剧本 → 草稿 → 磁盘文件 → project.json 最后」：project.json 是资产目录的
// 真相源，它最后写意味着中途失败时旧名仍然自洽，重跑同一次改名即可收敛。
func (a *AssetService) Rename(project string, kind domain.AssetKind, oldName, newName string, dryRun bool) (RenameReport, error) {
	var report RenameReport
	spec, ok := domain.Spec(kind)
	if !ok {
		return report, ErrUnknownAssetKind
	}
	name, err := domain.ValidateAssetName(newName)
	if err != nil {
		return report, &AssetNameError{Raw: newName}
	}

	dir, err := a.store.Dir(project)
	if err != nil {
		return report, err
	}

	err = a.store.WithProjectLock(project, func() error {
		pf, err := a.store.load(dir)
		if err != nil {
			return err
		}
		bucket := bucketFor(pf, kind)
		oldKey, ok := bucket.ResolveKey(oldName)
		if !ok {
			return ErrAssetNotFound
		}
		if conflict, ok := bucket.ResolveKey(name); ok && conflict != oldKey {
			return &AssetExistsError{Name: conflict}
		}
		if oldKey == name {
			// 只是编码形式/空白差异：无需级联，直接返回零影响报告。
			report = RenameReport{Table: spec.BucketKey, OldName: oldKey, NewName: name, DryRun: dryRun}
			return nil
		}

		// —— 扫描 ——
		type pendingWrite struct {
			path    string
			payload map[string]any
		}
		var (
			writes     []pendingWrite
			episodeAt  = map[string]bool{}
			references int
		)
		scriptNames, err := a.store.ScriptFileNames(project)
		if err != nil {
			return err
		}
		for _, filename := range scriptNames {
			payload, err := a.store.ReadScriptJSON(project, filename)
			if err != nil {
				if errors.Is(err, fs.ErrNotFound) {
					continue
				}
				return err
			}
			changes := domain.RewritePayloadReferences(payload, kind, oldKey, name)
			if changes == 0 {
				continue
			}
			references += changes
			episodeAt[strings.TrimSuffix(filename, ".json")] = true
			writes = append(writes, pendingWrite{
				path:    filepath.Join(dir, "scripts", filename),
				payload: payload,
			})
		}
		draftPaths, err := a.store.DraftJSONPaths(project)
		if err != nil {
			return err
		}
		for _, p := range draftPaths {
			payload, err := a.store.readJSONObject(p)
			if err != nil {
				continue // 草稿可能是半成品/隔离的 .invalid.json，跳过而非中止
			}
			changes := domain.RewritePayloadReferences(payload, kind, oldKey, name)
			if changes == 0 {
				continue
			}
			references += changes
			episodeAt[filepath.Base(filepath.Dir(p))] = true
			writes = append(writes, pendingWrite{path: p, payload: payload})
		}

		moves, err := planAssetFileRenames(dir, spec, oldKey, name)
		if err != nil {
			return err
		}

		// project.json 的变更先在副本上应用并做「不更坏」校验，失败则整体拒绝。
		working := pf.Clone()
		allow := working.ValidateErrors()
		entry, ok := working.BucketRef(kind)
		if !ok {
			return ErrUnknownAssetKind
		}
		rekeyed, _ := (*entry).RekeyEquivalent(oldKey, name)
		if rekeyed != nil {
			domain.RewriteEntryPaths(rekeyed, spec, oldKey, name)
		}
		if kind == domain.AssetCharacter {
			// 关系图节点身份就是角色名：与剧本引用在同一事务内改写，
			// 否则画布会在改名成功后留下指向旧角色的悬空边。
			working.CharacterRelations = domain.RenameRelationReferences(working.CharacterRelations, oldKey, name)
		}
		if fresh := domain.NewErrors(working.ValidateErrors(), allow); len(fresh) > 0 {
			return &ProjectInvalidError{Errors: fresh}
		}

		// files：迁移的文件数。版本历史快照（versions.json + versions/ 目录）待
		// versions 模块落地后一并计入并改名，那里改名的语义与本处一致。
		report = RenameReport{
			Table:      spec.BucketKey,
			OldName:    oldKey,
			NewName:    name,
			Episodes:   len(episodeAt),
			References: references,
			Files:      len(moves),
			DryRun:     dryRun,
		}
		if dryRun {
			return nil
		}

		// —— 落盘，每步幂等 ——
		for _, w := range writes {
			if err := fs.WriteJSONAtomic(w.path, w.payload); err != nil {
				return err
			}
		}
		for _, m := range moves {
			if err := os.Rename(m.src, m.dst); err != nil {
				return fmt.Errorf("move %s: %w", m.src, err)
			}
		}
		return a.store.persistUnlocked(dir, working, allow)
	})
	if err != nil {
		return RenameReport{}, err
	}
	return report, nil
}

type fileMove struct{ src, dst string }

// planAssetFileRenames 扫描该类型目录（本级 + refs / refs_audio），规划 stem 命中旧名的文件迁移。
//
// 按目录扫描而非只信条目里的路径字段：生成中间产物可能已按旧名落盘而字段未写，
// 旧名文件不应残留。「旧名_{序号}」形态只在多图序列资产的 refs 子目录放行，
// 否则兄弟资产「旧名_2」的设计图会被一并卷走。
func planAssetFileRenames(projectDir string, spec domain.AssetSpec, oldName, newName string) ([]fileMove, error) {
	sequencedRefs := spec.SequencedRefs()
	var (
		moves   []fileMove
		planned = map[string]bool{}
	)
	for i, relDir := range spec.MigratedDirs() {
		allowSequence := i == 1 && sequencedRefs // 只有 refs 子目录放行序号形态
		abs := filepath.Join(projectDir, filepath.FromSlash(relDir))
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			continue
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			src := filepath.Join(abs, e.Name())
			stem, ext := splitBaseStemExt(e.Name())
			renamed, ok := domain.RenamedFileStem(stem, oldName, newName, allowSequence)
			if !ok || renamed == stem {
				continue
			}
			dst := filepath.Join(abs, renamed+ext)
			// 占用有两种来源：磁盘上的孤儿文件，以及同批两个源文件（NFC / NFD 两种编码
			// 的同名文件）撞到同一个目标——后者若放行，第二次 rename 会吃掉第一次的成果。
			if planned[dst] {
				return nil, &RenameFileCollisionError{Path: dst}
			}
			if existing, err := os.Stat(dst); err == nil && !sameFile(src, existing) {
				return nil, &RenameFileCollisionError{Path: dst}
			}
			planned[dst] = true
			moves = append(moves, fileMove{src: src, dst: dst})
		}
	}
	return moves, nil
}

func splitBaseStemExt(base string) (string, string) {
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return base, ""
	}
	return base[:dot], base[dot:]
}

// sameFile 判定两个路径是否指向同一 inode（大小写/归一不敏感的文件系统上，
// 仅改大小写的改名目标会解析回源文件自身，那不是占用）。
func sameFile(src string, st os.FileInfo) bool {
	srcSt, err := os.Stat(src)
	if err != nil {
		return false
	}
	return os.SameFile(srcSt, st)
}

func bucketFor(pf *domain.ProjectFile, kind domain.AssetKind) domain.AssetBucket {
	ref, ok := pf.BucketRef(kind)
	if !ok {
		return domain.AssetBucket{}
	}
	if *ref == nil {
		*ref = domain.AssetBucket{}
	}
	return *ref
}

func toStringList(value any) ([]string, bool) {
	raw, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
