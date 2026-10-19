// 角色变体的域级模型与归一。对应 lib/character_variants.py。
//
// 变体存挂在 character 条目的 variants 子字典上（key = slug），每条记录带稳定 id；
// 提及语法「角色/衍生slug」按 slug 查找，但删除/更新允许按 id 或 slug 定位。
package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// VariantStatuses 是变体状态的合法集合。
var VariantStatuses = map[string]bool{"draft": true, "ready": true, "missing": true, "archived": true}

// NewVariantID 生成与 Python str(uuid4()) 同形（8-4-4-4-12 连字符）的变体号，
// 保证与存量 id 的引用关系兼容。
func NewVariantID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用是环境级故障：不 panic，用纳秒时间戳兜底保证 id 唯一性语义。
		return fmt.Sprintf("%08x-%04x-4%03x", time.Now().UnixNano()&0xFFFFFFFF, 0, time.Now().UnixNano()%0x1000)
	}
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// NormalizeVariant 归一一条变体记录；非法的遗留数据返回 nil（按缺失容忍，不报错）。
//
// 与 Python normalize_variant 同口径：slug 必须合法、status 越界回落 draft、
// metadata 非字典回落空、display_name 缺省取 slug。
func NormalizeVariant(raw map[string]any, characterID, slug string) map[string]any {
	if raw == nil {
		return nil
	}
	rawSlug, _ := raw["slug"].(string)
	if rawSlug == "" && slug != "" {
		rawSlug = slug
	}
	if rawSlug == "" {
		return nil
	}
	cleanSlug, err := ValidateAssetName(rawSlug)
	if err != nil {
		return nil
	}
	if slug != "" {
		if want, wantErr := ValidateAssetName(slug); wantErr != nil || want != cleanSlug {
			return nil
		}
	}
	variantID, _ := raw["id"].(string)
	if variantID == "" {
		variantID = NewVariantID()
	}
	status, _ := raw["status"].(string)
	if !VariantStatuses[status] {
		status = "draft"
	}
	metadata, _ := raw["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	imagePath, _ := raw["image_path"].(string)
	displayName, _ := raw["display_name"].(string)
	if displayName == "" {
		displayName = cleanSlug
	}
	description, _ := raw["description"].(string)
	result := map[string]any{
		"id":           variantID,
		"character_id": characterID,
		"slug":         cleanSlug,
		"display_name": displayName,
		"description":  description,
		"image_path":   imagePath,
		"status":       status,
		"metadata":     metadata,
	}
	if assetID, ok := raw["image_asset_id"].(string); ok && assetID != "" {
		result["image_asset_id"] = assetID
	}
	return result
}

// IterCharacterVariants 读取条目的 variants 子字典，容忍脏数据（非法条目跳过）。
// characterID 会补进每条记录（条目自身不携带名字）。
func IterCharacterVariants(character *AssetEntry, characterID string) []map[string]any {
	if character == nil {
		return nil
	}
	rawVariants, ok := character.Fields()["variants"].(map[string]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(rawVariants))
	for slugKey, raw := range rawVariants {
		item, _ := raw.(map[string]any)
		if normalized := NormalizeVariant(item, characterID, slugKey); normalized != nil {
			out = append(out, normalized)
		}
	}
	return out
}

// FindCharacterVariant 先按稳定 id、再按 slug 定位变体。
func FindCharacterVariant(character *AssetEntry, identifier string) map[string]any {
	target := NormalizeAssetName(identifier)
	for _, variant := range IterCharacterVariants(character, "") {
		id, _ := variant["id"].(string)
		slug, _ := variant["slug"].(string)
		if id == identifier || NormalizeAssetName(slug) == target {
			return variant
		}
	}
	return nil
}
