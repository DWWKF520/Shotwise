package tools

import "sort"

// Contains 判断字符串是否在列表中。
func Contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SortedKeys 返回 map 键的字典序切片（报错与汇总文本稳定可复现）。
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
