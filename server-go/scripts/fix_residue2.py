# -*- coding: utf-8 -*-
"""迁移残留清理第二轮:video 注册表 var 块与 timeline timelineDeps var 块。"""
import re

TOOLS = r"C:\wkf\Shotwise\server-go\internal\agent\tools"

# video: 删注册表 var 块(注释 + var ( ... ) )
p = TOOLS + r"\video\tools_generate_video.go"
src = open(p, encoding="utf-8", newline="").read()
src, n = re.subn(
    r"\n// videoGenRegistry 把视频生成工具依赖按 Runtime 实例登记。.*?var \(\n.*?\n\)\n",
    "\n",
    src,
    flags=re.S,
)
assert n == 1, "video registry pattern matched %d times" % n
open(p, "w", encoding="utf-8", newline="").write(src)
assert "videoGenRegistry" not in src and "videoGenDeps" not in src, "video residue remains"

# timeline: 删 timelineDeps var 块
p = TOOLS + r"\timeline\tools_timeline.go"
src = open(p, encoding="utf-8", newline="").read()
src, n = re.subn(
    r"\n// timelineDeps 把注入的依赖按 Runtime 指针关联。.*?\nvar timelineDeps = struct \{.*?\n\}\{by: map\[\*Runtime\]TimelinePlanService\{\}\}\n",
    "\n",
    src,
    flags=re.S,
)
assert n == 1, "timelineDeps pattern matched %d times" % n
open(p, "w", encoding="utf-8", newline="").write(src)
assert "timelineDeps" not in src, "timeline residue remains"

print("residue round 2 OK")
