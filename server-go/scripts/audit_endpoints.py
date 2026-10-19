#!/usr/bin/env python3
"""端点覆盖审计：逐条比对 Python 路由与 Go 注册面。

用法：
    python server-go/scripts/audit_endpoints.py          # 汇总 + 缺失清单
    python server-go/scripts/audit_endpoints.py --diff   # 只打印缺失清单

为什么需要它：Python 侧 328 个路由装饰器散在 45 个 router 文件里，Go 侧的注册面又按
router 分文件重新组织过，「哪几条还没迁」不能靠人眼数——逐条对拍才有唯一答案。

比对口径：
- Python：`@router.<method>("<path>")` / `@router.api_route("<path>", methods=[...])`，
  最终前缀 = `include_router` 的 prefix + router 自带的 `APIRouter(prefix=...)`；
- Go：`<group>.<METHOD>("<path>")`，group 前缀由 `x := parent.Group("/base")` 链式推导，
  根前缀为 `/api/v1`（与 `internal/app` 的 v1 组一致）；
- 路径参数 `{name}` 与 `:name` 一律归一为 `:param`，再做集合比对。
"""

from __future__ import annotations

import ast
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PY_ROUTERS = ROOT / "server" / "routers"
PY_APP = ROOT / "server" / "app.py"
GO_HTTP = ROOT / "server-go" / "internal" / "transport" / "http"
GO_APP = ROOT / "server-go" / "internal" / "app"

HTTP_METHODS = {"get", "post", "put", "patch", "delete", "head", "options"}


def normalize(path: str) -> str:
    path = re.sub(r"\{[^}/]+\}", ":param", path)
    path = re.sub(r":[A-Za-z_][A-Za-z0-9_]*", ":param", path)
    # gin 的 catch-all / 通配段（*template_path）等价于一个路径参数。
    path = re.sub(r"\*[A-Za-z_][A-Za-z0-9_]*", ":param", path)
    path = re.sub(r"/{2,}", "/", path)
    if len(path) > 1 and path.endswith("/"):
        path = path[:-1]
    return path


# ---------------- Python 侧 ----------------


def python_includes() -> list[tuple[str, str, str]]:
    """解析 app.py：返回 [(module, attr, prefix)]。"""
    source = PY_APP.read_text(encoding="utf-8")
    out: list[tuple[str, str, str]] = []
    for match in re.finditer(
        r"app\.include_router\(\s*\n?\s*([A-Za-z_][\w]*)\.(\w+),(.*?)\n\)", source, re.S
    ) or []:
        pass
    # 逐调用扫描：以 "app.include_router(" 起，按括号配平截取实参文本。
    index = 0
    while True:
        start = source.find("app.include_router(", index)
        if start < 0:
            break
        pos = start + len("app.include_router(")
        depth = 1
        while pos < len(source) and depth:
            if source[pos] == "(":
                depth += 1
            elif source[pos] == ")":
                depth -= 1
            pos += 1
        args = source[start + len("app.include_router(") : pos - 1]
        head = re.match(r"\s*([A-Za-z_][\w]*)\.(\w+)", args)
        prefix_match = re.search(r'prefix\s*=\s*"([^"]*)"', args)
        if head:
            out.append((head.group(1), head.group(2), prefix_match.group(1) if prefix_match else ""))
        index = pos
    return out


def router_own_prefix(module: str, attr: str) -> str:
    path = PY_ROUTERS / f"{module}.py"
    if not path.exists():
        return ""
    source = path.read_text(encoding="utf-8")
    match = re.search(rf"{attr}\s*=\s*APIRouter\(([^)]*)\)", source, re.S)
    if not match:
        return ""
    prefix_match = re.search(r'prefix\s*=\s*"([^"]*)"', match.group(1))
    return prefix_match.group(1) if prefix_match else ""


def python_endpoints() -> dict[str, set[str]]:
    includes = python_includes()
    routes: dict[str, set[str]] = {}
    for module, attr, include_prefix in includes:
        path = PY_ROUTERS / f"{module}.py"
        if not path.exists():
            continue
        prefix = include_prefix + router_own_prefix(module, attr)
        tree = ast.parse(path.read_text(encoding="utf-8"))
        for node in ast.walk(tree):
            if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            for deco in node.decorator_list:
                target = deco.func if isinstance(deco, ast.Call) else deco
                if not isinstance(target, ast.Attribute):
                    continue
                if not isinstance(target.value, ast.Name) or target.value.id != attr:
                    continue
                if not isinstance(deco, ast.Call) or not deco.args:
                    continue
                if not isinstance(deco.args[0], ast.Constant):
                    continue
                route = normalize(prefix + deco.args[0].value)
                if target.attr in HTTP_METHODS:
                    routes.setdefault(module, set()).add(f"{target.attr.upper()} {route}")
                elif target.attr == "api_route":
                    for kw in deco.keywords:
                        if kw.arg != "methods" or not isinstance(kw.value, (ast.List, ast.Tuple)):
                            continue
                        for elt in kw.value.elts:
                            if isinstance(elt, ast.Constant):
                                routes.setdefault(module, set()).add(f"{str(elt.value).upper()} {route}")
    return routes


