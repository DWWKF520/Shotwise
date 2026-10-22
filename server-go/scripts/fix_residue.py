# -*- coding: utf-8 -*-
"""迁移残留清理:删除三处未删除的孤儿声明,改造 video 依赖结构。"""
import re

TOOLS = r"C:\wkf\Shotwise\server-go\internal\agent\tools"

# 1) imageedit: 删 WithImageEditTools 块(含前置注释)
p = TOOLS + r"\imageedit\tools_image_edit.go"
src = open(p, encoding="utf-8", newline="").read()
src = re.sub(r"\n// WithImageEditTools [^\n]*\nfunc WithImageEditTools\(.*?\n\}\n", "\n", src, flags=re.S)
open(p, "w", encoding="utf-8", newline="").write(src)
assert "WithImageEditTools" not in src, "imageedit residue remains"

# 2) video: 删注册表 var 块;videoGenDeps -> Deps 导出
p = TOOLS + r"\video\tools_generate_video.go"
src = open(p, encoding="utf-8", newline="").read()
src = re.sub(r"\n// videoGenRegistry 把视频生成工具依赖按 Runtime 实例登记。.*?\n\}\n\)\n", "\n", src, flags=re.S)
src = src.replace("type videoGenDeps struct {", "type Deps struct {")
src = re.sub(r"\bvideoGenDeps\b", "Deps", src)
src = re.sub(r"func \(d Deps\) ready\(\) bool", "func (d Deps) Ready() bool", src)
src = re.sub(r"\.ready\(\)", ".Ready()", src)
src = re.sub(r"^\t(queue|projects|caps|scripts)(\s)", lambda m: "\t" + m.group(1).capitalize() + m.group(2), src, flags=re.M)
src = re.sub(r"\.(queue|projects|caps|scripts)\b", lambda m: "." + m.group(1).capitalize(), src)
open(p, "w", encoding="utf-8", newline="").write(src)
assert "videoGenRegistry" not in src and "videoGenDeps" not in src, "video residue remains"

# 3) timeline: 删 timelineDeps var 块(含前置注释)
p = TOOLS + r"\timeline\tools_timeline.go"
src = open(p, encoding="utf-8", newline="").read()
src = re.sub(r"\n// timelineDeps 把注入的依赖按 Runtime 指针关联。.*?\n\}\{by: map\[\*Runtime\]TimelinePlanService\{\}\}\n", "\n", src, flags=re.S)
open(p, "w", encoding="utf-8", newline="").write(src)
assert "timelineDeps" not in src, "timeline residue remains"

print("residue removal OK")
