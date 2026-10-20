#!/usr/bin/env bash
# Option Scope 部署与运维脚本（菜单式，纯 Go 版）
#
# 适用系统：Ubuntu / Debian（apt）、CentOS / RHEL / Rocky / Alma（dnf、yum）、Alpine（apk）
# 适用架构：Linux x86_64（amd64）、aarch64（arm64）
# 运行方式：直接运行与 run.sh 同目录的预编译二进制：
#   - option-scope-linux-amd64
#   - option-scope-linux-arm64
#
# 用法：
#   ./run.sh                      打开交互菜单
#   ./run.sh start|stop|restart   启动 / 停止 / 重启
#   ./run.sh status               查看运行状态
#   ./run.sh logs [行数]          查看应用日志（默认 80 行）
#   ./run.sh config               修改 .env（保存后可自动重启）
#   ./run.sh doctor               环境自检
#   ./run.sh trim-logs            立即把日志裁剪到 200 行
#   ./run.sh memory [数量]        查看内存使用情况与占用最高的进程（默认前 10）
#   ./run.sh kill-port [端口]     交互确认后结束监听该 TCP 端口的进程
#   ./run.sh install-service      安装并启用 systemd 常驻服务（需要 systemd）
#   ./run.sh remove-service       移除 systemd 服务
#   ./run.sh enable-autostart     配置开机自启（crontab @reboot，适配容器）
#   ./run.sh disable-autostart    取消开机自启
#   ./run.sh foreground           前台运行（供 systemd 使用）
#   ./run.sh __watchdog           内部使用：看门狗主循环
set -u
set -o pipefail

# ---------- 基础路径与常量 ----------
SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$PROJECT_DIR/.env"
ENV_EXAMPLE="$PROJECT_DIR/.env.example"
RUN_DIR="$PROJECT_DIR/.run"
LOG_DIR="$PROJECT_DIR/logs"
APP_LOG="$LOG_DIR/app.log"
WATCHDOG_LOG="$LOG_DIR/watchdog.log"
CRON_LOG="$LOG_DIR/cron.log"
APP_PID_FILE="$RUN_DIR/app.pid"
WATCHDOG_PID_FILE="$RUN_DIR/watchdog.pid"
SYSTEMD_UNIT_NAME="option-scope.service"
SYSTEMD_UNIT_FILE="/etc/systemd/system/$SYSTEMD_UNIT_NAME"

# 应用二进制：与 run.sh 同目录，按 CPU 架构自动选择
APP_BINARY=""
BINARY_ARCH=""

WATCHDOG_INTERVAL=15
WATCHDOG_FAIL_LIMIT=2
START_TIMEOUT=60
STOP_TIMEOUT=10

# 日志裁剪：只保留最后 N 行，原地覆盖不换 inode（避免 Go 进程 fd 悬空）
LOG_MAX_LINES="${LOG_MAX_LINES:-200}"    # 应用 / 看门狗日志最多保留行数
LOG_MAX_KB="${LOG_MAX_KB:-512}"          # 单文件字节兜底（防单行超长撑爆）
CRON_LOG_MAX_LINES="${CRON_LOG_MAX_LINES:-500}"
CRON_LOG_MAX_KB="${CRON_LOG_MAX_KB:-1024}"

AUTOSTART_TAG="# option-scope-autostart"

# ---------- 输出样式 ----------
if [ -t 1 ]; then
  C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'; C_RED=$'\033[31m'
  C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_CYAN=$'\033[36m'
  C_DIM=$'\033[2m'
else
  C_RESET=""; C_BOLD=""; C_RED=""; C_GREEN=""; C_YELLOW=""; C_CYAN=""; C_DIM=""
fi

info()    { printf '%s[信息]%s %s\n' "$C_CYAN" "$C_RESET" "$*"; }
ok()      { printf '%s[完成]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn()    { printf '%s[注意]%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
fail()    { printf '%s[错误]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
section() { printf '\n%s== %s ==%s\n' "$C_BOLD" "$*" "$C_RESET"; }
dim()     { printf '%s%s%s\n' "$C_DIM" "$*" "$C_RESET"; }
die()     { fail "$*"; exit 1; }
has_cmd() { command -v "$1" >/dev/null 2>&1; }

# ---------- 权限与系统探测 ----------
init_privilege() {
  if [ "$(id -u)" -eq 0 ]; then
    SUDO=""
  elif has_cmd sudo; then
    SUDO="sudo"
  else
    SUDO="none"
  fi
}

run_root() {
  case "$SUDO" in
    "") "$@" ;;
    none)
      fail "需要 root 权限：$*"
      fail "当前不是 root 且没有 sudo，请用 root 登录后手动执行上面这条命令"
      return 1
      ;;
    *) sudo "$@" ;;
  esac
}

detect_system() {
  OS_NAME="未知发行版"
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_NAME="${PRETTY_NAME:-${NAME:-未知发行版}}"
  fi
  ARCH="$(uname -m)"
  if has_cmd apt-get; then PKG_MANAGER="apt"
  elif has_cmd dnf; then PKG_MANAGER="dnf"
  elif has_cmd yum; then PKG_MANAGER="yum"
  elif has_cmd apk; then PKG_MANAGER="apk"
  elif has_cmd zypper; then PKG_MANAGER="zypper"
  else PKG_MANAGER=""
  fi
}

setup_binary_path() {
  case "$ARCH" in
    x86_64 | amd64)   BINARY_ARCH="amd64" ;;
    aarch64 | arm64)  BINARY_ARCH="arm64" ;;
    *)
      BINARY_ARCH=""
      APP_BINARY=""
      return 0
      ;;
  esac
  APP_BINARY="$PROJECT_DIR/option-scope-linux-$BINARY_ARCH"
}

# ---------- 访问密钥生成 ----------
generate_access_key() {
  local key=""
  if has_cmd openssl; then
    key="$(openssl rand -hex 8)"
  else
    key="$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | dd bs=1 count=16 2>/dev/null | tr -d '\n')"
  fi
  case "$key" in
    '' | *[!a-z0-9]*) return 1 ;;
  esac
  [ "${#key}" -eq 16 ] || return 1
  printf '%s' "$key"
}

# ---------- .env 读写 ----------
read_env_file_value() {
  local key="$1" default_value="$2"
  if [ -f "$ENV_FILE" ]; then
    awk -v key="$key" -v fallback="$default_value" '
      BEGIN { sq = sprintf("%c", 39) }
      {
        line = $0
        sub(/^[[:space:]]*/, "", line)
        sub(/^export[[:space:]]+/, "", line)
        prefix = key "="
        if (index(line, prefix) == 1) {
          value = substr(line, length(prefix) + 1)
          gsub(/\r/, "", value)
          sub(/^[[:space:]]*/, "", value)
          sub(/[[:space:]]*$/, "", value)
          quoted = ((substr(value, 1, 1) == "\"" && substr(value, length(value), 1) == "\"") || (substr(value, 1, 1) == sq && substr(value, length(value), 1) == sq))
          if (!quoted) {
            sub(/[[:space:]]+#.*/, "", value)
            sub(/[[:space:]]*$/, "", value)
            quoted = ((substr(value, 1, 1) == "\"" && substr(value, length(value), 1) == "\"") || (substr(value, 1, 1) == sq && substr(value, length(value), 1) == sq))
          }
          if (quoted) {
            value = substr(value, 2, length(value) - 2)
          }
        }
      }
      END { print value != "" ? value : fallback }
    ' "$ENV_FILE"
  else
    printf '%s' "$default_value"
  fi
}

read_env_value() {
  local key="$1" default_value="$2"
  if [[ -v $key ]]; then
    printf '%s' "${!key}"
  else
    read_env_file_value "$key" "$default_value"
  fi
}

write_env_value() {
  local key="$1" value="$2" tmp metadata mode owner_uid owner_gid current_uid current_gid
  [ -f "$ENV_FILE" ] || ensure_env_file || return 1
  metadata="$(stat -c '%a %u %g' "$ENV_FILE" 2>/dev/null)" || {
    fail "无法读取 $ENV_FILE 的权限与属主"
    return 1
  }
  read -r mode owner_uid owner_gid <<<"$metadata"
  current_uid="$(id -u)"
  current_gid="$(id -g)"
  tmp="$(mktemp "$PROJECT_DIR/.env.XXXXXX")" || return 1
  if awk -v k="$key" -v v="$value" '
    BEGIN { written = 0 }
    $0 ~ "^[[:space:]]*" k "=" { if (!written) { print k "=" v; written = 1 } ; next }
    { print }
    END { if (!written) print k "=" v }
  ' "$ENV_FILE" > "$tmp" && chmod "$mode" "$tmp" &&
      { [ "$current_uid" -eq 0 ] && chown "$owner_uid:$owner_gid" "$tmp" ||
        { [ "$current_uid" -eq "$owner_uid" ] && { [ "$current_gid" -eq "$owner_gid" ] || chgrp "$owner_gid" "$tmp"; } || chown "$owner_uid:$owner_gid" "$tmp"; }; } &&
      mv -f "$tmp" "$ENV_FILE"; then
    return 0
  fi
  rm -f "$tmp" 2>/dev/null
  return 1
}

ensure_env_file() {
  if [ ! -f "$ENV_FILE" ]; then
    if [ -f "$ENV_EXAMPLE" ]; then
      cp "$ENV_EXAMPLE" "$ENV_FILE" || return 1
      ok "已由 .env.example 生成 $ENV_FILE"
    else
      cat >"$ENV_FILE" <<'ENVEOF'
# Option Scope 配置
HOST=0.0.0.0
PORT=8000
ACCESS_KEY=
DATABASE_PATH=data/options.db
DATABASE_MAX_MB=256M
RAW_RETENTION_DAYS=30
SCHEDULER_ENABLED=true
RESOURCE_MODE=auto
ENVEOF
      ok "已生成默认 $ENV_FILE"
    fi
    chmod 600 "$ENV_FILE" || { rm -f "$ENV_FILE"; return 1; }
    if [ "$(id -u)" -eq 0 ] && has_cmd stat; then
      local project_owner
      project_owner="$(stat -c '%u:%g' "$PROJECT_DIR" 2>/dev/null)" || return 1
      chown "$project_owner" "$ENV_FILE" || { rm -f "$ENV_FILE"; return 1; }
    fi
  else
    if [ "$(id -u)" -eq 0 ] && has_cmd stat; then
      local project_owner env_owner
      project_owner="$(stat -c '%u:%g' "$PROJECT_DIR" 2>/dev/null)" || return 1
      env_owner="$(stat -c '%u:%g' "$ENV_FILE" 2>/dev/null)" || return 1
      if [ "${env_owner%%:*}" != "${project_owner%%:*}" ]; then
        chown "$project_owner" "$ENV_FILE" || { fail "无法将 $ENV_FILE 交给项目服务用户"; return 1; }
      fi
    fi
    chmod u+r "$ENV_FILE" || { fail "无法授予项目服务用户读取 $ENV_FILE 的权限"; return 1; }
  fi

  local access_key=""
  access_key="$(read_env_value ACCESS_KEY "")"
  [ -n "$access_key" ] && return 0
  access_key="$(generate_access_key)" || {
    fail "无法生成访问密钥，请确认系统存在 openssl 或可用的 /dev/urandom"
    return 1
  }
  write_env_value ACCESS_KEY "$access_key" || return 1
  ok "已生成访问密钥 ACCESS_KEY=$access_key"
}

ensure_app_binary() {
  if [ -z "$APP_BINARY" ]; then
    fail "当前架构 $ARCH 不受支持（仅支持 linux-amd64 / linux-arm64）"
    return 1
  fi
  if [ ! -x "$APP_BINARY" ]; then
    if [ -f "$APP_BINARY" ]; then
      warn "二进制缺少执行权限，尝试 chmod +x：$APP_BINARY"
      chmod +x "$APP_BINARY" 2>/dev/null || true
    fi
    if [ ! -x "$APP_BINARY" ]; then
      fail "应用二进制缺失或不可执行：$APP_BINARY"
      fail "请将 option-scope-linux-$BINARY_ARCH 与 run.sh 放在同一目录"
      return 1
    fi
  fi
  return 0
}

ensure_runtime() {
  ensure_env_file || return 1
  ensure_app_binary || return 1
  mkdir -p "$RUN_DIR" "$LOG_DIR"
  return 0
}

# ---------- 日志裁剪（保留最后 N 行，原地覆盖不换 inode） ----------
# 说明：
#   - spawn_detached 用 >> 打开日志，Go 进程持有 fd 且 O_APPEND。
#   - 直接 cat 临时文件 > logfile 覆盖，inode 不变，Go fd 继续有效。
#   - 不能用 mv 替换，会换 inode 导致后续写入进"已删除"的文件。
trim_log_file() {
  local log="$1" max_lines="${2:-$LOG_MAX_LINES}" max_kb="${3:-$LOG_MAX_KB}"
  local lines size tmp
  [ -f "$log" ] || return 0

  # 先按字节兜底（防单行超长把文件撑爆）
  case "$max_kb" in '' | *[!0-9]*) ;; *)
    if [ "$max_kb" -gt 0 ]; then
      size="$(wc -c <"$log" 2>/dev/null | tr -d '[:space:]')"
      case "$size" in '' | *[!0-9]*) size=0 ;; esac
      if [ "$size" -gt $((max_kb * 1024)) ]; then
        tmp="$(mktemp "${log}.trim.XXXXXX")" || return 0
        if tail -c $((max_kb * 1024)) "$log" >"$tmp" 2>/dev/null; then
          cat "$tmp" >"$log" 2>/dev/null || true
        fi
        rm -f "$tmp" 2>/dev/null
      fi
    fi
    ;;
  esac

  # 再按行数裁剪
  case "$max_lines" in '' | *[!0-9]*) return 0 ;; esac
  [ "$max_lines" -gt 0 ] || return 0
  lines="$(wc -l <"$log" 2>/dev/null | tr -d '[:space:]')"
  case "$lines" in '' | *[!0-9]*) return 0 ;; esac
  [ "$lines" -gt "$max_lines" ] || return 0

  tmp="$(mktemp "${log}.trim.XXXXXX")" || return 0
  if tail -n "$max_lines" "$log" >"$tmp" 2>/dev/null; then
    cat "$tmp" >"$log" 2>/dev/null || true
  fi
  rm -f "$tmp" 2>/dev/null
}

