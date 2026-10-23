#!/usr/bin/env bash
# 本地构建完整产物，上传并预检成功后，再重启远程 Linux/amd64 实例。
# 不覆盖数据库、.env、日志和上传数据；远程 Java 需预先安装。
set -euo pipefail

GO_BIN="${GO_BIN:-/Users/kay/sdk/go1.25.5/bin/go}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOST=""
REMOTE_USER=""
PASS=""
SSH_PORT=22
DEPLOY_DIR="/opt/dbgold"
REMOTE_PORT=18089
REMOTE_JAVA=""
WORK_DIR=""
REMOTE_STAGE=""

usage() {
  cat <<'EOF'
用法: ./deploy.sh -h <host> -u <user> -p <password> [-P <SSH端口>] [-d <部署目录>] [-r <服务端口>] [-j <远程Java绝对路径>]

  -h   远程主机 host/IP        (必填)
  -u   远程账号                (必填)
  -p   远程密码                (必填，可能短暂出现在本机进程列表中)
  -P   SSH 端口                (默认 22)
  -d   远程部署绝对路径        (默认 /opt/dbgold)
  -r   远程 dbgold 监听端口     (默认 18089)
  -j   远程 Java 绝对路径      (默认查找部署目录 runtime/bin/java，然后查找 PATH)

可通过 GO_BIN 环境变量覆盖本机 Go 路径。
示例: ./deploy.sh -h 192.168.1.10 -u root -p 'secret' -d /opt/dbgold -r 18089
EOF
}

fail() { echo "错误: $*" >&2; exit 1; }

while getopts "h:u:p:P:d:r:j:" opt; do
  case "$opt" in
    h) HOST="$OPTARG" ;;
    u) REMOTE_USER="$OPTARG" ;;
    p) PASS="$OPTARG" ;;
    P) SSH_PORT="$OPTARG" ;;
    d) DEPLOY_DIR="$OPTARG" ;;
    r) REMOTE_PORT="$OPTARG" ;;
    j) REMOTE_JAVA="$OPTARG" ;;
    *) usage; exit 1 ;;
  esac
