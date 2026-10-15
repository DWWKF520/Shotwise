package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
)

type fakeProjects struct{ pf *domain.ProjectFile }

func (f *fakeProjects) Update(_ string, mutate func(*domain.ProjectFile) error) error {
	return mutate(f.pf)
}

type fakeAssets struct {
	calls  []string
	fields []map[string]any
	fresh  map[string]bool
}

func (f *fakeAssets) Upsert(_ string, kind domain.AssetKind, name string, fields map[string]any) (bool, error) {
	f.calls = append(f.calls, string(kind)+":"+name)
	f.fields = append(f.fields, fields)
	return f.fresh[name], nil
}

func TestPositiveIntCoercion(t *testing.T) {
	if n, err := positiveInt("episode_target_units", "10"); err != nil || n != 10 {
		t.Fatalf("string '10' -> %d, %v", n, err)
	}
	if n, err := positiveInt("episode_target_units", float64(7)); err != nil || n != 7 {
		t.Fatalf("number 7 -> %d, %v", n, err)
	}
	for _, bad := range []any{"10.5", "10.0", "abc", "", float64(0), float64(2.5), true} {
		if _, err := positiveInt("episode_target_units", bad); err == nil {
			t.Errorf("positiveInt(%v) should fail", bad)
		}
	}
}

func TestPositiveFloatCoercion(t *testing.T) {
	if v, err := positiveFloat("1.5"); err != nil || v != 1.5 {
		t.Fatalf("'1.5' -> %v, %v", v, err)
	}
	for _, bad := range []any{"0", float64(-1), "NaN", "inf"} {
		if _, err := positiveFloat(bad); err == nil {
			t.Errorf("positiveFloat(%v) should fail", bad)
		}
	}
}

func TestApplySettingsWritesCoercedValuesAndClears(t *testing.T) {
	pf := &domain.ProjectFile{ContentMode: "drama", SourceLanguage: "zh"}
	p := &fakeProjects{pf: pf}

	out, err := applySettings(p, "proj", map[string]any{
		"episode_target_units": "12",
		"narration_speed":      "1.25",
		"source_language":      "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pf.EpisodeTargetUnits == nil || *pf.EpisodeTargetUnits != 12 {
		t.Fatalf("episode_target_units not stored as int 12: %+v", pf.EpisodeTargetUnits)
	}
	if pf.NarrationSpeed == nil || *pf.NarrationSpeed != 1.25 || pf.SourceLanguage != "en" {
		t.Fatalf("settings not applied: speed=%v lang=%q", pf.NarrationSpeed, pf.SourceLanguage)
	}
	if !strings.Contains(out, "已更新") {
		t.Fatalf("diagnostics = %q", out)
	}

	if _, err := applySettings(p, "proj", map[string]any{"episode_target_units": nil}); err != nil {
		t.Fatal(err)
	}
	if pf.EpisodeTargetUnits != nil {
		t.Fatal("null must clear episode_target_units")
	}
}

func TestApplySettingsRejectsBriefOutsideAd(t *testing.T) {
	pf := &domain.ProjectFile{ContentMode: "drama"}
	if _, err := applySettings(&fakeProjects{pf: pf}, "proj", map[string]any{"brief": "x"}); err == nil {
		t.Fatal("brief must be rejected for non-ad projects")
	}
	if _, err := applySettings(&fakeProjects{pf: pf}, "proj", map[string]any{"brief": nil}); err == nil {
		t.Fatal("brief key with null must also be gated for non-ad projects")
	}
}

func TestApplySettingsRejectsUnknownKeyAndWritesNothing(t *testing.T) {
	pf := &domain.ProjectFile{ContentMode: "ad", Brief: "keep"}
	_, err := applySettings(&fakeProjects{pf: pf}, "proj", map[string]any{"brief": "new", "not_a_field": 1})
	if err == nil {
		t.Fatal("unknown settings key must be rejected")
	}
	if pf.Brief != "keep" {
		t.Fatal("validation failure must not mutate the project")
	}
}

func TestApplyOverviewMergesOnlyPassedFields(t *testing.T) {
	pf := &domain.ProjectFile{Overview: &domain.Overview{Synopsis: "old", Genre: "drama"}}
	out, err := applyOverview(&fakeProjects{pf: pf}, "proj", map[string]any{"theme": "hope"})
	if err != nil {
		t.Fatal(err)
	}
	if pf.Overview.Theme != "hope" || pf.Overview.Synopsis != "old" || pf.Overview.Genre != "drama" {
		t.Fatalf("merge semantics broken: %+v", pf.Overview)
	}
	if !strings.Contains(out, "overview") {
		t.Fatalf("diagnostics = %q", out)
	}
	if _, err := applyOverview(&fakeProjects{pf: pf}, "proj", map[string]any{"language": "zh"}); err == nil {
		t.Fatal("non-whitelisted overview field must be rejected")
	}
}

func TestApplyUpsertFiltersSheetFieldsAndReportsCreated(t *testing.T) {
	a := &fakeAssets{fresh: map[string]bool{"阿明": true}}
	out, err := applyUpsert(a, "proj", "characters", map[string]map[string]any{
		"阿明": {"description": "少年，黑发", "character_sheet": "characters/x.png"},
		"小红": {"description": "少女", "voice_style": "warm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.calls) != 2 {
		t.Fatalf("expected 2 upserts, got %v", a.calls)
	}
	if _, has := a.fields[0]["character_sheet"]; has {
		t.Fatal("sheet field must be dropped before upsert")
	}
	if !strings.Contains(out, "新增 1 个: 阿明") || !strings.Contains(out, "合并改字段 1 个: 小红") {
		t.Fatalf("summary = %q", out)
	}
	if !strings.Contains(out, "character_sheet") {
		t.Fatalf("dropped field must be reported: %q", out)
	}
}

func TestApplyUpsertRejectsUnknownTable(t *testing.T) {
	if _, err := applyUpsert(&fakeAssets{}, "proj", "weapons", map[string]map[string]any{"x": {}}); err == nil {
		t.Fatal("unknown table must be rejected")
	}
}

func TestPatchProjectToolRejectsMultipleBranches(t *testing.T) {
	tl, err := newPatchProjectTool("proj", &fakeAssets{}, &fakeProjects{pf: &domain.ProjectFile{}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{"settings":{"brief":"x"},"overview":{"theme":"y"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "三选一") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("expected deny envelope, got %s", out)
	}
}
