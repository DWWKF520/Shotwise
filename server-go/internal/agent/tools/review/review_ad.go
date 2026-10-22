package review

import (
	"github.com/shotwise/shotwise/internal/domain"
)

// ad 参考单元陈旧判定的薄封装：纯逻辑（镜头索引、参考集、来源签名、stale）收口在
// domain/reference_units.go，本文件只保留审阅工具沿用的包内名。
// 签名以 Python 落盘的 source_signature 为比较基准，规范化 JSON 必须与 Python
// json.dumps(sort_keys=True, ensure_ascii=False, separators=(",", ":")) 逐字节一致。

// adShotsByID 按 shot_id 索引 script.shots；非 dict 与缺 shot_id 的脏条目跳过。
func adShotsByID(script map[string]any) map[string]map[string]any {
	return domain.AdShotsByID(script)
}

// adSourceSignature 计算 unit 的编排 + 参考集签名（与 Python _source_signature 同口径）。
func adSourceSignature(byID map[string]map[string]any, unit map[string]any) string {
	return domain.AdSourceSignature(byID, unit)
}

// adUnitStale 判定产物签名与当前编排签名是否偏离（同 Python _is_stale）：
// 无成片或无签名的存量产物视为非 stale。
func adUnitStale(byID map[string]map[string]any, unit map[string]any) bool {
	return domain.AdUnitStaleByID(byID, unit)
}
