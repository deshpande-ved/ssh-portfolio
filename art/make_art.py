"""Regenerate the about-page photo sources embedded in main.go.

    python3 art/make_art.py ~/Desktop/code/vedsite/media/about

Each photo is cropped, sharpened and contrast-stretched with ffmpeg at the
biggest size the page will ever draw it (one value per terminal cell, at a
cell aspect of 0.5). main.go scales that down to whatever fits the visitor's
terminal and maps each cell to a character from terminalcam's 70-step
brightness ramp, coloured by its pixel.

Output: art/<name>.json.gz holding {cols, rows, lum: [...], rgb: [...]}, one
base64 entry per frame: lum is 1 byte per cell (already stretched), rgb is 3.
"""
import base64, glob, gzip, json, os, subprocess, sys, tempfile

ASPECT = 0.5              # terminal cell width / height
MAX_COLS, MAX_ROWS = 220, 64

# name: (file, ffmpeg crop w:h:x:y or None, width after crop, height after crop, gamma)
PHOTOS = {
    "hk":   ("1.jpg",   "3024:2650:0:1070", 3024, 2650, 1.0),
    "sem":  ("5.jpeg",  "640:598:150:170",  640,  598,  0.9),
    "bday": ("10.jpeg", None,               768,  1024, 1.0),
}
# screen recording: a scrolling grid, then a flip-through of the 52 weekly photos
BUZZ = ("3.mp4", "760:820:63:470", 760, 820)


def fit(w, h, max_cols, max_rows):
    cols = max_cols
    rows = round(cols * ASPECT * h / w)
    if rows > max_rows:
        rows = max_rows
        cols = round(rows * w / h / ASPECT)
    return cols, rows


def grab(src, crop, cols, rows, fmt):
    pre = f"crop={crop}," if crop else ""
    # full-resolution colour before scaling: on 4:2:0 the sharpen + saturation steps
    # leave the last column black whenever the width is odd
    vf = f"{pre}format=yuv444p,scale={cols}:{rows}:flags=lanczos,unsharp=3:3:1.2,eq=saturation=1.25,format={fmt}"
    raw = subprocess.run(["ffmpeg", "-loglevel", "error", "-i", src, "-vf", vf, "-f", "rawvideo", "-"],
                         capture_output=True, check=True).stdout
    n = cols * rows * (3 if fmt == "rgb24" else 1)
    return [raw[i:i + n] for i in range(0, len(raw), n)]


def convert(name, src, crop, w, h, gamma, out):
    cols, rows = fit(w, h, MAX_COLS, MAX_ROWS)
    gray = grab(src, crop, cols, rows, "gray")
    rgb = grab(src, crop, cols, rows, "rgb24")
    # one stretch for the whole clip so frames don't flicker
    px = sorted(b for f in gray for b in f)
    lo = px[len(px) // 100]
    hi = max(px[len(px) * 99 // 100], lo + 1)
    lum = [bytes(round(min(max((v - lo) / (hi - lo), 0), 1) ** gamma * 255) for v in f) for f in gray]
    data = {"cols": cols, "rows": rows,
            "lum": [base64.b64encode(f).decode() for f in lum],
            "rgb": [base64.b64encode(f).decode() for f in rgb]}
    with gzip.open(os.path.join(out, f"{name}.json.gz"), "wt") as fh:
        json.dump(data, fh, separators=(",", ":"))
    print(f"{name}: {cols}x{rows}, {len(lum)} frame(s)")


def buzz_frames(media, tmp):
    """One PNG per weekly photo: the first one, then one per cut in the flip-through."""
    src, crop = os.path.join(media, BUZZ[0]), BUZZ[1]
    subprocess.run(["ffmpeg", "-loglevel", "error", "-ss", "5.6", "-i", src, "-frames:v", "1",
                    "-vf", f"crop={crop}", os.path.join(tmp, "w_000.png")], check=True)
    subprocess.run(["ffmpeg", "-loglevel", "error", "-ss", "4.5", "-i", src,
                    "-vf", f"crop={crop},select='gt(scene,0.2)'", "-fps_mode", "vfr",
                    "-start_number", "1", os.path.join(tmp, "w_%03d.png")], check=True)
    n = len(glob.glob(os.path.join(tmp, "w_*.png")))
    if n != 52:
        sys.exit(f"expected 52 weekly photos in {BUZZ[0]}, found {n}; check the scene threshold")
    return os.path.join(tmp, "w_%03d.png")


if __name__ == "__main__":
    media = os.path.expanduser(sys.argv[1])
    out = os.path.dirname(os.path.abspath(__file__))
    for name, (f, crop, w, h, gamma) in PHOTOS.items():
        convert(name, os.path.join(media, f), crop, w, h, gamma, out)
    with tempfile.TemporaryDirectory() as tmp:
        convert("buzz", buzz_frames(media, tmp), None, BUZZ[2], BUZZ[3], 1.0, out)
