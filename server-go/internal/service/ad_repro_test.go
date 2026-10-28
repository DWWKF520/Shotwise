package service

import (
	"testing"

	"github.com/shotwise/shotwise/internal/domain/project"
)

// ad 项目创建后即满足落盘校验：target_duration 有默认值、brief 空串、恒为第 1 集单条；
// 直接 Save 不得被「不更坏」校验拒绝。
func TestCreateAdProjectShape(t *testing.T) {
	s := NewProjectStore(t.TempDir())
	pf, err := s.Create("proj-ad", "测试", project.ContentAd, project.GenerationStoryboard, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if pf.TargetDuration == nil || *pf.TargetDuration != project.ADDefaultTargetDuration {
		t.Errorf("target_duration = %v, want 默认 %d", pf.TargetDuration, project.ADDefaultTargetDuration)
	}
	if pf.Brief != "" {
		t.Errorf("brief = %q, want 空串", pf.Brief)
	}
	if len(pf.Episodes) != 1 || pf.Episodes[0].Episode != 1 || pf.Episodes[0].ScriptFile != "scripts/episode_1.json" {
		t.Errorf("episodes = %#v, want 第 1 集单条", pf.Episodes)
	}
	if err := s.Save("proj-ad", pf); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 非 ad 项目不带 ad 专属字段。
	pf2, err := s.Create("proj-narration", "测试二", project.ContentNarration, project.GenerationStoryboard, "")
	if err != nil {
		t.Fatalf("create narration: %v", err)
	}
	if pf2.TargetDuration != nil || len(pf2.Episodes) != 0 {
		t.Errorf("非 ad 项目不应携带 ad 字段: %#v", pf2)
	}
}
