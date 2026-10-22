# -*- coding: utf-8 -*-
"""server-go 工具目录化迁移:
Phase A: git mv 工具文件到 internal/agent/tools/<分类>/,切割共享声明组装根包文件,删除 WithXxx/登记表
Phase B: 改 package 声明、插入根包 import、全局改名共享标识符、导出构造器
"""
import re, os, subprocess, sys

AGENT = r"C:\wkf\Shotwise\server-go\internal\agent"
TOOLS = os.path.join(AGENT, "tools")
ROOT_IMPORT = "github.com/shotwise/shotwise/internal/agent/tools"
os.chdir(AGENT)

def read(f):
    with open(f, encoding="utf-8", newline="") as fh:
        return fh.read().replace("\r\n", "\n")

def write(f, src):
    with open(f, "w", encoding="utf-8", newline="") as fh:
        fh.write(src)

# ---------- Phase A: 声明块切割 ----------

def decl_end(lines, i):
    """声明的结束行(exclusive)。规则:gofmt 代码顶层闭合大括号必在列 0;
    多行函数签名(首行无 {)先找到含 { 的行;分组 var/const 以 ^) 闭合。"""
    line = lines[i]
    if line.startswith("var (") or line.startswith("const ("):
        j = i + 1
        while j < len(lines):
            if re.match(r"^\)", lines[j]):
                return j + 1
            j += 1
        return len(lines)
    if "{" in line:
        depth = 0
        for ch in line:
            if ch == "{":
                depth += 1
            elif ch == "}":
                depth -= 1
        if depth == 0:
            return i + 1  # 单行声明
    elif line.startswith("type ") or line.startswith("var ") or line.startswith("const "):
        return i + 1  # 单行 type 别名 / var / const
    # func 多行签名:找含 { 的行,再找列 0 的 }
    j = i + 1
    while j < len(lines) and "{" not in lines[j]:
        j += 1
    if j >= len(lines):
        return len(lines)
    depth = 0
    for ch in lines[j]:
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
    if depth == 0:
        return j + 1  # 单行体
    j += 1
    while j < len(lines):
        if re.match(r"^\}", lines[j]):
            return j + 1
        j += 1
    return len(lines)

def find_decl_spans(src):
    """返回 {名字: (start, end)} 顶层声明区间(含前置注释)。end exclusive。"""
    lines = src.split("\n")
    spans = {}
    i, n = 0, len(lines)
    while i < n:
        line = lines[i]
        mfunc = re.match(r"^func (?:\([a-zA-Z_]+ \*?[a-zA-Z_]\w*\) )?([a-zA-Z_]\w*)\s*[\(]", line)
        mtype = re.match(r"^type ([a-zA-Z_]\w*)\b", line)
        mgroup = re.match(r"^(?:var|const) \(", line)
        msingle = re.match(r"^(?:var|const) ([a-zA-Z_]\w*)\s*[=]", line)
        if mfunc or mtype or msingle:
            name = (mfunc or mtype or msingle).group(1)
            end = decl_end(lines, i)
            s = i
            while s > 0 and (lines[s - 1].startswith("//") or lines[s - 1].strip() == ""):
                if lines[s - 1].strip() == "" and s - 1 > 0 and not lines[s - 2].startswith("//"):
                    break
                s -= 1
            spans.setdefault(name, (s, end))
            i = end
        elif mgroup:
            end = decl_end(lines, i)
            for k in range(i + 1, end - 1):
                vm = re.match(r"^\t([a-zA-Z_]\w*)\s*[= ]", lines[k])
                if vm:
                    spans.setdefault(vm.group(1), (i, end))
            i = end
        else:
            i += 1
    return spans, lines

def cut(src, names):
    spans, lines = find_decl_spans(src)
    ranges = []
    for name in names:
        if name in spans:
            s, e = spans[name]
            ranges.append((s, e, name))
        else:
            print("  !! 未找到声明:", name)
    ranges.sort()
    out, cutted, prev = [], {}, 0
    for s, e, name in ranges:
        if s < prev:
            continue  # 同一 var/const 块的多名,整块已删
        out.extend(lines[prev:s])
        cutted[name] = "\n".join(lines[s:e])
        prev = e
    out.extend(lines[prev:])
    return "\n".join(out), cutted

