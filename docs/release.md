# 发布流程

> 返回 [README](../README.md)

一次发版包含两份产物，**版本号必须一致**：

| 产物 | 去处 | 版本号来源 |
| --- | --- | --- |
| Docker 镜像 `ghcr.io/haihaipypy/autodocker` | GitHub Container Registry | `git tag vX.Y.Z` |
| 飞牛应用包 `Autodocker.fpk` | GitHub Releases | `fnos/manifest` 的 `version` |

## 一、改版本号

`fnos/manifest` 三处一起改：

| 位置 | 用途 |
| --- | --- |
| `version` | 飞牛应用中心显示的版本 |
| `changelog` | 更新说明，**单行**，最新的写在最前，旧的往后接 |
| `platform` / `resource` 等 | 与版本无关，一般不动 |

`changelog` 是单行字符串，写法是在同一行里按版本倒序拼接：

```
changelog = 1.2.3 本次…；1.2.2 图标去掉白色底…；1.2.1 界面重做…
```

同时把 README 与 docs 里出现的旧版本号（镜像标签示例等）一并更新。

## 二、本地校验

```bash
go test ./... && go vet ./... && gofmt -l .
node --check web/app.js
```

`gofmt -l .` 应该什么都不输出。

## 三、打飞牛应用包

```bash
rm -f fnos/app/bin/autodocker      # 必做，见下方说明
./build-fpk.sh                     # x86
./build-fpk.sh arm64               # ARM
```

> **编译前先删旧二进制。** `go build -o` 在目标文件已存在且 build ID 相同时会跳过写入，
> 结果打出来的包还是上一版的程序。

产物是项目根目录的 `Autodocker.fpk`。解包核对一遍：

```bash
tar -tzf Autodocker.fpk | head -30
```

`.fpk` **不进 git**（体积 6 MB 左右），只作为 Release 附件。

## 四、推送代码

```bash
git add -A
git commit -m "release: v1.2.3"
git push origin main
```

先推 main，确认 Actions 里的 `Build & Push Image` 跑通了，再打 tag。
反过来的话 `latest` 会先指向一个还没验证过的版本。

## 五、打 tag 触发镜像构建

```bash
git tag v1.2.3
git push origin v1.2.3
```

| 触发 | 产物标签 |
| --- | --- |
| push 到 `main` | `main`、`sha-xxxxxxx` |
| push tag `v1.2.3` | `1.2.3`、`1.2`、**`latest`**、`sha-xxxxxxx` |
| Pull Request | 只构建校验，不推送 |
| Actions 页面手动触发 | 同 main |

> `latest` 只在打 tag 时移动。分支构建不抢这个名字，否则 main 上的半成品会直接
> 成为用户拉到的「最新版」。

会构建 `linux/amd64` + `linux/arm64` 双架构（飞牛有 x86 也有 ARM 机型），
**同时推到 Docker Hub 与 GHCR**：

| 仓库 | 地址 | 说明 |
| --- | --- | --- |
| Docker Hub | `haihaipypy/autodocker` | **默认推荐**，国内快，能吃加速站；匿名拉取按 IP 限流 |
| GHCR | `ghcr.io/haihaipypy/autodocker` | 用仓库自带 `GITHUB_TOKEN` 推，不占 Docker Hub 配额 |

Docker Hub 那一段需要一次性配置两个 secret，见下一节；没配时 workflow 会自动只发 GHCR，
不会让流水线失败。

> GHCR 的包在**公开仓库**下默认就是可匿名拉取的，实测 `docker pull` 不需要额外改可见性。
> 若从包页面看到 private，再去 `Package settings → Change visibility` 改成 Public。

## 六、一次性配置 Docker Hub 发布

1. **建仓库**：登录 [hub.docker.com](https://hub.docker.com) → `Create repository` →
   名称填 `autodocker` → Visibility 选 **Public**。
2. **建 access token**：右上头像 → `Account settings` → `Personal access tokens` →
   `Generate new token` → 描述写 `github-actions` → 权限选 **Read & Write** → 复制（只显示一次）。
   **不要用登录密码**，密码在 CI 里既不能用也不安全。
3. **加 secrets**：GitHub 仓库 → `Settings` → `Secrets and variables` → `Actions` →
   `New repository secret`，加两条：

   | Name | Value |
   | --- | --- |
   | `DOCKERHUB_USERNAME` | 你的 Docker Hub 用户名（如 `haihaipypy`） |
   | `DOCKERHUB_TOKEN` | 上一步复制的 token |

4. 推一次 tag 验证：Docker Hub 仓库页的 `Tags` 里应该出现 `1.2.3`、`1.2`、`latest`、
   `main`、`sha-xxxxxxx`。

> 镜像名由 workflow 自动拼成 `<DOCKERHUB_USERNAME>/autodocker`，
> 换用户名只改 secret，不用动代码。
> 若 Docker Hub 账号属于某个组织，把 secret 填成 `组织名`，并确认该账号有推送权限。

## 七、建 Release 并上传 fpk

```bash
gh release create v1.2.3 Autodocker.fpk \
  --title "v1.2.3" \
  --notes-file /tmp/notes.md
```

或者网页操作：仓库 → Releases → Draft a new release → 选 tag `v1.2.3` →
把 `Autodocker.fpk` 拖进去。

Release 说明按「新增 / 改进 / 修复」三段写，把 `manifest` 的 changelog 展开成可读条目。

### 用 API 上传时注意两件事

**1. 附件必须发原始字节，不要自己拼 multipart。**

GitHub 的 uploads 接口支持直接 POST 二进制：

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-binary "@Autodocker.fpk" \
  "https://uploads.github.com/repos/haihaipypy/autodocker/releases/$REL_ID/assets?name=Autodocker.fpk"
```

手写 multipart 时只要有一处格式不被接受，GitHub **不报错**，而是把整个请求体
（含 boundary 与表单头）当作文件内容原样存下 —— 附件大小会多出几百字节，
下载回来 head 一看开头是 `------boundary` 而不是 `1f8b`（gzip 魔数）。

**2. 换过同名附件后，下载要先绕开 CDN 缓存。**

同名 asset 删除后重传，`releases/download/...` 这个地址会继续返回**旧文件**，
哈希与刚上传的完全不匹配。加个时间戳参数即可拿到新内容：

```bash
curl -sL -o dl.fpk "https://github.com/OWNER/REPO/releases/download/v1.2.3/Autodocker.fpk?t=$(date +%s)"
```

验完再决定要不要报警，别被缓存骗成「上传失败」。

## 八、发版前 checklist

- [ ] `fnos/manifest` 的 `version` 与即将打的 tag 一致
- [ ] `changelog` 已加本次条目，写的是用户能感知的变化
- [ ] 删过 `fnos/app/bin/autodocker` 再打包
- [ ] `go test ./...` 与 `go vet ./...` 通过
- [ ] main 推送后 Actions 通过
- [ ] tag 推送后双架构镜像在 **Docker Hub 与 GHCR** 都出现，且 `latest` 已移动
- [ ] Release 附上了 `Autodocker.fpk`，且解包能核对到新版本号
- [ ] 下载附件比对 sha256 与本地一致（不一致先加 `?t=<时间戳>` 排掉 CDN 缓存）
