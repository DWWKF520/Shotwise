package reference

import "github.com/shotwise/shotwise/internal/service"

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
)

func refProject() *domain.ProjectFile {
	characters := domain.AssetBucket{"李明": domain.NewAssetEntry()}
	return &domain.ProjectFile{Characters: characters}
}

func TestParsePromptAndRenderShotsTextRoundTrip(t *testing.T) {
	text := "镜头1：李明推开门。\n镜头2：他坐下。"
	shots, mentions := parsePrompt(text)
	if len(shots) != 2 || len(mentions) != 0 {
		t.Fatalf("want 2 shots, got %d shots / %d mentions", len(shots), len(mentions))
	}
	if got := renderShotsText(shots); got != text {
		t.Fatalf("round trip mismatch:\n got=%q\nwant=%q", got, text)
	}
	// 无 header 的整段视为单镜头。
	single, _ := parsePrompt("李明站在门口。")
	if len(single) != 1 {
		t.Fatalf("no header should be a single shot, got %d", len(single))
	}
}

func TestParsePromptMentions(t *testing.T) {
	_, mentions := parsePrompt("镜头1：@[李明] 推开 @[酒馆] 的门。")
	if len(mentions) != 2 || mentions[0] != "李明" || mentions[1] != "酒馆" {
		t.Fatalf("unexpected mentions: %v", mentions)
	}
	// 规范台词行的说话人不计入 references。
	if got := extractMentions("@[李明]：{你好}"); len(got) != 0 {
		t.Fatalf("dialogue speaker must not be a reference: %v", got)
	}
}

func TestValidateUnitTextRejectsUnregisteredAsset(t *testing.T) {
	_, _, err := ValidateUnitText("unit E1U01", "镜头1：@[张三] 走进来。", refProject(), nil)
	if err == nil {
		t.Fatal("unregistered asset should fail")
	}
}

func TestValidateUnitTextRejectsFullwidthBraces(t *testing.T) {
	_, _, err := ValidateUnitText("unit E1U01", "镜头1：他喊｛快来｝。", refProject(), nil)
	if err == nil {
		t.Fatal("fullwidth braces should fail")
	}
}

func TestValidateUnitTextRejectsBlankShot(t *testing.T) {
	_, _, err := ValidateUnitText("unit E1U01", "镜头1：李明走进来。\n镜头2：\n@[李明]：{你好}", refProject(), nil)
	if err == nil {
		t.Fatal("blank shot without description should fail")
	}
}

func TestValidateUnitTextDerivesReferences(t *testing.T) {
	shots, refs, err := ValidateUnitText("unit E1U01", "镜头1：@[李明] 推开门。", refProject(), nil)
	if err != nil {
		t.Fatalf("valid text: %v", err)
	}
	if len(shots) != 1 || len(refs) != 1 || refs[0]["type"] != "character" || refs[0]["name"] != "李明" {
		t.Fatalf("unexpected derive: shots=%v refs=%v", shots, refs)
	}
}

func TestValidateSourceTextAnchor(t *testing.T) {
	if err := validateSourceTextAnchor("u", "他推开门。", "他推开门。风灌了进来。"); err != nil {
		t.Fatalf("verbatim anchor should pass: %v", err)
	}
	if err := validateSourceTextAnchor("u", "杜撰的原文", "他推开门。"); err == nil {
		t.Fatal("fabricated anchor should fail")
	}
}

func TestValidateDialogueLoad(t *testing.T) {
	text := "@[李明]：{这是一句很短的话}"
	if err := ValidateDialogueLoad("u", text, 4, "zh", nil); err != nil {
		t.Fatalf("short dialogue should fit: %v", err)
	}
	long := "@[李明]：{这是一段非常非常非常长的话" + repeatRunes("很", 60) + "}"
	if err := ValidateDialogueLoad("u", long, 2, "zh", nil); err == nil {
		t.Fatal("overloaded dialogue should fail")
	}
}

func TestAssertDialoguePreserved(t *testing.T) {
	step1 := "镜头1：李明推门。\n@[李明]：{你好}"
	if err := assertDialoguePreserved("u", step1, "镜头1：李明推门走进屋。\n@[李明]：{你好}"); err != nil {
		t.Fatalf("visual expansion keeping dialogue should pass: %v", err)
	}
	if err := assertDialoguePreserved("u", step1, "镜头1：李明推门。\n@[李明]：{再见}"); err == nil {
		t.Fatal("rewritten dialogue should fail")
	}
}

func TestWriteStep1LockedDetectsConflict(t *testing.T) {
	dir := t.TempDir()
	// 基线指纹取自一份不存在的文件 → 现值也应为「不存在」；写入盘上文件后再比对即冲突。
	first, err := WriteStep1Locked(dir, 1, map[string]any{"units": []any{}}, BaselineFingerprint(nil), true)
	if err != nil || !first {
		t.Fatalf("initial write: changed=%v err=%v", first, err)
	}
	// 记录现值指纹。
	fingerprint, err := service.ContentFingerprint(OfficialReferenceStep1Path(dir, 1))
	if err != nil || fingerprint == nil {
		t.Fatalf("fingerprint: %v", err)
	}
	// 盘上文件随后被「其他写入方」改动。
	if err := os.WriteFile(OfficialReferenceStep1Path(dir, 1), []byte(`{"units":[{"unit_id":"E1U01"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = WriteStep1Locked(dir, 1, map[string]any{"units": []any{}}, BaselineFingerprint(fingerprint), true)
	var conflict *Step1WriteConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("want Step1WriteConflict, got %v", err)
	}
	if conflict.CurrentContent == nil {
		t.Fatal("conflict should carry the on-disk content")
	}
}

func TestStep1WriteLockCreatesDraftsDir(t *testing.T) {
	dir := t.TempDir()
	var seen string
	if err := Step1WriteLock(dir, 3, func(path string) error {
		seen = path
		return os.WriteFile(path, []byte("{}"), 0o644)
	}); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if seen != filepath.Join(service.EpisodeDraftsDir(dir, 3), service.ReferenceVideoStep1Filename) {
		t.Fatalf("unexpected step1 path: %s", seen)
	}
}

func TestCollectReferenceFlatViolations(t *testing.T) {
	project := refProject()
	caps := ReferenceSplitCaps{
		Durations:          []int{4, 6, 8},
		ReferenceDurations: []int{4, 6, 8},
		TextDurations:      []int{4, 6, 8},
		MaxDuration:        8,
	}
	flat := []map[string]any{
		{"duration_seconds": 6, "source_text": "他推开门。", "text": "镜头1：他推开门。"},
	}
	if v := CollectReferenceFlatViolations(flat, project, 1, "他推开门。风灌了进来。", caps, "zh"); len(v) != 0 {
		t.Fatalf("want no violations, got %v", v)
	}
	// 时长出档。
	bad := []map[string]any{{"duration_seconds": 5, "source_text": "他推开门。", "text": "镜头1：他推开门。"}}
	if v := CollectReferenceFlatViolations(bad, project, 1, "他推开门。", caps, "zh"); len(v) == 0 {
		t.Fatal("off-tier duration should be reported")
	}
}

func repeatRunes(ch string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += ch
	}
	return out
}
