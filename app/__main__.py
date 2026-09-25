"""提供默认监听 0.0.0.0 的本地启动入口。"""

from __future__ import annotations

import os
import socket
import sys

from dotenv import load_dotenv

from app.runtime import low_memory_enabled


def primary_lan_ip() -> str | None:
    """取本机出站网卡的 IPv4，作为局域网访问地址。探测失败时返回 None。"""
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
            # 只查询路由，不会真正发出数据包。
            sock.connect(("8.8.8.8", 80))
            ip = sock.getsockname()[0]
    except OSError:
        ip = ""
    if ip and not ip.startswith("127."):
        return ip
    try:
        infos = socket.getaddrinfo(socket.gethostname(), None, socket.AF_INET, socket.SOCK_DGRAM)
    except OSError:
        return None
    for info in infos:
        candidate = info[4][0]
        if candidate and not candidate.startswith("127."):
            return candidate
    return None


def access_lines(host: str, port: int, access_key: str = "", lan_ip: str | None = None) -> list[str]:
    """生成启动时要打印的本机和局域网访问地址。"""
    suffix = f"/?key={access_key}" if access_key else ""
    if host in {"127.0.0.1", "localhost", "::1"}:
        return [f"INFO:     本机访问：http://127.0.0.1:{port}{suffix}"]
    if host not in {"0.0.0.0", "::", ""}:
        return [f"INFO:     局域网访问：http://{host}:{port}{suffix}"]
    lines = [f"INFO:     本机访问：http://127.0.0.1:{port}{suffix}"]
    if lan_ip:
        lines.append(f"INFO:     局域网访问：http://{lan_ip}:{port}{suffix}")
    else:
        lines.append("INFO:     局域网访问：未能探测到本机局域网地址")
    return lines


def resolve_web_workers(raw: str | None, low_memory: bool) -> int:
    """解析 worker 数。低内存机器强制单进程，两份解释器会把 256MB 直接撑爆。"""
    text = (raw or "").strip()
    try:
        requested = int(text) if text else 1
    except ValueError:
        requested = 1
    if requested < 1:
        requested = 1
    if low_memory and requested > 1:
        print(f"INFO:     低内存保护：WEB_WORKERS={requested} 已降为 1", flush=True)
        return 1
    return requested


def ensure_low_memory_allocator() -> None:
    """glibc 的内存 arena 只能在进程启动前限制，因此低内存模式重新拉起一次自己。"""
    if os.environ.get("OPTION_SCOPE_LOW_MEMORY_REEXEC") == "1":
        return
    if not low_memory_enabled():
        return
    os.environ["MALLOC_ARENA_MAX"] = "1"
    os.environ["MALLOC_TRIM_THRESHOLD_"] = "131072"
    os.environ["MALLOC_MMAP_THRESHOLD_"] = "131072"
    os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
    os.environ["OPTION_SCOPE_LOW_MEMORY_REEXEC"] = "1"
    os.execv(sys.executable, [sys.executable, "-m", "app"])


def main() -> None:
    """启动 FastAPI 服务，允许通过环境变量覆盖端口。"""
    load_dotenv()
    ensure_low_memory_allocator()
    host = os.getenv("HOST", "0.0.0.0")
    port = int(os.getenv("PORT", "8000"))
    low_memory = low_memory_enabled()
    workers = resolve_web_workers(os.getenv("WEB_WORKERS"), low_memory)
    access_key = os.getenv("ACCESS_KEY", "").strip()
    for line in access_lines(host, port, access_key, primary_lan_ip()):
        print(line, flush=True)
    import uvicorn

    run_kwargs: dict[str, int] = {}
    if low_memory:
        # 限制排队连接，避免慢请求把工作线程和响应缓冲一起堆满。
        run_kwargs = {"limit_concurrency": 12, "backlog": 16, "timeout_keep_alive": 5}
    uvicorn.run(
        "app.main:app",
        host=host,
        port=port,
        workers=workers,
        **run_kwargs,
    )


if __name__ == "__main__":
    main()
