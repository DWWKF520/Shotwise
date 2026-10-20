﻿package service

// SourceLoader（对应 lib/source_loader）：把上传的小说源文规范化为 UTF-8 .txt，
// 按需备份原始字节到 raw/。
//
// - txt/md：BOM → 严格 UTF-8 → GB18030 兜底（替换率 >5% 判失败）。
//   与 Python 的差异：charset-normalizer 概率检测层未迁——GB18030 已覆盖
//   主要中文场景，其余罕见编码按解码失败拒绝（422），不静默猜错。
// - docx：zip + word/document.xml 的 w:p/w:t 事件流解析（对应 docx2txt 语义）。
// - epub：zip + OPF spine 顺序 + nav toc 标题（对应 ebooklib + bs4 语义）。
// - pdf：github.com/ledongthuc/pdf 逐页抽取 + 扫描件密度检测。
//
// 错误形态收敛为 *SourceError（Kind 区分），transport 按 Kind 映射状态码：
// unsupported_format→400 / too_large→413 / decode_failed→422 / corrupt_file→422 /
// conflict→409（对象 detail）。

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"

	pdfapi "github.com/ledongthuc/pdf"
)

// SourceErrorKind 是源文加载失败的形态分类。
type SourceErrorKind string

const (
	SourceErrUnsupportedFormat SourceErrorKind = "unsupported_format"
	SourceErrDecodeFailed      SourceErrorKind = "decode_failed"
	SourceErrCorruptFile       SourceErrorKind = "corrupt_file"
	SourceErrTooLarge          SourceErrorKind = "too_large"
	SourceErrConflict          SourceErrorKind = "conflict"
)

// SourceError 是源文加载的结构化错误（对应 Python SourceLoader 异常族）。
type SourceError struct {
	Kind       SourceErrorKind
	Ext        string // unsupported_format
	Filename   string
	Reason     string // corrupt_file
	Tried      []string
	SizeBytes  int64 // too_large
	LimitBytes int64 // too_large
	Existing   string // conflict（建议重命名的现状名）
	Suggested  string // conflict
}

func (e *SourceError) Error() string {
	switch e.Kind {
	case SourceErrUnsupportedFormat:
		return fmt.Sprintf("unsupported source format: %s", e.Ext)
	case SourceErrDecodeFailed:
		return fmt.Sprintf("failed to decode %s (tried: %s)", e.Filename, strings.Join(e.Tried, ", "))
	case SourceErrCorruptFile:
		return fmt.Sprintf("%s corrupt or unreadable: %s", e.Filename, e.Reason)
	case SourceErrTooLarge:
		return fmt.Sprintf("%s (%d bytes) exceeds limit (%d bytes)", e.Filename, e.SizeBytes, e.LimitBytes)
	case SourceErrConflict:
		return fmt.Sprintf("file conflict: %s; suggested rename: %s", e.Existing, e.Suggested)
	}
	return string(e.Kind)
}

// SourceNormalizeResult 是一次规范化上传的结果（对应 Python NormalizeResult）。
type SourceNormalizeResult struct {
	NormalizedPath   string
	RawPath          string // 空 = 未备份（纯 UTF-8 无损）
	UsedEncoding     string
	ChapterCount     int
	OriginalFilename string
}

// Source 冲突策略（对应 Python OnConflict）。
const (
	SourceConflictFail    = "fail"
	SourceConflictReplace = "replace"
	SourceConflictRename  = "rename"
)

const (
	// SourceMaxBytes 是源文原始字节上限（50 MB，与 Python DEFAULT_MAX_BYTES 同值）。
	SourceMaxBytes = 50 * 1024 * 1024
	sourceMaxConflictIter = 10000
	// scannedPDFCharsPerPage 是扫描版 PDF 的字符密度阈值（同 Python）。
	scannedPDFCharsPerPage = 50
)

// SourceSupportedExts 返回受支持的源文扩展名集合。
var SourceSupportedExts = map[string]bool{
	".txt": true, ".md": true, ".docx": true, ".epub": true, ".pdf": true,
}

