package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// draftFakeRepo 是 DraftPromotionRepository 的内存实现，语义与
// server/draft_promotion.py 的 FileDraftPromotionRepository 对齐：
// promote 是同一临界区内的 compare-and-write。
type draftFakeRepo struct {
	official OfficialScript
	drafts   map[string]*Draft
	promoted int
}

func newDraftFakeRepo(content map[string]any, revision int64) *draftFakeRepo {
	fingerprint, err := CanonicalFingerprint(content)
	if err != nil {
		panic(err)
	}
	return &draftFakeRepo{official: OfficialScript{Content: content, Revision: revision, Fingerprint: fingerprint}, drafts: map[string]*Draft{}}
}

func (r *draftFakeRepo) ReadOfficial(DraftTarget) (OfficialScript, error) {
	return OfficialScript{
		Content:     deepCopyJSONObject(r.official.Content),
		Revision:    r.official.Revision,
		Fingerprint: r.official.Fingerprint,
	}, nil
}

func (r *draftFakeRepo) LoadDraft(draftID, actorID string) (*Draft, error) {
	draft, ok := r.drafts[draftID]
	if !ok {
		return nil, nil
	}
	if actorID != "" && draft.ActorID != actorID {
		return nil, nil
	}
	return draft, nil
}

func (r *draftFakeRepo) ListDrafts(target DraftTarget, actorID string) ([]*Draft, error) {
	var out []*Draft
	for _, draft := range r.drafts {
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

func (r *draftFakeRepo) ListAll(projectName, actorID string) ([]*Draft, error) {
	var out []*Draft
	for _, draft := range r.drafts {
		if draft.Target.ProjectName != projectName {
			continue
		}
		if actorID != "" && draft.ActorID != actorID {
			continue
		}
		out = append(out, draft)
	}
	return out, nil
}

func (r *draftFakeRepo) SaveDraft(draft *Draft) error {
	saved := *draft
	r.drafts[draft.ID] = &saved
	return nil
}

func (r *draftFakeRepo) PromoteAtomically(target DraftTarget, content map[string]any, expectedRevision int64, expectedFingerprint string) (OfficialScript, error) {
	if r.official.Revision != expectedRevision || r.official.Fingerprint != expectedFingerprint {
		return OfficialScript{}, &OfficialRevisionConflict{Current: r.ReadOfficialNow()}
	}
	fingerprint, err := CanonicalFingerprint(content)
	if err != nil {
		return OfficialScript{}, err
	}
	r.promoted++
	r.official = OfficialScript{Content: deepCopyJSONObject(content), Revision: r.official.Revision + 1, Fingerprint: fingerprint}
	return r.ReadOfficialNow(), nil
}

func (r *draftFakeRepo) ReadOfficialNow() OfficialScript {
	return OfficialScript{
		Content:     deepCopyJSONObject(r.official.Content),
		Revision:    r.official.Revision,
		Fingerprint: r.official.Fingerprint,
	}
}

// setOfficial 模拟外部写者改了正式剧本（revision 与 fingerprint 都变）。
func (r *draftFakeRepo) setOfficial(content map[string]any) {
	fingerprint, err := CanonicalFingerprint(content)
	if err != nil {
		panic(err)
	}
	r.official = OfficialScript{Content: deepCopyJSONObject(content), Revision: r.official.Revision + 100, Fingerprint: fingerprint}
}

func draftTestService(repo *draftFakeRepo) *DraftPromotionService {
	service := &DraftPromotionService{
		repo:         repo,
		idFactory:    func() string { return "draft-1" },
		tokenFactory: func() string { return "token-1" },
	}
	return service
}

func draftMustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// TestThreeWayMergeParityWithPython 与 server/draft_promotion.py 的
// _three_way_merge 实算输出逐字段对拍（冲突 / 自动合并路径 / 合并内容）。
func TestThreeWayMergeParityWithPython(t *testing.T) {
	cases := []struct {
		name             string
		base, cur, draft map[string]any
		wantConflicts    string
		wantAuto         []string
		wantContent      string
	}{
		{
			name:          "auto-merge",
			base:          map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "a"}}, "title": "t"},
			cur:           map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "a"}}, "title": "official"},
			draft:         map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "b"}}, "title": "t"},
			wantConflicts: `[]`,
			wantAuto:      []string{"/title"},
			wantContent:   `{"scenes":[{"desc":"b","scene_id":"s1"}],"title":"official"}`,
		},
		{
			name:          "conflict",
			base:          map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "a"}}},
			cur:           map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "x"}}},
			draft:         map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "y"}}},
			wantConflicts: `[{"path":"/scenes/s1/desc","base_value":"a","current_value":"x","draft_value":"y"}]`,
			wantContent:   `{}`,
		},
		{
			name:          "add-both",
			base:          map[string]any{"scenes": []any{map[string]any{"scene_id": "s1"}}},
			cur:           map[string]any{"scenes": []any{map[string]any{"scene_id": "s1"}, map[string]any{"scene_id": "s2"}}},
			draft:         map[string]any{"scenes": []any{map[string]any{"scene_id": "s1"}, map[string]any{"scene_id": "s3"}}},
			wantConflicts: `[]`,
			wantAuto:      []string{"/scenes/s2"},
			wantContent:   `{"scenes":[{"scene_id":"s1"},{"scene_id":"s2"},{"scene_id":"s3"}]}`,
		},
		{
			name:          "order-double-change",
			base:          map[string]any{"scenes": []any{map[string]any{"scene_id": "s1"}, map[string]any{"scene_id": "s2"}}},
			cur:           map[string]any{"scenes": []any{map[string]any{"scene_id": "s2"}, map[string]any{"scene_id": "s1"}}},
			draft:         map[string]any{"scenes": []any{map[string]any{"scene_id": "s1"}, map[string]any{"scene_id": "s2"}, map[string]any{"scene_id": "s3"}}},
			wantConflicts: `[]`,
			wantContent:   `{"scenes":[{"scene_id":"s2"},{"scene_id":"s1"},{"scene_id":"s3"}]}`,
		},
		{
			name:          "plain-array",
			base:          map[string]any{"tags": []any{"a"}},
			cur:           map[string]any{"tags": []any{"b"}},
			draft:         map[string]any{"tags": []any{"c"}},
			wantConflicts: `[{"path":"/tags","base_value":["a"],"current_value":["b"],"draft_value":["c"]}]`,
			wantContent:   `{}`,
		},
		{
			name:          "official-delete",
			base:          map[string]any{"a": 1.5, "b": 2.5},
			cur:           map[string]any{"a": 1.5},
			draft:         map[string]any{"a": 1.5, "b": 2.5},
			wantConflicts: `[]`,
			wantAuto:      []string{"/"},
			wantContent:   `{"a":1.5}`,
		},
		{
			name:          "numeric",
			base:          map[string]any{"a": float64(1)},
			cur:           map[string]any{"a": float64(2)},
			draft:         map[string]any{"a": float64(3)},
			wantConflicts: `[{"path":"/a","base_value":1,"current_value":2,"draft_value":3}]`,
			wantContent:   `{}`,
		},
		{
			name:          "pointer-escape",
			base:          map[string]any{"a/b": map[string]any{"x": float64(1)}, "c~d": map[string]any{"y": float64(1)}},
			cur:           map[string]any{"a/b": map[string]any{"x": float64(2)}, "c~d": map[string]any{"y": float64(1)}},
			draft:         map[string]any{"a/b": map[string]any{"x": float64(3)}, "c~d": map[string]any{"y": float64(1)}},
			wantConflicts: `[{"path":"/a~1b/x","base_value":1,"current_value":2,"draft_value":3}]`,
			// 内层 dict 分支恒返回合并 dict（哪怕因冲突而空），键不会被删除。
			// 数字按 Go 的 float64 口径序列化（1.0），见 CanonicalFingerprint 注释。
			wantContent: `{"a/b":{},"c~d":{"y":1.0}}`,
		},
		{
			name:          "draft-order-only",
			base:          map[string]any{"scenes": []any{map[string]any{"scene_id": "s1"}, map[string]any{"scene_id": "s2"}}},
			cur:           map[string]any{"scenes": []any{map[string]any{"scene_id": "s1"}, map[string]any{"scene_id": "s2"}}},
			draft:         map[string]any{"scenes": []any{map[string]any{"scene_id": "s2"}, map[string]any{"scene_id": "s1"}}},
			wantConflicts: `[]`,
			wantContent:   `{"scenes":[{"scene_id":"s2"},{"scene_id":"s1"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := threeWayMerge(tc.base, tc.cur, tc.draft)
			if got := draftMustJSON(t, merged.conflicts); got != tc.wantConflicts {
				t.Fatalf("conflicts = %s, want %s", got, tc.wantConflicts)
			}
			if !reflect.DeepEqual(merged.autoMergedPaths, tc.wantAuto) && !(len(merged.autoMergedPaths) == 0 && len(tc.wantAuto) == 0) {
				t.Fatalf("auto merged = %v, want %v", merged.autoMergedPaths, tc.wantAuto)
			}
			got, err := WorkflowCanonicalJSON(merged.content)
			if err != nil {
				t.Fatalf("canonical: %v", err)
			}
			if string(got) != tc.wantContent {
				t.Fatalf("content = %s, want %s", got, tc.wantContent)
			}
		})
	}
}

// TestCanonicalFingerprintParity 指纹与 Python canonical_fingerprint 逐字节一致
// （整位数字除外：Python 区分 int/float，Go 统一 float64，见 CanonicalFingerprint 注释）。
func TestCanonicalFingerprintParity(t *testing.T) {
	cases := []struct {
		content map[string]any
		want    string
	}{
		// 非整位数字：Python 与 Go 序列化同形，指纹必须逐字节一致。
		{map[string]any{"weight": 1.5, "s": "中文"}, "e238c6797e4380d2d595eea54d59e9ee9fc16ad934df36e7e84878093e229f14"},
		{map[string]any{"nested": map[string]any{"a": []any{1.5, 2.5, true, nil}}, "t": "x"}, "ee6938c3ae88ebdf9986cd4a957a856b0646fa1674d5bf043ca39264460f7545"},
		{map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "weight": 2.0}}, "tags": []any{"a", "b"}}, "14a94d67d22ad6ae11879c6e69769a952cc950eceeefb1cf7e2c2c066e1840fc"},
		{map[string]any{}, "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"},
	}
	for _, tc := range cases {
		got, err := CanonicalFingerprint(tc.content)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		if got != tc.want {
			t.Fatalf("fingerprint(%v) = %s, want %s", tc.content, got, tc.want)
		}
	}
}

// TestDraftPromotionServiceLifecycle 覆盖创建快照 / 更新 / 作废 / 准备 / 确认全链路。
func TestDraftPromotionServiceLifecycle(t *testing.T) {
	target := DraftTarget{ProjectName: "p1", ScriptFile: "episode_1.json"}
	repo := newDraftFakeRepo(map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "a"}}}, 7)
	svc := draftTestService(repo)

	draft, err := svc.Create(target, map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "b"}}}, DraftOriginAgent, "u1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if draft.ID != "draft-1" || draft.Status != DraftStatusActive || draft.Origin != DraftOriginAgent {
		t.Fatalf("draft = %+v", draft)
	}
	if draft.BaseRevision != 7 || draft.BaseFingerprint == "" {
		t.Fatalf("baseline not snapshotted: %+v", draft)
	}
	// create 深拷贝：改入参不能污染草稿。
	content := map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "b"}}}
	if _, err := svc.Create(target, content, DraftOriginUpload, "u1"); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	content["scenes"].([]any)[0].(map[string]any)["desc"] = "mutated"
	stored := repo.drafts["draft-1"]
	if stored.Content["scenes"].([]any)[0].(map[string]any)["desc"] != "b" {
		t.Fatalf("create must deep-copy content: %+v", stored.Content)
	}

	// 官方未变 → 直接 ready_for_confirmation。
	prep, err := svc.PreparePromotion("draft-1", "u1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Status != PreparationReadyForConfirmation || prep.ConfirmationToken != "token-1" {
		t.Fatalf("prep = %+v", prep)
	}
	if !reflect.DeepEqual(prep.PreviewContent, draft.Content) {
		t.Fatalf("preview = %v", prep.PreviewContent)
	}

	// 错误令牌 → invalid_confirmation；已作废 → invalid_confirmation。
	bad, err := svc.ConfirmPromotion("draft-1", "wrong", "u1")
	if err != nil || bad.Status != ResultInvalidConfirmation {
		t.Fatalf("bad token: %+v %v", bad, err)
	}

	// 确认前官方被外部改 → prepare 自动合并；再确认 → 原子晋升成功。
	repo.setOfficial(map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "a"}}, "title": "official"})
	prep, err = svc.PreparePromotion("draft-1", "u1")
	if err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	if prep.Status != PreparationReadyForConfirmation || !reflect.DeepEqual(prep.AutoMergedPaths, []string{"/title"}) {
		t.Fatalf("prep 2 = %+v", prep)
	}
	result, err := svc.ConfirmPromotion("draft-1", "token-1", "u1")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if result.Status != ResultPromoted || result.PromotedRevision == nil || *result.PromotedRevision != 108 {
		t.Fatalf("result = %+v", result)
	}
	if repo.promoted != 1 {
		t.Fatalf("promote_atomically calls = %d, want 1", repo.promoted)
	}
	// 晋升后草稿的 prepared 被清掉，不能拿旧令牌二次确认。
	again, err := svc.ConfirmPromotion("draft-1", "token-1", "u1")
	if err != nil || again.Status != ResultInvalidConfirmation {
		t.Fatalf("re-confirm = %+v %v", again, err)
	}
}

// TestDraftPromotionConflictsAndStale 冲突 / 陈旧确认 / 作废 / 归属隔离。
func TestDraftPromotionConflictsAndStale(t *testing.T) {
	target := DraftTarget{ProjectName: "p1", ScriptFile: "episode_1.json"}
	repo := newDraftFakeRepo(map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "a"}}}, 1)
	svc := draftTestService(repo)

	if _, err := svc.Create(target, map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "b"}}}, DraftOriginAgent, "u1"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 同一路径双改 → conflicted，且草稿上的 prepared 被清空。
	repo.setOfficial(map[string]any{"scenes": []any{map[string]any{"scene_id": "s1", "desc": "x"}}})
	prep, err := svc.PreparePromotion("draft-1", "u1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Status != PreparationConflicted || len(prep.Conflicts) != 1 {
		t.Fatalf("prep = %+v", prep)
	}
	if prep.Conflicts[0].Path != "/scenes/s1/desc" {
		t.Fatalf("conflict path = %q", prep.Conflicts[0].Path)
	}
	if repo.drafts["draft-1"].Prepared != nil {
		t.Fatal("conflicted prepare must clear prepared")
	}
	// 没有 prepared 时确认 → invalid_confirmation。
	bad, err := svc.ConfirmPromotion("draft-1", "token-1", "u1")
	if err != nil || bad.Status != ResultInvalidConfirmation {
		t.Fatalf("confirm without prepared: %+v %v", bad, err)
	}

	// 作废后：update 报错、prepare 报 invalid、abandon 幂等。
	abandoned, err := svc.Abandon("draft-1", "u1")
	if err != nil || abandoned.Status != DraftStatusAbandoned {
		t.Fatalf("abandon: %+v %v", abandoned, err)
	}
	if _, err := svc.Update("draft-1", map[string]any{"x": 1}, "u1"); !errors.Is(err, ErrDraftAbandoned) {
		t.Fatalf("update abandoned err = %v", err)
	}
	prep, err = svc.PreparePromotion("draft-1", "u1")
	if err != nil {
		t.Fatalf("prepare abandoned: %v", err)
	}
	if prep.Status != PreparationInvalid || prep.ValidationIssues[0].Code != "draft_abandoned" {
		t.Fatalf("prepare abandoned = %+v", prep)
	}
	if _, err := svc.Abandon("draft-1", "u1"); err != nil {
		t.Fatalf("abandon twice: %v", err)
	}

	// 归属隔离：别人的 actor 读不到 → ErrDraftNotFound。
	if _, err := svc.GetDraft("draft-1", "u2"); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("actor isolation err = %v", err)
	}
	if _, err := svc.GetDraft("nope", "u1"); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("unknown draft err = %v", err)
	}

	// stale_confirmation：prepare 之后、confirm 之前官方再变一次。
	repo2 := newDraftFakeRepo(map[string]any{"a": float64(1)}, 1)
	svc2 := draftTestService(repo2)
	if _, err := svc2.Create(target, map[string]any{"b": float64(2)}, DraftOriginOnlineAI, "u1"); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if _, err := svc2.PreparePromotion("draft-1", "u1"); err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	repo2.setOfficial(map[string]any{"a": float64(9)})
	stale, err := svc2.ConfirmPromotion("draft-1", "token-1", "u1")
	if err != nil || stale.Status != ResultStaleConfirmation {
		t.Fatalf("stale = %+v %v", stale, err)
	}
	if repo2.promoted != 0 {
		t.Fatalf("stale confirm must not write, promoted = %d", repo2.promoted)
	}
	if repo2.drafts["draft-1"].Prepared != nil {
		t.Fatal("stale confirm must clear prepared")
	}
}

// TestDraftValidatorInjection 校验器注入：非对象 / 结构错误都转成 issue。
func TestDraftValidatorInjection(t *testing.T) {
	var got map[string]any
	svc := &DraftPromotionService{
		repo:         newDraftFakeRepo(map[string]any{}, 0),
		idFactory:    func() string { return "draft-1" },
		tokenFactory: func() string { return "token-1" },
		validator: func(content map[string]any) []DraftValidationIssue {
			got = content
			return []DraftValidationIssue{{Code: "script_structure_invalid", Message: "scenes.0.desc: 必填"}}
		},
	}
	repo := svc.repo.(*draftFakeRepo)
	if _, err := svc.Create(DraftTarget{ProjectName: "p", ScriptFile: "s.json"}, map[string]any{"k": "v"}, DraftOriginAgent, "u1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	prep, err := svc.PreparePromotion("draft-1", "u1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Status != PreparationInvalid || prep.ValidationIssues[0].Code != "script_structure_invalid" {
		t.Fatalf("prep = %+v", prep)
	}
	if got["k"] != "v" {
		t.Fatalf("validator content = %v", got)
	}
	if repo.drafts["draft-1"].Prepared != nil {
		t.Fatal("invalid prepare must clear prepared")
	}

	// 默认校验器：nil 内容才报 draft_not_object。
	plain := &DraftPromotionService{repo: newDraftFakeRepo(map[string]any{}, 0), idFactory: func() string { return "d" }, tokenFactory: func() string { return "t" }}
	plainRepo := plain.repo.(*draftFakeRepo)
	if _, err := plain.Create(DraftTarget{ProjectName: "p", ScriptFile: "s.json"}, map[string]any{}, DraftOriginAgent, "u1"); err != nil {
		t.Fatalf("plain create: %v", err)
	}
	prep, err = plain.PreparePromotion("d", "u1")
	if err != nil {
		t.Fatalf("plain prepare: %v", err)
	}
	if prep.Status != PreparationReadyForConfirmation {
		t.Fatalf("plain prep = %+v", prep)
	}
	_ = plainRepo
}

// TestListDraftsSortedByID list_drafts 与 list_all 都按 id 字典序。
func TestListDraftsSortedByID(t *testing.T) {
	target := DraftTarget{ProjectName: "p1", ScriptFile: "episode_1.json"}
	repo := newDraftFakeRepo(map[string]any{}, 0)
	svc := draftTestService(repo)
	for _, id := range []string{"c", "a", "b"} {
		draft := &Draft{ID: id, Target: target, ActorID: "u1", Status: DraftStatusActive}
		repo.drafts[id] = draft
	}
	repo.drafts["other"] = &Draft{ID: "z", Target: DraftTarget{ProjectName: "p2", ScriptFile: "x.json"}, ActorID: "u1", Status: DraftStatusActive}
	repo.drafts["alien"] = &Draft{ID: "y", Target: target, ActorID: "u2", Status: DraftStatusActive}

	drafts, err := svc.ListDrafts(target, "u1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(drafts) != 3 || drafts[0].ID != "a" || drafts[1].ID != "b" || drafts[2].ID != "c" {
		t.Fatalf("list_drafts = %+v", drafts)
	}
	all, err := repo.ListAll("p1", "u1")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list_all = %d", len(all))
	}
}
