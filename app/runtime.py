"""小内存机器的运行时探测和内存归还。"""

from __future__ import annotations

import ctypes
import gc
import os
import shutil
from pathlib import Path


# 容器内存不超过这个值时，auto 模式启用低内存保护。256MB 的 LXC 会落在这里。
LOW_MEMORY_LIMIT_MB = 512
_TRUE = {"1", "true", "yes", "on"}
_FALSE = {"0", "false", "no", "off"}


def _positive_bytes(path: Path) -> int | None:
    """读取 cgroup 内存上限。max 或极大值表示不限制。"""
    try:
        raw = path.read_text(encoding="utf-8").strip()
    except OSError:
        return None
    if not raw or raw == "max" or not raw.isdigit():
        return None
    value = int(raw)
    if value <= 0 or value >= 1 << 60:
        return None
    return value


def memory_limit_bytes() -> int | None:
    """返回 cgroup 内存上限；没有上限时退回 MemTotal。"""
    candidates = [
        Path("/sys/fs/cgroup/memory.max"),
        Path("/sys/fs/cgroup/memory/memory.limit_in_bytes"),
    ]
    try:
        for line in Path("/proc/self/cgroup").read_text(encoding="utf-8").splitlines():
            parts = line.split(":", 2)
            if len(parts) == 3 and parts[0] == "0" and parts[2] not in {"", "/"}:
                candidates.insert(0, Path("/sys/fs/cgroup") / parts[2].lstrip("/") / "memory.max")
    except OSError:
        pass
    for path in candidates:
        value = _positive_bytes(path)
        if value is not None:
            return value
    try:
        for line in Path("/proc/meminfo").read_text(encoding="utf-8").splitlines():
            if line.startswith("MemTotal:"):
                return int(line.split()[1]) * 1024
    except (OSError, ValueError, IndexError):
        return None
    return None


def memory_limit_mb() -> int | None:
    value = memory_limit_bytes()
    if value is None:
        return None
    return max(1, value // 1048576)


def low_memory_enabled(raw: str | None = None) -> bool:
    """LOW_MEMORY=auto 时，内存上限不超过 512MB 就启用保护。"""
    value = os.getenv("LOW_MEMORY", "auto") if raw is None else raw
    normalized = value.strip().lower()
    if normalized in _TRUE:
        return True
    if normalized in _FALSE:
        return False
    limit = memory_limit_mb()
    return limit is not None and limit <= LOW_MEMORY_LIMIT_MB


def release_memory() -> None:
    """把已经不用的堆页还给操作系统，避免下一次刷新叠在碎片上。"""
    gc.collect()
    if not low_memory_enabled():
        return
    try:
        ctypes.CDLL("libc.so.6").malloc_trim(0)
    except (OSError, AttributeError):
        return


def effective_database_max_mb(configured_mb: int, path: Path, low_memory: bool) -> int:
    """小内存机器同时通常也是小磁盘，按剩余空间收紧数据库上限。"""
    if not low_memory:
        return configured_mb
    target = path if path.exists() else path.parent
    try:
        free_mb = shutil.disk_usage(target).free // 1048576
    except OSError:
        return configured_mb if configured_mb > 0 else 64
    # 给系统、虚拟环境和 SQLite 临时文件留出余量。
    budget = max(32, int(free_mb) - 96)
    if configured_mb <= 0:
        return budget
    return min(configured_mb, budget)
