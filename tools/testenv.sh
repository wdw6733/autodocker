#!/usr/bin/env bash
#
# 方案 B 测试环境：本地 registry + 可反复「发新版」的测试容器。
#
# 用途：在装了 Docker 的机器（如飞牛 NAS）上验证 Autodocker 的完整链路
#   —— 检测到远端 digest 变化 → 推飞书卡片 → 点按钮 → 重建容器 → 挂载/端口/环境变量零丢失。
#
# 用法（在 NAS 上执行）：
#   bash testenv.sh up      # 起 registry、造 v1 镜像并推送、拉起测试容器
#   bash testenv.sh bump    # 覆盖推送新内容，模拟「上游发了新版」
#   bash testenv.sh show    # 打印测试容器的关键配置（重建前后各跑一次，用来对比）
#   bash testenv.sh diag    # 卡住时跑这个：打印 build/pull 现场、基础镜像、代理、加速地址
#   bash testenv.sh clean   # 清掉容器、网络、卷、registry 与工作目录
#
# 需要 sudo 时：DOCKER="sudo docker" bash testenv.sh up
# 想换端口/名字：REG_PORT=5001 HOST_PORT=18082 bash testenv.sh up
# 拉基础镜像太慢/被墙：BASE_IMAGE=nginx:latest bash testenv.sh up
#   （BASE_IMAGE 决定测试镜像的基础镜像，换成你本机已有的任意一个即可绕开拉取）

set -euo pipefail

