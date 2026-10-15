package domain

import (
	"strings"
	"testing"
)

func TestDesignPromptsKeepLayoutGuardAndNegativeTail(t *testing.T) {
	scene := BuildScenePrompt("客厅", "旧式木屋", "水墨", "留白")
	for _, want := range []string{"风格：水墨", "描述：留白", "标志性场景「客厅」", sceneLayout, sceneGuard, negativeTailScene} {
		if !strings.Contains(scene, want) {
			t.Errorf("scene prompt missing %q", want)
		}
	}
	if !strings.HasPrefix(scene, "风格：水墨\n描述：留白\n\n") {
		t.Errorf("style block must lead the prompt: %q", scene[:30])
	}

	prop := BuildPropPrompt("玉佩", "青玉", "", "")
	if strings.Contains(prop, "风格") {
		t.Error("empty style must not add a style block")
	}
	if !strings.HasSuffix(prop, negativeTailProp) {
		t.Error("prop prompt must end with the negative tail")
	}

	product := BuildProductPrompt("奶茶", "包装盒")
	if strings.Contains(product, "风格") {
		t.Error("product sheet must not inject style (matches Python)")
	}
	if !strings.Contains(product, productGuard) {
		t.Error("product prompt must carry the fidelity guard")
	}
}
