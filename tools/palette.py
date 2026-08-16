#!/usr/bin/env python3
"""Colour separation, measured rather than eyeballed.

Two colours that read as different to most people can collapse into one for
a colour-blind reader, and no amount of looking at them will tell you. This
reports the perceptual distance between every pair — CIEDE2000 in Lab — with
normal vision and again under each of the three dichromacies, and fails the
pair on its WORST case.

    tools/palette.py --css app/static/style.css --set tones
    tools/palette.py '#3D4B96' '#00819B' '#C9432B'

Thresholds. dE2000 of 1 is the just-noticeable difference for adjacent
patches. Categorical colours are read apart at a glance, at small size, in
prose, so they need far more: this fails under 15 and warns under 25. The
number that mattered here was 5.8 — the first green/amber/red attempt for the
issue traffic light measured that under deuteranopia and had to be redone,
which is why this script exists at all.

No dependencies: sRGB -> linear -> XYZ -> Lab, the Viénot-Brettel-Mollon
dichromat simulation, and the CIEDE2000 formula, all in the standard library.
"""

import argparse
import math
import re
import sys

FAIL, WARN = 15.0, 25.0


def parse_hex(s):
    s = s.strip().lstrip("#")
    if len(s) == 3:
        s = "".join(c * 2 for c in s)
    if len(s) != 6:
        raise ValueError("not a hex colour: " + s)
    return tuple(int(s[i:i + 2], 16) / 255 for i in (0, 2, 4))


def to_linear(c):
    return c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4


def to_srgb(c):
    c = max(0.0, min(1.0, c))
    return 12.92 * c if c <= 0.0031308 else 1.055 * c ** (1 / 2.4) - 0.055


def rgb_to_xyz(rgb):
    r, g, b = (to_linear(c) for c in rgb)
    return (
        0.4124564 * r + 0.3575761 * g + 0.1804375 * b,
        0.2126729 * r + 0.7151522 * g + 0.0721750 * b,
        0.0193339 * r + 0.1191920 * g + 0.9503041 * b,
    )


def xyz_to_lab(xyz):
    # D65 white point.
    xn, yn, zn = 0.95047, 1.0, 1.08883
    def f(t):
        return t ** (1 / 3) if t > 216 / 24389 else (841 / 108) * t + 4 / 29
    fx, fy, fz = f(xyz[0] / xn), f(xyz[1] / yn), f(xyz[2] / zn)
    return (116 * fy - 16, 500 * (fx - fy), 200 * (fy - fz))


def lab(rgb):
    return xyz_to_lab(rgb_to_xyz(rgb))


# Viénot, Brettel & Mollon (1999) simulation matrices, applied in linear RGB.
DICHROMAT = {
    "protanopia": ((0.0, 2.02344, -2.52581), (0.0, 1.0, 0.0), (0.0, 0.0, 1.0)),
    "deuteranopia": ((1.0, 0.0, 0.0), (0.494207, 0.0, 1.24827), (0.0, 0.0, 1.0)),
    "tritanopia": ((1.0, 0.0, 0.0), (0.0, 1.0, 0.0), (-0.395913, 0.801109, 0.0)),
}


def simulate(rgb, kind):
    """What a dichromat sees, back in sRGB."""
    r, g, b = (to_linear(c) for c in rgb)
    m = DICHROMAT[kind]
    if kind == "protanopia":
        r2 = m[0][1] * g + m[0][2] * b
        g2, b2 = g, b
    elif kind == "deuteranopia":
        g2 = m[1][0] * r + m[1][2] * b
        r2, b2 = r, b
    else:
        b2 = m[2][0] * r + m[2][1] * g
        r2, g2 = r, g
    return tuple(to_srgb(c) for c in (r2, g2, b2))


