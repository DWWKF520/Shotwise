package tools

import "sync"

// TextFileGate 记录每个项目最近一次 list_project_text_files 的清单签名。
// read_project_text 仅在签名与当前文件状态一致时放行（对应 Python ctx.has_fresh_text_file_list），
// 避免会话跨轮次读取已被页面上传 / 编辑 / 删除的过期清单。
// 非导出字段配合导出方法：签名由持有方（agent Runtime）按会话生命周期管理。
type TextFileGate struct {
	mu   sync.Mutex
	sigs map[string]string
}

// Mark 记录项目当前的清单签名。
func (g *TextFileGate) Mark(project, sig string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sigs == nil {
		g.sigs = map[string]string{}
	}
	g.sigs[project] = sig
}

// Fresh 判断签名与最近一次记录是否一致。
func (g *TextFileGate) Fresh(project, sig string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sigs[project] != "" && g.sigs[project] == sig
}