// extractedText 是抽取器的产出（对应 Python ExtractedText）。
type extractedText struct {
	text         string
	usedEncoding string
	chapterCount int
}

// decodeTxt 是 TXT/MD 的解码链：BOM → 严格 UTF-8 → GB18030 宽容兜底。
// GB18030 兜底按 RuneError 占比判定（>5% 报错，≤5% 记 lossy）。
func decodeTxt(raw []byte, filename string) (*extractedText, error) {
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		return &extractedText{text: string(raw[3:]), usedEncoding: "utf-8-sig"}, nil
	}
	if bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) {
		decoded, _ := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder().Bytes(raw[2:])
		return &extractedText{text: string(decoded), usedEncoding: "utf-16-le"}, nil
	}
	if bytes.HasPrefix(raw, []byte{0xFE, 0xFF}) {
		decoded, _ := unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder().Bytes(raw[2:])
		return &extractedText{text: string(decoded), usedEncoding: "utf-16-be"}, nil
	}
	if utf8.Valid(raw) {
		return &extractedText{text: string(raw), usedEncoding: "utf-8"}, nil
	}
	// GB18030 宽容解码：非法序列替换为 RuneError，不报错（对应 errors='replace'）。
	decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(raw)
	if err != nil {
		// GB18030 解码器在宽容模式下不报错；防御性按解码失败处理。
		return nil, &SourceError{Kind: SourceErrDecodeFailed, Filename: filename,
			Tried: []string{"utf-8", "gb18030"}}
	}
	runes := 0
	replacements := 0
	for _, r := range string(decoded) {
		runes++
		if r == utf8.RuneError {
			replacements++
		}
	}
	ratio := 0.0
	if runes > 0 {
		ratio = float64(replacements) / float64(runes)
	}
	if ratio > 0.05 {
		return nil, &SourceError{Kind: SourceErrDecodeFailed, Filename: filename,
			Tried: []string{"utf-8", "gb18030"}}
	}
	if replacements > 0 {
		return &extractedText{text: string(decoded), usedEncoding: "gb18030-lossy"}, nil
	}
	return &extractedText{text: string(decoded), usedEncoding: "gb18030"}, nil
}

// extractDocx 从 docx 抽取纯文本：zip → word/document.xml → w:p 段落流。
// 嵌套段落（表格内的 w:p）按普通段落收录；w:br 换行。
func extractDocx(raw []byte, filename string) (*extractedText, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "DOCX 打开失败: " + err.Error()}
	}
	f := findZipEntry(zr, "word/document.xml")
	if f == nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "DOCX 缺少 word/document.xml"}
	}
	rc, err := f.Open()
	if err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "DOCX 打开失败: " + err.Error()}
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)
	// XML 声明外的杂散字节（BOM/空白）容忍：直接解码。
	var paragraphs []string
	var cur strings.Builder
	depthP := 0
	for {
		tok, terr := dec.Token()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
				Reason: "DOCX 解析失败: " + terr.Error()}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				depthP++
				if depthP == 1 {
					cur.Reset()
				}
			case "t":
				var s string
				if derr := dec.DecodeElement(&s, &t); derr == nil {
					cur.WriteString(s)
				}
			case "br":
				cur.WriteByte('\n')
			}
		case xml.EndElement:
			if t.Name.Local == "p" {
				if depthP == 1 {
					paragraphs = append(paragraphs, cur.String())
				}
				if depthP > 0 {
					depthP--
				}
			}
		}
	}
	text := strings.Join(paragraphs, "\n")
	if strings.TrimSpace(text) == "" {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "DOCX 抽取结果为空"}
	}
	return &extractedText{text: text}, nil
}

// findZipEntry 按名称（大小写不敏感，ZIP 规范要求 ASCII 但现实有漂移）取条目。
func findZipEntry(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, name) {
			return f
		}
	}
	return nil
}

