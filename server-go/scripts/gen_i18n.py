#!/usr/bin/env python3
"""把 Python 侧 i18n 文案表导出成 Go 的生成文件。

用法（仓库根目录）：

    python server-go/scripts/gen_i18n.py

读 lib/i18n/{zh,en,vi}/{errors,system,emails,providers,templates,assets,validation}.py
的 MESSAGES 字典（用 ast 解析，不 import 这些模块——它们依赖 pydantic），按 locale
合并后写 internal/i18n/bundle_generated.go。

约束：生成文件只做「补齐」，i18n.go 里的手写块优先（同 key 覆盖），因此手写块的措辞
调整不会被下次生成覆盖。
"""

from __future__ import annotations

import ast
import json
import os
import sys

LOCALES = ("zh", "en", "vi")
GO_NAMES = {"zh": "ZH", "en": "EN", "vi": "VI"}
MODULES = ("errors", "system", "emails", "providers", "templates", "assets", "validation")


def read_messages(path: str) -> dict[str, str]:
    tree = ast.parse(open(path, encoding="utf-8").read(), path)
    merged: dict[str, str] = {}
    for node in tree.body:
        target = None
        if isinstance(node, ast.Assign):
            if any(getattr(t, "id", None) == "MESSAGES" for t in node.targets):
                target = node.value
        elif isinstance(node, ast.AnnAssign) and getattr(node.target, "id", None) == "MESSAGES":
            target = node.value
        if target is not None:
            merged.update(ast.literal_eval(target))
    return merged


def main() -> int:
    repo_root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    i18n_root = os.path.join(repo_root, "lib", "i18n")
    bundles: dict[str, dict[str, str]] = {}
    for locale in LOCALES:
        merged: dict[str, str] = {}
        for module in MODULES:
            path = os.path.join(i18n_root, locale, f"{module}.py")
            if not os.path.exists(path):
                print(f"skip missing {path}", file=sys.stderr)
                continue
            merged.update(read_messages(path))
        bundles[locale] = merged
        print(f"{locale}: {len(merged)} keys")

    lines = [
        "// Code generated from lib/i18n/{zh,en,vi}/*.py by server-go/scripts/gen_i18n.py.",
        "// DO NOT EDIT.",
        "",
        "package i18n",
        "",
        "// generatedBundles 是 Python 侧文案表（errors / system / emails / providers /",
        "// templates / assets / validation 七张表合并）的逐条导出口，保证 Go 侧渲染出的",
        "// 文案与 Python 逐字一致。i18n.go 里的手写块优先（同 key 覆盖）。",
        "var generatedBundles = map[Locale]map[string]string{",
    ]
    for locale in LOCALES:
        messages = bundles[locale]
        lines.append(f"\t{GO_NAMES[locale]}: {{")
        for key in sorted(messages):
            value = messages[key].replace("\n", "\\n")
            lines.append(
                "\t\t%s: %s,"
                % (json.dumps(key, ensure_ascii=False), json.dumps(value, ensure_ascii=False))
            )
        lines.append("\t},")
    lines.append("}")
    lines.append("")

    out_path = os.path.join(repo_root, "server-go", "internal", "i18n", "bundle_generated.go")
    with open(out_path, "w", encoding="utf-8", newline="\n") as handle:
        handle.write("\n".join(lines))
    print(f"wrote {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
