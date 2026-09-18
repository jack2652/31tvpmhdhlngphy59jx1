"""应用配置与环境变量解析。"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass
from pathlib import Path

from dotenv import load_dotenv


load_dotenv()


def _bool(name: str, default: bool) -> bool:
    value = os.getenv(name)
    if value is None:
        return default
    return value.strip().lower() in {"1", "true", "yes", "on"}


def _positive_int(name: str, default: int, minimum: int = 1) -> int:
    value = int(os.getenv(name, str(default)))
    if value < minimum:
        raise ValueError(f"{name} 必须大于等于 {minimum}")
    return value


def _size_mb(name: str, default: int) -> int:
    """解析以 MB 为单位的体积配置。

    允许三种写法：纯数字（按 MB 解释）、数字加 M/MB、数字加 G/GB；
    0 表示不限制。写法无法识别时直接报错，避免启动后才发现配置没生效。
    """
    raw = os.getenv(name, str(default)).strip().upper()
    matched = re.fullmatch(r"([0-9]+)\s*(MB|M|GB|G)?", raw)
    if not matched:
        raise ValueError(f"{name} 需要填写整数（单位 MB，例如 300），也可以写成 300M、1G")
    value = int(matched.group(1))
    return value * 1024 if matched.group(2) in {"G", "GB"} else value


@dataclass(frozen=True)
class Settings:
    database_path: Path
    proxy_url: str | None
    default_symbols: tuple[str, ...]
    refresh_interval_seconds: int
    raw_retention_days: int
    cleanup_interval_seconds: int
    scheduler_enabled: bool
    # 日线历史（斐波那契/筹码分布/承接位）的回源间隔，默认 1 小时
    history_max_age_seconds: int = 3600
    # 52 周/历史高低点的回源间隔，默认 1 天（全量日线，变化慢）
    extremes_max_age_seconds: int = 86400
    # SQLite 文件体积上限（MB），0 表示不限制；超过后按阶梯清理历史数据
    database_max_mb: int = 0
    # 页面与 API 的访问密钥；留空表示不启用访问保护，run.sh 会自动生成 16 位密钥
    access_key: str = ""

    @classmethod
    def from_env(cls) -> "Settings":
        raw_symbols = os.getenv("DEFAULT_SYMBOLS", "QQQ")
        symbols = tuple(dict.fromkeys(s.strip().upper() for s in raw_symbols.split(",") if s.strip()))
        if not symbols:
            raise ValueError("DEFAULT_SYMBOLS 至少需要一个标的")
        return cls(
            database_path=Path(os.getenv("DATABASE_PATH", "data/options.db")),
            proxy_url=os.getenv("MARKET_PROXY", "").strip() or None,
            default_symbols=symbols,
            refresh_interval_seconds=_positive_int("REFRESH_INTERVAL_SECONDS", 60),
            raw_retention_days=_positive_int("RAW_RETENTION_DAYS", 30),
            cleanup_interval_seconds=_positive_int("CLEANUP_INTERVAL_SECONDS", 86400),
            scheduler_enabled=_bool("SCHEDULER_ENABLED", True),
            history_max_age_seconds=_positive_int("HISTORY_MAX_AGE_SECONDS", 3600),
            extremes_max_age_seconds=_positive_int("EXTREMES_MAX_AGE_SECONDS", 86400),
            database_max_mb=_size_mb("DATABASE_MAX_MB", 0),
            access_key=os.getenv("ACCESS_KEY", "").strip(),
        )
