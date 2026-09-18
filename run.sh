#!/usr/bin/env bash
# Option Scope 部署与运维脚本（菜单式）
#
# 适用系统：Ubuntu / Debian（apt）、CentOS / RHEL / Rocky / Alma（dnf、yum）、Alpine（apk）
# 适用架构：x86_64、aarch64、armv7l 等，取决于发行版是否提供对应的 Python 与预编译包
# 运行方式：源码运行（不打包二进制）——建虚拟环境 → 装依赖 → 准备 .env → 后台启动
#
# 用法：
#   ./run.sh                打开交互菜单
#   ./run.sh 2              直接执行第 2 项（也可用在开机脚本里）
#   ./run.sh status|stop    直接查看状态 / 停止应用
#   ./run.sh __watchdog     内部使用：看门狗主循环
#
# 从零安装（无需事先下载源码，会自动克隆/更新到最新版本）：
#   bash <(curl -Ls https://raw.githubusercontent.com/jack2652/31tvpmhdhlngphy59jx1/main/run.sh)
#   bash <(curl -Ls https://raw.githubusercontent.com/jack2652/31tvpmhdhlngphy59jx1/main/run.sh) 2   直接安装并启动
#   INSTALL_DIR=/opt/us_stocks bash <(curl -Ls .../run.sh)    自定义安装目录（默认 ./us_stocks）
#
# 说明：脚本自身幂等，重复执行安全；所有路径都基于脚本所在目录，可在任意工作目录调用。
set -u
set -o pipefail

# ---------- 基础路径与常量 ----------
SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENV_DIR="$PROJECT_DIR/.venv"
VENV_PY="$VENV_DIR/bin/python"
VENV_PIP="$VENV_DIR/bin/pip"
ENV_FILE="$PROJECT_DIR/.env"
ENV_EXAMPLE="$PROJECT_DIR/.env.example"
PYPROJECT="$PROJECT_DIR/pyproject.toml"
RUN_DIR="$PROJECT_DIR/.run"
LOG_DIR="$PROJECT_DIR/logs"
APP_LOG="$LOG_DIR/app.log"
WATCHDOG_LOG="$LOG_DIR/watchdog.log"
APP_PID_FILE="$RUN_DIR/app.pid"
WATCHDOG_PID_FILE="$RUN_DIR/watchdog.pid"
DEPS_STAMP="$RUN_DIR/deps.stamp"

# Python 版本下限，与 pyproject.toml 的 requires-python 保持一致
PYTHON_MIN_MAJOR=3
PYTHON_MIN_MINOR=11
WATCHDOG_INTERVAL=60          # 看门狗检查间隔（秒），可理解成「每分钟检查一次」
WATCHDOG_FAIL_LIMIT=3         # 连续多少次健康检查失败才重启，避免偶发抖动引发重启风暴
START_TIMEOUT=60              # 启动后等待健康检查的最长秒数
STOP_TIMEOUT=10               # 优雅退出等待秒数，超时强制结束
LOG_MAX_KB=$((5 * 1024))      # 单个日志上限，超过就轮转，避免 512M 容器被日志写满

# ---------- 输出样式 ----------
if [ -t 1 ]; then
  C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'; C_RED=$'\033[31m'
  C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_CYAN=$'\033[36m'
else
  C_RESET=""; C_BOLD=""; C_RED=""; C_GREEN=""; C_YELLOW=""; C_CYAN=""
fi

info()    { printf '%s[信息]%s %s\n' "$C_CYAN" "$C_RESET" "$*"; }
ok()      { printf '%s[完成]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn()    { printf '%s[注意]%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
fail()    { printf '%s[错误]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
section() { printf '\n%s== %s ==%s\n' "$C_BOLD" "$*" "$C_RESET"; }
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

# 以 root 身份执行命令：root 直跑，普通用户自动加 sudo，都没有时给出可复制的命令
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
  if has_cmd apt-get; then
    PKG_MANAGER="apt"
  elif has_cmd dnf; then
    PKG_MANAGER="dnf"
  elif has_cmd yum; then
    PKG_MANAGER="yum"
  elif has_cmd apk; then
    PKG_MANAGER="apk"
  elif has_cmd zypper; then
    PKG_MANAGER="zypper"
  else
    PKG_MANAGER=""
  fi
}

# 安装系统软件包，自动选择 apt / dnf / yum / apk / zypper
pkg_install() {
  [ "$#" -gt 0 ] || return 0
  local status=0
  case "$PKG_MANAGER" in
    apt)
      run_root apt-get update -qq && run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y "$@"
      ;;
    dnf) run_root dnf install -y "$@" ;;
    yum) run_root yum install -y "$@" ;;
    apk) run_root apk add --no-cache "$@" ;;
    zypper) run_root zypper --non-interactive install "$@" ;;
    *)
      fail "未识别的包管理器，请手动安装：$*"
      return 1
      ;;
  esac
  status=$?
  # 被 OOM Killer 干掉时给出针对性提示，避免误判成「软件源挂了」
  if is_oom_status "$status"; then
    warn "包管理器进程被系统强制结束（Killed）——内存不足（OOM），不是软件源或网络问题"
    printf '        当前内存：%s\n' "$(memory_summary)"
    oom_advice
    if [ "$PKG_MANAGER" = "apk" ]; then
      warn "apk 被中断可能留下半装状态，内存恢复后先执行：apk fix"
    fi
  fi
  return "$status"
}

# ---------- Python 与依赖 ----------
python_version_of() {
  "$1" -c 'import sys; print("%d.%d.%d" % sys.version_info[:3])' 2>/dev/null
}

# 判断解释器版本是否满足项目要求（requires-python >= 3.11）
python_is_supported() {
  "$1" -c "import sys; sys.exit(0 if sys.version_info >= (${PYTHON_MIN_MAJOR}, ${PYTHON_MIN_MINOR}) else 1)" 2>/dev/null
}

# 按「高版本优先」寻找可用解释器，返回其绝对路径
find_supported_python() {
  local name path
  for name in python3.13 python3.12 python3.11 python3; do
    path="$(command -v "$name" 2>/dev/null)" || continue
    if python_is_supported "$path"; then
      printf '%s' "$path"
      return 0
    fi
  done
  return 1
}