# ---------- Phase A: git mv ----------
MOVES = [
    ("tools_builtin.go", "builtin", "tools_builtin.go"),
    ("tools_project.go", "project", "tools_project.go"),
    ("tools_read.go", "project", "tools_read.go"),
    ("tools_memory.go", "memory", "tools_memory.go"),
    ("tools_assets.go", "assets", "tools_assets.go"),
    ("tools_rename.go", "assets", "tools_rename.go"),
    ("tools_relations.go", "assets", "tools_relations.go"),
    ("tools_script.go", "script", "tools_script.go"),
    ("tools_episode_planning.go", "script", "tools_episode_planning.go"),
    ("tools_text_generation.go", "script", "tools_text_generation.go"),
    ("tools_storyboards.go", "storyboard", "tools_storyboards.go"),
    ("tools_grid.go", "storyboard", "tools_grid.go"),
    ("tools_image_edit.go", "imageedit", "tools_image_edit.go"),
    ("tools_generate_video.go", "video", "tools_generate_video.go"),
    ("tools_video.go", "video", "tools_video.go"),
    ("tools_reference.go", "video", "tools_reference.go"),
    ("tools_review.go", "review", "tools_review.go"),
    ("review_ad.go", "review", "review_ad.go"),
    ("review_media.go", None, "mediaprobe.go"),
    ("tools_timeline.go", "timeline", "tools_timeline.go"),
]
for fname, cat, newname in MOVES:
    dest_dir = TOOLS if cat is None else os.path.join(TOOLS, cat)
    os.makedirs(dest_dir, exist_ok=True)
    dest = os.path.join(dest_dir, newname)
    r = subprocess.run(["git", "mv", fname, dest], capture_output=True, text=True)
    if r.returncode != 0:
        print("git mv FAIL", fname, r.stderr)
        sys.exit(1)
    print("moved", fname, "->", os.path.relpath(dest, AGENT))

os.remove("tools_generation.go")
subprocess.run(["git", "rm", "-q", "--cached", "tools_generation.go"], capture_output=True)
print("deleted tools_generation.go (根包 gentask.go 替代)")

# ---------- Phase A: 各文件删除/移动 ----------
PLAN = {
    "tools/assets/tools_assets.go": dict(
        delete=["WithAssetGeneration", "WithAssetProjects", "AssetQueue", "AssetProjects",
                "awaitAssetTask", "dedupStrings", "assetPollInterval", "assetWorkerOfflineGrace", "assetDefaultLease"]),
    "tools/project/tools_project.go": dict(delete=["WithProjectWriters", "contains"]),
    "tools/project/tools_read.go": dict(delete=["textFileGate", "mark", "fresh"]),
    "tools/memory/tools_memory.go": dict(delete=["WithMemoryProposals"]),
    "tools/assets/tools_rename.go": dict(delete=["WithAssetRename"]),
    "tools/assets/tools_relations.go": dict(delete=["WithRelationAnalysis"]),
    "tools/script/tools_script.go": dict(delete=["WithScriptEditing", "ScriptStore"]),
    "tools/script/tools_episode_planning.go": dict(delete=["WithEpisodePlanning"]),
    "tools/script/tools_text_generation.go": dict(
        delete=["WithTextGenerationTools", "WithScriptReviewTools", "TextGenerationProjects"],
        move={"readJSONObject": "ReadJSONObject", "targetLanguage": "TargetLanguage",
              "normalizeInstructions": "NormalizeInstructions", "usesReferenceVideoUnits": "UsesReferenceVideoUnits"}),
    "tools/video/tools_reference.go": dict(
        move={"referenceResolver": "referenceResolver", "referenceContextForTool": "ReferenceContextForTool"}),
    "tools/storyboard/tools_storyboards.go": dict(delete=["WithGenerationTools"]),
    "tools/storyboard/tools_grid.go": dict(delete=["WithGridTools"]),
    "tools/imageedit/tools_image_edit.go": dict(delete=["WithImageEditTools"]),
    "tools/video/tools_video.go": dict(delete=["WithVideoCapabilities", "VideoCapabilityReader"]),
    "tools/video/tools_generate_video.go": dict(
        delete=["WithVideoGeneration", "VideoGenerationDeps", "videoGenRegistry"]),
    "tools/timeline/tools_timeline.go": dict(
        delete=["WithTimelineTools", "timelineDeps", "timelineTools"]),
    "tools/review/tools_review.go": dict(
        move={
            "videoPathFor": "VideoPathFor", "loadRawProject": "LoadRawProject", "loadScript": "LoadScript",
            "validateScriptFilename": "ValidateScriptFilename", "relativeScriptName": "relativeScriptName",
            "scriptCandidates": "scriptCandidates", "episodeMetaFor": "EpisodeMetaFor",
            "chooseScript": "ChooseScript", "resolveContentMode": "ResolveContentMode",
            "itemCollection": "ItemCollection", "reviewItemID": "ReviewItemID",
            "plannedDuration": "PlannedDuration", "reviewItemTexts": "ReviewItemTexts",
            "reviewItemReferences": "ReviewItemReferences", "reviewRef": "ReviewRef",
            "optionalFloat": "OptionalFloat", "boolIs": "BoolIs", "asString": "AsString",
            "asMap": "AsMap", "asList": "AsList", "asNumber": "AsNumber", "truthy": "Truthy",
            "pyStr": "pyStr", "encodeReviewJSON": "EncodeReviewJSON", "roundReview3": "RoundReview3",
        }),
}

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
}

