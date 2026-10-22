# -*- coding: utf-8 -*-
"""server-go 工具目录化迁移 Phase C:工具测试文件随实现迁入各分类子包。

- git mv 测试文件到 internal/agent/tools/<分类>/,改 package 声明,插入根包 import
- 共享标识符加 tools. 前缀,构造器改为子包导出名
- tools_generation_test.go 迁入根包(package tools),共享标识符去前缀只改导出名
- tools_registration_test.go / tools_timeline_test.go 留在 agent 包(测的是 Runtime 装配)
"""
import os
import re
import subprocess

AGENT = r"C:\wkf\Shotwise\server-go\internal\agent"
TOOLS = os.path.join(AGENT, "tools")
ROOT_IMPORT = "github.com/shotwise/shotwise/internal/agent/tools"


def read(f):
    with open(f, encoding="utf-8", newline="") as fh:
        return fh.read().replace("\r\n", "\n").lstrip("﻿")


def write(f, src):
    with open(f, "w", encoding="utf-8", newline="") as fh:
        fh.write(src)


# 带根包前缀的共享标识符(子包测试用)
PREFIXED = [
    (r"\bmaxInstructionsLen\b", "tools.MaxInstructionsLen"),
    (r"\btextMaxOutputTokens\b", "tools.TextMaxOutputTokens"),
    (r"\bsortedKeys\(", "tools.SortedKeys("),
    (r"\bfetchVideoDurations\(", "tools.FetchVideoDurations("),
    (r"\bpatchTableKinds\b", "tools.PatchTableKinds"),
    (r"\breferenceResolver\(", "tools.ReferenceResolver("),
    (r"\breferenceContextForTool\(", "tools.ReferenceContextForTool("),
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
    (r"\bAssetQueue\b", "tools.AssetQueue"),
    (r"\bAssetProjects\b", "tools.AssetProjects"),
    (r"\bGenerationProjects\b", "tools.GenerationProjects"),
    (r"\bScriptStore\b", "tools.ScriptStore"),
    (r"\bVideoCapabilityReader\b", "tools.VideoCapabilityReader"),
    (r"\bTextGenerationProjects\b", "tools.TextGenerationProjects"),
    (r"\btextFileGate\b", "tools.TextFileGate"),
]

# 不带前缀的导出名(根包测试用,同包直接引用)
BARE_EXPORT = [
    (r"\benqueueGenerationTasks\(", "EnqueueGenerationTasks("),
    (r"\bawaitGenerationTasks\(", "AwaitGenerationTasks("),
    (r"\bgenerationTask\b", "GenerationTask"),
    (r"\bgenerationOutcome\b", "GenerationOutcome"),
    (r"\bgenerationSummary\(", "GenerationSummary("),
    (r"\bawaitAssetTask\(", "AwaitAssetTask("),
    (r"\bdedupStrings\(", "DedupStrings("),
    (r"\btoolWaitTimeout\b", "ToolWaitTimeout"),
    (r"\bselectScriptItemsByID\(", "SelectScriptItemsByID("),
    (r"\bgeneratedAssetPath\(", "GeneratedAssetPath("),
    (r"\bmissingGeneratedAsset\(", "MissingGeneratedAsset("),
    (r"\.versionSuffix\(", ".VersionSuffix("),
    (r"\.filePath\(", ".FilePath("),
]

# 构造器改名(迁移后与实现同包,直接用导出名)
CTOR_MAP = {
    "assets": ["newGenerateAssetsTool", "newListPendingAssetsTool", "newRenameAssetTool",
               "newAnalyzeCharacterRelationsTool"],
    "project": ["newPatchProjectTool", "newListProjectTextFilesTool", "newReadProjectTextTool"],
    "memory": ["newProposeMemoryCandidateTool"],
    "script": ["newPatchEpisodeScriptTool", "newInsertSegmentTool", "newRemoveSegmentTool",
               "newSplitSegmentTool", "newPatchEpisodeMetaTool", "newResetEpisodePlanningTool",
               "newPlanEpisodesTool", "newSplitNarrationSegmentsTool", "newNormalizeDramaScriptTool",
               "newGenerateEpisodeScriptTool", "newConfirmScriptReviewTool"],
    "storyboard": ["newGenerateStoryboardsTool", "newGenerateNarrationAudioTool", "newGenerateGridTool"],
    "imageedit": ["newEditImagesTool"],
    "video": ["newGetVideoCapabilitiesTool", "newGenerateVideoEpisodeTool", "newGenerateVideoSceneTool",
              "newGenerateVideoAllTool", "newGenerateVideoSelectedTool"],
    "review": ["newInspectDirectorReviewTool"],
}

MOVES = [
    ("tools_assets_test.go", "assets"),
    ("tools_relations_test.go", "assets"),
    ("tools_project_test.go", "project"),
    ("tools_read_test.go", "project"),
    ("tools_memory_test.go", "memory"),
    ("tools_script_test.go", "script"),
    ("tools_episode_planning_test.go", "script"),
    ("tools_text_generation_test.go", "script"),
    ("tools_grid_test.go", "storyboard"),
    ("tools_image_edit_test.go", "imageedit"),
    ("tools_generate_video_test.go", "video"),
    ("tools_reference_test.go", "video"),
    ("tools_video_test.go", "video"),
    ("tools_review_test.go", "review"),
    ("tools_generation_test.go", "tools"),
]

for fname, cat in MOVES:
    dest_dir = os.path.join(TOOLS, cat)
    os.makedirs(dest_dir, exist_ok=True)
    dest = os.path.join(dest_dir, fname)
    r = subprocess.run(["git", "mv", os.path.join(AGENT, fname), dest], capture_output=True, text=True)
    if r.returncode != 0:
        print("git mv FAIL", fname, r.stderr)
        continue
    src = read(dest)
    src = re.sub(r"^package agent\b", "package " + cat, src, count=1)
    src = re.sub(r"^(package " + cat + r")\n", r"\1\n\nimport \"" + ROOT_IMPORT + "\"\n", src, count=1)
    if cat == "tools":
        for pat, rep in BARE_EXPORT:
            src = re.sub(pat, rep, src)
    else:
        for pat, rep in PREFIXED:
            src = re.sub(pat, rep, src)
        for ctor in CTOR_MAP.get(cat, []):
            src = re.sub(r"\b" + ctor + r"\b", ctor[0].upper() + ctor[1:], src)
    write(dest, src)
    print("moved+phased", cat + "/" + fname)

# agent 包留守测试:共享标识符加前缀,BuiltinTools 改 builtin.Tools
for fname in ["tools_registration_test.go", "tools_timeline_test.go"]:
    f = os.path.join(AGENT, fname)
    src = read(f)
    for pat, rep in PREFIXED:
        src = re.sub(pat, rep, src)
    src = re.sub(r"\bBuiltinTools\(", "builtin.Tools(", src)
    write(f, src)
    print("phased (kept in agent)", fname)

print("PHASE C DONE")