// extractEpub 按 spine 顺序遍历正文章节，注入「# 标题」；标题三级退化
// nav toc → 「第 N 章」。导航文档（properties=nav）排除在正文之外。
func extractEpub(raw []byte, filename string) (*extractedText, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "EPUB 打开失败: " + err.Error()}
	}
	// container.xml → OPF 路径。
	container := findZipEntry(zr, "META-INF/container.xml")
	if container == nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "EPUB 缺少 META-INF/container.xml"}
	}
	opfPath, err := parseContainerOPFPath(container)
	if err != nil || opfPath == "" {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "EPUB container.xml 解析失败"}
	}
	opf := findZipEntry(zr, opfPath)
	if opf == nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "EPUB 缺少 OPF: " + opfPath}
	}
	manifest, spine, err := parseOPF(opf)
	if err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "EPUB OPF 解析失败: " + err.Error()}
	}
	opfDir := path.Dir(opfPath)

	// spine idref → manifest 条目 → zip 路径；跳过 nav。
	type chapter struct {
		zipPath string
	}
	var chapters []chapter
	for _, idref := range spine {
		item, ok := manifest[idref]
		if !ok || item.nav || item.href == "" {
			continue
		}
		zp := item.href
		if opfDir != "" && opfDir != "." {
			zp = opfDir + "/" + item.href
		}
		if findZipEntry(zr, zp) != nil {
			chapters = append(chapters, chapter{zipPath: zp})
		}
	}
	if len(chapters) == 0 {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "EPUB 不含正文章节"}
	}

	// 标题映射：nav 文档的 toc 链接（href → title），退化「第 N 章」。
	titleByHref := map[string]string{}
	if navPath := findNavPath(manifest, opfDir, zr); navPath != "" {
		titleByHref = parseNavToc(zr, navPath)
	}

	var parts []string
	for idx, ch := range chapters {
		title := titleByHref[chapterRelHref(opfDir, ch.zipPath)]
		if title == "" {
			title = fmt.Sprintf("第 %d 章", idx+1)
		}
		entry := findZipEntry(zr, ch.zipPath)
		if entry == nil {
			continue
		}
		rc, rerr := entry.Open()
		if rerr != nil {
			return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
				Reason: "EPUB 章节读取失败: " + rerr.Error()}
		}
		htmlRaw, _ := io.ReadAll(rc)
		rc.Close()
		body := stripHTMLTags(htmlRaw)
		parts = append(parts, fmt.Sprintf("\n\n# %s\n\n%s", title, strings.TrimSpace(body)))
	}
	if len(parts) == 0 {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "EPUB 正文章节读取失败"}
	}
	return &extractedText{text: strings.TrimLeft(strings.Join(parts, ""), "\n"), chapterCount: len(parts)}, nil
}

// opfItem 是 manifest 条目的最小投影。
type opfItem struct {
	href string
	nav  bool // properties 含 "nav"
}

// parseContainerOPFPath 从 container.xml 取 rootfile 全路径。
func parseContainerOPFPath(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var container struct {
		Rootfiles []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := xml.NewDecoder(rc).Decode(&container); err != nil {
		return "", err
	}
	if len(container.Rootfiles) == 0 || container.Rootfiles[0].FullPath == "" {
		return "", errors.New("no rootfile")
	}
	return container.Rootfiles[0].FullPath, nil
}

// parseOPF 解析 OPF 的 manifest 与 spine（spine@toc 指向的 nav 不依赖 guide）。
func parseOPF(f *zip.File) (map[string]opfItem, []string, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, nil, err
	}
	defer rc.Close()
	var pkg struct {
		Manifest struct {
			Items []struct {
				ID         string `xml:"id,attr"`
				Href       string `xml:"href,attr"`
				Properties string `xml:"properties,attr"`
			} `xml:"item"`
		} `xml:"manifest"`
		Spine struct {
			Itemrefs []struct {
				IDRef string `xml:"idref,attr"`
			} `xml:"itemref"`
		} `xml:"spine"`
	}
	if err := xml.NewDecoder(rc).Decode(&pkg); err != nil {
		return nil, nil, err
	}
	manifest := map[string]opfItem{}
	for _, it := range pkg.Manifest.Items {
		manifest[it.ID] = opfItem{href: it.Href, nav: strings.Contains(it.Properties, "nav")}
	}
	spine := make([]string, 0, len(pkg.Spine.Itemrefs))
	for _, ref := range pkg.Spine.Itemrefs {
		spine = append(spine, ref.IDRef)
	}
	return manifest, spine, nil
}