trim_all_logs() {
  mkdir -p "$LOG_DIR" 2>/dev/null || true
  trim_log_file "$APP_LOG"       "$LOG_MAX_LINES"      "$LOG_MAX_KB"
  trim_log_file "$WATCHDOG_LOG"  "$LOG_MAX_LINES"      "$LOG_MAX_KB"
  trim_log_file "$CRON_LOG"      "$CRON_LOG_MAX_LINES" "$CRON_LOG_MAX_KB"
}

# ---------- 托管方式诊断 ----------
systemd_diagnose() {
  if ! has_cmd systemctl; then
    printf '未安装 systemctl 命令'
    return 1
  fi
  if [ -d /run/systemd/system ]; then
    return 0
  fi
  local pid1_comm=""
  if [ -r /proc/1/comm ]; then
    pid1_comm="$(tr -d '\n' </proc/1/comm 2>/dev/null)"
  fi
  if [ -n "$pid1_comm" ] && [ "$pid1_comm" != "systemd" ]; then
    printf 'PID 1 不是 systemd（当前为 %s，常见于 LXC / Docker 容器）' "${pid1_comm:-未知}"
    return 1
  fi
  printf 'systemd 未作为系统服务管理器运行（/run/systemd/system 不存在）'
  return 1
}

systemd_available() {
  systemd_diagnose >/dev/null 2>&1
}

systemd_unit_installed() {
  [ -f "$SYSTEMD_UNIT_FILE" ]
}

systemd_managed() {
  systemd_available && systemd_unit_installed
}

systemd_service_active() {
  systemd_managed && systemctl is-active --quiet "$SYSTEMD_UNIT_NAME"
}

hosting_mode() {
  if systemd_managed; then
    printf 'systemd'
  elif app_running || watchdog_running; then
    printf 'watchdog'
  else
    printf 'none'
  fi
}

hosting_mode_label() {
  case "$(hosting_mode)" in
    systemd) printf 'systemd（开机自启，崩溃自动重启）' ;;
    watchdog) printf 'shell 看门狗（每 %ss 检查，崩溃自动拉起）' "$WATCHDOG_INTERVAL" ;;
    none) printf '未运行' ;;
  esac
}

# ---------- 开机自启（crontab @reboot） ----------
autostart_installed() {
  has_cmd crontab || return 1
  crontab -l 2>/dev/null | grep -qF "$AUTOSTART_TAG"
}

autostart_line() {
  printf '@reboot cd %q && nohup bash %q start >>%q 2>&1 & %s' \
    "$PROJECT_DIR" "$SCRIPT_PATH" "$CRON_LOG" "$AUTOSTART_TAG"
}

setup_autostart() {
  section "配置开机自启（crontab @reboot）"
  if systemd_managed; then
    warn "当前已用 systemd 托管，无需 crontab；开机自启由 systemd 保证"
    return 0
  fi
  if ! has_cmd crontab; then
    fail "未安装 crontab 命令"
    printf '  安装方式（挑一条执行后重试）：\n'
    printf '    Debian/Ubuntu：apt-get install -y cron && service cron start\n'
    printf '    CentOS/Rocky ：dnf install -y cronie && systemctl enable --now crond\n'
    printf '    Alpine       ：apk add busybox-initscripts && rc-update add crond\n'
    printf '  注意：容器里若没有 init 系统，cron 服务需要由宿主或 supervisor 拉起\n'
    return 1
  fi
  local tmp
  tmp="$(mktemp)" || return 1
  crontab -l 2>/dev/null | grep -vF "$AUTOSTART_TAG" > "$tmp" || true
  printf '%s\n' "$(autostart_line)" >> "$tmp"
  if crontab "$tmp"; then
    rm -f "$tmp"
    ok "已写入 crontab：@reboot 时自动执行 ./run.sh start"
    printf '  crontab 记录：%s\n' "$(autostart_line)"
    printf '  说明：容器启动后由 cron 触发，会走 shell 看门狗模式（无需 systemd）\n'
    if ! crontab -l 2>/dev/null | grep -q .; then
      warn "crontab 内容为空，请确认 cron 服务已启用"
    fi
    return 0
  fi
  rm -f "$tmp"
  fail "写入 crontab 失败"
  return 1
}

remove_autostart() {
  section "取消开机自启"
  if ! has_cmd crontab; then
    info "未安装 crontab，无需处理"
    return 0
  fi
  if ! autostart_installed; then
    info "未发现开机自启记录"
    return 0
  fi
  local tmp
  tmp="$(mktemp)" || return 1
  crontab -l 2>/dev/null | grep -vF "$AUTOSTART_TAG" > "$tmp" || true
  if crontab "$tmp"; then
    rm -f "$tmp"
    ok "已移除开机自启记录"
    return 0
  fi
  rm -f "$tmp"
  fail "更新 crontab 失败"
  return 1
}

# ---------- 进程 / 端口 / 日志辅助 ----------
timestamp() { date '+%Y-%m-%d %H:%M:%S'; }

app_port() {
  local port="${PORT:-}"
  if [ -z "$port" ]; then
    port="$(read_env_value PORT 8000)"
  fi
  case "$port" in
    '' | *[!0-9]*) port=8000 ;;
  esac
  printf '%s' "$port"
}

port_in_use() {
  local port="$1"
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    return 0
  fi
  if has_cmd nc; then
    nc -z -w 1 127.0.0.1 "$port" >/dev/null 2>&1
    return $?
  fi
  return 1
}

port_owner() {
  local port="$1"
  if has_cmd ss; then
    ss -ltnp 2>/dev/null | grep -E "[:.]${port}[[:space:]]" | head -n 3
  elif has_cmd netstat; then
    netstat -ltnp 2>/dev/null | grep -E "[:.]${port}[[:space:]]" | head -n 3
  elif has_cmd lsof; then
    lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | head -n 3
  fi
  return 0
}

port_listener_pids() {
  local port="$1"
  {
    if has_cmd ss; then
      ss -H -ltnp "sport = :$port" 2>/dev/null | grep -oE 'pid=[0-9]+' | cut -d= -f2 || true
    fi
    if has_cmd lsof; then
      lsof -t -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null || true
    fi
  } | sort -u
}

process_name() {
  local name
  name="$(ps -p "$1" -o comm= 2>/dev/null | tr -d '\r\n')"
  printf '%s' "$name"
}

