// 阅读单位度量（对应 lib/text_metrics.py）。
//
// 按源文语言裁剪的「阅读单位」计数：zh 按汉字 + CJK 标点/全角符号计，en/vi 按 unicode
// word-boundary 词数计。返回的偏移是「字符（码点）偏移」，与账本坐标系一致——Go 的
// len(string) 是字节数，故本文件的所有偏移都按 rune 计数，切片须经 ledgerSlice。
package service

import (
	"strings"
	"unicode"
)

// isZHUnit 判定单个字符是否计入 zh 阅读单位。
//
// 覆盖 Python 侧正则 [㐀-鿿豈-﫿　-〿＀-￯𠀀-𲎯] 的全部码点区间：
// CJK 基本区 + 扩展 A、CJK 兼容汉字、CJK 符号与标点、全角符号区、SIP 平面 CJK 扩展 B-H。
func isZHUnit(r rune) bool {
	switch {
	case r >= 0x3400 && r <= 0x9FFF,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0x3000 && r <= 0x303F,
		r >= 0xFF00 && r <= 0xFFEF,
		r >= 0x20000 && r <= 0x323AF:
		return true
	}
	return false
}

// isLatinWordRune 复刻 Python re.UNICODE 下 \w 的口径：字母 / 数字 / 下划线。
//
// 刻意不用 Go 的 \w（ASCII-only），否则带变音符号的越南语词会被拆成多个 token。
func isLatinWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// usesLatinWords 判定该语言是否走词计数口径（en / vi）。
func usesLatinWords(language string) bool {
	code := strings.ToLower(strings.TrimSpace(language))
	return code == "en" || code == "vi"
}

// CountReadingUnits 按源文语言数阅读单位；未知 / 空语言按 zh 路径处理（兼容缺 source_language 的老项目）。
func CountReadingUnits(text, language string) int {
	if text == "" {
		return 0
	}
	count := 0
	if usesLatinWords(language) {
		inWord := false
		for _, r := range text {
			if isLatinWordRune(r) {
				if !inWord {
					count++
					inWord = true
				}
				continue
			}
			inWord = false
		}
		return count
	}
	for _, r := range text {
		if isZHUnit(r) {
			count++
		}
	}
	return count
}

// ReadingUnitNoun 返回该语言阅读单位的中文量词：词（en / vi）/ 字（zh 及未知语言）。
func ReadingUnitNoun(language string) string {
	if usesLatinWords(language) {
		return "词"
	}
	return "字"
}

// FindReadingUnitOffset 返回第 targetUnits 个阅读单位末尾的字符偏移（含尾）。
//
// targetUnits <= 0 或 text 为空返回 0；文本中的阅读单位不足时返回字符总长。
func FindReadingUnitOffset(text string, targetUnits int, language string) int {
	if targetUnits <= 0 || text == "" {
		return 0
	}
	if usesLatinWords(language) {
		count := 0
		index := 0
		prevWord := false
		for _, r := range text {
			index++
			if isLatinWordRune(r) {
				if !prevWord {
					count++
				}
				prevWord = true
				continue
			}
			// 词在 index-1 处结束（index 已计入当前非词字符）。
			if prevWord {
				if count >= targetUnits {
					return index - 1
				}
				prevWord = false
			}
		}
		if prevWord && count >= targetUnits {
			return index
		}
		return index
	}
	count := 0
	index := 0
	for _, r := range text {
		index++
		if !isZHUnit(r) {
			continue
		}
		count++
		if count >= targetUnits {
			return index
		}
	}
	return index
}
