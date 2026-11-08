"""纯 Python 读 PNG：报尺寸 / 色彩类型 / 四角像素 / 是否含透明通道。

不依赖 Pillow —— 本机没装。只解析到 IHDR + 首行扫描线即可判断四角。
"""
import struct
import sys
import zlib


def read_png(path):
    data = open(path, 'rb').read()
    assert data[:8] == b'\x89PNG\r\n\x1a\n', '不是 PNG'
    pos = 8
    idat = b''
    width = height = depth = ctype = None
    plte = None
    trns = None
    while pos < len(data):
        (length,) = struct.unpack('>I', data[pos:pos + 4])
        ctag = data[pos + 4:pos + 8]
        body = data[pos + 8:pos + 8 + length]
        if ctag == b'IHDR':
            width, height, depth, ctype, comp, filt, interlace = struct.unpack('>IIBBBBB', body)
            assert interlace == 0, '不支持隔行扫描'
        elif ctag == b'IDAT':
            idat += body
        elif ctag == b'PLTE':
            plte = body
        elif ctag == b'tRNS':
            trns = body
        elif ctag == b'IEND':
            break
        pos += 12 + length
    return {
        'w': width, 'h': height, 'depth': depth, 'ctype': ctype,
        'raw': zlib.decompress(idat), 'plte': plte, 'trns': trns,
    }


CHANNELS = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}


def rows(png):
    """返回每行像素列表（已反过滤），每像素是 RGBA 或调色板索引。"""
    w, h = png['w'], png['h']
    ch = CHANNELS[png['ctype']]
    bpp = max(1, ch * png['depth'] // 8)
    stride = (w * ch * png['depth'] + 7) // 8
    raw = png['raw']
    prev = bytearray(stride)
    out = []
    off = 0
    for _ in range(h):
        ft = raw[off]
        off += 1
        line = bytearray(raw[off:off + stride])
        off += stride
        if ft == 1:
            for i in range(bpp, stride):
                line[i] = (line[i] + line[i - bpp]) & 0xFF
        elif ft == 2:
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif ft == 3:
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                line[i] = (line[i] + ((a + prev[i]) >> 1)) & 0xFF
        elif ft == 4:
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                b = prev[i]
                c = prev[i - bpp] if i >= bpp else 0
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                pr = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pr) & 0xFF
        prev = line
        out.append(line)
    return out


def pixel(png, lines, x, y):
    """取 (x, y) 的 RGBA（调色板按 PLTE/tRNS 展开）。"""
    w = png['w']
    ct = png['ctype']
    if ct == 3:
        idx = lines[y][x]
        p = png['plte']
        r, g, b = p[idx * 3], p[idx * 3 + 1], p[idx * 3 + 2]
        a = png['trns'][idx] if png['trns'] and idx < len(png['trns']) else 255
        return (r, g, b, a)
    ch = CHANNELS[ct]
    px = lines[y][x * ch:(x + 1) * ch]
    if ct == 0:
        return (px[0], px[0], px[0], 255)
    if ct == 2:
        return (px[0], px[1], px[2], 255)
    if ct == 4:
        return (px[0], px[0], px[0], px[1])
    return (px[0], px[1], px[2], px[3])


BLURB = {0: '灰度', 2: '真彩 RGB', 3: '调色板', 4: '灰度+Alpha', 6: '真彩 RGBA'}

for path in sys.argv[1:]:
    png = read_png(path)
    lines = rows(png)
    w, h = png['w'], png['h']
    print(f'== {path}')
    print(f'   尺寸 {w}x{h}  位深 {png["depth"]}  色彩类型 {png["ctype"]} ({BLURB.get(png["ctype"], "?")})')
    has_alpha = png['ctype'] in (4, 6) or bool(png['trns'])
    print(f'   含透明通道: {has_alpha}')
    corners = {'左上': (0, 0), '右上': (w - 1, 0), '左下': (0, h - 1), '右下': (w - 1, h - 1)}
    for name, (x, y) in corners.items():
        print(f'   {name} 角像素 RGBA = {pixel(png, lines, x, y)}')
    # 沿上边缘扫一遍，看有多少个不透明像素（用来判断白边多宽）
    opaque_top = 0
    for x in range(w):
        r, g, b, a = pixel(png, lines, x, 0)
        if a > 8:
            opaque_top += 1
    print(f'   最上一行不透明像素数: {opaque_top}/{w}')
    opaque_left = sum(1 for y in range(h) if pixel(png, lines, 0, y)[3] > 8)
    print(f'   最左一列不透明像素数: {opaque_left}/{h}')