print_python_help() {
  cat <<'TXT'
  手动安装示例（挑一条执行后重新运行本脚本）：
    Ubuntu 24.04+ / Debian 12+ ：sudo apt-get install -y python3 python3-venv python3-pip
    Ubuntu 22.04（自带 3.10）  ：sudo add-apt-repository ppa:deadsnakes/ppa && sudo apt-get install -y python3.11 python3.11-venv python3.11-dev
    CentOS / Rocky 9           ：sudo dnf install -y python3.11 python3.11-pip
    Alpine 3.19+               ：sudo apk add python3 py3-pip
TXT
}

ensure_python() {
  local found
  if found="$(find_supported_python)"; then
    ok "Python 已就绪：$found（$("$found" --version 2>&1)）"
    SYSTEM_PYTHON="$found"
    return 0
  fi
  warn "未找到 Python ${PYTHON_MIN_MAJOR}.${PYTHON_MIN_MINOR}+，尝试自动安装"
  case "$PKG_MANAGER" in
    apt) pkg_install python3 python3-venv python3-pip ;;
    apk) pkg_install python3 py3-pip ;;
    dnf | yum) pkg_install python3 python3-pip ;;
    *) warn "未识别的包管理器，跳过自动安装" ;;
  esac
  if found="$(find_supported_python)"; then
    ok "Python 安装完成：$found"
    SYSTEM_PYTHON="$found"
    return 0
  fi
  # 系统仓库版本仍不达标时，再尝试高版本包名（CentOS/Rocky 9、Debian 常见做法）
  warn "系统自带 Python 版本低于 ${PYTHON_MIN_MAJOR}.${PYTHON_MIN_MINOR}，尝试安装高版本解释器"
  case "$PKG_MANAGER" in
    apt) pkg_install python3.11 python3.11-venv python3.11-dev ;;
    dnf | yum) pkg_install python3.11 python3.11-pip ;;
  esac
  if found="$(find_supported_python)"; then
    ok "Python 安装完成：$found"
    SYSTEM_PYTHON="$found"
    return 0
  fi
  fail "无法自动安装 Python ${PYTHON_MIN_MAJOR}.${PYTHON_MIN_MINOR}+"
  print_python_help
  return 1
}

ensure_venv() {
  if [ -x "$VENV_PY" ] && "$VENV_PY" -c 'import sys' >/dev/null 2>&1; then
    ok "虚拟环境已存在：$VENV_DIR（跳过创建）"
    return 0
  fi
  local interpreter="${SYSTEM_PYTHON:-}"
  if [ -z "$interpreter" ]; then
    interpreter="$(find_supported_python)" || {
      fail "没有可用的 Python 解释器"
      return 1
    }
  fi
  info "创建虚拟环境：$VENV_DIR"
  # --clear 会清空目标目录后重建，避免残留上一次失败的半成品环境
  if ! "$interpreter" -m venv --clear "$VENV_DIR" >/dev/null 2>&1; then
    warn "创建虚拟环境失败，尝试补齐 venv/pip 系统包后重试"
    case "$PKG_MANAGER" in
      apt) pkg_install python3-venv python3-pip ;;
      apk) pkg_install py3-pip ;;
      dnf | yum) pkg_install python3-pip ;;
    esac
    "$interpreter" -m venv --clear "$VENV_DIR" >/dev/null 2>&1 || {
      fail "创建虚拟环境失败，请手动执行：$interpreter -m venv $VENV_DIR"
      return 1
    }
  fi
  if [ ! -x "$VENV_PIP" ]; then
    "$VENV_PY" -m ensurepip --upgrade >/dev/null 2>&1 || true
  fi
  [ -x "$VENV_PIP" ] || {
    fail "虚拟环境缺少 pip，请安装 pip 后重试"
    return 1
  }
  ok "虚拟环境创建完成"
}

# pip 安装失败时补齐编译工具链（Alpine 的 musl 环境最容易踩到）
install_build_deps() {
  info "安装编译依赖（numpy/pandas/lxml 等可能需要现场编译）"
  case "$PKG_MANAGER" in
    apt) pkg_install build-essential python3-dev ;;
    apk) pkg_install build-base python3-dev linux-headers ;;
    dnf | yum) pkg_install gcc gcc-c++ make python3-devel ;;
    *) return 1 ;;
  esac
}

# 运行依赖是否可导入：通过项目自己的行情入口验证，顺带确认上游 SDK 已装好。
# 用子 shell 切到项目目录，保证脚本在任意工作目录调用都能导入 app 包。
deps_importable() {
  (
    cd "$PROJECT_DIR" || exit 1
    "$VENV_PY" -c 'import fastapi, uvicorn, pandas, dotenv, app.providers.market as market; market.load_upstream_sdk()'
  ) >/dev/null 2>&1
}

ensure_deps() {
  [ -x "$VENV_PIP" ] || { fail "缺少虚拟环境，请先执行第 1 项"; return 1; }
  local target="$PROJECT_DIR" stamp pip_status=0
  # 依赖指纹：pyproject.toml 内容 + Python 版本，任一变化就重装
  stamp="$(cksum "$PYPROJECT" 2>/dev/null | awk '{print $1}')-$(python_version_of "$VENV_PY")"
  if [ -f "$DEPS_STAMP" ] && [ "$(cat "$DEPS_STAMP" 2>/dev/null)" = "$stamp" ] \
     && deps_importable; then
    ok "依赖已安装且与当前代码匹配（跳过安装）"
    return 0
  fi
  info "安装项目依赖：pip install -e ."
  # 应用在跑时会一起占内存，小内存机器上容易把 pip 挤到被 OOM 杀掉
  if app_running; then
    warn "检测到应用正在运行：安装依赖时内存占用会翻倍，若被 Killed 请先执行 ./run.sh 3 停掉应用"
  fi
  "$VENV_PIP" install -e "$target" --disable-pip-version-check || pip_status=$?
  if [ "$pip_status" -ne 0 ]; then
    # 内存不足导致的中断重试也没用，直接给出可操作的结论，不去装编译工具白费时间
    if is_oom_status "$pip_status"; then
      fail "依赖安装被系统强制结束（Killed）——内存不足（OOM），不是网络或代码问题"
      printf '        当前内存：%s\n' "$(memory_summary)"
      oom_advice
      return 1
    fi
    warn "依赖安装失败，补齐编译依赖后重试一次"
    install_build_deps || true
    pip_status=0
    "$VENV_PIP" install -e "$target" --disable-pip-version-check || pip_status=$?
    if [ "$pip_status" -ne 0 ]; then
      if is_oom_status "$pip_status"; then
        fail "重试仍被系统强制结束（Killed）——内存不足（OOM），不是网络或代码问题"
        printf '        当前内存：%s\n' "$(memory_summary)"
        oom_advice
        return 1
      fi
      fail "依赖安装失败。可尝试：设置 PIP_INDEX_URL 换国内镜像、检查网络/代理，或先安装编译工具后重试"
      return 1
    fi
  fi
  mkdir -p "$RUN_DIR"
  printf '%s' "$stamp" > "$DEPS_STAMP"
  ok "依赖安装完成"
}

