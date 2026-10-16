import os
import subprocess
import tempfile

d = tempfile.mkdtemp()
# 视频流 2s，音频流 2.1s → 容器时长 2.1，last 位置 2.099 落在视频 EOF 之后
src = os.path.join(d, "long_audio.mp4")
gen = subprocess.run(
    [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
        "-f", "lavfi", "-i", "color=c=red:s=320x240:d=2:r=25",
        "-f", "lavfi", "-i", "sine=frequency=440:duration=2.1",
        "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
        "-c:a", "aac", src,
    ],
    capture_output=True, text=True,
)
print("gen exit:", gen.returncode, gen.stderr[-200:])

probe = subprocess.run(
    ["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "json", src],
    capture_output=True, text=True,
)
print("probe:", probe.stdout.strip().replace("\n", " "))

out = os.path.join(d, "frame_last.jpg")
r = subprocess.run(
    [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
        "-ss", "2.099", "-i", src, "-frames:v", "1", "-q:v", "2", out,
    ],
    capture_output=True, text=True,
)
size = os.path.getsize(out) if os.path.exists(out) else -1
print(f"last(2.099) exit={r.returncode} size={size}")
print("  stderr:", r.stderr[-300:].replace("\n", " | "))