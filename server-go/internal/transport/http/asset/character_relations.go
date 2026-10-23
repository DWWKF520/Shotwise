// 角色关系图路由（对应 server/routers/character_relations.py，3 端点）。
//
// 读：normalize + payload；写：base_revision OCC（乐观并发，冲突 409）+ 手动边合并 +
// 可选节点坐标覆盖；分析：复用 agent 包的 analyzeCharacterRelations（与 SDK 工具同一条
// 代码路径，规则不分叉）。
package asset

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"

	"github.com/shotwise/shotwise/internal/agent/tools/assets"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain/character"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service"
)

// CharacterRelationsDeps 是角色关系路由的依赖。
type CharacterRelationsDeps struct {
	Store *service.ProjectStore
	// RelationFactory 按项目装配文本生成器（与 agent 工具同源）；nil 时分析端点报 503 语义。
	RelationFactory assets.RelationGeneratorFactory
}

// relationsFromProject 读项目并归一关系图（characters 必须是对象）。
func relationsFromProject(pf *project.ProjectFile) (map[string]any, error) {
	if pf == nil {
		return nil, &service.ValueError{Msg: "project characters must be an object"}
	}
	var existing map[string]any
	if raw, err := json.Marshal(pf.CharacterRelations); err == nil && pf.CharacterRelations != nil {
		_ = json.Unmarshal(raw, &existing)
	}
	var characters project.AssetBucket
	if bucket, ok := pf.BucketRef(project.AssetCharacter); ok && bucket != nil {
		characters = *bucket
	}
	data, err := character.NormalizeCharacterRelations(existing, characters)
	if err != nil {
		return nil, err
	}
	return character.RelationsPayload(data), nil
}

// getCharacterRelations 读取关系图。
func getCharacterRelations(d CharacterRelationsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		pf, err := d.Store.Get(c.Param("name"))
		if err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found",
				map[string]any{"name": c.Param("name")}))
			return
		}
		payload, err := relationsFromProject(pf)
		if err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "script_validation_failed",
				map[string]any{"details": err.Error()}))
			return
		}
		c.JSON(http.StatusOK, payload)
	}
}

// saveCharacterRelations 保存画布快照（base_revision 乐观并发）。
func saveCharacterRelations(d CharacterRelationsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			BaseRevision  *int                       `json:"base_revision"`
			Edges         []json.RawMessage          `json:"edges"`
			NodePositions map[string]json.RawMessage `json:"node_positions"`
		}
		if !httpkit.DecodeJSONBody(c, &body) {
			return
		}
		if body.BaseRevision == nil || *body.BaseRevision < 0 {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "base_revision"},
				Msg: "Input should be greater than or equal to 0", Type: "greater_than_equal",
			}}})
			return
		}
		edges := make([]character.RelationEdge, 0, len(body.Edges))
		for _, raw := range body.Edges {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "script_validation_failed",
					map[string]any{"details": err.Error()}))
				return
			}
			edge, err := character.ParseRelationEdge(obj)
			if err != nil {
				apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "script_validation_failed",
					map[string]any{"details": err.Error()}))
				return
			}
			edges = append(edges, edge)
		}
		baseRevision := *body.BaseRevision
		saved := map[string]any{}
		err := d.Store.Update(c.Param("name"), func(pf *project.ProjectFile) error {
			current, err := relationsFromProject(pf)
			if err != nil {
				return err
			}
			// cloneValue 深拷贝可能保留 int 而非 float64：必须数值化比较，不能用类型断言
			// （断言失败会静默变 0，让过期基线误判为匹配、静默覆盖别人的写入）。
			revision, revisionOK := coerceRevision(current["revision"])
			if !revisionOK || revision != baseRevision {
				// 后写方拿 409 去刷新合并，先写方的内容不被静默覆盖。
				return errRelationsRevisionConflict
			}
			var characters project.AssetBucket
			if bucket, ok := pf.BucketRef(project.AssetCharacter); ok && bucket != nil {
				characters = *bucket
			}
			data, err := character.ApplyManualRelationships(pf.CharacterRelations, characters, edges)
			if err != nil {
				return err
			}
			if body.NodePositions != nil {
				positions := map[string]json.RawMessage{}
				for name, raw := range body.NodePositions {
					positions[name] = raw
				}
				rawPositions, marshalErr := json.Marshal(map[string]any{"node_positions": body.NodePositions})
				if marshalErr != nil {
					return marshalErr
				}
				var wrapper map[string]any
				if unmarshalErr := json.Unmarshal(rawPositions, &wrapper); unmarshalErr != nil {
					return unmarshalErr
				}
				normalized, normalizeErr := character.NormalizeCharacterRelations(wrapper, characters)
				if normalizeErr != nil {
					return normalizeErr
				}
				data.NodePositions = normalized.NodePositions
			}
			payload := character.RelationsPayload(data)
			saved = payload
			if pf.CharacterRelations == nil {
				pf.CharacterRelations = map[string]any{}
			}
			for key, value := range payload {
				pf.CharacterRelations[key] = value
			}
			return nil
		})
		if err != nil {
			abortRelationsErr(c, err)
			return
		}
		c.JSON(http.StatusOK, saved)
	}
}