ensure_env_file() {
  if [ -f "$ENV_FILE" ]; then
    ok "配置文件已存在：$ENV_FILE（跳过生成）"
    return 0
  fi
  [ -f "$ENV_EXAMPLE" ] || { fail "缺少模板文件 $ENV_EXAMPLE"; return 1; }
  cp "$ENV_EXAMPLE" "$ENV_FILE" && ok "已由 .env.example 生成 $ENV_FILE"
}

# 读取 .env 中的配置项（取最后一次出现的值），不存在时返回默认值
read_env_value() {
  local key="$1" default_value="$2" value=""
  if [ -f "$ENV_FILE" ]; then
    value="$(grep -E "^[[:space:]]*${key}=" "$ENV_FILE" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d '\r')"
  fi
  value="$(printf '%s' "$value" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  if [ -n "$value" ]; then printf '%s' "$value"; else printf '%s' "$default_value"; fi
}

# 写入配置项：已存在则就地替换（并清掉重复项），不存在则追加
write_env_value() {
  local key="$1" value="$2" tmp
  [ -f "$ENV_FILE" ] || ensure_env_file || return 1
  tmp="$(mktemp "${TMPDIR:-/tmp}/runsh.XXXXXX")" || return 1
  awk -v k="$key" -v v="$value" '
    BEGIN { written = 0 }
    $0 ~ "^[[:space:]]*" k "=" { if (!written) { print k "=" v; written = 1 } ; next }
    { print }
    END { if (!written) print k "=" v }
  ' "$ENV_FILE" > "$tmp" && mv "$tmp" "$ENV_FILE"
  rm -f "$tmp" 2>/dev/null
}

# 一次性把「Python / 虚拟环境 / 依赖 / .env」准备好，已就绪的部分自动跳过
ensure_runtime() {
  ensure_python || return 1
  ensure_venv || return 1
  ensure_deps || return 1
  ensure_env_file || return 1
  mkdir -p "$RUN_DIR" "$LOG_DIR"
  return 0
}

# ---------- 进程 / 端口 / 日志辅助 ----------
timestamp() { date '+%Y-%m-%d %H:%M:%S'; }

# 运行期使用的解释器：优先虚拟环境，缺失时回退系统 python3（只读检查场景）
runtime_python() {
  if [ -x "$VENV_PY" ]; then
    printf '%s' "$VENV_PY"
  elif has_cmd python3; then
    command -v python3
  else
    printf '%s' "$VENV_PY"
  fi
}

# 读取 .env 里的监听端口；缺失或非法时回落到 8000（与 app/__main__.py 默认值一致）
app_port() {
  local port
  port="$(read_env_value PORT 8000)"
  case "$port" in
    '' | *[!0-9]*) port=8000 ;;
  esac
  printf '%s' "$port"
}

# 判断本机端口是否已被监听（纯 socket 探测，busybox 没有 ss 也能用）
port_in_use() {
  "$(runtime_python)" - "$1" <<'PY' >/dev/null 2>&1
import socket
import sys

sock = socket.socket()
sock.settimeout(1)
sys.exit(0 if sock.connect_ex(("127.0.0.1", int(sys.argv[1]))) == 0 else 1)
PY
}

# 打印占用端口的进程，方便判断是不是上一次没停干净的实例
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

# HTTP 健康检查：比「进程还在」更能反映服务是否真的可用
health_ok() {
  "$(runtime_python)" - "$(app_port)" <<'PY' >/dev/null 2>&1
import sys
import urllib.error
import urllib.request

url = "http://127.0.0.1:%s/health" % sys.argv[1]
try:
    with urllib.request.urlopen(url, timeout=3) as response:
        sys.exit(0 if response.status == 200 else 1)
except (urllib.error.URLError, OSError, ValueError):
    sys.exit(1)
PY
}

# 通过 /proc 精确识别属于本项目的进程：命令行匹配 + 工作目录匹配，避免误伤同名进程
proc_matches() {
  local pid="$1" keyword="$2" cmd cwd
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
  [ -d /proc ] || return 0
  for pid in /proc/[0-9]*; do
    pid="${pid#/proc/}"
    [ "$pid" = "$$" ] && continue
    if proc_matches "$pid" "$keyword"; then
      printf '%s\n' "$pid"
    fi
  done
}

# 读取 PID 文件并校验它确实指向本项目的进程，失效时回退到 /proc 扫描
resolve_pid() {
  local pid_file="$1" keyword="$2" pid=""
  if [ -f "$pid_file" ]; then
    pid="$(tr -d '[:space:]' <"$pid_file" 2>/dev/null)"
  fi
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    # 没有 /proc（非 Linux）时只看进程是否存在
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

app_pid() { resolve_pid "$APP_PID_FILE" "-m app"; }
watchdog_pid() { resolve_pid "$WATCHDOG_PID_FILE" "__watchdog"; }
app_running() { app_pid >/dev/null 2>&1; }
watchdog_running() { watchdog_pid >/dev/null 2>&1; }

# 进程已运行时长，busybox 的 ps 不支持时返回「未知」
process_uptime() {
  local etime=""
  if has_cmd ps; then
    etime="$(ps -o etime= -p "$1" 2>/dev/null | tr -d '[:space:]')"
  fi
  [ -n "$etime" ] || etime="未知"
  printf '%s' "$etime"
}

# 本机内网 IP，用于打印访问地址；探测不到时回退 127.0.0.1
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

# 打印访问地址（监听 0.0.0.0 时同时给出内网地址）
show_access_url() {
  local port host
  port="$(app_port)"
  host="$(read_env_value HOST 0.0.0.0)"
  printf '  本机访问：http://127.0.0.1:%s\n' "$port"
  if [ "$host" != "127.0.0.1" ] && [ "$host" != "localhost" ]; then
    printf '  局域网访问：http://%s:%s\n' "$(local_ip)" "$port"
  fi
}

# 日志超过上限就轮转：先复制成 .1 再清空原文件，保证服务已打开的文件句柄继续可用
rotate_log_if_needed() {
  local log="$1" size=""
  [ -f "$log" ] || return 0
  size="$(wc -c <"$log" 2>/dev/null | tr -d '[:space:]')"
  case "$size" in
    '' | *[!0-9]*) return 0 ;;
  esac
  [ "$size" -gt $((LOG_MAX_KB * 1024)) ] || return 0
  cp "$log" "$log.1" 2>/dev/null || true
  : >"$log"
}

