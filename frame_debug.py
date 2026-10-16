import os
import subprocess
import tempfile

d = tempfile.mkdtemp()
src = os.path.join(d, "sample.mp4")
gen = subprocess.run(
    [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
        "-f", "lavfi", "-i", "color=c=red:s=320x240:d=2",
        "-f", "lavfi", "-i", "sine=frequency=440:duration=2",
        "-shortest", "-c:v", "libx264", "-preset", "ultrafast",
        "-pix_fmt", "yuv420p", "-c:a", "aac", src,
    ],
    capture_output=True, text=True,
)
print("gen exit:", gen.returncode, gen.stderr[-200:])

for pos, ts in [("first", "0.000"), ("middle", "1.000"), ("last", "1.999")]:
    out = os.path.join(d, f"frame_{pos}.jpg")
    r = subprocess.run(
        [
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
            "-ss", ts, "-i", src, "-frames:v", "1", "-q:v", "2", out,
        ],
        capture_output=True, text=True,
    )
    size = os.path.getsize(out) if os.path.exists(out) else -1
    print(f"pos={pos} ts={ts} exit={r.returncode} size={size}")
    print("  stderr:", r.stderr[-400:].replace("\n", " | "))

print("ffmpeg version:")
v = subprocess.run(["ffmpeg", "-version"], capture_output=True, text=True)
print(v.stdout.splitlines()[0])