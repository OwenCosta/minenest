"""Draws MineNest's icon (the same 16x16 pixel art as internal/web/static/icon.svg) into
assets/minenest.ico (and .png) at 16, 32, 48, 64, 128 and 256 pixels. Run it again after changing the
art: python scripts/make-icon.py"""
import os
import re

from PIL import Image

here = os.path.dirname(os.path.abspath(__file__))
svg = open(os.path.join(here, "..", "internal", "web", "static", "icon.svg"), encoding="utf-8").read()
art = Image.new("RGBA", (16, 16), (0, 0, 0, 0))
for x, y, w, h, colour in re.findall(r'<rect x="(\d+)" y="(\d+)" width="(\d+)" height="(\d+)" fill="#([0-9a-f]{6})"', svg):
    rgb = tuple(int(colour[i:i + 2], 16) for i in (0, 2, 4))
    for px in range(int(x), int(x) + int(w)):
        for py in range(int(y), int(y) + int(h)):
            art.putpixel((px, py), rgb + (255,))
sizes = [16, 32, 48, 64, 128, 256]
big = art.resize((256, 256), Image.NEAREST)
out = os.path.join(here, "..", "assets", "minenest.ico")
big.save(out, sizes=[(s, s) for s in sizes])
big.save(os.path.join(here, "..", "assets", "minenest.png"))
print("wrote", out)