# 打印日志末尾若干行，缺文件时给一句提示
tail_log() {
  local log="$1" lines="${2:-30}"
  if [ -s "$log" ]; then
    printf '%s最近 %s 行（%s）%s\n' "$C_BOLD" "$lines" "$log" "$C_RESET"
    tail -n "$lines" "$log"
  else
    info "日志为空：$log"
  fi
}

# 把字节数转成便于阅读的单位
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

# ---------- 内存探测：小内存机器上「安装依赖被 Killed」几乎都是内存不足（OOM） ----------
# 退出码 137 = 128 + 9，即进程收到 SIGKILL；在容器里通常由内存超限触发
is_oom_status() {
  [ "${1:-0}" = "137" ]
}

# 可用内存（MB），优先 MemAvailable，取不到时退回 MemFree；无法读取时输出空
mem_available_mb() {
  local kb
  kb="$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo 2>/dev/null)"
  [ -n "$kb" ] || kb="$(awk '/^MemFree:/ {print $2}' /proc/meminfo 2>/dev/null)"
  case "$kb" in
    '' | *[!0-9]*) printf '' ;;
    *) printf '%d' $((kb / 1024)) ;;
  esac
}

# 物理内存总量（MB）
mem_total_mb() {
  local kb
  kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null)"
  case "$kb" in
    '' | *[!0-9]*) printf '' ;;
    *) printf '%d' $((kb / 1024)) ;;
  esac
}

# Swap 总量（MB），0 表示没有交换空间
mem_swap_mb() {
  local kb
  kb="$(awk '/^SwapTotal:/ {print $2}' /proc/meminfo 2>/dev/null)"
  case "$kb" in
    '' | *[!0-9]*) printf '' ;;
    *) printf '%d' $((kb / 1024)) ;;
  esac
}

# cgroup（LXC / Docker）内存上限（MB）；未限制时输出空
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
      # 极大的数字表示「不限制」，避免换算成天文数字
      if [ "$value" -ge 1152921504606846976 ] 2>/dev/null; then
        printf ''
      else
        printf '%d' $((value / 1048576))
      fi
      ;;
  esac
}

# 一行内存概况：可用 / 总量 / Swap / 容器上限
memory_summary() {
  local avail total swap limit text
  avail="$(mem_available_mb)"; total="$(mem_total_mb)"
  swap="$(mem_swap_mb)"; limit="$(cgroup_mem_limit_mb)"
  text="可用 ${avail:-未知}M / 总 ${total:-未知}M，Swap ${swap:-未知}M"
  [ -n "$limit" ] && text="$text，容器内存上限 ${limit}M"
  printf '%s' "$text"
}

# 内存不足（OOM）时的处理建议，供安装依赖、装系统包两处复用
oom_advice() {
  printf '        处理办法（按推荐顺序任选一条）：\n'
  printf '          1) 先停掉正在运行的应用再重试安装（安装时内存占用会翻倍）：./run.sh 3\n'
  printf '          2) 临时加 1G Swap（装完依赖即可保留或删除）：\n'
  printf '             fallocate -l 1G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile\n'
  printf '             没有 fallocate 时改用：dd if=/dev/zero of=/swapfile bs=1M count=1024\n'
  printf '             （LXC 容器可能禁止 swapon，此时改用第 3 条）\n'
  printf '          3) 调大容器/主机内存上限到 1G 以上，或换一台内存充足的机器安装\n'
  printf '          4) 内存无法增加时改成分步安装，降低单次峰值：\n'
  printf '             .venv/bin/pip install --no-cache-dir pandas\n'
  printf '             .venv/bin/pip install --no-cache-dir -e .\n'
  printf '        确认是否 OOM：dmesg | tail -20，或 cat /sys/fs/cgroup/memory.events\n'
}

# SQLite 文件（含 WAL/SHM）实际占用字节数
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

