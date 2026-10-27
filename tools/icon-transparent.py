"""把「白底方形图标」转成四角透明的 RGBA 图标。

## 为什么需要这个脚本

fpk 里的 `ICON.PNG` / `ICON_256.PNG` / `ui/images/icon_{64,256}.png` 是从设计稿导出的
**RGB 白底图**（没有 alpha 通道）：蓝色圆角方块是满画布的，方块之外全是纯白。
飞牛桌面不做圆角遮罩，直接把 PNG 铺在壁纸上 —— 于是四个角就是四块白边。

## 做法（纯像素驱动，不需要知道圆角半径）

图里只有「蓝填充」和「白」两种成分，所以观测色满足混合方程
`C = α·F + (1-α)·255`，其中 `F` 是填充色。据此可以反解每个像素的覆盖率 α：

1. 取所有偏蓝像素的中位色作为填充色 `F`（方块内部可能有一点渐变，取中位更稳）；
2. 用动态范围最大的通道反解 α（蓝 `R=45`，动态范围 210，比 G/B 都可靠）；
3. `α > 0.5` 二值化成形状 → **填洞**，把方块内部的白色箭头补回形状里
   （否则箭头会被当成背景抠掉）；
4. 形状内**贴着外轮廓的那一圈**才是抗锯齿边 → 按 α 做反混合（un-matte），
   把混进去的白还原掉，颜色改成 `(C - (1-α)·255)/α`；
   形状内的其它像素（箭头本体 + 箭头自己的抗锯齿边）一律保持不透明、颜色原样；
5. 形状外 α = 0，颜色写成填充色 —— 万一被系统降采样，也不会渗出白边。

## 用法

    python tools/icon-transparent.py fnos/ICON_256.PNG fnos/app/ui/images/icon_64.png
    python tools/icon-transparent.py --preview out.png fnos/ICON_256.PNG   # 只看效果，不写回

注意：本脚本**原地覆盖**输入文件，跑之前先备份。
"""

import argparse
import os
import sys

import numpy as np
from PIL import Image, ImageDraw, ImageFilter

# 动态范围小于这个值的通道不参与 α 反解（参考白 255 减填充色，B 通道只剩 23，噪声大）
MIN_CHANNEL_RANGE = 40
# α 小于这个值的像素颜色不可靠，直接写填充色（反正几乎看不见）
UNMATTE_MIN_ALPHA = 0.15
# 外轮廓一圈的宽度（像素）：轮廓外的像素往形状内膨胀这么多，算作抗锯齿边
EDGE_BAND = 3


def solve_fill(rgb):
    """取偏蓝像素的中位色作为填充色。"""
    R = rgb[:, :, 0]
    blue = R < 200
    if blue.sum() < 100:
        raise SystemExit('找不到足够的非白像素，这张图可能不是白底图标')
    return np.median(rgb[blue].reshape(-1, 3).astype(np.float64), axis=0)


def solve_coverage(rgb, fill):
    """按混合方程反解每像素的覆盖率 α。"""
    C = rgb.astype(np.float64)
    denom = 255.0 - fill
    use = denom > MIN_CHANNEL_RANGE
    if not use.any():
        raise SystemExit('填充色太浅，反解不出 α')
    a = (255.0 - C[:, :, use]) / denom[use]
    return np.clip(np.median(a, axis=2), 0.0, 1.0)


def fill_holes(shape):
    """把被形状包住的空洞（内部的白色箭头）并回形状。

    从**整圈边框**灌水，灌不到的地方就是洞。用整圈而不是只取四个角：
    这张图的形状贴着画布四边中点，外部区域是**互不相连的四块**，
    只从左上角灌水的话另外三个角会被误判成洞。
    """
    h, w = shape.shape
    outside = np.where(shape, 0, 255).astype(np.uint8)
    # 必须 .copy()：Image.fromarray() 给出的是只读图像，
    # ImageDraw.floodfill 在只读图像上**静默失效**（不报错、也不改一个像素）。
    im = Image.fromarray(outside, 'L').copy()
    seeds = [(x, 0) for x in range(w)] + [(x, h - 1) for x in range(w)]
    seeds += [(0, y) for y in range(h)] + [(w - 1, y) for y in range(h)]
    for seed in seeds:
        if im.getpixel(seed) == 255:
            ImageDraw.floodfill(im, seed, 128)
    arr = np.array(im)
    holes = arr == 255  # 没被灌到 ⇒ 被形状包住
    return shape | holes


def dilate(mask, k):
    """二值膨胀 k 像素。"""
    im = Image.fromarray((mask * 255).astype(np.uint8), 'L')
    im = im.filter(ImageFilter.MaxFilter(2 * k + 1))
    return np.array(im) > 127