done
shift "$((OPTIND - 1))"
[[ $# == 0 ]] || fail "不支持额外的位置参数。"
if [[ -z "$HOST" || -z "$REMOTE_USER" || -z "$PASS" ]]; then
  usage >&2
  fail "-h(主机)、-u(账号)、-p(密码) 均为必填。"
fi
for port in "$SSH_PORT" "$REMOTE_PORT"; do
  [[ "$port" =~ ^[0-9]{1,5}$ ]] || fail "端口必须为 1–65535 的整数。"
  (( 10#$port >= 1 && 10#$port <= 65535 )) || fail "端口必须在 1–65535 范围内。"
done
SSH_PORT=$((10#$SSH_PORT))
REMOTE_PORT=$((10#$REMOTE_PORT))
(( REMOTE_PORT >= 1024 )) || fail "dbgold 服务端口必须在 1024–65535 范围内。"
[[ "$DEPLOY_DIR" == /* && "$DEPLOY_DIR" != / && "$DEPLOY_DIR" != *$'\n'* ]] || fail "部署目录必须为非根目录的绝对路径。"
[[ -z "$REMOTE_JAVA" || "$REMOTE_JAVA" == /* ]] || fail "-j 必须指定远程 Java 的绝对路径。"

for tool in sshpass ssh npm javac jar tar shasum; do
  command -v "$tool" >/dev/null 2>&1 || fail "未找到本机构建工具: $tool"
done
command -v "$GO_BIN" >/dev/null 2>&1 || fail "未找到 Go 可执行文件: $GO_BIN"
for file in jdbcbridge/java/build.sh deploy/remote-deploy.sh third_party/oscar/oscarJDBC8.jar third_party/oscar/README.md third_party/oscar/LICENSE; do
  [[ -s "$SCRIPT_DIR/$file" ]] || fail "缺少或为空的构建依赖: $file"
done

# 使用 POSIX 单引号转义传递参数，避免远程路径被当成 shell 代码执行。
remote_run() {
  local command_text="" arg
  for arg in "$@"; do
    command_text+="'${arg//\'/\'\\\'\'}' "
  done
  sshpass -p "$PASS" ssh -p "$SSH_PORT" -o StrictHostKeyChecking=no \
    "$REMOTE_USER@$HOST" "$command_text"
}

cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$REMOTE_STAGE" ]]; then
    remote_run rm -rf -- "$REMOTE_STAGE" || echo "警告: 远程暂存目录清理失败: $REMOTE_STAGE" >&2
  fi
  if [[ -n "$WORK_DIR" ]]; then
    rm -rf -- "$WORK_DIR"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
WORK_DIR="$(mktemp -d)"
BUNDLE_DIR="$WORK_DIR/bundle"
mkdir -p "$BUNDLE_DIR/lib"

echo "==> [1/6] 交叉编译后端 (linux/amd64)..."
cd "$SCRIPT_DIR"
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PKG="dbgold/api/handler"
LDFLAGS="-X ${PKG}.Version=${VERSION} -X ${PKG}.GitCommit=${GIT_COMMIT} -X ${PKG}.BuildTime=${BUILD_TIME}"
GOOS=linux GOARCH=amd64 "$GO_BIN" build -ldflags "$LDFLAGS" -o "$BUNDLE_DIR/dbgold" dbgold
[[ -s "$BUNDLE_DIR/dbgold" ]] || fail "后端编译产物 dbgold 不存在或为空。"

echo "==> [2/6] 构建前端..."
(cd "$SCRIPT_DIR/frontend" && npm run build)
[[ -s "$SCRIPT_DIR/frontend/dist/index.html" ]] || fail "前端构建产物 frontend/dist/index.html 不存在或为空。"
cp -R "$SCRIPT_DIR/frontend/dist" "$BUNDLE_DIR/dist"

echo "==> [3/6] 重建 Oscar bridge 并打包 JDBC 依赖..."
sh "$SCRIPT_DIR/jdbcbridge/java/build.sh"
[[ -s "$SCRIPT_DIR/jdbcbridge/java/build/dbgold-oscar-bridge.jar" ]] || fail "Oscar bridge 构建产物不存在或为空。"
cp "$SCRIPT_DIR/jdbcbridge/java/build/dbgold-oscar-bridge.jar" "$BUNDLE_DIR/lib/"
cp "$SCRIPT_DIR/third_party/oscar/oscarJDBC8.jar" "$BUNDLE_DIR/lib/"
cp "$SCRIPT_DIR/third_party/oscar/README.md" "$BUNDLE_DIR/lib/OSCAR-JDBC-NOTICE.md"
cp "$SCRIPT_DIR/third_party/oscar/LICENSE" "$BUNDLE_DIR/lib/OSCAR-JDBC-LICENSE"
cp "$SCRIPT_DIR/deploy/remote-deploy.sh" "$BUNDLE_DIR/remote-deploy.sh"
(
  cd "$BUNDLE_DIR"
  find . -type f -print0 | xargs -0 shasum -a 256 > "$WORK_DIR/SHA256SUMS"
  cp "$WORK_DIR/SHA256SUMS" SHA256SUMS
  tar -czf "$WORK_DIR/bundle.tar.gz" .
)

echo "==> [4/6] 上传到远程暂存目录（旧服务继续运行）..."
STAGE_CANDIDATE="$(remote_run bash -c 'set -eu; mkdir -p -- "$1"; cd -- "$1"; test "$(pwd -P)" != /; mktemp -d "$(pwd -P)/.deploy.XXXXXX"' bash "$DEPLOY_DIR")"
[[ "$STAGE_CANDIDATE" =~ ^/.*/\.deploy\.[[:alnum:]]{6}$ && "$STAGE_CANDIDATE" != *$'\n'* ]] || fail "远程返回了无效的暂存目录。"
REMOTE_STAGE=$STAGE_CANDIDATE
# 通过 SSH 流式上传归档，不依赖 scp 对远程路径的引号处理。
remote_run bash -c 'cat > "$1/bundle.tar.gz"' bash "$REMOTE_STAGE" < "$WORK_DIR/bundle.tar.gz"
remote_run tar -xzf "$REMOTE_STAGE/bundle.tar.gz" -C "$REMOTE_STAGE"

echo "==> [5/6] 校验完整性、Java 和 JDBC 加载..."
remote_run bash "$REMOTE_STAGE/remote-deploy.sh" preflight "$REMOTE_STAGE" "$REMOTE_JAVA"

echo "==> [6/6] 替换产物并检查新实例就绪..."
remote_run bash "$REMOTE_STAGE/remote-deploy.sh" activate "$REMOTE_STAGE" "$REMOTE_JAVA" "$REMOTE_PORT"

echo
echo "✅ 部署完成"
echo "   主机:     $REMOTE_USER@$HOST:$SSH_PORT"
echo "   目录:     $DEPLOY_DIR"
echo "   服务端口: $REMOTE_PORT"
echo "   日志:     $DEPLOY_DIR/dbgold.out (应用日志在 $DEPLOY_DIR/log/)"
