# Regenerates the logo files from JetBrains Mono (github.com/JetBrains/JetBrainsMono).
# uv run --with fonttools python docs/assets/build.py <dir with JetBrainsMono-*.ttf> docs/assets
# then rsvg-convert -w <width> <name>.svg -o <name>.png
import sys, os
from fontTools.ttLib import TTFont
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen

FONTDIR, OUT = sys.argv[1], sys.argv[2]
BG, CARD, EDGE = "#170E0D", "#241513", "#3A2420"
RED, WHITE, SOFT, MUTED, GRAY = "#E34234", "#FFFFFF", "#E2D2CD", "#9C8580", "#6E5A56"
fonts = {w: TTFont(os.path.join(FONTDIR, f"JetBrainsMono-{w}.ttf")) for w in ("ExtraBold", "Medium", "Regular")}

def text(s, x, y, size, weight, color, spacing=0.0, anchor="start"):
    f = fonts[weight]; gs = f.getGlyphSet(); cmap = f.getBestCmap(); k = size / f["head"].unitsPerEm
    if anchor == "end":
        x -= sum(gs[cmap[ord(c)]].width * k + spacing for c in s)
    out, cx = [], x
    for ch in s:
        g = cmap[ord(ch)]; pen = SVGPathPen(gs)
        gs[g].draw(TransformPen(pen, (k, 0, 0, -k, cx, y)))
        if pen.getCommands(): out.append(pen.getCommands())
        cx += gs[g].width * k + spacing
    return f'<path fill="{color}" d="{" ".join(out)}"/>', cx

CRYSTAL = '''<polygon points="64,8 28,36 64,48" fill="#FF7A5C"/>
<polygon points="64,8 64,48 100,36" fill="#F0533C"/>
<polygon points="28,36 64,48 64,104 28,88" fill="#E34234"/>
<polygon points="64,48 100,36 100,88 64,104" fill="#B8281D"/>
<polygon points="28,88 64,104 64,120" fill="#9E2118"/>
<polygon points="64,104 100,88 64,120" fill="#7A1610"/>
<polyline points="28,36 64,48 100,36" fill="none" stroke="#FFD2C4" stroke-width="1.5" stroke-opacity=".55" stroke-linejoin="round"/>
<line x1="64" y1="48" x2="64" y2="104" stroke="#FFD2C4" stroke-width="1.5" stroke-opacity=".35"/>
<path d="M106 10 L108.5 17.5 L116 20 L108.5 22.5 L106 30 L103.5 22.5 L96 20 L103.5 17.5 Z" fill="#FFB39E"/>'''

def crystal(x, y, s, flat=None):
    body = CRYSTAL if flat is None else f'<g fill="{flat}">' + "".join(
        l.replace(l[l.index(' fill="'):l.index('"', l.index(' fill="') + 7) + 1], "")
        for l in CRYSTAL.splitlines() if l.startswith("<polygon")) + "</g>"
    return f'<g transform="translate({x} {y}) scale({s/128})">{body}</g>'

def svg(w, h, body, title):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" viewBox="0 0 {w} {h}" role="img">'
            f'<title>{title}</title>{body}</svg>\n')

def save(name, content):
    open(os.path.join(OUT, name), "w").write(content)

def word(x, y, size):
    return text("Cinnabar", x, y, size, "ExtraBold", WHITE, spacing=-size * 0.02)[0]

# avatar 400x400, crystal body centred (body spans x 28..100, y 8..120)
save("avatar.svg", svg(400, 400, f'<rect width="400" height="400" fill="{BG}"/>' + crystal(200 - 64 * 2.1, 200 - 64 * 2.1, 128 * 2.1), "Cinnabar"))

# README banner 1280x320
b = f'<rect width="1280" height="320" rx="24" fill="{BG}"/>' + crystal(64, 46, 220)
b += word(320, 168, 104)
b += text("Mémoire hybride pour agents conversationnels.", 326, 228, 28, "Medium", SOFT)[0]
b += text("vectoriel · lexical · graphe · rerank · peut se taire", 326, 268, 21, "Regular", "#FF7A5C")[0]
save("banner.svg", svg(1280, 320, b, "Cinnabar : mémoire hybride pour agents conversationnels"))

# LinkedIn cover 1584x396
W, H = 1584, 396
c = f'<rect width="{W}" height="{H}" fill="{BG}"/>'
c += f'<g opacity="0.06">{crystal(-150, -70, 560, WHITE)}</g>'
c += crystal(452, 40, 150)
c += word(618, 150, 78)
c += text("Mémoire hybride", 476, 222, 27, "Medium", SOFT)[0]
c += text("pour agents conversationnels.", 476, 258, 27, "Medium", SOFT)[0]
c += text("vectoriel · lexical · graphe · rerank", 476, 304, 20, "Regular", "#FF7A5C")[0]
cx, cy, cw, ch = 1040, 50, 492, 296
c += f'<rect x="{cx}" y="{cy}" width="{cw}" height="{ch}" rx="16" fill="{CARD}" stroke="{EDGE}"/>'
c += text("LoCoMo · rappel R@5", cx + 26, cy + 38, 15, "Medium", MUTED)[0]
rows = [("MemPalace, hybride", 78.4, GRAY), ("Cinnabar, sans modèle", 85.0, RED), ("Cinnabar + graphe + rerank", 92.8, RED)]
track = cw - 52 - 84
y = cy + 82
for label, v, col in rows:
    c += text(label, cx + 26, y, 15, "Regular", SOFT)[0]
    by, bh, bw = y + 12, 16, track * v / 100
    c += (f'<path fill="{col}" d="M{cx+26} {by} H{cx+26+bw-4} a4 4 0 0 1 4 4 V{by+bh-4} '
          f'a4 4 0 0 1 -4 4 H{cx+26} Z"/>')
    c += text(f"{v:.1f} %".replace(".", ","), cx + 26 + bw + 12, by + 14, 17, "ExtraBold", WHITE)[0]
    y += 64
c += text("1 986 questions, 10 conversations", cx + 26, cy + ch - 22, 13, "Regular", MUTED)[0]
save("linkedin-cover.svg", svg(W, H, c, "Cinnabar"))
print("ok")