DOCKER="${DOCKER:-docker}"
REG_NAME="${REG_NAME:-ad-registry}"
REG_PORT="${REG_PORT:-5000}"
# 用 127.0.0.1 而不是 localhost：Docker 守护进程默认把回环地址当安全源，
# 拉取时不必改 daemon.json。Autodocker 1.1.4 起回环地址也自动走 http。
REG_ADDR="${REG_ADDR:-127.0.0.1:${REG_PORT}}"
IMAGE="${REG_ADDR}/myapp:latest"
CONTAINER="${CONTAINER:-ad-myapp}"
NET="${NET:-ad-test-net}"
VOL="${VOL:-ad-test-log}"
HOST_PORT="${HOST_PORT:-18081}"
WORKDIR="${WORKDIR:-$HOME/ad-test}"
# 基础镜像。默认 nginx:alpine（自带 80 端口静态服务，方便 curl 验证内容变化）。
# 如果 Docker Hub 拉不动，换成 `docker images` 里已有的任意镜像即可 —— 只是拿来当底，
# 关键是上面的 VOLUME /data（验匿名卷保真）和 COPY 进去的 index.html。
BASE_IMAGE="${BASE_IMAGE:-nginx:alpine}"
# 容器内静态文件目录。换基础镜像时可能要跟着改（nginx 系是 /usr/share/nginx/html）。
DOC_ROOT="${DOC_ROOT:-/usr/share/nginx/html}"

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31m!!\033[0m %s\n' "$*" >&2; exit 1; }

ensure_registry() {
    if $DOCKER ps -a --format '{{.Names}}' | grep -qx "$REG_NAME"; then
        running="$($DOCKER inspect -f '{{.State.Running}}' "$REG_NAME" 2>/dev/null || echo false)"
        if [ "$running" != "true" ]; then
            # 停着就启起来。数据在容器的匿名卷里，重启不会丢已有 tag。
            log "registry 容器存在但已停止，重新启动（仓库内容保留）"
            $DOCKER start "$REG_NAME" >/dev/null
        fi
    else
        # 这里不吞输出：registry:2 也要从 Docker Hub 拉，卡住时能看到卡在哪。
        # 注意新容器的匿名卷是空的 —— 之前推过的 tag 全没了，属预期。
        log "本机没有 registry 容器，新建一个（新仓库是空的，需重新推送）"
        $DOCKER run -d --name "$REG_NAME" --restart unless-stopped \
            -p "${REG_PORT}:5000" registry:2
    fi
    for _ in $(seq 1 20); do
        if curl -fsS --max-time 3 "http://${REG_ADDR}/v2/" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.5
    done
    die "registry ${REG_ADDR} 起不来。排查三步：
    $DOCKER ps -a --filter name=${REG_NAME}
    $DOCKER port ${REG_NAME}          # 端口映射没绑到回环地址？
    $0 diag"
}

# 基础镜像必须先在本地，否则 build 会去远端拉 —— 国内这步最容易卡住。
# 先显式拉一次，把「拉取」和「构建」拆开，卡在哪一步一目了然。
ensure_base() {
    if $DOCKER image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
        log "基础镜像本机已有：$BASE_IMAGE"
        return 0
    fi
    log "本机没有 $BASE_IMAGE，开始拉取 —— 这一步走公网，慢/卡基本都在这里"
    log "  想绕开：BASE_IMAGE=<本机已有镜像> bash $0 up"
    if ! $DOCKER pull "$BASE_IMAGE"; then
        echo
        echo "  ⚠️ 拉取 $BASE_IMAGE 失败。可换成本机已有镜像绕开，例如："
        $DOCKER images --format '    {{.Repository}}:{{.Tag}}' | head -8
        echo "  然后：BASE_IMAGE=<上面挑一个> bash $0 up"
        exit 1
    fi
}

# 构造 build context。Dockerfile 里刻意写了 VOLUME /data ——
# nginx 官方镜像本身没有声明匿名卷，加上它才能验证
# 「匿名卷在重建时被补回原卷名，数据不会落到新空卷」这条保真规则。
write_context() {
    mkdir -p "$WORKDIR"
    printf '%s\n' "$1" > "$WORKDIR/index.html"
    cat > "$WORKDIR/Dockerfile" <<EOF
FROM ${BASE_IMAGE}
COPY index.html ${DOC_ROOT}/index.html
VOLUME /data
EOF
}

remote_digest() {
    curl -fsS --max-time 8 -o /dev/null -D - \
        -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
        "http://${REG_ADDR}/v2/myapp/manifests/latest" 2>/dev/null \
        | tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest"{print $2}'
}

build_push() {
    # 输出一律保留。原来加了 -q 和 >/dev/null，卡住时什么都看不到，
    # 只能干等 —— 这是上次卡半天的直接原因。
    log "docker build（输出保留，卡在哪一步看得见）"
    ( cd "$WORKDIR" && $DOCKER build -t "$IMAGE" . )
    log "docker push → ${REG_ADDR}（推的是本机 registry，正常几秒内完成）"
    if ! $DOCKER push "$IMAGE"; then
        die "推送失败。最常见的原因是 registry 没在跑（连接被拒）。
    查看：$DOCKER ps -a --filter name=${REG_NAME}
    修复：bash $0 up     （会把 registry 拉起来，并重建测试容器）
    详情：bash $0 diag"
    fi
    log "已推送，registry 当前摘要：$(remote_digest)"
}

cmd_up() {
    log "1/4 启动本地 registry（${REG_ADDR}）"
    ensure_registry

    log "2/4 构造 v1 镜像并推送"
    ensure_base
    write_context "v1"
    build_push

    log "3/4 创建网络与命名卷"
    $DOCKER network inspect "$NET" >/dev/null 2>&1 || $DOCKER network create "$NET" >/dev/null
    $DOCKER volume inspect "$VOL" >/dev/null 2>&1 || $DOCKER volume create "$VOL" >/dev/null

    log "4/4 启动测试容器 ${CONTAINER}"
    $DOCKER rm -f "$CONTAINER" >/dev/null 2>&1 || true
    $DOCKER run -d --name "$CONTAINER" \
        --network "$NET" --network-alias web \
        -p "${HOST_PORT}:80" \
        -v "${VOL}:/var/log/nginx" \
        -e TZ=Asia/Shanghai -e APP_MODE=test \
        --restart unless-stopped \
        "$IMAGE" >/dev/null
    sleep 2

    log "容器页面内容：$(curl -fsS --max-time 5 "http://127.0.0.1:${HOST_PORT}" 2>/dev/null || echo '(还没起来)')"
    cat <<EOF

    ──────────────── 接下来 ────────────────
    1. 回「容器」页点「立即扫描」，应看到 ${IMAGE}，状态是「已是最新」
       （1.1.4 起本地回环地址自动走 http，无需任何开关；
        更早的版本要先在设置页勾一次「允许 http 明文注册表」）
    2. 若状态是「未知」或页面一直转，多半是基础镜像没拉下来或页面内容不对，
       先跑：bash $0 diag
    3. 执行：$0 bump    （模拟上游发版），然后再点一次「立即扫描」

EOF
}

cmd_bump() {
    [ -d "$WORKDIR" ] || die "还没跑过 up，先执行：$0 up"

    # registry 是 bump 的前提，但它可能已经被停掉/被 clean 掉了。
    # 原版这里什么都不查，直接 push —— 于是报一句 connection refused，
    # 看着像网络问题，实际只是容器没在跑。
    log "0/2 确认 registry 可用"
    ensure_registry

    cur="$(tr -d '[:space:]' < "$WORKDIR/index.html" 2>/dev/null || true)"
    n=1
    case "$cur" in v[0-9]*) n="${cur#v}" ;; esac
    next="v$((n + 1))"

    log "1/2 覆盖推送 ${cur:-?} -> ${next}"
    ensure_base
    write_context "$next"
    build_push

    cat <<EOF

    ──────────────── 接下来 ────────────────
    去 Autodocker 点「立即扫描」，预期：
      · 出现「有更新」，并推送一张飞书卡片
      · 点卡片按钮（或页面上那个「更新」），容器被重建
      · 重建后访问 http://127.0.0.1:${HOST_PORT} 应显示 ${next}
      · 想核对配置有没有丢：重建前后各跑一次 $0 show

EOF
}

