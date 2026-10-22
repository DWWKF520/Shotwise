package project

import "github.com/shotwise/shotwise/internal/agent/tools"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"

	"github.com/shotwise/shotwise/internal/infra/fs"
)

// 项目文本只读工具（对应 Python sdk_tools/file_read.py 与 services/project_files.py）。
// 文稿候选按 source/ → drafts/ → scripts/ 优先级列出；source/raw/ 上传备份与隐藏文件不计入。

const (
	textReadDefaultLines = 200
	textReadMaxLines     = 400
	textReadMaxChars     = 120_000
)

var (
	textCategories = []string{"source", "drafts", "scripts"}
	textSuffixes   = map[string]bool{".txt": true, ".md": true, ".json": true}
)

// projectTextFile 是清单中的一条文本文件记录（路径相对项目根，使用 / 分隔）。
type projectTextFile struct {
	Path     string
	Category string
	Size     int64
	MtimeNs  int64
}

// enumerateProjectTextFiles 枚举项目根下可读文本文件，按类别内路径排序。
func enumerateProjectTextFiles(root string) ([]projectTextFile, error) {
	var result []projectTextFile
	for _, cat := range textCategories {
		base := filepath.Join(root, cat)
		if info, err := os.Stat(base); err != nil || !info.IsDir() {
			continue
		}
		var files []projectTextFile
		err := filepath.WalkDir(base, func(p string, d iofs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if p != base && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return iofs.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if cat == "source" && strings.EqualFold(rel, "raw") {
					return iofs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !textSuffixes[strings.ToLower(filepath.Ext(p))] {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			files = append(files, projectTextFile{
				Path:     cat + "/" + filepath.ToSlash(rel),
				Category: cat,
				Size:     info.Size(),
				MtimeNs:  info.ModTime().UnixNano(),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		result = append(result, files...)
	}

	if info, err := os.Stat(filepath.Join(root, "project.json")); err == nil && info.Mode().IsRegular() {
		result = append(result, projectTextFile{
			Path:     "project.json",
			Category: "metadata",
			Size:     info.Size(),
			MtimeNs:  info.ModTime().UnixNano(),
		})
	}
	return result, nil
}

// projectTextSignature 对清单的路径、大小与修改时间求摘要，文件增删改后签名必变。
func projectTextSignature(files []projectTextFile) string {
	lines := make([]string, 0, len(files))
	for _, f := range files {
		lines = append(lines, fmt.Sprintf("%s\x00%d\x00%d", f.Path, f.Size, f.MtimeNs))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func renderTextFileList(project string, files []projectTextFile) string {
	byCat := map[string][]projectTextFile{}
	for _, f := range files {
		byCat[f.Category] = append(byCat[f.Category], f)
	}
	primary := ""
	for _, cat := range textCategories {
		if len(byCat[cat]) > 0 {
			primary = cat
			break
		}
	}

	lines := []string{"当前项目: " + project, "文稿目录优先级：source/ → drafts/ → scripts/"}
	writeList := func(items []projectTextFile) {
		for _, f := range items {
			lines = append(lines, fmt.Sprintf("- %s (%d bytes)", f.Path, f.Size))
		}
	}
	if primary == "" {
		lines = append(lines, "当前首选文稿候选：无（这不代表页面没有其他类型文件）")
	} else {
		lines = append(lines, fmt.Sprintf("当前首选文稿候选（%s/）：", primary))
		writeList(byCat[primary])
		for _, cat := range textCategories {
			if cat == primary || len(byCat[cat]) == 0 {
				continue
			}
			lines = append(lines, fmt.Sprintf("备用项目文本（%s/，仅在首选目录没有合适文稿时使用）：", cat))
			writeList(byCat[cat])
		}
	}
	if meta := byCat["metadata"]; len(meta) > 0 {
		lines = append(lines, "项目元数据（不计入文稿候选，仅按需读取）：")
		writeList(meta)
	}
	return strings.Join(lines, "\n")
}

type listProjectTextFilesIn struct{}

// NewListProjectTextFilesTool 列出项目文本清单，并刷新该项目的读取门禁签名。
func NewListProjectTextFilesTool(project, root string, gate *tools.TextFileGate) (tool.InvokableTool, error) {
	return toolutils.InferTool("list_project_text_files",
		"列出当前绑定项目的文稿、草稿和结构化剧本文本文件，不读取全文。文稿任务开始前必须调用；默认检查 source/，且不包含 source/raw/ 上传备份。",
		func(ctx context.Context, _ listProjectTextFilesIn) (string, error) {
			files, err := enumerateProjectTextFiles(root)
			if err != nil {
				return tools.DenyEnvelope("list failed: " + err.Error()), nil
			}
			gate.Mark(project, projectTextSignature(files))
			return tools.OKEnvelope(renderTextFileList(project, files)), nil
		})
}

type readProjectTextIn struct {
	Path      string `json:"path" jsonschema:"description=项目根目录下的相对路径；仅限 project.json、source/、scripts/、drafts/ 下的 .txt/.md/.json"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"description=起始行号（从 1 开始），省略为 1"`
	MaxLines  int    `json:"max_lines,omitempty" jsonschema:"description=本次读取的行数，1-400，省略为 200"`
}

// NewReadProjectTextTool 分页读取项目文本；须先调用 list_project_text_files 且清单未过期。
func NewReadProjectTextTool(project, root string, gate *tools.TextFileGate) (tool.InvokableTool, error) {
	return toolutils.InferTool("read_project_text",
		"分页读取当前项目内的文本文件。只能读取 project.json、source/、scripts/、drafts/ 下的 .txt/.md/.json 文件；禁止路径穿越、写文件和执行命令。使用 start_line/max_lines 分页。",
		func(ctx context.Context, in readProjectTextIn) (string, error) {
			files, err := enumerateProjectTextFiles(root)
			if err != nil {
				return tools.DenyEnvelope("list failed: " + err.Error()), nil
			}
			if !gate.Fresh(project, projectTextSignature(files)) {
				return tools.DenyEnvelope("请先调用 list_project_text_files 获取当前项目文件清单；文件发生变化后必须重新列出"), nil
			}
			if strings.TrimSpace(in.Path) == "" {
				return tools.DenyEnvelope("path 必须是非空字符串"), nil
			}
			if in.StartLine < 0 {
				return tools.DenyEnvelope("start_line 必须是正整数"), nil
			}
			startLine := in.StartLine
			if startLine == 0 {
				startLine = 1
			}
			maxLines := in.MaxLines
			switch {
			case maxLines < 0:
				return tools.DenyEnvelope("max_lines 必须是正整数"), nil
			case maxLines == 0:
				maxLines = textReadDefaultLines
			case maxLines > textReadMaxLines:
				return tools.DenyEnvelope(fmt.Sprintf("max_lines 不能超过 %d", textReadMaxLines)), nil
			}

			target, err := fs.SafeResolve(root, in.Path)
			if err != nil {
				return tools.DenyEnvelope("路径不可读取: " + err.Error()), nil
			}
			realRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				return tools.DenyEnvelope("project root unavailable: " + err.Error()), nil
			}
			rel, err := filepath.Rel(realRoot, target)
			if err != nil {
				return tools.DenyEnvelope(err.Error()), nil
			}
			rel = filepath.ToSlash(rel)
			lowerRel := strings.ToLower(rel)
			if lowerRel == "source/raw" || strings.HasPrefix(lowerRel, "source/raw/") {
				return tools.DenyEnvelope("source/raw/ 是上传备份目录，不作为文稿读取"), nil
			}
			if rel != "project.json" && !isUnderTextCategory(rel) {
				return tools.DenyEnvelope("只能读取 project.json、source/、scripts/ 或 drafts/ 下的文件"), nil
			}
			if !textSuffixes[strings.ToLower(filepath.Ext(rel))] {
				return tools.DenyEnvelope("只允许读取 .txt、.md、.json 文件"), nil
			}

			raw, err := os.ReadFile(target)
			if err != nil {
				return tools.DenyEnvelope("read failed: " + err.Error()), nil
			}
			if !utf8.Valid(raw) {
				return tools.DenyEnvelope("read failed: 文件不是合法的 UTF-8 文本"), nil
			}
			lines := splitTextLines(string(raw))
			start := startLine - 1
			if start > len(lines) {
				start = len(lines)
			}
			end := start + maxLines
			if end > len(lines) {
				end = len(lines)
			}
			text := strings.Join(lines[start:end], "\n")
			if runes := []rune(text); len(runes) > textReadMaxChars {
				text = string(runes[:textReadMaxChars])
			}
			return tools.OKEnvelope(fmt.Sprintf("文件: %s\n行范围: %d-%d / %d\n内容:\n%s",
				rel, startLine, end, len(lines), text)), nil
		})
}

func isUnderTextCategory(rel string) bool {
	for _, cat := range textCategories {
		if strings.HasPrefix(rel, cat+"/") {
			return true
		}
	}
	return false
}

// splitTextLines 近似 Python str.splitlines：兼容 \r\n 与 \r，末尾换行不产生空行。
func splitTextLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