action_kill_port() {
  local port="${1:-}" answer="" pid="" owns_app=false name=""
  local -a pids=() remaining=()
  if [ -z "$port" ]; then
    printf '  输入要释放的 TCP 端口：'
    read -r port || return 0
  fi
  case "$port" in
    '' | *[!0-9]*) fail "端口必须是 1 到 65535 的数字"; return 2 ;;
  esac
  if [ "${#port}" -gt 5 ] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
    fail "端口必须在 1 到 65535 之间"
    return 2
  fi
  if ! has_cmd ss && ! has_cmd lsof; then
    fail "缺少 ss/lsof，无法安全识别监听进程；请安装 iproute2 或 lsof 后重试"
    return 1
  fi
  mapfile -t pids < <(port_listener_pids "$port")
  if [ "${#pids[@]}" -eq 0 ]; then
    if has_cmd ss; then
      ss -H -ltn "sport = :$port" 2>/dev/null
    fi
    info "TCP 端口 $port 没有可识别的监听进程"
    return 0
  fi
  printf '  TCP 端口 %s 的监听进程：\n' "$port"
  for pid in "${pids[@]}"; do
    case "$pid" in '' | *[!0-9]*) continue ;; esac
    name="$(process_name "$pid")"
    if [ -n "$name" ]; then
      printf '    PID %-7s %s\n' "$pid" "$name"
    else
      printf '    PID %s（进程详情不可读）\n' "$pid"
    fi
    if [ -n "$APP_BINARY" ] && proc_matches "$pid" "$APP_BINARY"; then
      owns_app=true
    fi
  done
  printf '  确认结束以上进程并释放端口（先 SIGTERM，未退出则 SIGKILL）？[y/N]：'
  read -r answer || answer=""
  case "$answer" in y | Y | yes | YES) ;; *) info "已取消，未结束任何进程"; return 0 ;; esac

  if [ "$owns_app" = true ]; then
    warn "检测到当前 Option Scope 服务；将通过正常停服流程停止它"
    stop_app || return 1
    mapfile -t pids < <(port_listener_pids "$port")
    if [ "${#pids[@]}" -gt 0 ]; then
      warn "端口上出现了其他监听进程；为避免结束未确认的进程，请重新运行本功能检查"
      return 1
    fi
  fi
  if [ "${#pids[@]}" -gt 0 ]; then
    local waited=0 signal_status=0
    for pid in "${pids[@]}"; do
      if [ "$pid" = 1 ]; then
        warn "拒绝结束 PID 1（系统初始化进程）"
        continue
      fi
      name="$(process_name "$pid")"
      printf '  正在结束 %s（PID %s，SIGTERM）\n' "${name:-未知进程}" "$pid"
      kill -TERM "$pid" 2>/dev/null || run_root kill -TERM "$pid" || signal_status=1
    done
    [ "$signal_status" -eq 0 ] || warn "部分进程未能发送 SIGTERM；可能需要更高权限"
    while [ "$waited" -lt 5 ]; do
      mapfile -t remaining < <(port_listener_pids "$port")
      [ "${#remaining[@]}" -gt 0 ] || break
      sleep 1
      waited=$((waited + 1))
    done
    mapfile -t remaining < <(port_listener_pids "$port")
    local -a force_pids=()
    for pid in "${remaining[@]}"; do
      [ "$pid" = 1 ] || force_pids+=("$pid")
    done
    if [ "${#force_pids[@]}" -gt 0 ]; then
      local force_status=0 force_pid force_name
      for force_pid in "${force_pids[@]}"; do
        force_name="$(process_name "$force_pid")"
        printf '  %s（PID %s）未响应 SIGTERM，发送 SIGKILL\n' "${force_name:-未知进程}" "$force_pid"
        kill -KILL "$force_pid" 2>/dev/null || run_root kill -KILL "$force_pid" || force_status=1
      done
      [ "$force_status" -eq 0 ] || return 1
      ok "已发送 SIGKILL"
      # The kernel needs a moment to drop the listener after SIGKILL.
      local settled=0
      while [ "$settled" -lt 3 ] && port_in_use "$port"; do
        sleep 1
        settled=$((settled + 1))
      done
    else
      ok "监听进程已退出"
    fi
  fi
  if port_in_use "$port"; then
    warn "端口 $port 仍有响应，可能有其他进程刚刚接管；请重新检查"
    return 1
  fi
  ok "TCP 端口 $port 已释放"
}

health_host() {
  local host
  host="$(read_env_value HOST 0.0.0.0)"
  case "$host" in
    '' | 0.0.0.0 | :: | 0:0:0:0:0:0:0:0 | '*') host=127.0.0.1 ;;
  esac
  printf '%s' "$host"
}

health_url_host() {
  local host="$1"
  case "$host" in
    *:*) printf '[%s]' "$host" ;;
    *) printf '%s' "$host" ;;
  esac
}

health_ok() {
  local host url
  host="$(health_host)"
  url="http://$(health_url_host "$host"):$(app_port)/health"
  if has_cmd curl; then
    curl -fsS --max-time 3 "$url" >/dev/null 2>&1
    return $?
  fi
  if has_cmd wget; then
    wget -q -T 3 -O /dev/null "$url" >/dev/null 2>&1
    return $?
  fi
  (
    local response
    exec 2>/dev/null
    exec 3<>"/dev/tcp/$host/$(app_port)" || exit 1
    printf 'GET /health HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n' "$(health_url_host "$host")" >&3 || exit 1
    IFS= read -r -t 3 response <&3 || exit 1
    case "$response" in
      'HTTP/1.0 200 '* | 'HTTP/1.1 200 '*) exit 0 ;;
      *) exit 1 ;;
    esac
  )
}

proc_matches() {
  local pid="$1" keyword="$2" cmd cwd
  [ -n "$keyword" ] || return 1
  [ -d "/proc/$pid" ] || return 1
  cmd="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)" || return 1
  case "$cmd" in
    *"$keyword"*) ;;
    *) return 1 ;;
  esac
  cwd="$(readlink "/proc/$pid/cwd" 2>/dev/null)"
  [ "$cwd" = "$PROJECT_DIR" ]
}

scan_pids() {
  local keyword="$1" pid
  [ -n "$keyword" ] || return 0
  [ -d /proc ] || return 0
  for pid in /proc/[0-9]*; do
    pid="${pid#/proc/}"
    [ "$pid" = "$$" ] && continue
    if proc_matches "$pid" "$keyword"; then
      printf '%s\n' "$pid"
    fi
  done
}

resolve_pid() {
  local pid_file="$1" keyword="$2" pid=""
  if [ -f "$pid_file" ]; then
    pid="$(tr -d '[:space:]' <"$pid_file" 2>/dev/null)"
  fi
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    if [ ! -d /proc ] || proc_matches "$pid" "$keyword"; then
      printf '%s' "$pid"
      return 0
    fi
  fi
  pid="$(scan_pids "$keyword" | head -n 1)"
  if [ -n "$pid" ]; then
    printf '%s' "$pid" >"$pid_file" 2>/dev/null
    printf '%s' "$pid"
    return 0
  fi
  if [ -f "$pid_file" ]; then
    rm -f "$pid_file" 2>/dev/null
  fi
  return 1
}

app_pid() {
  local pid=""
  [ -n "$APP_BINARY" ] || return 1
  if [ -f "$APP_PID_FILE" ]; then
    pid="$(tr -d '[:space:]' <"$APP_PID_FILE" 2>/dev/null)"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      if [ ! -d /proc ] || proc_matches "$pid" "$APP_BINARY"; then
        printf '%s' "$pid"
        return 0
      fi
    fi
  fi
  pid="$(scan_pids "$APP_BINARY" | head -n 1)"
  if [ -n "$pid" ]; then
    printf '%s\n' "$pid" >"$APP_PID_FILE" 2>/dev/null || true
    printf '%s' "$pid"
    return 0
  fi
  [ -f "$APP_PID_FILE" ] && rm -f "$APP_PID_FILE" 2>/dev/null || true
  return 1
}

watchdog_pid() { resolve_pid "$WATCHDOG_PID_FILE" "__watchdog"; }
app_running() { app_pid >/dev/null 2>&1; }
watchdog_running() { watchdog_pid >/dev/null 2>&1; }

process_uptime() {
  local etime=""
  if has_cmd ps; then
    etime="$(ps -o etime= -p "$1" 2>/dev/null | tr -d '[:space:]')"
  fi
  [ -n "$etime" ] || etime="未知"
  printf '%s' "$etime"
}

local_ip() {
  local ip=""
  if has_cmd hostname; then
    ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  fi
  if [ -z "$ip" ] && has_cmd ip; then
    ip="$(ip route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }}')"
  fi
  [ -n "$ip" ] || ip="127.0.0.1"
  printf '%s' "$ip"
}

show_access_url() {
  local port host access_key
  port="$(app_port)"
  host="$(read_env_value HOST 0.0.0.0)"
  access_key="$(read_env_value ACCESS_KEY "")"
  if [ -n "$access_key" ]; then
    printf '  本机访问：http://127.0.0.1:%s/?key=%s\n' "$port" "$access_key"
  else
    printf '  本机访问：http://127.0.0.1:%s\n' "$port"
  fi
  if [ "$host" != "127.0.0.1" ] && [ "$host" != "localhost" ]; then
    if [ -n "$access_key" ]; then
      printf '  局域网访问：http://%s:%s/?key=%s\n' "$(local_ip)" "$port" "$access_key"
    else
      printf '  局域网访问：http://%s:%s\n' "$(local_ip)" "$port"
    fi
  fi
}

tail_log() {
  local log="$1" lines="${2:-30}"
  if [ -s "$log" ]; then
    printf '%s最近 %s 行（%s）%s\n' "$C_BOLD" "$lines" "$log" "$C_RESET"
    tail -n "$lines" "$log"
  else
    info "日志为空：$log"
  fi
}

human_size() {
  local bytes="$1"
  case "$bytes" in
    '' | *[!0-9]*) printf '未知'; return 0 ;;
  esac
  if [ "$bytes" -ge 1073741824 ]; then
    printf '%d.%dG' $((bytes / 1073741824)) $(((bytes % 1073741824) / 107374182))
  elif [ "$bytes" -ge 1048576 ]; then
    printf '%d.%dM' $((bytes / 1048576)) $(((bytes % 1048576) / 104857))
  elif [ "$bytes" -ge 1024 ]; then
    printf '%d.%dK' $((bytes / 1024)) $(((bytes % 1024) / 102))
  else
    printf '%dB' "$bytes"
  fi
}

mem_available_mb() {
  local kb
  kb="$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo 2>/dev/null)"
  [ -n "$kb" ] || kb="$(awk '/^MemFree:/ {print $2}' /proc/meminfo 2>/dev/null)"
  case "$kb" in
    '' | *[!0-9]*) printf '' ;;
    *) printf '%d' $((kb / 1024)) ;;
  esac
}

mem_total_mb() {
  local kb
  kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null)"
  case "$kb" in
    '' | *[!0-9]*) printf '' ;;
    *) printf '%d' $((kb / 1024)) ;;
  esac
}

mem_swap_mb() {
  local kb
  kb="$(awk '/^SwapTotal:/ {print $2}' /proc/meminfo 2>/dev/null)"
  case "$kb" in
    '' | *[!0-9]*) printf '' ;;
    *) printf '%d' $((kb / 1024)) ;;
  esac
}

