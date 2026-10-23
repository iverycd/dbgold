#!/usr/bin/env bash
# deploy.sh 上传后执行的远程阶段；也可 source 以隔离测试。
set -euo pipefail

error() { echo "错误: $*" >&2; return 1; }

select_java() {
  local deploy_dir=$1 override=$2 java_bin
  if [[ -n "$override" ]]; then
    [[ "$override" == /* ]] || { error "Java 路径必须是绝对路径。"; return 1; }
    java_bin=$override
  elif [[ -x "$deploy_dir/runtime/bin/java" ]]; then
    java_bin="$deploy_dir/runtime/bin/java"
  else
    java_bin="$(command -v java)" || { error "远程服务器未找到 Java；请安装 Java 或使用 -j 指定路径。"; return 1; }
  fi
  [[ -f "$java_bin" && -x "$java_bin" ]] || { error "远程 Java 不可执行: $java_bin"; return 1; }
  readlink -f -- "$java_bin"
}

preflight() {
  local stage=$1 java_override=$2 deploy_dir file tool
  deploy_dir="$(cd -- "$stage/.." && pwd -P)"
  [[ "$deploy_dir" != / && "$(basename -- "$stage")" == .deploy.* ]] || { error "无效的部署暂存目录。"; return 1; }
  [[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || { error "此脚本只支持 Linux/amd64 服务器。"; return 1; }
  for tool in sha256sum readlink pgrep nohup timeout; do
    command -v "$tool" >/dev/null 2>&1 || { error "远程服务器缺少工具: $tool"; return 1; }
  done
  for file in dbgold dist/index.html lib/dbgold-oscar-bridge.jar lib/oscarJDBC8.jar lib/OSCAR-JDBC-NOTICE.md lib/OSCAR-JDBC-LICENSE SHA256SUMS; do
    [[ -s "$stage/$file" ]] || { error "部署产物缺失或为空: $file"; return 1; }
  done
  (cd -- "$stage" && sha256sum -c SHA256SUMS) || return 1
  SELECTED_JAVA="$(select_java "$deploy_dir" "$java_override")" || return 1
  echo "使用远程 Java: $SELECTED_JAVA"
  timeout 15s "$SELECTED_JAVA" -cp "$stage/lib/dbgold-oscar-bridge.jar:$stage/lib/oscarJDBC8.jar" \
    com.dbgold.oscar.BridgeMain </dev/null || { error "远程 Java 无法加载本次部署的 Oscar bridge/JDBC。"; return 1; }
}

pid_is_instance() {
  local pid=$1 deploy_dir=$2 executable
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  executable="$(readlink -- "/proc/$pid/exe" 2>/dev/null)" || return 1
  # 兼容旧部署曾直接删除运行中二进制的情形。
  executable="${executable% (deleted)}"
  [[ "$executable" == "$deploy_dir/dbgold" ]]
}

instance_pids() {
  local deploy_dir=$1 pid
  while IFS= read -r pid; do
    if pid_is_instance "$pid" "$deploy_dir"; then
      printf '%s\n' "$pid"
    fi
  done < <(pgrep -x dbgold || true)
}

stop_instance() {
  local deploy_dir=$1 pid deadline
  local pids=()
  while IFS= read -r pid; do
    [[ -z "$pid" ]] || pids+=("$pid")
  done < <(instance_pids "$deploy_dir")
  if [[ ${#pids[@]} == 0 ]]; then
    echo "当前部署目录没有运行中的 dbgold。"
    return
  fi
  echo "停止当前实例: ${pids[*]}"
  for pid in "${pids[@]}"; do
    if pid_is_instance "$pid" "$deploy_dir"; then
      if ! kill -TERM "$pid" && pid_is_instance "$pid" "$deploy_dir"; then
        error "无法停止旧进程: $pid"
        return 1
      fi
    fi
  done
  deadline=$((SECONDS + 20))
  while (( SECONDS < deadline )); do
    [[ -n "$(instance_pids "$deploy_dir")" ]] || return 0
    sleep 1
  done
  for pid in "${pids[@]}"; do
    if pid_is_instance "$pid" "$deploy_dir"; then
      echo "优雅退出超时，强制停止当前实例: $pid"
      if ! kill -KILL "$pid" && pid_is_instance "$pid" "$deploy_dir"; then
        error "无法强制停止旧进程: $pid"
        return 1
      fi
    fi
  done
  # 强杀完成前不得替换正在使用的产物。
  for _ in 1 2 3 4 5; do
    [[ -n "$(instance_pids "$deploy_dir")" ]] || return 0
    sleep 1
  done
  error "旧实例仍未退出，停止替换产物。"
}

pid_alive() { kill -0 "$1" 2>/dev/null; }

# 从新进程持有的 socket 确定监听地址，尊重 .env 的 LISTEN_HOST，
# 并避免同端口的其他服务被误当成新实例。第三个参数仅用于隔离测试。
instance_listen_host() {
  local pid=$1 port=$2 proc_root=${3:-/proc} fd socket inodes=" " port_hex
  local slot address remote_address state queues timer retransmit uid tcp_timeout inode rest
  printf -v port_hex '%04X' "$port"
  for fd in "$proc_root/$pid/fd/"*; do
    socket="$(readlink -- "$fd" 2>/dev/null)" || continue
    if [[ "$socket" =~ ^socket:\[([0-9]+)\]$ ]]; then
      inodes+="${BASH_REMATCH[1]} "
    fi
  done
  [[ -r "$proc_root/$pid/net/tcp" ]] || return 1
  while read -r slot address remote_address state queues timer retransmit uid tcp_timeout inode rest; do
    if [[ "$state" == 0A && "${address#*:}" == "$port_hex" && "$inodes" == *" $inode "* ]]; then
      address=${address%:*}
      if [[ "$address" == 00000000 ]]; then
        echo 127.0.0.1
      else
        # Linux/amd64 的 /proc/net/tcp 使用小端 IPv4 十六进制。
        printf '%d.%d.%d.%d\n' "$((16#${address:6:2}))" "$((16#${address:4:2}))" \
          "$((16#${address:2:2}))" "$((16#${address:0:2}))"
      fi
      return 0
    fi
  done < "$proc_root/$pid/net/tcp"
  return 1
}

verify_ready() {
  local deploy_dir=$1 pid=$2 port=$3 deadline listen_host
  deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    if ! pid_alive "$pid"; then
      error "新 dbgold 进程已退出 (PID: $pid)。" || true
      tail -n 50 "$deploy_dir/dbgold.out" >&2 || true
      return 1
    fi
    # 异步启动后，PID 可能尚在执行 nohup；等待它 exec 为本实例再探测。
    if pid_is_instance "$pid" "$deploy_dir" && listen_host="$(instance_listen_host "$pid" "$port")"; then
      if NO_PROXY='*' no_proxy='*' "$deploy_dir/dbgold" healthcheck --url "http://$listen_host:$port/api/health/ready" --timeout 1s >/dev/null 2>&1; then
        if pid_alive "$pid" && pid_is_instance "$pid" "$deploy_dir"; then
          echo "dbgold 已就绪，PID: $pid"
          return 0
        fi
      fi
    fi
    sleep 1
  done
  error "新 dbgold 在 30 秒内未就绪 (PID: $pid)。" || true
  tail -n 50 "$deploy_dir/dbgold.out" >&2 || true
  return 1
}

activate() {
  local stage=$1 java_override=$2 port=$3 deploy_dir pid file
  deploy_dir="$(cd -- "$stage/.." && pwd -P)"
  # 紧邻停机再次验证，防止暂存文件或运行环境在两个 SSH 阶段间变化。
  preflight "$stage" "$java_override" || return 1
  stop_instance "$deploy_dir" || return 1
  chmod +x "$stage/dbgold" || return 1
  mkdir -p "$deploy_dir/lib" || return 1
  # 所有网络传输已完成；按文件替换 lib，保留其他驱动和运行数据。
  for file in dbgold-oscar-bridge.jar oscarJDBC8.jar OSCAR-JDBC-NOTICE.md OSCAR-JDBC-LICENSE; do
    mv -f -- "$stage/lib/$file" "$deploy_dir/lib/$file" || return 1
  done
  rm -rf -- "$deploy_dir/dist" || return 1
  mv -- "$stage/dist" "$deploy_dir/dist" || return 1
  mv -f -- "$stage/dbgold" "$deploy_dir/dbgold" || return 1
  cd -- "$deploy_dir" || return 1
  PORT="$port" STATIC_DIR="$deploy_dir/dist" OSCAR_JAVA_BIN="$SELECTED_JAVA" \
    OSCAR_BRIDGE_JAR="$deploy_dir/lib/dbgold-oscar-bridge.jar" \
    OSCAR_JDBC_JAR="$deploy_dir/lib/oscarJDBC8.jar" \
    nohup "$deploy_dir/dbgold" > "$deploy_dir/dbgold.out" 2>&1 </dev/null &
  pid=$!
  printf '%s\n' "$pid" > "$deploy_dir/dbgold.pid"
  verify_ready "$deploy_dir" "$pid" "$port"
}

main() {
  [[ $# -ge 3 ]] || { error "用法: remote-deploy.sh preflight STAGE JAVA | activate STAGE JAVA PORT"; return 1; }
  case "$1" in
    preflight) [[ $# == 3 ]] && preflight "$2" "$3" ;;
    activate) [[ $# == 4 ]] && activate "$2" "$3" "$4" ;;
    *) error "未知远程部署阶段: $1" ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