def ciede2000(l1, l2):
    """The full CIEDE2000 difference between two Lab colours."""
    (L1, a1, b1), (L2, a2, b2) = l1, l2
    kL = kC = kH = 1.0
    C1, C2 = math.hypot(a1, b1), math.hypot(a2, b2)
    Cb = (C1 + C2) / 2
    G = 0.5 * (1 - math.sqrt(Cb ** 7 / (Cb ** 7 + 25 ** 7))) if Cb > 0 else 0.5
    a1p, a2p = (1 + G) * a1, (1 + G) * a2
    C1p, C2p = math.hypot(a1p, b1), math.hypot(a2p, b2)
    h1p = math.degrees(math.atan2(b1, a1p)) % 360 if (a1p or b1) else 0.0
    h2p = math.degrees(math.atan2(b2, a2p)) % 360 if (a2p or b2) else 0.0

    dLp = L2 - L1
    dCp = C2p - C1p
    if C1p * C2p == 0:
        dhp = 0.0
    elif abs(h2p - h1p) <= 180:
        dhp = h2p - h1p
    elif h2p - h1p > 180:
        dhp = h2p - h1p - 360
    else:
        dhp = h2p - h1p + 360
    dHp = 2 * math.sqrt(C1p * C2p) * math.sin(math.radians(dhp) / 2)

    Lbp = (L1 + L2) / 2
    Cbp = (C1p + C2p) / 2
    if C1p * C2p == 0:
        hbp = h1p + h2p
    elif abs(h1p - h2p) <= 180:
        hbp = (h1p + h2p) / 2
    elif h1p + h2p < 360:
        hbp = (h1p + h2p + 360) / 2
    else:
        hbp = (h1p + h2p - 360) / 2

    T = (1 - 0.17 * math.cos(math.radians(hbp - 30))
         + 0.24 * math.cos(math.radians(2 * hbp))
         + 0.32 * math.cos(math.radians(3 * hbp + 6))
         - 0.20 * math.cos(math.radians(4 * hbp - 63)))
    dTheta = 30 * math.exp(-(((hbp - 275) / 25) ** 2))
    Rc = 2 * math.sqrt(Cbp ** 7 / (Cbp ** 7 + 25 ** 7)) if Cbp > 0 else 0.0
    Sl = 1 + (0.015 * (Lbp - 50) ** 2) / math.sqrt(20 + (Lbp - 50) ** 2)
    Sc = 1 + 0.045 * Cbp
    Sh = 1 + 0.015 * Cbp * T
    Rt = -math.sin(math.radians(2 * dTheta)) * Rc

    return math.sqrt(
        (dLp / (kL * Sl)) ** 2
        + (dCp / (kC * Sc)) ** 2
        + (dHp / (kH * Sh)) ** 2
        + Rt * (dCp / (kC * Sc)) * (dHp / (kH * Sh))
    )


def worst(c1, c2):
    """The pair's distance under normal vision and each dichromacy."""
    out = {"normal": ciede2000(lab(c1), lab(c2))}
    for kind in DICHROMAT:
        out[kind] = ciede2000(lab(simulate(c1, kind)), lab(simulate(c2, kind)))
    return out


# The sets this app actually asks a reader to tell apart.
SETS = {
    "tones": ["--go", "--caution", "--stop"],
    # The chart draws each trace in its own labelled panel, so only these
    # two are ever asked to be told apart: the effort and the heart rate.
    "chart": ["--accent", "--hard"],
    "grades": ["--easy", "--ink-2", "--caution", "--stop"],
}


def read_css(path):
    """Every custom property, per theme block. The first :root is light; the
    prefers-color-scheme block is dark."""
    text = open(path).read()
    blocks = re.split(r"@media\s*\(prefers-color-scheme:dark\)", text)
    themes = {}
    for name, chunk in (("light", blocks[0]), ("dark", blocks[1] if len(blocks) > 1 else "")):
        vals = {}
        for m in re.finditer(r"(--[a-z0-9-]+)\s*:\s*(#[0-9A-Fa-f]{3,6})", chunk):
            vals.setdefault(m.group(1), m.group(2))
        themes[name] = vals
    return themes


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("colours", nargs="*", help="hex colours to compare")
    ap.add_argument("--css", help="stylesheet to read custom properties from")
    ap.add_argument("--set", dest="sets", action="append",
                    help="a named set from the stylesheet: " + ", ".join(SETS))
    args = ap.parse_args()

    jobs = []
    if args.colours:
        jobs.append(("given", {c: parse_hex(c) for c in args.colours}))
    if args.css:
        themes = read_css(args.css)
        for theme, vals in themes.items():
            for name in (args.sets or list(SETS)):
                want = SETS[name]
                missing = [t for t in want if t not in vals]
                if missing:
                    print(f"{theme}/{name}: missing {', '.join(missing)}", file=sys.stderr)
                    continue
                jobs.append((f"{theme}/{name}", {t: parse_hex(vals[t]) for t in want}))

    failed = False
    for label, colours in jobs:
        print(f"\n{label}")
        names = list(colours)
        for i in range(len(names)):
            for j in range(i + 1, len(names)):
                d = worst(colours[names[i]], colours[names[j]])
                low = min(d.values())
                where = min(d, key=d.get)
                verdict = "ok  "
                if low < FAIL:
                    verdict, failed = "FAIL", True
                elif low < WARN:
                    verdict = "warn"
                print(f"  {verdict}  {names[i]:>10} / {names[j]:<10} "
                      f"worst dE2000 {low:5.1f} ({where})  normal {d['normal']:5.1f}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
