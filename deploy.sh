#!/usr/bin/env bash
#
# deploy.sh — dbgold 一键编译 + 远程部署
#
#   1. 本地交叉编译后端 (GOOS=linux GOARCH=amd64)
#   2. 本地打包前端 (frontend: npm run build -> dist)
#   3. 用 sshpass 以密码方式 scp 到远程服务器
#   4. 远程重启 dbgold 进程 (启动前 kill 旧进程)
#
# 注意:
#   - 使用 sshpass 传密码,密码会短暂出现在本机进程列表中。
#   - 默认放宽了 SSH host key 校验 (StrictHostKeyChecking=no),方便首次连接。
#   - 不拷贝 dbgold.db,避免覆盖远程运行时数据;dist 由远程 nginx 托管。
#
set -euo pipefail

# ----- 固定路径 -----
GO_BIN="/Users/kay/sdk/go1.25.5/bin/go"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ----- 默认参数 -----
HOST=""
USER=""
PASS=""
SSH_PORT=22
DEPLOY_DIR="/opt/dbgold"
REMOTE_PORT=18089

usage() {
  cat <<'EOF'
用法: ./deploy.sh -h <host> -u <user> -p <password> [-P <ssh端口>] [-d <部署目录>] [-r <服务端口>]

  -h   远程主机 host/IP        (必填)
  -u   远程账号                (必填)
  -p   远程密码                (必填)
  -P   SSH 端口                (可选, 默认 22)
  -d   远程部署目录            (可选, 默认 /opt/dbgold)
  -r   远程 dbgold 监听端口    (可选, 默认 18089, 传给远程 PORT 环境变量)

示例:
  ./deploy.sh -h 192.168.1.10 -u root -p 'secret' -P 22 -d /opt/dbgold -r 18089
EOF
}

# ----- 解析参数 -----
while getopts "h:u:p:P:d:r:" opt; do
  case "$opt" in
    h) HOST="$OPTARG" ;;
    u) USER="$OPTARG" ;;
    p) PASS="$OPTARG" ;;
    P) SSH_PORT="$OPTARG" ;;
    d) DEPLOY_DIR="$OPTARG" ;;
    r) REMOTE_PORT="$OPTARG" ;;
    *) usage; exit 1 ;;
  esac
done

# ----- 校验必填参数 -----
if [[ -z "$HOST" || -z "$USER" || -z "$PASS" ]]; then
  echo "错误: -h(主机)、-u(账号)、-p(密码) 均为必填。" >&2
  echo >&2
  usage
  exit 1
fi

# ----- 前置检查 -----
if ! command -v sshpass >/dev/null 2>&1; then
  echo "错误: 未找到 sshpass。macOS 可执行: brew install sshpass" >&2
  exit 1
fi
if [[ ! -x "$GO_BIN" ]]; then
  echo "错误: 未找到 Go 可执行文件: $GO_BIN" >&2
  exit 1
fi

# ----- 远程命令封装 -----
remote() {
  sshpass -p "$PASS" ssh -p "$SSH_PORT" -o StrictHostKeyChecking=no \
    "$USER@$HOST" "$@"
}

remote_copy() {
  # remote_copy <本地路径> <远程绝对路径>  支持文件和目录(-r)
  sshpass -p "$PASS" scp -r -P "$SSH_PORT" -o StrictHostKeyChecking=no \
    "$1" "$USER@$HOST:$2"
}

echo "==> [1/6] 交叉编译后端 (linux/amd64)..."
cd "$SCRIPT_DIR"
# 采集 git 版本信息，通过 ldflags 注入到 api/handler 包变量。
# BuildTime 用无空格 ISO 格式，避免 ldflags 引号问题；git describe 无 tag 时回退到短 commit。
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PKG="dbgold/api/handler"
LDFLAGS="-X ${PKG}.Version=${VERSION} -X ${PKG}.GitCommit=${GIT_COMMIT} -X ${PKG}.BuildTime=${BUILD_TIME}"
GOOS=linux GOARCH=amd64 "$GO_BIN" build -ldflags "$LDFLAGS" -o dbgold dbgold
if [[ ! -f "$SCRIPT_DIR/dbgold" ]]; then
  echo "错误: 后端编译产物 dbgold 不存在。" >&2
  exit 1
fi

echo "==> [2/6] 打包前端 (npm run build)..."
cd "$SCRIPT_DIR/frontend"
npm run build
cd "$SCRIPT_DIR"
if [[ ! -f "$SCRIPT_DIR/frontend/dist/index.html" ]]; then
  echo "错误: 前端构建产物 frontend/dist/index.html 不存在。" >&2
  exit 1
fi

echo "==> [3/6] 远程准备部署目录: $DEPLOY_DIR ..."
remote "mkdir -p '$DEPLOY_DIR'"

echo "==> [4/6] 停止远程旧 dbgold 进程..."
remote "bash -s" <<EOF
set -e
# 按可执行文件名精确匹配 (-x),无论以 ./dbgold 还是绝对路径启动都能命中
PIDS=\$(pgrep -x dbgold || true)
if [ -n "\$PIDS" ]; then
  echo "发现旧进程: \$PIDS, 正在停止..."
  kill \$PIDS || true
  # 等待最多 5 秒优雅退出
  for i in 1 2 3 4 5; do
    sleep 1
    pgrep -x dbgold >/dev/null || break
  done
  STILL=\$(pgrep -x dbgold || true)
  if [ -n "\$STILL" ]; then
    echo "进程未退出, 强制 kill -9: \$STILL"
    kill -9 \$STILL || true
    sleep 1
  fi
else
  echo "无运行中的 dbgold 进程。"
fi
EOF

echo "==> [5/6] 拷贝产物到远程..."
# 后端二进制:先删旧文件,避免 "text file busy" 导致 scp 覆盖失败
remote "rm -f '$DEPLOY_DIR/dbgold'"
remote_copy "$SCRIPT_DIR/dbgold" "$DEPLOY_DIR/dbgold"
# 前端 dist:先删旧的再传,避免残留旧 assets
remote "rm -rf '$DEPLOY_DIR/dist'"
remote_copy "$SCRIPT_DIR/frontend/dist" "$DEPLOY_DIR/dist"
# 不拷贝 dbgold.db (保护远程运行时数据)

echo "==> [6/6] 远程启动 dbgold (PORT=$REMOTE_PORT)..."
remote "bash -s" <<EOF
set -e
cd '$DEPLOY_DIR'
chmod +x dbgold
# 用绝对路径启动,保证进程名/命令行可被后续精确匹配
PORT=$REMOTE_PORT nohup '$DEPLOY_DIR/dbgold' > '$DEPLOY_DIR/dbgold.out' 2>&1 &
sleep 2
PIDS=\$(pgrep -x dbgold || true)
if [ -z "\$PIDS" ]; then
  echo "错误: dbgold 启动失败, 最近日志:" >&2
  tail -n 30 '$DEPLOY_DIR/dbgold.out' >&2 || true
  exit 1
fi
echo "dbgold 已启动, PID: \$PIDS"
EOF

echo
echo "✅ 部署完成"
echo "   主机:     $USER@$HOST:$SSH_PORT"
echo "   目录:     $DEPLOY_DIR"
echo "   服务端口: $REMOTE_PORT"
echo "   日志:     $DEPLOY_DIR/dbgold.out (应用日志在 $DEPLOY_DIR/log/)"