cgroup_mem_limit_mb() {
  local value=""
  if [ -r /sys/fs/cgroup/memory.max ]; then
    value="$(cat /sys/fs/cgroup/memory.max 2>/dev/null)"
  elif [ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]; then
    value="$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null)"
  fi
  case "$value" in
    '' | max | *[!0-9]*) printf '' ;;
    *)
      if [ "$value" -ge 1152921504606846976 ] 2>/dev/null; then
        printf ''
      else
        printf '%d' $((value / 1048576))
      fi
      ;;
  esac
}

memory_summary() {
  local avail total swap limit text
  avail="$(mem_available_mb)"; total="$(mem_total_mb)"
  swap="$(mem_swap_mb)"; limit="$(cgroup_mem_limit_mb)"
  text="可用 ${avail:-未知}M / 总 ${total:-未知}M，Swap ${swap:-未知}M"
  [ -n "$limit" ] && text="$text，容器内存上限 ${limit}M"
  printf '%s' "$text"
}

cgroup_mem_current_mb() {
  local value=""
  if [ -r /sys/fs/cgroup/memory.current ]; then
    value="$(cat /sys/fs/cgroup/memory.current 2>/dev/null)"
  elif [ -r /sys/fs/cgroup/memory/memory.usage_in_bytes ]; then
    value="$(cat /sys/fs/cgroup/memory/memory.usage_in_bytes 2>/dev/null)"
  fi
  case "$value" in
    '' | *[!0-9]*) printf '' ;;
    *) printf '%d' $((value / 1048576)) ;;
  esac
}

process_rss_mb() {
  local pid="$1" kb
  [ -n "$pid" ] || return 0
  kb="$(ps -o rss= -p "$pid" 2>/dev/null | tr -d '[:space:]')"
  case "$kb" in
    '' | *[!0-9]*) return 0 ;;
  esac
  printf '%d' $((kb / 1024))
}

# Print the top memory consumers as "PID|USER|RSS_KB|COMMAND" so the caller can
# format sizes without depending on awk's locale.
top_memory_rows() {
  local limit="$1"
  if ps -eo pid=,user=,rss=,args= >/dev/null 2>&1; then
    ps -eo pid=,user=,rss=,args= 2>/dev/null | sort -k3,3 -nr | head -n "$limit" |
      awk '{ cmd = $4; for (i = 5; i <= NF; i++) cmd = cmd " " $i; if (length(cmd) > 60) cmd = substr(cmd, 1, 57) "..."; printf "%s|%s|%s|%s\n", $1, $2, $3, cmd }'
  elif ps aux >/dev/null 2>&1; then
    ps aux 2>/dev/null | awk 'NR > 1 {
      cmd = "";
      for (i = 11; i <= NF; i++) cmd = cmd (i > 11 ? " " : "") $i;
      if (length(cmd) > 60) cmd = substr(cmd, 1, 57) "...";
      printf "%10d|%s|%s|%s\n", $6, $2, $1, cmd;
    }' | sort -t'|' -k1,1 -nr | head -n "$limit" |
      awk -F'|' '{ printf "%s|%s|%s|%s\n", $2, $3, $1 + 0, $4 }'
  fi
}

action_memory() {
  local limit_count total avail swap cap current used pid rss rows pid_value user rss_kb cmd
  limit_count="${1:-10}"
  case "$limit_count" in
    '' | *[!0-9]*) limit_count=10 ;;
  esac
  total="$(mem_total_mb)"; avail="$(mem_available_mb)"; swap="$(mem_swap_mb)"
  cap="$(cgroup_mem_limit_mb)"; current="$(cgroup_mem_current_mb)"
  section "内存使用"
  case "$total" in
    '' | *[!0-9]*)
      printf '  物理内存：未知（读取 /proc/meminfo 失败）\n'
      ;;
    *)
      used=""
      [ -n "$avail" ] && used=$((total - avail))
      printf '  物理内存：总 %sM，已用 %sM，可用 %sM' "$total" "${used:-未知}" "${avail:-未知}"
      [ -n "$used" ] && [ "$total" -gt 0 ] && printf '（使用率 %s%%）' $((used * 100 / total))
      printf '\n'
      ;;
  esac
  case "$swap" in
    '' | *[!0-9]*) ;;
    0) printf '  Swap：未配置\n' ;;
    *) printf '  Swap：总 %sM\n' "$swap" ;;
  esac
  if [ -n "$cap" ]; then
    printf '  容器内存上限：%sM' "$cap"
    [ -n "$current" ] && printf '，当前用量 %sM（%s%%）' "$current" $((cap > 0 ? current * 100 / cap : 0))
    printf '\n'
  fi
  pid_value="$(app_pid 2>/dev/null || true)"
  if [ -n "$pid_value" ]; then
    rss="$(process_rss_mb "$pid_value")"
    if [ -n "$rss" ]; then
      printf '  本应用：PID %s，常驻内存 %sM' "$pid_value" "$rss"
      case "$total" in
        '' | *[!0-9]*) ;;
        *) [ "$total" -gt 0 ] && printf '，占物理内存 %s%%' $((rss * 100 / total)) ;;
      esac
      printf '\n'
    else
      printf '  本应用：运行中（PID %s），但读取 RSS 失败\n' "$pid_value"
    fi
  else
    printf '  本应用：未运行\n'
  fi
  printf '\n  内存占用排行（按 RSS，前 %s 名）：\n' "$limit_count"
  rows="$(top_memory_rows "$limit_count")"
  if [ -z "$rows" ]; then
    printf '  无法读取进程列表（ps 不可用）\n'
  else
    printf '  %-8s %-10s %9s %6s  %s\n' "PID" "USER" "RSS" "MEM%" "COMMAND"
    while IFS='|' read -r pid user rss_kb cmd; do
      case "$rss_kb" in
        '' | *[!0-9]*) continue ;;
      esac
      printf '  %-8s %-10s %9s' "$pid" "$user" "$(human_size $((rss_kb * 1024)))"
      case "$total" in
        '' | *[!0-9]*) printf ' %6s  ' "--" ;;
        *) printf ' %5s%%  ' $((total > 0 ? rss_kb / 1024 * 100 / total : 0)) ;;
      esac
      printf '%s\n' "$cmd"
    done <<<"$rows"
    printf '\n  说明：常驻内存为 RSS（含共享页，未按去重折算），容器内只统计本容器的进程。\n'
  fi
  return 0
}

database_size_bytes() {
  local db_path="$1" total=0 suffix candidate size
  for suffix in "" "-wal" "-shm"; do
    candidate="${db_path}${suffix}"
    if [ -f "$candidate" ]; then
      size="$(wc -c <"$candidate" 2>/dev/null | tr -d '[:space:]')"
      case "$size" in
        '' | *[!0-9]*) size=0 ;;
      esac
      total=$((total + size))
    fi
  done
  printf '%s' "$total"
}

