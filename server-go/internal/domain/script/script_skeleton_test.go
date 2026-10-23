package script

import "testing"

func TestResolveDeclaredKindCoversModeMatrix(t *testing.T) {
	cases := []struct {
		contentMode, genMode, want string
	}{
		{"ad", "storyboard", "shots"},
		{"ad", "reference_video", "shots"},
		{"narration", "storyboard", "segments"},
		{"narration", "reference_video", "video_units"},
		{"drama", "storyboard", "scenes"},
		{"drama", "reference_video", "video_units"},
	}
	for _, c := range cases {
		got, err := ResolveDeclaredKind(c.contentMode, c.genMode)
		if err != nil || got != c.want {
			t.Errorf("ResolveDeclaredKind(%q,%q) = %q, %v；期望 %q", c.contentMode, c.genMode, got, err, c.want)
		}
	}
	// 未知 content_mode fail loud，不静默兜底。
	if _, err := ResolveDeclaredKind("", "storyboard"); err == nil {
		t.Error("空 content_mode 应报错")
	}
	if _, err := ResolveDeclaredKind("comic", "storyboard"); err == nil {
		t.Error("未知 content_mode 应报错")
	}
}

func TestResolveScriptKindPrefersDataShape(t *testing.T) {
	cases := []struct {
		name   string
		script map[string]any
		want   string
	}{
		{"reference 独存", map[string]any{"video_units": []any{}}, "video_units"},
		{"残留分镜族数组时不算 reference", map[string]any{"video_units": []any{}, "segments": []any{}}, "segments"},
		{"ad 恒 shots", map[string]any{"content_mode": "ad", "scenes": []any{}}, "shots"},
		{"drama → scenes", map[string]any{"content_mode": "drama"}, "scenes"},
		{"narration 正常", map[string]any{"content_mode": "narration"}, "segments"},
		{"narration 数据落 scenes 的历史形态", map[string]any{"content_mode": "narration", "scenes": []any{}}, "scenes"},
		{"缺 content_mode 时有 scenes", map[string]any{"scenes": []any{}}, "scenes"},
		{"缺 content_mode 时有 shots", map[string]any{"shots": []any{}}, "shots"},
		{"全缺兜底 segments", map[string]any{}, "segments"},
	}
	for _, c := range cases {
		if got := ResolveScriptKind(c.script); got != c.want {
			t.Errorf("%s: ResolveScriptKind = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestEnsureRouteSkeletonGatesCrossFamilyOnly(t *testing.T) {
	// 参考路线只问 video_units 键在不在。
	if kind, err := EnsureRouteSkeleton(map[string]any{"video_units": []any{}}, "drama", "reference_video"); err != nil || kind != "video_units" {
		t.Errorf("参考路线放行失败: %q %v", kind, err)
	}
	// 参考路线上残留分镜族数组不参与投票。
	if kind, err := EnsureRouteSkeleton(
		map[string]any{"video_units": []any{}, "segments": []any{}}, "narration", "reference_video"); err != nil || kind != "video_units" {
		t.Errorf("残留数组不应改变参考路线判定: %q %v", kind, err)
	}
	// 分镜路线只问分镜族键有没有一个在场，族内差异放行。
	if kind, err := EnsureRouteSkeleton(map[string]any{"scenes": []any{}}, "narration", "storyboard"); err != nil || kind != "scenes" {
		t.Errorf("族内历史形态应放行: %q %v", kind, err)
	}
	// 跨族拒绝。
	_, err := EnsureRouteSkeleton(map[string]any{"scenes": []any{}}, "drama", "reference_video")
	var mismatch *SkeletonRouteMismatchError
	if err == nil {
		t.Fatal("跨族应拒绝")
	}
	if e, ok := err.(*SkeletonRouteMismatchError); ok {
		mismatch = e
	}
	if mismatch == nil || !mismatch.HasActual || mismatch.Actual != "scenes" || mismatch.Expected != "video_units" {
		t.Fatalf("失配错误内容不符: %+v", err)
	}
	// 三个键全缺时不能走「合成答案」的假成功。
	_, err = EnsureRouteSkeleton(map[string]any{}, "drama", "storyboard")
	if err == nil {
		t.Fatal("无任何骨架数组应拒绝")
	}
	if e, ok := err.(*SkeletonRouteMismatchError); !ok || e.HasActual {
		t.Fatalf("无数组的失配应标记 HasActual=false: %v", err)
	}
	// 键在但值类型脏是类型错误，不是路线失配：闸门不越俎代庖。
	if _, err := EnsureRouteSkeleton(map[string]any{"video_units": map[string]any{}}, "drama", "reference_video"); err != nil {
		t.Fatalf("脏值不应被路线闸门拦下: %v", err)
	}
}

func TestScriptSkeletonRegistryIsConsistent(t *testing.T) {
	if len(ScriptSkeletons) != 4 {
		t.Fatalf("骨架表应恰有四类: %v", ScriptSkeletons)
	}
	expected := map[string]bool{"segments": true, "scenes": true, "shots": true, "video_units": true}
	for name, table := range map[string]map[string]string{
		"ScriptSkeletonItemNouns":   ScriptSkeletonItemNouns,
		"ScriptSkeletonEntityTypes": ScriptSkeletonEntityTypes,
		"ScriptSkeletonAnchorTypes": ScriptSkeletonAnchorTypes,
	} {
		if len(table) != len(expected) {
			t.Errorf("%s 骨架种类不齐: %v", name, table)
		}
		for kind := range expected {
			if _, ok := table[kind]; !ok {
				t.Errorf("%s 缺骨架 %q", name, kind)
			}
		}
	}
	// video_units 无逐条角色名单字段，如实声明缺位而不是填假字段名。
	if skeleton, _ := ScriptSkeletonOf("video_units"); skeleton.CharsField != "" {
		t.Errorf("video_units 不应有 chars_field: %q", skeleton.CharsField)
	}
	if skeleton, _ := ScriptSkeletonOf("scenes"); skeleton.IDField != "scene_id" || skeleton.CharsField != "characters_in_scene" {
		t.Errorf("scenes 骨架事实不符: %+v", skeleton)
	}
	if _, ok := ScriptSkeletonOf("frames"); ok {
		t.Error("未知骨架种类应返回 ok=false")
	}
}