moved_doc, moved_gen = [], []
for relpath, ops in PLAN.items():
    f = os.path.normpath(os.path.join(AGENT, relpath))
    src = read(f)
    names = list(ops.get("delete", [])) + list(ops.get("move", {}).keys())
    src2, cutted = cut(src, names)
    for orig, exported in ops.get("move", {}).items():
        if orig in cutted:
            body = cutted[orig]
            if orig in DOCRENAMES and DOCRENAMES[orig] != orig:
                body = re.sub(r"\b" + orig + r"\b", DOCRENAMES[orig], body)
            (moved_doc if "review" in relpath else moved_gen).append(body)
    write(f, src2)
    print("transformed", relpath)

# 组装根包 projectdoc.go / genjson.go
DOC_HEADER = """package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// 项目 / 剧本文档读取与 JSON 解析的共享辅助(原 agent 包 tools_review.go 的通用部分)。

"""
GEN_HEADER = """package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/service"
)

// 剧本生成 / 参考视频链共享的 LLM 输出解析与参考上下文辅助。

"""
write(os.path.join(TOOLS, "projectdoc.go"), DOC_HEADER + "\n".join(moved_doc))
write(os.path.join(TOOLS, "genjson.go"), GEN_HEADER + "\n".join(moved_gen))
print("wrote projectdoc.go (%d decls), genjson.go (%d decls)" % (len(moved_doc), len(moved_gen)))

# ---------- Phase B: 包名 / import / 全局改名 ----------

GLOBAL_MAP = [
    (r"\bokEnvelope\(", "tools.OKEnvelope("),
    (r"\bdenyEnvelope\(", "tools.DenyEnvelope("),
    (r"\bdenyResult\(", "tools.DenyResult("),
    (r"\bawaitAssetTask\(", "tools.AwaitAssetTask("),
    (r"\bdedupStrings\(", "tools.DedupStrings("),
    (r"\bcontains\(", "tools.Contains("),
    (r"\bgenerationTask\b", "tools.GenerationTask"),
    (r"\benqueueGenerationTasks\(", "tools.EnqueueGenerationTasks("),
    (r"\bawaitGenerationTasks\(", "tools.AwaitGenerationTasks("),
    (r"\bgenerationOutcome\b", "tools.GenerationOutcome"),
    (r"\bgenerationSummary\(", "tools.GenerationSummary("),
    (r"\bmissingGeneratedAsset\(", "tools.MissingGeneratedAsset("),
    (r"\bselectScriptItemsByID\(", "tools.SelectScriptItemsByID("),
    (r"\bgeneratedAssetPath\(", "tools.GeneratedAssetPath("),
    (r"\.versionSuffix\(", ".VersionSuffix("),
    (r"\.filePath\(", ".FilePath("),
    (r"\btoolWaitTimeout\b", "tools.ToolWaitTimeout"),
    (r"\bvalidateScriptFilename\(", "tools.ValidateScriptFilename("),
    (r"\bloadRawProject\(", "tools.LoadRawProject("),
    (r"\bloadScript\(", "tools.LoadScript("),
    (r"\btruthy\(", "tools.Truthy("),
    (r"\basList\(", "tools.AsList("),
    (r"\basMap\(", "tools.AsMap("),
    (r"\basNumber\(", "tools.AsNumber("),
    (r"\basString\(", "tools.AsString("),
    (r"\bboolIs\(", "tools.BoolIs("),
    (r"\bchooseScript\(", "tools.ChooseScript("),
    (r"\bencodeReviewJSON\(", "tools.EncodeReviewJSON("),
    (r"\bepisodeMetaFor\(", "tools.EpisodeMetaFor("),
    (r"\bitemCollection\(", "tools.ItemCollection("),
    (r"\boptionalFloat\(", "tools.OptionalFloat("),
    (r"\bplannedDuration\(", "tools.PlannedDuration("),
    (r"\bresolveContentMode\(", "tools.ResolveContentMode("),
    (r"\breviewItemID\(", "tools.ReviewItemID("),
    (r"\breviewItemReferences\(", "tools.ReviewItemReferences("),
    (r"\breviewItemTexts\(", "tools.ReviewItemTexts("),
    (r"\bvideoPathFor\(", "tools.VideoPathFor("),
    (r"\broundReview3\(", "tools.RoundReview3("),
    (r"\breviewRef\b", "tools.ReviewRef"),
    (r"\bprobeMedia\(", "tools.ProbeMedia("),
    (r"\blookPathOK\(", "tools.LookPathOK("),
    (r"\bextractFrame\(", "tools.ExtractFrame("),
    (r"\bnormalizeInstructions\(", "tools.NormalizeInstructions("),
    (r"\breadJSONObject\(", "tools.ReadJSONObject("),
    (r"\btargetLanguage\(", "tools.TargetLanguage("),
    (r"\busesReferenceVideoUnits\(", "tools.UsesReferenceVideoUnits("),
    (r"\breferenceContextForTool\(", "tools.ReferenceContextForTool("),
    (r"\bAssetQueue\b", "tools.AssetQueue"),
    (r"\bAssetProjects\b", "tools.AssetProjects"),
    (r"\bGenerationProjects\b", "tools.GenerationProjects"),
    (r"\bScriptStore\b", "tools.ScriptStore"),
    (r"\bVideoCapabilityReader\b", "tools.VideoCapabilityReader"),
    (r"\bTextGenerationProjects\b", "tools.TextGenerationProjects"),
    (r"\btextFileGate\b", "tools.TextFileGate"),
    (r"\.mark\(", ".Mark("),
    (r"\.fresh\(", ".Fresh("),
]

