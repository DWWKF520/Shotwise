package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// fakeRelationProjects 是内存版项目读写：读原文件、记录每次写回的关系图。
type fakeRelationProjects struct {
	dir     string
	pf      *domain.ProjectFile
	scripts map[string]map[string]any
	writes  []map[string]any
}

func (f *fakeRelationProjects) Get(string) (*domain.ProjectFile, error) { return f.pf, nil }
func (f *fakeRelationProjects) Dir(string) (string, error)              { return f.dir, nil }

func (f *fakeRelationProjects) Update(_ string, mutate func(*domain.ProjectFile) error) error {
	if err := mutate(f.pf); err != nil {
		return err
	}
	f.writes = append(f.writes, f.pf.CharacterRelations)
	return nil
}

func (f *fakeRelationProjects) ScriptFileNames(string) ([]string, error) {
	out := make([]string, 0, len(f.scripts))
	for name := range f.scripts {
		out = append(out, name)
	}
	return out, nil
}

// fakeRelationGenerator 记录调用参数并返回预置输出。
type fakeRelationGenerator struct {
	text       string
	err        error
	calls      int
	lastPrompt string
	lastSchema map[string]any
	lastMax    int
}

func (f *fakeRelationGenerator) Generate(_ context.Context, prompt string, schema map[string]any, max int) (string, error) {
	f.calls++
	f.lastPrompt, f.lastSchema, f.lastMax = prompt, schema, max
	return f.text, f.err
}

// newRelationFixture 建一个含两个角色、一条人工边与一集剧本的项目。
func newRelationFixture(t *testing.T, characters map[string]any, relations map[string]any) (*fakeRelationProjects, string) {
	t.Helper()
	dir := t.TempDir()
	raw := map[string]any{
		"content_mode": "drama",
		"overview":     map[string]any{"synopsis": "少年与少女的故事"},
		"characters":   characters,
	}
	if relations != nil {
		raw["character_relations"] = relations
	}
	if err := fs.WriteJSONAtomic(filepath.Join(dir, "project.json"), raw); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := map[string]any{
		"episode": float64(1),
		"scenes": []any{map[string]any{
			"scene_id":            "s1",
			"characters_in_scene": []any{"阿明", "小红"},
			"scene_description":   "两人在桥上相遇",
			"utterances":          []any{map[string]any{"speaker": "阿明", "text": "你好"}},
		}},
	}
	if err := fs.WriteJSONAtomic(filepath.Join(dir, "scripts", "episode_1.json"), script); err != nil {
		t.Fatal(err)
	}
	pf, err := domain.NewProjectFile("测试", domain.ContentDrama, domain.GenerationStoryboard), error(nil)
	if err != nil {
		t.Fatal(err)
	}
	pf.CharacterRelations = relations
	return &fakeRelationProjects{
		dir:     dir,
		pf:      pf,
		scripts: map[string]map[string]any{"episode_1.json": script},
	}, dir
}

func twoCharacters() map[string]any {
	return map[string]any{
		"阿明": map[string]any{"description": "少年，黑发"},
		"小红": map[string]any{"description": "少女，红裙"},
	}
}

