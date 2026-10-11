package domain

import (
	"encoding/json"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestValidateAssetName(t *testing.T) {
	cases := []struct {
		raw   string
		want  string
		valid bool
	}{
		{"阿明", "阿明", true},
		{"  Armin  ", "Armin", true},
		{"张三.二", "张三.二", true},
		{"a b", "a b", true},
		{"", "", false},
		{"   ", "", false},
		{"a/b", "", false},
		{"a\\b", "", false},
		{"a:b", "", false},
		{"a*b", "", false},
		{"a?b", "", false},
		{"a\"b", "", false},
		{"a<b", "", false},
		{"a>b", "", false},
		{"a|b", "", false},
		{"a]b", "", false},
		{"a..b", "", false},
		{"tail.", "", false},
		{"a\x07b", "", false},
		{"CON", "", false},
		{"con.backup", "", false},
		{"LPT9", "", false},
		{"NUL.txt", "", false},
		{"laptop", "laptop", true}, // 保留名前缀但非保留名，放行
	}
	for _, c := range cases {
		got, err := ValidateAssetName(c.raw)
		if c.valid {
			if err != nil || got != c.want {
				t.Errorf("ValidateAssetName(%q) = %q, %v; want %q, nil", c.raw, got, err, c.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("ValidateAssetName(%q) = %q, want error", c.raw, got)
		}
	}
}

func TestValidateAssetNameNormalizesToNFC(t *testing.T) {
	nfd := norm.NFD.String("é") // 分解形式，与 NFC 肉眼同字、字节不同
	got, err := ValidateAssetName(nfd)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if norm.NFC.String(got) != got {
		t.Errorf("name %q is not NFC", got)
	}
}

func TestBucketResolveKeyCoversNFDDuplicates(t *testing.T) {
	nfdKey := norm.NFD.String("été")
	bucket := AssetBucket{nfdKey: WrapAssetEntry(map[string]any{"description": "旧"})}

	// 查询侧是 NFC，落盘 key 是 NFD：必须解析出真实 key，否则会对同一视觉名字新建第二条。
	found, ok := bucket.ResolveKey("été")
	if !ok || found != nfdKey {
		t.Fatalf("ResolveKey = %q, %v; want %q", found, ok, nfdKey)
	}

	bucket["été"] = WrapAssetEntry(map[string]any{"description": "新"})
	entry, ok := bucket.RekeyEquivalent("été", "été2")
	if !ok || entry == nil {
		t.Fatalf("RekeyEquivalent found nothing")
	}
	if _, still := bucket[nfdKey]; still {
		t.Error("NFD 等价 key 未被收编，旧名会带着失效路径字段留在桶里")
	}
	if got := bucket["été2"].Description(); got != "新" {
		t.Errorf("胜出的应是后写入的条目，got %q", got)
	}
}

func TestRewriteMentions(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
		n    int
	}{
		{"wrapped", "@[阿明] 走进房间", "@[阿明那] 走进房间", 1},
		{"bare 升格为包裹形态", "镜头里的 阿明 和@阿明 一起", "镜头里的 阿明 和@[阿明那] 一起", 1},
		{"email 片段不算 mention", "mail me at a@b.com", "mail me at a@b.com", 0},
		{"变体 slug 保留", "@[阿明/少年]", "@[阿明那/少年]", 1},
		{"换行不跨", "@[阿明\n] 断掉", "@[阿明\n] 断掉", 0},
		{"NFD 正文也命中", norm.NFD.String("@[é]") + " x", "@[é2] x", 1},
		{"已是目标形式不计数", "@[阿明那] 已改过", "@[阿明那] 已改过", 0},
	}
	for _, c := range cases {
		got, n := RewriteMentions(c.text, "阿明", "阿明那")
		if c.name == "NFD 正文也命中" {
			got, n = RewriteMentions(c.text, "é", "é2")
		}
		if got != c.want || n != c.n {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", c.name, got, n, c.want, c.n)
		}
	}
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestRewritePayloadReferences(t *testing.T) {
	payload := decodeJSON(t, `{
	  "segments": [
		{"segment_id": "E1S1", "characters_in_segment": ["阿明", "阿红"],
		 "scenes": ["村口"], "props": ["剑"],
		 "utterances": [{"kind": "dialogue", "speaker": "阿明", "text": "hi"}]},
		{"segment_id": "E1S2", "characters_in_segment": ["阿明"], "video_prompt": {"dialogue": [{"speaker": "阿明"}]}}
	  ],
	  "units": [{"unit_id": "E1S1_1", "text": "@[阿明] 与 @阿红 同框"}]
	}`)
	got := RewritePayloadReferences(payload, AssetCharacter, "阿明", "阿明那")
	if want := 5; got != want {
		t.Errorf("character rewrites = %d, want %d", got, want)
	}
	first := payload["segments"].([]any)[0].(map[string]any)
	list := first["characters_in_segment"].([]any)
	if list[0] != "阿明那" || list[1] != "阿红" {
		t.Errorf("list not rewritten: %v", list)
	}
	speaker := first["utterances"].([]any)[0].(map[string]any)["speaker"]
	if speaker != "阿明那" {
		t.Errorf("speaker = %v", speaker)
	}
	unitText := payload["units"].([]any)[0].(map[string]any)["text"]
	if unitText != "@[阿明那] 与 @阿红 同框" {
		t.Errorf("unit text = %v", unitText)
	}
}

func TestRewritePayloadReferencesSceneUsesDictAwareList(t *testing.T) {
	// drama 顶层 scenes 是 dict 列表，与 narration 的场景名列表同 key 不同形：
	// 只有 str 元素才算名称引用。
	payload := decodeJSON(t, `{
	  "scenes": [{"scene_id": "E1S1", "scenes_in_scene": ["村口"], "characters_in_scene": ["阿明"]}],
	  "names": ["村口"]
	}`)
	narration := map[string]any{
		"segments": []any{map[string]any{"scenes": []any{"村口", "渡口"}}},
	}
	if got := RewritePayloadReferences(payload, AssetScene, "村口", "村口新"); got != 0 {
		t.Errorf("dict-shaped scenes must not be rewritten, got %d", got)
	}
	if got := RewritePayloadReferences(narration, AssetScene, "村口", "村口新"); got != 1 {
		t.Errorf("str-shaped scene list got %d, want 1", got)
	}
	list := narration["segments"].([]any)[0].(map[string]any)["scenes"].([]any)
	if list[0] != "村口新" {
		t.Errorf("list = %v", list)
	}
}

func TestRewritePayloadReferencesTypedRefs(t *testing.T) {
	payload := decodeJSON(t, `{
	  "video_units": [{"unit_id": "E1S1_1", "references": [
		{"type": "character", "name": "阿明"},
		{"type": "scene", "name": "阿明"}
	  ]}]
	}`)
	if got := RewritePayloadReferences(payload, AssetProduct, "阿明", "产品A"); got != 0 {
		t.Errorf("product must not rewrite character-typed refs, got %d", got)
	}
	if got := RewritePayloadReferences(payload, AssetCharacter, "阿明", "阿明那"); got != 1 {
		t.Errorf("character typed ref got %d, want 1", got)
	}
}

func TestRewritePayloadReferencesLegacyEmbeddedCharacters(t *testing.T) {
	payload := decodeJSON(t, `{"characters": {"阿明": {"character_sheet": "characters/阿明.png"}}}`)
	got := RewritePayloadReferences(payload, AssetCharacter, "阿明", "阿明那")
	if got != 2 { // key 改写 + sheet 路径同步
		t.Errorf("legacy mirror rewrites = %d, want 2", got)
	}
	embedded := payload["characters"].(map[string]any)
	if _, stale := embedded["阿明"]; stale {
		t.Error("旧名镜像残留")
	}
	if entry := embedded["阿明那"].(map[string]any); entry["character_sheet"] != "characters/阿明那.png" {
		t.Errorf("mirror path not synced: %v", entry["character_sheet"])
	}
}

func TestRenamedFileStemAndRelPath(t *testing.T) {
	if stem, ok := RenamedFileStem("阿明", "阿明", "阿明那", false); !ok || stem != "阿明那" {
		t.Errorf("exact stem: %q %v", stem, ok)
	}
	if _, ok := RenamedFileStem("阿明_2", "阿明", "阿明那", false); ok {
		t.Error("序号形态在普通目录不该放行，否则兄弟资产的文件会被卷走")
	}
	if stem, ok := RenamedFileStem("阿明_2", "阿明", "阿明那", true); !ok || stem != "阿明那_2" {
		t.Errorf("sequenced stem: %q %v", stem, ok)
	}
	if stem, ok := RenamedFileStem("阿明_x", "阿明", "阿明那", true); ok {
		t.Errorf("非纯数字后缀不该被当作序号: %q", stem)
	}
	if p, ok := RenamedRelPath(`characters\阿明.png`, "阿明", "阿明那", false); !ok || p != "characters/阿明那.png" {
		t.Errorf("relpath with backslash: %q %v", p, ok)
	}
	if _, ok := RenamedRelPath("thumbnails/阿明.png", "阿明", "阿明那", false); !ok {
		t.Error("stem 命中即应可改写，目录限制在 RewriteEntryPaths 里判")
	}
	if stem, ok := RenamedFileStem(".甲", "甲", "乙", false); ok {
		t.Errorf("前导点属于 stem，不该命中: %q", stem)
	}
}

func TestRewriteEntryPathsConfinedToMigratedDirs(t *testing.T) {
	spec := AssetSpecs[AssetCharacter]
	entry := WrapAssetEntry(map[string]any{
		"character_sheet":  "characters/阿明.png",
		"character_avatar": "characters/refs/阿明_avatar_manual.png",
		"reference_audio":  "characters/refs_audio/阿明.wav",
		"reference_image":  "thumbnails/阿明.png",
		"voice_style":      "低沉",
	})
	n := RewriteEntryPaths(entry, spec, "阿明", "阿明那")
	if n != 2 { // sheet + reference_audio；avatar 的 stem 是「阿明_avatar_manual」不算命中
		t.Errorf("rewrites = %d, want 2", n)
	}
	if got := entry.MustString("character_sheet"); got != "characters/阿明那.png" {
		t.Errorf("sheet = %q", got)
	}
	if got := entry.MustString("reference_image"); got != "thumbnails/阿明.png" {
		t.Errorf("用户手动指到别处的路径不该被改成悬空引用: %q", got)
	}
	if got := entry.MustString("reference_audio"); got != "characters/refs_audio/阿明那.wav" {
		t.Errorf("audio = %q", got)
	}
	if got := entry.MustString("voice_style"); got != "低沉" {
		t.Errorf("voice_style 不是路径字段: %q", got)
	}
}

func TestRewriteEntryPathsSequencedImages(t *testing.T) {
	spec := AssetSpecs[AssetProduct]
	entry := WrapAssetEntry(map[string]any{
		"reference_images": []any{"products/refs/甲_1.png", "products/refs/甲_2.png", "products/甲_3.png"},
	})
	if n := RewriteEntryPaths(entry, spec, "甲", "甲新"); n != 2 {
		t.Errorf("rewrites = %d, want 2 (refs 子目录放行序号，本级不放行)", n)
	}
	images := entry.Fields()["reference_images"].([]any)
	if images[2] != "products/甲_3.png" {
		t.Errorf("兄弟资产形态的文件被误卷走: %v", images[2])
	}
}

func TestRelationReferencesRenameAndRemove(t *testing.T) {
	relations := decodeJSON(t, `{
	  "revision": 3,
	  "edges": [{"source": "阿明", "target": "阿红"}, {"source": "阿红", "target": "阿明"}],
	  "suppressed_pairs": [{"source": "阿明", "target": "阿红"}],
	  "node_positions": {"阿明": {"x": 1}, "阿红": {"x": 2}}
	}`)
	renamed := RenameRelationReferences(cloneMap(relations), "阿明", "阿明那")
	edges := renamed["edges"].([]any)
	if e := edges[0].(map[string]any); e["source"] != "阿明那" {
		t.Errorf("edge source = %v", e["source"])
	}
	if e := edges[1].(map[string]any); e["target"] != "阿明那" {
		t.Errorf("edge target = %v", e["target"])
	}
	if _, stale := renamed["node_positions"].(map[string]any)["阿明"]; stale {
		t.Error("旧节点坐标残留，画布会有悬空节点")
	}

	removed := RemoveRelationReferences(cloneMap(relations), "阿明")
	if len(removed["edges"].([]any)) != 0 {
		t.Errorf("删角色后仍残留关系边: %v", removed["edges"])
	}
	if len(removed["suppressed_pairs"].([]any)) != 0 {
		t.Error("AI 删除抑制记录未清理")
	}
	if pos := removed["node_positions"].(map[string]any); len(pos) != 1 {
		t.Errorf("node_positions = %v", pos)
	}
}

func TestAssetEntryPreservesUnknownKeysAndOrdersDescription(t *testing.T) {
	raw := `{"description":"d","voice_style":"低沉","future_field":{"nested":[1,2]}}`
	var entry AssetEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(&entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// extra="allow" 语义：Go 服务写回不能丢掉自己不认识的字段。
	if !containsString(sortedTopKeys(t, out), "future_field") {
		t.Fatalf("unknown key lost: %s", out)
	}
	if string(out[:len(`{"description":"d"`)]) != `{"description":"d"` {
		t.Errorf("description 应排在最前: %s", out)
	}

	// 类型不符的脏数据按缺失处理，不让它渗进路径拼接。
	dirty := WrapAssetEntry(map[string]any{"reference_images": []any{"a", 1}})
	if _, ok := dirty.List("reference_images"); ok {
		t.Error("含非字符串元素的列表应判为不可用")
	}
}

func sortedTopKeys(t *testing.T, data []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode keys: %v", err)
	}
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAssetSpecDrivesWritableSurface(t *testing.T) {
	character := AssetSpecs[AssetCharacter]
	if got := character.WritableStringFields(); len(got) != 7 {
		t.Errorf("character writable string fields = %v", got)
	}
	if character.SequencedRefs() {
		t.Error("character 没有多图序列字段")
	}
	product := AssetSpecs[AssetProduct]
	if !product.SequencedRefs() {
		t.Error("product 的 refs 目录应按序号放行")
	}
	if product.InGlobalLibrary {
		t.Error("全局资产库是单图列模型，product 暂不进入")
	}
}
