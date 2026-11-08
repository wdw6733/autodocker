"""从透明图标生成网页版要用的 favicon。

    python tools/make-favicon.py

产出（都写进 `web/`，靠 `web/embed.go` 的 go:embed 编进二进制）：
- `web/favicon.ico`   —— 16 / 32 / 48 三个尺寸打包在一个文件里，
  浏览器不写 `<link rel="icon">` 时也会自动请求 `/favicon.ico`，这个文件是兜底主力；
- `web/icon-192.png`  —— 192×192，给 `apple-touch-icon`（iOS 加到桌面）与高分辨率标签页。

**用透明底**，不要白底 —— favicon 会显示在浏览器的深色/浅色两种标签栏上，
白底会像桌面图标那样露出一圈白边。
"""

import os
import sys

from PIL import Image

SRC = 'fnos/ICON_256.PNG'
ICO_SIZES = [(16, 16), (32, 32), (48, 48)]
PNG_SIZE = 192


def main():
    if not os.path.isfile(SRC):
        raise SystemExit(f'找不到源图 {SRC}，先跑 tools/icon-transparent.py')
    im = Image.open(SRC)
    if im.mode != 'RGBA':
        raise SystemExit(f'{SRC} 没有透明通道（mode={im.mode}），先跑 tools/icon-transparent.py')

    im.save('web/favicon.ico', sizes=ICO_SIZES)
    im.resize((PNG_SIZE, PNG_SIZE), Image.LANCZOS).save('web/icon-192.png')

    for f in ('web/favicon.ico', 'web/icon-192.png'):
        print(f'{f}  {os.path.getsize(f)} bytes')
    # 自检：把 ico 再读回来，确认确实带了三个尺寸、且角是透明的
    back = Image.open('web/favicon.ico')
    print('ico 内尺寸:', sorted(back.info.get('sizes', [])))
    back = Image.open('web/icon-192.png').convert('RGBA')
    print('192 四角 α:', [back.getpixel(p)[3] for p in ((0, 0), (back.width - 1, 0), (0, back.height - 1), (back.width - 1, back.height - 1))])


if __name__ == '__main__':
    sys.exit(main())