cmd_show() {
    $DOCKER inspect "$CONTAINER" --format \
'Image      {{.Config.Image}}
ImageID    {{.Image}}
Hostname   {{.Config.Hostname}}
Restart    {{.HostConfig.RestartPolicy.Name}}
Env        {{range .Config.Env}}{{.}} {{end}}
Mounts     {{range .Mounts}}{{.Type}}:{{if .Name}}{{.Name}}{{else}}{{.Source}}{{end}}->{{.Destination}} {{end}}
Networks   {{range $k, $v := .NetworkSettings.Networks}}{{$k}}({{range $v.Aliases}}{{.}} {{end}}) {{end}}'
}

# 卡住时跑这个：把所有可能「等在那里」的环节一次性摊开。
cmd_diag() {
    echo "──── 1. 正在跑的 docker 子进程（卡住的就是它） ────"
    ps -ef | grep -E '[d]ocker (build|pull|push)' || echo "  （没有正在跑的 build/pull/push）"

    echo
    echo "──── 2. docker 守护进程 ────"
    $DOCKER version --format '  client {{.Client.Version}} / server {{.Server.Version}}' 2>&1 || echo "  连不上守护进程"

    echo
    echo "──── 3. 基础镜像 ${BASE_IMAGE} 在不在本机 ────"
    if $DOCKER image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
        echo "  在"
    else
        echo "  不在 → build 会去公网拉，国内最容易卡这一步"
        echo "  本机现有镜像（挑一个用 BASE_IMAGE 传入）："
        $DOCKER images --format '    {{.Repository}}:{{.Tag}}' | head -10
    fi

    echo
    echo "──── 4. registry 容器与 HTTP ────"
    $DOCKER ps -a --filter "name=^${REG_NAME}$" --format '  {{.Names}} {{.Status}} {{.Ports}}' \
        || echo "  （没有这个容器）"
    # 端口映射没绑到回环地址时，docker push 到 127.0.0.1 会被拒
    echo "  端口映射：$($DOCKER port "$REG_NAME" 2>/dev/null | paste -sd' ' - || echo '(取不到)')"
    printf '  GET http://%s/v2/ → %s\n' "$REG_ADDR" \
        "$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "http://${REG_ADDR}/v2/" 2>&1 || echo 连不上)"

    echo
    echo "──── 5. 可能干扰的网络配置 ────"
    echo "  环境变量代理："
    env | grep -i -E '^(http_proxy|https_proxy|no_proxy|HTTP_PROXY|HTTPS_PROXY|NO_PROXY)=' \
        | sed 's/^/    /' || echo "    （无）"
    echo "  /etc/docker/daemon.json："
    if [ -f /etc/docker/daemon.json ]; then
        sed 's/^/    /' /etc/docker/daemon.json
    else
        echo "    （无 daemon.json，即无加速地址）"
    fi

    echo
    echo "  说明：daemon 若配了 HTTP 代理且 no_proxy 里没有 127.0.0.1，"
    echo "        docker push 到本机 registry 会被塞进代理，表现为永久卡住。"
}

cmd_clean() {
    log "清理测试环境"
    # -v 必须加：ad-myapp 的 VOLUME /data 和 registry:2 自带的
    # /var/lib/registry 都是**匿名卷**，只 rm 容器会把卷留下来。
    # 匿名卷没有名字、不会出现在 volume ls 的命名列表里，但占着磁盘，
    # 反复测几轮就会攒出一堆。命名卷 $VOL 不在 -v 的清理范围内，单独删。
    $DOCKER rm -fv "$CONTAINER" >/dev/null 2>&1 || true
    $DOCKER rm -fv "$REG_NAME" >/dev/null 2>&1 || true
    $DOCKER network rm "$NET" >/dev/null 2>&1 || true
    $DOCKER volume rm "$VOL" >/dev/null 2>&1 || true
    $DOCKER rmi "$IMAGE" >/dev/null 2>&1 || true
    rm -rf "$WORKDIR"

    # 自查：删失败了不吭声最容易让人以为已经干净了
    log "残留自查（下面各段没有输出，就说明清干净了）"
    for n in "$CONTAINER" "$REG_NAME"; do
        if $DOCKER ps -a --format '{{.Names}}' | grep -qx "$n"; then
            echo "  容器仍存在：$n（若在跑，先 $DOCKER stop $n）"
        fi
    done
    $DOCKER network ls --format '{{.Name}}' | grep -x "$NET" | sed 's/^/  网络残留：/' || true
    $DOCKER volume ls --format '{{.Name}}' | grep -x "$VOL" | sed 's/^/  命名卷残留：/' || true
    $DOCKER volume ls -qf dangling=true | sed 's/^/  孤儿卷残留：/' || true
    echo
    echo "  说明：基础镜像 ${BASE_IMAGE} 未删（可能被你其它容器共用），"
    echo "        真要删：$DOCKER rmi ${BASE_IMAGE}"
    log "完成。若之前为跑这个测试勾过「允许 http 明文注册表」，记得取消"
}

case "${1:-}" in
up) cmd_up ;;
bump) cmd_bump ;;
show) cmd_show ;;
diag) cmd_diag ;;
clean) cmd_clean ;;
*)
    sed -n '5,22p' "$0"
    exit 1
    ;;
esac
