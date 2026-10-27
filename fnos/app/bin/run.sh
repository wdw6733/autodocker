#!/bin/bash
# AutoDocker 启动包装脚本 —— 由 cmd/main 调用，不要在命令行直接执行。
#
# 职责：把用户配置（$TRIM_PKGETC/config.env）装载成环境变量，
# 补齐路径类变量，然后 exec 到真正的服务进程。
# 用 exec 是为了让进程号保持不变，cmd/main 记下的 pid 才能直接命中服务本身。

set -eu

ENV_FILE="${TRIM_PKGETC:-}/config.env"

# 配置文件里存的是用户填的飞书凭证与访问密码，随安装位置走，不写死在包里
if [ -n "${TRIM_PKGETC:-}" ] && [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    . "$ENV_FILE"
    set +a
fi

# 以下三项由脚本决定，不接受配置文件的覆盖：
# 端口在 manifest 与 app/ui/config 中同样写死为 19999，三处必须一致，
# 否则飞牛桌面入口会指向一个没人监听的端口，页面必然打不开。
export PORT="${TRIM_SERVICE_PORT:-19999}"
export DB_PATH="${TRIM_PKGVAR:-/tmp}/autodocker.db"
export STATIC_DIR="${TRIM_APPDEST:-.}/www"

export LOG_LEVEL="${LOG_LEVEL:-info}"
export DOCKER_HOST="${DOCKER_HOST:-unix:///var/run/docker.sock}"

BIN="${TRIM_APPDEST:-.}/bin/autodocker"
if [ ! -x "$BIN" ]; then
    echo "未找到可执行文件：$BIN（可能打包时丢失了可执行位）" >&2
    exit 1
fi

exec "$BIN"