CTOR_EXPORTS = {
    "builtin": ["BuiltinTools>Tools"],
    "assets": ["newGenerateAssetsTool", "newListPendingAssetsTool", "newRenameAssetTool", "newAnalyzeCharacterRelationsTool"],
    "project": ["newPatchProjectTool", "newListProjectTextFilesTool", "newReadProjectTextTool"],
    "memory": ["newProposeMemoryCandidateTool"],
    "script": ["newPatchEpisodeScriptTool", "newInsertSegmentTool", "newRemoveSegmentTool", "newSplitSegmentTool",
               "newPatchEpisodeMetaTool", "newResetEpisodePlanningTool", "newPlanEpisodesTool",
               "newSplitNarrationSegmentsTool", "newNormalizeDramaScriptTool", "newGenerateEpisodeScriptTool",
               "newConfirmScriptReviewTool"],
    "storyboard": ["newGenerateStoryboardsTool", "newGenerateNarrationAudioTool", "newGenerateGridTool"],
    "imageedit": ["newEditImagesTool"],
    "video": ["newGetVideoCapabilitiesTool", "newGenerateVideoEpisodeTool", "newGenerateVideoSceneTool",
              "newGenerateVideoAllTool", "newGenerateVideoSelectedTool"],
    "review": ["newInspectDirectorReviewTool"],
}

CATS = ["builtin", "project", "memory", "assets", "script", "storyboard", "imageedit", "video", "review", "timeline"]
for cat in CATS:
    d = os.path.join(TOOLS, cat)
    for fn in sorted(os.listdir(d)):
        if not fn.endswith(".go"):
            continue
        f = os.path.join(d, fn)
        src = read(f)
        src = re.sub(r"^package agent\b", "package " + cat, src, count=1)
        # 插入根包 import(独立 import 声明,合法且便于后续 gofmt)
        src = re.sub(r"^(package " + cat + r")\n", r"\1\n\nimport \"" + ROOT_IMPORT + "\"\n", src, count=1)
        for pat, rep in GLOBAL_MAP:
            src = re.sub(pat, rep, src)
        for ctor in CTOR_EXPORTS.get(cat, []):
            if ">" in ctor:
                old, new = ctor.split(">")
            else:
                old, new = ctor, ctor[0].upper() + ctor[1:]
            src = re.sub(r"\b" + old + r"\b", new, src)
        write(f, src)
        print("phased", os.path.relpath(f, AGENT))

# mediaprobe.go(根包):改包名 + 就地导出改名(同包内引用不加前缀)
f = os.path.join(TOOLS, "mediaprobe.go")
src = read(f)
src = re.sub(r"^package agent\b", "package tools", src, count=1)
for old, new in [("probeMedia", "ProbeMedia"), ("lookPathOK", "LookPathOK"), ("extractFrame", "ExtractFrame"),
                 ("asList", "AsList"), ("asMap", "AsMap"), ("asNumber", "AsNumber"), ("asString", "AsString"),
                 ("okEnvelope", "OKEnvelope"), ("denyEnvelope", "DenyEnvelope")]:
    src = re.sub(r"\b" + old + r"\b", new, src)
write(f, src)
print("phased mediaprobe.go")
print("PHASE A+B DONE")
