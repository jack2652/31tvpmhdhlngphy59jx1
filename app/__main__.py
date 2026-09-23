"""提供默认监听 0.0.0.0 的本地启动入口。"""

from __future__ import annotations

import os
import socket

from dotenv import load_dotenv
import uvicorn


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


def main() -> None:
    """启动 FastAPI 服务，允许通过环境变量覆盖端口。"""
    load_dotenv()
    host = os.getenv("HOST", "0.0.0.0")
    port = int(os.getenv("PORT", "8000"))
    workers = max(1, int(os.getenv("WEB_WORKERS", "2")))
    access_key = os.getenv("ACCESS_KEY", "").strip()
    for line in access_lines(host, port, access_key, primary_lan_ip()):
        print(line, flush=True)
    uvicorn.run(
        "app.main:app",
        host=host,
        port=port,
        workers=workers,
    )


if __name__ == "__main__":
    main()