# .env 中 DATABASE_PATH 解析成绝对路径（相对路径以项目根目录为基准）
resolve_db_path() {
  local db_path
  db_path="$(read_env_value DATABASE_PATH data/options.db)"
  case "$db_path" in
    /*) printf '%s' "$db_path" ;;
    *) printf '%s/%s' "$PROJECT_DIR" "$db_path" ;;
  esac
}

# ---------- 应用启停与看门狗 ----------
# 看门狗自己的日志行，带时间戳和固定前缀，便于 tail 时区分
wd_log() { printf '[%s] [看门狗] %s\n' "$(timestamp)" "$*"; }

# 以脱离当前会话的方式启动后台命令（不依赖 systemd / cron，LXC、Alpine 都能用）
spawn_detached() {
  local log="$1"
  shift
  if has_cmd setsid; then
    setsid nohup "$@" >>"$log" 2>&1 </dev/null &
  else
    nohup "$@" >>"$log" 2>&1 </dev/null &
  fi
  return 0
}

# 启动应用必须用虚拟环境里的解释器，避免污染系统 Python
app_python() {
  if [ -x "$VENV_PY" ]; then
    printf '%s' "$VENV_PY"
    return 0
  fi
  fail "虚拟环境不可用：$VENV_PY（请先执行菜单第 1 项完成环境安装）"
  return 1
}

# 启动应用进程本体（不含看门狗）：已在运行直接返回，端口被占用则打印占用者
start_app_internal() {
  local port existing py waited=0 dead=0 pid=""
  port="$(app_port)"
  existing="$(app_pid 2>/dev/null || true)"
  if [ -n "$existing" ]; then
    info "应用已在运行（PID $existing），跳过启动"
    return 0
  fi
  py="$(app_python)" || return 1
  if port_in_use "$port"; then
    fail "端口 $port 已被占用，无法启动应用"
    port_owner "$port" | sed 's/^/    /'
    fail "若是上一次没停干净的进程：先执行 ./run.sh stop；也可以改 .env 里的 PORT 换端口"
    return 1
  fi
  mkdir -p "$RUN_DIR" "$LOG_DIR"
  printf '[%s] 启动应用：%s -m app（端口 %s）\n' "$(timestamp)" "$py" "$port" >>"$APP_LOG"
  # 工作目录必须是项目根目录：python -m app 与进程识别（/proc/<pid>/cwd）都依赖它
  ( cd "$PROJECT_DIR" && spawn_detached "$APP_LOG" "$py" -m app )
  while [ "$waited" -lt "$START_TIMEOUT" ]; do
    sleep 1
    waited=$((waited + 1))
    if health_ok; then
      pid="$(app_pid 2>/dev/null || true)"
      ok "应用已启动（PID ${pid:-未知}，端口 $port），健康检查通过，耗时 ${waited}s"
      return 0
    fi
    if [ -z "$(app_pid 2>/dev/null || true)" ]; then
      dead=$((dead + 1))
      # 连续 5 秒都看不到进程，说明是启动即崩，不必等满超时
      if [ "$dead" -ge 5 ]; then
        break
      fi
    else
      dead=0
    fi
  done
  fail "启动失败：${START_TIMEOUT}s 内未通过健康检查"
  tail_log "$APP_LOG" 20
  return 1
}

# 启动看门狗（每分钟检查一次应用是否还在，异常自动拉起）
start_watchdog() {
  local pid waited=0
  pid="$(watchdog_pid 2>/dev/null || true)"
  if [ -n "$pid" ]; then
    info "看门狗已在运行（PID $pid），跳过启动"
    return 0
  fi
  mkdir -p "$RUN_DIR" "$LOG_DIR"
  printf '[%s] 启动看门狗（检查间隔 %s 秒）\n' "$(timestamp)" "$WATCHDOG_INTERVAL" >>"$WATCHDOG_LOG"
  ( cd "$PROJECT_DIR" && spawn_detached "$WATCHDOG_LOG" bash "$SCRIPT_PATH" __watchdog )
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

stop_watchdog() {
  local pid waited=0
  pid="$(watchdog_pid 2>/dev/null || true)"
  if [ -z "$pid" ]; then
    info "看门狗未在运行"
    return 0
  fi
  info "停止看门狗（PID $pid）"
  kill "$pid" 2>/dev/null || true
  while [ "$waited" -lt "$STOP_TIMEOUT" ]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
    waited=$((waited + 1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    warn "看门狗未在 ${STOP_TIMEOUT}s 内退出，强制结束"
    kill -9 "$pid" 2>/dev/null || true
    sleep 1
  fi
  if [ -f "$WATCHDOG_PID_FILE" ]; then
    rm -f "$WATCHDOG_PID_FILE" 2>/dev/null
  fi
  ok "看门狗已停止"
}

stop_app_process() {
  local pid waited=0 extra=""
  pid="$(app_pid 2>/dev/null || true)"
  if [ -z "$pid" ]; then
    info "应用未在运行"
    return 0
  fi
  info "停止应用（PID $pid）"
  kill "$pid" 2>/dev/null || true
  while [ "$waited" -lt "$STOP_TIMEOUT" ]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
    waited=$((waited + 1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    warn "应用未在 ${STOP_TIMEOUT}s 内退出，强制结束"
    kill -9 "$pid" 2>/dev/null || true
    sleep 1
  fi
  # 端口仍被占用说明还有残留（例如手动起过的实例），再做一次精确清理
  if port_in_use "$(app_port)"; then
    extra="$(scan_pids "-m app" | head -n 1)"
    if [ -n "$extra" ]; then
      warn "发现残留进程 PID $extra，一并结束"
      kill "$extra" 2>/dev/null || true
      sleep 1
      kill -9 "$extra" 2>/dev/null || true
    fi
  fi
  if [ -f "$APP_PID_FILE" ]; then
    rm -f "$APP_PID_FILE" 2>/dev/null
  fi
  ok "应用已停止"
}

start_app() {
  section "启动应用"
  ensure_runtime || return 1
  start_app_internal || return 1
  start_watchdog || warn "应用已启动，但看门狗没起来（不影响使用，可稍后重试）"
  show_access_url
  return 0
}

stop_app() {
  section "停止应用"
  # 先停看门狗，否则它会把刚停掉的应用又拉起来
  stop_watchdog
  stop_app_process
  sleep 1
  if app_running; then
    warn "看门狗在退出前又拉起过一次应用，再次停止"
    stop_app_process
  fi
  return 0
}

restart_app() {
  stop_app
  start_app
}

# 看门狗主循环：日志轮转 → 进程存活 → 健康检查 → 必要时拉起（每分钟一轮）
watchdog_loop() {
  local fails=0 pid="" other=""
  mkdir -p "$RUN_DIR" "$LOG_DIR"
  # 防止重复运行：已经有别的看门狗活着时直接退出，避免两个看门狗互相抢着重启
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
    rotate_log_if_needed "$WATCHDOG_LOG"
    if [ -x "$VENV_PY" ]; then
      rotate_log_if_needed "$APP_LOG"
      pid="$(app_pid 2>/dev/null || true)"
      if [ -z "$pid" ]; then
        wd_log "应用未在运行，尝试拉起"
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
    else
      wd_log "虚拟环境不可用（$VENV_PY），等待下一次检查"
    fi
    sleep "$WATCHDOG_INTERVAL"
  done
}

# ---------- 状态展示与各菜单动作 ----------
show_status() {
  local port pid db_path size limit
  section "运行状态"
  printf '  系统：%s（架构 %s，包管理器 %s）\n' "$OS_NAME" "$ARCH" "${PKG_MANAGER:-未知}"
  printf '  项目目录：%s\n' "$PROJECT_DIR"
  if [ -x "$VENV_PY" ]; then
    printf '  虚拟环境：%s（Python %s）\n' "$VENV_DIR" "$(python_version_of "$VENV_PY")"
  else
    printf '  虚拟环境：%s（未安装）\n' "$VENV_DIR"
  fi
  port="$(app_port)"
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
    printf '  看门狗：运行中（PID %s，每 %s 秒检查一次）\n' "$pid" "$WATCHDOG_INTERVAL"
  else
    printf '  看门狗：未运行（应用异常退出后不会自动拉起）\n'
  fi
  show_access_url
  db_path="$(resolve_db_path)"
  limit="$(read_env_value DATABASE_MAX_MB 0)"
  if [ -f "$db_path" ]; then
    size="$(database_size_bytes "$db_path")"
    printf '  数据库：%s（%s，上限 %sMB）\n' "$db_path" "$(human_size "$size")" "$limit"
  else
    printf '  数据库：%s（尚未创建）\n' "$db_path"
  fi
  printf '  日志：%s\n' "$APP_LOG"
  printf '        %s\n' "$WATCHDOG_LOG"
}

action_install() {
  section "检测环境并安装"
  ensure_runtime || return 1
  return 0
}

action_status() {
  show_status
  printf '\n'
  tail_log "$APP_LOG" 15
  return 0
}

# 配置向导用的单键修改：直接回车表示保持原值
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

action_config() {
  local port limit answer=""
  section "修改配置（.env）"
  ensure_env_file || return 1
  printf '  配置文件：%s\n' "$ENV_FILE"
  ask_env_value HOST "监听地址（0.0.0.0 表示允许局域网访问）"
  ask_env_value PORT "监听端口（1024-65535）"
  ask_env_value DEFAULT_SYMBOLS "默认标的（多个用英文逗号分隔）"
  ask_env_value REFRESH_INTERVAL_SECONDS "后台刷新间隔（秒）"
  ask_env_value RAW_RETENTION_DAYS "历史数据保留天数"
  ask_env_value DATABASE_MAX_MB "SQLite 体积上限（512 / 512M / 1G，0 表示不限制）"
  ask_env_value MARKET_PROXY "上游行情接口的代理地址（留空表示不使用代理）"
  ask_env_value SCHEDULER_ENABLED "是否启用后台刷新（true/false）"
  printf '\n'
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
  ok "配置已保存到 $ENV_FILE"
  if app_running; then
    printf '  配置需要重启后才生效，现在重启？[y/N]：'
    read -r answer || answer=""
    case "$answer" in
      y | Y | yes | YES) restart_app ;;
      *) info "稍后可执行 ./run.sh 5 选择重启使其生效" ;;
    esac
  fi
  return 0
}

update_deps_and_restart() {
  section "更新依赖并重启"
  # 删掉依赖指纹，强制 ensure_deps 重新安装一次
  if [ -f "$DEPS_STAMP" ]; then
    rm -f "$DEPS_STAMP" 2>/dev/null
  fi
  ensure_runtime || return 1
  restart_app
}

uninstall_runtime() {
  local answer="" target
  section "卸载运行环境"
  printf '  将停止服务并删除：%s、%s、%s\n' "$VENV_DIR" "$RUN_DIR" "$LOG_DIR"
  printf '  会保留：.env 配置、data 数据库、项目源码\n'
  printf '  确认继续？输入 yes 继续：'
  read -r answer || answer=""
  if [ "$answer" != "yes" ]; then
    info "已取消"
    return 0
  fi
  stop_app
  # 只删除明确列出的目录，并校验它们都在项目目录内，避免误删其他文件
  for target in "$VENV_DIR" "$RUN_DIR" "$LOG_DIR"; do
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

action_service() {
  local choice=""
  section "服务管理"
  printf '  1) 重启应用\n'
  printf '  2) 更新依赖并重启（改过 pyproject.toml 时用）\n'
  printf '  3) 卸载运行环境（停止服务并删除 .venv/.run/logs）\n'
  printf '  0) 返回\n'
  printf '请选择：'
  read -r choice || return 0
  case "$choice" in
    1) restart_app ;;
    2) update_deps_and_restart ;;
    3) uninstall_runtime ;;
    0 | "") return 0 ;;
    *) warn "无效选择：$choice" ;;
  esac
  return 0
}

# 环境自检：把「装不上 / 起不来 / 取不到数」的常见原因一次性列出来
action_doctor() {
  local problems=0 port="" host="" avail_kb="" avail="" db_path="" db_size="" limit="" proxy=""
  local mem_avail="" mem_total="" mem_swap=""
  section "环境自检"
  printf '  系统：%s（架构 %s，包管理器 %s）\n' "$OS_NAME" "$ARCH" "${PKG_MANAGER:-未知}"
  # 内存与 Swap：小内存 LXC 上「装依赖被 Killed」的根因就在这里
  printf '  内存：%s\n' "$(memory_summary)"
  mem_avail="$(mem_available_mb)"; mem_total="$(mem_total_mb)"; mem_swap="$(mem_swap_mb)"
  case "$mem_avail" in
    '' | *[!0-9]*) ;;
    *)
      if [ "$mem_avail" -lt 300 ]; then
        printf '  [注意] 可用内存不足 300M：安装依赖时容易被 OOM 杀掉，先 ./run.sh 3 停应用，或临时加 1G Swap\n'
      fi
      ;;
  esac
  case "$mem_total:$mem_swap" in
    *[!0-9:]* | :* | *:) ;;
    *)
      if [ "$mem_swap" = "0" ] && [ "$mem_total" -lt 800 ]; then
        printf '  [注意] 内存只有 %sM 且没有 Swap：装系统包与 pip 依赖都可能被杀，建议临时挂载 1G Swap\n' "$mem_total"
      fi
      ;;
  esac
  if has_cmd python3; then
    printf '  [通过] 系统 Python：%s（%s）\n' "$(command -v python3)" "$(python_version_of "$(command -v python3)")"
  else
    printf '  [问题] 未找到 python3，请先执行菜单第 1 项\n'
    problems=$((problems + 1))
  fi
  if [ -x "$VENV_PY" ]; then
    printf '  [通过] 虚拟环境：%s（Python %s）\n' "$VENV_DIR" "$(python_version_of "$VENV_PY")"
    if deps_importable; then
      printf '  [通过] 运行依赖导入正常\n'
    else
      printf '  [问题] 依赖不完整或损坏，请执行菜单第 1 项重装\n'
      problems=$((problems + 1))
    fi
    if "$VENV_PY" -m pip check >/dev/null 2>&1; then
      printf '  [通过] pip check 未发现依赖冲突\n'
    else
      printf '  [注意] pip check 报告依赖冲突：\n'
      "$VENV_PY" -m pip check 2>&1 | sed 's/^/        /'
    fi
  else
    printf '  [问题] 虚拟环境缺失：%s\n' "$VENV_DIR"
    problems=$((problems + 1))
  fi
  if [ -f "$ENV_FILE" ]; then
    port="$(app_port)"
    host="$(read_env_value HOST 0.0.0.0)"
    printf '  [通过] 配置文件：%s（HOST=%s PORT=%s）\n' "$ENV_FILE" "$host" "$port"
  else
    printf '  [注意] 缺少 %s，首次启动会由 .env.example 自动生成\n' "$ENV_FILE"
    port="$(app_port)"
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
    printf '  数据库：%s / 上限 %sMB\n' "$(human_size "$db_size")" "$limit"
  fi
  proxy="$(read_env_value MARKET_PROXY "")"
  if [ -n "$proxy" ]; then
    if "$(runtime_python)" - "$proxy" <<'PY' >/dev/null 2>&1
import socket
import sys
from urllib.parse import urlparse

parts = urlparse(sys.argv[1])
host = parts.hostname or "127.0.0.1"
port = parts.port or (443 if parts.scheme == "https" else 80)
sock = socket.socket()
sock.settimeout(3)
sys.exit(0 if sock.connect_ex((host, port)) == 0 else 1)
PY
    then
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

# ---------- 菜单与入口 ----------
usage() {
  cat <<'TXT'
Option Scope 运维脚本用法：
  ./run.sh                    打开交互菜单
  ./run.sh 2                  直接执行第 2 项（适合脚本、计划任务调用）
  ./run.sh start|stop|restart|status|doctor|install|config|db
  ./run.sh help               显示本帮助
从零安装（当前目录下没有源码时先自动拉取，再继续执行）：
  bash <(curl -Ls https://raw.githubusercontent.com/jack2652/31tvpmhdhlngphy59jx1/main/run.sh)
说明：脚本幂等，已就绪的步骤会自动跳过；路径全部基于脚本所在目录。
TXT
}

show_menu() {
  printf '\n%s================= Option Scope 运维菜单 =================%s\n' "$C_BOLD" "$C_RESET"
  printf '  1) 检测环境并安装依赖（已安装的步骤自动跳过）\n'
  printf '  2) 启动应用（后台运行 + 看门狗守护）\n'
  printf '  3) 停止应用（含看门狗）\n'
  printf '  4) 查看状态与日志\n'
  printf '  5) 服务管理（重启 / 更新依赖 / 卸载运行环境）\n'
  printf '  6) 修改配置（.env 交互式编辑）\n'
  printf '  7) 数据库工具（清理 / 备份 / 统计）\n'
  printf '  8) 环境自检（doctor）\n'
  printf '  0) 退出\n'
  printf '%s=========================================================%s\n' "$C_BOLD" "$C_RESET"
}

menu_loop() {
  local choice=""
  while true; do
    show_menu
    printf '请选择操作 [0-8]：'
    if ! read -r choice; then
      printf '\n'
      break
    fi
    case "$choice" in
      1) action_install ;;
      2) action_start ;;
      3) action_stop ;;
      4) action_status ;;
      5) action_service ;;
      6) action_config ;;
      7) action_database ;;
      8) action_doctor ;;
      0 | q | quit | exit) break ;;
      "") ;;
      *) warn "无效选择：$choice" ;;
    esac
  done
  info "已退出。"
}

action_start() {
  start_app
}

action_stop() {
  stop_app
}

# ---------- 引导安装：curl | bash 场景 ----------
# 通过 `bash <(curl -Ls <脚本地址>)` 或 `curl -Ls <脚本地址> | bash` 运行时，脚本自身并不在项目目录里
# （BASH_SOURCE 指向 /dev/fd/*），PROJECT_DIR 会解析成 /dev，所有路径都会失效。
# 因此这里先把仓库下载到本地，再切换到真实目录把后续流程交给同一份脚本继续执行。
GIT_REMOTE_URL="${GIT_REMOTE_URL:-https://github.com/jack2652/31tvpmhdhlngphy59jx1.git}"
ARCHIVE_URL="${ARCHIVE_URL:-https://codeload.github.com/jack2652/31tvpmhdhlngphy59jx1/tar.gz/refs/heads/main}"

# 判断当前脚本是否就运行在完整源码目录里
in_project_checkout() {
  # shellcheck disable=SC2153
  [ -f "$PROJECT_DIR/pyproject.toml" ] && [ -d "$PROJECT_DIR/app" ]
}

# 自动更新源码需要 git；缺失时尝试用系统包管理器装上，装不上则退化为压缩包下载
ensure_git() {
  if has_cmd git; then
    ok "git 已就绪：$(git --version 2>/dev/null)"
    return 0
  fi
  warn "未找到 git，尝试自动安装"
  pkg_install git || true
  if has_cmd git; then
    ok "git 安装完成"
    return 0
  fi
  warn "自动安装 git 失败，改用源码压缩包方式（后续无法自动增量更新）"
  return 1
}

# 没有 git 时的兜底：直接下载分支源码包并解压
download_archive() {
  local target="$1" tmp="" extracted=""
  has_cmd curl || { fail "缺少 curl，无法下载源码包"; return 1; }
  has_cmd tar || { fail "缺少 tar，无法解压源码包"; return 1; }
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/us_stocks.XXXXXX")" || return 1
  info "下载源码压缩包：$ARCHIVE_URL"
  if ! curl -LfsS "$ARCHIVE_URL" | tar -xz -C "$tmp"; then
    fail "源码包下载或解压失败，请检查网络或代理"
    rm -rf "$tmp"
    return 1
  fi
  extracted="$(find "$tmp" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  if [ -z "$extracted" ]; then
    fail "源码包内容异常（未找到解压目录）"
    rm -rf "$tmp"
    return 1
  fi
  mkdir -p "$(dirname "$target")"
  if ! mv "$extracted" "$target"; then
    fail "移动到 $target 失败"
    rm -rf "$tmp"
    return 1
  fi
  rm -rf "$tmp"
  return 0
}

bootstrap_if_needed() {
  if in_project_checkout; then
    return 0
  fi
  # 引导只允许发生一次：目标目录内容异常时再次 exec 会无限套娃，这里直接报错退出
  if [ "${OPTION_SCOPE_BOOTSTRAP_DONE:-0}" = "1" ]; then
    fail "引导安装后仍未进入完整源码目录，请手动检查安装目录后重试"
    return 1
  fi
  section "首次安装：获取最新源码"
  local target="${INSTALL_DIR:-$PWD/us_stocks}"
  case "$target" in
    /*) ;;
    *) target="$PWD/$target" ;;
  esac
  info "脚本来自 ${BASH_SOURCE[0]}，安装目录：$target"
  if [ -d "$target/.git" ]; then
    info "检测到已存在的仓库，更新到最新版本"
    if has_cmd git && git -C "$target" pull --ff-only --quiet 2>/dev/null; then
      ok "源码已更新到最新版本"
    else
      warn "自动更新失败（缺少 git 或存在本地修改），继续使用现有源码：$target"
    fi
  elif [ -e "$target" ]; then
    fail "目标目录已存在且不是 git 仓库：$target"
    fail "请换一个安装目录，例如：INSTALL_DIR=/opt/us_stocks bash <(curl -Ls <脚本地址>)"
    return 1
  else
    if ensure_git; then
      info "克隆源码：$GIT_REMOTE_URL"
      if ! git clone --depth 1 --quiet "$GIT_REMOTE_URL" "$target"; then
        fail "克隆失败，请检查网络或代理设置"
        return 1
      fi
    else
      download_archive "$target" || return 1
    fi
    ok "源码已下载到 $target"
  fi
  # 内容校验：缺文件就报错，避免 exec 出去以后又回到引导逻辑里空转
  if [ ! -f "$target/pyproject.toml" ] || [ ! -d "$target/app" ] || [ ! -f "$target/run.sh" ]; then
    fail "源码不完整（缺少 run.sh / pyproject.toml / app）：$target"
    return 1
  fi
  if [ ! -x "$target/run.sh" ]; then
    chmod +x "$target/run.sh" 2>/dev/null || true
  fi
  cd "$target" || { fail "无法进入目录：$target"; return 1; }
  info "切换到项目目录，继续执行脚本"
  export OPTION_SCOPE_BOOTSTRAP_DONE=1
  # 用 `curl | bash` 时标准输入是管道，菜单读不到按键；能打开终端就重新接回 /dev/tty
  if [ ! -t 0 ] && (exec </dev/tty) 2>/dev/null; then
    exec bash "$target/run.sh" "$@" </dev/tty
  fi
  exec bash "$target/run.sh" "$@"
}

main() {
  local cmd="${1:-}"
  if [ "$cmd" = "__watchdog" ]; then
    watchdog_loop
    return 0
  fi
  init_privilege
  detect_system
  # 不在源码目录里（curl | bash 场景）时先拉取源码，再切换过去继续执行
  bootstrap_if_needed "$@" || return 1
  case "$cmd" in
    "") menu_loop ;;
    1 | install) action_install ;;
    2 | start) action_start ;;
    3 | stop) action_stop ;;
    4 | status) action_status ;;
    5 | service) action_service ;;
    6 | config) action_config ;;
    7 | db | database) action_database ;;
    8 | doctor | check) action_doctor ;;
    restart) restart_app ;;
    log | logs) tail_log "$APP_LOG" "${2:-50}" ;;
    -h | --help | help) usage ;;
    *)
      fail "未知参数：$cmd"
      usage
      exit 2
      ;;
  esac
}

# 立即清理历史数据：服务在跑就走 API，没跑就直接操作数据库文件
cleanup_database() {
  section "清理历史数据"
  if app_running; then
    info "服务正在运行，调用 POST /api/cleanup"
    "$(runtime_python)" - "$(app_port)" <<'PY'
import json
import sys
import urllib.request

request = urllib.request.Request("http://127.0.0.1:%s/api/cleanup" % sys.argv[1], method="POST")
try:
    with urllib.request.urlopen(request, timeout=120) as response:
        print(json.dumps(json.load(response), ensure_ascii=False, indent=2))
except Exception as exc:  # noqa: BLE001 - 命令行工具，直接把错误显示给用户
    sys.exit("调用失败：%s" % exc)
PY
  else
    info "服务未运行，直接操作数据库文件"
    ( cd "$PROJECT_DIR" && "$VENV_PY" - <<'PY'
from app.config import Settings
from app.db import Database

settings = Settings.from_env()
db = Database(settings.database_path)
print("删除过期记录：", db.cleanup(settings.raw_retention_days))
print("按体积清理：", db.cleanup_by_size(settings.database_max_mb * 1048576))
PY
    )
  fi
  return 0
}

# 在线备份：VACUUM INTO 由 SQLite 自己保证一致性，服务运行中也能安全执行
backup_database() {
  local db_path target
  section "备份数据库"
  db_path="$(resolve_db_path)"
  if [ ! -f "$db_path" ]; then
    fail "数据库文件不存在：$db_path"
    return 1
  fi
  mkdir -p "$PROJECT_DIR/backup"
  target="$PROJECT_DIR/backup/options-$(date +%Y%m%d-%H%M%S).db"
  "$(runtime_python)" - "$db_path" "$target" <<'PY'
import sqlite3
import sys

source, dest = sys.argv[1], sys.argv[2]
with sqlite3.connect(source) as connection:
    connection.execute("VACUUM INTO ?", (dest,))
print("备份文件：", dest)
PY
}

database_stats() {
  section "数据表统计"
  ( cd "$PROJECT_DIR" && "$VENV_PY" - <<'PY'
from app.config import Settings
from app.db import Database

settings = Settings.from_env()
db = Database(settings.database_path)
with db.connect() as connection:
    tables = connection.execute(
        "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name"
    ).fetchall()
    for (name,) in tables:
        count = connection.execute("SELECT COUNT(*) FROM %s" % name).fetchone()[0]
        print("  %-28s %d" % (name, count))
PY
  )
  return 0
}

action_database() {
  local choice="" db_path size
  section "数据库工具"
  db_path="$(resolve_db_path)"
  if [ -f "$db_path" ]; then
    size="$(database_size_bytes "$db_path")"
    printf '  路径：%s\n' "$db_path"
    printf '  体积：%s（上限 %sMB，0 表示不限制）\n' "$(human_size "$size")" "$(read_env_value DATABASE_MAX_MB 0)"
  else
    printf '  路径：%s（尚未创建）\n' "$db_path"
  fi
  printf '\n  1) 立即清理历史数据\n'
  printf '  2) 备份数据库（在线安全备份）\n'
  printf '  3) 查看各表记录数\n'
  printf '  0) 返回\n'
  printf '请选择：'
  read -r choice || return 0
  case "$choice" in
    1) cleanup_database ;;
    2) backup_database ;;
    3) database_stats ;;
    0 | "") return 0 ;;
    *) warn "无效选择：$choice" ;;
  esac
  return 0
}

# ---------- 入口 ----------
# 放在文件最末尾：此时所有函数都已定义，且脚本退出码等于对应动作的返回值，
# 便于计划任务、监控脚本据此判断成功（0）与失败（非 0）。
main "$@"