// analyzeCharacterRelationsEndpoint 触发一次 AI 关系分析。
func analyzeCharacterRelationsEndpoint(d CharacterRelationsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpkit.RequireExistingProject(c, d.Store) {
			return
		}
		if d.RelationFactory == nil {
			apperr.Abort(c, apperr.NewText(http.StatusServiceUnavailable,
				"character_relations_analysis_failed", nil))
			return
		}
		project := c.Param("name")
		projects := relationProjectsAdapter{store: d.Store}
		if _, err := assets.AnalyzeCharacterRelations(
			c.Request.Context(), project, d.RelationFactory, projects); err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity,
				"character_relations_analysis_invalid", map[string]any{"message": err.Error()}))
			return
		}
		pf, err := d.Store.Get(project)
		if err != nil {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "project_not_found",
				map[string]any{"name": project}))
			return
		}
		payload, payloadErr := relationsFromProject(pf)
		if payloadErr != nil {
			apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "script_validation_failed",
				map[string]any{"details": payloadErr.Error()}))
			return
		}
		c.JSON(http.StatusOK, payload)
	}
}

// relationProjectsAdapter 把 ProjectStore 适配成 assets.RelationProjects。
type relationProjectsAdapter struct{ store *service.ProjectStore }

func (a relationProjectsAdapter) Get(name string) (*project.ProjectFile, error) {
	return a.store.Get(name)
}
func (a relationProjectsAdapter) Dir(name string) (string, error) { return a.store.Dir(name) }
func (a relationProjectsAdapter) Update(name string, mutate func(*project.ProjectFile) error) error {
	return a.store.Update(name, mutate)
}
func (a relationProjectsAdapter) ScriptFileNames(name string) ([]string, error) {
	return a.store.ScriptFileNames(name)
}

// errRelationsRevisionConflict 表示画布基于过期修订号保存。
var errRelationsRevisionConflict = errors.New("character_relations_revision_conflict")

// coerceRevision 把 JSON / Go 两态的整数归一为 int。
func coerceRevision(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if float64(int(v)) == v {
			return int(v), true
		}
	}
	return 0, false
}

// RegisterCharacterRelations 注册角色关系路由。
func RegisterCharacterRelations(group *gin.RouterGroup, d CharacterRelationsDeps) {
	base := "/projects/:name/character-relations"
	group.GET(base, getCharacterRelations(d))
	group.PUT(base, saveCharacterRelations(d))
	group.POST(base+"/analyze", analyzeCharacterRelationsEndpoint(d))
}

// abortRelationsErr 映射关系域错误。
func abortRelationsErr(c *gin.Context, err error) {
	var valueErr *service.ValueError
	switch {
	case errors.Is(err, errRelationsRevisionConflict):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "character_relations_revision_conflict", nil))
	case errors.As(err, &valueErr):
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "script_validation_failed",
			map[string]any{"details": valueErr.Msg}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