def convert(path, preview=None, band=EDGE_BAND):
    im = Image.open(path)
    rgb = np.asarray(im.convert('RGB'))
    h, w, _ = rgb.shape

    fill = solve_fill(rgb)
    alpha_raw = solve_coverage(rgb, fill)
    shape = fill_holes(alpha_raw > 0.5)

    outside = ~shape
    outside_dil = dilate(outside, band)
    boundary = shape & outside_dil
    interior = shape & ~outside_dil

    alpha = np.zeros((h, w), dtype=np.float64)
    out = rgb.astype(np.float64).copy()
    out[outside] = fill  # 透明区写成填充色，防止降采样时渗出白边

    # 形状内部（箭头 + 蓝底）：完全不透明，颜色原样
    alpha[interior] = 1.0

    # 轮廓一圈：按覆盖率反混合，把混进去的白还原掉
    ys, xs = np.nonzero(boundary)
    if len(ys):
        a = alpha_raw[ys, xs]
        a = np.where(a >= 0.995, 1.0, a)
        alpha[ys, xs] = a
        solid = (rgb[ys, xs].astype(np.float64) - (1 - a)[:, None] * 255.0) / np.maximum(a, 1e-6)[:, None]
        solid = np.clip(solid, 0, 255)
        solid[a < UNMATTE_MIN_ALPHA] = fill  # α 太小，颜色不可信
        out[ys, xs] = solid

    alpha[alpha < 0.02] = 0.0

    rgba = np.dstack([out, alpha * 255.0]).round().astype(np.uint8)
    # 去色偏：形状内本来是纯白（255,255,255）的像素不能被当成"透明"处理；
    # 这里只做一次自检 —— 形状内 α<0.5 的像素不该存在（那说明填洞漏了）。
    stray = int((alpha < 0.5).sum() - (~shape).sum())
    report = {
        'path': path,
        'size': f'{w}x{h}',
        'fill': tuple(int(v) for v in fill),
        'opaque': int((alpha >= 0.995).sum()),
        'soft_edge': int(((alpha > 0.02) & (alpha < 0.995)).sum()),
        'transparent': int((alpha <= 0.02).sum()),
        'corners': [round(float(alpha[y, x]), 3) for x, y in ((0, 0), (w - 1, 0), (0, h - 1), (w - 1, h - 1))],
        'stray': stray,
    }

    if preview:
        # 拼一张对照图：原图 / 棋盘格上 / 深色底上
        dark = Image.new('RGBA', (w, h), (18, 22, 34, 255))
        cb = checker(w, h)
        dark.alpha_composite(Image.fromarray(rgba, 'RGBA'))
        cb.alpha_composite(Image.fromarray(rgba, 'RGBA'))
        sheet = Image.new('RGB', (w * 3 + 24, h + 16), (240, 240, 244))
        sheet.paste(im.convert('RGB'), (8, 8))
        sheet.paste(cb.convert('RGB'), (w + 16, 8))
        sheet.paste(dark.convert('RGB'), (w * 2 + 24, 8))
        sheet.save(preview)
        report['preview'] = preview
    else:
        Image.fromarray(rgba, 'RGBA').save(path)
    return report


def checker(w, h, cell=8):
    """棋盘格底，用来看透明区。"""
    im = Image.new('RGBA', (w, h), (255, 255, 255, 255))
    d = ImageDraw.Draw(im)
    for y in range(0, h, cell):
        for x in range(0, w, cell):
            if (x // cell + y // cell) % 2:
                d.rectangle([x, y, x + cell - 1, y + cell - 1], fill=(205, 208, 214, 255))
    return im


def main():
    ap = argparse.ArgumentParser(description='把白底方形图标转成四角透明的 RGBA 图标')
    ap.add_argument('files', nargs='+')
    ap.add_argument('--preview', metavar='OUT.png', help='只出对照图，不写回原文件')
    ap.add_argument('--band', type=int, default=EDGE_BAND, help='抗锯齿边宽度（像素）')
    args = ap.parse_args()

    for f in args.files:
        if not os.path.isfile(f):
            print(f'!! 文件不存在: {f}', file=sys.stderr)
            continue
        r = convert(f, preview=args.preview, band=args.band)
        print(f"== {r['path']}  {r['size']}")
        print(f"   填充色 {r['fill']}   不透明 {r['opaque']}  半透明边 {r['soft_edge']}  透明 {r['transparent']}")
        print(f"   四角 α = {r['corners']}   形状内漏抠像素 {r['stray']}")
        if r.get('preview'):
            print(f"   对照图 -> {r['preview']}")


if __name__ == '__main__':
    main()
