# -*- coding: utf-8 -*-
"""修复 tools_reference.go:包声明 + 删 WithReferenceTools 适配器。"""
import re

p = r"C:\wkf\Shotwise\server-go\internal\agent\tools\video\tools_reference.go"
src = open(p, encoding="utf-8", newline="").read()

# 1) 包声明(包注释后) + 插入根包 import
src = src.replace("package agent\n\nimport (", "package video\n\nimport \"github.com/shotwise/shotwise/internal/agent/tools\"\n\nimport (", 1)
assert src.startswith("//") and "package video\n" in src

# 2) 删 WithReferenceTools(含前置注释)
src, n = re.subn(
    r"// WithReferenceTools 注入参考生视频工具链依赖.*?\nfunc WithReferenceTools\(.*?\n\}\n\n\n",
    "",
    src,
    flags=re.S,
)
assert n == 1, "WithReferenceTools matched %d" % n
assert "RuntimeOption" not in src and "WithReferenceTools" not in src, "adapter residue"

open(p, "w", encoding="utf-8", newline="").write(src)
print("tools_reference.go fixed")
