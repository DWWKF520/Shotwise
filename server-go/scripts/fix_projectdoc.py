# -*- coding: utf-8 -*-
"""修复 projectdoc.go / genjson.go / tools_reference.go:
1) 恢复被反斜杠替换破坏的 Go 转义字符串
2) 对根包组装文件全局应用 DOCRENAMES(块间互引)
3) 声明间补空行
"""
import re

TOOLS = r"C:\wkf\Shotwise\server-go\internal\agent\tools"
BS = chr(92)  # backslash
BSQ = BS + chr(34)  # backslash + quote

DOCRENAMES = {
    "loadRawProject": "LoadRawProject", "loadScript": "LoadScript", "validateScriptFilename": "ValidateScriptFilename",
    "episodeMetaFor": "EpisodeMetaFor", "chooseScript": "ChooseScript", "resolveContentMode": "ResolveContentMode",
    "itemCollection": "ItemCollection", "reviewItemID": "ReviewItemID", "plannedDuration": "PlannedDuration",
    "reviewItemTexts": "ReviewItemTexts", "reviewItemReferences": "ReviewItemReferences", "reviewRef": "ReviewRef",
    "optionalFloat": "OptionalFloat", "boolIs": "BoolIs", "asString": "AsString", "asMap": "AsMap",
    "asList": "AsList", "asNumber": "AsNumber", "truthy": "Truthy", "encodeReviewJSON": "EncodeReviewJSON",
    "roundReview3": "RoundReview3", "videoPathFor": "VideoPathFor", "readJSONObject": "ReadJSONObject",
    "targetLanguage": "TargetLanguage", "normalizeInstructions": "NormalizeInstructions",
    "usesReferenceVideoUnits": "UsesReferenceVideoUnits", "referenceContextForTool": "ReferenceContextForTool",
    "okEnvelope": "OKEnvelope", "denyEnvelope": "DenyEnvelope", "denyResult": "DenyResult",
}

def fix_root(fname):
    p = TOOLS + BS + fname
    src = open(p, encoding="utf-8", newline="").read()
    for old, new in DOCRENAMES.items():
        if old != new:
            src = re.sub(r"\b" + old + r"\b", new, src)
    lines = src.split("\n")
    out = []
    for i, line in enumerate(lines):
        out.append(line)
        if line == "}" and i + 1 < len(lines) and lines[i + 1].strip() != "":
            out.append("")
    src = "\n".join(out)
    open(p, "w", encoding="utf-8", newline="").write(src)
    print("renamed", fname)

fix_root("projectdoc.go")
fix_root("genjson.go")

# 恢复 projectdoc.go 的反斜杠字符串
p = TOOLS + BS + "projectdoc.go"
src = open(p, encoding="utf-8", newline="").read()
bad = "strings.ReplaceAll(clip, " + chr(34) + ", " + chr(34) + "/" + chr(34) + ")"
good = "strings.ReplaceAll(clip, " + chr(34) + BSQ + chr(34) + ", " + chr(34) + "/" + chr(34) + ")"
if bad in src:
    src = src.replace(bad, good)
    open(p, "w", encoding="utf-8", newline="").write(src)
    print("fixed ReplaceAll in projectdoc.go")
else:
    print("WARN: ReplaceAll bad pattern not found; check manually")

# 恢复 tools_reference.go 的内嵌转义引号
p = TOOLS + BS + "video" + BS + "tools_reference.go"
src = open(p, encoding="utf-8", newline="").read()
bad = "({" + chr(34) + "episode" + chr(34) + ": %d})"
good = "({" + BSQ + chr(34) + "episode" + BSQ + chr(34) + ": %d})"
n = src.count(bad)
src = src.replace(bad, good)
open(p, "w", encoding="utf-8", newline="").write(src)
print("tools_reference.go episode escapes fixed:", n)
