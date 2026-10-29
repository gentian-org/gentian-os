#!/usr/bin/env python3
"""Render the platform's tile glyphs to PNG, and write them where they are read.

    render-tiles.py            # rewrite the catalogue and the chart's tiles
    render-tiles.py --check    # exit 1 when either differs from its source
    render-tiles.py --convert FILE...
                               # replace every SVG data URI in FILE with its PNG

A tile is a PNG wherever it is read: the glyph catalogue the operator embeds
(internal/tiles/catalogue.json), the tiles of the components the chart ships
(charts/gentian-os/templates/componentprofile-*.yaml), and a profile's
tile.logo. An SVG is only ever the template a PNG is rendered from, here and
before commit; no browser draws one as a tile and nothing serves one.

The templates are internal/tiles/glyphs/<id>.svg, one per catalogue entry.
Each is rendered at 128x128 and inlined as data:image/png;base64,...

--convert is for files that carry an SVG tile inline and no template of their
own, such as a profile bundle copied into test data: each
data:image/svg+xml;base64,... in the file is rendered and replaced in place.

Needs cairosvg (pip install cairosvg), which needs the cairo library.
"""

from __future__ import annotations

import argparse
import base64
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
TILES = ROOT / "internal" / "tiles"
CATALOGUE = TILES / "catalogue.json"
GLYPHS = TILES / "glyphs"
TEMPLATES = ROOT / "charts" / "gentian-os" / "templates"

SIZE = 128
PNG_PREFIX = "data:image/png;base64,"
SVG_DATA_URI = re.compile(r"data:image/svg\+xml;base64,([A-Za-z0-9+/]+=*)")

# The chart's components whose tile is a catalog glyph, and which glyph.
CHART_TILES = {
    "componentprofile-admin-console.yaml": "settings",
}

# A tile's logo line in a profile: the key, then the data URI, on one line.
LOGO_LINE = re.compile(r"^(\s*logo: )data:image/[a-z+.-]+;base64,[A-Za-z0-9+/]+=*\s*$", re.M)


def render(svg: bytes) -> str:
    """The PNG data URI of one SVG, square at SIZE."""
    import cairosvg

    png = cairosvg.svg2png(bytestring=svg, output_width=SIZE, output_height=SIZE)
    return PNG_PREFIX + base64.b64encode(png).decode()


def catalogue_text() -> tuple[str, dict[str, str]]:
    cat = json.loads(CATALOGUE.read_text())
    uris: dict[str, str] = {}
    for tile_id, tile in cat["tiles"].items():
        source = GLYPHS / tile["glyph"]
        if not source.is_file():
            sys.exit(f"{tile_id}: no template at {source.relative_to(ROOT)}")
        tile["dataUri"] = uris[tile_id] = render(source.read_bytes())
    cat["frame"]["pixels"] = SIZE
    return json.dumps(cat, indent=2, ensure_ascii=False) + "\n", uris


def chart_text(name: str, uri: str) -> str:
    path = TEMPLATES / name
    text = path.read_text()
    new, count = LOGO_LINE.subn(lambda m: m.group(1) + uri, text)
    if count != 1:
        sys.exit(f"{path.relative_to(ROOT)}: expected one tile logo line, found {count}")
    return new


def convert(path: Path) -> bool:
    text = path.read_text()
    new = SVG_DATA_URI.sub(lambda m: render(base64.b64decode(m.group(1))), text)
    if new != text:
        path.write_text(new)
        return True
    return False


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--check", action="store_true", help="report drift instead of writing")
    ap.add_argument("--convert", nargs="+", type=Path, metavar="FILE", help="replace SVG data URIs in FILE")
    args = ap.parse_args()

    if args.convert:
        for path in args.convert:
            if convert(path):
                print(f"converted {path}")
        return 0

    catalogue, uris = catalogue_text()
    outputs = {CATALOGUE: catalogue}
    for name, glyph in CHART_TILES.items():
        outputs[TEMPLATES / name] = chart_text(name, uris[glyph])

    stale = [p for p, text in outputs.items() if p.read_text() != text]
    if args.check:
        for p in stale:
            print(f"{p.relative_to(ROOT)} is not what scripts/tools/render-tiles.py renders", file=sys.stderr)
        return 1 if stale else 0
    for p in stale:
        p.write_text(outputs[p])
        print(f"wrote {p.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