# ---------------- Go 侧 ----------------

GO_GROUP = re.compile(r'\b([A-Za-z_]\w*)\s*:?=\s*([A-Za-z_]\w*(?:\.\w+)?)\s*\.\s*Group\(\s*"([^"]*)"')
GO_CALL = re.compile(
    r'\b([A-Za-z_]\w*(?:\.\w+)?)\s*\.\s*(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Any)\s*\(\s*([^,()]+)'
)
# 路由首参常是常量（base / prefix 拼接），需要先把字符串常量收集出来再求值。
GO_STR_DECL = re.compile(r'\b([A-Za-z_]\w*)\s*(?::=|=)\s*((?:"(?:[^"\\]|\\.)*"\s*(?:\+\s*)?)+)')


def eval_string(expr: str, consts: dict[str, str], depth: int = 0) -> str | None:
    """求值「字面量 / 已知常量 / 二者用 + 拼接」的表达式；求不出返回 None。"""
    if depth > 6:
        return None
    parts = [p.strip() for p in expr.split("+")]
    out = []
    for part in parts:
        if not part:
            continue
        if part.startswith('"') and part.endswith('"') and len(part) >= 2:
            out.append(part[1:-1])
            continue
        if part.startswith("(") and part.endswith(")"):
            inner = eval_string(part[1:-1], consts, depth + 1)
            if inner is None:
                return None
            out.append(inner)
            continue
        resolved = consts.get(part)
        if resolved is None:
            return None
        out.append(resolved)
    return "".join(out)


def go_endpoints() -> dict[str, set[str]]:
    """解析 Go 侧注册面。

    前缀与字符串常量都按**文件**作用域解析：`admin` / `base` / `gated` 这类短变量在不同
    register 函数里指不同分组，混成全局字典会让后解析的文件覆盖前一个（例如把
    `admin := group.Group("/agent", ...)` 覆盖成 `/api/v1`，导致整组端点被误判未迁）。
    """
    out: dict[str, set[str]] = {}
    files = [p for p in sorted(GO_HTTP.glob("*.go")) if not p.name.endswith("_test.go")]
    files += [p for p in sorted(GO_APP.glob("*.go")) if not p.name.endswith("_test.go")]

    # 跨文件也能看到的包级字符串常量（const / var），先建一份全局底表。
    texts = {p: p.read_text(encoding="utf-8") for p in files}
    global_consts: dict[str, str] = {}
    for text in texts.values():
        for match in GO_STR_DECL.finditer(text):
            value = eval_string(match.group(2), global_consts)
            if value is not None:
                global_consts.setdefault(match.group(1), value)

    for path in files:
        text = texts[path]
        consts = dict(global_consts)
        for match in GO_STR_DECL.finditer(text):
            value = eval_string(match.group(2), consts)
            if value is not None:
                consts[match.group(1)] = value

        prefixes: dict[str, str] = {"r": "", "router": ""}
        for _ in range(4):
            for match in GO_GROUP.finditer(text):
                var, parent, base = match.group(1), match.group(2), match.group(3)
                parent_prefix = prefixes.get(parent, prefixes.get(parent.split(".")[-1], "/api/v1"))
                prefixes[var] = normalize(parent_prefix + base)

        for match in GO_CALL.finditer(text):
            target, method, raw = match.group(1), match.group(2), match.group(3)
            route = eval_string(raw, consts)
            if route is None:
                continue
            prefix = prefixes.get(target, prefixes.get(target.split(".")[-1], "/api/v1"))
            full = normalize(prefix + route)
            methods = ["GET", "POST", "PUT", "PATCH", "DELETE"] if method == "Any" else [method]
            for m in methods:
                out.setdefault(path.stem, set()).add(f"{m} {full}")
    return out


def main() -> int:
    py = python_endpoints()
    go_flat: set[str] = set()
    for routes in go_endpoints().values():
        go_flat |= routes
    total_py = sum(len(v) for v in py.values())
    missing_by_module = {m: routes - go_flat for m, routes in py.items()}
    missing_by_module = {m: v for m, v in missing_by_module.items() if v}
    missing = sum(len(v) for v in missing_by_module.values())

    if "--diff" not in sys.argv:
        print(f"Python 端点 {total_py} / Go 注册 {len(go_flat)} / 缺失 {missing}")
        print()
    for module, routes in sorted(missing_by_module.items(), key=lambda kv: (-len(kv[1]), kv[0])):
        print(f"## {module} ({len(routes)})")
        for route in sorted(routes):
            print(f"  {route}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