// findNavPath 定位 nav 文档（properties=nav）在 zip 内的路径。
func findNavPath(manifest map[string]opfItem, opfDir string, zr *zip.Reader) string {
	for _, it := range manifest {
		if !it.nav || it.href == "" {
			continue
		}
		zp := it.href
		if opfDir != "" && opfDir != "." {
			zp = opfDir + "/" + it.href
		}
		if findZipEntry(zr, zp) != nil {
			return zp
		}
	}
	return ""
}

// parseNavToc 解析 nav 文档的 toc：a[href] → 文本。
func parseNavToc(zr *zip.Reader, navPath string) map[string]string {
	f := findZipEntry(zr, navPath)
	if f == nil {
		return nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	out := map[string]string{}
	// XHTML 属性值转义以 XML 规范为准，用 XML 解码器提取 (href, 文本) 对。
	dec := xml.NewDecoder(bytes.NewReader(stripHTMLDoctype(raw)))
	var href string
	var text strings.Builder
	inAnchor := false
	for {
		tok, terr := dec.Token()
		if terr != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "a" {
				inAnchor = true
				href = ""
				text.Reset()
				for _, a := range t.Attr {
					if a.Name.Local == "href" {
						href = a.Value
					}
				}
			}
		case xml.CharData:
			if inAnchor {
				text.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "a" && inAnchor {
				inAnchor = false
				if href != "" {
					// href 可能带片段（#frag）与 URL 编码，都归一掉。
					rel := strings.SplitN(href, "#", 2)[0]
					if unescaped, uerr := url.PathUnescape(rel); uerr == nil {
						rel = unescaped
					}
					if title := strings.TrimSpace(text.String()); title != "" {
						out[rel] = title
					}
				}
			}
		}
	}
	return out
}

// stripHTMLDoctype 移除 <!DOCTYPE ...> 声明（XML 解码器对含实体声明的 DTD 敏感）。
func stripHTMLDoctype(raw []byte) []byte {
	s := string(raw)
	if i := strings.Index(s, "<!"); i >= 0 && strings.HasPrefix(strings.ToUpper(s[i:i+9]), "<!DOCTYPE") {
		if j := strings.Index(s[i:], ">"); j >= 0 {
			s = s[:i] + s[i+j+1:]
		}
	}
	return []byte(s)
}

// chapterRelHref 把章节 zip 路径归一到 nav href 的坐标系（去 opfDir 前缀 + URL 解码）。
func chapterRelHref(opfDir, zipPath string) string {
	rel := zipPath
	if opfDir != "" && opfDir != "." && strings.HasPrefix(zipPath, opfDir+"/") {
		rel = zipPath[len(opfDir)+1:]
	}
	if unescaped, err := url.PathUnescape(rel); err == nil {
		rel = unescaped
	}
	return rel
}

// htmlTagPattern 匹配开/闭标签（属性内含 > 的病态构造不在处理范围）。
var htmlTagPattern = regexp.MustCompile(`(?s)<[^>]*>`)

// stripHTMLTags 把 XHTML 转为纯文本：块级标签补换行，其余标签丢弃，实体解码。
func stripHTMLTags(raw []byte) string {
	s := string(raw)
	s = htmlTagPattern.ReplaceAllStringFunc(s, func(tag string) string {
		name := strings.ToLower(strings.TrimSpace(tag[1 : len(tag)-1]))
		name = strings.TrimPrefix(strings.TrimPrefix(name, "/"), "?")
		switch name {
		case "p", "div", "br", "li", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "tr":
			return "\n"
		}
		return ""
	})
	s = htmlEntityPattern.ReplaceAllStringFunc(s, decodeHTMLEntity)
	return strings.TrimSpace(s)
}

var htmlEntityPattern = regexp.MustCompile(`&[a-zA-Z#0-9]+;`)

// decodeHTMLEntity 解码常见 HTML 实体（覆盖 EPUB 正文高频集）。
func decodeHTMLEntity(e string) string {
	switch e {
	case "&amp;":
		return "&"
	case "&lt;":
		return "<"
	case "&gt;":
		return ">"
	case "&quot;":
		return `"`
	case "&apos;", "&#39;":
		return "'"
	case "&nbsp;":
		return " "
	}
	if strings.HasPrefix(e, "&#x") || strings.HasPrefix(e, "&#X") {
		if n, err := strconv.ParseInt(e[3:len(e)-1], 16, 32); err == nil {
			return string(rune(n))
		}
	}
	if strings.HasPrefix(e, "&#") {
		if n, err := strconv.ParseInt(e[2:len(e)-1], 10, 32); err == nil {
			return string(rune(n))
		}
	}
	return e
}

// extractPDF 逐页抽取文本并做扫描件检测（对应 Python PdfOxideExtractor）。
func extractPDF(src string, filename string) (*extractedText, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "PDF 打开失败: " + err.Error()}
	}
	defer f.Close()
	reader, err := pdfapi.NewReader(f, int64(fileSizeOf(f)))
	if err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "PDF 打开失败: " + err.Error()}
	}
	pages := reader.NumPage()
	if pages == 0 {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "PDF 无页面"}
	}
	var pageTexts []string
	for i := 1; i <= pages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		var b strings.Builder
		texts := page.Content().Text
		lastMatrixY := -1.0
		for _, t := range texts {
			// 同一行的多个片段直拼，跨行补换行（Y 递减为换页方向）。
			if lastMatrixY >= 0 && t.Y < lastMatrixY-0.5 {
				b.WriteByte('\n')
			}
			b.WriteString(t.S)
			lastMatrixY = t.Y
		}
		pageTexts = append(pageTexts, b.String())
	}
	full := strings.TrimSpace(strings.Join(pageTexts, "\n\n"))
	if full == "" || len([]rune(full))/pages < scannedPDFCharsPerPage {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
			Reason: "疑似扫描版 PDF，需 OCR，本次不支持"}
	}
	return &extractedText{text: full}, nil
}

