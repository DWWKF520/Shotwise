import json
import os
import subprocess
import tempfile

d = tempfile.mkdtemp()
p = os.path.join(d, "sample.mp4")
gen = subprocess.run(
    [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
        "-f", "lavfi", "-i", "color=c=red:s=320x240:d=2",
        "-f", "lavfi", "-i", "sine=frequency=440:duration=2",
        "-shortest", "-c:v", "libx264", "-preset", "ultrafast",
        "-pix_fmt", "yuv420p", "-c:a", "aac", p,
    ],
    capture_output=True, text=True,
)
print("gen exit:", gen.returncode)
print("gen stderr:", gen.stderr[-300:])

probe = subprocess.run(
    [
        "ffprobe", "-v", "error",
        "-show_entries", "format=duration:stream=codec_type,width,height",
        "-of", "json", p,
    ],
    capture_output=True, text=True,
)
print("probe exit:", probe.returncode)
print("probe stdout:", probe.stdout)
print("probe stderr:", probe.stderr[-300:])
with open(os.path.join(d, "result.json"), "w") as f:
    f.write(probe.stdout)
print("dir:", d)