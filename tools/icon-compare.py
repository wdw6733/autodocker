"""出「改透明前 / 改透明后」对照图，背景画成飞牛桌面那种橙色渐变（不用真壁纸）。

    python tools/icon-compare.py out.png [改前的原图]

改前的原图不传就用 `%TEMP%/autodocker-icon/orig/fnos_ICON_256.PNG`
（重做图标前先把原图备份到那儿）。
"""

import os
import sys

from PIL import Image, ImageDraw, ImageFont

DEFAULT_ORIG = os.path.join(os.environ.get('TEMP', '.'), 'autodocker-icon', 'orig', 'fnos_ICON_256.PNG')
NEW = 'fnos/ICON_256.PNG'
SIZE = 128  # 模拟桌面上的显示尺寸
SCALE = 3   # 放大倍数，方便看边缘
FONT = 'C:/Windows/Fonts/msyh.ttc'  # 默认位图字体画不了中文


def label_font(size):
    for p in (FONT, 'C:/Windows/Fonts/simhei.ttf'):
        if os.path.isfile(p):
            return ImageFont.truetype(p, size)
    return ImageFont.load_default()


def wallpaper(w, h):
    """橙色斜向渐变，模拟飞牛默认壁纸的色调。"""
    im = Image.new('RGB', (w, h))
    px = im.load()
    for y in range(h):
        for x in range(w):
            t = (x / w * 0.45 + y / h * 0.55)
            r = int(232 - 60 * t)
            g = int(120 - 30 * t)
            b = int(48 + 20 * t)
            px[x, y] = (r, g, b)
    return im


def main(out, orig):
    cell = SIZE * SCALE
    sheet = Image.new('RGB', (cell * 2 + 60, cell + 80), (245, 246, 248))
    pairs = ((orig, '改前 · 四角是白底'), (NEW, '改后 · 四角透明'))
    for i, (path, _) in enumerate(pairs):
        icon = Image.open(path).convert('RGBA').resize((cell, cell), Image.LANCZOS)
        bg = wallpaper(cell, cell)
        bg.paste(icon, (0, 0), icon)  # 用 alpha 当蒙版贴上去
        sheet.paste(bg, (20 + i * (cell + 20), 20))
    d = ImageDraw.Draw(sheet)
    f = label_font(30)
    for i, (_, tag) in enumerate(pairs):
        d.text((20 + i * (cell + 20), cell + 34), tag, fill=(40, 44, 52), font=f)
    sheet.save(out)
    print('对照图 ->', out)


if __name__ == '__main__':
    main(sys.argv[1] if len(sys.argv) > 1 else 'compare.png',
         sys.argv[2] if len(sys.argv) > 2 else DEFAULT_ORIG)