func fileSizeOf(f *os.File) int64 {
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	return st.Size()
}

// extractSource 按扩展名分派到对应抽取器。
func extractSource(src, ext, filename string) (*extractedText, error) {
	switch ext {
	case ".txt", ".md":
		raw, err := os.ReadFile(src)
		if err != nil {
			return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
				Reason: "文件读取失败: " + err.Error()}
		}
		return decodeTxt(raw, filename)
	case ".docx":
		raw, err := os.ReadFile(src)
		if err != nil {
			return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
				Reason: "文件读取失败: " + err.Error()}
		}
		return extractDocx(raw, filename)
	case ".epub":
		raw, err := os.ReadFile(src)
		if err != nil {
			return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: filename,
				Reason: "文件读取失败: " + err.Error()}
		}
		return extractEpub(raw, filename)
	case ".pdf":
		return extractPDF(src, filename)
	}
	return nil, &SourceError{Kind: SourceErrUnsupportedFormat, Ext: ext}
}

// DetectSourceConflict 返回 (是否冲突, 建议的 stem)。冲突条件与 Python 同：
// dst/<stem>.txt 或 dst/raw/<原名> 已存在；建议名 stem_1、stem_2… 递增。
func DetectSourceConflict(originalFilename, dstDir string) (bool, string, error) {
	stem := strings.TrimSuffix(filepath.Base(originalFilename), filepath.Ext(originalFilename))
	ext := strings.ToLower(filepath.Ext(originalFilename))
	normalized := filepath.Join(dstDir, stem+".txt")
	raw := filepath.Join(dstDir, "raw", filepath.Base(originalFilename))
	if !sourceFileExists(normalized) && !sourceFileExists(raw) {
		return false, stem, nil
	}
	for idx := 1; idx < sourceMaxConflictIter; idx++ {
		candidateStem := fmt.Sprintf("%s_%d", stem, idx)
		candidateNorm := filepath.Join(dstDir, candidateStem+".txt")
		candidateRaw := filepath.Join(dstDir, "raw", candidateStem+ext)
		if !sourceFileExists(candidateNorm) && !sourceFileExists(candidateRaw) {
			return true, candidateStem, nil
		}
	}
	return true, stem, &SourceError{Kind: SourceErrConflict,
		Existing: stem + ".txt", Suggested: fmt.Sprintf("%s_%d", stem, sourceMaxConflictIter)}
}

func sourceFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// LoadSource 规范化上传文件为 UTF-8 .txt 并按需备份原始字节（对应 Python SourceLoader.load）。
//
// 原子性：normalized 写入后 raw 备份失败则回滚 normalized 再冒泡错误。
// onConflict: fail（409）/ replace（沿用 stem 覆盖，先清同 stem 旧 raw）/ rename。
func LoadSource(src, dstDir, originalFilename, onConflict string, maxBytes int64) (*SourceNormalizeResult, error) {
	if originalFilename == "" {
		originalFilename = filepath.Base(src)
	}
	ext := strings.ToLower(filepath.Ext(originalFilename))
	if !SourceSupportedExts[ext] {
		return nil, &SourceError{Kind: SourceErrUnsupportedFormat, Ext: ext}
	}
	st, err := os.Stat(src)
	if err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: originalFilename,
			Reason: "文件读取失败: " + err.Error()}
	}
	if maxBytes <= 0 {
		maxBytes = SourceMaxBytes
	}
	if st.Size() > maxBytes {
		return nil, &SourceError{Kind: SourceErrTooLarge, Filename: originalFilename,
			SizeBytes: st.Size(), LimitBytes: maxBytes}
	}

	stem := strings.TrimSuffix(filepath.Base(originalFilename), ext)
	hasConflict, suggestedStem, err := DetectSourceConflict(originalFilename, dstDir)
	if err != nil {
		return nil, err
	}
	effectiveFilename := filepath.Base(originalFilename)
	if hasConflict {
		switch onConflict {
		case SourceConflictFail:
			return nil, &SourceError{Kind: SourceErrConflict, Existing: stem + ".txt", Suggested: suggestedStem}
		case SourceConflictRename:
			stem = suggestedStem
			effectiveFilename = suggestedStem + ext
		case SourceConflictReplace:
			purgeStaleRaw(dstDir, stem)
		}
	}

	extracted, err := extractSource(src, ext, originalFilename)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: originalFilename,
			Reason: "目标目录创建失败: " + err.Error()}
	}
	normalizedPath := filepath.Join(dstDir, stem+".txt")
	if err := os.WriteFile(normalizedPath, []byte(extracted.text), 0o644); err != nil {
		return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: originalFilename,
			Reason: "规范化文件写入失败: " + err.Error()}
	}

	// 决策 7：仅当 normalized 与原始字节等价（纯 UTF-8 无 BOM 的 txt/md）时跳过备份。
	rawPath := ""
	if !(ext == ".txt" || ext == ".md") || extracted.usedEncoding != "utf-8" {
		rawDir := filepath.Join(dstDir, "raw")
		if err := os.MkdirAll(rawDir, 0o755); err == nil {
			target := filepath.Join(rawDir, effectiveFilename)
			if copyErr := copyFile(src, target); copyErr == nil {
				rawPath = target
			} else {
				// raw 备份失败：回滚 normalized，避免孤儿文件误导后续冲突检测。
				_ = os.Remove(normalizedPath)
				return nil, &SourceError{Kind: SourceErrCorruptFile, Filename: originalFilename,
					Reason: "原始文件备份失败: " + copyErr.Error()}
			}
		}
	}

	return &SourceNormalizeResult{
		NormalizedPath:   normalizedPath,
		RawPath:          rawPath,
		UsedEncoding:     extracted.usedEncoding,
		ChapterCount:     extracted.chapterCount,
		OriginalFilename: effectiveFilename,
	}, nil
}

// purgeStaleRaw 清掉同 stem 的旧 raw 备份（replace 策略，防陈旧 raw 误暴露）。
func purgeStaleRaw(dstDir, targetStem string) {
	rawDir := filepath.Join(dstDir, "raw")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())) == targetStem {
			_ = os.Remove(filepath.Join(rawDir, e.Name()))
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
