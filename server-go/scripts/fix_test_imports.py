# -*- coding: utf-8 -*-
"""修复 migrate_tests.py 的 import 插入残留:去掉多余反斜杠,为缺失的文件补插 import。"""
import os
import re

TOOLS = r"C:\wkf\Shotwise\server-go\internal\agent\tools"
ROOT_IMPORT = "github.com/shotwise/shotwise/internal/agent/tools"

for dirpath, _, files in os.walk(TOOLS):
    for fn in files:
        if not fn.endswith("_test.go"):
            continue
        p = os.path.join(dirpath, fn)
        with open(p, encoding="utf-8", newline="") as fh:
            src = fh.read().replace("\r\n", "\n")
        orig = src
        src = src.replace('import \\"', 'import "')
        pkg = re.match(r"(?s).*?^package (\w+)$", src, re.M).group(1)
        has_import = re.search(r'^import "' + re.escape(ROOT_IMPORT) + '"$', src, re.M)
        if not has_import:
            # 在 package 声明行后补插独立 import 声明
            src = re.sub(r"^(package " + pkg + r")\n", r"\1\n\nimport \"" + ROOT_IMPORT + "\"\n", src, count=1)
            # 再把可能的转义引号修掉(replacement 模板语义不确定,统一复查)
            src = src.replace('import \\"', 'import "')
        if src != orig:
            with open(p, "w", encoding="utf-8", newline="") as fh:
                fh.write(src)
            print("fixed", os.path.relpath(p, TOOLS))
print("DONE")
