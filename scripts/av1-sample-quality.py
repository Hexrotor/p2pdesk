"""Compare saved 1080p I420 benchmark samples against the exact input frames.

SSIM uses non-overlapping 8x8 Y blocks with sample covariance. These are sampled
frame metrics, not whole-sequence SSIM or a guarantee of equal visual quality.
Requires numpy and Pillow. All panels use the same limited BT.709 conversion.
"""
import argparse
import json
from pathlib import Path
import re

import numpy as np
from PIL import Image, ImageDraw

WIDTH, HEIGHT = 1920, 1080
FRAME_BYTES = WIDTH * HEIGHT * 3 // 2


def luma(data):
    return np.frombuffer(data, dtype=np.uint8, count=WIDTH * HEIGHT).reshape(HEIGHT, WIDTH)


def ssim(original, decoded):
    h, w = (n // 8 * 8 for n in original.shape)
    a, b = [image[:h, :w].astype(np.float64).reshape(h // 8, 8, w // 8, 8)
            .transpose(0, 2, 1, 3).reshape(-1, 64) for image in (original, decoded)]
    ma, mb = a.mean(axis=1), b.mean(axis=1)
    da, db = a - ma[:, None], b - mb[:, None]
    va, vb, cov = (da * da).sum(axis=1) / 63, (db * db).sum(axis=1) / 63, (da * db).sum(axis=1) / 63
    return float((((2 * ma * mb + 6.5025) * (2 * cov + 58.5225)) /
                  ((ma * ma + mb * mb + 6.5025) * (va + vb + 58.5225))).mean())


def rgb(data):
    y = (luma(data).astype(np.float32) - 16) * (255 / 219)
    planes = np.frombuffer(data, dtype=np.uint8, offset=WIDTH * HEIGHT).reshape(2, HEIGHT // 2, WIDTH // 2)
    u, v = (planes.astype(np.float32) - 128).repeat(2, axis=1).repeat(2, axis=2) * (255 / 224)
    channels = np.stack((y + 1.5748 * v, y - 0.187324 * u - 0.468124 * v, y + 1.8556 * u), axis=-1)
    return Image.fromarray(np.clip(channels.round(), 0, 255).astype(np.uint8))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--samples", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    references = {}
    results = []
    with args.input.open("rb") as stream:
        for path in sorted(args.samples.glob("*.i420")):
            match = re.search(r"-f(\d+)\.i420$", path.name)
            if match is None:
                continue
            index = int(match[1])
            if index not in references:
                stream.seek(index * FRAME_BYTES)
                references[index] = stream.read(FRAME_BYTES)
            original, decoded = references[index], path.read_bytes()
            if len(original) != FRAME_BYTES or len(decoded) != FRAME_BYTES:
                raise ValueError(f"incorrect sample size: {path}")
            a, b = luma(original), luma(decoded)
            results.append(dict(sample=path.name, y_ssim_8x8=ssim(a, b),
                                code_roi_y_ssim_8x8=ssim(a[150:950, 45:645], b[150:950, 45:645])))
    if not results:
        raise ValueError("no benchmark samples found")
    (args.output / "sample-quality.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
    # Keep an unscaled text crop for visual checks, using frame 180 of the standard corpus.
    if 180 in references:
        panels = [("Input I420", references[180])]
        for scale in ("1", "0.5"):
            name = f"s8-t8-afalse-ifalse-legacyfalse-r{scale}-f180.i420"
            panels.append((f"AV1 speed 8 / target x{scale}", (args.samples / name).read_bytes()))
        for cq in (15, 20, 28, 35):
            path = args.samples / f"s8-t8-afalse-ifalse-legacyfalse-r1-cq{cq}-f180.i420"
            if path.exists():
                panels.append((f"AV1 speed 8 / CQ {cq} / target x1", path.read_bytes()))
        crop = (60, 180, 520, 390)
        canvas = Image.new("RGB", (460 * len(panels), 240), "white")
        draw = ImageDraw.Draw(canvas)
        for i, (title, data) in enumerate(panels):
            draw.text((460 * i + 4, 8), title, fill="black")
            canvas.paste(rgb(data).crop(crop), (460 * i, 30))
        canvas.save(args.output / "text-comparison.png")
    print(f"SAMPLE_QUALITY samples={len(results)} dimensions=1920x1080 output={args.output}")


if __name__ == "__main__":
    main()