func TestAnalyzeCharacterRelationsSkipsModelForSingleCharacter(t *testing.T) {
	projects, _ := newRelationFixture(t, map[string]any{"阿明": map[string]any{"description": "少年"}}, nil)
	gen := &fakeRelationGenerator{text: `{"relations":[]}`}
	tl, err := newAnalyzeCharacterRelationsTool("proj", func(context.Context, *domain.ProjectFile) (RelationTextGenerator, error) {
		return gen, nil
	}, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if gen.calls != 0 {
		t.Fatalf("角色少于两个不应调用模型，calls=%d", gen.calls)
	}
	if !strings.Contains(out, "当前关系数: 0") {
		t.Fatalf("out = %s", out)
	}
	if len(projects.writes) != 2 {
		t.Fatalf("应写两次（analyzing + ready），writes=%d", len(projects.writes))
	}
}

func TestAnalyzeCharacterRelationsMergesModelOutputAndPreservesManualEdges(t *testing.T) {
	existing := map[string]any{
		"revision": float64(1),
		"edges": []any{map[string]any{
			"id": "m1", "source": "阿明", "target": "小红", "type": "family",
			"label": "兄妹", "directed": false, "origin": "manual",
		}},
	}
	characters := twoCharacters()
	characters["老王"] = map[string]any{"description": "茶馆老板"}
	projects, dir := newRelationFixture(t, characters, existing)
	// 第一条边落在已有人工边的同一对上（整对受保护，应被跳过），第二条是新增对。
	gen := &fakeRelationGenerator{text: "```json\n" + `{"relations":[
		{"source":"阿明","target":"小红","type":"ally","label":"队友","directed":false},
		{"source":"小红","target":"老王","type":"ally","label":"忘年交","directed":false,
		 "description":"常去茶馆","confidence":0.9,
		 "evidence":[{"episode":1,"script_file":"episode_1.json","scene_id":"s1","excerpt":"桥上相遇"}]}
	]}` + "\n```"}
	tl, err := newAnalyzeCharacterRelationsTool("proj", func(context.Context, *domain.ProjectFile) (RelationTextGenerator, error) {
		return gen, nil
	}, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "当前关系数: 2") {
		t.Fatalf("out = %s", out)
	}

	// 提示词与结构化请求参数。
	if !strings.Contains(gen.lastPrompt, "<project_context>") || !strings.Contains(gen.lastPrompt, "阿明: 少年，黑发") {
		t.Fatalf("prompt 缺少角色/内容定界: %s", gen.lastPrompt)
	}
	if !strings.Contains(gen.lastPrompt, "episode=1; file=episode_1.json; id='s1'; characters=阿明, 小红; 两人在桥上相遇 | 阿明: 你好") {
		t.Fatalf("prompt 证据行不符: %s", gen.lastPrompt)
	}
	if gen.lastMax != 8000 || gen.lastSchema == nil {
		t.Fatalf("max=%d schema=%v", gen.lastMax, gen.lastSchema)
	}

	written := projects.writes[len(projects.writes)-1]
	edges, _ := written["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("edges = %+v", edges)
	}
	if written["analysis_status"] != domain.RelationStatusReady {
		t.Fatalf("status = %v", written["analysis_status"])
	}
	if written["source_fingerprint"] == nil || written["source_fingerprint"] == "" {
		t.Fatalf("fingerprint = %v", written["source_fingerprint"])
	}
	// 人工边原样保留，AI 边带新 id 与 origin=ai。
	first := edges[0].(map[string]any)
	second := edges[1].(map[string]any)
	if first["id"] != "m1" || first["origin"] != domain.RelationOriginManual {
		t.Fatalf("人工边被改写: %+v", first)
	}
	if second["origin"] != domain.RelationOriginAI || second["type"] != "ally" {
		t.Fatalf("AI 边不符: %+v", second)
	}
	if second["id"] == "" || second["id"] == "m1" {
		t.Fatalf("AI 边应有自己的 id: %+v", second)
	}

	// 剧本与项目原文件不被改写。
	raw, err := os.ReadFile(filepath.Join(dir, "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil || probe["character_relations"] == nil {
		t.Fatalf("原文件应保持未写入（写入走 ProjectStore.Update）: %v", err)
	}
}

func TestAnalyzeCharacterRelationsMarksFailureAndKeepsOldGraph(t *testing.T) {
	existing := map[string]any{
		"edges": []any{map[string]any{
			"id": "keep", "source": "阿明", "target": "小红", "type": "friend",
			"label": "旧边", "directed": false, "origin": "ai",
		}},
	}

	cases := map[string]struct {
		text string
		err  error
		want string
	}{
		"非 JSON 输出": {text: "这不是 JSON", want: "did not return JSON"},
		"未知关系类型":    {text: `{"relations":[{"source":"阿明","target":"小红","type":"enemy_of"}]}`, want: "unknown relationship type"},
		"顶层未知字段":    {text: `{"relations":[],"extra":1}`, want: "顶层未知字段"},
		"模型调用失败":    {err: errors.New("provider timeout"), want: "provider timeout"},
	}
	for name, c := range cases {
		projects, _ := newRelationFixture(t, twoCharacters(), existing)
		gen := &fakeRelationGenerator{text: c.text, err: c.err}
		tl, err := newAnalyzeCharacterRelationsTool("proj", func(context.Context, *domain.ProjectFile) (RelationTextGenerator, error) {
			return gen, nil
		}, projects)
		if err != nil {
			t.Fatal(err)
		}
		out, err := tl.InvokableRun(context.Background(), `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"is_error":true`) || !strings.Contains(out, c.want) {
			t.Errorf("%s: out = %s", name, out)
		}
		written := projects.writes[len(projects.writes)-1]
		if written["analysis_status"] != domain.RelationStatusFailed {
			t.Errorf("%s: 失败需落盘 failed，得到 %v", name, written["analysis_status"])
		}
		edges, _ := written["edges"].([]any)
		if len(edges) != 1 || edges[0].(map[string]any)["id"] != "keep" {
			t.Errorf("%s: 旧图应保留，edges=%+v", name, edges)
		}
	}
}

func TestAnalyzeCharacterRelationsExpiredListingAndConfigGates(t *testing.T) {
	projects, _ := newRelationFixture(t, twoCharacters(), nil)

	tl, err := newAnalyzeCharacterRelationsTool("proj", nil, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := tl.InvokableRun(context.Background(), `{}`)
	if !strings.Contains(out, "text generation not configured") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("未注入生成器应 deny: %s", out)
	}

	glitched := &fakeRelationProjects{dir: filepath.Join(projects.dir, "已删除"), pf: projects.pf}
	tl, _ = newAnalyzeCharacterRelationsTool("proj", func(context.Context, *domain.ProjectFile) (RelationTextGenerator, error) {
		return &fakeRelationGenerator{text: `{"relations":[]}`}, nil
	}, glitched)
	// 项目目录不存在时按「项目未找到」回灌信封，而不是 panic。
	out, err = tl.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"is_error":true`) || !strings.Contains(out, "project.json") {
		t.Fatalf("读取失败应回灌 is_error: %s", out)
	}
}

func TestRelationAnalysisSchemaIsInlineAndWithinGeminiSubset(t *testing.T) {
	schema := relationAnalysisSchema()
	if err := hasRef(schema); err != nil {
		t.Fatalf("schema 不得含 $ref: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	relations, _ := props["relations"].(map[string]any)
	items, _ := relations["items"].(map[string]any)
	inner, _ := items["properties"].(map[string]any)
	enum, _ := inner["type"].(map[string]any)["enum"].([]any)
	if len(enum) != len(domain.RelationshipTypes) {
		t.Fatalf("type 枚举应与 domain.RelationshipTypes 对齐: %v", enum)
	}
	if _, present := items["additionalProperties"]; present {
		t.Fatal("wire schema 不声明 additionalProperties（Go 侧解析已做同等约束）")
	}
}

func hasRef(node any) error {
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v["$ref"]; ok {
			return errors.New("found $ref")
		}
		for _, item := range v {
			if err := hasRef(item); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := hasRef(item); err != nil {
				return err
			}
		}
	}
	return nil
}