resolve_db_path() {
  local db_path
  db_path="$(read_env_value DATABASE_PATH data/options.db)"
  case "$db_path" in
    /*) printf '%s' "$db_path" ;;
    *) printf '%s/%s' "$PROJECT_DIR" "$db_path" ;;
  esac
}

# ---------- 应用启停与看门狗 ----------
wd_log() { printf '[%s] [看门狗] %s\n' "$(timestamp)" "$*"; }

spawn_detached() {
  local log="$1"
  shift
  if has_cmd setsid; then
    setsid nohup "$@" >>"$log" 2>&1 </dev/null &
  else
    nohup "$@" >>"$log" 2>&1 </dev/null &
  fi
  printf '%s\n' "$!"
  return 0
}

systemd_service_account() {
  local owner group
  if has_cmd stat; then
    owner="$(stat -c '%U' "$PROJECT_DIR" 2>/dev/null || true)"
    group="$(stat -c '%G' "$PROJECT_DIR" 2>/dev/null || true)"
  fi
  [ -n "$owner" ] || owner="${SUDO_USER:-$(id -un)}"
  [ -n "$group" ] || group="$(id -gn "$owner" 2>/dev/null || id -gn)"
  printf '%s\t%s' "$owner" "$group"
}

wait_for_health() {
  local timeout="$1" stop_if_app_dies="${2:-false}" elapsed=0 missing=0 pid=""
  while [ "$elapsed" -lt "$timeout" ]; do
    if health_ok; then
      printf '%s' "$elapsed"
      return 0
    fi
    if [ "$stop_if_app_dies" = true ]; then
      pid="$(app_pid 2>/dev/null || true)"
      if [ -z "$pid" ]; then
        missing=$((missing + 1))
        [ "$missing" -lt 5 ] || return 2
      else
        missing=0
      fi
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  return 1
}

systemd_start_service() {
  local waited=0
  run_root systemctl start "$SYSTEMD_UNIT_NAME" || return 1
  if waited="$(wait_for_health 45)"; then
    ok "systemd 服务已启动，健康检查通过（耗时 ${waited}s）"
    return 0
  fi
  fail "systemd 服务启动后 45s 内未通过健康检查"
  run_root systemctl status "$SYSTEMD_UNIT_NAME" --no-pager 2>&1 | tail -n 30 || true
  systemd_stop_service || warn "停止 systemd 服务失败，请检查 $SYSTEMD_UNIT_NAME 状态"
  return 1
}

systemd_stop_service() {
  if systemd_service_active; then
    info "停止 systemd 服务（$SYSTEMD_UNIT_NAME）"
    run_root systemctl stop "$SYSTEMD_UNIT_NAME" || return 1
  fi
  return 0
}

rollback_systemd_install() {
  local backup="$1" had_unit="$2" was_active="$3" was_enabled="$4" was_app="$5" was_watchdog="$6"
  local failed=0
  if [ "$had_unit" = true ]; then
    run_root install -m 0644 "$backup" "$SYSTEMD_UNIT_FILE" || failed=1
  else
    run_root rm -f "$SYSTEMD_UNIT_FILE" || failed=1
  fi
  run_root systemctl daemon-reload || failed=1
  run_root systemctl disable "$SYSTEMD_UNIT_NAME" >/dev/null 2>&1 || true
  case "$was_enabled" in
    enabled | enabled-runtime) run_root systemctl enable "$SYSTEMD_UNIT_NAME" || failed=1 ;;
    *) ;;
  esac
  if [ "$had_unit" = true ]; then
    if [ "$was_active" = true ]; then
      run_root systemctl start "$SYSTEMD_UNIT_NAME" || failed=1
    else
      run_root systemctl stop "$SYSTEMD_UNIT_NAME" >/dev/null 2>&1 || true
    fi
  elif [ "$was_app" = true ]; then
    start_app || failed=1
  elif [ "$was_watchdog" = true ]; then
    start_watchdog || failed=1
  fi
  return "$failed"
}

install_systemd_service() {
  section "安装 systemd 服务"
  if ! systemd_available; then
    local reason
    reason="$(systemd_diagnose)"
    warn "当前环境不支持 systemd：$reason"
    printf '\n'
    printf '  这意味着：应用无法由 systemd 托管（不能依赖“systemctl enable”实现开机自启）。\n'
    printf '  替代方案：\n'
    printf '   1) 使用内置 shell 看门狗：./run.sh start 后会自动拉起一个看门狗进程，\n'
    printf '      每 %s 秒检查一次应用存活，崩溃/健康检查失败会自动重启。\n' "$WATCHDOG_INTERVAL"
    printf '   2) 需要开机自启：./run.sh enable-autostart（写入 crontab @reboot）。\n'
    printf '      前提是容器里启用了 cron 服务，或宿主机在容器启动时调用本脚本。\n'
    printf '   3) 若你希望启用 systemd，请参考：\n'
    printf '      Proxmox LXC：编辑 /etc/pve/lxc/<vmid>.conf，加入\n'
    printf '        lxc.apparmor.profile: unconfined\n'
    printf '        lxc.cap.drop:\n'
    printf '      然后重启容器，再回来安装 systemd 服务。\n'
    printf '\n'
    return 1
  fi
  local tmp="" backup="" service_user="" service_group=""
  local had_unit=false was_active=false was_enabled="disabled" was_app=false was_watchdog=false
  ensure_runtime || return 1
  if systemd_unit_installed; then
    had_unit=true
    backup="$(mktemp "${TMPDIR:-/tmp}/option-scope.service.old.XXXXXX")" || return 1
    run_root cp -p "$SYSTEMD_UNIT_FILE" "$backup" || { rm -f "$backup"; return 1; }
    systemd_service_active && was_active=true
    was_enabled="$(run_root systemctl is-enabled "$SYSTEMD_UNIT_NAME" 2>/dev/null || true)"
  else
    backup="$(mktemp "${TMPDIR:-/tmp}/option-scope.service.old.XXXXXX")" || return 1
  fi
  app_running && was_app=true
  watchdog_running && was_watchdog=true
  IFS=$'\t' read -r service_user service_group <<<"$(systemd_service_account)"
  if [ -z "$service_user" ] || [ "$service_user" = "root" ]; then
    service_user="root"
    service_group="root"
    warn "项目目录属于 root，将以 root 运行 systemd 服务；如需最小权限，请先把项目目录交给普通用户"
  fi
  tmp="$(mktemp "${TMPDIR:-/tmp}/option-scope.service.XXXXXX")" || return 1
  cat >"$tmp" <<UNIT
[Unit]
Description=Option Scope stock and options dashboard
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory="$PROJECT_DIR"
ExecStart=/bin/bash "$SCRIPT_PATH" foreground
User=$service_user
Group=$service_group
Restart=always
RestartSec=3
TimeoutStartSec=120
TimeoutStopSec=30
KillSignal=SIGINT
KillMode=mixed
LimitNOFILE=8192
StandardOutput=journal
StandardError=journal
SyslogIdentifier=option-scope

[Install]
WantedBy=multi-user.target
UNIT
  stop_app || {
    rm -f "$tmp"
    rollback_systemd_install "$backup" "$had_unit" "$was_active" "$was_enabled" "$was_app" "$was_watchdog" || warn "恢复旧服务状态失败"
    rm -f "$backup"
    return 1
  }
  if ! run_root install -m 0644 "$tmp" "$SYSTEMD_UNIT_FILE"; then
    rm -f "$tmp"
    rollback_systemd_install "$backup" "$had_unit" "$was_active" "$was_enabled" "$was_app" "$was_watchdog" || warn "恢复旧服务状态失败"
    rm -f "$backup"
    return 1
  fi
  rm -f "$tmp"
  if ! run_root systemctl daemon-reload || ! run_root systemctl enable "$SYSTEMD_UNIT_NAME" || ! systemd_start_service; then
    fail "systemd 安装未完成，正在恢复原 unit 与服务状态"
    rollback_systemd_install "$backup" "$had_unit" "$was_active" "$was_enabled" "$was_app" "$was_watchdog" || warn "恢复旧服务状态失败，请检查 $SYSTEMD_UNIT_NAME"
    rm -f "$backup"
    return 1
  fi
  rm -f "$backup"
  ok "systemd 服务已安装并设置为开机自动启动"
  printf '  查看状态：systemctl status %s\n' "$SYSTEMD_UNIT_NAME"
  printf '  查看日志：journalctl -u %s -f\n' "$SYSTEMD_UNIT_NAME"
  return 0
}

remove_systemd_service() {
  section "移除 systemd 服务"
  if ! systemd_available; then
    local reason
    reason="$(systemd_diagnose)"
    warn "当前环境不支持 systemd：$reason"
    return 0
  fi
  if ! systemd_unit_installed; then
    info "未找到 $SYSTEMD_UNIT_FILE"
    return 0
  fi
  stop_app || return 1
  run_root systemctl disable --now "$SYSTEMD_UNIT_NAME" >/dev/null 2>&1 || true
  run_root rm -f "$SYSTEMD_UNIT_FILE" || return 1
  run_root systemctl daemon-reload || return 1
  ok "systemd 服务已移除"
  return 0
}

foreground_app() {
  ensure_runtime || return 1
  cd "$PROJECT_DIR" || return 1
  exec "$APP_BINARY"
}

start_app_internal() {
  local port existing waited=0 pid="" wait_status=0
  port="$(app_port)"
  existing="$(app_pid 2>/dev/null || true)"
  if [ -n "$existing" ]; then
    info "应用已在运行（PID $existing），跳过启动"
    return 0
  fi
  ensure_app_binary || return 1
  if port_in_use "$port"; then
    fail "端口 $port 已被占用，无法启动应用"
    port_owner "$port" | sed 's/^/    /'
    fail "若是上一次没停干净的进程：先执行 ./run.sh stop；也可以改 .env 里的 PORT 换端口"
    return 1
  fi
  mkdir -p "$RUN_DIR" "$LOG_DIR"
  trim_all_logs
  printf '[%s] 启动应用：%s（端口 %s）\n' "$(timestamp)" "$APP_BINARY" "$port" >>"$APP_LOG"
  pid="$( cd "$PROJECT_DIR" && spawn_detached "$APP_LOG" "$APP_BINARY" )" || return 1
  wait_status=0
  waited="$(wait_for_health "$START_TIMEOUT" true)" || wait_status=$?
  if [ "$wait_status" -eq 0 ]; then
    pid="$(app_pid 2>/dev/null || true)"
    ok "应用已启动（PID ${pid:-未知}，端口 $port），健康检查通过，耗时 ${waited}s"
    return 0
  fi
  if [ "$wait_status" -eq 2 ]; then
    fail "应用进程快速退出，未能启动（端口 $port）"
  else
    fail "启动失败：${START_TIMEOUT}s 内未通过健康检查"
  fi
  if [ -n "$pid" ]; then
    stop_pid "$pid" "启动失败的应用"
    if [ -f "$APP_PID_FILE" ] && [ "$(tr -d '[:space:]' <"$APP_PID_FILE" 2>/dev/null)" = "$pid" ]; then
      rm -f "$APP_PID_FILE" 2>/dev/null || true
    fi
  fi
  tail_log "$APP_LOG" 20
  return 1
}

start_watchdog() {
  local pid waited=0
  pid="$(watchdog_pid 2>/dev/null || true)"
  if [ -n "$pid" ]; then
    info "看门狗已在运行（PID $pid），跳过启动"
    return 0
  fi
  mkdir -p "$RUN_DIR" "$LOG_DIR"
  printf '[%s] 启动看门狗（检查间隔 %s 秒）\n' "$(timestamp)" "$WATCHDOG_INTERVAL" >>"$WATCHDOG_LOG"
  ( cd "$PROJECT_DIR" && spawn_detached "$WATCHDOG_LOG" bash "$SCRIPT_PATH" __watchdog ) >/dev/null
  while [ "$waited" -lt 10 ]; do
    sleep 1
    waited=$((waited + 1))
    pid="$(watchdog_pid 2>/dev/null || true)"
    if [ -n "$pid" ]; then
      ok "看门狗已启动（PID $pid，每 ${WATCHDOG_INTERVAL} 秒检查一次，异常自动拉起）"
      return 0
    fi
  done
  warn "看门狗未在 10s 内就绪，请查看日志：$WATCHDOG_LOG"
  return 1
}

stop_pid() {
  local pid="$1" label="$2" waited=0
  info "停止${label}（PID $pid）"
  kill "$pid" 2>/dev/null || true
  while [ "$waited" -lt "$STOP_TIMEOUT" ] && kill -0 "$pid" 2>/dev/null; do
    sleep 1
    waited=$((waited + 1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    warn "${label}未在 ${STOP_TIMEOUT}s 内退出，强制结束"
    kill -9 "$pid" 2>/dev/null || true
    sleep 1
  fi
}

stop_watchdog() {
  local pid
  pid="$(watchdog_pid 2>/dev/null || true)"
  if [ -z "$pid" ]; then
    info "看门狗未在运行"
    return 0
  fi
  stop_pid "$pid" "看门狗"
  rm -f "$WATCHDOG_PID_FILE" 2>/dev/null || true
  ok "看门狗已停止"
}

stop_app_process() {
  local pid extra=""
  pid="$(app_pid 2>/dev/null || true)"
  if [ -z "$pid" ]; then
    info "应用未在运行"
    return 0
  fi
  stop_pid "$pid" "应用"
  if port_in_use "$(app_port)" && [ -n "$APP_BINARY" ]; then
    extra="$(scan_pids "$APP_BINARY" | head -n 1)"
    if [ -n "$extra" ]; then
      warn "发现残留进程 PID $extra，一并结束"
      stop_pid "$extra" "残留应用"
    fi
  fi
  rm -f "$APP_PID_FILE" 2>/dev/null || true
  ok "应用已停止"
}

start_managed_app() {
  if systemd_managed; then
    systemd_start_service || return 1
  else
    start_app_internal || return 1
    if start_watchdog; then
      info "当前使用 shell 看门狗托管（每 ${WATCHDOG_INTERVAL}s 检查一次）"
    else
      warn "应用已启动，但看门狗没起来（不影响使用，可稍后重试）"
    fi
  fi
  show_access_url
}

start_app() {
  section "启动应用"
  ensure_runtime || return 1
  start_managed_app
}

stop_app() {
  section "停止应用"
  stop_watchdog
  if systemd_managed; then
    systemd_stop_service || return 1
  fi
  stop_app_process
  if app_running; then
    warn "应用仍在运行，再次停止"
    stop_app_process
  fi
  if app_running; then
    fail "无法确认应用已停止"
    return 1
  fi
  return 0
}

restart_app() {
  stop_app || return 1
  start_app
}

watchdog_loop() {
  local fails=0 pid="" other=""
  mkdir -p "$RUN_DIR" "$LOG_DIR"
  if [ -f "$WATCHDOG_PID_FILE" ]; then
    other="$(tr -d '[:space:]' <"$WATCHDOG_PID_FILE" 2>/dev/null)"
    if [ -n "$other" ] && [ "$other" != "$$" ] && kill -0 "$other" 2>/dev/null; then
      wd_log "已有看门狗在运行（PID $other），本次退出"
      return 0
    fi
  fi
  printf '%s\n' "$$" >"$WATCHDOG_PID_FILE"
  wd_log "看门狗启动（PID $$，间隔 ${WATCHDOG_INTERVAL}s，连续 ${WATCHDOG_FAIL_LIMIT} 次健康检查失败才重启）"
  while true; do
    # 每轮都把日志裁剪到上限，避免 512M 小硬盘被写满
    trim_all_logs
    pid="$(app_pid 2>/dev/null || true)"
    if [ -z "$pid" ]; then
      wd_log "应用未在运行，尝试拉起（$(memory_summary)）"
      fails=0
      if start_app_internal >/dev/null 2>&1; then
        wd_log "应用已恢复"
      else
        wd_log "拉起失败，${WATCHDOG_INTERVAL}s 后重试"
        tail_log "$APP_LOG" 5 >>"$WATCHDOG_LOG" 2>/dev/null
      fi
    elif health_ok; then
      fails=0
    else
      fails=$((fails + 1))
      wd_log "健康检查失败（第 $fails 次，PID $pid）"
      if [ "$fails" -ge "$WATCHDOG_FAIL_LIMIT" ]; then
        wd_log "连续失败达到 $WATCHDOG_FAIL_LIMIT 次，重启应用"
        fails=0
        stop_app_process
        if start_app_internal >/dev/null 2>&1; then
          wd_log "应用已重启"
        else
          wd_log "重启失败，${WATCHDOG_INTERVAL}s 后重试"
        fi
      fi
    fi
    sleep "$WATCHDOG_INTERVAL"
  done
}

# ---------- 状态展示与各菜单动作 ----------
show_status() {
  local port pid db_path size limit systemd_state systemd_enabled
  section "运行状态"
  printf '  系统：%s（架构 %s / %s，包管理器 %s）\n' "$OS_NAME" "$ARCH" "${BINARY_ARCH:-未知}" "${PKG_MANAGER:-未知}"
  printf '  项目目录：%s\n' "$PROJECT_DIR"
  if [ -n "$APP_BINARY" ] && [ -x "$APP_BINARY" ]; then
    printf '  应用二进制：%s\n' "$APP_BINARY"
  else
    printf '  应用二进制：%s（缺失）\n' "${APP_BINARY:-未识别架构}"
  fi
  port="$(app_port)"
  if systemd_managed; then
    systemd_state="$(systemctl is-active "$SYSTEMD_UNIT_NAME" 2>/dev/null || true)"
    systemd_enabled="$(systemctl is-enabled "$SYSTEMD_UNIT_NAME" 2>/dev/null || true)"
    printf '  托管方式：systemd（状态 %s，开机启动 %s）\n' "${systemd_state:-未知}" "${systemd_enabled:-未知}"
  elif systemd_unit_installed; then
    printf '  托管方式：systemd 服务文件存在，但当前环境未运行 systemd\n'
  else
    printf '  托管方式：shell 看门狗（无 systemd；每 %s 秒检查一次）\n' "$WATCHDOG_INTERVAL"
  fi
  if autostart_installed; then
    printf '  开机自启：已配置（crontab @reboot）\n'
  elif systemd_managed; then
    printf '  开机自启：由 systemd 保证\n'
  else
    printf '  开机自启：未配置（菜单可设置：./run.sh enable-autostart）\n'
  fi
  pid="$(app_pid 2>/dev/null || true)"
  if [ -n "$pid" ]; then
    if health_ok; then
      printf '  应用：运行中（PID %s，已运行 %s，端口 %s 健康检查通过）\n' "$pid" "$(process_uptime "$pid")" "$port"
    else
      printf '  应用：进程存在（PID %s）但健康检查未通过，端口 %s\n' "$pid" "$port"
    fi
  else
    printf '  应用：未运行（端口 %s）\n' "$port"
  fi
  pid="$(watchdog_pid 2>/dev/null || true)"
  if [ -n "$pid" ]; then
    printf '  看门狗：运行中（PID %s）\n' "$pid"
  elif systemd_managed; then
    printf '  看门狗：未使用（由 systemd 托管）\n'
  else
    printf '  看门狗：未运行（应用异常退出后不会自动拉起）\n'
  fi
  show_access_url
  db_path="$(resolve_db_path)"
  limit="$(read_env_value DATABASE_MAX_MB 0)"
  if [ -f "$db_path" ]; then
    size="$(database_size_bytes "$db_path")"
    printf '  数据库：%s（%s，上限 %s）\n' "$db_path" "$(human_size "$size")" "$limit"
  else
    printf '  数据库：%s（尚未创建）\n' "$db_path"
  fi
  # 日志概况
  local app_lines wd_lines app_bytes wd_bytes
  app_lines="$(wc -l <"$APP_LOG" 2>/dev/null | tr -d '[:space:]')"
  wd_lines="$(wc -l <"$WATCHDOG_LOG" 2>/dev/null | tr -d '[:space:]')"
  app_bytes="$(wc -c <"$APP_LOG" 2>/dev/null | tr -d '[:space:]')"
  wd_bytes="$(wc -c <"$WATCHDOG_LOG" 2>/dev/null | tr -d '[:space:]')"
  printf '  日志：app.log %s 行 / %s，watchdog.log %s 行 / %s\n' \
    "${app_lines:-0}" "$(human_size "${app_bytes:-0}")" \
    "${wd_lines:-0}" "$(human_size "${wd_bytes:-0}")"
  printf '        裁剪上限：%s 行 / %s KB（可在 .env 外设置 LOG_MAX_LINES / LOG_MAX_KB）\n' \
    "$LOG_MAX_LINES" "$LOG_MAX_KB"
}

action_status() {
  show_status
  printf '\n'
  tail_log "$APP_LOG" 15
}

action_logs() {
  section "应用日志"
  tail_log "$APP_LOG" "${1:-80}"
  printf '  完整日志：%s\n' "$APP_LOG"
  printf '  看门狗日志：%s\n' "$WATCHDOG_LOG"
}

action_trim_logs() {
  section "裁剪日志"
  local before_app before_wd before_app_b before_wd_b
  before_app="$(wc -l <"$APP_LOG" 2>/dev/null | tr -d '[:space:]')"
  before_wd="$(wc -l <"$WATCHDOG_LOG" 2>/dev/null | tr -d '[:space:]')"
  before_app_b="$(wc -c <"$APP_LOG" 2>/dev/null | tr -d '[:space:]')"
  before_wd_b="$(wc -c <"$WATCHDOG_LOG" 2>/dev/null | tr -d '[:space:]')"
  printf '  app.log      当前 %s 行 / %s\n' "${before_app:-0}" "$(human_size "${before_app_b:-0}")"
  printf '  watchdog.log 当前 %s 行 / %s\n' "${before_wd:-0}" "$(human_size "${before_wd_b:-0}")"
  printf '  裁剪上限：%s 行 / %s KB\n' "$LOG_MAX_LINES" "$LOG_MAX_KB"
  trim_all_logs
  local after_app after_wd after_app_b after_wd_b
  after_app="$(wc -l <"$APP_LOG" 2>/dev/null | tr -d '[:space:]')"
  after_wd="$(wc -l <"$WATCHDOG_LOG" 2>/dev/null | tr -d '[:space:]')"
  after_app_b="$(wc -c <"$APP_LOG" 2>/dev/null | tr -d '[:space:]')"
  after_wd_b="$(wc -c <"$WATCHDOG_LOG" 2>/dev/null | tr -d '[:space:]')"
  ok "已裁剪："
  printf '  app.log      %s 行 / %s → %s 行 / %s\n' \
    "${before_app:-0}" "$(human_size "${before_app_b:-0}")" \
    "${after_app:-0}" "$(human_size "${after_app_b:-0}")"
  printf '  watchdog.log %s 行 / %s → %s 行 / %s\n' \
    "${before_wd:-0}" "$(human_size "${before_wd_b:-0}")" \
    "${after_wd:-0}" "$(human_size "${after_wd_b:-0}")"
  return 0
}

# ---------- 配置菜单 ----------
ask_env_value() {
  local key="$1" desc="$2" current="" input=""
  current="$(read_env_value "$key" "")"
  printf '\n  %s\n' "$desc"
  printf '  当前值：%s\n' "${current:-（空）}"
  printf '  新值（直接回车保持不变）：'
  read -r input || input=""
  if [ -z "$input" ]; then
    info "$key 保持不变"
    return 0
  fi
  write_env_value "$key" "$input" && ok "$key = $input"
  return 0
}

ask_secret_env_value() {
  local key="$1" desc="$2" current="" input=""
  current="$(read_env_value "$key" "")"
  printf '\n  %s\n' "$desc"
  printf '  当前值：%s\n' "$(mask_access_key "$current")"
  printf '  新值（直接回车保持不变，输入 clear 清空）：'
  if [ -t 0 ] && [ -t 1 ] && [ -r /dev/tty ]; then
    IFS= read -r -s input < /dev/tty || input=""
    printf '\n'
  else
    read -r input || input=""
  fi
  if [ -z "$input" ]; then
    info "$key 保持不变"
  elif [ "$input" = "clear" ]; then
    write_env_value "$key" "" && ok "$key 已清空"
  else
    write_env_value "$key" "$input" && ok "$key 已更新（值已隐藏）"
  fi
  return 0
}

mask_access_key() {
  local value="$1"
  if [ -z "$value" ]; then
    printf '（未配置）'
  elif [ "${#value}" -le 4 ]; then
    printf '****'
  else
    printf '%s****%s' "${value:0:2}" "${value:$((${#value} - 2)):2}"
  fi
}

ask_access_key() {
  local choice="" input="" generated="" current=""
  current="$(read_env_value ACCESS_KEY "")"
  printf '\n  ACCESS_KEY 当前值：%s\n' "$(mask_access_key "$current")"
  printf '  1) 手动设置新密钥\n'
  printf '  2) 自动生成 16 位密钥\n'
  printf '  3) 清空密钥（关闭访问保护）\n'
  printf '  0) 返回\n'
  printf '  请选择：'
  read -r choice || return 0
  case "$choice" in
    1)
      printf '  输入新 ACCESS_KEY（直接回车取消）：'
      read -r input || input=""
      if [ -z "$input" ]; then
        info "ACCESS_KEY 保持不变"
      else
        write_env_value ACCESS_KEY "$input" && ok "ACCESS_KEY 已更新"
      fi
      ;;
    2)
      generated="$(generate_access_key)" || {
        fail "无法生成访问密钥"
        return 1
      }
      write_env_value ACCESS_KEY "$generated" && ok "ACCESS_KEY 已重新生成：$generated"
      ;;
    3)
      printf '  清空后页面和 API 将不再要求密钥，确认请输入 yes：'
      read -r input || input=""
      if [ "$input" = "yes" ]; then
        write_env_value ACCESS_KEY "" && ok "ACCESS_KEY 已清空，访问保护已关闭"
      else
        info "已取消"
      fi
      ;;
    0 | "") return 0 ;;
    *) warn "无效选择：$choice" ;;
  esac
  return 0
}

CONFIG_ROWS=(
  'DATABASE_PATH|data/options.db|plain|SQLite 数据库文件路径'
  'HOST|0.0.0.0|plain|Web 服务监听地址'
  'PORT|8000|plain|Web 服务监听端口（1024-65535）'
  'ACCESS_KEY||access|页面和 API 访问密钥'
  'MARKET_PROXY|（空）|plain|上游行情接口代理地址（留空表示不使用代理）'
  'DEFAULT_SYMBOLS|QQQ|plain|默认页面标的（兼容逗号分隔，仅取第一项）'
  'RAW_RETENTION_DAYS|30|plain|历史数据保留天数（每天自动清理）'
  'DATABASE_MAX_MB|256M|plain|SQLite 体积上限（512 / 512M / 1G，0 表示不限制）'
  'HISTORY_MAX_AGE_SECONDS|3600|plain|日线历史回源间隔（秒）'
  'EXTREMES_MAX_AGE_SECONDS|86400|plain|历史极值回源间隔（秒）'
  'SCHEDULER_ENABLED|true|plain|是否启用定时历史清理和数据库维护（true/false）'
  'RESOURCE_MODE|auto|plain|资源模式（auto 启动时按内存/CPU 自动分配；off 不限制）'
  'ALPACA_API_KEY||secret|Alpaca API Key（夜盘股票行情）'
  'ALPACA_API_SECRET||secret|Alpaca API Secret（夜盘股票行情）'
)

show_config_summary() {
  local index=0 key default_value type description value row
  printf '  配置文件：%s\n' "$ENV_FILE"
  for row in "${CONFIG_ROWS[@]}"; do
    IFS='|' read -r key default_value type description <<<"$row"
    value="$(read_env_value "$key" "$default_value")"
    case "$type" in access | secret) value="$(mask_access_key "$value")" ;; esac
    printf ' %2d) %s=%s  # %s' "$((index + 1))" "$key" "$value" "$description"
    case "$type" in access | secret) printf '（已脱敏）' ;; esac
    printf '\n'
    index=$((index + 1))
  done
}

validate_config_values() {
  local port limit resource_mode
  port="$(read_env_value PORT 8000)"
  case "$port" in
    '' | *[!0-9]*)
      fail "PORT 必须是数字，当前值为：$port"
      return 1
      ;;
  esac
  if [ "$port" -lt 1024 ] || [ "$port" -gt 65535 ]; then
    warn "PORT=$port 不在推荐的 1024-65535 范围内（1024 以下需要 root 权限）"
  fi
  limit="$(read_env_value DATABASE_MAX_MB 0)"
  case "$(printf '%s' "$limit" | tr '[:lower:]' '[:upper:]')" in
    '' | *[!0-9MG]*) warn "DATABASE_MAX_MB=$limit 写法可能不合法，应用启动时会报错" ;;
  esac
  resource_mode="$(read_env_value RESOURCE_MODE auto)"
  case "$(printf '%s' "$resource_mode" | tr '[:upper:]' '[:lower:]')" in
    '' | auto | off) ;;
    *) fail "RESOURCE_MODE 只能是 auto 或 off"; return 1 ;;
  esac
  return 0
}

action_config() {
  local choice="" answer="" before="" after="" index key description type default_value
  section "修改配置（.env）"
  ensure_env_file || return 1
  before="$(cksum "$ENV_FILE" 2>/dev/null || true)"
  while true; do
    section "配置菜单"
    show_config_summary
    printf '  0) 返回\n'
    printf '请选择要修改的配置项：'
    read -r choice || break
    case "$choice" in
      0 | "") break ;;
      *[!0-9]* | 0*) warn "无效选择：$choice"; continue ;;
    esac
    index=$((choice - 1))
    if [ "$index" -lt 0 ] || [ "$index" -ge "${#CONFIG_ROWS[@]}" ]; then
      warn "无效选择：$choice"
      continue
    fi
    IFS='|' read -r key default_value type description <<<"${CONFIG_ROWS[index]}"
    case "$type" in
      access) ask_access_key ;;
      secret) ask_secret_env_value "$key" "$description" ;;
      *) ask_env_value "$key" "$description" ;;
    esac
  done
  validate_config_values || return 1
  after="$(cksum "$ENV_FILE" 2>/dev/null || true)"
  if [ "$before" != "$after" ]; then
    ok "配置已保存到 $ENV_FILE"
    if app_running; then
      printf '  配置需要重启后才生效，现在重启？[y/N]：'
      read -r answer || answer=""
      case "$answer" in
        y | Y | yes | YES) restart_app ;;
        *) info "稍后可从主菜单选择“重启应用”使其生效" ;;
      esac
    fi
  else
    info "未修改配置"
  fi
  return 0
}

uninstall_runtime() {
  local answer="" target
  section "卸载运行环境"
  printf '  将停止服务并删除：%s、%s\n' "$RUN_DIR" "$LOG_DIR"
  printf '  会保留：.env 配置、data 数据库、应用二进制与项目文件\n'
  printf '  确认继续？输入 yes 继续：'
  read -r answer || answer=""
  if [ "$answer" != "yes" ]; then
    info "已取消"
    return 0
  fi
  stop_app
  for target in "$RUN_DIR" "$LOG_DIR"; do
    case "$target" in
      "$PROJECT_DIR"/*)
        if [ -e "$target" ]; then
          rm -rf "$target"
          info "已删除：$target"
        fi
        ;;
      *) warn "跳过异常路径：$target" ;;
    esac
  done
  ok "运行环境已卸载"
  return 0
}

menu_status_hint() {
  local pid port
  port="$(app_port)"
  pid="$(app_pid 2>/dev/null || true)"
  if [ -n "$pid" ]; then
    if health_ok; then
      printf '  状态：%s运行中%s（端口 %s，%s）\n' "$C_GREEN" "$C_RESET" "$port" "$(hosting_mode_label)"
    else
      printf '  状态：%s进程存在但健康检查未通过%s（端口 %s）\n' "$C_YELLOW" "$C_RESET" "$port"
    fi
  else
    printf '  状态：%s未运行%s（端口 %s）\n' "$C_DIM" "$C_RESET" "$port"
  fi
  if autostart_installed; then
    printf '  开机自启：已配置\n'
  elif systemd_managed; then
    printf '  开机自启：由 systemd 保证\n'
  fi
}

run_menu() {
  local title="$1" prompt="$2" mode="$3" choice entry number label action result found
  shift 3
  while true; do
    if [ "$mode" = main ]; then
      printf '\n%s================= Option Scope 运维菜单 =================%s\n' "$C_BOLD" "$C_RESET"
    else
      section "$title"
    fi
    if [ "$mode" = main ]; then
      menu_status_hint
    fi
    for entry in "$@"; do
      IFS='|' read -r number label action <<<"$entry"
      if [ "$action" = group ]; then
        printf '\n  %s\n' "$label"
      else
        printf '  %2s) %s\n' "$number" "$label"
      fi
    done
    if [ "$mode" = main ]; then
      printf '\n%s---------------------------------------------------------%s\n' "$C_BOLD" "$C_RESET"
      printf '  输入 0 退出；直接回车刷新菜单\n'
      printf '%s=========================================================%s\n' "$C_BOLD" "$C_RESET"
    else
      printf '\n  输入 0 返回\n'
    fi
    printf '%s' "$prompt"
    if ! read -r choice; then
      return 0
    fi
    [ -n "$choice" ] || { [ "$mode" = main ] && continue || return 0; }
    found=false
    result=0
    for entry in "$@"; do
      IFS='|' read -r number label action <<<"$entry"
      if [ "$choice" = "$number" ]; then
        found=true
        case "$action" in
          quit) return 0 ;;
          return) return 0 ;;
          *) "$action"; result=$? ;;
        esac
        break
      fi
    done
    [ "$found" = true ] || warn "无效选择：$choice"
    if [ "$mode" != main ]; then
      return "$result"
    fi
  done
}

action_service() {
  if systemd_available; then
    run_menu "高级服务管理" "请选择：" once \
      '1|安装/启用 systemd 服务（生产环境推荐）|install_systemd_service' \
      '2|移除 systemd 服务（恢复 shell 看门狗）|remove_systemd_service' \
      '3|卸载运行环境（停止服务并删除 .run / logs）|uninstall_runtime' \
      '0|返回|return'
  else
    local reason
    reason="$(systemd_diagnose)"
    local options=(
      "info|systemd 不可用：$reason|show_systemd_help"
      '2|配置开机自启（crontab @reboot）|setup_autostart'
      '3|取消开机自启|remove_autostart'
      '4|卸载运行环境（停止服务并删除 .run / logs）|uninstall_runtime'
      '0|返回|return'
    )
    if autostart_installed; then
      options[1]='2|更新开机自启（已配置）|setup_autostart'
    fi
    run_menu "高级服务管理（无 systemd 模式）" "请选择：" once "${options[@]}"
  fi
}

show_systemd_help() {
  local reason
  reason="$(systemd_diagnose)"
  section "关于 systemd"
  printf '  检测结果：%s\n' "$reason"
  printf '\n'
  printf '  影响：应用无法由 systemd 托管，也不会自动开机启动。\n'
  printf '  替代：使用内置 shell 看门狗 + crontab @reboot（见本菜单第 2 项）。\n'
  printf '\n'
  printf '  如果你确实想在容器里启用 systemd：\n'
  printf '   - Proxmox LXC：编辑 /etc/pve/lxc/<vmid>.conf，加入：\n'
  printf '       lxc.apparmor.profile: unconfined\n'
  printf '       lxc.cap.drop:\n'
  printf '     然后重启容器。\n'
  printf '   - Docker：改用“宿主 systemd 拉起容器 + 容器内直接运行二进制”的架构，\n'
  printf '     容器内不建议跑 systemd。\n'
  printf '\n'
}

action_doctor() {
  local problems=0 port="" host="" avail_kb="" avail="" db_path="" db_size="" limit="" proxy="" access_key=""
  local mem_avail="" mem_total="" mem_swap=""
  section "环境自检"
  printf '  系统：%s（架构 %s / %s，包管理器 %s）\n' "$OS_NAME" "$ARCH" "${BINARY_ARCH:-未知}" "${PKG_MANAGER:-未知}"
  printf '  项目目录：%s\n' "$PROJECT_DIR"
  printf '  内存：%s\n' "$(memory_summary)"

  if systemd_available; then
    printf '  [通过] systemd 可用\n'
  else
    local reason
    reason="$(systemd_diagnose)"
    printf '  [提示] systemd 不可用：%s\n' "$reason"
    printf '         可使用 shell 看门狗托管；开机自启用 crontab @reboot（菜单第 7 项）\n'
  fi

  mem_avail="$(mem_available_mb)"; mem_total="$(mem_total_mb)"; mem_swap="$(mem_swap_mb)"
  case "$mem_avail" in
    '' | *[!0-9]*) ;;
    *)
      if [ "$mem_avail" -lt 200 ]; then
        printf '  [注意] 可用内存不足 200M：应用可能起不来或频繁被 OOM 杀掉\n'
      fi
      ;;
  esac
  case "$mem_total:$mem_swap" in
    *[!0-9:]* | :* | *:) ;;
    *)
      if [ "$mem_swap" = "0" ] && [ "$mem_total" -lt 800 ]; then
        printf '  [注意] 内存只有 %sM 且没有 Swap：建议临时挂载 1G Swap\n' "$mem_total"
      fi
      ;;
  esac
  if [ -z "$BINARY_ARCH" ]; then
    printf '  [问题] 当前架构 %s 不受支持（仅提供 linux-amd64 / linux-arm64）\n' "$ARCH"
    problems=$((problems + 1))
  elif [ -x "$APP_BINARY" ]; then
    printf '  [通过] 应用二进制：%s\n' "$APP_BINARY"
  else
    printf '  [问题] 应用二进制缺失或不可执行：%s\n' "$APP_BINARY"
    printf '         请将 option-scope-linux-%s 与 run.sh 放在同一目录\n' "$BINARY_ARCH"
    problems=$((problems + 1))
  fi
  if [ -f "$ENV_FILE" ]; then
    port="$(app_port)"
    host="$(read_env_value HOST 0.0.0.0)"
    printf '  [通过] 配置文件：%s（HOST=%s PORT=%s）\n' "$ENV_FILE" "$host" "$port"
  else
    printf '  [注意] 缺少 %s，首次启动会自动生成\n' "$ENV_FILE"
    port="$(app_port)"
  fi
  access_key="$(read_env_value ACCESS_KEY "")"
  if [ -n "$access_key" ]; then
    printf '  [通过] 访问密钥：已配置（页面和 API 必须携带 ?key= 或 X-Access-Key）\n'
  else
    printf '  [问题] ACCESS_KEY 为空：访问保护未启用，可修改配置自动生成\n'
    problems=$((problems + 1))
  fi
  if app_running && health_ok; then
    printf '  [通过] 端口 %s：本项目服务正在监听且健康\n' "$port"
  elif port_in_use "$port"; then
    printf '  [问题] 端口 %s 被其他程序占用：\n' "$port"
    port_owner "$port" | sed 's/^/        /'
    problems=$((problems + 1))
  else
    printf '  [通过] 端口 %s 空闲\n' "$port"
  fi
  avail_kb="$(df -Pk "$PROJECT_DIR" 2>/dev/null | awk 'NR==2 {print $4}')"
  case "$avail_kb" in
    '' | *[!0-9]*)
      printf '  [注意] 无法获取磁盘剩余空间\n'
      ;;
    *)
      avail=$((avail_kb * 1024))
      printf '  磁盘剩余：%s\n' "$(human_size "$avail")"
      if [ "$avail" -lt 104857600 ]; then
        printf '  [注意] 剩余空间不足 100M，建议把 DATABASE_MAX_MB 设为 200-300\n'
      fi
      ;;
  esac
  db_path="$(resolve_db_path)"
  if [ -f "$db_path" ]; then
    db_size="$(database_size_bytes "$db_path")"
    limit="$(read_env_value DATABASE_MAX_MB 0)"
    printf '  数据库：%s / 上限 %s\n' "$(human_size "$db_size")" "$limit"
  fi
  # 日志概况
  if [ -d "$LOG_DIR" ]; then
    local total_log_bytes=0 f s
    for f in "$APP_LOG" "$WATCHDOG_LOG" "$CRON_LOG"; do
      [ -f "$f" ] || continue
      s="$(wc -c <"$f" 2>/dev/null | tr -d '[:space:]')"
      case "$s" in '' | *[!0-9]*) s=0 ;; esac
      total_log_bytes=$((total_log_bytes + s))
    done
    printf '  日志目录：%s（合计 %s；单文件上限 %s 行 / %s KB）\n' \
      "$LOG_DIR" "$(human_size "$total_log_bytes")" "$LOG_MAX_LINES" "$LOG_MAX_KB"
    if [ "$avail_kb" -ge 0 ] 2>/dev/null; then
      avail=$((avail_kb * 1024))
      if [ "$total_log_bytes" -gt $((avail / 10)) ]; then
        printf '  [注意] 日志占用超过磁盘剩余空间的 10%%，可执行 ./run.sh trim-logs\n'
      fi
    fi
  fi
  proxy="$(read_env_value MARKET_PROXY "")"
  if [ -n "$proxy" ]; then
    if proxy_reachable "$proxy"; then
      printf '  [通过] 代理可连接：%s\n' "$proxy"
    else
      printf '  [注意] 代理无法连接：%s（行情取数会失败，请确认代理已启动）\n' "$proxy"
    fi
  fi
  printf '\n'
  if [ "$problems" -eq 0 ]; then
    ok "自检完成：未发现阻塞性问题"
  else
    warn "自检完成：发现 $problems 个问题，按上面的提示处理后重试"
  fi
  return 0
}

proxy_reachable() {
  local proxy="$1" scheme authority host port rest
  scheme="${proxy%%://*}"
  authority="${proxy#*://}"
  authority="${authority%%/*}"
  authority="${authority##*@}"
  if [[ "$authority" == \[* ]]; then
    host="${authority#\[}"
    host="${host%%\]*}"
    rest="${authority#*\]}"
    case "$rest" in
      :*) port="${rest#:}" ;;
      *) port="" ;;
    esac
  else
    case "$authority" in
      *:*) host="${authority%:*}"; port="${authority##*:}" ;;
      *) host="$authority"; port="" ;;
    esac
  fi
  [ -n "$host" ] || host=127.0.0.1
  [ -n "$port" ] || { [ "$scheme" = https ] && port=443 || port=80; }
  case "$port" in '' | *[!0-9]*) return 1 ;; esac
  if has_cmd nc; then
    nc -z -w 3 "$host" "$port" >/dev/null 2>&1
  else
    (exec 3<>"/dev/tcp/$host/$port") >/dev/null 2>&1
  fi
}

# ---------- 菜单与入口 ----------
usage() {
  cat <<'TXT'
Option Scope 运维脚本用法：
  ./run.sh                      打开交互菜单
  ./run.sh start                启动应用（无 systemd 时自动带 shell 看门狗）
  ./run.sh stop                 停止应用
  ./run.sh restart              重启应用
  ./run.sh status               查看运行状态与最近日志
  ./run.sh logs [行数]          查看应用日志（默认末尾 80 行）
  ./run.sh config               修改 .env 配置（保存后可自动重启）
  ./run.sh trim-logs            立即把日志裁剪到上限（默认 200 行）
  ./run.sh memory [数量]        查看内存使用情况与占用最高的进程（默认前 10）
  ./run.sh doctor               环境自检
  ./run.sh kill-port [端口]     交互确认后结束监听该 TCP 端口的进程
  ./run.sh install-service      安装并启用 systemd 常驻服务（需要 systemd）
  ./run.sh remove-service       移除 systemd 服务
  ./run.sh enable-autostart     配置开机自启（crontab @reboot，适配容器）
  ./run.sh disable-autostart    取消开机自启
  ./run.sh foreground           前台运行（供 systemd 使用）
  ./run.sh help                 显示本帮助
说明：脚本幂等，路径全部基于脚本所在目录。
      应用二进制需与 run.sh 放在同一目录，按架构自动选择：
        option-scope-linux-amd64  （x86_64）
        option-scope-linux-arm64  （aarch64 / arm64）
      容器（LXC / Docker）无 systemd 时，托管方式自动降级为 shell 看门狗；
      开机自启可选用 crontab @reboot：./run.sh enable-autostart
      日志裁剪：每轮看门狗自动裁剪到 LOG_MAX_LINES 行（默认 200），
      原地覆盖不换 inode。可通过环境变量调整：
        LOG_MAX_LINES=100 ./run.sh start
TXT
}

MAIN_MENU_ROWS=(
  '|日常操作|group'
  '1|查看服务状态和日志|action_status'
  '2|启动应用|start_app'
  '3|停止应用|stop_app'
  '4|重启应用|restart_app'
  '5|修改配置（保存后可自动重启）|action_config'
  '6|查看应用日志|action_logs'
  '|安装与维护|group'
  '7|服务管理（systemd / 开机自启 / 卸载）|action_service'
  '8|结束占用指定 TCP 端口的程序|action_kill_port'
  '9|环境自检|action_doctor'
  '10|立即裁剪日志|action_trim_logs'
  '11|查看内存使用与占用排行|action_memory'
  '0|退出|quit'
)

menu_loop() {
  run_menu "运维菜单" "请选择操作（输入编号）：" main "${MAIN_MENU_ROWS[@]}"
  info "已退出。"
}

main() {
  local cmd="${1:-}"
  init_privilege
  detect_system
  setup_binary_path
  if [ "$cmd" = "__watchdog" ]; then
    watchdog_loop
    return 0
  fi
  case "$cmd" in
    "") menu_loop ;;
    start) start_app ;;
    stop) stop_app ;;
    restart) restart_app ;;
    status) action_status ;;
    logs | log) action_logs "${2:-80}" ;;
    config) action_config ;;
    trim-logs | trim) action_trim_logs ;;
    memory | mem) action_memory "${2:-10}" ;;
    doctor | check) action_doctor ;;
    install | install-runtime) ensure_runtime && ok "运行环境已就绪，可在主菜单选择 2) 启动应用" ;;
    kill-port) action_kill_port "${2:-}" ;;
    install-service | service-install) install_systemd_service ;;
    remove-service | service-remove) remove_systemd_service ;;
    enable-autostart | autostart) setup_autostart ;;
    disable-autostart | no-autostart) remove_autostart ;;
    foreground | serve) foreground_app ;;
    -h | --help | help) usage ;;
    *)
      fail "未知参数：$cmd"
      usage
      exit 2
      ;;
  esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
